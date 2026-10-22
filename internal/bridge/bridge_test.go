// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/uartgw"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

var (
	central  = [3]byte{0x1a, 0x2b, 0x3c}
	rtdn     = [3]byte{0x38, 0xf5, 0x9c}
	rtdn2    = [3]byte{0x38, 0xf5, 0xa1}
	tc       = [3]byte{0x2a, 0x1b, 0x3c}
	sco      = [3]byte{0x4c, 0x5d, 0x6e}
	stranger = [3]byte{0x11, 0x22, 0x33}
)

const testConfig = `{
  "central_address": "1A2B3C",
  "reset": "none",
  "state_file": "",
  "security": {"passphrase": "secret", "index": 1},
  "mqtt": {"broker": ""},
  "devices": [
    {"name": "Wohnzimmer Heizung", "model": "HM-CC-RT-DN", "address": "38F59C", "aes": true},
    {"name": "Wohnzimmer Heizung 2", "model": "HM-CC-RT-DN", "address": "38F5A1"},
    {"name": "Wohnzimmer Wand", "model": "HM-TC-IT-WM-W-EU", "address": "2A1B3C"},
    {"name": "Wohnzimmer Fenster", "model": "HM-Sec-SCo", "address": "4C5D6E"}
  ],
  "groups": [
    {"name": "Wohnzimmer", "members": ["Wohnzimmer Heizung", "Wohnzimmer Heizung 2", "Wohnzimmer Wand", "Wohnzimmer Fenster"]}
  ]
}`

// fakePub records published messages (the last one per topic, and all
// event messages in order).
type fakePub struct {
	mu     sync.Mutex
	msgs   map[string][]byte
	events []ha.Message
}

func (p *fakePub) Publish(m ha.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.msgs == nil {
		p.msgs = make(map[string][]byte)
	}
	p.msgs[m.Topic] = append([]byte(nil), m.Payload...)
	if strings.HasPrefix(m.Topic, "hmbridge/") && strings.Contains(m.Topic, "/event/") {
		p.events = append(p.events, m)
	}
	return nil
}

// eventLog returns the published events as "button_<n> <type>".
func (p *fakePub) eventLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var res []string
	for _, m := range p.events {
		var v struct {
			EventType string `json:"event_type"`
		}
		json.Unmarshal(m.Payload, &v)
		res = append(res, m.Topic[strings.LastIndex(m.Topic, "/")+1:]+" "+v.EventType)
	}
	return res
}

func (p *fakePub) Topics() ha.TopicConfig { return ha.TopicConfig{} }

func (p *fakePub) get(topic string) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.msgs[topic]
}

// countNonEmpty counts topics with prefix whose last payload is not
// empty (empty retained payloads remove stale discovery entries).
func (p *fakePub) countNonEmpty(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for t, payload := range p.msgs {
		if strings.HasPrefix(t, prefix) && len(payload) > 0 {
			n++
		}
	}
	return n
}

type env struct {
	t   *testing.T
	mod *fakemod.Module
	b   *Bridge
	pub *fakePub
	run chan error

	stopOnce sync.Once
	cancel   context.CancelFunc
}

// stop stops the bridge and waits for Run to return.
func (e *env) stop() {
	e.stopOnce.Do(func() {
		e.cancel()
		<-e.run
	})
}

func newEnv(t *testing.T, cfgJSON string, modOpts fakemod.Options) *env {
	t.Helper()
	return newEnvWith(t, cfgJSON, modOpts, nil)
}

