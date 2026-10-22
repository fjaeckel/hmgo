// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/json"
	"strings"
	"testing"
)

var testGroupSettings = []SettingDesc{
	{Key: "comfort_temperature", Name: "Comfort temperature", Component: SettingNumber, Min: 15, Max: 30, Step: 0.5, Unit: "°C", Enabled: true},
	{Key: "window_open_temperature", Name: "Window open temperature (radiator thermostats only)", Component: SettingNumber, Min: 5, Max: 30, Step: 0.5, Unit: "°C", Enabled: true},
	{Key: "week_profile_monday", Name: "Week profile Monday", Component: SettingText, MaxLength: 255, Enabled: true},
}

func TestGroupSettingsDiscovery(t *testing.T) {
	cfg := TopicConfig{}
	g := testGroup()
	g.Settings = testGroupSettings
	msgs := byTopic(t, GroupDiscoveryMessages(cfg, g))

	num := msgs["homeassistant/number/hmbridge_group_wohnzimmer/setting_comfort_temperature/config"]
	for k, want := range map[string]any{
		"unique_id":             "hmbridge_group_wohnzimmer_setting_comfort_temperature",
		"default_entity_id":     "number.hmbridge_group_wohnzimmer_setting_comfort_temperature",
		"entity_category":       "config",
		"state_topic":           "hmbridge/group/wohnzimmer/settings",
		"json_attributes_topic": "hmbridge/group/wohnzimmer/settings",
		"command_topic":         "hmbridge/group/wohnzimmer/set/setting_comfort_temperature",
		"availability_topic":    "hmbridge/bridge/availability",
		"min":                   15.0,
		"max":                   30.0,
	} {
		if num[k] != want {
			t.Errorf("number %s = %v, want %v", k, num[k], want)
		}
	}
	if tpl, _ := num["value_template"].(string); !strings.Contains(tpl, "'None'") {
		t.Errorf("number value_template %q does not render unknown", tpl)
	}
	if tpl, _ := num["json_attributes_template"].(string); !strings.Contains(tpl, "members") {
		t.Errorf("json_attributes_template = %q", tpl)
	}
	if dev := num["device"].(map[string]any); dev["identifiers"].([]any)[0] != "hmbridge_group_wohnzimmer" {
		t.Errorf("device = %v", dev)
	}
	if w := msgs["homeassistant/number/hmbridge_group_wohnzimmer/setting_window_open_temperature/config"]; w["name"] != "Window open temperature (radiator thermostats only)" {
		t.Errorf("window = %v", w)
	}
	txt := msgs["homeassistant/text/hmbridge_group_wohnzimmer/setting_week_profile_monday/config"]
	if tpl, _ := txt["value_template"].(string); txt["command_topic"] != "hmbridge/group/wohnzimmer/set/setting_week_profile_monday" || !strings.Contains(tpl, "'mixed'") {
		t.Errorf("text = %v", txt)
	}
	cons := msgs["homeassistant/sensor/hmbridge_group_wohnzimmer/settings_consistency/config"]
	if cons["entity_category"] != "diagnostic" || cons["state_topic"] != "hmbridge/group/wohnzimmer/settings" || cons["unique_id"] != "hmbridge_group_wohnzimmer_settings_consistency" {
		t.Errorf("consistency = %v", cons)
	}
	if st := msgs["homeassistant/sensor/hmbridge_group_wohnzimmer/settings_status/config"]; st["json_attributes_topic"] != "hmbridge/group/wohnzimmer/settings" {
		t.Errorf("status = %v", st)
	}
	if btn := msgs["homeassistant/button/hmbridge_group_wohnzimmer/settings_refresh/config"]; btn["command_topic"] != "hmbridge/group/wohnzimmer/set/settings_refresh" {
		t.Errorf("refresh = %v", btn)
	}

	// Without settings: none of these entities.
	for topic := range byTopic(t, GroupDiscoveryMessages(cfg, testGroup())) {
		if strings.Contains(topic, "setting") {
			t.Errorf("group without settings: %s", topic)
		}
	}

	// Removal covers everything, including the settings topic.
	rm := topicsOf(GroupRemovalMessages(cfg, g.Slug, g.Members, g.Settings...))
	for topic := range msgs {
		if _, ok := rm[topic]; !ok {
			t.Errorf("GroupRemovalMessages misses %s", topic)
		}
	}
	if _, ok := rm["hmbridge/group/wohnzimmer/settings"]; !ok {
		t.Error("GroupRemovalMessages misses the settings topic")
	}
	if slug, ok := cfg.ParseGroupSettingsTopic("hmbridge/group/wohnzimmer/settings"); !ok || slug != "wohnzimmer" {
		t.Errorf("ParseGroupSettingsTopic = %q, %v", slug, ok)
	}
}

