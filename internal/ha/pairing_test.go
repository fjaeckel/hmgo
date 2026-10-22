// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParsePairingCommands(t *testing.T) {
	cfg := TopicConfig{}
	for _, tc := range []struct {
		topic, payload string
		want           Command
		err            string
	}{
		{"hmbridge/bridge/set/pair", "PRESS", Command{Kind: StartPairing}, ""},
		{"hmbridge/bridge/set/pair", "", Command{Kind: StartPairing}, ""},
		{"hmbridge/bridge/set/pair", "120", Command{Kind: StartPairing, Duration: 2 * time.Minute}, ""},
		{"hmbridge/bridge/set/pair", "5", Command{}, "out of range"},
		{"hmbridge/bridge/set/pair", "soon", Command{}, "invalid pairing payload"},
		{"hmbridge/bridge/set/reboot", "PRESS", Command{}, "unknown bridge command"},
		{"hmbridge/4c5d6e/set/unpair", "PRESS", Command{Addr: "4c5d6e", Kind: Unpair}, ""},
		{"hmbridge/4C5D6E/set/name", " Küche Fenster ", Command{Addr: "4c5d6e", Kind: Rename, Name: "Küche Fenster"}, ""},
		{"hmbridge/4c5d6e/set/name", "", Command{}, "1 … 64"},
		{"hmbridge/4c5d6e/set/name", strings.Repeat("x", 65), Command{}, "1 … 64"},
		{"hmbridge/4c5d6e/set/name", "a\nb", Command{}, "control characters"},
		{"hmbridge/group/bad/set/unpair", "PRESS", Command{}, "not supported for groups"},
	} {
		got, err := ParseCommand(cfg, tc.topic, []byte(tc.payload))
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s %q: err %v, want %q", tc.topic, tc.payload, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s %q: got %+v, %v; want %+v", tc.topic, tc.payload, got, err, tc.want)
		}
	}
	// The bridge command topic is covered by the device command
	// subscription.
	if sub := cfg.CommandSubscription(); sub != "hmbridge/+/set/+" || cfg.PairCommandTopic() != "hmbridge/bridge/set/pair" {
		t.Errorf("subscription %q, pair topic %q", sub, cfg.PairCommandTopic())
	}
}

func TestPairedDeviceEntities(t *testing.T) {
	d := DeviceDesc{Addr: "4c5d6e", Name: "Fenster", Model: "HM-Sec-SCo", Kind: WindowContact, Paired: true}
	got := discovered(t, d)
	unpair := got["button/hmbridge_4c5d6e/unpair"]
	if unpair == nil || unpair["command_topic"] != "hmbridge/4c5d6e/set/unpair" || unpair["enabled_by_default"] != false ||
		unpair["entity_category"] != "diagnostic" || unpair["state_topic"] != nil || unpair["availability_topic"] != "hmbridge/bridge/availability" {
		t.Errorf("unpair button %v", unpair)
	}
	name := got["text/hmbridge_4c5d6e/name"]
	if name == nil || name["command_topic"] != "hmbridge/4c5d6e/set/name" || name["state_topic"] != "hmbridge/4c5d6e/name" || name["max"] != 64.0 {
		t.Errorf("name text %v", name)
	}
	// Configured devices have neither; their stale messages remove them.
	d.Paired = false
	got = discovered(t, d)
	if got["button/hmbridge_4c5d6e/unpair"] != nil || got["text/hmbridge_4c5d6e/name"] != nil {
		t.Error("configured device has pairing entities")
	}
	removed := map[string]bool{}
	for _, m := range StaleDiscoveryMessages(TopicConfig{}, d) {
		removed[m.Topic] = len(m.Payload) == 0
	}
	if !removed["homeassistant/button/hmbridge_4c5d6e/unpair/config"] || !removed["homeassistant/text/hmbridge_4c5d6e/name/config"] {
		t.Error("stale pairing entities not removed")
	}
	all := map[string]bool{}
	for _, m := range RemovalMessages(TopicConfig{}, "4c5d6e") {
		all[m.Topic] = true
	}
	for _, topic := range []string{"homeassistant/button/hmbridge_4c5d6e/unpair/config", "hmbridge/4c5d6e/name", "hmbridge/4c5d6e/state"} {
		if !all[topic] {
			t.Errorf("RemovalMessages lacks %s", topic)
		}
	}
	if m := NameMessage(TopicConfig{}, "4C5D6E", "Fenster"); m.Topic != "hmbridge/4c5d6e/name" || !m.Retain || string(m.Payload) != "Fenster" {
		t.Errorf("NameMessage %+v", m)
	}
}

func TestBridgeDiscovery(t *testing.T) {
	msgs := BridgeDiscoveryMessages(TopicConfig{}, "v1.2", true)
	got := map[string]map[string]any{}
	for _, m := range msgs {
		got[m.Topic] = decode(t, m)
	}
	button := got["homeassistant/button/hmbridge_bridge/pair/config"]
	if button == nil || button["command_topic"] != "hmbridge/bridge/set/pair" || button["payload_press"] != "PRESS" || button["state_topic"] != nil {
		t.Errorf("pair button %v", button)
	}
	dev, _ := button["device"].(map[string]any)
	if dev["sw_version"] != "v1.2" || dev["name"] != "hmbridge" {
		t.Errorf("bridge device %v", dev)
	}
	for _, key := range []string{"binary_sensor/hmbridge_bridge/pairing_active", "sensor/hmbridge_bridge/pairing_remaining", "sensor/hmbridge_bridge/pairing_status", "sensor/hmbridge_bridge/pairing_last_result"} {
		e := got["homeassistant/"+key+"/config"]
		if e == nil || e["state_topic"] != "hmbridge/bridge/pairing" || e["availability_topic"] != "hmbridge/bridge/availability" {
			t.Errorf("%s: %v", key, e)
		}
	}
	for _, m := range BridgeDiscoveryMessages(TopicConfig{}, "", false) {
		if m.Topic == "homeassistant/button/hmbridge_bridge/pair/config" && len(m.Payload) != 0 {
			t.Error("pair button published with pairing disabled")
		}
	}

	m := PairingStateMessage(TopicConfig{}, PairingState{Status: PairingActive, Active: true, Remaining: 42, LastResult: "paired x"})
	var v map[string]any
	json.Unmarshal(m.Payload, &v)
	if m.Topic != "hmbridge/bridge/pairing" || !m.Retain || v["remaining"] != 42.0 || v["status"] != "active" || v["active"] != true {
		t.Errorf("pairing state %s %s", m.Topic, m.Payload)
	}
}