func newEnvWith(t *testing.T, cfgJSON string, modOpts fakemod.Options, opt func(*Options)) *env {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	mod, rw := fakemod.New(modOpts)
	t.Cleanup(func() { mod.Close() })
	cur, prev := cfg.Keys()
	conn, err := uartgw.Open(rw, uartgw.Config{
		HMID: cfg.CentralAddr(), Current: cur, Previous: prev,
		Timeout: time.Second, AnnounceWait: 100 * time.Millisecond, CreditsInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	opts := Options{
		Config:      cfg,
		Radio:       conn,
		RetryDelay:  50 * time.Millisecond,
		Logf:        t.Logf,
		ClockSynced: func() bool { return true },
	}
	if opt != nil {
		opt(&opts)
	}
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	pub := &fakePub{}
	b.SetPublisher(pub)
	e := &env{t: t, mod: mod, b: b, pub: pub, run: make(chan error, 1)}
	e.cancel = cancel
	go func() { e.run <- b.Run(ctx) }()
	t.Cleanup(e.stop)
	return e
}

// msg builds a BidCoS message as received over the air.
func msg(cnt, flags, cmd byte, src, dst [3]byte, payload ...byte) []byte {
	m := []byte{cnt, flags, cmd}
	m = append(m, src[:]...)
	m = append(m, dst[:]...)
	return append(m, payload...)
}

func (e *env) inject(status byte, m []byte) {
	e.t.Helper()
	if err := e.mod.InjectRecv(status, m); err != nil {
		e.t.Fatal(err)
	}
}

// waitFor polls cond until it returns true or the test times out.
func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

func (e *env) state(topic string) map[string]any {
	e.t.Helper()
	b := e.pub.get(topic)
	if b == nil {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		e.t.Fatalf("%s: %v", topic, err)
	}
	return v
}

// sentClimate returns the cmd 0x11 frames the module transmitted to dst.
func (e *env) sentClimate(dst [3]byte) [][]byte {
	var res [][]byte
	for _, m := range e.mod.Sent() {
		if len(m) >= 9 && m[2] == 0x11 && bytes.Equal(m[6:9], dst[:]) {
			res = append(res, m)
		}
	}
	return res
}

func TestSetupRegistersPeersWithAES(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	peers := e.mod.Peers()
	if len(peers) != 4 {
		t.Fatalf("got %d peers, want 4", len(peers))
	}
	p := peers[rtdn]
	if p.KeyIndex != 1 {
		t.Errorf("key index = %d, want 1", p.KeyIndex)
	}
	if got := p.AESChannels(7); len(got) != 6 {
		t.Errorf("RT-DN AES channels = %v, want 1..6", got)
	}
	if got := peers[rtdn2].AESChannels(7); len(got) != 0 {
		t.Errorf("RT-DN 2 AES channels = %v, want none", got)
	}
	if hmid, _ := e.mod.HMID(); hmid != central {
		t.Errorf("HMID = %x, want %x", hmid, central)
	}
	// Discovery is published for all devices and the group.
	e.waitFor("climate discovery", func() bool {
		return e.pub.countNonEmpty("homeassistant/climate/") == 4 // 2 RT-DN + TC + group
	})
}

func TestRadiatorInfoEvent(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	// INFO_LEVEL: target 22.0, actual 22.6, no fault, 2.3 V, valve 37 %, auto.
	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	topic := "hmbridge/38f59c/state"
	e.waitFor("RT-DN state", func() bool {
		st := e.state(topic)
		return st != nil && st["current_temperature"] != nil
	})
	st := e.state(topic)
	for k, want := range map[string]any{
		"current_temperature": 22.6,
		"target_temperature":  22.0,
		"valve_position":      37.0,
		"battery_voltage":     2.3,
		"battery_percent":     12.0, // (2.3-2.2)/0.8 = 12.5 %, which float rounding puts at 12
		"low_battery":         false,
		"mode":                "auto",
		"action":              "heating",
		"control_mode":        "auto",
		"fault":               "none",
	} {
		if st[k] != want {
			t.Errorf("%s = %v (%T), want %v", k, st[k], st[k], want)
		}
	}
	if st["last_seen"] == nil || st["messages_received"] != 1.0 {
		t.Errorf("last_seen/messages_received missing: %v", st)
	}
}

func TestSetTemperatureCommand(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetTemperature, Temperature: 21.5}
	e.waitFor("command sent", func() bool { return len(e.sentClimate(rtdn)) > 0 })
	m := e.sentClimate(rtdn)[0]
	if got, want := m[9:], []byte{0x86, 0x04, 43}; !bytes.Equal(got, want) {
		t.Errorf("payload = %x, want %x", got, want)
	}
	if m[1]&0x10 == 0 || m[1]&0x20 == 0 {
		t.Errorf("flags = %02x, want burst and BiDi", m[1])
	}
	if !bytes.Equal(m[3:6], central[:]) {
		t.Errorf("source = %x, want central %x", m[3:6], central)
	}
	e.waitFor("optimistic state", func() bool {
		st := e.state("hmbridge/38f59c/state")
		return st != nil && st["target_temperature"] == 21.5
	})
}

func TestGroupCommandOrder(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetMode, Mode: ha.ModeOff}
	e.waitFor("three commands", func() bool {
		return len(e.sentClimate(tc))+len(e.sentClimate(rtdn))+len(e.sentClimate(rtdn2)) == 3
	})
	var order [][3]byte
	for _, m := range e.mod.Sent() {
		if m[2] == 0x11 {
			var a [3]byte
			copy(a[:], m[6:9])
			order = append(order, a)
			// manual mode at 4.5 °C: 81 <chn> 09
			if m[9] != 0x81 || m[11] != 9 {
				t.Errorf("payload = %x, want manual 4.5", m[9:])
			}
		}
	}
	if order[0] != tc {
		t.Errorf("first command went to %x, want the wall thermostat %x", order[0], tc)
	}
	// The wall thermostat's control channel is 2, the radiator's 4.
	if m := e.sentClimate(tc)[0]; m[10] != 0x02 {
		t.Errorf("TC channel = %d, want 2", m[10])
	}
	e.waitFor("group state off", func() bool {
		st := e.state("hmbridge/group/wohnzimmer/state")
		return st != nil && st["mode"] == "off"
	})
}

