// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Under Home Assistant control the climate entity offers heat and off
// only, with the devices' real setpoint range; the unique ID stays.
func TestClimateDiscoveryHAControl(t *testing.T) {
	cfg := TopicConfig{}
	for _, tc := range []struct {
		name  string
		msgs  func(ha bool) []Message
		topic string
		uid   string
	}{
		{"device", func(ha bool) []Message {
			return DiscoveryMessages(cfg, DeviceDesc{Addr: "38f59c", Name: "Bad", Model: "HM-CC-RT-DN", Kind: RadiatorThermostat, HAControl: ha})
		}, "homeassistant/climate/hmbridge_38f59c/config", "hmbridge_38f59c"},
		{"group", func(ha bool) []Message {
			return GroupDiscoveryMessages(cfg, GroupDesc{Slug: "bad", Name: "Bad", HAControl: ha})
		}, "homeassistant/climate/hmbridge_group_bad/config", "hmbridge_group_bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, haCtl := byTopic(t, tc.msgs(false))[tc.topic], byTopic(t, tc.msgs(true))[tc.topic]
			if dev == nil || haCtl == nil {
				t.Fatalf("climate entity missing")
			}
			if got := haCtl["modes"]; !reflect.DeepEqual(got, []any{"heat", "off"}) {
				t.Errorf("modes = %v, want [heat off] (no auto)", got)
			}
			if got := dev["modes"]; !reflect.DeepEqual(got, []any{"auto", "heat", "off"}) {
				t.Errorf("device control modes = %v", got)
			}
			if haCtl["unique_id"] != tc.uid || dev["unique_id"] != tc.uid {
				t.Errorf("unique_id %v / %v, want %s for both", haCtl["unique_id"], dev["unique_id"], tc.uid)
			}
			if haCtl["temp_step"] != 0.5 || haCtl["min_temp"] != 5.0 || haCtl["max_temp"] != 30.0 {
				t.Errorf("step/min/max = %v %v %v", haCtl["temp_step"], haCtl["min_temp"], haCtl["max_temp"])
			}
			if !reflect.DeepEqual(haCtl["preset_modes"], []any{"boost"}) {
				t.Errorf("preset_modes = %v", haCtl["preset_modes"])
			}
			// auto (the fail-safe's week program) renders as heat.
			if tpl := haCtl["mode_state_template"].(string); !strings.Contains(tpl, "'heat' if value_json.mode == 'auto'") {
				t.Errorf("mode_state_template = %s", tpl)
			}
		})
	}
}

func TestControlSensor(t *testing.T) {
	cfg := TopicConfig{}
	const topic = "homeassistant/sensor/hmbridge_38f59c/control/config"
	for kind, want := range map[Kind]bool{RadiatorThermostat: true, WallThermostat: true, WindowContact: false, Switch: false} {
		msgs := byTopic(t, DiscoveryMessages(cfg, DeviceDesc{Addr: "38f59c", Model: "x", Kind: kind}))
		c, ok := msgs[topic]
		if ok != want {
			t.Errorf("%v: control sensor present %v, want %v", kind, ok, want)
			continue
		}
		if ok && (c["entity_category"] != "diagnostic" || c["device_class"] != "enum" ||
			!reflect.DeepEqual(c["options"], []any{"device", "home_assistant", "fallback"}) ||
			!strings.Contains(c["value_template"].(string), "value_json.control")) {
			t.Errorf("%v: %v", kind, c)
		}
	}
	// Removed with the device.
	if _, ok := topicsOf(RemovalMessages(cfg, "38f59c"))[topic]; !ok {
		t.Error("RemovalMessages misses the control sensor")
	}
	if _, ok := topicsOf(GroupRemovalMessages(cfg, "bad", nil))["homeassistant/sensor/hmbridge_group_bad/control/config"]; !ok {
		t.Error("GroupRemovalMessages misses the control sensor")
	}
}

func TestStateControlJSON(t *testing.T) {
	var s State
	s.Merge(State{Control: ControlHomeAssistant})
	s.Merge(State{LastCommand: "x"}) // keeps control
	b, err := json.Marshal(s)
	if err != nil || !strings.Contains(string(b), `"control":"home_assistant"`) {
		t.Fatalf("%s %v", b, err)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil || back.Control != ControlHomeAssistant {
		t.Errorf("round trip: %+v %v", back, err)
	}
}
