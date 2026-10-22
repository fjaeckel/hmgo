package ha

import (
	"strings"
	"testing"
)

func topicsOf(msgs []Message) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, m := range msgs {
		out[m.Topic] = nil
	}
	return out
}

func TestWindowContactDiscovery(t *testing.T) {
	cfg := TopicConfig{}
	for _, tc := range []struct {
		model   string
		want    []string
		notWant []string
	}{
		{"HM-Sec-SC-2",
			[]string{"binary_sensor/hmbridge_aabbcc/window_open", "binary_sensor/hmbridge_aabbcc/sabotage", "binary_sensor/hmbridge_aabbcc/low_battery", "sensor/hmbridge_aabbcc/rssi", "sensor/hmbridge_aabbcc/last_seen", "sensor/hmbridge_aabbcc/messages_received"},
			[]string{"climate/", "window_state", "battery_voltage", "/battery/", "temperature", "valve"}},
		{"HM-Sec-RHS",
			[]string{"binary_sensor/hmbridge_aabbcc/window_open", "sensor/hmbridge_aabbcc/window_state", "binary_sensor/hmbridge_aabbcc/low_battery", "binary_sensor/hmbridge_aabbcc/sabotage"},
			[]string{"climate/", "battery_voltage", "/battery/"}},
		{"HM-Sec-SCo",
			[]string{"binary_sensor/hmbridge_aabbcc/window_open", "binary_sensor/hmbridge_aabbcc/sabotage", "binary_sensor/hmbridge_aabbcc/low_battery"},
			[]string{"climate/", "window_state", "battery_voltage", "/battery/"}},
	} {
		kind, ok := KindForModel(tc.model)
		if !ok || kind != WindowContact {
			t.Fatalf("KindForModel(%s) = %v, %v", tc.model, kind, ok)
		}
		msgs := DiscoveryMessages(cfg, DeviceDesc{Addr: "AABBCC", Name: "Fenster", Model: tc.model, Kind: kind})
		got := topicsOf(msgs)
		for _, w := range tc.want {
			if _, ok := got["homeassistant/"+w+"/config"]; !ok {
				t.Errorf("%s: missing %s", tc.model, w)
			}
		}
		for topic := range got {
			for _, nw := range tc.notWant {
				if strings.Contains(topic, nw) {
					t.Errorf("%s: unexpected %s", tc.model, topic)
				}
			}
		}
		for _, m := range msgs {
			v := decode(t, m)
			if strings.HasSuffix(m.Topic, "/sabotage/config") && (v["device_class"] != "tamper" || v["entity_category"] != "diagnostic") {
				t.Errorf("sabotage config = %v", v)
			}
			if strings.HasSuffix(m.Topic, "/window_state/config") && len(v["options"].([]any)) != 3 {
				t.Errorf("window_state config = %v", v)
			}
		}
		// Stale messages for a contact include the climate entity and
		// never overlap the live ones.
		stale := StaleDiscoveryMessages(cfg, DeviceDesc{Addr: "aabbcc", Model: tc.model, Kind: kind})
		for _, m := range stale {
			if _, ok := got[m.Topic]; ok {
				t.Errorf("stale overlaps live: %s", m.Topic)
			}
		}
		if _, ok := topicsOf(stale)["homeassistant/climate/hmbridge_aabbcc/config"]; !ok {
			t.Error("stale messages miss climate entity")
		}
	}
}

