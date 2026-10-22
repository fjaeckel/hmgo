// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package bridge connects the radio module (uartgw) with Home Assistant
// (ha): it decodes frames received from the configured devices into
// Home Assistant state, executes commands from Home Assistant on the
// devices, answers time requests and keeps the module's clock in sync.
//
// Concurrency: Run's event loop owns all device state. Other goroutines
// (the MQTT client's OnConnect callback, API handlers) only read state
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
	"github.com/fjaeckel/hmgo/internal/hm/devices"
	"github.com/fjaeckel/hmgo/internal/hm/params"
	"github.com/fjaeckel/hmgo/internal/hm/power"
	"github.com/fjaeckel/hmgo/internal/hm/remote"
	"github.com/fjaeckel/hmgo/internal/ratelog"
	"github.com/fjaeckel/hmgo/internal/store"
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
	// Debugf logs details only wanted with -debug (e.g. messages that
	// are not decoded); nil discards them.
	Debugf func(format string, args ...any)
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
	// and a state file save is considered. Zero means 1 minute.
	HousekeepingInterval time.Duration
	// StateSaveInterval overrides the configured minimum time between
	// state file writes (for tests). Zero means the configuration.
	StateSaveInterval time.Duration
	// StatusPollStagger is the spacing of the status requests sent to
	// the actuators after startup: the i-th actuator is asked after
	// (i+1)·StatusPollStagger. Zero means 2s.
	StatusPollStagger time.Duration
	// ReleaseCheckInterval is how often long button presses are checked
	// for a missing release. Zero means 500ms.
	ReleaseCheckInterval time.Duration
	// HMIP, if set, lists the Homematic IP devices the radio module
	// heard in the API (/api/v1/unknown).
	HMIP *HMIPMonitor
	// PairingDuration overrides the configured pairing mode duration
	// (for tests). Zero means the configuration.
	PairingDuration time.Duration
	// PairingTick is how often pairing mode checks its timeout. Zero
	// means 1s.
	PairingTick time.Duration
	// Version is shown as hmbridge's software version in Home
	// Assistant.
	Version string
	// Store is where state and paired devices are persisted (see
	// package store). Nil: opened from Config (state_dir).
	Store *store.Store
	// Heating control by Home Assistant (control.go), for tests: how
	// often the link is checked (default 10s), how long it must be back
	// before control is handed back (1m), the least time between two
	// “keep manual mode” commands to a device (10m), and when after
	// startup thermostats of unknown mode get manual mode (1m).
	ControlTick, ReconnectGrace, EnforceInterval, ControlStartupDelay time.Duration
}

// Bridge is the hmbridge core. Create it with New, then call Setup once
// and Run.
type Bridge struct {
	cfg          *config.Config
	radio        Radio
	bcs          *bidcos.Sender
	now          func() time.Time
	logf         func(string, ...any)
	debugf       func(string, ...any)
	retryDelay   time.Duration
	pollStagger  time.Duration
	releaseCheck time.Duration
	retries      int
	clockSync    func() bool
	housekeep    time.Duration
	store        *store.Store
	stateFile    *store.File // nil = no state file

	// devicesMu orders the writes of the devices file; devicesSeq
	// (guarded by mu) numbers the snapshots, devicesWritten (guarded by
	// devicesMu) is the newest one written.
	devicesMu      sync.Mutex
	devicesSeq     uint64
	devicesWritten uint64
	pairTick       time.Duration
	version        string

	// floodLog rate-limits log lines anyone in radio range can trigger.
	floodLog *ratelog.Limiter

	// Event-loop-only state (not guarded by mu).
	clockWasSynced bool // result of the last clock sync check
	clockChecked   bool
	lastSave       time.Time // when the last state file save was requested
	saveInterval   time.Duration
	saveReq        chan struct{} // to the saver goroutine

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

	mu      sync.Mutex // guards the fields below and all device/group state
	pub     Publisher
	unknown map[[3]byte]*UnknownSender
	hmip    *HMIPMonitor // has its own lock
	started time.Time
	// stateDirty: any state changed since the last save;
	// significantDirty: a change worth writing to the SD card.
	stateDirty, significantDirty bool
	// allStaleSent: the first PublishAll removed the entities of all
	// other kinds (AllStaleDiscoveryMessages); later ones are scoped.
	allStaleSent bool
	// pair is the pairing mode state (see pairing.go).
	pair pairingMode
	// staleGroups removes the HA entities of heating groups and group
	// members that are no longer configured (found by SeedFromRetained);
	// sent with every PublishAll.
	staleGroups []ha.Message

	// paramCol receives the frames of the register read the worker is
	// doing (settings.go); guarded by paramMu.
	paramMu  sync.Mutex
	paramCol *paramCollector
	// paramTimeout is the longest gap between two frames of a register
	// read (paramFrameTimeout; tests shorten it).
	paramTimeout time.Duration
	// wakeRetryGap is the least time between a failed settings job of a
	// config-only battery device and the next one a wake-up frame
	// triggers (settingsWake).
	wakeRetryGap time.Duration

	// Heating control by Home Assistant (control.go); event loop only
	// unless noted.
	ctlTargets      []*controlTarget
	ctlFile         *store.File // control.json; nil = none
	debounce        time.Duration
	debounceC       chan string // keys of debounced setpoints that may be due
	pending         map[string]*pendingSetpoint
	ctlTick         time.Duration
	reconnectGrace  time.Duration
	enforceInterval time.Duration
	ctlStartupDelay time.Duration
	ctlStartupDone  bool
	// linkUp: Home Assistant reachable (MQTT) since linkSince.
	linkKnown, linkUp bool
	linkSince         time.Time
}

