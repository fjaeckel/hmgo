// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

// Pairing (“teach-in”) and unpairing of classic BidCoS devices, like a
// CCU does it:
//
//  1. Pairing mode is started from Home Assistant (button “Pair new
//     device”) or the status page and stays on for pairing.duration
//     (default 60s), one device at a time.
//  2. The user presses the device's config button; it sends DEVICE_INFO
//     (cmd 0x00, usually as a broadcast) with its model ID and serial.
//  3. hmbridge maps the model ID to a supported model
//     (internal/hm/pairing), registers the device with the radio
//     module and writes the central address to the device's list 0
//     (CONFIG_START, CONFIG_WRITE_INDEX 02/0A/0B/0C, CONFIG_END). The
//     device is awake while in config mode, so this is sent at once,
//     without burst.
//  4. With pairing.aes "system_key", an AES-capable device is given the
//     configured key (AES key exchange, cmd 0x04, see package
//     bidcosaes) and AES_ACTIVE is set on the channels a CCU enables it
//     on (contacts, push buttons, remotes).
//  5. The device is stored in the devices file and appears in Home
//     Assistant at once.
//
// A device that is already known (configuration file or devices file)
// is only re-confirmed (its central address written again) and
// reported as “already known”: devices taken over from OpenCCU belong
// to the same central address and key.
//
// Unpairing (Home Assistant button “Unpair”, only for devices in the
// devices file) removes the device at once and writes the central
// address 000000 to its list 0 (Homegear: unpair). Mains powered
// devices get it immediately, wake-on-radio thermostats with a burst;
// devices that only listen in config mode (contacts, remotes, weather
// sensors) get it when their config button is pressed within the
// pairing duration.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	bidcosaes "github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/pairing"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/store"
	"github.com/fjaeckel/hmgo/internal/uartgw"
)

// pairingMode is the pairing state. Written on the event loop under
// b.mu, read under b.mu.
type pairingMode struct {
	enabled bool
	// disabledReason says why pairing is off (when !enabled).
	disabledReason string
	// warnings are problems with the devices file, for the status page.
	warnings []string
	// kept are devices file records that could not be used (e.g. their
	// key index is no longer configured). They are written back
	// unchanged, so that fixing the configuration brings them back.
	kept     []store.Device
	aes      string        // config.PairingAES*
	duration time.Duration // default duration of pairing mode

	active bool      // pairing mode on
	until  time.Time // end of pairing mode
	// busy: a device is being configured (pair job queued or running);
	// further DEVICE_INFOs are ignored until it is done.
	busy bool

	lastResult   string
	lastResultAt time.Time
	// lastRemaining is the remaining time last published (seconds).
	lastRemaining int

	// unpairWait: devices that were removed but only listen in config
	// mode; their next DEVICE_INFO triggers the unpair messages.
	unpairWait map[[3]byte]unpairWaiting
}

type unpairWaiting struct {
	until time.Time
	job   unpairJob
}

func (p *pairingMode) init(cfg *config.Config, override time.Duration, enabled bool) {
	p.enabled = enabled
	if !enabled {
		p.disabledReason = `state_dir is ""`
	}
	p.aes = cfg.PairingAES()
	p.duration = cfg.PairingDuration()
	if override > 0 {
		p.duration = override
	}
	p.unpairWait = make(map[[3]byte]unpairWaiting)
}

// PairingSnapshot is the pairing state for the status page and the
// JSON API.
type PairingSnapshot struct {
	Enabled      bool      `json:"enabled"`
	AES          string    `json:"aes"`
	Status       string    `json:"status"`
	Remaining    int       `json:"remaining_seconds"`
	LastResult   string    `json:"last_result,omitempty"`
	LastResultAt time.Time `json:"last_result_at,omitempty"`
	// DisabledReason says why pairing is disabled.
	DisabledReason string `json:"disabled_reason,omitempty"`
	// Warnings are problems with the devices file.
	Warnings []string `json:"warnings,omitempty"`
	// UnpairWaiting lists devices whose config button must be pressed
	// to complete their unpairing.
	UnpairWaiting []string `json:"unpair_waiting,omitempty"`
}

func remainingSeconds(until, now time.Time) int {
	if !until.After(now) {
		return 0
	}
	return int((until.Sub(now) + time.Second - 1) / time.Second)
}

