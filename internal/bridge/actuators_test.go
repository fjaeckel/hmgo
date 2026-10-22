// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

var (
	plug    = [3]byte{0x6a, 0x9d, 0x15}
	dim     = [3]byte{0x41, 0xe9, 0x51}
	pb      = [3]byte{0x73, 0x78, 0x8e}
	rc8     = [3]byte{0x6c, 0x10, 0xce}
	led     = [3]byte{0x75, 0x64, 0xea}
	wds10   = [3]byte{0x6f, 0xca, 0x4a}
	wds30   = [3]byte{0x78, 0x5b, 0x74}
	devConf = `{
  "central_address": "1A2B3C",
  "reset": "none",
  "state_file": "",
  "security": {"passphrase": "secret", "index": 1},
  "mqtt": {"broker": ""},
  "devices": [
    {"name": "Stehlampe", "model": "HM-LC-Sw1-Pl-DN-R1", "address": "6A9D15", "aes": true},
    {"name": "Leselampe", "model": "HM-LC-Dim1L-Pl-3", "address": "41E951"},
    {"name": "Taster", "model": "HM-PB-2-WM55", "address": "73788E", "aes": true},
    {"name": "Fernbedienung", "model": "HM-RC-8", "address": "6C10CE"},
    {"name": "Status", "model": "HM-OU-LED16", "address": "7564EA"},
    {"name": "Garten", "model": "HM-WDS10-TH-O", "address": "6FCA4A"},
    {"name": "Teich", "model": "HM-WDS30-T-O", "address": "785B74"}
  ]
}`
)

// actuatorDevices emulates the switch and the dimmer (their level is
// kept per address) and an unreachable LED display.
type actuatorDevices struct {
	mu    sync.Mutex
	level map[[3]byte]byte
	// ramping: the dimmer answers LEVEL_SET with an ACK_STATUS showing
	// the start of the ramp.
	ramping bool
}

