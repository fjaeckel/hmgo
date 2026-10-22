// Package bridge connects the radio module (uartgw) with Home Assistant
// (ha): it decodes frames received from the configured devices into
// Home Assistant state, executes commands from Home Assistant on the
// devices, answers time requests and keeps the module's clock in sync.
//
// Concurrency: Run's event loop owns all device state. Other goroutines
// (the MQTT client's OnConnect callback, HTTP handlers) only read state
// through methods that take b.mu. Commands are executed sequentially by
// a separate worker goroutine, because a single radio command can take
// several seconds (burst transmission, retries), during which received
// frames must keep flowing.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/contact"
	"github.com/fjaeckel/hmgo/internal/hm/heating"
	"github.com/fjaeckel/hmgo/internal/hm/thermal"
	"github.com/fjaeckel/hmgo/internal/uartgw"
)

// Radio is the subset of *uartgw.Conn the bridge uses.
type Radio interface {
	bidcos.Transport
	Recv() <-chan []byte
	Err() error
	AddPeer(ctx context.Context, p uartgw.Peer) error
	SetTime(ctx context.Context, t time.Time) error
}

// Publisher is the subset of *ha.Client the bridge uses.
type Publisher interface {
	Publish(m ha.Message) error
	Topics() ha.TopicConfig
}

// Options configure a Bridge.
type Options struct {
	Config *config.Config
	Radio  Radio
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// Logf logs; nil means log.Printf.
	Logf func(format string, args ...any)
	// RetryDelay is the delay before retrying a command a device did not
	// answer. Zero means 20s.
	RetryDelay time.Duration
	// Retries is the number of retries for unanswered commands. Zero
	// means 2; negative disables retries.
	Retries int
	// ClockSynced reports whether the system clock is synchronised;
	// nil means asking the kernel (adjtimex). Only consulted when the
	// configuration requires a synced clock.
	ClockSynced func() bool
	// HousekeepingInterval is how often device availability is checked
	// and the state file is written. Zero means 1 minute.
	HousekeepingInterval time.Duration
}

// Bridge is the hmbridge core. Create it with New, then call Setup once
// and Run.
type Bridge struct {
	cfg        *config.Config
	radio      Radio
	bcs        *bidcos.Sender
	now        func() time.Time
	logf       func(string, ...any)
	retryDelay time.Duration
	retries    int
	clockSync  func() bool
	housekeep  time.Duration
	stateFile  string

	devices []*device
	byAddr  map[[3]byte]*device
	groups  []*group
	bySlug  map[string]*group

	// commands from Home Assistant (forwarded from the MQTT client)
	commands chan ha.Command
	// results of executed jobs, applied by the event loop
	results chan jobResult
	queue   *jobQueue
	outbox  *outbox

	mu         sync.Mutex // guards the fields below and all device/group state
	pub        Publisher
	unknown    map[[3]byte]*UnknownSender
	started    time.Time
	stateDirty bool
	// clockWasSynced tracks the last clock sync check (for logging and
	// for setting the module clock as soon as NTP has synchronised).
	clockWasSynced bool
	clockChecked   bool
}

type device struct {
	cfg   *config.Device
	dev   hm.Device
	desc  ha.DeviceDesc
	group *group

	state    ha.State
	msgs     uint64
	lastAuth bidcos.Auth
	// lastEvent is the type of the last decoded event, for the status page.
	lastEvent string
	// lastOnTarget is the last target temperature that was not “off”,
	// used when HA switches the device back to heat mode.
	lastOnTarget float64
	// preBoostHAMode is the HA mode (auto/heat/off) before the last
	// boost, restored when HA leaves boost (preset none).
	preBoostHAMode string
	// warnedForeignDest is set once a message to another address was logged.
	warnedForeignDest bool
	// available is false while the device has been silent for longer
	// than its stale timeout; availPublished once it was sent to MQTT.
	available, availPublished bool
	// heardSinceStart is set once a message arrived after startup.
	heardSinceStart bool
}

type group struct {
	name        string
	slug        string
	thermostats []*device // wall thermostats first
	targets     []*device // receivers of group commands
	contacts    []*device
	climate     *device // source of temperature/humidity/target/mode
	state       ha.State
}

// maxUnknownSenders bounds the list of unconfigured senders.
const maxUnknownSenders = 100

