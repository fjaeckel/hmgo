// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"testing"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/hm/params"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

var e2eTC = [3]byte{0x2a, 0x1b, 0x3c}

// thermostatList7 loads a thermostat's list 7 (eQ-3 defaults, the same
// week profile every day).
func thermostatList7(d *fakemod.Device) {
	for reg, v := range map[byte]byte{1: 42, 2: 34, 3: 9, 4: 61, 5: 24, 6: 3, 7: 0, 8: 22, 9: 7, 10: 0x30, 11: 0, 12: 100, 13: 15, 14: 5, 15: 0, 19: 0x8e} {
		d.SetReg(0, 7, reg, v)
	}
	day, _ := params.ParseDay("06:00 17, 22:00 21, 24:00 17")
	for n := 0; n < 7; n++ {
		for a, v := range params.EncodeDay(day, params.ProfileBase, n) {
			d.SetReg(0, 7, a, v)
		}
	}
}

func (h *fakeHA) groupSettings(slug string) (current, settings, detail map[string]any, consistency, status string) {
	v := h.json("hmbridge/group/" + slug + "/settings")
	current, _ = v["current"].(map[string]any)
	settings, _ = v["settings"].(map[string]any)
	detail, _ = v["detail"].(map[string]any)
	consistency, _ = v["consistency"].(string)
	status, _ = v["status"].(string)
	return
}

// firstConfigWrite returns the index of the first CONFIG_START (cmd
// 0x01, subtype 0x05) sent to dst from index from on (-1 if none).
func firstConfigWrite(mod *fakemod.Module, dst [3]byte, from int) int {
	for i, m := range mod.Sent() {
		if i >= from && len(m) >= 11 && m[2] == 0x01 && [3]byte(m[6:9]) == dst && m[10] == 0x05 {
			return i
		}
	}
	return -1
}

