// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

// Switches, dimmers, the LED display, and remotes.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm/led16"
	"github.com/fjaeckel/hmgo/internal/hm/power"
	"github.com/fjaeckel/hmgo/internal/hm/remote"
)

// DefaultRampTime is the dimmer's ramp time when Home Assistant gives
// no transition (the dimmer's own default, RAMP_TIME 0.5 s in eQ-3's
// rftypes).
const DefaultRampTime = 0.5

// actuators returns the devices that are polled for their status.
func (b *Bridge) actuators() []*device {
	var res []*device
	for _, d := range b.devices {
		switch d.desc.Kind {
		case ha.Switch, ha.Dimmer, ha.LEDDisplay:
			res = append(res, d)
		}
	}
	return res
}

func statusJob(d *device) job {
	return job{key: "status:" + d.desc.Addr, kind: jobStatus, dev: d}
}

// initialStatusPolls asks every actuator for its status shortly after
// startup, one after the other, so that Home Assistant does not show
// them as unknown until they change.
func (b *Bridge) initialStatusPolls(ctx context.Context, actuators []*device) {
	for _, d := range actuators {
		t := time.NewTimer(b.pollStagger)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		b.queue.push(statusJob(d))
	}
}

// handleActuatorCommand turns switch, light and LED commands into jobs.
// Runs on the event loop.
func (b *Bridge) handleActuatorCommand(cmd ha.Command) {
	var d *device
	for _, cand := range b.devices {
		if cand.desc.Addr == cmd.Addr {
			d = cand
		}
	}
	label := cmd.Kind.String()
	if d == nil {
		b.logf("command for unknown device %q ignored", cmd.Addr)
		metricCommands.WithLabelValues("unknown_target", "rejected").Inc()
		return
	}
	reject := func() {
		b.logf("command %s for %s (%s) ignored: not supported by this model", label, d.cfg.Name, d.cfg.Model)
		metricCommands.WithLabelValues("not_supported", "rejected").Inc()
	}
	j := job{dev: d}
	switch cmd.Kind {
	case ha.SetSwitch:
		switch d.desc.Kind {
		case ha.Switch:
			j.action, j.level = actionSwitch, 0
			if cmd.On {
				j.level = 100
			}
		case ha.Dimmer:
			b.mu.Lock()
			j.action, j.level, j.ramp = actionLight, 0, DefaultRampTime
			if cmd.On {
				j.level = onLevel(d)
			}
			b.mu.Unlock()
		default:
			reject()
			return
		}
		j.kind = jobLevel
	case ha.SetLight:
		if d.desc.Kind != ha.Dimmer {
			reject()
			return
		}
		j.kind, j.action, j.ramp = jobLevel, actionLight, DefaultRampTime
		if cmd.Transition != nil {
			j.ramp = *cmd.Transition
		}
		switch {
		case !cmd.On:
			j.level = 0
		case cmd.Brightness != nil:
			j.level = brightnessToLevel(*cmd.Brightness)
		default:
			b.mu.Lock()
			j.level = onLevel(d)
			b.mu.Unlock()
		}
	case ha.SetLED:
		if d.desc.Kind != ha.LEDDisplay {
			reject()
			return
		}
		j.kind, j.action, j.led, j.color = jobLED, actionLED, cmd.LED, cmd.Color
	default:
		reject()
		return
	}
	// One coalescing key and supersede target per output: a newer level
	// replaces a queued one and cancels the retries of an older one; the
	// 16 LEDs are independent of each other.
	if j.kind == jobLED {
		j.key = fmt.Sprintf("led:%s:%d", d.desc.Addr, j.led)
	} else {
		j.key = "level:" + d.desc.Addr
	}
	j.target = j.key
	b.logf("%s: queueing %s", d.cfg.Name, describeAction(j))
	b.queue.push(j)
}

// brightnessToLevel maps HA's brightness (0 … 255) to percent in the
// dimmer's 0.5 % steps; any brightness > 0 is at least 0.5 %.
func brightnessToLevel(br int) float64 {
	if br <= 0 {
		return 0
	}
	l := math.Round(float64(br)/255*200) / 2
	if l < 0.5 {
		l = 0.5
	}
	return math.Min(l, 100)
}

// onLevel is the level used when a dimmer is switched on without a
// brightness: its last level > 0, else 100 %. Caller holds b.mu.
func onLevel(d *device) float64 {
	if d.lastOnLevel > 0 {
		return d.lastOnLevel
	}
	if l := d.state.Level; l != nil && *l > 0 {
		return *l
	}
	return 100
}