func TestRetryOnNoResponse(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	e := newEnv(t, testConfig, fakemod.Options{
		OnSend: func(m []byte) (byte, []byte) {
			if len(m) > 2 && m[2] == 0x11 {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					return fakemod.AckEUnknown, nil // device did not answer
				}
			}
			return fakemod.DefaultOnSend(m)
		},
	})
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetPreset, Preset: ha.PresetBoost}
	e.waitFor("retry", func() bool { return len(e.sentClimate(rtdn)) == 2 })
	e.waitFor("boost state", func() bool {
		st := e.state("hmbridge/38f59c/state")
		return st != nil && st["preset"] == "boost"
	})
}

func TestCoalescing(t *testing.T) {
	block := make(chan struct{})
	sending := make(chan struct{})
	var once sync.Once
	e := newEnv(t, testConfig, fakemod.Options{
		OnSend: func(m []byte) (byte, []byte) {
			if len(m) > 2 && m[2] == 0x11 {
				// Hold the first command until the others are queued.
				once.Do(func() {
					close(sending)
					<-block
				})
			}
			return fakemod.DefaultOnSend(m)
		},
	})
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(block) }) }
	t.Cleanup(release) // runs before the env cleanup

	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetTemperature, Temperature: 18}
	<-sending
	for _, temp := range []float64{19, 20, 21, 22} {
		e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetTemperature, Temperature: temp}
	}
	// Wait until the event loop has handled all four commands: the
	// single queued job then carries the latest temperature.
	e.waitFor("queued", func() bool {
		e.b.queue.mu.Lock()
		defer e.b.queue.mu.Unlock()
		j, ok := e.b.queue.jobs["climate:38f59c:temperature"]
		return len(e.b.queue.order) == 1 && ok && j.temp == 22
	})
	release()
	e.waitFor("two commands", func() bool { return len(e.sentClimate(rtdn)) == 2 })
	time.Sleep(100 * time.Millisecond)
	sent := e.sentClimate(rtdn)
	if len(sent) != 2 {
		t.Fatalf("sent %d commands, want 2 (first + latest)", len(sent))
	}
	if sent[1][11] != 44 {
		t.Errorf("second command temp = %v, want 22.0", float64(sent[1][11])/2)
	}
}

func TestAESFailedIgnored(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.inject(0x03, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	// A later valid frame from another device proves the first one was processed.
	e.inject(0x01, msg(0x06, 0xa6, 0x10, rtdn2, central, 0x0a, 0xb0, 0xe2, 0x08, 0x00, 0x00))
	e.waitFor("second device", func() bool {
		st := e.state("hmbridge/38f5a1/state")
		return st != nil && st["current_temperature"] != nil
	})
	if st := e.state("hmbridge/38f59c/state"); st != nil && st["current_temperature"] != nil {
		t.Errorf("state of device with failed AES was updated: %v", st)
	}
}

func TestAESOKStatus(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.inject(0x02, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	e.waitFor("auth ok", func() bool {
		st := e.state("hmbridge/38f59c/state")
		return st != nil && st["auth_status"] == "ok"
	})
}

func TestWindowContactAndGroup(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	// EVENT: channel 1, counter 0x1c, open (0xc8).
	e.inject(0x11, msg(0x07, 0x84, 0x41, sco, central, 0x01, 0x1c, 0xc8))
	e.waitFor("contact state", func() bool {
		st := e.state("hmbridge/4c5d6e/state")
		return st != nil && st["window_open"] == true && st["window_state"] == "open"
	})
	e.waitFor("group window", func() bool {
		st := e.state("hmbridge/group/wohnzimmer/state")
		return st != nil && st["window_open"] == true
	})
	// INFO_LEVEL with low battery and sabotage.
	e.inject(0x01, msg(0x08, 0xa4, 0x10, sco, central, 0x06, 0x01, 0x00, 0x8e))
	e.waitFor("lowbat+sabotage", func() bool {
		st := e.state("hmbridge/4c5d6e/state")
		return st != nil && st["low_battery"] == true && st["sabotage"] == true && st["window_open"] == false
	})
}

func TestWallThermostatEvents(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	// THERMALCONTROL_EVENT: target 25.0 (200>>2/2), actual 21.5, humidity 65.
	e.inject(0x01, msg(0x09, 0x86, 0x5a, tc, central, 200, 215, 65))
	e.waitFor("tc state", func() bool {
		st := e.state("hmbridge/2a1b3c/state")
		return st != nil && st["humidity"] == 65.0 && st["current_temperature"] == 21.5
	})
	e.waitFor("group takes temperature from wall thermostat", func() bool {
		st := e.state("hmbridge/group/wohnzimmer/state")
		return st != nil && st["current_temperature"] == 21.5 && st["humidity"] == 65.0
	})
}

func TestTimeRequestAnswered(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.inject(0x01, msg(0x42, 0xa6, 0x3f, tc, central))
	e.waitFor("time response", func() bool {
		for _, m := range e.mod.Sent() {
			if m[2] == 0x3f && bytes.Equal(m[6:9], tc[:]) {
				if m[0] != 0x42 {
					t.Errorf("response counter = %02x, want 42", m[0])
				}
				return true
			}
		}
		return false
	})
}

func TestUnknownSender(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	// DEVICE_INFO from an unconfigured device (config button pressed).
	payload := append([]byte{0x14, 0x00, 0x95}, []byte("MEQ0123456")...)
	payload = append(payload, 0x58, 0x00, 0x00)
	e.inject(0x01, msg(0x01, 0x84, 0x00, stranger, central, payload...))
	e.waitFor("unknown sender", func() bool {
		s := e.b.Snapshot()
		return len(s.Unknown) == 1 && s.Unknown[0].Serial == "MEQ0123456"
	})

	// The status page renders it.
	srv := httptest.NewServer(e.b.StatusHandler(Info{"firmware": "1.4.1"}))
	defer srv.Close()
	for _, path := range []string{"/", "/api/state"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), "MEQ0123456") {
			t.Errorf("%s: status %d, body lacks serial:\n%s", path, resp.StatusCode, body)
		}
	}
}

