// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// ctlConfig is testConfig without AES, with the group under Home
// Assistant control. Arguments: state_dir, setpoint_debounce, extra
// group fields.
const ctlConfig = `{
  "central_address": "1A2B3C",
  "reset": "none",
  "state_dir": %q,
  "mqtt": {"broker": "tcp://127.0.0.1:1"},
  "home_assistant_control": {"setpoint_debounce": %q},
  "devices": [
    {"name": "Wohnzimmer Heizung", "model": "HM-CC-RT-DN", "address": "38F59C"},
    {"name": "Wohnzimmer Heizung 2", "model": "HM-CC-RT-DN", "address": "38F5A1"},
    {"name": "Wohnzimmer Wand", "model": "HM-TC-IT-WM-W-EU", "address": "2A1B3C"},
    {"name": "Wohnzimmer Fenster", "model": "HM-Sec-SCo", "address": "4C5D6E"}
  ],
  "groups": [
    {"name": "Wohnzimmer", "control": "home_assistant", "fallback_after": "5m"%s,
     "members": ["Wohnzimmer Heizung", "Wohnzimmer Heizung 2", "Wohnzimmer Wand", "Wohnzimmer Fenster"]}
  ]
}`

const wzGroupState = "hmbridge/group/wohnzimmer/state"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// linkPub is a publisher like ha.Client: it knows whether the broker
// connection is up and whether Home Assistant announced "offline".
type linkPub struct {
	fakePub
	down, haOffline atomic.Bool
}

func (p *linkPub) Connected() bool { return !p.down.Load() }
func (p *linkPub) HAOnline() bool  { return !p.haOffline.Load() }

type ctlEnv struct {
	*env
	clock *fakeClock
	link  *linkPub
}

// newCtlEnv starts a bridge on the fake clock, with a fast control tick
// and (unless opt changes it) without the startup enforcement.
func newCtlEnv(t *testing.T, cfg string, clock *fakeClock, opt func(*Options)) *ctlEnv {
	t.Helper()
	e := newEnvWith(t, cfg, fakemod.Options{}, func(o *Options) {
		o.Now = clock.Now
		o.ControlTick = 10 * time.Millisecond
		o.ControlStartupDelay = 1000 * time.Hour
		if opt != nil {
			opt(o)
		}
	})
	lp := &linkPub{}
	e.b.SetPublisher(lp)
	e.pub = &lp.fakePub
	return &ctlEnv{env: e, clock: clock, link: lp}
}

// climate returns the cmd 0x11 payloads sent to dst, in hex.
func (e *env) climate(dst [3]byte) []string {
	var res []string
	for _, m := range e.sentClimate(dst) {
		res = append(res, hex.EncodeToString(m[9:]))
	}
	return res
}

// totalClimate counts the climate commands sent to the three thermostats.
func (e *env) totalClimate() int {
	return len(e.sentClimate(rtdn)) + len(e.sentClimate(rtdn2)) + len(e.sentClimate(tc))
}

// settle lets several control ticks run.
func settle() { time.Sleep(100 * time.Millisecond) }

// rtInfo is an INFO_LEVEL of a radiator thermostat: target 22 °C, mode
// auto (0x00) or manual (0x40).
func rtInfo(cnt byte, addr [3]byte, mode byte) []byte {
	return msg(cnt, 0xa6, 0x10, addr, central, 0x0a, 0xb0, 0xe2, 0x0f, 0x25, mode)
}

const (
	modeAuto   = 0x00
	modeManual = 0x40
)

func manualHex(ch byte, temp float64) string {
	return hex.EncodeToString([]byte{0x81, ch, byte(temp * 2)})
}

