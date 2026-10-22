// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

// Heating control by Home Assistant ("control": "home_assistant" on a
// heating group or on a thermostat outside groups): Home Assistant (e.g.
// the Advanced Heating Control blueprint) decides the target
// temperature, hmbridge is a thin bridge with a fail-safe.
//
//   - Manual mode: the thermostats are kept in manual mode, so that their
//     week program does not change the target temperature behind Home
//     Assistant's back. A member that reports auto (device program,
//     button on the device) gets manual mode with the intended target
//     temperature again (enforceManual), at most once per command
//     receiver and enforceInterval, not while a window of the target is
//     open or a boost runs. After startup, members whose mode is not known
//     get it once, too.
//   - Setpoints: target temperatures from Home Assistant are debounced per
//     group or thermostat (setpoint_debounce): only the latest is sent,
//     as manual mode with that temperature.
//   - Fail-safe: when the MQTT connection (or Home Assistant, by its
//     status topic) has been gone for fallback_after, every target is
//     switched to auto (the devices' week program) once; when the link
//     has been back for reconnectGrace, Home Assistant's last mode and
//     setpoint are applied again (manual mode). Without a known setpoint
//     the target stays on the week program until Home Assistant sends a
//     command. Any climate command from Home Assistant for a target in
//     fallback hands it back at once. The state survives restarts
//     (control.json in state_dir).

import (
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/store"
)

// Defaults of the control timing (Options override them for tests).
const (
	// defaultControlTick is how often the link is checked and
	// enforcement is retried.
	defaultControlTick = 10 * time.Second
	// defaultReconnectGrace is how long the link must be back before
	// control is handed back to Home Assistant after a fallback.
	defaultReconnectGrace = time.Minute
	// defaultEnforceInterval is the least time between two “keep manual
	// mode” commands to the same device (battery, duty cycle).
	defaultEnforceInterval = 10 * time.Minute
	// defaultControlStartupDelay is when after startup the members whose
	// mode is not known get manual mode (the retained state from the
	// broker is read in the meantime).
	defaultControlStartupDelay = time.Minute
)

// controlTarget is a heating group or a thermostat outside groups under
// Home Assistant control. Event loop only, except fallback, which is
// written under b.mu (it is read when states are published).
type controlTarget struct {
	key  string // "group:<slug>" or "device:<addr>" (control.json, debounce)
	kind string // "group" or "device" (metric label)
	name string
	// group is nil for a thermostat outside groups.
	group *group
	// members are the thermostats whose control mode is watched.
	members []*device
	// targets receive the target's commands (a group's command order,
	// via_wall_thermostat honoured).
	targets       []*device
	fallbackAfter time.Duration

	// haMode is the last mode Home Assistant asked for (ha.ModeHeat or
	// ha.ModeOff, "" = unknown), haSetpoint its last target temperature
	// above off (0 = unknown).
	haMode     string
	haSetpoint float64

	// fallback: the thermostats were switched to their week program
	// because Home Assistant was unreachable (guarded by b.mu).
	fallback bool
	// waitLogged: “waiting for a setpoint” was logged for this fallback.
	waitLogged bool
	// lastEnforce is when a receiver last got a manual mode command.
	lastEnforce map[string]time.Time
}

// controlValue is the value of the “Control” sensor (State.Control) of a
// thermostat or group controlled by t (nil: the device). Caller holds
// b.mu or runs on the event loop.
func (t *controlTarget) controlValue() string {
	switch {
	case t == nil:
		return ha.ControlDevice
	case t.fallback:
		return ha.ControlFallback
	}
	return ha.ControlHomeAssistant
}

// pendingSetpoint is a debounced target temperature from Home Assistant.
type pendingSetpoint struct {
	t         *controlTarget
	receivers []*device
	cmd       ha.Command
	intent    bool
	count     int
	last      time.Time // wall clock (time.Now), not b.now
	what      string
}