type device struct {
	cfg   *config.Device
	dev   hm.Device
	desc  ha.DeviceDesc
	group *group

	state    ha.State
	msgs     uint64
	lastAuth bidcos.Auth
	// lastEvent is the type of the last decoded event, for the API.
	lastEvent string
	// lastOnTarget is the last target temperature that was not “off”,
	// used when HA switches the device back to heat mode.
	lastOnTarget float64
	// preBoostHAMode is the HA mode (auto/heat/off) before the last
	// boost, restored when HA leaves boost (preset none).
	preBoostHAMode string
	// warnedForeignDest is set once a message to another address was logged.
	warnedForeignDest bool
	// timeResp is the budget of time responses (rate limit against
	// spoofed time requests; event loop only).
	timeResp tokenBucket
	// available is false while the device has been silent for longer
	// than its stale timeout; availPublished once it was sent to MQTT.
	available, availPublished bool
	// heardSinceStart is set once a message arrived after startup.
	heardSinceStart bool
	// tracker turns a remote's key events into presses (remotes only).
	tracker *remote.Tracker
	// lastOnLevel is the last level (percent) > 0 of a dimmer, used when
	// Home Assistant switches it on without a brightness.
	lastOnLevel float64
	// stored is the devices file record of a device paired with
	// hmbridge (nil for devices of the configuration file).
	stored *store.Device
	// removed is set (on the event loop) once the device was unpaired
	// or replaced (renamed); results of its pending jobs then go to the
	// device now registered at its address, if any.
	removed bool
	// settings are the device's settings (nil for models without any).
	settings *deviceSettings
	// ctl is the Home Assistant control of the thermostat (its own or
	// its group's; nil: device control).
	ctl *controlTarget
}

type group struct {
	name        string
	slug        string
	thermostats []*device // wall thermostats first
	targets     []*device // receivers of group commands
	// available while at least one thermostat is; availPublished once
	// sent to MQTT.
	available, availPublished bool
	contacts                  []*device
	climate                   *device // source of temperature/humidity/target/mode
	state                     ha.State
	// desc is the group's discovery description (members included).
	desc ha.GroupDesc
	// settings are the group settings (groupsettings.go; nil if no
	// thermostat of the group has settings).
	settings *groupSettings
	// ctl is the group's Home Assistant control (nil: device control).
	ctl *controlTarget
}

// maxUnknownSenders bounds the list of unconfigured senders.
const maxUnknownSenders = 100