// UnknownSender describes a radio address that talked to the central but
// is not configured. Shown on the status page to help with setup.
type UnknownSender struct {
	Address  string
	Messages uint64
	LastSeen time.Time
	LastCmd  string
	RSSI     int
	// Serial and ModelID are known once the device sent DEVICE_INFO
	// (e.g. config button pressed).
	Serial  string
	ModelID string
}

// New builds the device registry from the configuration.
func New(opts Options) (*Bridge, error) {
	if opts.Config == nil || opts.Radio == nil {
		return nil, errors.New("bridge: Config and Radio are required")
	}
	b := &Bridge{
		cfg:        opts.Config,
		radio:      opts.Radio,
		now:        opts.Now,
		logf:       opts.Logf,
		retryDelay: opts.RetryDelay,
		retries:    opts.Retries,
		clockSync:  opts.ClockSynced,
		housekeep:  opts.HousekeepingInterval,
		stateFile:  opts.Config.StateFilePath(),
		byAddr:     make(map[[3]byte]*device),
		bySlug:     make(map[string]*group),
		commands:   make(chan ha.Command, 32),
		results:    make(chan jobResult, 32),
		queue:      newJobQueue(),
		outbox:     newOutbox(),
		unknown:    make(map[[3]byte]*UnknownSender),
	}
	if b.clockSync == nil {
		b.clockSync = systemClockSynced
	}
	if b.housekeep <= 0 {
		b.housekeep = time.Minute
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.logf == nil {
		b.logf = log.Printf
	}
	if b.retryDelay == 0 {
		b.retryDelay = 20 * time.Second
	}
	if b.retries == 0 {
		b.retries = 2
	} else if b.retries < 0 {
		b.retries = 0
	}
	b.bcs = bidcos.NewSender(opts.Radio, opts.Config.CentralAddr())

	for i := range b.cfg.Devices {
		dc := &b.cfg.Devices[i]
		sd := hm.NewStandardDevice(b.bcs, dc.Addr(), dc.Name)
		var dev hm.Device
		kind, ok := ha.KindForModel(dc.Model)
		if !ok {
			return nil, fmt.Errorf("bridge: model %q of %s not supported by the Home Assistant layer", dc.Model, dc.Name)
		}
		switch dc.Model {
		case config.ModelRTDN:
			dev = heating.NewThermostat(sd)
		case config.ModelTC:
			dev = thermal.NewThermalControl(sd)
		default:
			c, err := contact.NewContact(sd, dc.Model)
			if err != nil {
				return nil, fmt.Errorf("bridge: %s: %v", dc.Name, err)
			}
			dev = c
		}
		d := &device{
			cfg: dc,
			dev: dev,
			desc: ha.DeviceDesc{
				Addr:   dc.AddrHex(),
				Name:   dc.Name,
				Model:  dc.Model,
				Serial: dc.Serial,
				Kind:   kind,
			},
			available: true,
		}
		b.devices = append(b.devices, d)
		b.byAddr[dc.Addr()] = d
	}

	for _, rg := range b.cfg.ResolvedGroups() {
		slug := ha.Slugify(rg.Name)
		if _, dup := b.bySlug[slug]; dup || !ha.ValidSlug(slug) {
			return nil, fmt.Errorf("bridge: group name %q does not yield a unique topic name (%q)", rg.Name, slug)
		}
		g := &group{name: rg.Name, slug: slug}
		for _, dc := range rg.Thermostats {
			d := b.byAddr[dc.Addr()]
			d.group = g
			g.thermostats = append(g.thermostats, d)
		}
		for _, dc := range rg.Contacts {
			d := b.byAddr[dc.Addr()]
			d.group = g
			g.contacts = append(g.contacts, d)
		}
		if rg.Climate != nil {
			g.climate = b.byAddr[rg.Climate.Addr()]
		}
		for _, dc := range rg.CommandTargets {
			g.targets = append(g.targets, b.byAddr[dc.Addr()])
		}
		b.groups = append(b.groups, g)
		b.bySlug[slug] = g
	}
	if b.stateFile != "" {
		if err := b.loadState(b.stateFile); err != nil {
			b.logf("state file: %v", err)
		}
	}
	return b, nil
}

// Setup registers all configured devices as peers of the radio module,
// so that it acknowledges their messages and handles AES for them.
func (b *Bridge) Setup(ctx context.Context) error {
	for _, d := range b.devices {
		p := uartgw.Peer{
			Addr: d.cfg.Addr(),
			// config counts the highest channel number; the module
			// wants the number of channels including channel 0.
			Channels: d.cfg.Channels() + 1,
		}
		p.KeyIndex = d.cfg.KeyIndex()
		if d.cfg.AESEnabled() {
			p.AESChannels = d.cfg.AESChannelList()
		}
		if err := b.radio.AddPeer(ctx, p); err != nil {
			return fmt.Errorf("adding peer %s (%s): %w", d.cfg.Name, d.desc.Addr, err)
		}
		aes := "off"
		if len(p.AESChannels) > 0 {
			aes = fmt.Sprintf("on (channels %v, key index %d)", p.AESChannels, p.KeyIndex)
		}
		b.logf("peer %s %s (%s): AES %s", d.desc.Addr, d.cfg.Name, d.cfg.Model, aes)
	}
	b.mu.Lock()
	b.started = b.now()
	b.mu.Unlock()
	return nil
}

// SetPublisher attaches the MQTT client once it is connected and
// publishes discovery and state for everything. It is safe to call from
// any goroutine; a nil publisher detaches.
func (b *Bridge) SetPublisher(p Publisher) {
	b.mu.Lock()
	b.pub = p
	b.mu.Unlock()
	if p != nil {
		b.PublishAll()
	}
}

// Commands returns the channel on which Home Assistant commands are
// accepted (typically fed from ha.Client.Commands).
func (b *Bridge) Commands() chan<- ha.Command { return b.commands }

// PublishAll publishes discovery messages and the current state of all
// devices and groups. Call it whenever the MQTT connection (re)connects
// or Home Assistant restarts (ha.Options.OnConnect).
func (b *Bridge) PublishAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	pub := b.pub
	if pub == nil {
		return
	}
	topics := pub.Topics()
	var msgs []ha.Message
	for _, d := range b.devices {
		msgs = append(msgs, ha.DiscoveryMessages(topics, d.desc)...)
		msgs = append(msgs, ha.StaleDiscoveryMessages(topics, d.desc)...)
		// Never overwrite the retained state with an empty one (e.g.
		// right after a restart without a state file).
		if !d.state.LastSeen.IsZero() {
			if m, err := ha.StateMessage(topics, d.desc.Addr, d.state); err == nil {
				msgs = append(msgs, m)
			}
		}
		msgs = append(msgs, ha.DeviceAvailabilityMessage(topics, d.desc.Addr, d.available))
		d.availPublished = true
	}
	for _, g := range b.groups {
		msgs = append(msgs, ha.GroupDiscoveryMessages(topics, ha.GroupDesc{Slug: g.slug, Name: g.name})...)
		if !g.state.LastSeen.IsZero() {
			if m, err := ha.GroupStateMessage(topics, g.slug, g.state); err == nil {
				msgs = append(msgs, m)
			}
		}
	}
	// Enqueue while holding b.mu: the outbox keeps only the latest
	// message per topic, so a state snapshot taken here can never
	// overwrite a newer one queued by the event loop.
	b.outbox.put(msgs...)
}