func TestGroupDiscovery(t *testing.T) {
	cfg := TopicConfig{}
	msgs := GroupDiscoveryMessages(cfg, GroupDesc{Slug: "wohnzimmer", Name: "Wohnzimmer"})
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	c := decode(t, msgs[0])
	if msgs[0].Topic != "homeassistant/climate/hmbridge_group_wohnzimmer/config" {
		t.Errorf("topic = %s", msgs[0].Topic)
	}
	for k, want := range map[string]any{
		"unique_id":                 "hmbridge_group_wohnzimmer",
		"default_entity_id":         "climate.hmbridge_group_wohnzimmer",
		"temperature_command_topic": "hmbridge/group/wohnzimmer/set/temperature",
		"mode_command_topic":        "hmbridge/group/wohnzimmer/set/mode",
		"preset_mode_command_topic": "hmbridge/group/wohnzimmer/set/preset",
		"temperature_state_topic":   "hmbridge/group/wohnzimmer/state",
	} {
		if c[k] != want {
			t.Errorf("%s = %v, want %v", k, c[k], want)
		}
	}
	dev := c["device"].(map[string]any)
	if dev["name"] != "Wohnzimmer (group)" || dev["model"] != "Heating group" || dev["identifiers"].([]any)[0] != "hmbridge_group_wohnzimmer" {
		t.Errorf("device = %v", dev)
	}
	w := decode(t, msgs[1])
	if msgs[1].Topic != "homeassistant/binary_sensor/hmbridge_group_wohnzimmer/window_open/config" || w["device_class"] != "window" || w["state_topic"] != "hmbridge/group/wohnzimmer/state" {
		t.Errorf("window = %s %v", msgs[1].Topic, w)
	}
	rm := topicsOf(GroupRemovalMessages(cfg, "wohnzimmer"))
	for _, m := range msgs {
		if _, ok := rm[m.Topic]; !ok {
			t.Errorf("GroupRemovalMessages misses %s", m.Topic)
		}
	}
	sm, err := GroupStateMessage(cfg, "wohnzimmer", State{WindowOpen: ptr(true)})
	if err != nil || sm.Topic != "hmbridge/group/wohnzimmer/state" || string(sm.Payload) != `{"window_open":true}` {
		t.Errorf("GroupStateMessage = %+v, %v", sm, err)
	}
}

func TestParseGroupCommand(t *testing.T) {
	cfg := TopicConfig{}
	got, err := ParseCommand(cfg, "hmbridge/group/wohnzimmer/set/temperature", []byte("22"))
	if err != nil || got != (Command{Group: "wohnzimmer", Kind: SetTemperature, Temperature: 22}) {
		t.Errorf("got %+v, %v", got, err)
	}
	got, err = ParseCommand(cfg, "hmbridge/group/wohnzimmer/set/preset", []byte("boost"))
	if err != nil || got != (Command{Group: "wohnzimmer", Kind: SetPreset, Preset: "boost"}) {
		t.Errorf("got %+v, %v", got, err)
	}
	for _, topic := range []string{"hmbridge/group/Wohn Zimmer/set/mode", "hmbridge/group/x/set/fan", "hmbridge/group/x/state"} {
		if _, err := ParseCommand(cfg, topic, []byte("auto")); err == nil {
			t.Errorf("ParseCommand(%s) succeeded", topic)
		}
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Wohnzimmer":     "wohnzimmer",
		"Wohnzimmer Süd": "wohnzimmer_s_d",
		"  Bad / OG ":    "bad_og",
		"küche-1":        "k_che-1",
		"äöü":            "group",
	} {
		if got := Slugify(in); got != want || !ValidSlug(got) {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGroupAvailability(t *testing.T) {
	cfg := TopicConfig{}
	for _, m := range GroupDiscoveryMessages(cfg, GroupDesc{Slug: "bad", Name: "Bad"}) {
		v := decode(t, m)
		av, _ := v["availability"].([]any)
		if len(av) != 2 || v["availability_mode"] != "all" || av[1].(map[string]any)["topic"] != "hmbridge/group/bad/availability" {
			t.Errorf("%s: availability = %v", m.Topic, v["availability"])
		}
	}
	if m := GroupAvailabilityMessage(cfg, "bad", false); m.Topic != "hmbridge/group/bad/availability" || string(m.Payload) != "offline" || !m.Retain {
		t.Errorf("GroupAvailabilityMessage = %+v", m)
	}
}
