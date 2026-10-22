// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testSettings = []SettingDesc{
	{Key: "temperature_offset", Name: "Temperature offset", Component: SettingNumber, Unit: "K", Min: -3.5, Max: 3.5, Step: 0.5, Enabled: true},
	{Key: "decalcification_day", Name: "Decalcification day", Component: SettingSelect, Options: []string{"Saturday", "Sunday"}},
	{Key: "button_lock", Name: "Button lock", Component: SettingSwitch, Enabled: true},
	{Key: "week_profile_monday", Name: "Week profile Monday", Component: SettingText, MaxLength: 255, Enabled: true},
}

func TestParseSettingCommands(t *testing.T) {
	cfg := TopicConfig{}
	cmd, err := ParseCommand(cfg, "hmbridge/38F59C/set/setting_temperature_offset", []byte(" 1.5 "))
	if err != nil || cmd.Kind != SetSetting || cmd.Addr != "38f59c" || cmd.Setting != "temperature_offset" || cmd.Value != "1.5" {
		t.Errorf("SetSetting: %+v, %v", cmd, err)
	}
	cmd, err = ParseCommand(cfg, "hmbridge/38f59c/set/settings_refresh", []byte("PRESS"))
	if err != nil || cmd.Kind != RefreshSettings || cmd.Addr != "38f59c" {
		t.Errorf("RefreshSettings: %+v, %v", cmd, err)
	}
	for _, c := range []struct{ topic, payload string }{
		{"hmbridge/38f59c/set/setting_", "1"},
		{"hmbridge/38f59c/set/setting_Temperature", "1"},
		{"hmbridge/38f59c/set/setting_a-b", "1"},
		{"hmbridge/38f59c/set/setting_" + strings.Repeat("x", 65), "1"},
		{"hmbridge/38f59c/set/setting_x", strings.Repeat("1", MaxSettingValue+1)},
		{"hmbridge/38f59c/set/setting_x", "\xff"},
		{"hmbridge/group/bad/set/setting_x", "1"},
		{"hmbridge/group/bad/set/settings_refresh", ""},
	} {
		if cmd, err := ParseCommand(cfg, c.topic, []byte(c.payload)); err == nil {
			t.Errorf("%s accepted: %+v", c.topic, cmd)
		}
	}
}

func discoveryByTopic(msgs []Message) map[string]map[string]any {
	res := map[string]map[string]any{}
	for _, m := range msgs {
		var v map[string]any
		if len(m.Payload) > 0 {
			json.Unmarshal(m.Payload, &v)
		}
		res[m.Topic] = v
	}
	return res
}

