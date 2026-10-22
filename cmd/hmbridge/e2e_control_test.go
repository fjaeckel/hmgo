// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/fjaeckel/hmgo/internal/bridge"
	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// e2eClock is a settable clock for the bridge (fallback_after is at
// least 5 minutes).
type e2eClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *e2eClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *e2eClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestEndToEndHomeAssistantControl runs a heating group under Home
// Assistant control against a real broker: discovery without auto,
// debounced setpoints, the fail-safe when the broker goes away and the
// hand back when it returns.
func TestEndToEndHomeAssistantControl(t *testing.T) {
	mosquitto, err := exec.LookPath("mosquitto")
	if err != nil {
		if _, err2 := os.Stat("/usr/sbin/mosquitto"); err2 != nil {
			t.Skip("mosquitto not installed")
		}
		mosquitto = "/usr/sbin/mosquitto"
	}
	brokerPort := freePort(t)
	httpPort := freePort(t)
	dir := t.TempDir()
	conf := filepath.Join(dir, "mosquitto.conf")
	os.WriteFile(conf, []byte(fmt.Sprintf("listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n", brokerPort)), 0o644)
	var broker *exec.Cmd
	startBroker := func() {
		broker = exec.Command(mosquitto, "-c", conf)
		if err := broker.Start(); err != nil {
			t.Fatal(err)
		}
		waitTCP(t, fmt.Sprintf("127.0.0.1:%d", brokerPort))
	}
	stopBroker := func() {
		if broker != nil {
			broker.Process.Kill()
			broker.Wait()
			broker = nil
		}
	}
	startBroker()
	t.Cleanup(stopBroker)

	stateDir := filepath.Join(dir, "state")
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "central_address": "1A2B3C",
	  "reset": "none",
	  "state_dir": %q,
	  "require_synced_clock": false,
	  "mqtt": {"broker": "tcp://127.0.0.1:%d"},
	  "http_listen": "127.0.0.1:%d",
	  "home_assistant_control": {"setpoint_debounce": "300ms"},
	  "devices": [
	    {"name": "Bad", "model": "HM-CC-RT-DN", "address": "38F59C"},
	    {"name": "Bad Fenster", "model": "HM-Sec-SCo", "address": "4C5D6E"}
	  ],
	  "groups": [{"name": "Bad", "control": "home_assistant", "fallback_after": "5m", "members": ["Bad", "Bad Fenster"]}]
	}`, stateDir, brokerPort, httpPort)))
	if err != nil {
		t.Fatal(err)
	}
	clock := &e2eClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	testBridgeOptions = func(o *bridge.Options) {
		o.Now = clock.Now
		o.ControlTick = 20 * time.Millisecond
		o.ControlStartupDelay = 1000 * time.Hour
	}
	t.Cleanup(func() { testBridgeOptions = nil })

	// Home Assistant's side.
	var mu sync.Mutex
	seen := map[string][]byte{}
	connectHA := func() mqtt.Client {
		c := mqtt.NewClient(mqtt.NewClientOptions().
			AddBroker(fmt.Sprintf("tcp://127.0.0.1:%d", brokerPort)).
			SetClientID("fake-home-assistant").SetAutoReconnect(false))
		if tok := c.Connect(); tok.Wait() && tok.Error() != nil {
			t.Fatal(tok.Error())
		}
		record := func(_ mqtt.Client, m mqtt.Message) {
			mu.Lock()
			seen[m.Topic()] = append([]byte(nil), m.Payload()...)
			mu.Unlock()
		}
		for _, topic := range []string{"hmbridge/#", "homeassistant/#"} {
			if tok := c.Subscribe(topic, 0, record); tok.Wait() && tok.Error() != nil {
				t.Fatal(tok.Error())
			}
		}
		return c
	}
	ha := connectHA()
	defer func() { ha.Disconnect(100) }()
	jsonOf := func(topic string) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var v map[string]any
		json.Unmarshal(seen[topic], &v)
		return v
	}
	publish := func(topic, payload string) {
		t.Helper()
		if tok := ha.Publish(topic, 0, false, payload); tok.Wait() && tok.Error() != nil {
			t.Fatal(tok.Error())
		}
	}

	mod, rw := fakemod.New(fakemod.Options{})
	defer mod.Close()
	sent := func(prefix ...byte) int {
		n := 0
		for _, m := range mod.Sent() {
			if len(m) >= 9+len(prefix) && m[2] == 0x11 && bytes.Equal(m[6:9], []byte{0x38, 0xf5, 0x9c}) && bytes.Equal(m[9:9+len(prefix)], prefix) {
				n++
			}
		}
		return n
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, rw) }()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, "climate discovery without auto", func() bool {
		return fmt.Sprint(jsonOf("homeassistant/climate/hmbridge_group_bad/config")["modes"]) == "[heat off]" &&
			fmt.Sprint(jsonOf("homeassistant/climate/hmbridge_38f59c/config")["modes"]) == "[heat off]"
	})
	waitFor(t, "control sensor", func() bool {
		return jsonOf("homeassistant/sensor/hmbridge_group_bad/control/config")["unique_id"] == "hmbridge_group_bad_control"
	})

	// Several setpoints within the debounce time: one radio command.
	for _, v := range []string{"20", "20.5", "21"} {
		publish("hmbridge/group/bad/set/temperature", v)
	}
	waitFor(t, "manual 21", func() bool { return sent(0x81, 0x04, 42) == 1 })
	time.Sleep(500 * time.Millisecond)
	if n := sent(); n != 1 {
		t.Fatalf("%d radio commands for three setpoints, want 1", n)
	}
	waitFor(t, "last command", func() bool {
		return jsonOf("hmbridge/38f59c/state")["last_command"] == "ok: manual 21.0 (latest of 3 setpoints)"
	})
	waitFor(t, "group control", func() bool { return jsonOf("hmbridge/group/bad/state")["control"] == "home_assistant" })

	// The broker goes away: after fallback_after the week program runs.
	ha.Disconnect(0)
	stopBroker()
	time.Sleep(300 * time.Millisecond)
	clock.Advance(4 * time.Minute)
	time.Sleep(200 * time.Millisecond)
	if n := sent(0x80); n != 0 {
		t.Fatal("fallback before fallback_after")
	}
	clock.Advance(2 * time.Minute)
	waitFor(t, "auto mode", func() bool { return sent(0x80, 0x04) == 1 })
	if m := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", httpPort)); !strings.Contains(m, `hmbridge_control_fallback_active{kind="group",name="Bad"} 1`) {
		t.Error("fallback metric not 1")
	}
	b, err := os.ReadFile(filepath.Join(stateDir, "control.json"))
	if err != nil || !strings.Contains(string(b), `"fallback":true`) {
		t.Errorf("control.json: %s %v", b, err)
	}

	// Back: Home Assistant's setpoint after a minute.
	startBroker()
	ha = connectHA()
	waitFor(t, "MQTT reconnected", func() bool {
		return strings.Contains(httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", httpPort)), "\nhmbridge_mqtt_connected 1\n")
	})
	time.Sleep(200 * time.Millisecond)
	clock.Advance(2 * time.Minute)
	waitFor(t, "manual 21 again", func() bool { return sent(0x81, 0x04, 42) == 2 })
	waitFor(t, "group control back", func() bool { return jsonOf("hmbridge/group/bad/state")["control"] == "home_assistant" })
	if n := sent(0x80); n != 1 {
		t.Errorf("%d auto commands, want 1", n)
	}
}
