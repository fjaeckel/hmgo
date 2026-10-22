// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// testGroup is a group with a wall thermostat, a radiator thermostat
// and an optical window contact.
func testGroup() GroupDesc {
	return GroupDesc{
		Slug: "wohnzimmer",
		Name: "Wohnzimmer",
		Members: []GroupMember{
			{Name: "WZ Wand", Model: "HM-TC-IT-WM-W-EU", Address: "2a1b3c", Role: RoleWall},
			{Name: "WZ Heizung", Model: "HM-CC-RT-DN", Address: "38f59c", Role: RoleRadiator},
			{Name: "WZ Fenster", Model: "HM-Sec-SCo", Address: "4c5d6e", Role: RoleWindow},
		},
		Climate:     "2a1b3c",
		SuggestArea: true,
	}
}

func byTopic(t *testing.T, msgs []Message) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, m := range msgs {
		if _, dup := out[m.Topic]; dup {
			t.Errorf("duplicate topic %s", m.Topic)
		}
		out[m.Topic] = decode(t, m)
	}
	return out
}

func TestGroupMemberDiscovery(t *testing.T) {
	cfg := TopicConfig{}
	g := testGroup()
	msgs := byTopic(t, GroupDiscoveryMessages(cfg, g))

	type want struct {
		name, stateTopic, field, deviceClass, unit string
		bridgeOnly                                 bool
	}
	members := map[string]want{
		"sensor/hmbridge_group_wohnzimmer/2a1b3c_temperature":      {"WZ Wand temperature", "hmbridge/2a1b3c/state", "current_temperature", "temperature", "°C", false},
		"sensor/hmbridge_group_wohnzimmer/2a1b3c_humidity":         {"WZ Wand humidity", "hmbridge/2a1b3c/state", "humidity", "humidity", "%", false},
		"binary_sensor/hmbridge_group_wohnzimmer/2a1b3c_available": {"WZ Wand available", "hmbridge/2a1b3c/availability", "", "connectivity", "", true},
		"sensor/hmbridge_group_wohnzimmer/38f59c_valve":            {"WZ Heizung valve", "hmbridge/38f59c/state", "valve_position", "", "%", false},
		"binary_sensor/hmbridge_group_wohnzimmer/38f59c_available": {"WZ Heizung available", "hmbridge/38f59c/availability", "", "connectivity", "", true},
		"binary_sensor/hmbridge_group_wohnzimmer/4c5d6e_window":    {"WZ Fenster window", "hmbridge/4c5d6e/state", "window_open", "window", "", false},
		"binary_sensor/hmbridge_group_wohnzimmer/4c5d6e_available": {"WZ Fenster available", "hmbridge/4c5d6e/availability", "", "connectivity", "", true},
	}
	// climate + group window + member entities, nothing else.
	if len(msgs) != 2+len(members) {
		t.Errorf("got %d messages, want %d", len(msgs), 2+len(members))
	}
	for path, w := range members {
		topic := "homeassistant/" + path + "/config"
		v, ok := msgs[topic]
		if !ok {
			t.Errorf("missing %s", topic)
			continue
		}
		parts := strings.Split(path, "/")
		uid := "hmbridge_group_wohnzimmer_" + parts[2]
		if v["unique_id"] != uid || v["default_entity_id"] != parts[0]+"."+uid {
			t.Errorf("%s: unique_id %v, default_entity_id %v", path, v["unique_id"], v["default_entity_id"])
		}
		if v["name"] != w.name || v["state_topic"] != w.stateTopic {
			t.Errorf("%s: name %v, state_topic %v", path, v["name"], v["state_topic"])
		}
		if w.field != "" && !strings.Contains(v["value_template"].(string), "value_json."+w.field) {
			t.Errorf("%s: value_template %v", path, v["value_template"])
		}
		if (w.deviceClass == "" && v["device_class"] != nil) || (w.deviceClass != "" && v["device_class"] != w.deviceClass) {
			t.Errorf("%s: device_class %v", path, v["device_class"])
		}
		if (w.unit == "" && v["unit_of_measurement"] != nil) || (w.unit != "" && v["unit_of_measurement"] != w.unit) {
			t.Errorf("%s: unit %v", path, v["unit_of_measurement"])
		}
		if v["state_class"] != nil {
			t.Errorf("%s: state_class %v (the member's own sensor records statistics)", path, v["state_class"])
		}
		// On the group's device.
		dev := v["device"].(map[string]any)
		if ids := dev["identifiers"].([]any); len(ids) != 1 || ids[0] != "hmbridge_group_wohnzimmer" {
			t.Errorf("%s: device %v", path, dev)
		}
		if w.bridgeOnly {
			if v["availability_topic"] != "hmbridge/bridge/availability" || v["availability"] != nil ||
				v["payload_on"] != "online" || v["payload_off"] != "offline" || v["entity_category"] != "diagnostic" {
				t.Errorf("%s: %v", path, v)
			}
			continue
		}
		av, _ := v["availability"].([]any)
		addr := strings.SplitN(parts[2], "_", 2)[0]
		if len(av) != 2 || av[0].(map[string]any)["topic"] != "hmbridge/bridge/availability" ||
			av[1].(map[string]any)["topic"] != "hmbridge/"+addr+"/availability" || v["availability_mode"] != "all" {
			t.Errorf("%s: availability %v %v", path, v["availability"], v["availability_mode"])
		}
	}

	c := msgs["homeassistant/climate/hmbridge_group_wohnzimmer/config"]
	if c["json_attributes_topic"] != "hmbridge/group/wohnzimmer/members" {
		t.Errorf("climate json_attributes_topic = %v", c["json_attributes_topic"])
	}
	if dev := c["device"].(map[string]any); dev["suggested_area"] != "Wohnzimmer" {
		t.Errorf("group device = %v", dev)
	}
	g.SuggestArea = false
	c = byTopic(t, GroupDiscoveryMessages(cfg, g))["homeassistant/climate/hmbridge_group_wohnzimmer/config"]
	if _, ok := c["device"].(map[string]any)["suggested_area"]; ok {
		t.Error("suggested_area sent without SuggestArea")
	}

	// Member devices get the suggested area through DeviceDesc.
	for _, m := range DiscoveryMessages(cfg, DeviceDesc{Addr: "38f59c", Name: "WZ Heizung", Model: "HM-CC-RT-DN", Kind: RadiatorThermostat, SuggestedArea: "Wohnzimmer"}) {
		if dev := decode(t, m)["device"].(map[string]any); dev["suggested_area"] != "Wohnzimmer" {
			t.Fatalf("%s: device %v", m.Topic, dev)
		}
	}
	if dev := decode(t, DiscoveryMessages(cfg, DeviceDesc{Addr: "38f59c", Model: "HM-CC-RT-DN", Kind: RadiatorThermostat})[0])["device"].(map[string]any); dev["suggested_area"] != nil {
		t.Errorf("device without area: %v", dev)
	}
}