// Run runs the event loop until ctx is done or the radio connection
// fails. It returns the radio's error in the latter case.
func (b *Bridge) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b.worker(ctx)
	}()
	go func() {
		defer wg.Done()
		b.publisher(ctx)
	}()
	// Stop the worker before waiting for it.
	defer func() {
		cancel()
		wg.Wait()
	}()

	interval := b.cfg.TimeSyncInterval.Duration
	if interval <= 0 {
		interval = time.Hour
	}
	timeSync := time.NewTicker(interval)
	defer timeSync.Stop()
	housekeeping := time.NewTicker(b.housekeep)
	defer housekeeping.Stop()
	if b.stateFile != "" {
		defer func() {
			if err := b.saveState(b.stateFile); err != nil {
				b.logf("saving state: %v", err)
			}
		}()
	}
	b.checkClock()

	recv := b.radio.Recv()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case frame, ok := <-recv:
			if !ok {
				err := b.radio.Err()
				if err == nil {
					err = errors.New("radio connection closed")
				}
				return fmt.Errorf("radio: %w", err)
			}
			b.handleFrame(frame)

		case cmd := <-b.commands:
			b.handleCommand(cmd)

		case res := <-b.results:
			b.handleResult(res)

		case <-timeSync.C:
			if b.timeUsable() {
				b.queue.push(job{key: "settime", kind: jobSetTime})
			}

		case <-housekeeping.C:
			b.housekeeping()
		}
	}
}