// UnknownSender describes a radio address that talked to the central but
// is not configured. Listed by the API (/api/v1/unknown) to help with
// setup.
type UnknownSender struct {
	Address  string    `json:"address"`
	Messages uint64    `json:"messages"`
	LastSeen time.Time `json:"last_seen"`
	LastCmd  string    `json:"last_cmd"`
	RSSI     int       `json:"rssi"`
	// Serial and ModelID are known once the device sent DEVICE_INFO
	// (e.g. config button pressed).
	Serial  string `json:"serial"`
	ModelID string `json:"model_id"`
}

// New builds the device registry from the configuration.
func New(opts Options) (*Bridge, error) {
	if opts.Config == nil || opts.Radio == nil {
		return nil, errors.New("bridge: Config and Radio are required")
	}
	b := &Bridge{
		cfg:          opts.Config,
		radio:        opts.Radio,
		now:          opts.Now,
		logf:         opts.Logf,
		debugf:       opts.Debugf,
		pollStagger:  opts.StatusPollStagger,
		releaseCheck: opts.ReleaseCheckInterval,
		retryDelay:   opts.RetryDelay,
		retries:      opts.Retries,
		clockSync:    opts.ClockSynced,
		housekeep:    opts.HousekeepingInterval,
		store:        opts.Store,
		pairTick:     opts.PairingTick,
		version:      opts.Version,
		saveInterval: opts.Config.StateSaveInterval(),
		saveReq:      make(chan struct{}, 1),
		byAddr:       make(map[[3]byte]*device),
		bySlug:       make(map[string]*group),
		commands:     make(chan ha.Command, 32),
		results:      make(chan jobResult, 32),
		queue:        newJobQueue(),
		outbox:       newOutbox(),
		unknown:      make(map[[3]byte]*UnknownSender),
		hmip:         opts.HMIP,
		floodLog:     newFloodLog(),
		paramTimeout: paramFrameTimeout,
		wakeRetryGap: settingsWakeRetryGap,
	}
	if b.clockSync == nil {
		b.clockSync = SystemClockSynced
	}
	if b.housekeep <= 0 {
		b.housekeep = time.Minute
	}
	if opts.StateSaveInterval > 0 {
		b.saveInterval = opts.StateSaveInterval
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.logf == nil {
		b.logf = log.Printf
	}
	if b.store == nil {
		b.store = store.OpenConfig(opts.Config, b.logf)
	}
	b.stateFile = b.store.State()
	if b.debugf == nil {
		b.debugf = func(string, ...any) {}
	}
	if b.pollStagger <= 0 {
		b.pollStagger = 2 * time.Second
	}
	if b.releaseCheck <= 0 {
		b.releaseCheck = 500 * time.Millisecond
	}
	if b.retryDelay == 0 {
		b.retryDelay = 20 * time.Second
	}
	if b.retries == 0 {
		b.retries = 2
	} else if b.retries < 0 {
		b.retries = 0
	}
	if b.pairTick <= 0 {
		b.pairTick = time.Second
	}
	b.bcs = bidcos.NewSender(opts.Radio, opts.Config.CentralAddr())
	b.pair.init(opts.Config, opts.PairingDuration, b.store.PairingEnabled())

	for i := range b.cfg.Devices {
		d, err := b.newDevice(&b.cfg.Devices[i], nil)
		if err != nil {
			return nil, err
		}
		b.devices = append(b.devices, d)
		b.byAddr[d.cfg.Addr()] = d
	}
	if b.store.PairingEnabled() {
		b.loadStore()
	}
	b.loadSettings()

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
		g.settings = newGroupSettings(g)
		g.desc = b.groupDesc(g)
		b.groups = append(b.groups, g)
		b.bySlug[slug] = g
	}
	b.setupControl(opts)
	if b.stateFile != nil {
		if err := b.loadState(b.stateFile); err != nil {
			b.logf("state file: %v", err)
		}
	}
	return b, nil
}