func TestModuleResetEndsRun(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	if err := e.mod.Crash(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-e.run:
		if err == nil || !strings.Contains(err.Error(), "radio") {
			t.Errorf("Run returned %v, want radio error", err)
		}
		e.run <- err // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after module reset")
	}
}

func init() {
	// Keep test output readable: uartgw logs via the log package.
	log.SetFlags(0)
	_ = fmt.Sprint
}

// Review findings (regression tests).

func TestMalformedFramesDoNotCrash(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	// Too short INFO_LEVEL, THERMALCONTROL with the wrong length, too
	// short ACK_STATUS: decoders fail and must not take down the bridge.
	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0x24, 0xc8))
	e.inject(0x01, msg(0x06, 0x86, 0x5a, tc, central, 200, 215))
	e.inject(0x01, msg(0x07, 0x80, 0x02, rtdn, central, 0x01, 0x04, 0x2a))
	// A valid frame afterwards is still processed.
	e.inject(0x01, msg(0x08, 0xa6, 0x10, rtdn2, central, 0x0a, 0xb0, 0xe2, 0x08, 0x00, 0x00))
	e.waitFor("valid frame after malformed ones", func() bool {
		st := e.state("hmbridge/38f5a1/state")
		return st != nil && st["current_temperature"] == 22.6
	})
	select {
	case err := <-e.run:
		t.Fatalf("Run stopped: %v", err)
	default:
	}
}

func TestTimeRequestCounterZero(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.inject(0x01, msg(0x00, 0xa6, 0x3f, tc, central))
	e.waitFor("time response", func() bool {
		for _, m := range e.mod.Sent() {
			if m[2] == 0x3f && bytes.Equal(m[6:9], tc[:]) {
				if m[0] != 0x00 {
					t.Errorf("response counter = %02x, want 00 (echo of the request)", m[0])
				}
				return true
			}
		}
		return false
	})
}

func TestTimeRequestsCanBeDisabled(t *testing.T) {
	cfg := strings.Replace(testConfig, `"reset": "none",`, `"reset": "none", "answer_time_requests": false,`, 1)
	e := newEnv(t, cfg, fakemod.Options{})
	e.inject(0x01, msg(0x42, 0xa6, 0x3f, tc, central))
	// Process a later frame, then check that no answer was sent.
	e.inject(0x01, msg(0x43, 0xa6, 0x10, rtdn2, central, 0x0a, 0xb0, 0xe2, 0x08, 0x00, 0x00))
	e.waitFor("later frame", func() bool { return e.state("hmbridge/38f5a1/state")["current_temperature"] != nil })
	time.Sleep(50 * time.Millisecond)
	for _, m := range e.mod.Sent() {
		if m[2] == 0x3f {
			t.Fatalf("time request answered although disabled: %x", m)
		}
	}
}

func TestPerDeviceKeyIndex(t *testing.T) {
	cfg := strings.Replace(testConfig,
		`"security": {"passphrase": "secret", "index": 1},`,
		`"security": {"passphrase": "new", "index": 2, "previous": {"passphrase": "secret", "index": 1}},`, 1)
	cfg = strings.Replace(cfg,
		`{"name": "Wohnzimmer Heizung 2", "model": "HM-CC-RT-DN", "address": "38F5A1"}`,
		`{"name": "Wohnzimmer Heizung 2", "model": "HM-CC-RT-DN", "address": "38F5A1", "aes": true, "key_index": 1}`, 1)
	e := newEnv(t, cfg, fakemod.Options{})
	peers := e.mod.Peers()
	if got := peers[rtdn].KeyIndex; got != 2 {
		t.Errorf("default device key index = %d, want 2 (current)", got)
	}
	if got := peers[rtdn2].KeyIndex; got != 1 {
		t.Errorf("overridden device key index = %d, want 1 (previous)", got)
	}
}