func TestGroupMembersMessage(t *testing.T) {
	cfg := TopicConfig{}
	m := GroupMembersMessage(cfg, testGroup())
	if m.Topic != "hmbridge/group/wohnzimmer/members" || !m.Retain || m.QoS != 1 {
		t.Errorf("message = %+v", m)
	}
	want := `{"members":[` +
		`{"name":"WZ Wand","model":"HM-TC-IT-WM-W-EU","address":"2a1b3c","role":"wall"},` +
		`{"name":"WZ Heizung","model":"HM-CC-RT-DN","address":"38f59c","role":"radiator"},` +
		`{"name":"WZ Fenster","model":"HM-Sec-SCo","address":"4c5d6e","role":"window"}],` +
		`"temperature_source":{"name":"WZ Wand","address":"2a1b3c"},` +
		`"humidity_source":{"name":"WZ Wand","address":"2a1b3c"}}`
	if string(m.Payload) != want {
		t.Errorf("payload = %s\nwant      %s", m.Payload, want)
	}
	// A radiator thermostat provides no humidity.
	g := GroupDesc{Slug: "bad", Members: []GroupMember{{Name: "Bad", Model: "HM-CC-RT-DN", Address: "38f59c", Role: RoleRadiator}}, Climate: "38F59C"}
	var a GroupAttributes
	if err := json.Unmarshal(GroupMembersMessage(cfg, g).Payload, &a); err != nil ||
		a.TemperatureSource == nil || a.TemperatureSource.Address != "38f59c" || a.HumiditySource != nil {
		t.Errorf("attributes = %+v, %v", a, err)
	}
	if slug, ok := cfg.ParseGroupMembersTopic(m.Topic); !ok || slug != "wohnzimmer" {
		t.Errorf("ParseGroupMembersTopic = %q, %v", slug, ok)
	}
	for _, bad := range []string{"hmbridge/group/wohnzimmer/state", "hmbridge/group/a/b/members", "hmbridge/group//members", "other/group/x/members"} {
		if _, ok := cfg.ParseGroupMembersTopic(bad); ok {
			t.Errorf("ParseGroupMembersTopic(%s) accepted", bad)
		}
	}
	if subs := cfg.RetainedSubscriptions(); !reflect.DeepEqual(subs, []string{"hmbridge/+/state", "hmbridge/group/+/state", "hmbridge/group/+/members", "hmbridge/group/+/settings"}) {
		t.Errorf("RetainedSubscriptions = %v", subs)
	}
}