// groupDesc builds g's discovery description and, with
// groups_suggest_area, makes the group name the suggested area of the
// members' HA devices.
func (b *Bridge) groupDesc(g *group) ha.GroupDesc {
	suggest := b.cfg.SuggestGroupAreas()
	desc := ha.GroupDesc{Slug: g.slug, Name: g.name, SuggestArea: suggest}
	for _, d := range append(append([]*device(nil), g.thermostats...), g.contacts...) {
		desc.Members = append(desc.Members, ha.GroupMember{
			Name:    d.cfg.Name,
			Model:   d.cfg.Model,
			Address: d.desc.Addr,
			Role:    ha.RoleForKind(d.desc.Kind),
		})
		if suggest {
			d.desc.SuggestedArea = g.name
		}
	}
	if g.climate != nil {
		desc.Climate = g.climate.desc.Addr
	}
	if g.settings != nil {
		desc.Settings = groupSettingDescs(g.settings.list)
	}
	return desc
}

// newDevice builds the registry entry for dc. stored is the devices
// file record of a device paired with hmbridge (nil otherwise).
func (b *Bridge) newDevice(dc *config.Device, stored *store.Device) (*device, error) {
	// The name becomes a Prometheus label everywhere (here and in the
	// hm packages), which must be valid UTF-8.
	dc.Name = labelValue(dc.Name)
	sd := hm.NewStandardDevice(b.bcs, dc.Addr(), dc.Name)
	// ACKs to commands for AES channels must be authenticated.
	sd.AESChannels = dc.AESChannelList()
	kind, ok := ha.KindForModel(dc.Model)
	if !ok {
		return nil, fmt.Errorf("bridge: model %q of %s not supported by the Home Assistant layer", dc.Model, dc.Name)
	}
	dev, err := devices.New(sd, dc.Model)
	if err != nil {
		return nil, fmt.Errorf("bridge: %s: %v", dc.Name, err)
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
			Paired: stored != nil,
		},
		available: true,
		stored:    stored,
		settings:  newDeviceSettings(dc.Model),
	}
	if d.settings != nil {
		d.desc.Settings = settingDescs(d.settings.model.Settings)
		d.desc.AllSettings = allSettingDescs
	}
	if kind == ha.Remote {
		d.tracker = &remote.Tracker{}
	}
	return d, nil
}

// peerOf returns the radio module peer entry of a device.
func peerOf(dc *config.Device) uartgw.Peer {
	p := uartgw.Peer{
		Addr: dc.Addr(),
		// config counts the highest channel number; the module
		// wants the number of channels including channel 0.
		Channels: dc.Channels() + 1,
		KeyIndex: dc.KeyIndex(),
	}
	if dc.AESEnabled() {
		p.AESChannels = dc.AESChannelList()
	}
	return p
}