// persistedControl is a target's entry in control.json.
type persistedControl struct {
	Fallback   bool    `json:"fallback,omitempty"`
	HAMode     string  `json:"ha_mode,omitempty"`
	HASetpoint float64 `json:"ha_setpoint,omitempty"`
}

// controlFile is the content of control.json.
type controlFile struct {
	Version int                         `json:"version"`
	Targets map[string]persistedControl `json:"targets"`
}

// setupControl creates the control targets of the configuration. Called
// by New after the groups were built.
func (b *Bridge) setupControl(opts Options) {
	b.debounce = b.cfg.SetpointDebounce()
	b.debounceC = make(chan string, 64)
	b.pending = make(map[string]*pendingSetpoint)
	b.ctlTick = durationOr(opts.ControlTick, defaultControlTick)
	b.reconnectGrace = durationOr(opts.ReconnectGrace, defaultReconnectGrace)
	b.enforceInterval = durationOr(opts.EnforceInterval, defaultEnforceInterval)
	b.ctlStartupDelay = durationOr(opts.ControlStartupDelay, defaultControlStartupDelay)

	for _, rg := range b.cfg.ResolvedGroups() {
		if !rg.HAControl {
			continue
		}
		g := b.bySlug[ha.Slugify(rg.Name)]
		t := &controlTarget{key: "group:" + g.slug, kind: "group", name: g.name, group: g,
			members: g.thermostats, targets: g.targets, fallbackAfter: rg.FallbackAfter}
		g.ctl = t
		g.desc.HAControl = true
		for _, d := range g.thermostats {
			d.ctl = t
			d.desc.HAControl = true
		}
		b.ctlTargets = append(b.ctlTargets, t)
	}
	for _, d := range b.devices {
		if d.group != nil || !d.cfg.HAControlled() {
			continue
		}
		t := &controlTarget{key: "device:" + d.desc.Addr, kind: "device", name: d.cfg.Name,
			members: []*device{d}, targets: []*device{d}, fallbackAfter: d.cfg.FallbackTimeout()}
		d.ctl = t
		d.desc.HAControl = true
		b.ctlTargets = append(b.ctlTargets, t)
	}
	for _, t := range b.ctlTargets {
		t.lastEnforce = make(map[string]time.Time)
		metricFallbackActive.WithLabelValues(t.kind, t.name).Set(0)
	}
	if len(b.ctlTargets) > 0 {
		b.ctlFile = b.store.Control()
		b.loadControl()
	}
	b.refreshControlState()
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// refreshControlState sets the Control field of every thermostat's and
// group's state. Caller holds b.mu (or b is not running yet).
func (b *Bridge) refreshControlState() {
	for _, d := range b.devices {
		deriveState(d)
	}
	for _, g := range b.groups {
		g.state = groupState(g)
	}
}

// loadControl restores the fallback state and Home Assistant's last
// setpoints from control.json.
func (b *Bridge) loadControl() {
	if b.ctlFile == nil {
		return
	}
	var cf controlFile
	found, err := b.ctlFile.Load(&cf)
	if err != nil {
		b.logf("control state: %v", err)
		return
	}
	if !found {
		return
	}
	for _, t := range b.ctlTargets {
		p, ok := cf.Targets[t.key]
		if !ok {
			continue
		}
		if p.HAMode == ha.ModeHeat || p.HAMode == ha.ModeOff {
			t.haMode = p.HAMode
		}
		if validSetpoint(p.HASetpoint) {
			t.haSetpoint = p.HASetpoint
		}
		if p.Fallback {
			t.fallback = true
			metricFallbackActive.WithLabelValues(t.kind, t.name).Set(1)
			b.logf("%s: the fail-safe was active before the restart: the thermostats stay on their week program until Home Assistant is reachable again", t.name)
		}
	}
}

// validSetpoint reports whether v is a target temperature above off.
func validSetpoint(v float64) bool {
	return v > ha.MinTemp && v <= ha.MaxTemp && math.Mod(v, ha.TempStep) == 0
}

// saveControl writes control.json (nothing if unchanged). Rare: on
// fallback transitions and on shutdown.
func (b *Bridge) saveControl() {
	if b.ctlFile == nil || len(b.ctlTargets) == 0 {
		return
	}
	cf := controlFile{Version: store.ControlVersion, Targets: make(map[string]persistedControl)}
	b.mu.Lock()
	for _, t := range b.ctlTargets {
		cf.Targets[t.key] = persistedControl{Fallback: t.fallback, HAMode: t.haMode, HASetpoint: t.haSetpoint}
	}
	b.mu.Unlock()
	if _, err := b.ctlFile.Save(cf); err != nil {
		b.logf("saving the control state: %v", err)
	}
}

// handleHAClimateCommand handles a climate command (temperature, mode,
// preset) for a group or thermostat under Home Assistant control. It
// returns false for targets under device control.
func (b *Bridge) handleHAClimateCommand(cmd ha.Command) bool {
	var (
		t         *controlTarget
		receivers []*device
		key, what string
		intent    bool
	)
	if cmd.Group != "" {
		g := b.bySlug[cmd.Group]
		if g == nil || g.ctl == nil {
			return false
		}
		t, receivers, key, what, intent = g.ctl, g.targets, g.ctl.key, "group "+g.name, true
	} else {
		d := b.byAddr[addrOf(cmd.Addr)]
		if d == nil || d.removed || d.ctl == nil {
			return false
		}
		// A member's own climate entity commands only that member; it
		// does not change the group's setpoint.
		t, receivers, key, what, intent = d.ctl, []*device{d}, "device:"+d.desc.Addr, d.cfg.Name, d.ctl.group == nil
	}

	if cmd.Kind == ha.SetMode && cmd.Mode == ha.ModeAuto {
		b.logf("command for %s: mode auto rejected: Home Assistant controls the heating (control home_assistant)", what)
		metricCommands.WithLabelValues(string(actionAuto), "rejected").Inc()
		for _, d := range receivers {
			b.mu.Lock()
			b.applyUpdate(d, ha.State{LastCommand: "rejected: auto (control is home_assistant)"})
			b.mu.Unlock()
			b.publishDevice(d)
		}
		return true
	}
	if intent {
		switch cmd.Kind {
		case ha.SetTemperature:
			if cmd.Temperature <= ha.MinTemp {
				t.haMode = ha.ModeOff
			} else {
				t.haMode, t.haSetpoint = ha.ModeHeat, cmd.Temperature
			}
		case ha.SetMode:
			t.haMode = cmd.Mode
		}
		if t.fallback {
			b.endFallback(t, "Home Assistant sent a command", false)
		}
	}
	if cmd.Kind == ha.SetTemperature && b.debounce > 0 {
		p := b.pending[key]
		if p == nil {
			p = &pendingSetpoint{}
			b.pending[key] = p
		}
		p.t, p.receivers, p.cmd, p.intent, p.what = t, receivers, cmd, intent, what
		p.count++
		p.last = time.Now()
		c := b.debounceC
		time.AfterFunc(b.debounce, func() {
			select {
			case c <- key:
			default: // the control tick flushes it
			}
		})
		return true
	}
	// A mode or preset command replaces a setpoint still waiting: its
	// value is already recorded as Home Assistant's setpoint.
	delete(b.pending, key)
	b.sendHA(t, receivers, cmd, 1, what)
	return true
}

// addrOf parses a hex address (invalid: zero).
func addrOf(s string) [3]byte {
	var a [3]byte
	if b, err := hex.DecodeString(s); err == nil && len(b) == 3 {
		copy(a[:], b)
	}
	return a
}

// flushPending sends the debounced setpoint key once it has been quiet
// for the debounce time (or at once with force).
func (b *Bridge) flushPending(key string, force bool) {
	p := b.pending[key]
	if p == nil || !force && time.Since(p.last) < b.debounce {
		return
	}
	delete(b.pending, key)
	b.sendHA(p.t, p.receivers, p.cmd, p.count, p.what)
}

// sendHA queues the radio jobs of a Home Assistant command for a target
// under Home Assistant control. merged is the number of setpoints the
// command stands for (debouncing).
func (b *Bridge) sendHA(t *controlTarget, receivers []*device, cmd ha.Command, merged int, what string) {
	note := ""
	if merged > 1 {
		note = fmt.Sprintf("latest of %d setpoints", merged)
	}
	now := b.now()
	for _, d := range receivers {
		action, temp, err := b.translateHA(t, d, cmd)
		if err == errNoop {
			continue
		}
		if err != nil {
			b.logf("command for %s: %v", what, err)
			metricCommands.WithLabelValues(string(action), "rejected").Inc()
			continue
		}
		b.logf("%s: queueing %s %v (%s)", d.cfg.Name, action, temp, what)
		b.pushClimate(d, action, temp, note)
		if action == actionManual {
			// The device may still report auto before it gets this.
			t.lastEnforce[d.desc.Addr] = now
		}
	}
}

// pushClimate queues a climate job for d (coalesced and superseded like
// the commands from Home Assistant).
func (b *Bridge) pushClimate(d *device, action climateAction, temp float64, note string) {
	b.queue.push(job{
		key:    fmt.Sprintf("climate:%s:%s", d.desc.Addr, actionClass(action)),
		target: "climate:" + d.desc.Addr,
		kind:   jobClimate,
		dev:    d,
		action: action,
		temp:   temp,
		note:   note,
	})
}

// translateHA maps a climate command to a device action under Home
// Assistant control: the device stays in manual mode, so a target
// temperature is sent as manual mode with that temperature (which also
// ends a running boost), heat as manual mode with Home Assistant's last
// setpoint, off as manual mode at 4.5 °C.
func (b *Bridge) translateHA(t *controlTarget, d *device, cmd ha.Command) (climateAction, float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch cmd.Kind {
	case ha.SetTemperature:
		return actionManual, math.Max(cmd.Temperature, ha.MinTemp), nil
	case ha.SetMode:
		switch cmd.Mode {
		case ha.ModeHeat:
			return actionManual, haOnTarget(t, d), nil
		case ha.ModeOff:
			return actionManual, ha.MinTemp, nil
		}
		return "", 0, fmt.Errorf("unsupported mode %q", cmd.Mode)
	case ha.SetPreset:
		switch cmd.Preset {
		case ha.PresetBoost:
			return actionBoost, 0, nil
		case ha.PresetNone:
			if d.state.ControlMode != "boost" {
				return "", 0, errNoop
			}
			if d.preBoostHAMode == ha.ModeOff {
				return actionManual, ha.MinTemp, nil
			}
			return actionManual, haOnTarget(t, d), nil
		}
		return "", 0, fmt.Errorf("unsupported preset %q", cmd.Preset)
	}
	return "", 0, fmt.Errorf("unsupported command kind %v", cmd.Kind)
}

// haOnTarget is the temperature for “heat”: Home Assistant's last
// setpoint, else the device's last one above off.
func haOnTarget(t *controlTarget, d *device) float64 {
	if t != nil && t.haSetpoint > ha.MinTemp {
		return t.haSetpoint
	}
	return comfortTarget(d)
}

// intendedTemp is the temperature d should hold in manual mode: what
// Home Assistant asked for last (off: 4.5 °C), else the device's current
// target temperature. Caller holds b.mu.
func intendedTemp(t *controlTarget, d *device) float64 {
	switch t.haMode {
	case ha.ModeOff:
		return ha.MinTemp
	case ha.ModeHeat:
		if t.haSetpoint > ha.MinTemp {
			return t.haSetpoint
		}
	}
	if tt := d.state.TargetTemperature; tt != nil && *tt >= ha.MinTemp && *tt <= ha.MaxTemp {
		return *tt
	}
	return comfortTarget(d)
}

// needsManual reports whether a control mode is one Home Assistant
// control does not want (the week program runs). startup also counts
// an unknown mode.
func needsManual(controlMode string, startup bool) bool {
	return controlMode == "auto" || controlMode == "party" || startup && controlMode == ""
}

// enforceManual switches the members of t that run their week program
// back to manual mode (see the package comment above for the rules).
// Runs on the event loop.
func (b *Bridge) enforceManual(t *controlTarget, startup bool) {
	if t.fallback {
		return
	}
	if _, waiting := b.pending[t.key]; waiting {
		return // the setpoint is sent as manual mode anyway
	}
	b.mu.Lock()
	var need []*device
	window, boost := false, false
	for _, d := range t.members {
		if needsManual(d.state.ControlMode, startup) {
			need = append(need, d)
		}
		boost = boost || d.state.ControlMode == "boost"
		window = window || d.state.WindowOpen != nil && *d.state.WindowOpen
	}
	if g := t.group; g != nil {
		for _, c := range g.contacts {
			window = window || c.state.WindowOpen != nil && *c.state.WindowOpen
		}
	}
	b.mu.Unlock()
	if len(need) == 0 || window || boost {
		return
	}
	// The receivers among the command targets; with via_wall_thermostat
	// a radiator thermostat is reached through the wall thermostat.
	var receivers []*device
	for _, r := range t.targets {
		for _, d := range need {
			if d == r {
				receivers = append(receivers, r)
			}
		}
	}
	// source is the device whose target temperature stands for a
	// receiver's while Home Assistant's is unknown: the receiver itself,
	// or with via_wall_thermostat the member that reported auto.
	var source *device
	if len(receivers) == 0 {
		receivers, source = t.targets, need[0]
	}
	now := b.now()
	for _, d := range receivers {
		if b.queue.has(fmt.Sprintf("climate:%s:mode", d.desc.Addr)) {
			continue
		}
		if last, ok := t.lastEnforce[d.desc.Addr]; ok && now.Sub(last) < b.enforceInterval {
			b.debugf("%s: not switching back to manual mode yet (last time %v ago)", d.cfg.Name, now.Sub(last).Round(time.Second))
			continue
		}
		src := d
		if source != nil {
			src = source
		}
		b.mu.Lock()
		temp := intendedTemp(t, src)
		b.mu.Unlock()
		why := "reported auto mode"
		if startup {
			why = "startup"
		}
		b.logf("%s: keeping it in manual mode at %.1f °C for Home Assistant (%s, %s)", d.cfg.Name, temp, t.name, why)
		b.pushClimate(d, actionManual, temp, "keep manual mode")
		t.lastEnforce[d.desc.Addr] = now
	}
}

// controlAfterFrame is called after a frame of d was applied.
func (b *Bridge) controlAfterFrame(d *device) {
	t := d.ctl
	if t == nil {
		return
	}
	b.mu.Lock()
	mode := d.state.ControlMode
	b.mu.Unlock()
	if needsManual(mode, false) {
		b.enforceManual(t, false)
	}
}

// linkUpNow reports whether Home Assistant can reach hmbridge: the MQTT
// client is attached and connected, and Home Assistant has not announced
// "offline" on its status topic.
func (b *Bridge) linkUpNow() bool {
	b.mu.Lock()
	pub := b.pub
	b.mu.Unlock()
	if pub == nil {
		return false
	}
	if c, ok := pub.(interface{ Connected() bool }); ok && !c.Connected() {
		return false
	}
	if h, ok := pub.(interface{ HAOnline() bool }); ok && !h.HAOnline() {
		return false
	}
	return true
}

// controlTick checks the link to Home Assistant (fail-safe and hand
// back), flushes debounced setpoints and retries enforcement. Runs on
// the event loop.
func (b *Bridge) controlTick() {
	if len(b.ctlTargets) == 0 {
		return
	}
	for key := range b.pending {
		b.flushPending(key, false)
	}
	now := b.now()
	up := b.linkUpNow()
	if !b.linkKnown || up != b.linkUp {
		since := now
		if !b.linkKnown && !up {
			since = b.started // down since startup
		}
		if b.linkKnown {
			if up {
				b.logf("heating control: Home Assistant is reachable again (MQTT)")
			} else {
				b.logf("heating control: Home Assistant is not reachable (MQTT connection or Home Assistant down)")
			}
		}
		b.linkUp, b.linkSince, b.linkKnown = up, since, true
	}
	for _, t := range b.ctlTargets {
		switch {
		case !up && !t.fallback && t.fallbackAfter > 0 && now.Sub(b.linkSince) >= t.fallbackAfter:
			b.startFallback(t, now.Sub(b.linkSince))
		case up && t.fallback && now.Sub(b.linkSince) >= b.reconnectGrace:
			b.handBack(t)
		}
	}
	if !b.ctlStartupDone && now.Sub(b.started) >= b.ctlStartupDelay {
		b.ctlStartupDone = true
		for _, t := range b.ctlTargets {
			b.enforceManual(t, true)
		}
		return
	}
	for _, t := range b.ctlTargets {
		b.enforceManual(t, false)
	}
}

// setFallback switches t's fallback state and publishes the change.
func (b *Bridge) setFallback(t *controlTarget, on bool) {
	b.mu.Lock()
	t.fallback = on
	t.waitLogged = false
	for _, d := range t.members {
		deriveState(d)
	}
	b.mu.Unlock()
	metricFallbackActive.WithLabelValues(t.kind, t.name).Set(boolFloat(on))
	for _, d := range t.members {
		b.publishDevice(d) // also the group's state
	}
	b.saveControl()
}

// startFallback switches t's thermostats to their week program because
// Home Assistant has been unreachable for down.
func (b *Bridge) startFallback(t *controlTarget, down time.Duration) {
	delete(b.pending, t.key)
	b.logf("%s: Home Assistant unreachable for %v (fallback_after %v): FAIL-SAFE, switching the thermostats to their week program (auto mode)", t.name, down.Round(time.Second), t.fallbackAfter)
	b.setFallback(t, true)
	for _, d := range t.targets {
		b.pushClimate(d, actionAuto, 0, "fail-safe: Home Assistant unreachable")
	}
}

// handBack returns t to Home Assistant once the link has been back for
// reconnectGrace: its last mode and setpoint in manual mode. Without them
// the week program keeps running until Home Assistant sends a command.
func (b *Bridge) handBack(t *controlTarget) {
	if t.haMode == "" {
		if !t.waitLogged {
			t.waitLogged = true
			b.logf("%s: Home Assistant is reachable again, but its setpoint is not known: the week program keeps running until Home Assistant sends a target temperature or mode", t.name)
		}
		return
	}
	b.endFallback(t, "Home Assistant is reachable again", true)
}

// endFallback ends t's fallback; with send, Home Assistant's last mode
// and setpoint are sent (otherwise the command that ended it does that).
func (b *Bridge) endFallback(t *controlTarget, why string, send bool) {
	b.logf("%s: %s: handing the heating control back to Home Assistant", t.name, why)
	b.setFallback(t, false)
	if !send {
		return
	}
	now := b.now()
	for _, d := range t.targets {
		b.mu.Lock()
		temp := intendedTemp(t, d)
		b.mu.Unlock()
		b.pushClimate(d, actionManual, temp, "Home Assistant back")
		t.lastEnforce[d.desc.Addr] = now
	}
}
