// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

package bridge

// State persistence, designed to be gentle on a Raspberry Pi's SD card:
//
//  1. The primary source after a restart is the broker: hmbridge
//     publishes every device's state as a retained MQTT message, and at
//     startup it reads those messages back (SeedFromRetained). This
//     costs no writes on the Pi at all.
//  2. The state file is only a fallback for when the broker is not
//     reachable at startup. It is written by a separate goroutine (never
//     blocking radio handling), only when something significant changed
//     (mode, target, window, battery, fault — not every temperature or
//     RSSI report), at most every state_save_interval (default 15m),
//     and once on a clean shutdown.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fjaeckel/hmgo/internal/ha"
)

// persistedDevice is the on-disk form of a device's state.
type persistedDevice struct {
	State          ha.State `json:"state"`
	LastOnTarget   float64  `json:"last_on_target,omitempty"`
	PreBoostHAMode string   `json:"pre_boost_ha_mode,omitempty"`
}

type persistedState struct {
	Version int                        `json:"version"`
	Saved   time.Time                  `json:"saved"`
	Devices map[string]persistedDevice `json:"devices"` // by lowercase address
}

// defaultBoostMinutes is assumed for a restored boost without a known
// remaining time (the devices' default boost duration is 5 minutes).
const defaultBoostMinutes = 5

// restore installs a previously saved state for d. Caller holds b.mu
// (or b is not running yet).
func (b *Bridge) restore(d *device, st ha.State, lastOnTarget float64, preBoost string) {
	// Counters and command outcomes describe this bridge run only.
	st.MessagesReceived = nil
	st.LastCommand = ""
	// A boost that has expired since the state was saved is over; don't
	// show it (and don't let “preset none” send a command for it).
	if st.ControlMode == "boost" {
		mins := defaultBoostMinutes
		if st.BoostMinutes != nil && *st.BoostMinutes > 0 {
			mins = *st.BoostMinutes
		}
		if st.LastSeen.IsZero() || b.now().After(st.LastSeen.Add(time.Duration(mins)*time.Minute)) {
			st.ControlMode = ""
			st.Preset = ha.PresetNone
			st.BoostMinutes = ptr(0)
			st.Mode = preBoost // "" = unknown until the next report
		}
	}
	d.state = st
	if lastOnTarget <= ha.MinTemp {
		if t := st.TargetTemperature; t != nil && *t > ha.MinTemp && *t < ha.MaxTemp {
			lastOnTarget = *t
		}
	}
	d.lastOnTarget = lastOnTarget
	d.preBoostHAMode = preBoost
	deriveState(d)
}

// loadState restores the last known device state from the state file. A
// missing file is not an error.
func (b *Bridge) loadState(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		return fmt.Errorf("%s: %v (ignoring it)", path, err)
	}
	n := 0
	for _, d := range b.devices {
		pd, ok := ps.Devices[d.desc.Addr]
		if !ok || pd.State.LastSeen.IsZero() {
			continue
		}
		b.restore(d, pd.State, pd.LastOnTarget, pd.PreBoostHAMode)
		n++
	}
	for _, g := range b.groups {
		g.state = groupState(g)
	}
	b.logf("restored the last known state of %d devices from %s (saved %v)", n, path, ps.Saved.Format(time.RFC3339))
	return nil
}

// SeedFromRetained restores device state from the retained state
// messages hmbridge published before (see ha.Client.ReadRetained). A
// device is only seeded if nothing newer is known about it (e.g. from
// the state file, or a message received since startup). Call it before
// SetPublisher.
func (b *Bridge) SeedFromRetained(topics ha.TopicConfig, msgs map[string][]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for topic, payload := range msgs {
		addr, _, ok := topics.ParseStateTopic(topic)
		if !ok || addr == "" || len(payload) == 0 {
			continue // group states are derived from the members
		}
		var d *device
		for _, cand := range b.devices {
			if cand.desc.Addr == addr {
				d = cand
			}
		}
		if d == nil || d.heardSinceStart {
			continue
		}
		var st ha.State
		if err := json.Unmarshal(payload, &st); err != nil {
			b.logf("ignoring retained state of %s: %v", addr, err)
			continue
		}
		if st.LastSeen.IsZero() || !st.LastSeen.After(d.state.LastSeen) {
			continue
		}
		b.restore(d, st, d.lastOnTarget, d.preBoostHAMode)
		b.stateDirty = true
		n++
	}
	for _, g := range b.groups {
		g.state = groupState(g)
	}
	b.checkAvailability(b.now())
	if n > 0 {
		b.logf("restored the last known state of %d devices from the MQTT broker", n)
	}
}