// pairingStateLocked returns the published pairing state. Caller holds
// b.mu.
func (b *Bridge) pairingStateLocked(now time.Time) ha.PairingState {
	p := &b.pair
	st := ha.PairingState{Status: ha.PairingIdle, LastResult: p.lastResult, LastResultAt: p.lastResultAt}
	switch {
	case !p.enabled:
		st.Status = ha.PairingDisabled
	case p.busy:
		st.Status, st.Active = ha.PairingConfiguring, true
	case p.active:
		st.Status, st.Active = ha.PairingActive, true
	}
	if p.active {
		st.Remaining = remainingSeconds(p.until, now)
	}
	return st
}

func (b *Bridge) pairingSnapshotLocked(now time.Time) PairingSnapshot {
	st := b.pairingStateLocked(now)
	s := PairingSnapshot{
		Enabled: b.pair.enabled, AES: b.pair.aes, Status: st.Status, Remaining: st.Remaining,
		LastResult: st.LastResult, LastResultAt: st.LastResultAt,
		DisabledReason: b.pair.disabledReason, Warnings: append([]string(nil), b.pair.warnings...),
	}
	for a, w := range b.pair.unpairWait {
		s.UnpairWaiting = append(s.UnpairWaiting, fmt.Sprintf("%s (%x, %d s)", w.job.name, a[:], remainingSeconds(w.until, now)))
	}
	return s
}

// publishPairingLocked queues the pairing state. Caller holds b.mu.
func (b *Bridge) publishPairingLocked(now time.Time) {
	st := b.pairingStateLocked(now)
	b.pair.lastRemaining = st.Remaining
	metricPairingActive.Set(boolFloat(st.Active))
	if b.pub != nil {
		b.outbox.put(ha.PairingStateMessage(b.pub.Topics(), st))
	}
}

// setResultLocked records the outcome of a pairing or unpairing. Caller
// holds b.mu.
func (b *Bridge) setResultLocked(msg string) {
	b.pair.lastResult = msg
	b.pair.lastResultAt = b.now()
	b.logf("pairing: %s", msg)
}

// startPairing turns pairing mode on (or extends it). Runs on the event
// loop.
func (b *Bridge) startPairing(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.pair.enabled {
		b.setResultLocked(fmt.Sprintf("pairing is disabled (%s)", b.pair.disabledReason))
		metricPairing.WithLabelValues("disabled").Inc()
		b.publishPairingLocked(b.now())
		return
	}
	if d <= 0 {
		d = b.pair.duration
	}
	now := b.now()
	b.pair.active = true
	b.pair.until = now.Add(d)
	b.logf("pairing mode on for %v: press the config button of the device to pair (AES: %s)", d, b.pair.aes)
	b.publishPairingLocked(now)
}

// stopPairingLocked turns pairing mode off. Caller holds b.mu.
func (b *Bridge) stopPairingLocked(now time.Time) {
	if !b.pair.active {
		return
	}
	b.pair.active = false
	b.publishPairingLocked(now)
}

// pairingTick ends pairing mode and expired unpair waits, and publishes
// the remaining time every 5 seconds. Runs on the event loop.
func (b *Bridge) pairingTick() {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for a, w := range b.pair.unpairWait {
		if !now.Before(w.until) {
			delete(b.pair.unpairWait, a)
			metricUnpair.WithLabelValues("unconfirmed").Inc()
			b.setResultLocked(fmt.Sprintf("%s (%x) was removed, but its config button was not pressed in time; the device still believes it is paired (factory-reset it, or pair it again)", w.job.name, a[:]))
			b.publishPairingLocked(now)
		}
	}
	if !b.pair.active {
		return
	}
	if !now.Before(b.pair.until) && !b.pair.busy {
		b.pair.active = false
		metricPairing.WithLabelValues("timeout").Inc()
		b.setResultLocked("pairing mode ended: no device announced itself (press the device's config button while pairing mode is on)")
		b.publishPairingLocked(now)
		return
	}
	if r := remainingSeconds(b.pair.until, now); r != b.pair.lastRemaining && (r%5 == 0 || b.pairTick < time.Second) {
		b.publishPairingLocked(now)
	}
}

// pairJob is a device to configure.
type pairJob struct {
	addr  [3]byte
	info  pairing.DeviceInfo
	model pairing.Model
	rssi  int
	// known is the already paired device at this address (re-pairing
	// only confirms the central address), nil for a new device.
	known    *device
	keyIndex byte // the device's current key index
	// aes: give the device the system key and enable AES_ACTIVE.
	aes bool
}

