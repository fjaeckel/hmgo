// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	bidcosaes "github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/fjaeckel/hmgo/internal/bridge"
	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// fakeHA is Home Assistant's side of the broker: it records the last
// message per topic and every event message.
type fakeHA struct {
	t      *testing.T
	c      mqtt.Client
	mu     sync.Mutex
	last   map[string][]byte
	events []string // "<topic> <event_type>"
}

func newFakeHA(t *testing.T, broker, id string) *fakeHA {
	t.Helper()
	h := &fakeHA{t: t, last: map[string][]byte{}}
	h.c = mqtt.NewClient(mqtt.NewClientOptions().AddBroker(broker).SetClientID(id))
	if tok := h.c.Connect(); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	t.Cleanup(func() { h.c.Disconnect(100) })
	for _, topic := range []string{"hmbridge/#", "homeassistant/#"} {
		h.c.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.last[m.Topic()] = append([]byte(nil), m.Payload()...)
			if strings.HasPrefix(m.Topic(), "hmbridge/") && strings.Contains(m.Topic(), "/event/") {
				var v struct {
					EventType string `json:"event_type"`
				}
				json.Unmarshal(m.Payload(), &v)
				h.events = append(h.events, m.Topic()+" "+v.EventType)
			}
		}).Wait()
	}
	return h
}

func (h *fakeHA) json(topic string) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var v map[string]any
	json.Unmarshal(h.last[topic], &v)
	return v
}

func (h *fakeHA) eventLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func (h *fakeHA) publish(topic, payload string) {
	h.t.Helper()
	if tok := h.c.Publish(topic, 1, false, payload); tok.Wait() && tok.Error() != nil {
		h.t.Fatal(tok.Error())
	}
}

func fastBridge(t *testing.T) {
	testBridgeOptions = func(o *bridge.Options) {
		o.RetryDelay = 50 * time.Millisecond
		o.StatusPollStagger = 20 * time.Millisecond
	}
	t.Cleanup(func() { testBridgeOptions = nil })
}

func startRun(t *testing.T, cfg *config.Config, rw interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, rw) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("run did not return")
		}
	})
}

var (
	e2eCentral = []byte{0x45, 0x80, 0x86}
	e2ePlug    = []byte{0x6a, 0x9d, 0x15}
	e2eDim     = []byte{0x41, 0xe9, 0x51}
	e2ePB      = []byte{0x73, 0x78, 0x8e}
	e2eLED     = []byte{0x75, 0x64, 0xea}
	e2eWDS     = []byte{0x6f, 0xca, 0x4a}
)

func bmsg(cnt, flags, cmd byte, src, dst []byte, payload ...byte) []byte {
	m := append([]byte{cnt, flags, cmd}, src...)
	m = append(m, dst...)
	return append(m, payload...)
}

// actuatorReply emulates the switch and dimmer plugs: LEVEL_SET is
// answered with ACK_STATUS, the status request with INFO_LEVEL. The LED
// display never answers. nil: not an actuator message.
func actuatorReply(m []byte, level map[string]byte) []byte {
	if len(m) < 11 || m[1]&0x20 == 0 {
		return nil
	}
	dst := string(m[6:9])
	if dst != string(e2ePlug) && dst != string(e2eDim) {
		return nil
	}
	reply := func(cmd byte, p ...byte) []byte { return bmsg(m[0], 0x80, cmd, m[6:9], m[3:6], p...) }
	switch {
	case m[2] == 0x11 && m[9] == 0x02 && len(m) >= 12:
		level[dst] = m[11]
		return reply(0x02, 0x01, m[10], m[11], 0x00, 0x41)
	case m[2] == 0x01 && m[10] == 0x0e:
		return reply(0x10, 0x06, m[9], level[dst], 0x00)
	}
	return nil
}

func sentMatching(mod *fakemod.Module, f func([]byte) bool) int {
	n := 0
	for _, m := range mod.Sent() {
		if f(m) {
			n++
		}
	}
	return n
}

