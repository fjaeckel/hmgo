// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/bridge"
	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

var e2eNewSCo = [3]byte{0x4c, 0x5d, 0x70}

// pairingE2EConfig keeps hmbridge's files in stateDir.
func pairingE2EConfig(t *testing.T, broker string, httpPort int, stateDir string) *config.Config {
	t.Helper()
	httpListen := `""`
	if httpPort != 0 {
		httpListen = fmt.Sprintf(`"127.0.0.1:%d"`, httpPort)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "central_address": "458086",
	  "reset": "none",
	  "state_dir": %q,
	  "require_synced_clock": false,
	  "security": {"passphrase": "geheim", "index": 2},
	  "mqtt": {"broker": %q},
	  "http_listen": %s
	}`, stateDir, broker, httpListen)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func (h *fakeHA) forget(topic string) {
	h.mu.Lock()
	delete(h.last, topic)
	h.mu.Unlock()
}

func (h *fakeHA) pairing() map[string]any { return h.json("hmbridge/bridge/pairing") }

func (h *fakeHA) lastResult() string {
	s, _ := h.pairing()["last_result"].(string)
	return s
}

// startPairing presses “Pair new device” in Home Assistant.
func (h *fakeHA) startPairing(t *testing.T) {
	t.Helper()
	waitFor(t, "pair button", func() bool {
		return h.json("homeassistant/button/hmbridge_bridge/pair/config")["command_topic"] == "hmbridge/bridge/set/pair"
	})
	h.publish("hmbridge/bridge/set/pair", "PRESS")
	waitFor(t, "pairing mode", func() bool { return h.pairing()["active"] == true })
}

func runCtx(t *testing.T, cfg *config.Config, rw interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, rw) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("run did not return")
		}
	}
	t.Cleanup(stop)
	return stop
}

// TestEndToEndPairing pairs a window contact with an emulated
// HM-MOD-RPI-PCB from Home Assistant, uses it, restarts hmbridge (the
// devices file is read again) and unpairs it.
func TestEndToEndPairing(t *testing.T) {
	broker := startBroker(t)
	httpPort := freePort(t)
	fastBridge(t)
	file := filepath.Join(t.TempDir(), "devices.json")
	cfg := pairingE2EConfig(t, broker, httpPort, filepath.Dir(file))
	ha := newFakeHA(t, broker, "fake-ha-pairing")
	key := hmkey.FromPassphrase("geheim", 2)
	dev := fakemod.NewDevice(e2eNewSCo, 0x00c7, "NEQ0000002")
	dev.ModuleKey = &key
	mod, rw := fakemod.New(fakemod.Options{OnSend: dev.HandleClassic})
	defer mod.Close()
	stop := runCtx(t, cfg, rw)

	ha.startPairing(t)
	// A device hmbridge does not support is rejected; pairing mode stays on.
	unknown := fakemod.NewDevice([3]byte{0x11, 0x22, 0x33}, 0x1234, "XEQ0000001")
	mod.InjectRecv(0x01, unknown.DeviceInfo(0x01, [3]byte{}))
	waitFor(t, "unsupported model", func() bool { return strings.Contains(ha.lastResult(), "model ID 1234") })
	if ha.pairing()["active"] != true {
		t.Fatal("pairing mode ended")
	}

	// The contact's config button is pressed.
	mod.InjectRecv(0x01, dev.DeviceInfo(0x02, [3]byte{}))
	waitFor(t, "paired", func() bool { return strings.HasPrefix(ha.lastResult(), "paired HM-Sec-SCo NEQ0000002 (4c5d70") })
	if c := dev.Central(); c != [3]byte(e2eCentral) {
		t.Errorf("device central %X", c)
	}
	if k := dev.Key(); k != key {
		t.Errorf("device key index %d, not the system key", k.Index)
	}
	if v, _ := dev.Reg(1, 1, 0x08); v != 1 {
		t.Errorf("AES_ACTIVE = %d", v)
	}
	waitFor(t, "discovery", func() bool {
		return ha.json("homeassistant/binary_sensor/hmbridge_4c5d70/window_open/config")["state_topic"] == "hmbridge/4c5d70/state" &&
			ha.json("homeassistant/button/hmbridge_4c5d70/unpair/config")["enabled_by_default"] == false
	})
	// The device is usable: an authenticated window event.
	mod.InjectRecv(0x02, dev.Message(0x20, 0xa4, 0x41, [3]byte(e2eCentral), 0x01, 0x05, 0xc8))
	waitFor(t, "window open", func() bool { return ha.json("hmbridge/4c5d70/state")["window_open"] == true })

	metrics := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", httpPort))
	for _, w := range []string{`hmbridge_pairing_attempts_total{result="paired"} `, `hmbridge_pairing_paired_devices 1`, `hmbridge_pairing_active 0`} {
		if !strings.Contains(metrics, w) {
			t.Errorf("/metrics lacks %q", w)
		}
	}

	// Restart: the paired device is back, registered with the module
	// with the system key and AES.
	stop()
	ha.forget("homeassistant/button/hmbridge_bridge/pair/config")
	mod2, rw2 := fakemod.New(fakemod.Options{OnSend: dev.HandleClassic})
	defer mod2.Close()
	runCtx(t, cfg, rw2)
	// Discovery is published once hmbridge subscribed to its commands.
	waitFor(t, "republished discovery", func() bool {
		return ha.json("homeassistant/button/hmbridge_bridge/pair/config")["command_topic"] != nil
	})
	waitFor(t, "peer after restart", func() bool {
		p, ok := mod2.Peers()[e2eNewSCo]
		return ok && p.KeyIndex == 2 && fmt.Sprint(p.AESChannels(2)) == "[1]"
	})
	var st bridge.Snapshot
	waitFor(t, "status API", func() bool {
		return json.Unmarshal([]byte(httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/api/state", httpPort))), &st) == nil && len(st.Devices) == 1
	})
	if d := st.Devices[0]; d.Name != "HM-Sec-SCo NEQ0000002" || !d.Paired || !d.AES {
		t.Errorf("device after restart %+v", d)
	}

	// Unpair from Home Assistant: the contact only listens after its
	// config button.
	ha.publish("hmbridge/4c5d70/set/unpair", "PRESS")
	waitFor(t, "removed", func() bool {
		return strings.Contains(ha.lastResult(), "press its config button") && ha.lastRaw("homeassistant/binary_sensor/hmbridge_4c5d70/window_open/config") == ""
	})
	mod2.InjectRecv(0x01, dev.DeviceInfo(0x03, [3]byte(e2eCentral)))
	waitFor(t, "unpaired", func() bool { return strings.HasPrefix(ha.lastResult(), "unpaired HM-Sec-SCo NEQ0000002") })
	if c := dev.Central(); c != ([3]byte{}) {
		t.Errorf("device still paired to %X", c)
	}
	if _, ok := mod2.Peers()[e2eNewSCo]; ok {
		t.Error("peer still registered")
	}
}

// TestEndToEndPairingRPIRFMod pairs a contact with an emulated
// RPI-RF-MOD: hmbridge encrypts the key exchange itself and answers the
// device's AES challenges in software; afterwards the contact's events
// are authenticated with the system key.
func TestEndToEndPairingRPIRFMod(t *testing.T) {
	broker := startBroker(t)
	fastBridge(t)
	cfg := pairingE2EConfig(t, broker, 0, t.TempDir())
	ha := newFakeHA(t, broker, "fake-ha-pairing-dual")
	dev := fakemod.NewDevice(e2eNewSCo, 0x00c7, "NEQ0000002")
	mod, rw := fakemod.New(fakemod.Options{DualCoPro: true, OnLLMACSend: dev.Handle})
	defer mod.Close()
	runCtx(t, cfg, rw)

	ha.startPairing(t)
	if err := mod.InjectLLMACRecv(0x40, dev.DeviceInfo(0x02, [3]byte{})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "paired", func() bool { return strings.HasPrefix(ha.lastResult(), "paired HM-Sec-SCo NEQ0000002") })
	if !strings.Contains(ha.lastResult(), "AES on channels [1] with key index 2") {
		t.Errorf("result %q", ha.lastResult())
	}
	if k := dev.Key(); k != hmkey.FromPassphrase("geheim", 2) {
		t.Fatalf("device key index %d, not the system key", k.Index)
	}
	if v, _ := dev.Reg(1, 1, 0x08); v != 1 || dev.Central() != [3]byte(e2eCentral) {
		t.Errorf("AES_ACTIVE %d, central %X", v, dev.Central())
	}
	// The key exchange went out encrypted (16 bytes), never as plain key.
	n := sentMatching(mod, func(m []byte) bool { return m[2] == 0x04 && len(m) == 9+16 })
	if n != 2 {
		t.Errorf("%d encrypted key exchange messages", n)
	}

	// A BiDi window event is challenged with the new key and accepted.
	mod.InjectLLMACRecv(0x40, dev.Message(0x21, 0xa0, 0x41, [3]byte(e2eCentral), 0x01, 0x06, 0xc8))
	waitFor(t, "authenticated event", func() bool {
		st := ha.json("hmbridge/4c5d70/state")
		return st["window_open"] == true && st["auth_status"] == "ok"
	})
}

// TestEndToEndPairingTimeout: pairing mode ends by itself.
func TestEndToEndPairingTimeout(t *testing.T) {
	broker := startBroker(t)
	testBridgeOptions = func(o *bridge.Options) {
		o.PairingDuration = 300 * time.Millisecond
		o.PairingTick = 20 * time.Millisecond
	}
	t.Cleanup(func() { testBridgeOptions = nil })
	cfg := pairingE2EConfig(t, broker, 0, t.TempDir())
	ha := newFakeHA(t, broker, "fake-ha-pairing-timeout")
	mod, rw := fakemod.New(fakemod.Options{})
	defer mod.Close()
	runCtx(t, cfg, rw)
	ha.startPairing(t)
	waitFor(t, "timeout", func() bool {
		return ha.pairing()["active"] == false && strings.Contains(ha.lastResult(), "no device announced itself")
	})
}