// pairOutcome is the result of a pair job.
type pairOutcome struct {
	keyIndex    byte
	aesChannels []int
	notes       []string // partial failures (key exchange, AES_ACTIVE)
}

// handleDeviceInfo handles a DEVICE_INFO frame: known is the device at
// its address, if any. Runs on the event loop.
func (b *Bridge) handleDeviceInfo(pkt *bidcos.Packet, known *device) {
	now := b.now()
	b.mu.Lock()
	if w, ok := b.pair.unpairWait[pkt.Source]; ok && known == nil {
		delete(b.pair.unpairWait, pkt.Source)
		b.mu.Unlock()
		j := w.job
		j.send, j.peerRemoved = true, true
		b.logf("pairing: %s (%x) pressed its config button: sending the unpair messages", j.name, j.addr[:])
		b.queue.push(job{key: fmt.Sprintf("unpair:%x", j.addr[:]), kind: jobUnpair, unpair: &j, priority: true})
		return
	}
	active, busy := b.pair.active && now.Before(b.pair.until), b.pair.busy
	b.mu.Unlock()
	if !active || busy {
		return
	}

	reject := func(label, msg string) {
		metricPairing.WithLabelValues(label).Inc()
		b.mu.Lock()
		b.setResultLocked(msg)
		b.publishPairingLocked(now)
		b.mu.Unlock()
	}
	addr := fmt.Sprintf("%x", pkt.Source[:])
	di, err := pairing.ParseDeviceInfo(pkt.Payload)
	if err != nil {
		reject("invalid", fmt.Sprintf("ignoring DEVICE_INFO of %s: %v", addr, err))
		return
	}
	central := b.cfg.CentralAddr()
	if pkt.Dest != central && pkt.Dest != ([3]byte{}) {
		reject("other_central", fmt.Sprintf("%s (%s, model %04X) is paired to another central (%x); factory-reset it first (see its manual), then pair it again", di.Serial, addr, di.ModelID, pkt.Dest[:]))
		return
	}
	model, ok := pairing.ModelForID(di.ModelID)
	if !ok {
		reject("unsupported_model", fmt.Sprintf("%s (%s) has model ID %04X, which hmbridge does not support", di.Serial, addr, di.ModelID))
		return
	}
	pj := &pairJob{addr: pkt.Source, info: di, model: model, rssi: pkt.RSSI()}
	if known != nil {
		if known.cfg.Model != model.Name || known.cfg.Serial != "" && known.cfg.Serial != di.Serial {
			reject("address_conflict", fmt.Sprintf("%s (%s, %s) has the address of the configured %s (%s, serial %q); not pairing it", di.Serial, addr, model.Name, known.cfg.Name, known.cfg.Model, known.cfg.Serial))
			return
		}
		pj.known = known
		pj.keyIndex = known.cfg.KeyIndex()
	} else {
		cur, _ := b.cfg.Keys()
		pj.aes = b.pair.aes == config.PairingAESSystemKey && model.AES && cur.Index > 0
	}
	b.mu.Lock()
	b.pair.busy = true
	b.publishPairingLocked(now)
	b.mu.Unlock()
	b.logf("pairing: %s %s (%s, firmware %s) announced itself; configuring it", model.Name, di.Serial, addr, di.FirmwareString())
	// The device listens only while in config mode: jump the queue.
	b.queue.push(job{key: "pair:" + addr, kind: jobPair, pair: pj, priority: true})
}

// sendConfig sends one configuration message to addr and checks the
// device's ACK.
func (b *Bridge) sendConfig(ctx context.Context, addr [3]byte, flags byte, m pairing.Message) error {
	pkt := &bidcos.Packet{Flags: flags, Cmd: m.Cmd, Dest: addr, Payload: m.Payload}
	resp, err := b.bcs.Send(ctx, pkt)
	if err != nil {
		return fmt.Errorf("%s: %w", m.What, err)
	}
	if err := hm.CheckAck(addr, resp); err != nil {
		return fmt.Errorf("%s: %w", m.What, err)
	}
	return nil
}