// significantKey summarises the fields whose change is worth writing the
// state file for. Temperatures, valve position, RSSI, last seen and
// counters change with every report and are deliberately left out.
func significantKey(d *device) string {
	st := &d.state
	f := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprintf("%.1f", *p)
	}
	bl := func(p *bool) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%.1f|%s",
		st.Mode, st.Preset, st.ControlMode, f(st.TargetTemperature), st.WindowState,
		bl(st.WindowOpen), bl(st.LowBattery), bl(st.Sabotage), st.Fault,
		f(st.BatteryVoltage), d.lastOnTarget, d.preBoostHAMode)
}

// saveState writes the current device state to path atomically. Caller
// must not hold b.mu. On failure the state stays dirty, so the next
// attempt writes it.
func (b *Bridge) saveState(path string) error {
	b.mu.Lock()
	ps := persistedState{Version: 1, Saved: b.now(), Devices: make(map[string]persistedDevice)}
	for _, d := range b.devices {
		if d.state.LastSeen.IsZero() {
			continue
		}
		var st ha.State
		st.Merge(d.state)
		ps.Devices[d.desc.Addr] = persistedDevice{State: st, LastOnTarget: d.lastOnTarget, PreBoostHAMode: d.preBoostHAMode}
	}
	b.stateDirty, b.significantDirty = false, false
	b.mu.Unlock()

	err := writeFileAtomic(path, ps)
	if err != nil {
		b.mu.Lock()
		b.stateDirty, b.significantDirty = true, true
		b.mu.Unlock()
	}
	return err
}

func writeFileAtomic(path string, v any) error {
	data, err := json.Marshal(v) // compact: fewer bytes written
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// saver writes the state file whenever the event loop asks for it, so
// that file I/O (and fsync on a slow SD card) never delays radio
// handling. Write errors are logged once until a write succeeds.
func (b *Bridge) saver(stop <-chan struct{}) {
	failing := false
	for {
		select {
		case <-stop:
			return
		case <-b.saveReq:
		}
		if err := b.saveState(b.stateFile); err != nil {
			if !failing {
				b.logf("saving state to %s: %v (will retry; set state_file to a writable path or \"\" to disable)", b.stateFile, err)
				failing = true
			}
			continue
		}
		if failing {
			b.logf("saving state to %s works again", b.stateFile)
			failing = false
		}
	}
}

// requestSave asks the saver to write the state file if something
// significant changed and the save interval has passed. Runs on the
// event loop.
func (b *Bridge) requestSave(now time.Time) {
	if b.stateFile == "" {
		return
	}
	b.mu.Lock()
	due := b.significantDirty && now.Sub(b.lastSave) >= b.saveInterval
	b.mu.Unlock()
	if !due {
		return
	}
	b.lastSave = now
	select {
	case b.saveReq <- struct{}{}:
	default: // a save is pending already
	}
}

// checkAvailability marks devices that have been silent for longer than
// their stale timeout as unavailable (and back), and groups as available
// while at least one of their thermostats is. Staleness is measured from
// the device's last message, also across restarts (restored state); a
// device never heard at all is measured from the start of the bridge.
// It returns the availability messages to publish. Caller holds b.mu.
func (b *Bridge) checkAvailability(now time.Time) []ha.Message {
	var msgs []ha.Message
	var topics ha.TopicConfig
	if b.pub != nil {
		topics = b.pub.Topics()
	}
	for _, d := range b.devices {
		ref := d.state.LastSeen
		if ref.IsZero() {
			ref = b.started
		}
		online := now.Sub(ref) < d.cfg.StaleTimeout()
		if online != d.available || !d.availPublished {
			if online != d.available {
				if online {
					b.logf("%s (%s) is reporting again", d.cfg.Name, d.desc.Addr)
				} else {
					b.logf("%s (%s) has been silent since %v; marking it unavailable (battery empty or out of range?)", d.cfg.Name, d.desc.Addr, lastSeenString(d.state.LastSeen))
				}
			}
			d.available = online
			if b.pub != nil {
				d.availPublished = true
				msgs = append(msgs, ha.DeviceAvailabilityMessage(topics, d.desc.Addr, online))
			}
		}
		metricAvailable.WithLabelValues(d.desc.Addr, d.cfg.Name, d.cfg.Model).Set(boolFloat(online))
	}
	for _, g := range b.groups {
		online := false
		for _, d := range g.thermostats {
			online = online || d.available
		}
		if online != g.available || !g.availPublished {
			g.available = online
			if b.pub != nil {
				g.availPublished = true
				msgs = append(msgs, ha.GroupAvailabilityMessage(topics, g.slug, online))
			}
		}
	}
	return msgs
}

func lastSeenString(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

// SystemClockSynced reports whether the kernel considers the system
// clock synchronised (systemd-timesyncd, chrony and ntpd clear
// STA_UNSYNC once they have synchronised).
func SystemClockSynced() bool {
	var tx unix.Timex
	if _, err := unix.Adjtimex(&tx); err != nil {
		return true // can't tell; don't block time handling
	}
	return tx.Status&unix.STA_UNSYNC == 0
}
