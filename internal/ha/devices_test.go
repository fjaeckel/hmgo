// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func discovered(t *testing.T, d DeviceDesc) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, m := range DiscoveryMessages(TopicConfig{}, d) {
		if !m.Retain {
			t.Errorf("%s not retained", m.Topic)
		}
		out[strings.TrimSuffix(strings.TrimPrefix(m.Topic, "homeassistant/"), "/config")] = decode(t, m)
	}
	return out
}

func TestSwitchAndDimmerDiscovery(t *testing.T) {
	sw := discovered(t, DeviceDesc{Addr: "6A9D15", Name: "Stehlampe", Model: "HM-LC-Sw1-Pl-DN-R1", Kind: Switch})
	s := sw["switch/hmbridge_6a9d15/switch"]
	if s == nil || s["command_topic"] != "hmbridge/6a9d15/set/switch" || s["state_topic"] != "hmbridge/6a9d15/state" ||
		s["payload_on"] != "ON" || s["unique_id"] != "hmbridge_6a9d15_switch" || s["name"] != nil {
		t.Errorf("switch config = %v", s)
	}
	for _, want := range []string{"sensor/hmbridge_6a9d15/last_command", "sensor/hmbridge_6a9d15/last_seen", "sensor/hmbridge_6a9d15/auth_status", "sensor/hmbridge_6a9d15/rssi"} {
		if sw[want] == nil {
			t.Errorf("switch: missing %s", want)
		}
	}
	for topic := range sw {
		for _, nw := range []string{"climate", "battery", "temperature", "light/", "button", "led_", "window"} {
			if strings.Contains(topic, nw) {
				t.Errorf("switch: unexpected %s", topic)
			}
		}
	}

	dim := discovered(t, DeviceDesc{Addr: "41E951", Name: "Leselampe", Model: "HM-LC-Dim1L-Pl-3", Kind: Dimmer})
	l := dim["light/hmbridge_41e951/light"]
	if l == nil || l["schema"] != "template" || l["command_topic"] != "hmbridge/41e951/set/light" ||
		!strings.Contains(l["command_on_template"].(string), "brightness") || !strings.Contains(l["brightness_template"].(string), "is defined") {
		t.Errorf("light config = %v", l)
	}
	// An unknown level renders empty (HA skips it), never 'None'.
	if bt := l["brightness_template"].(string); strings.Contains(bt, "None") || !strings.Contains(bt, "is defined else ''") || !strings.Contains(bt, "* 2.55") {
		t.Errorf("brightness_template = %s", bt)
	}
	if dim["sensor/hmbridge_41e951/fault"] == nil || dim["switch/hmbridge_41e951/switch"] != nil {
		t.Errorf("dimmer entities: %v", keys(dim))
	}
}

func keys(m map[string]map[string]any) []string {
	var r []string
	for k := range m {
		r = append(r, k)
	}
	return r
}