func (a *actuatorDevices) onSend(m []byte) (byte, []byte) {
	if len(m) < 9 || m[1]&0x20 == 0 {
		return fakemod.AckOK, nil
	}
	var dst [3]byte
	copy(dst[:], m[6:9])
	reply := func(cmd byte, payload ...byte) (byte, []byte) {
		r := []byte{m[0], 0x80, cmd}
		r = append(r, m[6:9]...)
		r = append(r, m[3:6]...)
		return fakemod.AckWithResponse, append(r, payload...)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.level == nil {
		a.level = map[[3]byte]byte{}
	}
	switch {
	case dst == led:
		return fakemod.AckEUnknown, nil // never answers
	case m[2] == 0x11 && len(m) >= 12 && m[9] == 0x02:
		old := a.level[dst]
		a.level[dst] = m[11]
		if a.ramping && dst == dim {
			return reply(0x02, 0x01, m[10], old, 0x10, 0x40)
		}
		return reply(0x02, 0x01, m[10], m[11], 0x00, 0x40)
	case m[2] == 0x01 && len(m) >= 11 && m[10] == 0x0e:
		return reply(0x10, 0x06, m[9], a.level[dst], 0x00)
	}
	return fakemod.DefaultOnSend(m)
}

func (e *env) sentTo(dst [3]byte, cmd byte) [][]byte {
	var res [][]byte
	for _, m := range e.mod.Sent() {
		if len(m) >= 9 && m[2] == cmd && bytes.Equal(m[6:9], dst[:]) {
			res = append(res, m)
		}
	}
	return res
}

func newDevEnv(t *testing.T, a *actuatorDevices, cfg string, opt func(*Options)) *env {
	return newEnvWith(t, cfg, fakemod.Options{OnSend: a.onSend}, func(o *Options) {
		o.StatusPollStagger = 10 * time.Millisecond
		o.ReleaseCheckInterval = 20 * time.Millisecond
		if opt != nil {
			opt(o)
		}
	})
}

func TestSwitchCommandsAndStatus(t *testing.T) {
	a := &actuatorDevices{}
	e := newDevEnv(t, a, devConf, nil)
	topic := "hmbridge/6a9d15/state"

	// Peers: AES on the switch's channel; 17 channels for the LED16.
	if p := e.mod.Peers()[plug]; !reflect.DeepEqual(p.AESChannels(2), []byte{1}) {
		t.Errorf("switch AES channels %v", p.AESChannels(2))
	}
	// Asked for its status after startup: known in Home Assistant at once.
	e.waitFor("status after startup", func() bool { return e.state(topic)["on"] == false })
	if s := e.sentTo(plug, 0x01); len(s) != 1 || !bytes.Equal(s[0][9:], []byte{0x01, 0x0e}) || s[0][1] != 0xa0 {
		t.Errorf("status requests %x", s)
	}

	e.b.Commands() <- ha.Command{Addr: "6a9d15", Kind: ha.SetSwitch, On: true}
	e.waitFor("switched on", func() bool {
		st := e.state(topic)
		return st["on"] == true && st["level"] == 100.0 && st["last_command"] == "ok: switch on"
	})
	s := e.sentTo(plug, 0x11)
	if len(s) != 1 || s[0][1] != 0xa0 || !bytes.Equal(s[0][9:], []byte{0x02, 0x01, 0xc8, 0x00, 0x00}) {
		t.Errorf("LEVEL_SET %x (want flags a0, no burst)", s)
	}

	// Switched off at the plug: INFO_LEVEL.
	e.inject(0x02, msg(0x30, 0xa4, 0x10, plug, central, 0x06, 0x01, 0x00, 0x00))
	e.waitFor("off by INFO_LEVEL", func() bool { return e.state(topic)["on"] == false && e.state(topic)["level"] == 0.0 })
	if v := testutil.ToFloat64(metricLevel.WithLabelValues("6a9d15", "Stehlampe", "HM-LC-Sw1-Pl-DN-R1")); v != 0 {
		t.Errorf("level metric %v", v)
	}
	// Status page.
	snap := e.b.Snapshot()
	for _, d := range snap.Devices {
		if d.Name == "Stehlampe" && d.Summary != "off" {
			t.Errorf("summary %q", d.Summary)
		}
	}
	// Thermostat commands are rejected.
	e.b.Commands() <- ha.Command{Addr: "6a9d15", Kind: ha.SetTemperature, Temperature: 20}
	e.b.Commands() <- ha.Command{Addr: "6a9d15", Kind: ha.SetLED, LED: 1, Color: "red"}
	time.Sleep(100 * time.Millisecond)
	if n := len(e.sentTo(plug, 0x11)); n != 1 {
		t.Errorf("%d LEVEL_SET frames after rejected commands", n)
	}
}

func TestDimmerBrightness(t *testing.T) {
	a := &actuatorDevices{ramping: true}
	e := newDevEnv(t, a, devConf, nil)
	topic := "hmbridge/41e951/state"
	e.waitFor("status after startup", func() bool { return e.state(topic)["level"] == 0.0 })

	two := 2.0
	e.b.Commands() <- ha.Command{Addr: "41e951", Kind: ha.SetLight, On: true, Brightness: ptr(128), Transition: &two}
	// 128/255 = 50.2 % → 50 % (raw 100), ramp 2 s = 20 units (0x0280).
	e.waitFor("LEVEL_SET", func() bool { return len(e.sentTo(dim, 0x11)) == 1 })
	if s := e.sentTo(dim, 0x11)[0]; !bytes.Equal(s[9:], []byte{0x02, 0x01, 0x64, 0x02, 0x80}) {
		t.Errorf("LEVEL_SET %x", s[9:])
	}
	// The ACK_STATUS shows the ramp starting at 0 %; the commanded level
	// is shown until the final INFO_LEVEL.
	e.waitFor("commanded level", func() bool {
		st := e.state(topic)
		return st["level"] == 50.0 && st["on"] == true && st["last_command"] == "ok: light 50 %"
	})
	e.inject(0x01, msg(0x31, 0xa4, 0x10, dim, central, 0x06, 0x01, 0x64, 0x00, 0x00, 0x64))
	e.waitFor("final level", func() bool { return e.state(topic)["fault"] == "none" })

	// Off, then on without brightness: back to 50 %, default ramp 0.5 s.
	e.b.Commands() <- ha.Command{Addr: "41e951", Kind: ha.SetLight}
	e.waitFor("off", func() bool { return len(e.sentTo(dim, 0x11)) == 2 })
	e.b.Commands() <- ha.Command{Addr: "41e951", Kind: ha.SetLight, On: true}
	e.waitFor("on again", func() bool { return len(e.sentTo(dim, 0x11)) == 3 })
	if s := e.sentTo(dim, 0x11); !bytes.Equal(s[1][9:], []byte{0x02, 0x01, 0x00, 0x00, 0xa0}) || !bytes.Equal(s[2][9:], []byte{0x02, 0x01, 0x64, 0x00, 0xa0}) {
		t.Errorf("off/on frames %x %x", s[1][9:], s[2][9:])
	}
	// Virtual channels are not the light's state.
	e.waitFor("on", func() bool { return e.state(topic)["level"] == 50.0 })
	e.inject(0x01, msg(0x32, 0xa4, 0x10, dim, central, 0x06, 0x02, 0x00, 0x00))
	e.inject(0x01, msg(0x33, 0xa4, 0x10, dim, central, 0x06, 0x01, 0x64, 0x06))
	e.waitFor("load failure", func() bool { return e.state(topic)["fault"] == "load_failure" })
	if e.state(topic)["level"] != 50.0 {
		t.Errorf("virtual channel changed the level: %v", e.state(topic))
	}
}

// A status answer or INFO_LEVEL of a dimmer that is still ramping
// (working) is no final level: it neither changes the level nor the
// level the dimmer is switched on at.
func TestDimmerWorkingReportKeepsLevel(t *testing.T) {
	a := &actuatorDevices{}
	e := newDevEnv(t, a, devConf, nil)
	topic := "hmbridge/41e951/state"
	e.waitFor("status after startup", func() bool { return e.state(topic)["level"] == 0.0 })
	e.inject(0x01, msg(0x31, 0xa4, 0x10, dim, central, 0x06, 0x01, 0x50, 0x00, 0x00))
	e.waitFor("final 40 %", func() bool { return e.state(topic)["level"] == 40.0 })
	// Ramping down: 8 %, working (flags 0x20 = down).
	e.inject(0x01, msg(0x32, 0xa4, 0x10, dim, central, 0x06, 0x01, 0x10, 0x20, 0x00))
	// Ramping up from off: a working report must not become lastOnLevel.
	e.inject(0x01, msg(0x33, 0xa4, 0x10, dim, central, 0x06, 0x01, 0x04, 0x10, 0x00))
	e.waitFor("working reports seen", func() bool {
		e.b.mu.Lock()
		defer e.b.mu.Unlock()
		return strings.Contains(e.b.byAddr[dim].lastEvent, "2 %")
	})
	e.b.mu.Lock()
	l, lastOn := e.b.byAddr[dim].state.Level, e.b.byAddr[dim].lastOnLevel
	e.b.mu.Unlock()
	if l == nil || *l != 40 || lastOn != 40 {
		t.Errorf("after working reports: level %v, lastOnLevel %v (want 40, 40)", l, lastOn)
	}
	if st := e.state(topic); st["level"] != 40.0 || st["on"] != true {
		t.Errorf("published state %v", st)
	}
}

func TestLEDDisplayUnreachable(t *testing.T) {
	a := &actuatorDevices{}
	e := newDevEnv(t, a, devConf, nil)
	topic := "hmbridge/7564ea/state"
	e.b.Commands() <- ha.Command{Addr: "7564ea", Kind: ha.SetLED, LED: 3, Color: "green"}
	// Sent like OpenCCU does, retried twice, then reported as failed.
	e.waitFor("failure reported", func() bool {
		lc, _ := e.state(topic)["last_command"].(string)
		return strings.HasPrefix(lc, "failed: led 3 green")
	})
	s := e.sentTo(led, 0x11)
	if len(s) != 3 || s[0][1] != 0xa0 || !bytes.Equal(s[0][9:], []byte{0x80, 0x03, 0x02}) {
		t.Errorf("SET_ONE_CHANNEL frames %x", s)
	}
	// Status requests (LED 1 only) failing don't touch last_command.
	if r := e.sentTo(led, 0x01); len(r) < 1 || !bytes.Equal(r[0][9:], []byte{0x01, 0x0e}) {
		t.Errorf("status requests %x", r)
	}
	if v := e.state(topic)["leds"]; v != nil {
		t.Errorf("LED state without an answer: %v", v)
	}

	// An LED color reported by the display (e.g. set by a direct link).
	e.inject(0x01, msg(0x40, 0xa4, 0x10, led, central, 0x06, 0x05, 0x01))
	e.waitFor("LED 5 red", func() bool {
		v, _ := e.state(topic)["leds"].([]any)
		return len(v) == 16 && v[4] == "red" && v[0] == ""
	})
	if v := testutil.ToFloat64(metricLEDColor.WithLabelValues("7564ea", "Status", "HM-OU-LED16", "5")); v != 1 {
		t.Errorf("led_color metric %v", v)
	}
}

func TestLEDCommandsSupersedePerLED(t *testing.T) {
	q := newJobQueue()
	d := &device{cfg: &config.Device{Name: "Status"}, desc: ha.DeviceDesc{Addr: "7564ea", Kind: ha.LEDDisplay}}
	b := &Bridge{queue: q, logf: t.Logf, devices: []*device{d}}
	b.handleActuatorCommand(ha.Command{Addr: "7564ea", Kind: ha.SetLED, LED: 1, Color: "red"})
	b.handleActuatorCommand(ha.Command{Addr: "7564ea", Kind: ha.SetLED, LED: 2, Color: "red"})
	b.handleActuatorCommand(ha.Command{Addr: "7564ea", Kind: ha.SetLED, LED: 1, Color: "green"})
	var got []string
	for {
		j, ok := q.pop()
		if !ok {
			break
		}
		got = append(got, describeAction(j))
	}
	if want := []string{"led 2 red", "led 1 green"}; !reflect.DeepEqual(got, want) {
		t.Errorf("queue %v, want %v", got, want)
	}
}

func TestButtonPresses(t *testing.T) {
	presses := metricButtonPresses.WithLabelValues("6c10ce", "Fernbedienung", "HM-RC-8", "8", "press_long")
	before := testutil.ToFloat64(presses)
	e := newDevEnv(t, &actuatorDevices{}, devConf, nil)
	want := func(events ...string) {
		t.Helper()
		e.waitFor("events "+strings.Join(events, ", "), func() bool { return reflect.DeepEqual(e.pub.eventLog(), events) })
	}
	// HM-PB-2-WM55 with AES: short press (BiDi, authenticated by the
	// module), retransmitted.
	e.inject(0x02, msg(0x50, 0xa0, 0x40, pb, central, 0x01, 0x2e))
	e.inject(0x02, msg(0x50, 0xa0, 0x40, pb, central, 0x01, 0x2e))
	want("button_1 press_short")
	// Long press: the repeats (no BiDi) are not authenticated and
	// ignored on the AES channel; the authenticated release yields both.
	e.inject(0x01, msg(0x51, 0x84, 0x40, pb, central, 0x42, 0x2f))
	e.inject(0x01, msg(0x52, 0x84, 0x40, pb, central, 0x42, 0x2f))
	e.inject(0x02, msg(0x53, 0xa4, 0x40, pb, central, 0x42, 0x2f))
	want("button_1 press_short", "button_2 press_long", "button_2 press_long_release")
	// A forged, unauthenticated short press is ignored.
	e.inject(0x01, msg(0x54, 0xa0, 0x40, pb, central, 0x01, 0x30))
	time.Sleep(50 * time.Millisecond)
	want("button_1 press_short", "button_2 press_long", "button_2 press_long_release")

	// HM-RC-8 without AES: the first repeat is the long press; low battery.
	e.pub.mu.Lock()
	e.pub.events = nil
	e.pub.mu.Unlock()
	e.inject(0x01, msg(0x60, 0x84, 0x40, rc8, central, 0xc8, 0x10))
	e.inject(0x01, msg(0x61, 0x84, 0x40, rc8, central, 0xc8, 0x10))
	e.inject(0x01, msg(0x62, 0xa4, 0x40, rc8, central, 0xc8, 0x10))
	want("button_8 press_long", "button_8 press_long_release")
	e.waitFor("low battery", func() bool { return e.state("hmbridge/6c10ce/state")["low_battery"] == true })
	if v := testutil.ToFloat64(presses) - before; v != 1 {
		t.Errorf("button_presses_total increased by %v", v)
	}
	// Events go to the event topics, never into the retained state.
	if st := e.state("hmbridge/6c10ce/state"); st["last_seen"] == nil || len(st) > 7 {
		t.Errorf("remote state %v", st)
	}
	for _, m := range e.pub.events {
		if m.Retain {
			t.Errorf("retained event %s", m.Topic)
		}
	}
	// Remotes are never stale by default.
	if e.b.cfg.Device(rc8).StaleTimeout() != 0 {
		t.Error("remote has a stale timeout")
	}
}

func TestButtonReleaseTimeout(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := newDevEnv(t, &actuatorDevices{}, devConf, func(o *Options) { o.Now = clock })
	e.inject(0x01, msg(0x60, 0x84, 0x40, rc8, central, 0x43, 0x11))
	e.waitFor("long press", func() bool { return len(e.pub.eventLog()) == 1 })
	mu.Lock()
	now = now.Add(6 * time.Second)
	mu.Unlock()
	e.waitFor("inferred release", func() bool {
		return reflect.DeepEqual(e.pub.eventLog(), []string{"button_3 press_long", "button_3 press_long_release"})
	})
}

func TestWeatherSensors(t *testing.T) {
	e := newDevEnv(t, &actuatorDevices{}, devConf, nil)
	// Captured broadcasts (flags 0x86, to 000000).
	e.inject(0x01, msg(0xce, 0x86, 0x70, wds10, [3]byte{}, 0x00, 0x64, 0x50))
	e.inject(0x01, msg(0xd7, 0x86, 0x70, wds30, [3]byte{}, 0x80, 0x82, 0x64))
	e.waitFor("TH-O", func() bool {
		st := e.state("hmbridge/6fca4a/state")
		return st["current_temperature"] == 10.0 && st["humidity"] == 80.0 && st["low_battery"] == false
	})
	e.waitFor("T-O", func() bool {
		st := e.state("hmbridge/785b74/state")
		return st["current_temperature"] == 13.0 && st["humidity"] == nil && st["low_battery"] == true
	})
	if v := testutil.ToFloat64(metricHumidity.WithLabelValues("6fca4a", "Garten", "HM-WDS10-TH-O")); v != 80 {
		t.Errorf("humidity metric %v", v)
	}
	if got := e.b.cfg.Device(wds10).StaleTimeout(); got != time.Hour {
		t.Errorf("weather stale timeout %v", got)
	}
}

func TestActuatorStatePersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := strings.Replace(devConf, `"state_file": "",`, `"state_file": "`+path+`",`, 1)
	a := &actuatorDevices{}
	e := newDevEnv(t, a, cfg, func(o *Options) {
		o.HousekeepingInterval = 10 * time.Millisecond
		o.StateSaveInterval = 30 * time.Millisecond
	})
	e.b.Commands() <- ha.Command{Addr: "41e951", Kind: ha.SetLight, On: true, Brightness: ptr(64)}
	e.inject(0x01, msg(0x40, 0xa4, 0x10, led, central, 0x06, 0x05, 0x01))
	e.waitFor("saved", func() bool {
		ps, _ := readStateFile(t, path)
		l := ps.Devices["41e951"].State.Level
		leds := ps.Devices["7564ea"].State.LEDs
		return l != nil && *l == 25 && len(leds) == 16 && leds[4] == "red" && ps.Devices["41e951"].LastOnLevel == 25
	})
	// Button presses are no state: nothing about them is saved.
	e.inject(0x01, msg(0x60, 0xa0, 0x40, rc8, central, 0x01, 0x10))
	e.waitFor("press", func() bool { return len(e.pub.eventLog()) == 1 })
	e.stop()
	ps, _ := readStateFile(t, path)
	if strings.Contains(ps.Devices["6c10ce"].State.LastCommand, "button") {
		t.Errorf("remote state %+v", ps.Devices["6c10ce"].State)
	}

	// Restored: the dimmer turns on at its last level.
	e2 := newDevEnv(t, a, cfg, nil)
	e2.waitFor("restored LEDs", func() bool {
		v, _ := e2.state("hmbridge/7564ea/state")["leds"].([]any)
		return len(v) == 16 && v[4] == "red"
	})
	e2.b.mu.Lock()
	lvl := e2.b.byAddr[dim].lastOnLevel
	e2.b.mu.Unlock()
	if lvl != 25 {
		t.Errorf("restored lastOnLevel %v", lvl)
	}
}

