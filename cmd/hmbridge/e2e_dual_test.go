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
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

func startBroker(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("mosquitto")
	if err != nil {
		if _, err2 := os.Stat("/usr/sbin/mosquitto"); err2 != nil {
			t.Skip("mosquitto not installed")
		}
		bin = "/usr/sbin/mosquitto"
	}
	port := freePort(t)
	conf := filepath.Join(t.TempDir(), "mosquitto.conf")
	os.WriteFile(conf, []byte(fmt.Sprintf("listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n", port)), 0o644)
	cmd := exec.Command(bin, "-c", conf)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))
	return fmt.Sprintf("tcp://127.0.0.1:%d", port)
}

// TestEndToEndRPIRFMod runs hmbridge against an emulated RPI-RF-MOD
// (eQ-3 dual firmware): auto-detection, a device report reaching Home
// Assistant, a software ACK for it, and a command from Home Assistant
// going out as LLMAC_SEND.
func TestEndToEndRPIRFMod(t *testing.T) {
	broker := startBroker(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "central_address": "458086",
	  "reset": "none",
	  "state_file": "",
	  "require_synced_clock": false,
	  "mqtt": {"broker": %q},
	  "http_listen": "",
	  "devices": [{"name": "Bad", "model": "HM-CC-RT-DN", "address": "4DA5A9"}]
	}`, broker)))
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[string][]byte{}
	ha := mqtt.NewClient(mqtt.NewClientOptions().AddBroker(broker).SetClientID("fake-ha-dual"))
	if tok := ha.Connect(); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer ha.Disconnect(100)
	ha.Subscribe("hmbridge/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		mu.Lock()
		seen[m.Topic()] = append([]byte(nil), m.Payload()...)
		mu.Unlock()
	}).Wait()
	state := func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var v map[string]any
		json.Unmarshal(seen["hmbridge/4da5a9/state"], &v)
		return v
	}

	mod, rw := fakemod.New(fakemod.Options{DualCoPro: true})
	defer mod.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, rw) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("run did not return")
		}
	}()

	waitFor(t, "module detected and bridge online", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return string(seen["hmbridge/bridge/availability"]) == "online"
	})

	central := []byte{0x45, 0x80, 0x86}
	rtdn := []byte{0x4D, 0xA5, 0xA9}
	// INFO_LEVEL with the BiDi flag (0xA4 = RPTEN|BIDI|BCAST):
	// target 22.0, actual 22.6, battery 2.3 V, valve 37 %.
	info := append([]byte{0x31, 0xA4, 0x10}, rtdn...)
	info = append(info, central...)
	info = append(info, 0x0A, 0xB0, 0xE2, 0x08, 0x25, 0x00)
	if err := mod.InjectLLMACRecv(0x41, info); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "state in Home Assistant", func() bool {
		v := state()
		return v["current_temperature"] == 22.6 && v["valve_position"] == 37.0
	})
	// The bridge acknowledged the message in software: 02 00 to the
	// device, same message counter.
	waitFor(t, "software ACK", func() bool {
		for _, m := range mod.Sent() {
			if len(m) >= 10 && m[0] == 0x31 && m[2] == 0x02 && bytes.Equal(m[6:9], rtdn) && m[9] == 0x00 {
				return true
			}
		}
		return false
	})

	ha.Publish("hmbridge/4da5a9/set/temperature", 0, false, "21.5").Wait()
	waitFor(t, "LLMAC_SEND of the set-temperature command", func() bool {
		for _, m := range mod.Sent() {
			if len(m) >= 12 && m[2] == 0x11 && bytes.Equal(m[3:6], central) && bytes.Equal(m[6:9], rtdn) && bytes.Equal(m[9:12], []byte{0x86, 0x04, 43}) {
				return true
			}
		}
		return false
	})
	waitFor(t, "acknowledged command in state", func() bool {
		return state()["target_temperature"] == 21.5 && state()["last_command"] == "ok: set_temperature 21.5"
	})
}