// executePair configures a device (worker goroutine).
func (b *Bridge) executePair(ctx context.Context, p *pairJob) (any, error) {
	out := &pairOutcome{keyIndex: p.keyIndex}
	if p.known != nil {
		// Already registered with the radio module: only confirm the
		// central address.
		for _, m := range pairing.PairSequence(b.cfg.CentralAddr(), p.model.InternalKeysVisible) {
			if err := b.sendConfig(ctx, p.addr, pairing.ConfigFlags, m); err != nil {
				return nil, err
			}
		}
		return out, nil
	}

	peer := uartgw.Peer{Addr: p.addr, KeyIndex: 0, Channels: config.ModelChannels(p.model.Name) + 1}
	// Register the device first, so that the module (HM-MOD-RPI-PCB)
	// passes its responses on (Homegear does the same).
	if err := b.radio.AddPeer(ctx, peer); err != nil {
		return nil, fmt.Errorf("registering with the radio module: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			b.removePeer(ctx, p.addr)
		}
	}()
	for _, m := range pairing.PairSequence(b.cfg.CentralAddr(), p.model.InternalKeysVisible) {
		if err := b.sendConfig(ctx, p.addr, pairing.ConfigFlags, m); err != nil {
			return nil, err
		}
	}
	ok = true

	if p.aes {
		cur, _ := b.cfg.Keys()
		if err := b.keyExchange(ctx, p.addr, hmkey.EQ3Default, *cur); err != nil {
			out.notes = append(out.notes, fmt.Sprintf("AES key exchange failed (%v); the device keeps the eQ-3 default key and AES stays off", err))
		} else {
			out.keyIndex = cur.Index
			peer.KeyIndex = cur.Index
			if err := b.radio.AddPeer(ctx, peer); err != nil {
				out.notes = append(out.notes, fmt.Sprintf("updating the radio module: %v", err))
			}
			for _, ch := range p.model.AESDefault {
				var err error
				for _, m := range pairing.AESActiveSequence(ch, true) {
					if err = b.sendConfig(ctx, p.addr, pairing.ConfigFlags, m); err != nil {
						break
					}
				}
				if err != nil {
					out.notes = append(out.notes, fmt.Sprintf("enabling AES on channel %d failed (%v)", ch, err))
					break
				}
				out.aesChannels = append(out.aesChannels, int(ch))
			}
		}
	}
	if len(out.aesChannels) > 0 {
		for _, ch := range out.aesChannels {
			peer.AESChannels = append(peer.AESChannels, byte(ch))
		}
		if err := b.radio.AddPeer(ctx, peer); err != nil {
			out.notes = append(out.notes, fmt.Sprintf("enabling AES in the radio module: %v", err))
		}
	}
	return out, nil
}