func TestHAControlDiscoveryAndState(t *testing.T) {
	e := newCtlEnv(t, fmt.Sprintf(ctlConfig, "", "0s", ""), newFakeClock(), nil)
	e.waitFor("discovery", func() bool { return e.pub.get("homeassistant/climate/hmbridge_group_wohnzimmer/config") != nil })
	for _, topic := range []string{"homeassistant/climate/hmbridge_group_wohnzimmer/config", "homeassistant/climate/hmbridge_38f59c/config"} {
		c := e.state(topic)
		if fmt.Sprint(c["modes"]) != "[heat off]" {
			t.Errorf("%s: modes %v, want [heat off]", topic, c["modes"])
		}
	}
	if e.pub.get("homeassistant/sensor/hmbridge_group_wohnzimmer/control/config") == nil ||
		e.pub.get("homeassistant/sensor/hmbridge_2a1b3c/control/config") == nil {
		t.Error("control sensors missing")
	}
	e.inject(0x01, rtInfo(1, rtdn2, modeManual))
	e.waitFor("states", func() bool {
		return e.state(wzGroupState)["control"] == "home_assistant" && e.state("hmbridge/38f5a1/state")["control"] == "home_assistant"
	})

	// Device control: the old climate entity, control "device".
	d := newEnv(t, testConfig, fakemod.Options{})
	d.inject(0x02, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	d.waitFor("device control", func() bool {
		return d.state(wzGroupState)["control"] == "device" && d.state("hmbridge/38f59c/state")["control"] == "device"
	})
	if c := d.state("homeassistant/climate/hmbridge_group_wohnzimmer/config"); fmt.Sprint(c["modes"]) != "[auto heat off]" {
		t.Errorf("device control modes %v", c["modes"])
	}
}

func TestHAControlKeepsManualMode(t *testing.T) {
	e := newCtlEnv(t, fmt.Sprintf(ctlConfig, "", "0s", ""), newFakeClock(), nil)
	e.inject(0x01, rtInfo(1, rtdn2, modeManual))
	settle()
	if n := e.totalClimate(); n != 0 {
		t.Fatalf("%d commands for a thermostat in manual mode", n)
	}

	// Switched to auto (button): manual at its current target, once.
	e.inject(0x01, rtInfo(2, rtdn2, modeAuto))
	e.waitFor("manual mode", func() bool { return len(e.sentClimate(rtdn2)) == 1 })
	if got := e.climate(rtdn2)[0]; got != manualHex(4, 22) {
		t.Errorf("sent %s, want manual 22", got)
	}
	e.waitFor("last command", func() bool {
		return e.state("hmbridge/38f5a1/state")["last_command"] == "ok: manual 22.0 (keep manual mode)"
	})
	// Rate limit: again auto within 10 minutes: nothing.
	e.inject(0x01, rtInfo(3, rtdn2, modeAuto))
	e.clock.Advance(9 * time.Minute)
	settle()
	if n := len(e.sentClimate(rtdn2)); n != 1 {
		t.Fatalf("%d commands within the rate limit, want 1", n)
	}
	// After 10 minutes the next tick sends it again.
	e.clock.Advance(2 * time.Minute)
	e.waitFor("second manual", func() bool { return len(e.sentClimate(rtdn2)) == 2 })

	// Not while a window is open.
	e.inject(0x01, msg(0x41, 0x84, 0x41, sco, central, 0x01, 0x1c, 0xc8))
	e.inject(0x01, rtInfo(4, rtdn2, modeAuto))
	e.clock.Advance(11 * time.Minute)
	settle()
	if n := len(e.sentClimate(rtdn2)); n != 2 {
		t.Fatalf("%d commands while the window is open, want 2", n)
	}
	e.inject(0x01, msg(0x42, 0x84, 0x41, sco, central, 0x01, 0x1d, 0x00))
	e.waitFor("manual after the window closed", func() bool { return len(e.sentClimate(rtdn2)) == 3 })

	// Not while a boost runs.
	e.inject(0x01, msg(5, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x0f, 0x25, 0xc5)) // boost, 5 min
	e.inject(0x01, rtInfo(6, rtdn2, modeAuto))
	e.clock.Advance(11 * time.Minute)
	settle()
	if n := len(e.sentClimate(rtdn2)); n != 3 {
		t.Fatalf("%d commands during boost, want 3", n)
	}
	e.inject(0x01, rtInfo(7, rtdn, modeManual)) // boost over
	e.waitFor("manual after the boost", func() bool { return len(e.sentClimate(rtdn2)) == 4 })

	// Home Assistant's setpoint wins over the device's target.
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetTemperature, Temperature: 19}
	e.waitFor("setpoint", func() bool { return len(e.sentClimate(rtdn2)) == 5 && len(e.sentClimate(tc)) == 1 })
	if got := e.climate(tc)[0]; got != manualHex(2, 19) {
		t.Errorf("setpoint sent to the wall thermostat as %s, want manual 19", got)
	}
	e.clock.Advance(11 * time.Minute)
	e.inject(0x01, rtInfo(8, rtdn2, modeAuto))
	e.waitFor("manual at Home Assistant's setpoint", func() bool { return len(e.sentClimate(rtdn2)) == 6 })
	if got := e.climate(rtdn2)[5]; got != manualHex(4, 19) {
		t.Errorf("sent %s, want manual 19", got)
	}

	// auto is not offered; a command for it is rejected.
	before := e.totalClimate()
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetMode, Mode: ha.ModeAuto}
	e.waitFor("rejected", func() bool {
		return strings.HasPrefix(fmt.Sprint(e.state("hmbridge/38f59c/state")["last_command"]), "rejected: auto")
	})
	if e.totalClimate() != before {
		t.Error("auto was sent")
	}
	// off is manual 4.5 °C.
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetMode, Mode: ha.ModeOff}
	e.waitFor("off", func() bool { return e.totalClimate() == before+3 })
	if got := e.climate(rtdn)[len(e.climate(rtdn))-1]; got != manualHex(4, 4.5) {
		t.Errorf("off sent as %s", got)
	}
}