func TestRetryDoesNotOverrideNewerCommand(t *testing.T) {
	q := newJobQueue()
	old := job{key: "climate:x:temperature", target: "climate:x", temp: 18}
	old.seq = q.push(old)
	if _, ok := q.pop(); !ok {
		t.Fatal("pop failed")
	}
	newer := job{key: "climate:x:temperature", target: "climate:x", temp: 22}
	q.push(newer)
	if q.retry(old) {
		t.Error("retry of a superseded job was accepted")
	}
	j, _ := q.pop()
	if j.temp != 22 {
		t.Errorf("queued temp = %v, want 22", j.temp)
	}
}

func TestGroupViaWallThermostat(t *testing.T) {
	cfg := strings.Replace(testConfig, `{"name": "Wohnzimmer", "members":`, `{"name": "Wohnzimmer", "via_wall_thermostat": true, "members":`, 1)
	e := newEnv(t, cfg, fakemod.Options{})
	e.b.Commands() <- ha.Command{Group: "wohnzimmer", Kind: ha.SetTemperature, Temperature: 20}
	e.waitFor("command to wall thermostat", func() bool { return len(e.sentClimate(tc)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := len(e.sentClimate(rtdn)) + len(e.sentClimate(rtdn2)); n != 0 {
		t.Errorf("%d commands sent to radiator thermostats, want 0", n)
	}
}

func TestLastCommandReported(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{
		OnSend: func(m []byte) (byte, []byte) {
			if len(m) > 9 && m[2] == 0x11 && m[9] == 0x83 { // boost: device never answers
				return fakemod.AckEUnknown, nil
			}
			return fakemod.DefaultOnSend(m)
		},
	})
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetTemperature, Temperature: 19}
	e.waitFor("ok", func() bool {
		return e.state("hmbridge/38f59c/state")["last_command"] == "ok: set_temperature 19.0"
	})
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetPreset, Preset: ha.PresetBoost}
	e.waitFor("failure after retries", func() bool {
		lc, _ := e.state("hmbridge/38f59c/state")["last_command"].(string)
		return strings.HasPrefix(lc, "failed: boost")
	})
}

// Opus review regression tests.

func TestStaleRetryDoesNotUndoOff(t *testing.T) {
	var mu sync.Mutex
	failed := false
	e := newEnv(t, testConfig, fakemod.Options{
		OnSend: func(m []byte) (byte, []byte) {
			mu.Lock()
			defer mu.Unlock()
			if len(m) > 9 && m[2] == 0x11 && m[9] == 0x86 && !failed {
				failed = true
				return fakemod.AckEUnknown, nil // first set_temperature unanswered
			}
			return fakemod.DefaultOnSend(m)
		},
	})
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetTemperature, Temperature: 22}
	e.waitFor("failed attempt", func() bool { return len(e.sentClimate(rtdn)) == 1 })
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetMode, Mode: ha.ModeOff}
	e.waitFor("off sent", func() bool { return len(e.sentClimate(rtdn)) == 2 })
	time.Sleep(300 * time.Millisecond) // > RetryDelay
	sent := e.sentClimate(rtdn)
	if len(sent) != 2 {
		t.Fatalf("sent %d commands, want 2: the retry of 22 °C must be superseded by off", len(sent))
	}
	if sent[1][9] != 0x81 || sent[1][11] != 9 {
		t.Errorf("last command = %x, want manual 4.5 (off)", sent[1][9:])
	}
}

func TestLastCommandRunsLast(t *testing.T) {
	q := newJobQueue()
	q.push(job{key: "climate:x:temperature", target: "climate:x", temp: 22})
	q.push(job{key: "climate:x:mode", target: "climate:x", action: actionManual, temp: 4.5})
	q.push(job{key: "climate:x:temperature", target: "climate:x", temp: 23})
	first, _ := q.pop()
	second, _ := q.pop()
	if first.key != "climate:x:mode" || second.temp != 23 {
		t.Errorf("order = %s, %s(%v); want mode first, then the newest temperature", first.key, second.key, second.temp)
	}
}

func TestLeaveBoostRestoresOff(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetMode, Mode: ha.ModeOff}
	e.waitFor("off", func() bool { return e.state("hmbridge/38f59c/state")["mode"] == "off" })
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetPreset, Preset: ha.PresetBoost}
	e.waitFor("boost", func() bool { return e.state("hmbridge/38f59c/state")["preset"] == "boost" })
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetPreset, Preset: ha.PresetNone}
	e.waitFor("three commands", func() bool { return len(e.sentClimate(rtdn)) == 3 })
	if m := e.sentClimate(rtdn)[2]; m[9] != 0x81 || m[11] != 9 {
		t.Errorf("leaving boost sent %x, want manual 4.5 (back to off)", m[9:])
	}
	// preset none while not boosting sends nothing.
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetPreset, Preset: ha.PresetNone}
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetTemperature, Temperature: 20}
	e.waitFor("set temperature", func() bool { return len(e.sentClimate(rtdn)) >= 4 })
	if m := e.sentClimate(rtdn)[3]; m[9] != 0x86 {
		t.Errorf("preset none outside boost sent %x", m[9:])
	}
}