// Setup registers all configured devices as peers of the radio module,
// so that it acknowledges their messages and handles AES for them.
func (b *Bridge) Setup(ctx context.Context) error {
	for _, d := range b.devices {
		p := peerOf(d.cfg)
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
	b.checkAvailability(b.started) // restored state may already be stale
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
	stale := ha.StaleDiscoveryMessages
	if !b.allStaleSent {
		// Once per start: a device whose kind changed loses all old
		// entities (button events/triggers, LED selects included).
		stale, b.allStaleSent = ha.AllStaleDiscoveryMessages, true
	}
	msgs = append(msgs, ha.BridgeDiscoveryMessages(topics, b.version, b.pair.enabled)...)
	msgs = append(msgs, ha.PairingStateMessage(topics, b.pairingStateLocked(b.now())))
	for _, d := range b.devices {
		msgs = append(msgs, ha.DiscoveryMessages(topics, d.desc)...)
		msgs = append(msgs, stale(topics, d.desc)...)
		if d.stored != nil {
			msgs = append(msgs, ha.NameMessage(topics, d.desc.Addr, d.cfg.Name))
		}
		// Never overwrite the retained state with an empty one (e.g.
		// right after a restart without a state file).
		if !d.state.LastSeen.IsZero() {
			if m, err := ha.StateMessage(topics, d.desc.Addr, d.state); err == nil {
				msgs = append(msgs, m)
			}
		}
		if d.settings != nil {
			if m, err := ha.SettingsMessage(topics, d.desc.Addr, b.settingsStateLocked(d)); err == nil {
				msgs = append(msgs, m)
			}
		}
		msgs = append(msgs, ha.DeviceAvailabilityMessage(topics, d.desc.Addr, d.available))
		d.availPublished = true
	}
	for _, g := range b.groups {
		msgs = append(msgs, ha.GroupDiscoveryMessages(topics, g.desc)...)
		msgs = append(msgs, ha.GroupMembersMessage(topics, g.desc))
		msgs = append(msgs, ha.GroupAvailabilityMessage(topics, g.slug, g.available))
		g.availPublished = true
		if !g.state.LastSeen.IsZero() {
			if m, err := ha.GroupStateMessage(topics, g.slug, g.state); err == nil {
				msgs = append(msgs, m)
			}
		}
		if g.settings != nil {
			if m, err := ha.GroupSettingsMessage(topics, g.slug, b.groupSettingsStateLocked(g)); err == nil {
				msgs = append(msgs, m)
			}
		}
	}
	// Idempotent, so resent on every reconnect (the outbox does not
	// retry failed publishes).
	msgs = append(msgs, b.staleGroups...)
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
	wg.Add(4)
	initialPolls := b.actuators() // b.devices is only read on the event loop (or under b.mu)
	go func() {
		defer wg.Done()
		b.initialStatusPolls(ctx, initialPolls)
	}()
	go func() {
		defer wg.Done()
		b.worker(ctx)
	}()
	go func() {
		defer wg.Done()
		b.publisher(ctx)
	}()
	go func() {
		defer wg.Done()
		b.saver(ctx.Done())
	}()
	// Stop the helpers, then write the state file a last time (only now,
	// so that it never races with the saver).
	defer func() {
		cancel()
		wg.Wait()
		b.saveControl()
		if b.stateFile == nil {
			return
		}
		b.mu.Lock()
		dirty := b.stateDirty
		b.mu.Unlock()
		if dirty {
			if err := b.saveState(b.stateFile); err != nil {
				b.logf("saving state to %s: %v", b.stateFile.Path(), err)
			}
		}
	}()

	interval := b.cfg.TimeSyncInterval.Duration
	if interval <= 0 {
		interval = time.Hour
	}
	timeSync := time.NewTicker(interval)
	defer timeSync.Stop()
	housekeeping := time.NewTicker(b.housekeep)
	defer housekeeping.Stop()
	// Devices may be paired at runtime, so the status polls and the
	// long press check run even if no such device exists yet.
	var pollC <-chan time.Time
	if p := b.cfg.StatusPollInterval(); p > 0 {
		poll := time.NewTicker(p)
		defer poll.Stop()
		pollC = poll.C
	}
	rel := time.NewTicker(b.releaseCheck)
	defer rel.Stop()
	releaseC := rel.C
	pairTick := time.NewTicker(b.pairTick)
	defer pairTick.Stop()
	var ctlC <-chan time.Time
	if len(b.ctlTargets) > 0 {
		ctl := time.NewTicker(b.ctlTick)
		defer ctl.Stop()
		ctlC = ctl.C
	}
	b.lastSave = b.now()
	b.checkClock()
	// Set the module's clock now if possible: Open skips it while the
	// system clock is not synchronised.
	if b.timeUsable() {
		b.queue.push(job{key: "settime", kind: jobSetTime, priority: true})
	}

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

		case <-pollC:
			for _, d := range b.actuators() {
				b.queue.push(statusJob(d))
			}

		case <-releaseC:
			b.expireLongPresses()

		case <-pairTick.C:
			b.pairingTick()

		case <-ctlC:
			b.controlTick()

		case key := <-b.debounceC:
			b.flushPending(key, false)
		}
	}
}