func TestRemoteDiscovery(t *testing.T) {
	for _, tc := range []struct {
		model   string
		buttons int
	}{{"HM-PB-2-WM55", 2}, {"HM-RC-8", 8}} {
		d := DeviceDesc{Addr: "6C10CE", Name: "Fernbedienung", Model: tc.model, Kind: Remote}
		got := discovered(t, d)
		events, triggers := 0, 0
		for topic, v := range got {
			switch {
			case strings.HasPrefix(topic, "event/"):
				events++
				if v["device_class"] != "button" || !reflect.DeepEqual(v["event_types"], []any{"press_short", "press_long", "press_long_release"}) ||
					!strings.HasPrefix(v["state_topic"].(string), "hmbridge/6c10ce/event/button_") {
					t.Errorf("%s = %v", topic, v)
				}
			case strings.HasPrefix(topic, "device_automation/"):
				triggers++
				if v["automation_type"] != "trigger" || v["unique_id"] != nil || v["availability"] != nil ||
					v["value_template"] != "{{ value_json.event_type }}" || v["device"] == nil {
					t.Errorf("%s = %v", topic, v)
				}
			}
		}
		if events != tc.buttons || triggers != 3*tc.buttons {
			t.Errorf("%s: %d events, %d triggers", tc.model, events, triggers)
		}
		tr := got["device_automation/hmbridge_6c10ce/button_2_press_long_release"]
		if tr["topic"] != "hmbridge/6c10ce/event/button_2" || tr["type"] != "button_long_release" || tr["subtype"] != "button_2" || tr["payload"] != "press_long_release" {
			t.Errorf("trigger = %v", tr)
		}
		if got["binary_sensor/hmbridge_6c10ce/low_battery"] == nil || got["sensor/hmbridge_6c10ce/last_command"] != nil {
			t.Errorf("%s entities: %v", tc.model, keys(got))
		}

		// The trigger payload matches the event message.
		m := EventMessage(TopicConfig{}, "6C10CE", 2, PressLongRelease)
		var ev map[string]any
		json.Unmarshal(m.Payload, &ev)
		if m.Topic != tr["topic"] || m.Retain || ev["event_type"] != tr["payload"] || ev["button"] != 2.0 {
			t.Errorf("event message %s %s", m.Topic, m.Payload)
		}

		// Stale removals: the buttons a model lacks, nothing that exists.
		stale := topicsOf(StaleDiscoveryMessages(TopicConfig{}, d))
		for topic := range stale {
			if got[strings.TrimSuffix(strings.TrimPrefix(topic, "homeassistant/"), "/config")] != nil {
				t.Errorf("stale message for live %s", topic)
			}
		}
		_, has8 := stale["homeassistant/event/hmbridge_6c10ce/button_8/config"]
		if has8 != (tc.buttons < 8) {
			t.Errorf("%s: stale button_8 = %v", tc.model, has8)
		}
	}
	// Other kinds don't get removal messages for 8 buttons and 16 LEDs.
	for topic := range topicsOf(StaleDiscoveryMessages(TopicConfig{}, DeviceDesc{Addr: "38f59c", Model: "HM-CC-RT-DN", Kind: RadiatorThermostat})) {
		if strings.Contains(topic, "button_") || strings.Contains(topic, "led_") {
			t.Errorf("thermostat stale message %s", topic)
		}
	}
	// Once per start, a device whose kind changed loses the entity sets
	// of all other kinds too, but nothing it has.
	sw := DeviceDesc{Addr: "6c10ce", Model: "HM-LC-Sw1-Pl-DN-R1", Kind: Switch}
	all := topicsOf(AllStaleDiscoveryMessages(TopicConfig{}, sw))
	for _, k := range []string{"homeassistant/device_automation/hmbridge_6c10ce/button_8_press_short/config", "homeassistant/event/hmbridge_6c10ce/button_1/config", "homeassistant/select/hmbridge_6c10ce/led_16/config", "homeassistant/light/hmbridge_6c10ce/light/config", "homeassistant/climate/hmbridge_6c10ce/config"} {
		if _, ok := all[k]; !ok {
			t.Errorf("AllStaleDiscoveryMessages lacks %s", k)
		}
	}
	for _, m := range DiscoveryMessages(TopicConfig{}, sw) {
		if _, ok := all[m.Topic]; ok {
			t.Errorf("AllStaleDiscoveryMessages removes live %s", m.Topic)
		}
	}
	// RemovalMessages covers them all.
	rm := topicsOf(RemovalMessages(TopicConfig{}, "6c10ce"))
	for _, k := range []string{"homeassistant/device_automation/hmbridge_6c10ce/button_8_press_short/config", "homeassistant/select/hmbridge_6c10ce/led_16/config", "homeassistant/light/hmbridge_6c10ce/light/config"} {
		if _, ok := rm[k]; !ok {
			t.Errorf("RemovalMessages lacks %s", k)
		}
	}
}

func TestLEDAndWeatherDiscovery(t *testing.T) {
	got := discovered(t, DeviceDesc{Addr: "7564EA", Name: "Status", Model: "HM-OU-LED16", Kind: LEDDisplay})
	n := 0
	for topic, v := range got {
		if strings.HasPrefix(topic, "select/") {
			n++
			if !reflect.DeepEqual(v["options"], []any{"off", "red", "green", "orange"}) {
				t.Errorf("%s options %v", topic, v["options"])
			}
		}
	}
	s := got["select/hmbridge_7564ea/led_16"]
	if n != 16 || s["command_topic"] != "hmbridge/7564ea/set/led_16" || !strings.Contains(s["value_template"].(string), "leds[15]") {
		t.Errorf("%d selects; led_16 = %v", n, s)
	}

	th := discovered(t, DeviceDesc{Addr: "6FCA4A", Name: "Garten", Model: "HM-WDS10-TH-O", Kind: WeatherSensor})
	to := discovered(t, DeviceDesc{Addr: "785B74", Name: "Teich", Model: "HM-WDS30-T-O", Kind: WeatherSensor})
	for _, w := range []string{"sensor/hmbridge_6fca4a/temperature", "sensor/hmbridge_6fca4a/humidity", "binary_sensor/hmbridge_6fca4a/low_battery"} {
		if th[w] == nil {
			t.Errorf("TH-O lacks %s", w)
		}
	}
	if to["sensor/hmbridge_785b74/humidity"] != nil || to["sensor/hmbridge_785b74/temperature"] == nil ||
		to["sensor/hmbridge_785b74/auth_status"] != nil || to["sensor/hmbridge_785b74/target_temperature"] != nil {
		t.Errorf("T-O entities: %v", keys(to))
	}
}