// executeActuator runs a level, LED or status job on the worker.
func (b *Bridge) executeActuator(ctx context.Context, j job) (any, error) {
	switch dev := j.dev.dev.(type) {
	case *power.Actuator:
		var ev *power.LevelEvent
		var err error
		switch j.kind {
		case jobLevel:
			raw, rerr := power.LevelToRaw(j.level)
			if rerr != nil {
				return nil, rerr
			}
			ev, err = dev.SetLevel(ctx, power.ActuatorChannel, raw, j.ramp, 0)
		case jobStatus:
			ev, err = dev.RequestStatus(ctx, power.ActuatorChannel)
		default:
			return nil, fmt.Errorf("%s does not accept %s", j.dev.cfg.Name, j.action)
		}
		if err != nil || ev == nil {
			return nil, err // no typed nil in the interface
		}
		return ev, nil
	case *led16.Display:
		var ev *led16.Event
		var err error
		switch j.kind {
		case jobLED:
			c, cerr := led16.ParseColor(j.color)
			if cerr != nil {
				return nil, cerr
			}
			ev, err = dev.SetLED(ctx, byte(j.led), c)
		case jobStatus:
			// Only LED 1: it tells whether the display is reachable; the
			// other LEDs' colors come from command answers and the saved
			// state (16 requests would block the radio for long while
			// the display is unreachable).
			ev, err = dev.RequestStatus(ctx, 1)
		default:
			return nil, fmt.Errorf("%s does not accept %s", j.dev.cfg.Name, j.action)
		}
		if err != nil || ev == nil {
			return nil, err
		}
		return ev, nil
	}
	return nil, fmt.Errorf("%s (%s) does not accept actuator commands", j.dev.cfg.Name, j.dev.cfg.Model)
}

// handleStatusResult applies the answer to a status request. Failures
// are only logged with -debug: the device's availability (stale_after)
// shows an unreachable device.
func (b *Bridge) handleStatusResult(res jobResult) {
	d := res.job.dev
	if res.err != nil {
		metricStatusPolls.WithLabelValues(d.desc.Addr, labelValue(d.cfg.Name), "error").Inc()
		b.debugf("%s (%s): status request: %v", d.cfg.Name, d.desc.Addr, res.err)
		return
	}
	metricStatusPolls.WithLabelValues(d.desc.Addr, labelValue(d.cfg.Name), "ok").Inc()
	b.applyResponse(d, stateFromEvent(d, res.ev), res.ev)
}

// applyActuatorResult applies an acknowledged level or LED command.
// Caller: handleResult (event loop).
func (b *Bridge) applyActuatorResult(res jobResult) {
	j := res.job
	d := j.dev
	upd := stateFromEvent(d, res.ev)
	upd.LastCommand = "ok: " + describeAction(j)
	switch j.kind {
	case jobLevel:
		// A plain ACK confirms the command; an ACK_STATUS of a ramping
		// dimmer shows the level where the ramp starts, and the final
		// level follows as INFO_LEVEL. Show the commanded level meanwhile.
		le, _ := res.ev.(*power.LevelEvent)
		if le == nil || le.Working {
			lvl := j.level
			if d.desc.Kind == ha.Switch && lvl > 0 {
				lvl = 100
			}
			upd.Level, upd.On = ptr(lvl), ptr(lvl > 0)
		}
	case jobLED:
		if res.ev == nil {
			upd.LEDs = ledUpdate(j.led, j.color)
		}
	}
	b.applyResponse(d, upd, res.ev)
}

// applyResponse merges upd, derived from a device's direct answer to one
// of our requests, into its state: the device was heard.
func (b *Bridge) applyResponse(d *device, upd ha.State, ev any) {
	now := b.now()
	b.mu.Lock()
	if ev != nil {
		d.lastEvent = eventName(ev)
	}
	upd.LastSeen = now
	b.applyUpdate(d, upd)
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

// ledUpdate returns a State.LEDs update setting LED n (1-based).
func ledUpdate(n int, color string) []string {
	if n < 1 || n > ha.LEDCount {
		return nil
	}
	leds := make([]string, ha.LEDCount)
	leds[n-1] = color
	return leds
}

// publishPresses publishes button presses as Home Assistant events and
// counts them. Runs on the event loop.
func (b *Bridge) publishPresses(d *device, presses []remote.Press) {
	if len(presses) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range presses {
		metricButtonPresses.WithLabelValues(d.desc.Addr, labelValue(d.cfg.Name), d.cfg.Model, strconv.Itoa(p.Button), p.Type).Inc()
		d.lastEvent = fmt.Sprintf("button %d %s", p.Button, p.Type)
		note := ""
		if p.Synthetic {
			note = " (inferred)"
		}
		b.logf("%s: button %d %s%s", d.cfg.Name, p.Button, p.Type, note)
		if b.pub != nil {
			b.outbox.putEvent(ha.EventMessage(b.pub.Topics(), d.desc.Addr, p.Button, p.Type))
		}
	}
}

// expireLongPresses ends long presses whose release never arrived.
func (b *Bridge) expireLongPresses() {
	now := b.now()
	for _, d := range b.devices {
		if d.tracker != nil && d.tracker.Held() {
			b.publishPresses(d, d.tracker.Expire(now))
		}
	}
}

// summary renders the state of switches, dimmers, the LED display and
// remotes for the status page.
func summary(kind ha.Kind, st *ha.State, lastEvent string) string {
	switch kind {
	case ha.Switch:
		if st.On != nil {
			return map[bool]string{true: "on", false: "off"}[*st.On]
		}
	case ha.Dimmer:
		if st.Level != nil {
			if *st.Level == 0 {
				return "off"
			}
			return "on, " + trimFloat(*st.Level) + " %"
		}
	case ha.LEDDisplay:
		var parts []string
		for i, c := range st.LEDs {
			if c != "" {
				parts = append(parts, fmt.Sprintf("%d %s", i+1, c))
			}
		}
		return strings.Join(parts, ", ")
	case ha.Remote:
		if lastEvent != "" {
			return "last: " + lastEvent
		}
	}
	return ""
}

func trimFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