// housekeeping checks device availability and clock sync and writes the
// state file. Runs on the event loop.
func (b *Bridge) housekeeping() {
	b.mu.Lock()
	msgs := b.checkAvailability(b.now())
	if len(msgs) > 0 {
		b.outbox.put(msgs...)
	}
	dirty := b.stateDirty
	b.mu.Unlock()
	b.checkClock()
	if dirty && b.stateFile != "" {
		if err := b.saveState(b.stateFile); err != nil {
			b.logf("saving state: %v", err)
		}
	}
}

// checkClock tracks whether the system clock is synchronised and sets
// the radio module's clock once it becomes synchronised.
func (b *Bridge) checkClock() {
	if !b.cfg.RequiresSyncedClock() {
		return
	}
	synced := b.clockSync()
	metricClockSynced.Set(boolFloat(synced))
	switch {
	case !synced && (b.clockWasSynced || !b.clockChecked):
		b.logf("system clock is not synchronised (NTP); not answering the devices' time requests until it is (enable systemd-timesyncd, or set require_synced_clock to false)")
	case synced && !b.clockWasSynced && b.clockChecked:
		b.logf("system clock synchronised; setting the radio module's clock")
		b.queue.push(job{key: "settime", kind: jobSetTime, priority: true})
	}
	b.clockWasSynced = synced
	b.clockChecked = true
}

// timeUsable reports whether the system time may be handed out.
func (b *Bridge) timeUsable() bool {
	return !b.cfg.RequiresSyncedClock() || b.clockSync()
}

func boolFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// handleFrame decodes a frame received by the radio module.
func (b *Bridge) handleFrame(frame []byte) {
	pkt, err := bidcos.Decode(frame)
	if err != nil {
		metricFrames.WithLabelValues("invalid").Inc()
		b.logf("skipping invalid BidCoS frame %x: %v", frame, err)
		return
	}
	central := b.cfg.CentralAddr()

	d, ok := b.byAddr[pkt.Source]
	if !ok {
		metricFrames.WithLabelValues("unknown").Inc()
		b.recordUnknown(pkt)
		return
	}
	metricFrames.WithLabelValues("known").Inc()
	now := b.now()

	// Messages addressed neither to us nor broadcast are either direct
	// device-to-device traffic (e.g. wall thermostat → valves) or a sign
	// of a wrong central_address: the devices keep reporting to the old
	// CCU address and will ignore our commands.
	if pkt.Dest != central && pkt.Dest != ([3]byte{}) {
		metricForeignDest.WithLabelValues(d.desc.Addr, d.cfg.Name).Inc()
		b.mu.Lock()
		first := !d.warnedForeignDest
		d.warnedForeignDest = true
		b.mu.Unlock()
		if first {
			b.logf("%s (%s) sent cmd %02x to %x, not to central_address %x. Direct device links do this; if all its messages go to %x, central_address is probably wrong", d.cfg.Name, d.desc.Addr, pkt.Cmd, pkt.Dest[:], central[:], pkt.Dest[:])
		}
	}

	auth := pkt.Auth()
	metricAuth.WithLabelValues(d.desc.Addr, d.cfg.Name, auth.String()).Inc()
	// The radio module only authenticates messages addressed to the
	// central. For an AES device, anything else (broadcasts, messages
	// to its peers) could be forged by anyone in radio range, so it must
	// not change the state (eQ-3: auth_violate_policy="reject").
	if d.cfg.AESEnabled() && auth != bidcos.AuthOK && pkt.Dest != central {
		metricUnauthenticated.WithLabelValues(d.desc.Addr, d.cfg.Name).Inc()
		return
	}
	if auth == bidcos.AuthFailed {
		b.logf("%s (%s): message failed AES authentication, ignoring it (cmd %02x). If the key is right, the device may use another key index than %d: set key_index for this device", d.cfg.Name, d.desc.Addr, pkt.Cmd, d.cfg.KeyIndex())
		return
	}

	ev, err := d.dev.Decode(pkt)
	if err != nil || isNilEvent(ev) {
		// Decoders may return a typed nil pointer together with the
		// error; never let it reach stateFromEvent.
		ev = nil
	}
	if err != nil {
		metricDecodeErrors.WithLabelValues(d.desc.Addr, d.cfg.Name).Inc()
		b.logf("%s (%s): decoding cmd %02x payload %x: %v", d.cfg.Name, d.desc.Addr, pkt.Cmd, pkt.Payload, err)
	}

	if tr, ok := ev.(*hm.TimeRequest); ok && tr.Dest == central && b.cfg.AnswersTimeRequests() {
		if b.timeUsable() {
			b.queue.push(job{
				key:      "time:" + d.desc.Addr,
				kind:     jobTimeResponse,
				dev:      d,
				reqNo:    tr.Msgcnt,
				priority: true,
			})
		} else {
			metricTimeRequestsSkipped.Inc()
		}
	}

	b.mu.Lock()
	d.msgs++
	d.lastAuth = auth
	if ev != nil {
		d.lastEvent = eventName(ev)
	}
	upd := stateFromEvent(d, ev)
	upd.LastSeen = now
	if rssi := pkt.RSSI(); rssi != 0 {
		upd.RSSI = ptr(rssi)
	}
	upd.MessagesReceived = ptr(d.msgs)
	upd.AuthStatus = authString(auth)
	b.applyUpdate(d, upd)
	d.heardSinceStart = true
	var avail []ha.Message
	if !d.available {
		avail = b.checkAvailability(now)
	}
	b.mu.Unlock()
	if len(avail) > 0 {
		b.outbox.put(avail...)
	}

	b.publishDevice(d)
}