// TestEndToEndGroupSettings changes the settings of a heating group with
// a wall thermostat and a radiator thermostat from Home Assistant: a
// group change is written to (and verified on) both members, the wall
// thermostat first; a per-device change makes the group mixed.
func TestEndToEndGroupSettings(t *testing.T) {
	broker := startBroker(t)
	fastBridge(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
	  "central_address": "458086",
	  "reset": "none",
	  "state_dir": %q,
	  "require_synced_clock": false,
	  "security": {"passphrase": "geheim", "index": 2},
	  "mqtt": {"broker": %q},
	  "http_listen": "",
	  "devices": [
	    {"name": "Bad Heizung", "model": "HM-CC-RT-DN", "address": "38F59C", "aes": true},
	    {"name": "Bad Wand", "model": "HM-TC-IT-WM-W-EU", "address": "2A1B3C", "aes": true}
	  ],
	  "groups": [{"name": "Bad", "members": ["Bad Heizung", "Bad Wand"]}]
	}`, t.TempDir(), broker)))
	if err != nil {
		t.Fatal(err)
	}
	ha := newFakeHA(t, broker, "fake-ha-group-settings")

	rt := fakemod.NewDevice(e2eRT, 0x0095, "MEQ0000001")
	wall := fakemod.NewDevice(e2eTC, 0x00ad, "MEQ0000002")
	for _, d := range []*fakemod.Device{rt, wall} {
		for reg, v := range map[byte]byte{1: 1, 14: 10, 15: 0, 18: 21} {
			d.SetReg(0, 0, reg, v)
		}
		thermostatList7(d)
	}
	onSend := func(m []byte) (byte, []byte) {
		if len(m) >= 9 && [3]byte(m[6:9]) == e2eTC {
			return wall.HandleClassic(m)
		}
		return rt.HandleClassic(m)
	}
	mod, rw := fakemod.New(fakemod.Options{OnSend: onSend, ChallengeAES: true})
	defer mod.Close()
	for _, d := range []*fakemod.Device{rt, wall} {
		d.SetInject(func(m []byte) { mod.InjectRecv(0x01, m) })
	}
	runCtx(t, cfg, rw)

	waitFor(t, "group settings discovery", func() bool {
		v := ha.json("homeassistant/number/hmbridge_group_bad/setting_comfort_temperature/config")
		w := ha.json("homeassistant/number/hmbridge_group_bad/setting_window_open_temperature/config")
		return v["command_topic"] == "hmbridge/group/bad/set/setting_comfort_temperature" &&
			v["unique_id"] == "hmbridge_group_bad_setting_comfort_temperature" && v["entity_category"] == "config" &&
			w["name"] == "Window open temperature (radiator thermostats only)" &&
			ha.json("homeassistant/button/hmbridge_group_bad/settings_refresh/config")["command_topic"] == "hmbridge/group/bad/set/settings_refresh" &&
			ha.json("homeassistant/sensor/hmbridge_group_bad/settings_consistency/config")["state_topic"] == "hmbridge/group/bad/settings"
	})
	if _, _, _, cons, _ := ha.groupSettings("bad"); cons != "unknown" {
		t.Errorf("consistency before reading = %q", cons)
	}

	// Refresh both members from the group.
	ha.publish("hmbridge/group/bad/set/settings_refresh", "PRESS")
	waitFor(t, "group settings read", func() bool {
		cur, _, _, cons, _ := ha.groupSettings("bad")
		return cons == "consistent" && cur["comfort_temperature"] == 21.0 && cur["window_open_temperature"] == 12.0
	})

	// Comfort temperature on the group: both written and verified, the
	// wall thermostat first.
	start := len(mod.Sent())
	ha.publish("hmbridge/group/bad/set/setting_comfort_temperature", "22.5")
	waitFor(t, "comfort temperature written", func() bool {
		cur, _, det, cons, st := ha.groupSettings("bad")
		return st == "ok: comfort_temperature = 22.5 °C" && det["Bad Wand"] == "ok" && det["Bad Heizung"] == "ok" &&
			cons == "consistent" && cur["comfort_temperature"] == 22.5
	})
	for _, d := range []*fakemod.Device{rt, wall} {
		if v, _ := d.Reg(0, 7, 1); v != 45 {
			t.Errorf("%x: TEMPERATURE_COMFORT register = %d, want 45", d.Addr, v)
		}
	}
	if w, r := firstConfigWrite(mod, e2eTC, start), firstConfigWrite(mod, e2eRT, start); w < 0 || r < 0 || w > r {
		t.Errorf("first writes: wall thermostat at %d, radiator thermostat at %d", w, r)
	}
	for _, addr := range []string{"38f59c", "2a1b3c"} {
		if cur, det, _ := ha.settingsOf(addr); cur["comfort_temperature"] != 22.5 || det["comfort_temperature"] != "ok" {
			t.Errorf("%s: device settings %v %v", addr, cur, det)
		}
	}

	// A week profile day on the group: both members.
	const monday = "06:30 18.0, 21:00 20.5, 24:00 17.0"
	ha.publish("hmbridge/group/bad/set/setting_week_profile_monday", monday)
	waitFor(t, "week profile written", func() bool {
		cur, _, _, _, st := ha.groupSettings("bad")
		return st == "ok: week_profile_monday = "+monday && cur["week_profile_monday"] == monday
	})
	day, _ := params.ParseDay(monday)
	for _, d := range []*fakemod.Device{rt, wall} {
		for a, v := range params.EncodeDay(day, params.ProfileBase, 2) {
			if got, _ := d.Reg(0, 7, a); got != v {
				t.Errorf("%x: register %d = %02x, want %02x", d.Addr, a, got, v)
			}
		}
	}

	// The window open temperature goes to the radiator thermostat only.
	start = len(mod.Sent())
	ha.publish("hmbridge/group/bad/set/setting_window_open_temperature", "10")
	waitFor(t, "window open temperature written", func() bool {
		_, _, det, _, st := ha.groupSettings("bad")
		return st == "ok: window_open_temperature = 10 °C" && det["Bad Wand"] == "not applicable (no such setting)"
	})
	if w := firstConfigWrite(mod, e2eTC, start); w >= 0 {
		t.Errorf("window open temperature written to the wall thermostat")
	}

	// A change on one member's own entity: the group is mixed.
	ha.publish("hmbridge/38f59c/set/setting_eco_temperature", "16")
	waitFor(t, "mixed", func() bool {
		cur, set, _, cons, _ := ha.groupSettings("bad")
		eco, _ := set["eco_temperature"].(map[string]any)
		members, _ := eco["members"].(map[string]any)
		_, shown := cur["eco_temperature"]
		return cons == "mixed" && !shown && eco["consistency"] == "mixed" && members["Bad Heizung"] == 16.0 && members["Bad Wand"] == 17.0
	})
	if v := ha.json("hmbridge/group/bad/settings"); fmt.Sprint(v["mixed"]) != "[eco_temperature]" {
		t.Errorf("mixed = %v", v["mixed"])
	}
}