func TestHAControlViaWallThermostat(t *testing.T) {
	e := newCtlEnv(t, fmt.Sprintf(ctlConfig, "", "0s", `, "via_wall_thermostat": true`), newFakeClock(), nil)
	e.inject(0x01, rtInfo(1, rtdn2, modeAuto))
	e.waitFor("manual to the wall thermostat", func() bool { return len(e.sentClimate(tc)) == 1 })
	settle()
	if n := len(e.sentClimate(rtdn)) + len(e.sentClimate(rtdn2)); n != 0 {
		t.Errorf("%d commands to radiator thermostats, want 0 (via the wall thermostat)", n)
	}
	if got := e.climate(tc)[0]; got != manualHex(2, 22) {
		t.Errorf("sent %s", got)
	}
}

func TestHAControlStartup(t *testing.T) {
	clock := newFakeClock()
	e := newCtlEnv(t, fmt.Sprintf(ctlConfig, "", "0s", ""), clock, func(o *Options) { o.ControlStartupDelay = time.Minute })
	e.inject(0x01, rtInfo(1, rtdn2, modeManual)) // known manual: left alone
	settle()
	clock.Advance(time.Minute)
	e.waitFor("startup", func() bool { return len(e.sentClimate(rtdn)) == 1 && len(e.sentClimate(tc)) == 1 })
	settle()
	if n := len(e.sentClimate(rtdn2)); n != 0 {
		t.Errorf("%d commands to the thermostat known to be in manual mode", n)
	}
	// Only once.
	clock.Advance(time.Hour)
	settle()
	if n := e.totalClimate(); n != 2 {
		t.Errorf("%d commands, want 2", n)
	}
}

func TestHAControlDebounce(t *testing.T) {
	e := newCtlEnv(t, fmt.Sprintf(ctlConfig, "", "300ms", ""), newFakeClock(), nil)
	for _, temp := range []float64{18, 19, 20, 21, 22.5} {
		e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetTemperature, Temperature: temp}
		time.Sleep(20 * time.Millisecond)
	}
	settle()
	if n := e.totalClimate(); n != 0 {
		t.Fatalf("%d commands sent during the debounce time", n)
	}
	e.waitFor("one command per thermostat", func() bool { return e.totalClimate() == 3 })
	time.Sleep(400 * time.Millisecond)
	if n := e.totalClimate(); n != 3 {
		t.Fatalf("%d commands, want 3 (one per thermostat)", n)
	}
	for _, d := range []struct {
		addr [3]byte
		ch   byte
	}{{rtdn, 4}, {rtdn2, 4}, {tc, 2}} {
		if got := e.climate(d.addr); len(got) != 1 || got[0] != manualHex(d.ch, 22.5) {
			t.Errorf("%x: %v, want manual 22.5", d.addr, got)
		}
	}
	e.waitFor("last command", func() bool {
		return e.state("hmbridge/38f59c/state")["last_command"] == "ok: manual 22.5 (latest of 5 setpoints)"
	})

	// A mode command replaces a waiting setpoint (which still becomes
	// Home Assistant's setpoint for heat).
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetTemperature, Temperature: 17}
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetMode, Mode: ha.ModeOff}
	e.waitFor("off", func() bool { return e.totalClimate() == 6 })
	time.Sleep(500 * time.Millisecond)
	if n := e.totalClimate(); n != 6 {
		t.Fatalf("%d commands: the setpoint was sent after off", n)
	}
	if got := e.climate(rtdn)[1]; got != manualHex(4, 4.5) {
		t.Errorf("sent %s, want off", got)
	}
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetMode, Mode: ha.ModeHeat}
	e.waitFor("heat", func() bool { return e.totalClimate() == 9 })
	if got := e.climate(rtdn)[2]; got != manualHex(4, 17) {
		t.Errorf("heat sent %s, want manual 17", got)
	}
}