// recordUnknown remembers a sender that is not configured.
func (b *Bridge) recordUnknown(pkt *bidcos.Packet) {
	b.mu.Lock()
	u, ok := b.unknown[pkt.Source]
	if !ok {
		if len(b.unknown) >= maxUnknownSenders {
			// Forget the one heard least recently (neighbours' devices,
			// or random forged addresses, must not grow memory forever).
			var oldest [3]byte
			var oldestT time.Time
			for a, o := range b.unknown {
				if oldestT.IsZero() || o.LastSeen.Before(oldestT) {
					oldest, oldestT = a, o.LastSeen
				}
			}
			delete(b.unknown, oldest)
		}
		u = &UnknownSender{Address: fmt.Sprintf("%x", pkt.Source[:])}
		b.unknown[pkt.Source] = u
		b.logf("ignoring messages from unconfigured device %s (cmd %02x, rssi %d dBm); see the status page", u.Address, pkt.Cmd, pkt.RSSI())
	}
	u.Messages++
	u.LastSeen = b.now()
	u.LastCmd = fmt.Sprintf("%02x", pkt.Cmd)
	u.RSSI = pkt.RSSI()
	if pkt.Cmd == bidcos.DeviceInfo {
		if di, err := hm.DecodeDeviceInfo(pkt.Payload); err == nil {
			u.Serial = di.Serial
			u.ModelID = fmt.Sprintf("%04x", di.ModelID)
		}
	}
	metricUnknownSenders.Set(float64(len(b.unknown)))
	b.mu.Unlock()
}

// publishDevice publishes the device's state and, if it belongs to a
// group, the group's state.
func (b *Bridge) publishDevice(d *device) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if g := d.group; g != nil {
		g.state = groupState(g)
	}
	pub := b.pub
	if pub == nil {
		return // PublishAll sends everything once MQTT is connected
	}
	topics := pub.Topics()
	if m, err := ha.StateMessage(topics, d.desc.Addr, d.state); err == nil {
		b.outbox.put(m)
	} else {
		b.logf("encoding state of %s: %v", d.cfg.Name, err)
	}
	if g := d.group; g != nil {
		if m, err := ha.GroupStateMessage(topics, g.slug, g.state); err == nil {
			b.outbox.put(m)
		}
	}
}