func TestStaleGroupMessages(t *testing.T) {
	cfg := TopicConfig{}
	old := testGroup()
	oldMembers := GroupMembersMessage(cfg, old)
	retainedMsgs := map[string][]byte{
		oldMembers.Topic:                    oldMembers.Payload,
		"hmbridge/group/wohnzimmer/state":   []byte(`{"window_open":false}`),
		"hmbridge/38f59c/state":             []byte(`{}`),
		"hmbridge/group/kueche/state":       []byte(`{}`), // group from before member lists
		"hmbridge/group/bad/members":        []byte(`{"members":[{"address":"aabbcc","role":"radiator"},{"address":"../x","role":"window"}]}`),
		"hmbridge/group/bad/state":          []byte(`{}`),
		"hmbridge/group/schlafen/members":   []byte(`not json`),
		"hmbridge/group/leer/members":       nil,
		"hmbridge/group/Bad Zimmer/members": []byte(`{}`),
	}

	// Unchanged configuration: nothing to remove.
	if got := StaleGroupMessages(cfg, []GroupDesc{old, {Slug: "bad", Name: "Bad", Members: []GroupMember{{Address: "aabbcc", Role: RoleRadiator}}}, {Slug: "kueche"}, {Slug: "schlafen"}},
		map[string][]byte{oldMembers.Topic: oldMembers.Payload, "hmbridge/group/bad/members": retainedMsgs["hmbridge/group/bad/members"]}); len(got) != 0 {
		t.Errorf("unchanged groups: %v", topicsOf(got))
	}

	// The window contact leaves "wohnzimmer", the radiator thermostat's
	// model is corrected to a wall thermostat; "bad", "kueche" and
	// "schlafen" are removed.
	now := testGroup()
	now.Members = []GroupMember{
		{Name: "WZ Wand", Model: "HM-TC-IT-WM-W-EU", Address: "2a1b3c", Role: RoleWall},
		{Name: "WZ Heizung", Model: "HM-TC-IT-WM-W-EU", Address: "38f59c", Role: RoleWall},
	}
	msgs := StaleGroupMessages(cfg, []GroupDesc{now}, retainedMsgs)
	got := topicsOf(msgs)
	live := topicsOf(append(GroupDiscoveryMessages(cfg, now), GroupMembersMessage(cfg, now)))
	for _, m := range msgs {
		if len(m.Payload) != 0 || !m.Retain {
			t.Errorf("%s: not an empty retained message", m.Topic)
		}
		if _, ok := live[m.Topic]; ok {
			t.Errorf("removes live topic %s", m.Topic)
		}
		if strings.Contains(m.Topic, "..") || strings.Contains(m.Topic, "Bad Zimmer") || strings.Contains(m.Topic, "leer") {
			t.Errorf("unexpected %s", m.Topic)
		}
	}
	for _, w := range []string{
		"homeassistant/binary_sensor/hmbridge_group_wohnzimmer/4c5d6e_window/config",
		"homeassistant/binary_sensor/hmbridge_group_wohnzimmer/4c5d6e_available/config",
		"homeassistant/sensor/hmbridge_group_wohnzimmer/38f59c_valve/config",
		"homeassistant/climate/hmbridge_group_bad/config",
		"homeassistant/binary_sensor/hmbridge_group_bad/window_open/config",
		"homeassistant/sensor/hmbridge_group_bad/aabbcc_valve/config",
		"homeassistant/binary_sensor/hmbridge_group_bad/aabbcc_available/config",
		"hmbridge/group/bad/members",
		"hmbridge/group/bad/state",
		"hmbridge/group/bad/availability",
		"homeassistant/climate/hmbridge_group_kueche/config",
		"hmbridge/group/kueche/state",
		"homeassistant/climate/hmbridge_group_schlafen/config",
		"hmbridge/group/schlafen/members",
	} {
		if _, ok := got[w]; !ok {
			t.Errorf("missing removal of %s", w)
		}
	}
	if _, ok := got["homeassistant/binary_sensor/hmbridge_group_wohnzimmer/38f59c_available/config"]; ok {
		t.Error("removes the connectivity sensor of a remaining member")
	}
	// Discovery configs are removed before the state topics.
	seenState := false
	for _, m := range msgs {
		isState := strings.HasPrefix(m.Topic, "hmbridge/")
		if seenState && !isState {
			t.Errorf("%s removed after a state topic", m.Topic)
		}
		seenState = seenState || isState
	}

	// GroupRemovalMessages covers everything GroupDiscoveryMessages and
	// GroupMembersMessage publish.
	rm := topicsOf(GroupRemovalMessages(cfg, old.Slug, old.Members))
	for _, m := range append(GroupDiscoveryMessages(cfg, old), GroupMembersMessage(cfg, old)) {
		if _, ok := rm[m.Topic]; !ok {
			t.Errorf("GroupRemovalMessages misses %s", m.Topic)
		}
	}
}