// goFallback runs a group under Home Assistant control into the fail-safe.
func goFallback(t *testing.T, e *ctlEnv) {
	t.Helper()
	e.link.down.Store(true)
	settle() // the link is seen down
	e.clock.Advance(4 * time.Minute)
	settle()
	if n := e.totalClimate(); n != 3 {
		t.Fatalf("%d commands before fallback_after", n)
	}
	e.clock.Advance(2 * time.Minute)
	e.waitFor("fallback", func() bool { return e.totalClimate() == 6 })
	for _, d := range []struct {
		addr [3]byte
		ch   byte
	}{{rtdn, 4}, {rtdn2, 4}, {tc, 2}} {
		if got := e.climate(d.addr); got[len(got)-1] != hex.EncodeToString([]byte{0x80, d.ch}) {
			t.Errorf("%x: %s, want auto", d.addr, got[len(got)-1])
		}
	}
	e.waitFor("control fallback", func() bool { return e.state(wzGroupState)["control"] == "fallback" })
}

func TestHAControlFallbackAndHandBack(t *testing.T) {
	dir := t.TempDir()
	e := newCtlEnv(t, fmt.Sprintf(ctlConfig, dir, "0s", ""), newFakeClock(), nil)
	e.inject(0x01, rtInfo(1, rtdn2, modeManual)) // a group state to publish
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetTemperature, Temperature: 21}
	e.waitFor("setpoint", func() bool { return e.totalClimate() == 3 })

	goFallback(t, e)
	if v := testutil.ToFloat64(metricFallbackActive.WithLabelValues("group", "Wohnzimmer")); v != 1 {
		t.Errorf("fallback metric = %v", v)
	}
	if e.state("hmbridge/38f59c/state")["last_command"] != "ok: auto (fail-safe: Home Assistant unreachable)" {
		t.Errorf("last_command %v", e.state("hmbridge/38f59c/state")["last_command"])
	}
	cf := readControl(t, dir)
	if p := cf.Targets["group:wohnzimmer"]; !p.Fallback || p.HASetpoint != 21 || p.HAMode != "heat" {
		t.Errorf("control.json: %+v", cf)
	}
	// Once only; a device on its week program is left alone.
	e.inject(0x01, rtInfo(2, rtdn2, modeAuto))
	e.clock.Advance(time.Hour)
	settle()
	if n := e.totalClimate(); n != 6 {
		t.Fatalf("%d commands during the fallback, want 6", n)
	}

	// Back: hand back after a minute with Home Assistant's setpoint.
	e.link.down.Store(false)
	settle()
	e.clock.Advance(30 * time.Second)
	settle()
	if n := e.totalClimate(); n != 6 {
		t.Fatalf("handed back before the grace time")
	}
	e.clock.Advance(31 * time.Second)
	e.waitFor("hand back", func() bool { return e.totalClimate() == 9 })
	if got := e.climate(tc); got[len(got)-1] != manualHex(2, 21) {
		t.Errorf("hand back sent %s, want manual 21", got[len(got)-1])
	}
	e.waitFor("control home_assistant", func() bool { return e.state(wzGroupState)["control"] == "home_assistant" })
	if v := testutil.ToFloat64(metricFallbackActive.WithLabelValues("group", "Wohnzimmer")); v != 0 {
		t.Errorf("fallback metric = %v", v)
	}
	if p := readControl(t, dir).Targets["group:wohnzimmer"]; p.Fallback {
		t.Errorf("control.json still in fallback")
	}

	// Home Assistant announcing "offline" counts as unreachable too.
	e.link.haOffline.Store(true)
	settle()
	e.clock.Advance(6 * time.Minute)
	e.waitFor("fallback on HA offline", func() bool { return e.totalClimate() == 12 })
}

func readControl(t *testing.T, dir string) controlFile {
	t.Helper()
	var cf controlFile
	b, err := os.ReadFile(filepath.Join(dir, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cf); err != nil {
		t.Fatal(err)
	}
	return cf
}

func TestHAControlCommandEndsFallback(t *testing.T) {
	e := newCtlEnv(t, fmt.Sprintf(ctlConfig, "", "0s", ""), newFakeClock(), nil)
	e.inject(0x01, rtInfo(1, rtdn2, modeManual))
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetTemperature, Temperature: 21}
	e.waitFor("setpoint", func() bool { return e.totalClimate() == 3 })
	goFallback(t, e)
	// Home Assistant is back and sends a setpoint before the grace time.
	e.link.down.Store(false)
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetTemperature, Temperature: 20}
	e.waitFor("setpoint sent", func() bool { return e.totalClimate() == 9 })
	e.waitFor("control home_assistant", func() bool { return e.state(wzGroupState)["control"] == "home_assistant" })
	e.clock.Advance(2 * time.Minute)
	settle()
	if n := e.totalClimate(); n != 9 {
		t.Errorf("%d commands: the hand back sent again", n)
	}
}