// publisher sends the outbox to the MQTT broker. Publishing happens on
// this goroutine so that a slow or unreachable broker never stalls the
// event loop.
func (b *Bridge) publisher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.outbox.signal:
		}
		msgs := b.outbox.take()
		b.mu.Lock()
		pub := b.pub
		b.mu.Unlock()
		if pub == nil {
			continue
		}
		var failed int
		var lastErr error
		for _, m := range msgs {
			if err := pub.Publish(m); err != nil {
				failed++
				lastErr = err
			}
		}
		// While disconnected, messages are dropped on purpose: the
		// reconnect triggers PublishAll with the then-current state.
		if failed > 0 && !errors.Is(lastErr, ha.ErrNotConnected) {
			b.logf("MQTT: %d of %d messages not published: %v", failed, len(msgs), lastErr)
		}
	}
}

// outbox holds messages waiting to be published, at most one per topic
// (the latest), in first-queued order.
type outbox struct {
	mu     sync.Mutex
	order  []string
	msgs   map[string]ha.Message
	signal chan struct{}
}

func newOutbox() *outbox {
	return &outbox{msgs: make(map[string]ha.Message), signal: make(chan struct{}, 1)}
}

func (o *outbox) put(msgs ...ha.Message) {
	o.mu.Lock()
	for _, m := range msgs {
		if _, queued := o.msgs[m.Topic]; !queued {
			o.order = append(o.order, m.Topic)
		}
		o.msgs[m.Topic] = m
	}
	o.mu.Unlock()
	select {
	case o.signal <- struct{}{}:
	default:
	}
}

func (o *outbox) take() []ha.Message {
	o.mu.Lock()
	defer o.mu.Unlock()
	res := make([]ha.Message, 0, len(o.order))
	for _, t := range o.order {
		res = append(res, o.msgs[t])
	}
	o.order = nil
	o.msgs = make(map[string]ha.Message)
	return res
}

// Snapshot is a read-only view of the bridge state for the status page
// and the JSON API.
type Snapshot struct {
	Started time.Time         `json:"started"`
	Devices []DeviceSnapshot  `json:"devices"`
	Groups  []GroupSnapshot   `json:"groups"`
	Unknown []UnknownSender   `json:"unknown_senders"`
	Queue   int               `json:"command_queue"`
	Extra   map[string]string `json:"info,omitempty"`
}

type DeviceSnapshot struct {
	Name      string   `json:"name"`
	Model     string   `json:"model"`
	Address   string   `json:"address"`
	Serial    string   `json:"serial,omitempty"`
	Group     string   `json:"group,omitempty"`
	AES       bool     `json:"aes"`
	LastEvent string   `json:"last_event,omitempty"`
	State     ha.State `json:"state"`
}

type GroupSnapshot struct {
	Name    string   `json:"name"`
	Slug    string   `json:"slug"`
	Members []string `json:"members"`
	State   ha.State `json:"state"`
}

// Snapshot returns a copy of the current state.
func (b *Bridge) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{Started: b.started, Queue: b.queue.len()}
	for _, d := range b.devices {
		var st ha.State
		st.Merge(d.state)
		ds := DeviceSnapshot{
			Name:      d.cfg.Name,
			Model:     d.cfg.Model,
			Address:   d.desc.Addr,
			Serial:    d.cfg.Serial,
			AES:       d.cfg.AESEnabled(),
			LastEvent: d.lastEvent,
			State:     st,
		}
		if d.group != nil {
			ds.Group = d.group.name
		}
		s.Devices = append(s.Devices, ds)
	}
	for _, g := range b.groups {
		var st ha.State
		st.Merge(g.state)
		gs := GroupSnapshot{Name: g.name, Slug: g.slug, State: st}
		for _, d := range g.thermostats {
			gs.Members = append(gs.Members, d.cfg.Name)
		}
		for _, d := range g.contacts {
			gs.Members = append(gs.Members, d.cfg.Name)
		}
		s.Groups = append(s.Groups, gs)
	}
	for _, u := range b.unknown {
		s.Unknown = append(s.Unknown, *u)
	}
	sort.Slice(s.Unknown, func(i, j int) bool { return s.Unknown[i].LastSeen.After(s.Unknown[j].LastSeen) })
	return s
}

func ptr[T any](v T) *T { return &v }

// isNilEvent reports whether ev is nil or a typed nil pointer.
func isNilEvent(ev any) bool {
	if ev == nil {
		return true
	}
	v := reflect.ValueOf(ev)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

func authString(a bidcos.Auth) string {
	switch a {
	case bidcos.AuthOK:
		return "ok"
	case bidcos.AuthFailed:
		return "failed"
	default:
		return "none"
	}
}