func TestGroupSettingsMessage(t *testing.T) {
	m, err := GroupSettingsMessage(TopicConfig{}, "bad", GroupSettingsState{
		Current: map[string]any{"eco_temperature": 17.0},
		Settings: map[string]GroupSettingValue{
			"comfort_temperature": {Consistency: SettingsMixed, Members: map[string]any{"Wand": 21.0, "Heizung": 20.0}},
			"eco_temperature":     {Consistency: SettingsConsistent, Members: map[string]any{"Wand": 17.0, "Heizung": 17.0}},
		},
		Consistency: SettingsMixed,
		Mixed:       []string{"comfort_temperature"},
	})
	if err != nil || m.Topic != "hmbridge/group/bad/settings" || !m.Retain {
		t.Fatalf("%+v, %v", m, err)
	}
	var v map[string]any
	if err := json.Unmarshal(m.Payload, &v); err != nil {
		t.Fatal(err)
	}
	if v["consistency"] != "mixed" || v["status"] != "" || v["unknown"] == nil || v["detail"] == nil ||
		v["settings"].(map[string]any)["comfort_temperature"].(map[string]any)["members"].(map[string]any)["Wand"] != 21.0 {
		t.Errorf("payload = %s", m.Payload)
	}
}

func TestStaleGroupSettings(t *testing.T) {
	cfg := TopicConfig{}
	g := testGroup()
	g.Settings = testGroupSettings
	all := append(append([]SettingDesc{}, testGroupSettings...),
		SettingDesc{Key: "eco_temperature", Component: SettingNumber})
	members := GroupMembersMessage(cfg, g)
	settings, _ := GroupSettingsMessage(cfg, g.Slug, GroupSettingsState{})
	retainedMsgs := map[string][]byte{members.Topic: members.Payload, settings.Topic: settings.Payload}

	// Unchanged: nothing to remove.
	if got := StaleGroupMessages(cfg, []GroupDesc{g}, retainedMsgs, testGroupSettings...); len(got) != 0 {
		t.Errorf("unchanged: %v", topicsOf(got))
	}
	// A group that never had settings published: nothing either.
	plain := testGroup()
	if got := StaleGroupMessages(cfg, []GroupDesc{plain}, map[string][]byte{members.Topic: members.Payload}, all...); len(got) != 0 {
		t.Errorf("group without settings: %v", topicsOf(got))
	}

	// The radiator thermostat left: the window open setting is gone.
	less := testGroup()
	less.Settings = []SettingDesc{testGroupSettings[0], testGroupSettings[2]}
	got := topicsOf(StaleGroupMessages(cfg, []GroupDesc{less}, retainedMsgs, all...))
	if _, ok := got["homeassistant/number/hmbridge_group_wohnzimmer/setting_window_open_temperature/config"]; !ok || len(got) != 2 {
		t.Errorf("setting dropped: %v", got) // window_open_temperature and eco_temperature
	}

	// No thermostat left: all settings entities and the settings topic go.
	got = topicsOf(StaleGroupMessages(cfg, []GroupDesc{plain}, retainedMsgs, all...))
	for _, w := range []string{
		"homeassistant/number/hmbridge_group_wohnzimmer/setting_comfort_temperature/config",
		"homeassistant/text/hmbridge_group_wohnzimmer/setting_week_profile_monday/config",
		"homeassistant/sensor/hmbridge_group_wohnzimmer/settings_status/config",
		"homeassistant/sensor/hmbridge_group_wohnzimmer/settings_consistency/config",
		"homeassistant/button/hmbridge_group_wohnzimmer/settings_refresh/config",
		"hmbridge/group/wohnzimmer/settings",
	} {
		if _, ok := got[w]; !ok {
			t.Errorf("no thermostats: missing removal of %s", w)
		}
	}
	for topic := range topicsOf(GroupDiscoveryMessages(cfg, plain)) {
		if _, ok := got[topic]; ok {
			t.Errorf("removes live %s", topic)
		}
	}

	// The group was removed: its settings entities go too.
	got = topicsOf(StaleGroupMessages(cfg, nil, retainedMsgs, all...))
	for _, w := range []string{
		"homeassistant/number/hmbridge_group_wohnzimmer/setting_eco_temperature/config",
		"homeassistant/sensor/hmbridge_group_wohnzimmer/settings_consistency/config",
		"hmbridge/group/wohnzimmer/settings",
		"homeassistant/climate/hmbridge_group_wohnzimmer/config",
	} {
		if _, ok := got[w]; !ok {
			t.Errorf("removed group: missing removal of %s", w)
		}
	}
}
