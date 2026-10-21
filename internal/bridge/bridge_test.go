package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
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

// fakePub records published messages.
type fakePub struct {
	mu   sync.Mutex
	msgs map[string][]byte
}

func (p *fakePub) Publish(m ha.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.msgs == nil {
		p.msgs = make(map[string][]byte)
	}
	p.msgs[m.Topic] = append([]byte(nil), m.Payload...)
	return nil
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
}

func newEnv(t *testing.T, cfgJSON string, modOpts fakemod.Options) *env {
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
	b, err := New(Options{
		Config:     cfg,
		Radio:      conn,
		RetryDelay: 50 * time.Millisecond,
		Logf:       t.Logf,
	})
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
	go func() { e.run <- b.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-e.run
	})
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
	e.waitFor("queued", func() bool { return e.b.queue.len() == 1 })
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
	old := job{key: "climate:x:temperature", temp: 18}
	old.seq = q.push(old)
	if _, ok := q.pop(); !ok {
		t.Fatal("pop failed")
	}
	newer := job{key: "climate:x:temperature", temp: 22}
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