func TestSettingsDiscovery(t *testing.T) {
	d := DeviceDesc{Addr: "38f59c", Name: "Bad", Model: "HM-CC-RT-DN", Kind: RadiatorThermostat, Settings: testSettings}
	got := discoveryByTopic(DiscoveryMessages(TopicConfig{}, d))
	num := got["homeassistant/number/hmbridge_38f59c/setting_temperature_offset/config"]
	if num == nil || num["min"] != -3.5 || num["max"] != 3.5 || num["step"] != 0.5 || num["unit_of_measurement"] != "K" ||
		num["entity_category"] != "config" || num["mode"] != "box" || num["enabled_by_default"] != nil ||
		num["command_topic"] != "hmbridge/38f59c/set/setting_temperature_offset" || num["state_topic"] != "hmbridge/38f59c/settings" ||
		num["unique_id"] != "hmbridge_38f59c_setting_temperature_offset" ||
		num["value_template"] != "{{ value_json.current['temperature_offset'] if value_json.current is defined and 'temperature_offset' in value_json.current else '' }}" {
		t.Errorf("number: %v", num)
	}
	// Settings stay available while the bridge is: changes to a sleeping
	// device are queued.
	if num["availability_topic"] != "hmbridge/bridge/availability" || num["availability"] != nil {
		t.Errorf("number availability: %v %v", num["availability_topic"], num["availability"])
	}
	sel := got["homeassistant/select/hmbridge_38f59c/setting_decalcification_day/config"]
	if sel == nil || sel["enabled_by_default"] != false || len(sel["options"].([]any)) != 2 || sel["optimistic"] != false {
		t.Errorf("select: %v", sel)
	}
	sw := got["homeassistant/switch/hmbridge_38f59c/setting_button_lock/config"]
	if sw == nil || sw["payload_on"] != "ON" || sw["state_off"] != "OFF" {
		t.Errorf("switch: %v", sw)
	}
	txt := got["homeassistant/text/hmbridge_38f59c/setting_week_profile_monday/config"]
	if txt == nil || txt["max"] != 255.0 || txt["min"] != 0.0 || txt["mode"] != "text" {
		t.Errorf("text: %v", txt)
	}
	st := got["homeassistant/sensor/hmbridge_38f59c/settings_status/config"]
	if st == nil || st["json_attributes_topic"] != "hmbridge/38f59c/settings" || st["entity_category"] != "diagnostic" {
		t.Errorf("status sensor: %v", st)
	}
	btn := got["homeassistant/button/hmbridge_38f59c/settings_refresh/config"]
	if btn == nil || btn["command_topic"] != "hmbridge/38f59c/set/settings_refresh" || btn["entity_category"] != "config" {
		t.Errorf("refresh button: %v", btn)
	}

	// A device without settings has none of them.
	plain := discoveryByTopic(DiscoveryMessages(TopicConfig{}, DeviceDesc{Addr: "6fca4a", Model: "HM-WDS30-T-O", Kind: WeatherSensor}))
	for topic := range plain {
		if strings.Contains(topic, "setting") {
			t.Errorf("weather sensor has %s", topic)
		}
	}

	// Stale settings of other models are removed once per start (not on
	// every reconnect); SettingsRemovalMessages removes all.
	d.AllSettings = append(testSettings, SettingDesc{Key: "power_on_action", Component: SettingSelect, Options: []string{"off", "on"}})
	other := "homeassistant/select/hmbridge_38f59c/setting_power_on_action/config"
	if _, ok := discoveryByTopic(StaleDiscoveryMessages(TopicConfig{}, d))[other]; ok {
		t.Error("scoped stale messages remove other models' settings")
	}
	all := discoveryByTopic(AllStaleDiscoveryMessages(TopicConfig{}, d))
	if v, ok := all[other]; !ok || v != nil {
		t.Errorf("AllStale lacks %s", other)
	}
	if _, ok := all["homeassistant/number/hmbridge_38f59c/setting_temperature_offset/config"]; ok {
		t.Error("AllStale removes an own setting")
	}
	rm := discoveryByTopic(SettingsRemovalMessages(TopicConfig{}, d))
	for _, topic := range []string{other, "homeassistant/text/hmbridge_38f59c/setting_week_profile_monday/config", "hmbridge/38f59c/settings"} {
		if v, ok := rm[topic]; !ok || v != nil {
			t.Errorf("SettingsRemovalMessages lacks %s", topic)
		}
	}
	if _, ok := discoveryByTopic(RemovalMessages(TopicConfig{}, "38f59c"))["hmbridge/38f59c/settings"]; !ok {
		t.Error("RemovalMessages keeps the retained settings")
	}
}

func TestSettingsMessage(t *testing.T) {
	m, err := SettingsMessage(TopicConfig{}, "38F59C", SettingsState{
		Current: map[string]any{"temperature_offset": 1.5, "button_lock": "ON"},
		Status:  SettingsOK,
		Detail:  map[string]string{"temperature_offset": "ok"},
		ReadAt:  time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
	})
	if err != nil || m.Topic != "hmbridge/38f59c/settings" || !m.Retain {
		t.Fatalf("%+v %v", m, err)
	}
	want := `{"current":{"button_lock":"ON","temperature_offset":1.5},"status":"ok","detail":{"temperature_offset":"ok"},"read_at":"2026-10-22T12:00:00Z"}`
	if string(m.Payload) != want {
		t.Errorf("payload %s\nwant    %s", m.Payload, want)
	}
	m, _ = SettingsMessage(TopicConfig{}, "38f59c", SettingsState{Status: SettingsUnread})
	if string(m.Payload) != `{"current":{},"status":"not read"}` {
		t.Errorf("empty: %s", m.Payload)
	}
}