func TestAESDeviceIgnoresUnauthenticatedForeignFrames(t *testing.T) {
	cfg := strings.Replace(testConfig, `"address": "4C5D6E"}`, `"address": "4C5D6E", "aes": true}`, 1)
	e := newEnv(t, cfg, fakemod.Options{})
	// A forged "open" EVENT broadcast (not challenged by the module).
	e.inject(0x11, msg(0x07, 0x84, 0x41, sco, [3]byte{}, 0x01, 0x1c, 0xc8))
	// Authenticated "closed" to the central is applied.
	e.inject(0x12, msg(0x08, 0x84, 0x41, sco, central, 0x01, 0x1d, 0x00))
	e.waitFor("closed", func() bool { return e.state("hmbridge/4c5d6e/state")["window_state"] == "closed" })
	// The broadcast was processed first; if it had been applied, the
	// message counter would be 2.
	if n := e.state("hmbridge/4c5d6e/state")["messages_received"]; n != 1.0 {
		t.Errorf("messages_received = %v, want 1 (the forged broadcast must be ignored)", n)
	}
}

func TestContactNonStandardOpenValue(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.inject(0x11, msg(0x07, 0x84, 0x41, sco, central, 0x01, 0x1c, 0x01))
	e.waitFor("open", func() bool {
		st := e.state("hmbridge/4c5d6e/state")
		return st["window_open"] == true && st["window_state"] == "open"
	})
}

func TestTimeRequestJumpsQueueAndNeedsSyncedClock(t *testing.T) {
	block := make(chan struct{})
	var once sync.Once
	synced := false
	var mu sync.Mutex
	e := newEnvWith(t, testConfig, fakemod.Options{
		OnSend: func(m []byte) (byte, []byte) {
			if len(m) > 2 && m[2] == 0x11 {
				once.Do(func() { <-block })
			}
			return fakemod.DefaultOnSend(m)
		},
	}, func(o *Options) {
		o.ClockSynced = func() bool { mu.Lock(); defer mu.Unlock(); return synced }
	})
	defer func() { close(block) }()

	// Clock not synced: no answer.
	e.inject(0x01, msg(0x41, 0xa6, 0x3f, tc, central))
	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn2, central, 0x0a, 0xb0, 0xe2, 0x08, 0x00, 0x00))
	e.waitFor("later frame", func() bool { return e.state("hmbridge/38f5a1/state")["current_temperature"] != nil })

	mu.Lock()
	synced = true
	mu.Unlock()
	// Occupy the worker with a command and queue another one; the
	// time response must go before the queued command.
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetTemperature, Temperature: 21}
	e.waitFor("worker busy", func() bool { return len(e.sentClimate(rtdn)) == 1 })
	e.b.Commands() <- ha.Command{Addr: "38f5a1", Kind: ha.SetTemperature, Temperature: 21}
	e.inject(0x01, msg(0x42, 0xa6, 0x3f, tc, central))
	e.waitFor("queued", func() bool { return e.b.queue.len() == 2 })
	// Popping from the live queue is safe here: the worker is blocked in
	// the first command (OnSend waits for block) and cannot pop.
	j, _ := e.b.queue.pop()
	if j.kind != jobTimeResponse || j.reqNo != 0x42 {
		t.Errorf("first queued job = %+v, want the time response to request 42", j)
	}
	for _, m := range e.mod.Sent() {
		if m[2] == 0x3f && m[0] == 0x41 {
			t.Errorf("answered a time request while the clock was not synchronised")
		}
	}
}

func TestStatePersistedAndRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := strings.Replace(testConfig, `"state_file": "",`, `"state_file": "`+path+`",`, 1)
	e := newEnv(t, cfg, fakemod.Options{})
	e.inject(0x11, msg(0x07, 0x84, 0x41, sco, central, 0x01, 0x1c, 0xc8))
	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	e.waitFor("state", func() bool { return e.state("hmbridge/38f59c/state")["current_temperature"] != nil })
	if err := e.b.saveState(path); err != nil {
		t.Fatal(err)
	}

	// A new bridge starts from the saved state and publishes it at once;
	// a device without saved state gets no (empty) state message.
	e2 := newEnv(t, cfg, fakemod.Options{})
	e2.waitFor("restored contact", func() bool { return e2.state("hmbridge/4c5d6e/state")["window_open"] == true })
	e2.waitFor("restored thermostat", func() bool { return e2.state("hmbridge/38f59c/state")["valve_position"] == 37.0 })
	e2.waitFor("group window", func() bool { return e2.state("hmbridge/group/wohnzimmer/state")["window_open"] == true })
	if b := e2.pub.get("hmbridge/2a1b3c/state"); b != nil {
		t.Errorf("published state for a device never heard: %s", b)
	}
	if st := e2.state("hmbridge/38f59c/state"); st["messages_received"] != nil {
		t.Errorf("message counter restored: %v", st["messages_received"])
	}
}