// TestEndToEndDevices: switch, dimmer, push button, weather sensor and
// LED display with an emulated HM-MOD-RPI-PCB, a real broker and Home
// Assistant played via MQTT.
func TestEndToEndDevices(t *testing.T) {
	broker := startBroker(t)
	httpPort := freePort(t)
	fastBridge(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "central_address": "458086",
	  "reset": "none",
	  "state_file": "",
	  "require_synced_clock": false,
	  "mqtt": {"broker": %q},
	  "http_listen": "127.0.0.1:%d",
	  "devices": [
	    {"name": "Stehlampe", "model": "HM-LC-Sw1-Pl-DN-R1", "address": "6A9D15"},
	    {"name": "Leselampe", "model": "HM-LC-Dim1L-Pl-3", "address": "41E951"},
	    {"name": "Taster", "model": "HM-PB-2-WM55", "address": "73788E"},
	    {"name": "Status", "model": "HM-OU-LED16", "address": "7564EA"},
	    {"name": "Garten", "model": "HM-WDS10-TH-O", "address": "6FCA4A"}
	  ]
	}`, broker, httpPort)))
	if err != nil {
		t.Fatal(err)
	}
	ha := newFakeHA(t, broker, "fake-ha-devices")

	var lmu sync.Mutex
	level := map[string]byte{}
	mod, rw := fakemod.New(fakemod.Options{OnSend: func(m []byte) (byte, []byte) {
		lmu.Lock()
		defer lmu.Unlock()
		if r := actuatorReply(m, level); r != nil {
			return fakemod.AckWithResponse, r
		}
		if len(m) >= 9 && bytes.Equal(m[6:9], e2eLED) {
			return fakemod.AckEUnknown, nil // the user's LED16 never answers
		}
		return fakemod.DefaultOnSend(m)
	}})
	defer mod.Close()
	startRun(t, cfg, rw)

	waitFor(t, "discovery", func() bool {
		sw := ha.json("homeassistant/switch/hmbridge_6a9d15/switch/config")
		light := ha.json("homeassistant/light/hmbridge_41e951/light/config")
		ev := ha.json("homeassistant/event/hmbridge_73788e/button_2/config")
		trig := ha.json("homeassistant/device_automation/hmbridge_73788e/button_1_press_long/config")
		sel := ha.json("homeassistant/select/hmbridge_7564ea/led_16/config")
		temp := ha.json("homeassistant/sensor/hmbridge_6fca4a/humidity/config")
		return sw["command_topic"] == "hmbridge/6a9d15/set/switch" && light["schema"] == "template" &&
			ev["state_topic"] == "hmbridge/73788e/event/button_2" && trig["type"] == "button_long_press" &&
			sel["command_topic"] == "hmbridge/7564ea/set/led_16" && temp["device_class"] == "humidity"
	})

	// Switch: status known after startup, then on via Home Assistant
	// with the ACK_STATUS round trip.
	waitFor(t, "switch status after startup", func() bool { return ha.json("hmbridge/6a9d15/state")["on"] == false })
	ha.publish("hmbridge/6a9d15/set/switch", "ON")
	waitFor(t, "switch on", func() bool {
		st := ha.json("hmbridge/6a9d15/state")
		return st["on"] == true && st["level"] == 100.0 && st["last_command"] == "ok: switch on"
	})
	if n := sentMatching(mod, func(m []byte) bool {
		return m[1] == 0xa0 && m[2] == 0x11 && bytes.Equal(m[6:9], e2ePlug) && bytes.Equal(m[9:], []byte{0x02, 0x01, 0xc8, 0x00, 0x00})
	}); n != 1 {
		t.Errorf("%d LEVEL_SET on frames", n)
	}
	ha.publish("hmbridge/6a9d15/set/switch", "OFF")
	waitFor(t, "switch off", func() bool { return ha.json("hmbridge/6a9d15/state")["on"] == false })

	// Dimmer: brightness 128 of 255 → 50 %, ramp 1.5 s.
	ha.publish("hmbridge/41e951/set/light", `{"state": "on", "brightness": 128, "transition": 1.5}`)
	waitFor(t, "dimmer at 50 %", func() bool {
		st := ha.json("hmbridge/41e951/state")
		return st["level"] == 50.0 && st["on"] == true
	})
	if n := sentMatching(mod, func(m []byte) bool {
		return m[2] == 0x11 && bytes.Equal(m[6:9], e2eDim) && bytes.Equal(m[9:], []byte{0x02, 0x01, 0x64, 0x01, 0xe0})
	}); n != 1 {
		t.Errorf("%d LEVEL_SET 50 %% frames", n)
	}

	// Push button: short press, then a long press (two repeats and the
	// release). The module acknowledges BiDi messages itself.
	mod.InjectRecv(0x01, bmsg(0x10, 0xa0, 0x40, e2ePB, e2eCentral, 0x01, 0x20))
	mod.InjectRecv(0x01, bmsg(0x11, 0x84, 0x40, e2ePB, e2eCentral, 0x42, 0x21))
	mod.InjectRecv(0x01, bmsg(0x12, 0x84, 0x40, e2ePB, e2eCentral, 0x42, 0x21))
	mod.InjectRecv(0x01, bmsg(0x13, 0xa4, 0x40, e2ePB, e2eCentral, 0x42, 0x21))
	want := []string{
		"hmbridge/73788e/event/button_1 press_short",
		"hmbridge/73788e/event/button_2 press_long",
		"hmbridge/73788e/event/button_2 press_long_release",
	}
	waitFor(t, "button events", func() bool { return fmt.Sprint(ha.eventLog()) == fmt.Sprint(want) })

	// Weather sensor broadcast (captured frame).
	mod.InjectRecv(0x01, bmsg(0xce, 0x86, 0x70, e2eWDS, []byte{0, 0, 0}, 0x00, 0x64, 0x50))
	waitFor(t, "weather", func() bool {
		st := ha.json("hmbridge/6fca4a/state")
		return st["current_temperature"] == 10.0 && st["humidity"] == 80.0
	})

	// LED display: never answers; the command fails cleanly after the
	// retries and Home Assistant shows it.
	ha.publish("hmbridge/7564ea/set/led_3", "green")
	waitFor(t, "LED command failed", func() bool {
		lc, _ := ha.json("hmbridge/7564ea/state")["last_command"].(string)
		return strings.HasPrefix(lc, "failed: led 3 green") && strings.Contains(lc, "did not respond")
	})
	if n := sentMatching(mod, func(m []byte) bool {
		return m[2] == 0x11 && bytes.Equal(m[6:9], e2eLED) && bytes.Equal(m[9:], []byte{0x80, 0x03, 0x02})
	}); n != 3 {
		t.Errorf("%d SET_ONE_CHANNEL frames, want 3 (2 retries)", n)
	}

	// Counters are process-wide (go test -count): only check that they
	// exist; gauges have exact values.
	metrics := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", httpPort))
	for _, w := range []string{
		`hmbridge_device_level_percent{address="41e951",model="HM-LC-Dim1L-Pl-3",name="Leselampe"} 50`,
		`hmbridge_device_button_presses_total{address="73788e",button="2",model="HM-PB-2-WM55",name="Taster",type="press_long_release"} `,
		`hmbridge_device_temperature_celsius{address="6fca4a",model="HM-WDS10-TH-O",name="Garten"} 10`,
		`hmbridge_commands_total{action="led",result="error"} `,
		`hmbridge_mqtt_commands_received_total{kind="light"} `,
	} {
		if !strings.Contains(metrics, w) {
			t.Errorf("/metrics lacks %q", w)
		}
	}
	page := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", httpPort))
	for _, w := range []string{"on, 50 %", "last: button 2 press_long_release"} {
		if !strings.Contains(page, w) {
			t.Errorf("status page lacks %q", w)
		}
	}
}

// TestEndToEndRPIRFModButtonAES: an AES push button on the RPI-RF-MOD,
// where hmbridge acknowledges and authenticates in software, plus a
// switch command.
func TestEndToEndRPIRFModButtonAES(t *testing.T) {
	broker := startBroker(t)
	fastBridge(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "central_address": "458086",
	  "reset": "none",
	  "state_file": "",
	  "require_synced_clock": false,
	  "security": {"passphrase": "geheim", "index": 2},
	  "mqtt": {"broker": %q},
	  "http_listen": "",
	  "devices": [
	    {"name": "Taster", "model": "HM-PB-2-WM55", "address": "73788E", "aes": true},
	    {"name": "Stehlampe", "model": "HM-LC-Sw1-Pl-DN-R1", "address": "6A9D15", "aes": true}
	  ]
	}`, broker)))
	if err != nil {
		t.Fatal(err)
	}
	ha := newFakeHA(t, broker, "fake-ha-dual-aes")
	device := bidcosaes.NewSigner(hmkey.FromPassphrase("geheim", 2))

	var mu sync.Mutex
	pending := map[byte][]byte{} // message counter → button message awaiting the challenge
	level := map[string]byte{}
	var challenges int
	mod, rw := fakemod.New(fakemod.Options{DualCoPro: true, OnLLMACSend: func(m []byte) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		// The bridge challenges the button's message: answer like the
		// device, with the real AES key.
		if m[2] == 0x02 && len(m) >= 17 && m[9] == 0x04 && bytes.Equal(m[6:9], e2ePB) {
			orig := pending[m[0]]
			if orig == nil {
				return nil
			}
			challenges++
			var ch [6]byte
			copy(ch[:], m[10:16])
			r, err := device.Respond(m[16]/2, ch, orig)
			if err != nil {
				t.Errorf("Respond: %v", err)
				return nil
			}
			return [][]byte{bmsg(m[0], 0xa0, 0x03, e2ePB, e2eCentral, r[:]...)}
		}
		if r := actuatorReply(m, level); r != nil {
			return [][]byte{r}
		}
		return fakemod.DefaultOnLLMACSend(m)
	}})
	defer mod.Close()
	startRun(t, cfg, rw)
	// Events are only published once the bridge publishes (after it has
	// read back the retained state), like the discovery.
	waitFor(t, "discovery", func() bool {
		return ha.json("homeassistant/event/hmbridge_73788e/button_1/config")["state_topic"] == "hmbridge/73788e/event/button_1"
	})

	press := func(cnt, flags, btn, counter byte) {
		m := bmsg(cnt, flags, 0x40, e2ePB, e2eCentral, btn, counter)
		mu.Lock()
		pending[cnt] = m
		mu.Unlock()
		if err := mod.InjectLLMACRecv(0x40, m); err != nil {
			t.Fatal(err)
		}
	}
	press(0x20, 0xa0, 0x01, 0x30) // short, button 1
	waitFor(t, "short press event", func() bool {
		return fmt.Sprint(ha.eventLog()) == "[hmbridge/73788e/event/button_1 press_short]"
	})
	// The final ACK carries the 4 auth bytes after the successful handshake.
	waitFor(t, "ACK with auth bytes", func() bool {
		return sentMatching(mod, func(m []byte) bool {
			return m[0] == 0x20 && m[2] == 0x02 && bytes.Equal(m[6:9], e2ePB) && len(m) == 14 && m[9] == 0x00
		}) == 1
	})
	// Long press: repeats without BiDi can't be authenticated and are
	// ignored on the AES channel; the challenged release brings both.
	press(0x21, 0x84, 0x42, 0x31)
	press(0x22, 0x84, 0x42, 0x31)
	press(0x23, 0xa4, 0x42, 0x31)
	waitFor(t, "long press events", func() bool {
		return fmt.Sprint(ha.eventLog()) == "[hmbridge/73788e/event/button_1 press_short hmbridge/73788e/event/button_2 press_long hmbridge/73788e/event/button_2 press_long_release]"
	})
	mu.Lock()
	if challenges != 2 {
		t.Errorf("%d challenges, want 2 (the BiDi messages)", challenges)
	}
	mu.Unlock()
	if st := ha.json("hmbridge/73788e/state"); st["auth_status"] != "ok" || st["low_battery"] != false {
		t.Errorf("button state %v", st)
	}

	// Switch on the RPI-RF-MOD.
	ha.publish("hmbridge/6a9d15/set/switch", "ON")
	waitFor(t, "switch on", func() bool {
		st := ha.json("hmbridge/6a9d15/state")
		return st["on"] == true && st["last_command"] == "ok: switch on"
	})
}

func (h *fakeHA) lastRaw(topic string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.last[topic])
}