func TestBrightnessToLevel(t *testing.T) {
	for br, want := range map[int]float64{0: 0, 1: 0.5, 2: 1, 128: 50, 254: 99.5, 255: 100} {
		if got := brightnessToLevel(br); got != want {
			t.Errorf("brightnessToLevel(%d) = %v, want %v", br, got, want)
		}
	}
}

// Restored actuator state (retained message or state file) is
// sanitized: no LEDs beyond 16, no unknown colors, no level outside
// 0…100.
func TestRestoredActuatorStateSanitized(t *testing.T) {
	newB := func() *Bridge {
		cfg, err := config.Parse([]byte(devConf))
		if err != nil {
			t.Fatal(err)
		}
		b, err := New(Options{Config: cfg, Radio: nopRadio{}, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	now := time.Now().UTC().Format(time.RFC3339)
	leds := `["red","blue","green","","orange","off","red","red","red","red","red","red","red","red","red","red","green","green"]`
	wantLEDs := []string{"red", "", "green", "", "orange", "off", "red", "red", "red", "red", "red", "red", "red", "red", "red", "red"}

	b := newB()
	b.SeedFromRetained(ha.TopicConfig{}, map[string][]byte{
		"hmbridge/41e951/state": []byte(`{"on":true,"level":150,"last_seen":"` + now + `"}`),
		"hmbridge/6a9d15/state": []byte(`{"on":false,"level":-1,"last_seen":"` + now + `"}`),
		"hmbridge/7564ea/state": []byte(`{"leds":` + leds + `,"last_seen":"` + now + `"}`),
	})
	if d := b.byAddr[dim]; d.state.Level != nil || d.lastOnLevel != 0 || d.state.On == nil || !*d.state.On {
		t.Errorf("retained dimmer: level %v, lastOnLevel %v, on %v", d.state.Level, d.lastOnLevel, d.state.On)
	}
	if l := b.byAddr[plug].state.Level; l != nil {
		t.Errorf("retained switch level %v", *l)
	}
	if got := b.byAddr[led].state.LEDs; !reflect.DeepEqual(got, wantLEDs) {
		t.Errorf("retained LEDs %q", got)
	}

	path := filepath.Join(t.TempDir(), "state.json")
	file := `{"version":1,"devices":{
		"41e951":{"state":{"on":true,"level":101,"last_seen":"` + now + `"},"last_on_level":250},
		"7564ea":{"state":{"leds":` + leds + `,"last_seen":"` + now + `"}}}}`
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	b = newB()
	if err := b.loadState(path); err != nil {
		t.Fatal(err)
	}
	if d := b.byAddr[dim]; d.state.Level != nil || d.lastOnLevel != 0 {
		t.Errorf("state file dimmer: level %v, lastOnLevel %v", d.state.Level, d.lastOnLevel)
	}
	if got := b.byAddr[led].state.LEDs; !reflect.DeepEqual(got, wantLEDs) {
		t.Errorf("state file LEDs %q", got)
	}
	// Valid values survive.
	b.SeedFromRetained(ha.TopicConfig{}, map[string][]byte{
		"hmbridge/41e951/state": []byte(`{"on":true,"level":42.5,"last_seen":"` + time.Now().UTC().Add(time.Second).Format(time.RFC3339) + `"}`),
	})
	if d := b.byAddr[dim]; d.state.Level == nil || *d.state.Level != 42.5 || d.lastOnLevel != 42.5 {
		t.Errorf("valid level not restored: %v %v", d.state.Level, d.lastOnLevel)
	}
}

// The first PublishAll after start removes the button and LED entities
// of other kinds (a device whose kind changed); reconnects don't repeat
// those removals.
func TestFirstPublishRemovesOtherKindsEntities(t *testing.T) {
	cfg, err := config.Parse([]byte(devConf))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{Config: cfg, Radio: nopRadio{}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	removed := func() map[string]bool {
		r := map[string]bool{}
		for _, m := range b.outbox.take() {
			if len(m.Payload) == 0 {
				r[m.Topic] = true
			}
		}
		return r
	}
	b.SetPublisher(&fakePub{})
	first := removed()
	for _, k := range []string{
		"homeassistant/select/hmbridge_6a9d15/led_16/config",                          // switch: LED selects
		"homeassistant/device_automation/hmbridge_6a9d15/button_8_press_short/config", // switch: button triggers
		"homeassistant/event/hmbridge_7564ea/button_1/config",                         // LED display: button events
		"homeassistant/select/hmbridge_6c10ce/led_1/config",                           // remote: LED selects
	} {
		if !first[k] {
			t.Errorf("first PublishAll does not remove %s", k)
		}
	}
	if first["homeassistant/select/hmbridge_7564ea/led_1/config"] || first["homeassistant/event/hmbridge_6c10ce/button_8/config"] {
		t.Error("first PublishAll removes a live entity")
	}
	b.PublishAll()
	if again := removed(); again["homeassistant/select/hmbridge_6a9d15/led_16/config"] || again["homeassistant/event/hmbridge_7564ea/button_1/config"] {
		t.Error("reconnect repeats the removals of other kinds' entity sets")
	}
}