// keyExchange gives the device at addr (using old) the key newKey.
func (b *Bridge) keyExchange(ctx context.Context, addr [3]byte, old, newKey hmkey.Key) error {
	var payloads [2][]byte
	if hk, ok := b.radio.(interface{ HostKeyExchange() bool }); ok && hk.HostKeyExchange() {
		enc, err := bidcosaes.KeyExchangePayloads(old.Bytes, newKey, rand.Reader)
		if err != nil {
			return err
		}
		payloads = [2][]byte{enc[0][:], enc[1][:]}
	} else {
		payloads = bidcosaes.KeyExchangeTemplates(old.Index)
	}
	for i, p := range payloads {
		m := pairing.Message{Cmd: pairing.CmdKeyExchange, Payload: p, What: fmt.Sprintf("AES key exchange part %d of 2", i+1)}
		if err := b.sendConfig(ctx, addr, pairing.ConfigFlags, m); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) removePeer(ctx context.Context, addr [3]byte) {
	if r, ok := b.radio.(interface {
		RemovePeer(context.Context, [3]byte) error
	}); ok {
		if err := r.RemovePeer(ctx, addr); err != nil {
			b.logf("removing %x from the radio module: %v", addr[:], err)
		}
	}
}

// handlePairResult applies the outcome of a pair job (event loop).
func (b *Bridge) handlePairResult(p *pairJob, ev any, err error) {
	now := b.now()
	addr := fmt.Sprintf("%x", p.addr[:])
	// The devices file is written after b.mu is released (deferred
	// before the unlock, so it runs after it).
	var save func() error
	var savedMsg string
	defer func() {
		if save == nil {
			return
		}
		if err := save(); err != nil {
			b.mu.Lock()
			b.setResultLocked(fmt.Sprintf("%s; WARNING: saving %s failed (%v), the device is lost on restart", savedMsg, b.store.DevicesPath(), err))
			b.publishPairingLocked(b.now())
			b.mu.Unlock()
		}
	}()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pair.busy = false
	defer b.publishPairingLocked(now)
	if err != nil {
		metricPairing.WithLabelValues("failed").Inc()
		b.setResultLocked(fmt.Sprintf("pairing %s %s (%s) failed: %v. Press the config button again while pairing mode is on; if the device keeps refusing (NACK), factory-reset it", p.model.Name, p.info.Serial, addr, err))
		return
	}
	out := ev.(*pairOutcome)
	if p.known != nil {
		metricPairing.WithLabelValues("already_known").Inc()
		b.pair.active = false
		where := "config.json"
		if p.known.stored != nil {
			where = "the devices file"
		}
		b.setResultLocked(fmt.Sprintf("already known: %s (%s, %s, from %s); confirmed its central address", p.known.cfg.Name, addr, p.model.Name, where))
		return
	}

	// A new device: store it and show it in Home Assistant.
	if cur := b.byAddr[p.addr]; cur != nil {
		// Paired meanwhile some other way (should not happen).
		metricPairing.WithLabelValues("failed").Inc()
		b.setResultLocked(fmt.Sprintf("%s is registered already as %s", addr, cur.cfg.Name))
		return
	}
	ki := int(out.keyIndex)
	dc := config.Device{
		Name:             b.uniqueNameLocked(p.model.Name+" "+p.info.Serial, p.addr),
		Model:            p.model.Name,
		Address:          strings.ToUpper(addr),
		AES:              len(out.aesChannels) > 0,
		AESChannels:      out.aesChannels,
		KeyIndexOverride: &ki,
	}
	if len(p.info.Serial) == 10 {
		dc.Serial = p.info.Serial
	}
	rc, err := b.cfg.ResolveStoreDevice(dc)
	if err != nil {
		metricPairing.WithLabelValues("failed").Inc()
		b.setResultLocked(fmt.Sprintf("pairing %s: %v", addr, err))
		return
	}
	rec := store.DeviceFromConfig(rc, p.info.FirmwareString(), fmt.Sprintf("%04X", p.info.ModelID), now)
	d, err := b.newDevice(rc, &rec)
	if err != nil {
		metricPairing.WithLabelValues("failed").Inc()
		b.setResultLocked(fmt.Sprintf("pairing %s: %v", addr, err))
		return
	}
	// The DEVICE_INFO counts as a message: the device is reachable now.
	upd := ha.State{LastSeen: now}
	if p.rssi != 0 {
		upd.RSSI = ptr(p.rssi)
	}
	b.devices = append(b.devices, d)
	b.byAddr[p.addr] = d
	b.applyUpdate(d, upd)
	delete(b.unknown, p.addr)
	metricUnknownSenders.Set(float64(len(b.unknown)))
	b.pair.active = false

	msg := fmt.Sprintf("paired %s (%s, %s, firmware %s", d.cfg.Name, addr, p.model.Name, p.info.FirmwareString())
	if len(out.aesChannels) > 0 {
		msg += fmt.Sprintf(", AES on channels %v with key index %d", out.aesChannels, out.keyIndex)
	} else if out.keyIndex > 0 {
		msg += fmt.Sprintf(", key index %d", out.keyIndex)
	}
	msg += ")"
	save = b.devicesSaverLocked()
	if len(out.notes) > 0 {
		msg += "; " + strings.Join(out.notes, "; ")
		metricPairing.WithLabelValues("paired_with_warnings").Inc()
	} else {
		metricPairing.WithLabelValues("paired").Inc()
	}
	savedMsg = msg
	b.setResultLocked(msg)
	b.publishNewDeviceLocked(d)
	if isActuatorKind(d.desc.Kind) {
		b.queue.push(statusJob(d))
	}
}

func isActuatorKind(k ha.Kind) bool { return k == ha.Switch || k == ha.Dimmer || k == ha.LEDDisplay }

// publishNewDeviceLocked publishes discovery, name, state and
// availability of a device added at runtime. Caller holds b.mu.
func (b *Bridge) publishNewDeviceLocked(d *device) {
	if b.pub == nil {
		return // PublishAll sends everything once MQTT is connected
	}
	topics := b.pub.Topics()
	msgs := ha.DiscoveryMessages(topics, d.desc)
	msgs = append(msgs, ha.StaleDiscoveryMessages(topics, d.desc)...)
	if d.stored != nil {
		msgs = append(msgs, ha.NameMessage(topics, d.desc.Addr, d.cfg.Name))
	}
	if !d.state.LastSeen.IsZero() {
		if m, err := ha.StateMessage(topics, d.desc.Addr, d.state); err == nil {
			msgs = append(msgs, m)
		}
	}
	msgs = append(msgs, ha.DeviceAvailabilityMessage(topics, d.desc.Addr, d.available))
	d.availPublished = true
	b.outbox.put(msgs...)
}

// uniqueNameLocked returns name, or name plus the address if another
// device has that name. Caller holds b.mu.
func (b *Bridge) uniqueNameLocked(name string, addr [3]byte) string {
	if !b.nameTakenLocked(name, addr) {
		return name
	}
	return fmt.Sprintf("%s %X", name, addr[:])
}

func (b *Bridge) nameTakenLocked(name string, except [3]byte) bool {
	for _, d := range b.devices {
		if d.cfg.Addr() != except && strings.EqualFold(d.cfg.Name, name) {
			return true
		}
	}
	return false
}

// unpairJob is the radio part of an unpairing.
type unpairJob struct {
	addr  [3]byte
	name  string
	model pairing.Model
	// send: send the unpair messages (else: only remove the peer from
	// the radio module and wait for the config button).
	send bool
	// peerRemoved: the radio module peer was removed by an earlier job.
	peerRemoved bool
}

// unpairDevice handles the “Unpair” command (event loop).
func (b *Bridge) unpairDevice(addrHex string) {
	now := b.now()
	var save func() error
	defer b.runDevicesSaver(&save) // after the unlock
	b.mu.Lock()
	defer b.mu.Unlock()
	defer b.publishPairingLocked(now)
	var d *device
	for _, cand := range b.devices {
		if cand.desc.Addr == addrHex {
			d = cand
		}
	}
	if d == nil {
		metricUnpair.WithLabelValues("rejected").Inc()
		b.setResultLocked(fmt.Sprintf("unpair: unknown device %s", addrHex))
		return
	}
	if d.stored == nil {
		metricUnpair.WithLabelValues("rejected").Inc()
		b.setResultLocked(fmt.Sprintf("%s (%s) is defined in config.json and cannot be unpaired from Home Assistant; remove it from config.json (and unpair it with the CCU, or factory-reset it)", d.cfg.Name, addrHex))
		return
	}
	model, ok := pairing.ModelByName(d.cfg.Model)
	if !ok {
		model = pairing.Model{Name: d.cfg.Model, RX: pairing.ConfigOnly}
	}
	b.removeDeviceLocked(d)
	save = b.devicesSaverLocked()
	j := unpairJob{addr: d.cfg.Addr(), name: d.cfg.Name, model: model}
	if model.RX == pairing.ConfigOnly {
		b.pair.unpairWait[j.addr] = unpairWaiting{until: now.Add(b.pair.duration), job: j}
		metricUnpair.WithLabelValues("waiting").Inc()
		b.setResultLocked(fmt.Sprintf("removed %s (%s); press its config button within %v to unpair the device itself", j.name, addrHex, b.pair.duration))
	} else {
		j.send = true
		b.setResultLocked(fmt.Sprintf("removed %s (%s); unpairing the device", j.name, addrHex))
	}
	b.queue.push(job{key: "unpair:" + addrHex, kind: jobUnpair, unpair: &j, priority: true})
}

// removeDeviceLocked removes d from the registry and from Home
// Assistant. Caller holds b.mu (event loop).
func (b *Bridge) removeDeviceLocked(d *device) {
	d.removed = true
	for i, cand := range b.devices {
		if cand == d {
			b.devices = append(b.devices[:i:i], b.devices[i+1:]...)
			break
		}
	}
	delete(b.byAddr, d.cfg.Addr())
	if b.pub != nil {
		b.outbox.put(ha.RemovalMessages(b.pub.Topics(), d.desc.Addr)...)
	}
	deleteDeviceMetrics(d.desc.Addr)
	b.stateDirty, b.significantDirty = true, true
}

// executeUnpair sends the unpair messages (worker goroutine).
func (b *Bridge) executeUnpair(ctx context.Context, u *unpairJob) error {
	if !u.peerRemoved {
		b.removePeer(ctx, u.addr)
	}
	if !u.send {
		return nil
	}
	flags := byte(pairing.ConfigFlags)
	if u.model.RX == pairing.WakeOnRadio {
		// Not in config mode: wake the thermostat with a burst.
		flags |= pairing.BurstFlag
	}
	for _, m := range pairing.PairSequence([3]byte{}, u.model.InternalKeysVisible) {
		if err := b.sendConfig(ctx, u.addr, flags, m); err != nil {
			return err
		}
	}
	return nil
}

// handleUnpairResult reports the outcome of the radio part (event loop).
func (b *Bridge) handleUnpairResult(u *unpairJob, err error) {
	if !u.send {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		metricUnpair.WithLabelValues("unconfirmed").Inc()
		b.setResultLocked(fmt.Sprintf("removed %s (%x), but the device did not confirm the unpairing (%v); factory-reset it before pairing it elsewhere", u.name, u.addr[:], err))
	} else {
		metricUnpair.WithLabelValues("unpaired").Inc()
		b.setResultLocked(fmt.Sprintf("unpaired %s (%x)", u.name, u.addr[:]))
	}
	b.publishPairingLocked(b.now())
}

// renameDevice handles the “Name” command of a paired device (event
// loop). The device entry is replaced (jobs of the old one are applied
// to the new one, see handleResult).
func (b *Bridge) renameDevice(addrHex, name string) {
	var save func() error
	defer b.runDevicesSaver(&save) // after the unlock
	b.mu.Lock()
	defer b.mu.Unlock()
	var d *device
	idx := -1
	for i, cand := range b.devices {
		if cand.desc.Addr == addrHex {
			d, idx = cand, i
		}
	}
	switch {
	case d == nil:
		b.logf("rename: unknown device %s", addrHex)
		return
	case d.stored == nil:
		b.logf("rename: %s (%s) is defined in config.json; rename it there", d.cfg.Name, addrHex)
		return
	case d.cfg.Name == name:
		return
	case b.nameTakenLocked(name, d.cfg.Addr()):
		b.logf("rename: %s (%s): the name %q is used by another device", d.cfg.Name, addrHex, name)
		if b.pub != nil {
			b.outbox.put(ha.NameMessage(b.pub.Topics(), addrHex, d.cfg.Name))
		}
		return
	}
	nc := *d.cfg
	nc.Name = name
	rec := *d.stored
	rec.Name = name
	nd, err := b.newDevice(&nc, &rec)
	if err != nil {
		b.logf("rename %s: %v", addrHex, err)
		return
	}
	// Carry the runtime state (state, counters, tracker) over.
	carried := *d
	carried.cfg, carried.dev, carried.desc, carried.stored = nd.cfg, nd.dev, nd.desc, nd.stored
	nd = &carried
	d.removed = true
	b.devices[idx] = nd
	b.byAddr[nd.cfg.Addr()] = nd
	deleteDeviceMetrics(addrHex)
	b.updateMetrics(nd)
	b.logf("renamed %s (%s) to %s", d.cfg.Name, addrHex, name)
	save = b.devicesSaverLocked()
	b.publishNewDeviceLocked(nd)
}

// loadStore adds the devices of the devices file (New). A file that
// cannot be read never leads to losing the paired devices: a damaged
// one has been moved aside by the store (a copy is kept), and one that
// must not be written (newer format version, symbolic link) disables
// pairing, unpairing and renaming. Both are shown on the status page.
func (b *Bridge) loadStore() {
	recs, err := b.store.LoadDevices()
	if err != nil {
		var ce *store.CorruptError
		switch {
		case b.store.DevicesBlocked() != nil:
			b.pair.enabled = false
			b.pair.disabledReason = fmt.Sprintf("the devices file cannot be used: %v", err)
			b.pair.warnings = append(b.pair.warnings, b.pair.disabledReason)
			b.logf("ERROR: paired devices not loaded, pairing disabled: %v", err)
		case errors.As(err, &ce):
			w := fmt.Sprintf("the devices file was damaged and paired devices were not loaded: %v; repair the moved file and copy it back while hmbridge is stopped", err)
			b.pair.warnings = append(b.pair.warnings, w)
			b.logf("ERROR: %s", w)
		default:
			w := fmt.Sprintf("paired devices not loaded: %v", err)
			b.pair.warnings = append(b.pair.warnings, w)
			b.logf("ERROR: %s", w)
		}
		return
	}
	n := 0
	for i := range recs {
		rec := recs[i]
		dc := rec.Config()
		rc, err := b.cfg.ResolveStoreDevice(dc)
		if err != nil {
			w := fmt.Sprintf("devices file: not using %s (%s): %v; it stays in the file until the configuration is fixed", rec.Name, rec.Address, err)
			b.logf("WARNING: %s", w)
			b.pair.warnings = append(b.pair.warnings, w)
			b.pair.kept = append(b.pair.kept, rec)
			continue
		}
		if c := b.cfg.Device(rc.Addr()); c != nil {
			b.logf("devices file: %s (%s) is also in config.json as %q; config.json wins", rec.Name, rc.AddrHex(), c.Name)
			continue
		}
		if _, dup := b.byAddr[rc.Addr()]; dup {
			b.logf("devices file: %s listed twice; ignoring the second entry", rc.AddrHex())
			continue
		}
		if b.nameTakenLocked(rc.Name, rc.Addr()) {
			newName := fmt.Sprintf("%s %s", rc.Name, strings.ToUpper(rc.AddrHex()))
			b.logf("devices file: the name %q of %s is used in config.json; calling it %q", rc.Name, rc.AddrHex(), newName)
			rc.Name = newName
			rec.Name = newName
		}
		d, err := b.newDevice(rc, &rec)
		if err != nil {
			b.logf("devices file: %v", err)
			continue
		}
		b.devices = append(b.devices, d)
		b.byAddr[rc.Addr()] = d
		n++
	}
	metricPairedDevices.Set(float64(n))
	if n > 0 {
		b.logf("loaded %d paired devices from %s", n, b.store.DevicesPath())
	}
}

// devicesSaverLocked snapshots the paired devices and returns the
// function that writes them to the devices file. Caller holds b.mu; the
// returned function must be called without it (the write fsyncs, which
// may be slow on an SD card). It is only called when a device is
// paired, unpaired or renamed. Successive snapshots are written in
// order: an older one is never written after a newer one.
func (b *Bridge) devicesSaverLocked() func() error {
	var recs []store.Device
	for _, d := range b.devices {
		if d.stored != nil {
			recs = append(recs, *d.stored)
		}
	}
	metricPairedDevices.Set(float64(len(recs)))
	// Records that could not be used are kept, unless their address
	// has been paired again since.
	for _, k := range b.pair.kept {
		if a, err := hex.DecodeString(k.Address); err == nil && len(a) == 3 {
			if _, taken := b.byAddr[[3]byte(a)]; taken {
				continue
			}
		}
		recs = append(recs, k)
	}
	b.devicesSeq++
	seq := b.devicesSeq
	return func() error {
		b.devicesMu.Lock()
		defer b.devicesMu.Unlock()
		if seq < b.devicesWritten {
			return nil // a newer snapshot has been written already
		}
		b.devicesWritten = seq
		return b.store.SaveDevices(recs)
	}
}

// runDevicesSaver calls *save (if set) and logs a failure. Deferred
// before b.mu is locked, so that it runs after the unlock.
func (b *Bridge) runDevicesSaver(save *func() error) {
	if *save == nil {
		return
	}
	if err := (*save)(); err != nil {
		b.logf("saving %s: %v", b.store.DevicesPath(), err)
	}
}

// deviceMetricVecs are the metrics with an "address" label.
var deviceMetricVecs = []interface {
	DeletePartialMatch(prometheus.Labels) int
}{
	metricLastSeen, metricRSSI, metricMessages, metricBatteryVolts, metricBatteryPercent,
	metricLowBattery, metricWindowOpen, metricCurrentTemp, metricTargetTemp, metricValve,
	metricHumidity, metricSabotage, metricFault, metricAuth, metricForeignDest, metricAvailable,
	metricUnauthenticated, metricDecodeErrors, metricLevel, metricLEDColor, metricButtonPresses,
	metricStatusPolls,
}

// deleteDeviceMetrics removes the metric series of a device.
func deleteDeviceMetrics(addr string) {
	for _, v := range deviceMetricVecs {
		v.DeletePartialMatch(prometheus.Labels{"address": addr})
	}
}