func TestParseActuatorCommands(t *testing.T) {
	cfg := TopicConfig{}
	for _, tc := range []struct {
		topic, payload string
		want           Command
		wantErr        bool
	}{
		{"hmbridge/6a9d15/set/switch", "ON", Command{Addr: "6a9d15", Kind: SetSwitch, On: true}, false},
		{"hmbridge/6a9d15/set/switch", "off", Command{Addr: "6a9d15", Kind: SetSwitch}, false},
		{"hmbridge/6a9d15/set/switch", "toggle", Command{}, true},
		{"hmbridge/41e951/set/light", `{"state": "on", "brightness": 128, "transition": 2}`,
			Command{Addr: "41e951", Kind: SetLight, On: true, Brightness: ptr(128), Transition: ptr(2.0)}, false},
		{"hmbridge/41e951/set/light", `{"state": "on"}`, Command{Addr: "41e951", Kind: SetLight, On: true}, false},
		{"hmbridge/41e951/set/light", `{"state": "off", "transition": 0.5}`, Command{Addr: "41e951", Kind: SetLight, Transition: ptr(0.5)}, false},
		{"hmbridge/41e951/set/light", "ON", Command{Addr: "41e951", Kind: SetLight, On: true}, false},
		{"hmbridge/41e951/set/light", `{"state": "on", "brightness": 300}`, Command{}, true},
		{"hmbridge/41e951/set/light", `{"state": "dim"}`, Command{}, true},
		{"hmbridge/41e951/set/light", `{"state": "on", "transition": -1}`, Command{}, true},
		{"hmbridge/7564ea/set/led_3", "Green", Command{Addr: "7564ea", Kind: SetLED, LED: 3, Color: "green"}, false},
		{"hmbridge/7564ea/set/led_16", "off", Command{Addr: "7564ea", Kind: SetLED, LED: 16, Color: "off"}, false},
		{"hmbridge/7564ea/set/led_17", "off", Command{}, true},
		{"hmbridge/7564ea/set/led_0", "off", Command{}, true},
		{"hmbridge/7564ea/set/led_x", "off", Command{}, true},
		{"hmbridge/7564ea/set/led_03", "off", Command{}, true},
		{"hmbridge/7564ea/set/led_+3", "off", Command{}, true},
		{"hmbridge/7564ea/set/led_1", "blue", Command{}, true},
		{"hmbridge/group/wohnzimmer/set/switch", "ON", Command{}, true},
	} {
		got, err := ParseCommand(cfg, tc.topic, []byte(tc.payload))
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseCommand(%q, %q) err = %v", tc.topic, tc.payload, err)
			continue
		}
		if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseCommand(%q, %q) = %+v, want %+v", tc.topic, tc.payload, got, tc.want)
		}
	}
	if cfg.messageKind("hmbridge/6c10ce/event/button_1") != "event" {
		t.Error("messageKind of an event topic")
	}
}

func TestStateActuatorFields(t *testing.T) {
	var s State
	s.Merge(State{On: ptr(true), Level: ptr(42.5), LEDs: []string{"red", "", "green"}})
	s.Merge(State{LEDs: []string{"", "orange"}})
	if !reflect.DeepEqual(s.LEDs, []string{"red", "orange", "green"}) || *s.Level != 42.5 || !*s.On {
		t.Errorf("merged %+v", s)
	}
	b, err := json.Marshal(s)
	if err != nil || string(b) != `{"on":true,"level":42.5,"leds":["red","orange","green"]}` {
		t.Errorf("JSON %s, %v", b, err)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, s) {
		t.Errorf("round trip %+v, %v", back, err)
	}
}