func TestDeviceAvailability(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	e := newEnvWith(t, testConfig, fakemod.Options{}, func(o *Options) {
		o.Now = clock
		o.HousekeepingInterval = 20 * time.Millisecond
	})
	avail := func(addr string) string { return string(e.pub.get("hmbridge/" + addr + "/availability")) }
	e.waitFor("initially online", func() bool { return avail("38f59c") == "online" && avail("4c5d6e") == "online" })

	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	e.waitFor("state", func() bool { return e.state("hmbridge/38f59c/state")["current_temperature"] != nil })
	advance(31 * time.Minute)
	e.waitFor("thermostats offline", func() bool { return avail("38f59c") == "offline" && avail("2a1b3c") == "offline" })
	if avail("4c5d6e") != "online" {
		t.Errorf("contact offline after 31 minutes; its timeout is 6h")
	}
	e.inject(0x01, msg(0x06, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	e.waitFor("back online", func() bool { return avail("38f59c") == "online" })
}

// Persistence and availability (review of da1ada5).

func readStateFile(t *testing.T, path string) (persistedState, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return persistedState{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatal(err)
	}
	return ps, true
}

func TestStateFileWrittenOnlyForSignificantChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := strings.Replace(testConfig, `"state_file": "",`, `"state_file": "`+path+`",`, 1)
	e := newEnvWith(t, cfg, fakemod.Options{}, func(o *Options) {
		o.HousekeepingInterval = 10 * time.Millisecond
		o.StateSaveInterval = 50 * time.Millisecond
	})
	// The first report is significant (mode, target, battery known now).
	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	e.waitFor("first save", func() bool { _, ok := readStateFile(t, path); return ok })
	first, _ := os.Stat(path)

	// Only the temperature and valve change: no write.
	e.inject(0x01, msg(0x06, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe8, 0x08, 0x30, 0x00))
	e.waitFor("processed", func() bool { return e.state("hmbridge/38f59c/state")["valve_position"] == 48.0 })
	time.Sleep(200 * time.Millisecond)
	if st, _ := os.Stat(path); !st.ModTime().Equal(first.ModTime()) {
		t.Errorf("state file rewritten for a temperature-only change")
	}

	// A window opening is significant: written after the interval.
	e.inject(0x11, msg(0x07, 0x84, 0x41, sco, central, 0x01, 0x1c, 0xc8))
	e.waitFor("window saved", func() bool {
		ps, _ := readStateFile(t, path)
		wo := ps.Devices["4c5d6e"].State.WindowOpen
		return wo != nil && *wo
	})
}

func TestStateFileWrittenOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := strings.Replace(testConfig, `"state_file": "",`, `"state_file": "`+path+`",`, 1)
	e := newEnv(t, cfg, fakemod.Options{}) // default interval: 15 minutes
	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	e.waitFor("processed", func() bool { return e.state("hmbridge/38f59c/state")["current_temperature"] != nil })
	if _, ok := readStateFile(t, path); ok {
		t.Fatal("state file written before the save interval passed")
	}
	e.stop()
	ps, ok := readStateFile(t, path)
	if !ok || ps.Devices["38f59c"].State.CurrentTemperature == nil {
		t.Errorf("state not saved on shutdown: %+v", ps)
	}
}

func TestStateSaveFailureIsRetried(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o644)
	path := filepath.Join(blocker, "state.json") // parent is a file: writes fail
	cfg := strings.Replace(testConfig, `"state_file": "",`, `"state_file": "`+path+`",`, 1)
	var mu sync.Mutex
	var logs []string
	e := newEnvWith(t, cfg, fakemod.Options{}, func(o *Options) {
		o.HousekeepingInterval = 10 * time.Millisecond
		o.StateSaveInterval = 20 * time.Millisecond
		o.Logf = func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }
	})
	e.inject(0x01, msg(0x05, 0xa6, 0x10, rtdn, central, 0x0a, 0xb0, 0xe2, 0x08, 0x25, 0x00))
	time.Sleep(300 * time.Millisecond) // many save attempts
	count := func(sub string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range logs {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}
	if n := count("saving state to"); n != 1 {
		t.Errorf("save failure logged %d times, want once", n)
	}
	e.b.mu.Lock()
	dirty := e.b.significantDirty
	e.b.mu.Unlock()
	if !dirty {
		t.Error("state not dirty after a failed save; it would not be retried")
	}
	// Make the path writable: the next attempt succeeds.
	os.Remove(blocker)
	e.waitFor("saved after recovery", func() bool { _, ok := readStateFile(t, path); return ok })
	e.waitFor("recovery logged", func() bool { return count("works again") == 1 })
}

func TestRestoredStaleDeviceStartsUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := time.Now().Add(-2 * time.Hour)
	writeFileAtomic(path, persistedState{Version: 1, Saved: old, Devices: map[string]persistedDevice{
		"38f59c": {State: ha.State{CurrentTemperature: ptr(20.5), LastSeen: old}},
		"4c5d6e": {State: ha.State{WindowOpen: ptr(true), WindowState: "open", LastSeen: old}},
	}})
	cfg := strings.Replace(testConfig, `"state_file": "",`, `"state_file": "`+path+`",`, 1)
	e := newEnv(t, cfg, fakemod.Options{})
	avail := func(topic string) string { return string(e.pub.get(topic)) }
	e.waitFor("availability", func() bool { return avail("hmbridge/38f59c/availability") != "" })
	if a := avail("hmbridge/38f59c/availability"); a != "offline" {
		t.Errorf("thermostat last heard 2h ago: availability %q, want offline", a)
	}
	if a := avail("hmbridge/4c5d6e/availability"); a != "online" {
		t.Errorf("contact last heard 2h ago (timeout 6h): availability %q, want online", a)
	}
	if a := avail("hmbridge/2a1b3c/availability"); a != "online" {
		t.Errorf("never-heard device right after start: availability %q, want online", a)
	}
	// The group has the wall thermostat (never heard, online) → online.
	if a := avail("hmbridge/group/wohnzimmer/availability"); a != "online" {
		t.Errorf("group availability %q, want online", a)
	}
}

func TestRestoreExpiresBoost(t *testing.T) {
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	b, err := New(Options{Config: cfg, Radio: nopRadio{}, Now: func() time.Time { return now }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	d := b.byAddr[rtdn]
	boost := ha.State{ControlMode: "boost", Mode: "heat", Preset: "boost", BoostMinutes: ptr(5), TargetTemperature: ptr(21.0)}

	expired := boost
	expired.LastSeen = now.Add(-10 * time.Minute)
	b.restore(d, expired, 0, ha.ModeOff)
	if d.state.ControlMode != "" || d.state.Preset != ha.PresetNone || d.state.Mode != ha.ModeOff {
		t.Errorf("expired boost restored as %q/%q/%q, want no boost, mode off", d.state.ControlMode, d.state.Preset, d.state.Mode)
	}
	if d.lastOnTarget != 21 {
		t.Errorf("lastOnTarget = %v, want 21 (from the restored target)", d.lastOnTarget)
	}

	running := boost
	running.LastSeen = now.Add(-2 * time.Minute)
	b.restore(d, running, 0, ha.ModeAuto)
	if d.state.ControlMode != "boost" {
		t.Errorf("running boost not restored")
	}
}

func TestSeedFromRetained(t *testing.T) {
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{Config: cfg, Radio: nopRadio{}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	topics := ha.TopicConfig{}
	enc := func(s ha.State) []byte { m, _ := ha.StateMessage(topics, "x", s); return m.Payload }
	// The wall thermostat was heard since startup: its retained state is older.
	b.byAddr[tc].heardSinceStart = true
	b.byAddr[tc].state = ha.State{CurrentTemperature: ptr(23.0), LastSeen: now}
	b.SeedFromRetained(topics, map[string][]byte{
		"hmbridge/38f59c/state":           enc(ha.State{TargetTemperature: ptr(19.5), MessagesReceived: ptr(uint64(7)), LastSeen: now}),
		"hmbridge/4c5d6e/state":           enc(ha.State{WindowOpen: ptr(true), WindowState: "open", LastSeen: now}),
		"hmbridge/2a1b3c/state":           enc(ha.State{CurrentTemperature: ptr(18.0), LastSeen: now.Add(-time.Hour)}),
		"hmbridge/ffffff/state":           enc(ha.State{LastSeen: now}), // not configured
		"hmbridge/group/wohnzimmer/state": enc(ha.State{WindowOpen: ptr(false), LastSeen: now}),
		"hmbridge/38f5a1/state":           []byte("not json"),
	})
	if got := b.byAddr[rtdn].state; got.TargetTemperature == nil || *got.TargetTemperature != 19.5 || got.MessagesReceived != nil {
		t.Errorf("RT-DN seeded as %+v", got)
	}
	if got := b.byAddr[tc].state.CurrentTemperature; *got != 23 {
		t.Errorf("seed overwrote a device heard since startup: %v", *got)
	}
	g := b.bySlug["wohnzimmer"]
	if g.state.WindowOpen == nil || !*g.state.WindowOpen {
		t.Errorf("group window = %v, want derived from the seeded contact (open)", g.state.WindowOpen)
	}
}

// nopRadio is a Radio that does nothing (for tests that don't run the bridge).
type nopRadio struct{}

func (nopRadio) Send(context.Context, []byte) ([]byte, error) { return nil, nil }
func (nopRadio) Recv() <-chan []byte                          { return nil }
func (nopRadio) Err() error                                   { return nil }
func (nopRadio) AddPeer(context.Context, uartgw.Peer) error   { return nil }
func (nopRadio) SetTime(context.Context, time.Time) error     { return nil }