// A restart during the fallback keeps the week program running until
// Home Assistant is back, then applies the setpoint saved before.
func TestHAControlRestartDuringFallback(t *testing.T) {
	dir := t.TempDir()
	cfg := fmt.Sprintf(ctlConfig, dir, "0s", "")
	e := newCtlEnv(t, cfg, newFakeClock(), nil)
	e.inject(0x01, rtInfo(1, rtdn2, modeManual))
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetTemperature, Temperature: 21.5}
	e.waitFor("setpoint", func() bool { return e.totalClimate() == 3 })
	goFallback(t, e)
	e.stop()

	// Restart while the broker is still down, with the startup check.
	clock := newFakeClock()
	r := newCtlEnv(t, cfg, clock, func(o *Options) { o.ControlStartupDelay = time.Minute })
	r.link.down.Store(true)
	r.inject(0x01, rtInfo(1, rtdn2, modeAuto))
	clock.Advance(2 * time.Hour)
	settle()
	if n := r.totalClimate(); n != 0 {
		t.Fatalf("%d commands after the restart during the fallback, want 0", n)
	}
	r.waitFor("control fallback", func() bool { return r.state(wzGroupState)["control"] == "fallback" })
	r.link.down.Store(false)
	settle()
	clock.Advance(2 * time.Minute)
	r.waitFor("hand back", func() bool { return r.totalClimate() == 3 })
	if got := r.climate(rtdn); got[0] != manualHex(4, 21.5) {
		t.Errorf("hand back sent %s, want manual 21.5 (saved)", got[0])
	}
}

// Without a known setpoint the week program keeps running after the
// outage until Home Assistant sends one.
func TestHAControlHandBackWaitsForSetpoint(t *testing.T) {
	e := newCtlEnv(t, fmt.Sprintf(ctlConfig, "", "0s", ""), newFakeClock(), nil)
	e.inject(0x01, rtInfo(1, rtdn2, modeManual))
	e.link.down.Store(true)
	settle()
	e.clock.Advance(6 * time.Minute)
	e.waitFor("fallback", func() bool { return e.totalClimate() == 3 })
	e.link.down.Store(false)
	settle()
	e.clock.Advance(5 * time.Minute)
	settle()
	if n := e.totalClimate(); n != 3 {
		t.Fatalf("%d commands without a setpoint", n)
	}
	if e.state(wzGroupState)["control"] != "fallback" {
		t.Errorf("control %v", e.state(wzGroupState)["control"])
	}
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetMode, Mode: ha.ModeHeat}
	e.waitFor("heat", func() bool { return e.totalClimate() == 6 })
	e.waitFor("control home_assistant", func() bool { return e.state(wzGroupState)["control"] == "home_assistant" })
}

// A thermostat outside groups; fallback_after 0 disables the fail-safe.
func TestHAControlStandaloneThermostat(t *testing.T) {
	cfg := `{
  "central_address": "1A2B3C", "reset": "none", "state_dir": "",
  "mqtt": {"broker": "tcp://127.0.0.1:1"},
  "home_assistant_control": {"setpoint_debounce": "0s"},
  "devices": [
    {"name": "Bad", "model": "HM-CC-RT-DN", "address": "38F59C", "control": "home_assistant"},
    {"name": "Flur", "model": "HM-CC-RT-DN", "address": "38F5A1", "control": "home_assistant", "fallback_after": "0s"}
  ]
}`
	e := newCtlEnv(t, cfg, newFakeClock(), nil)
	e.inject(0x01, rtInfo(1, rtdn, modeAuto))
	e.waitFor("manual", func() bool { return len(e.sentClimate(rtdn)) == 1 })
	e.b.Commands() <- ha.Command{Addr: "38f5a1", Kind: ha.SetTemperature, Temperature: 20}
	e.waitFor("setpoint", func() bool { return len(e.sentClimate(rtdn2)) == 1 })
	if got := e.climate(rtdn2)[0]; got != manualHex(4, 20) {
		t.Errorf("setpoint sent as %s", got)
	}
	e.link.down.Store(true)
	settle()
	e.clock.Advance(31 * time.Minute)
	e.waitFor("fallback of Bad", func() bool { return len(e.sentClimate(rtdn)) == 2 })
	settle()
	if n := len(e.sentClimate(rtdn2)); n != 1 {
		t.Errorf("Flur (fallback_after 0s) got %d commands, want 1", n)
	}
	e.waitFor("states", func() bool {
		return e.state("hmbridge/38f59c/state")["control"] == "fallback" && e.state("hmbridge/38f5a1/state")["control"] == "home_assistant"
	})
}
