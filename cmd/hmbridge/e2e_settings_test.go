// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/hm/params"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

var (
	e2eRT  = [3]byte{0x38, 0xf5, 0x9c}
	e2eSCo = [3]byte{0x4c, 0x5d, 0x6e}
)

func (h *fakeHA) settingsOf(addr string) (current map[string]any, detail map[string]any, status string) {
	v := h.json("hmbridge/" + addr + "/settings")
	current, _ = v["current"].(map[string]any)
	detail, _ = v["detail"].(map[string]any)
	status, _ = v["status"].(string)
	return
}

// configSent returns "flags payload" of the CONFIG messages sent to dst
// from index from on.
func configSent(mod *fakemod.Module, dst [3]byte, from int) []string {
	var res []string
	for i, m := range mod.Sent() {
		if i >= from && len(m) >= 10 && m[2] == 0x01 && bytes.Equal(m[6:9], dst[:]) {
			res = append(res, fmt.Sprintf("%02x %x", m[1], m[9:]))
		}
	}
	return res
}

// TestEndToEndSettings reads and changes device settings from Home
// Assistant through the broker: a radiator thermostat (wake-up burst,
// read-back), an out-of-range value, and a window contact whose change
// waits for its config button.
func TestEndToEndSettings(t *testing.T) {
	broker := startBroker(t)
	fastBridge(t)
	stateDir := t.TempDir()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "central_address": "458086",
	  "reset": "none",
	  "state_dir": %q,
	  "require_synced_clock": false,
	  "security": {"passphrase": "geheim", "index": 2},
	  "mqtt": {"broker": %q},
	  "http_listen": "",
	  "devices": [
	    {"name": "Bad", "model": "HM-CC-RT-DN", "address": "38F59C", "aes": true},
	    {"name": "Bad Fenster", "model": "HM-Sec-SCo", "address": "4C5D6E", "aes": true}
	  ]
	}`, stateDir, broker)))
	if err != nil {
		t.Fatal(err)
	}
	ha := newFakeHA(t, broker, "fake-ha-settings")

	rt := fakemod.NewDevice(e2eRT, 0x0095, "MEQ0000001")
	for reg, v := range map[byte]byte{1: 1, 14: 10, 15: 0, 18: 21} {
		rt.SetReg(0, 0, reg, v)
	}
	for reg, v := range map[byte]byte{1: 42, 2: 34, 3: 9, 4: 61, 5: 24, 6: 3, 7: 0, 8: 22, 9: 7, 10: 0x30, 11: 0, 12: 100, 13: 15, 14: 5, 19: 0x8e} {
		rt.SetReg(0, 7, reg, v)
	}
	day, _ := params.ParseDay("06:00 17, 22:00 21, 24:00 17")
	for d := 0; d < 7; d++ {
		for a, v := range params.EncodeDay(day, params.ProfileBase, d) {
			rt.SetReg(0, 7, a, v)
		}
	}
	sco := fakemod.NewDevice(e2eSCo, 0x00c7, "NEQ0000001")
	sco.SetReg(1, 1, 33, 0)
	sco.SetAsleep(true)
	onSend := func(m []byte) (byte, []byte) {
		if len(m) >= 9 && [3]byte(m[6:9]) == e2eSCo {
			return sco.HandleClassic(m)
		}
		return rt.HandleClassic(m)
	}
	mod, rw := fakemod.New(fakemod.Options{OnSend: onSend, ChallengeAES: true})
	defer mod.Close()
	for _, d := range []*fakemod.Device{rt, sco} {
		d.SetInject(func(m []byte) { mod.InjectRecv(0x01, m) })
	}
	runCtx(t, cfg, rw)

	waitFor(t, "settings discovery", func() bool {
		v := ha.json("homeassistant/number/hmbridge_38f59c/setting_comfort_temperature/config")
		return v["command_topic"] == "hmbridge/38f59c/set/setting_comfort_temperature" && v["min"] == 15.0 && v["max"] == 30.0 &&
			ha.json("homeassistant/button/hmbridge_38f59c/settings_refresh/config")["command_topic"] == "hmbridge/38f59c/set/settings_refresh"
	})

	// Read the settings.
	ha.publish("hmbridge/38f59c/set/settings_refresh", "PRESS")
	waitFor(t, "settings read", func() bool {
		cur, _, st := ha.settingsOf("38f59c")
		return st == "ok" && cur["comfort_temperature"] == 21.0 && cur["week_profile_sunday"] == "06:00 17.0, 22:00 21.0, 24:00 17.0"
	})

	// Change the comfort temperature (bits 0-5 of register 1): list 7 is
	// read right before the write (the other bits), written with a burst,
	// read back.
	start := len(mod.Sent())
	ha.publish("hmbridge/38f59c/set/setting_comfort_temperature", "22.5")
	waitFor(t, "written", func() bool {
		cur, det, _ := ha.settingsOf("38f59c")
		return det["comfort_temperature"] == "ok" && cur["comfort_temperature"] == 22.5
	})
	if got, want := configSent(mod, e2eRT, start), "[b0 00040000000007 b0 00050000000007 b0 0008012d b0 0006 b0 00040000000007]"; fmt.Sprint(got) != want {
		t.Errorf("frames %v, want %s", got, want)
	}
	if v, _ := rt.Reg(0, 7, 1); v != 45 {
		t.Errorf("TEMPERATURE_COMFORT register = %d, want 45", v)
	}

	// Out of range: rejected without radio traffic.
	start = len(mod.Sent())
	ha.publish("hmbridge/38f59c/set/setting_comfort_temperature", "35")
	waitFor(t, "rejected", func() bool {
		_, det, st := ha.settingsOf("38f59c")
		s, _ := det["comfort_temperature"].(string)
		return strings.Contains(s, "out of range") && strings.HasPrefix(st, "failed")
	})
	time.Sleep(100 * time.Millisecond)
	if got := configSent(mod, e2eRT, start); len(got) != 0 {
		t.Errorf("sent %v", got)
	}

	// The contact sleeps: the change waits for its config button.
	start = len(mod.Sent())
	ha.publish("hmbridge/4c5d6e/set/setting_event_delay", "60")
	waitFor(t, "pending", func() bool {
		_, det, st := ha.settingsOf("4c5d6e")
		return det["event_delay"] == "pending" && strings.HasPrefix(st, "pending")
	})
	time.Sleep(100 * time.Millisecond)
	if got := configSent(mod, e2eSCo, start); len(got) != 0 {
		t.Fatalf("sent to the sleeping contact: %v", got)
	}
	sco.SetAsleep(false)
	if err := mod.InjectRecv(0x01, sco.DeviceInfo(0x10, [3]byte(e2eCentral))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "contact written", func() bool {
		cur, det, st := ha.settingsOf("4c5d6e")
		return det["event_delay"] == "ok" && cur["event_delay"] == 60.0 && st == "ok"
	})
	if v, _ := sco.Reg(1, 1, 33); v != 60 {
		t.Errorf("EVENT_DELAYTIME register = %d", v)
	}

	// The cache is kept in the state directory.
	waitFor(t, "settings file", func() bool {
		b, err := os.ReadFile(filepath.Join(stateDir, "settings.json"))
		return err == nil && bytes.Contains(b, []byte(`"38f59c"`)) && bytes.Contains(b, []byte(`"4c5d6e"`))
	})
}