// housekeeping checks device availability and clock sync and asks the
// saver to write the state file if due. Runs on the event loop.
func (b *Bridge) housekeeping() {
	now := b.now()
	b.mu.Lock()
	msgs := b.checkAvailability(now)
	if len(msgs) > 0 {
		b.outbox.put(msgs...)
	}
	b.mu.Unlock()
	b.checkClock()
	b.requestSave(now)
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
		b.floodLog.Logf(b.logf, floodKeyInvalid, "skipping invalid BidCoS frame %x: %v", frame, err)
		return
	}
	central := b.cfg.CentralAddr()

	d, ok := b.byAddr[pkt.Source]
	if pkt.Cmd == bidcos.DeviceInfo {
		// A device's config button was pressed: pairing (or the
		// confirmation of an unpairing) may be waiting for it.
		b.handleDeviceInfo(pkt, d)
	}
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
	if auth == bidcos.AuthFailed {
		metricAESRejected.WithLabelValues(aesRejectFailed).Inc()
		b.floodLog.Logf(b.logf, floodKeyAESFailed, "%s (%s): message failed AES authentication, ignoring it (cmd %02x). If the key is right, the device may use another key index than %d: set key_index for this device", d.cfg.Name, d.desc.Addr, pkt.Cmd, d.cfg.KeyIndex())
		return
	}
	// The further frames of a register read go to the worker that asked
	// for them (settings.go). They never change the state. Like the
	// first frame (the answer to the request), they are taken without
	// AES authentication: whether a device signs them is not known, see
	// docs/DESIGN.md “Device settings”.
	if pkt.Cmd == bidcos.Info && params.IsParamResponse(pkt.Payload) && pkt.Dest == central && b.forwardParamResponse(pkt) {
		return
	}
	// For an AES device, a frame that was not authenticated could have
	// been forged by anyone in radio range (whatever its destination,
	// flags or command): it must not change any state (eQ-3:
	// auth_violate_policy="reject"). See aes.go for the few exceptions.
	if d.cfg.AESEnabled() && auth != bidcos.AuthOK {
		if reason := aesRejection(d, pkt, central); reason != "" {
			metricUnauthenticated.WithLabelValues(d.desc.Addr, d.cfg.Name).Inc()
			metricAESRejected.WithLabelValues(reason).Inc()
			b.debugf("%s (%s): ignoring unauthenticated cmd %02x (flags %02x, to %x) payload %x: %s", d.cfg.Name, d.desc.Addr, pkt.Cmd, pkt.Flags, pkt.Dest[:], pkt.Payload, reason)
			return
		}
	}

	// A battery device that only listens in config mode signals that it
	// listens: send the settings changes waiting for it. Only frames that
	// passed the AES checks above (an AES device's frames authenticated,
	// apart from the DEVICE_INFO of its config button, which no device
	// signs) and that are addressed to the central or broadcast.
	b.settingsWake(d, pkt, central)

	// A setting changed on the device itself (authenticated, or a device
	// without AES: the checks above).
	if pkt.Cmd == bidcos.Info && params.IsParamChange(pkt.Payload) {
		b.applyParamChange(d, pkt.Payload)
	}

	ev, err := d.dev.Decode(pkt)
	if err != nil || isNilEvent(ev) {
		// Decoders may return a typed nil pointer together with the
		// error; never let it reach stateFromEvent.
		ev = nil
	}
	if err != nil {
		metricDecodeErrors.WithLabelValues(d.desc.Addr, d.cfg.Name).Inc()
		b.floodLog.Logf(b.logf, floodKeyDecode, "%s (%s): decoding cmd %02x payload %x: %v", d.cfg.Name, d.desc.Addr, pkt.Cmd, pkt.Payload, err)
	} else if ev == nil && pkt.Cmd != bidcos.Ack {
		b.debugf("%s (%s): ignoring cmd %02x (flags %02x) payload %x: not used for %s", d.cfg.Name, d.desc.Addr, pkt.Cmd, pkt.Flags, pkt.Payload, d.cfg.Model)
	}
	if le, ok := ev.(*power.LevelEvent); ok && le.Channel != power.ActuatorChannel {
		b.debugf("%s (%s): ignoring level %.1f %% of channel %d (only channel %d is exposed)", d.cfg.Name, d.desc.Addr, le.Percent, le.Channel, power.ActuatorChannel)
		ev = nil
	}

	var presses []remote.Press
	if ke, ok := ev.(*remote.KeyEvent); ok {
		// Events of AES channels count only when authenticated: a forged
		// button press could trigger automations. (Long-press repeats
		// without BiDi cannot be challenged; the authenticated release
		// then also yields the long press, see remote.Tracker.)
		if d.cfg.HasAESChannel(ke.Channel) && auth != bidcos.AuthOK {
			metricUnauthenticated.WithLabelValues(d.desc.Addr, d.cfg.Name).Inc()
			b.debugf("%s (%s): ignoring unauthenticated %v on an AES channel (flags %02x)", d.cfg.Name, d.desc.Addr, ke, pkt.Flags)
			return
		}
		presses = d.tracker.Process(ke, now)
		if len(presses) == 0 {
			b.debugf("%s (%s): %v: repeat or retransmission", d.cfg.Name, d.desc.Addr, ke)
		}
	}

	if tr, ok := ev.(*hm.TimeRequest); ok && tr.Dest == central && b.cfg.AnswersTimeRequests() {
		switch {
		case !d.timeResp.take(now, timeRespBurst, timeRespRefill):
			// Spoofed time requests must not burn the duty cycle.
			metricRateLimited.WithLabelValues(rateKindTimeResponse).Inc()
			b.debugf("%s (%s): not answering time request %02x: rate limit", d.cfg.Name, d.desc.Addr, tr.Msgcnt)
		case b.timeUsable():
			b.queue.push(job{
				key:      "time:" + d.desc.Addr,
				kind:     jobTimeResponse,
				dev:      d,
				reqNo:    tr.Msgcnt,
				priority: true,
			})
		default:
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
	b.publishPresses(d, presses)
	b.controlAfterFrame(d)
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
		// Random forged addresses would log a line per frame.
		b.floodLog.Logf(b.logf, floodKeyUnknown, "ignoring messages from unconfigured device %s (cmd %02x, rssi %d dBm); see /api/v1/unknown", u.Address, pkt.Cmd, pkt.RSSI())
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
	if d.removed {
		return // unpaired: its retained topics were cleared
	}
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
// (the latest), in first-queued order. Events (putEvent) are never
// coalesced: every button press is published.
type outbox struct {
	mu     sync.Mutex
	order  []string
	msgs   map[string]ha.Message
	events uint64
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

// putEvent queues an event message without replacing anything.
func (o *outbox) putEvent(m ha.Message) {
	o.mu.Lock()
	o.events++
	key := fmt.Sprintf("%s\x00%d", m.Topic, o.events)
	o.order = append(o.order, key)
	o.msgs[key] = m
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

// Snapshot is a read-only view of the bridge state (tests, and the
// deprecated /api/state; the API in api.go builds its own views).
type Snapshot struct {
	Started time.Time        `json:"started"`
	Pairing PairingSnapshot  `json:"pairing"`
	Devices []DeviceSnapshot `json:"devices"`
	Groups  []GroupSnapshot  `json:"groups"`
	Unknown []UnknownSender  `json:"unknown_senders"`
	HMIP    []HMIPDevice     `json:"hmip_heard,omitempty"`
	Queue   int              `json:"command_queue"`
}

type DeviceSnapshot struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Address string `json:"address"`
	Serial  string `json:"serial,omitempty"`
	Group   string `json:"group,omitempty"`
	AES     bool   `json:"aes"`
	// Paired is set for devices paired with hmbridge (devices file).
	Paired    bool   `json:"paired,omitempty"`
	LastEvent string `json:"last_event,omitempty"`
	// Summary is a short rendering of a switch, dimmer, LED display or
	// remote state.
	Summary string   `json:"summary,omitempty"`
	State   ha.State `json:"state"`
	// Settings is the status of the device's settings (see the
	// “Settings” sensor), for models with settings.
	Settings string `json:"settings,omitempty"`
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
	s := Snapshot{Started: b.started, Queue: b.queue.len(), Pairing: b.pairingSnapshotLocked(b.now())}
	for _, d := range b.devices {
		var st ha.State
		st.Merge(d.state)
		ds := DeviceSnapshot{
			Name:      d.cfg.Name,
			Model:     d.cfg.Model,
			Address:   d.desc.Addr,
			Serial:    d.cfg.Serial,
			AES:       d.cfg.AESEnabled(),
			Paired:    d.stored != nil,
			LastEvent: d.lastEvent,
			Summary:   summary(d.desc.Kind, &st, d.lastEvent),
			State:     st,
		}
		if d.group != nil {
			ds.Group = d.group.name
		}
		if d.settings != nil {
			ds.Settings = settingsSummary(d.settings)
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
	if b.hmip != nil {
		s.HMIP = b.hmip.Heard()
	}
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
