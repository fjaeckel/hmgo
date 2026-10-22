// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// TestEndToEnd runs hmbridge's main wiring against an emulated radio
// module and a real mosquitto broker, and plays Home Assistant via MQTT.
func TestEndToEnd(t *testing.T) {
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
	broker := exec.Command(mosquitto, "-c", conf)
	if err := broker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { broker.Process.Kill(); broker.Wait() })
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", brokerPort))

	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "central_address": "1A2B3C",
	  "reset": "none",
	  "state_file": "",
	  "require_synced_clock": false,
	  "mqtt": {"broker": "tcp://127.0.0.1:%d"},
	  "http_listen": "127.0.0.1:%d",
	  "devices": [
	    {"name": "Bad", "model": "HM-CC-RT-DN", "address": "38F59C"},
	    {"name": "Bad Fenster", "model": "HM-Sec-SCo", "address": "4C5D6E"}
	  ],
	  "groups": [{"name": "Bad", "members": ["Bad", "Bad Fenster"]}]
	}`, brokerPort, httpPort)))
	if err != nil {
		t.Fatal(err)
	}

	// Home Assistant's side: record everything hmbridge publishes.
	var mu sync.Mutex
	seen := map[string][]byte{}
	ha := mqtt.NewClient(mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://127.0.0.1:%d", brokerPort)).
		SetClientID("fake-home-assistant"))
	if tok := ha.Connect(); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer ha.Disconnect(100)
	record := func(_ mqtt.Client, m mqtt.Message) {
		mu.Lock()
		seen[m.Topic()] = append([]byte(nil), m.Payload()...)
		mu.Unlock()
	}
	for _, topic := range []string{"hmbridge/#", "homeassistant/#"} {
		if tok := ha.Subscribe(topic, 0, record); tok.Wait() && tok.Error() != nil {
			t.Fatal(tok.Error())
		}
	}
	get := func(topic string) []byte {
		mu.Lock()
		defer mu.Unlock()
		return seen[topic]
	}
	jsonOf := func(topic string) map[string]any {
		var v map[string]any
		json.Unmarshal(get(topic), &v)
		return v
	}

	mod, rw := fakemod.New(fakemod.Options{})
	defer mod.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, rw) }()

	waitFor(t, "availability online", func() bool { return string(get("hmbridge/bridge/availability")) == "online" })
	waitFor(t, "climate discovery", func() bool {
		v := jsonOf("homeassistant/climate/hmbridge_38f59c/config")
		return v["unique_id"] == "hmbridge_38f59c"
	})
	waitFor(t, "group discovery", func() bool { return len(get("homeassistant/climate/hmbridge_group_bad/config")) > 0 })

	// The radiator thermostat reports its status.
	central := []byte{0x1a, 0x2b, 0x3c}
	info := append([]byte{0x05, 0xa6, 0x10, 0x38, 0xf5, 0x9c}, central...)
	info = append(info, 0x0a, 0xb0, 0xe2, 0x0f, 0x25, 0x00) // 22.0 target, 22.6 actual, 3.0 V, valve 37 %
	if err := mod.InjectRecv(0x01, info); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "device state", func() bool {
		v := jsonOf("hmbridge/38f59c/state")
		return v["current_temperature"] == 22.6 && v["battery_voltage"] == 3.0 && v["battery_percent"] == 100.0
	})
	waitFor(t, "group state", func() bool { return jsonOf("hmbridge/group/bad/state")["current_temperature"] == 22.6 })

	// Home Assistant sets the target temperature of the group.
	if tok := ha.Publish("hmbridge/group/bad/set/temperature", 0, false, "21.5"); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	waitFor(t, "radio command", func() bool {
		for _, m := range mod.Sent() {
			if len(m) >= 12 && m[2] == 0x11 && bytes.Equal(m[9:12], []byte{0x86, 0x04, 43}) {
				return true
			}
		}
		return false
	})
	waitFor(t, "updated target", func() bool { return jsonOf("hmbridge/38f59c/state")["target_temperature"] == 21.5 })

	// Metrics and the API.
	metrics := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", httpPort))
	for _, want := range []string{
		`hmbridge_device_battery_volts{address="38f59c",model="HM-CC-RT-DN",name="Bad"} 3`,
		`hmbridge_uartgw_up 1`,
		`hmbridge_mqtt_connected 1`,
		`hmheating_InfoEventActualTemperature{address="38f59c",name="Bad"} 22.6`,
		`hmbridge_commands_total{action="set_temperature",result="ok"} `,
		`hmbridge_build_info{`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	var dev struct {
		Name  string         `json:"name"`
		State map[string]any `json:"state"`
	}
	if err := json.Unmarshal([]byte(httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/api/v1/devices/38f59c", httpPort))), &dev); err != nil ||
		dev.Name != "Bad" || dev.State["target_temperature"] != 21.5 {
		t.Errorf("/api/v1/devices/38f59c: %v %+v", err, dev)
	}
	var health struct {
		Status string `json:"status"`
		MQTT   struct {
			Connected bool `json:"connected"`
		} `json:"mqtt"`
	}
	if err := json.Unmarshal([]byte(httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", httpPort))), &health); err != nil ||
		health.Status != "ok" || !health.MQTT.Connected {
		t.Errorf("/api/v1/health: %v %+v", err, health)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Errorf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
	waitFor(t, "availability offline", func() bool { return string(get("hmbridge/bridge/availability")) == "offline" })

	// Restart: without a state file, the second run restores the state
	// from the broker's retained messages and republishes it before any
	// device has reported (restored state has no message counter).
	mu.Lock()
	delete(seen, "hmbridge/38f59c/state")
	mu.Unlock()
	mod2, rw2 := fakemod.New(fakemod.Options{})
	defer mod2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- run(ctx2, cfg, rw2) }()
	waitFor(t, "online again", func() bool { return string(get("hmbridge/bridge/availability")) == "online" })
	waitFor(t, "restored state republished", func() bool {
		v := jsonOf("hmbridge/38f59c/state")
		return v["target_temperature"] == 21.5 && v["battery_voltage"] == 3.0 && v["messages_received"] == nil
	})
	waitFor(t, "restored group state", func() bool { return jsonOf("hmbridge/group/bad/state")["current_temperature"] == 22.6 })
	if a := string(get("hmbridge/38f59c/availability")); a != "online" {
		t.Errorf("device availability after restart = %q, want online", a)
	}
	cancel2()
	<-done2
}

// freePort returns a port that was free a moment ago. It is chosen
// below Linux's ephemeral range (32768-60999): a port the kernel hands
// out (":0") may be reused as the local port of an outgoing connection
// (e.g. an MQTT client's) before the broker or HTTP server binds it.
func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 100; i++ {
		port := 20000 + rand.IntN(10000)
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			l.Close()
			return port
		}
	}
	t.Fatal("no free port")
	return 0
}

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	waitFor(t, "listener on "+addr, func() bool {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// An http_listen address that cannot be bound makes run fail instead of
// running without API and metrics.
func TestRunHTTPListenError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "central_address": "1A2B3C",
	  "reset": "none",
	  "state_file": "",
	  "require_synced_clock": false,
	  "mqtt": {"broker": ""},
	  "http_listen": %q,
	  "devices": [{"name": "Bad", "model": "HM-CC-RT-DN", "address": "38F59C"}]
	}`, l.Addr().String())))
	if err != nil {
		t.Fatal(err)
	}
	mod, rw := fakemod.New(fakemod.Options{})
	defer mod.Close()
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), cfg, rw) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "http_listen") {
			t.Errorf("run: %v, want an http_listen error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
}
