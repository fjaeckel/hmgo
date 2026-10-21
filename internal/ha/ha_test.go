package ha

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestHAMode(t *testing.T) {
	for _, tc := range []struct {
		hm           string
		target       float64
		mode, preset string
	}{
		{"auto", 21, "auto", "none"},
		{"auto", 4.5, "auto", "none"},
		{"manual", 21, "heat", "none"},
		{"manual", 4.5, "off", "none"},
		{"manual", 4.0, "off", "none"},
		{"manual", 5, "heat", "none"},
		{"MANUAL", 30.5, "heat", "none"},
		{"party", 22, "auto", "none"},
		{"boost", 21, "heat", "boost"},
		{"bogus", 21, "", ""},
	} {
		m, p := HAMode(tc.hm, tc.target)
		if m != tc.mode || p != tc.preset {
			t.Errorf("HAMode(%q, %v) = %q, %q; want %q, %q", tc.hm, tc.target, m, p, tc.mode, tc.preset)
		}
	}
}

func TestHAAction(t *testing.T) {
	if got := HAAction("heat", 30); got != "heating" {
		t.Errorf("got %q", got)
	}
	if got := HAAction("auto", 0); got != "idle" {
		t.Errorf("got %q", got)
	}
	if got := HAAction("off", 0); got != "off" {
		t.Errorf("got %q", got)
	}
}

func TestBatteryPercent(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want int
	}{{1.5, 0}, {2.2, 0}, {2.6, 50}, {2.8, 75}, {3.0, 100}, {3.3, 100}} {
		if got := BatteryPercent(tc.v); got != tc.want {
			t.Errorf("BatteryPercent(%v) = %d, want %d", tc.v, got, tc.want)
		}
	}
}

func TestStateMergeAndJSON(t *testing.T) {
	var s State
	if b, _ := json.Marshal(s); string(b) != "{}" {
		t.Fatalf("empty state = %s, want {}", b)
	}
	seen := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	s.Merge(State{CurrentTemperature: ptr(20.5), TargetTemperature: ptr(21.0), Mode: "auto", Preset: "none", ValvePosition: ptr(0), LastSeen: seen})
	s.Merge(State{BatteryVoltage: ptr(2.6), LowBattery: ptr(false), Fault: "none", RSSI: ptr(-60), ControlMode: "auto", BoostMinutes: ptr(0), MessagesReceived: ptr(uint64(3)), AuthStatus: "ok"})
	// Partial update keeps everything else.
	s.Merge(State{CurrentTemperature: ptr(20.0), ValvePosition: ptr(30), Action: "heating"})

	msg, err := StateMessage(TopicConfig{}, "38F59C", s)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Topic != "hmbridge/38f59c/state" || !msg.Retain {
		t.Errorf("topic/retain = %q/%v", msg.Topic, msg.Retain)
	}
	var got map[string]any
	if err := json.Unmarshal(msg.Payload, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"current_temperature": 20.0, "target_temperature": 21.0, "mode": "auto", "preset": "none",
		"action": "heating", "valve_position": 30.0, "battery_voltage": 2.6, "battery_percent": 50.0,
		"low_battery": false, "fault": "none", "rssi": -60.0, "last_seen": "2026-10-21T12:00:00Z",
		"control_mode": "auto", "boost_minutes": 0.0, "messages_received": 3.0, "auth_status": "ok",
	}
	if len(got) != len(want) {
		t.Errorf("got %d fields %v, want %d", len(got), got, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}

	var back State
	if err := json.Unmarshal(msg.Payload, &back); err != nil {
		t.Fatal(err)
	}
	if *back.CurrentTemperature != 20 || !back.LastSeen.Equal(seen) || *back.MessagesReceived != 3 {
		t.Errorf("roundtrip mismatch: %+v", back)
	}

	// Merge must copy, not alias.
	v := 5
	var a State
	a.Merge(State{RSSI: &v})
	v = 7
	if *a.RSSI != 5 {
		t.Error("Merge aliases pointer fields")
	}
}

func TestParseCommand(t *testing.T) {
	cfg := TopicConfig{}
	for _, tc := range []struct {
		topic, payload string
		want           Command
		wantErr        bool
	}{
		{"hmbridge/38f59c/set/temperature", "21.5", Command{Addr: "38f59c", Kind: SetTemperature, Temperature: 21.5}, false},
		{"hmbridge/38F59C/set/temperature", " 21.3 ", Command{Addr: "38f59c", Kind: SetTemperature, Temperature: 21.5}, false},
		{"hmbridge/38f59c/set/temperature", "21.2", Command{Addr: "38f59c", Kind: SetTemperature, Temperature: 21}, false},
		{"hmbridge/38f59c/set/temperature", "4.5", Command{Addr: "38f59c", Kind: SetTemperature, Temperature: 4.5}, false},
		{"hmbridge/38f59c/set/temperature", "30.5", Command{Addr: "38f59c", Kind: SetTemperature, Temperature: 30.5}, false},
		{"hmbridge/38f59c/set/temperature", "4", Command{}, true},
		{"hmbridge/38f59c/set/temperature", "31", Command{}, true},
		{"hmbridge/38f59c/set/temperature", "NaN", Command{}, true},
		{"hmbridge/38f59c/set/temperature", "warm", Command{}, true},
		{"hmbridge/38f59c/set/mode", "heat", Command{Addr: "38f59c", Kind: SetMode, Mode: "heat"}, false},
		{"hmbridge/38f59c/set/mode", "auto", Command{Addr: "38f59c", Kind: SetMode, Mode: "auto"}, false},
		{"hmbridge/38f59c/set/mode", "off", Command{Addr: "38f59c", Kind: SetMode, Mode: "off"}, false},
		{"hmbridge/38f59c/set/mode", "cool", Command{}, true},
		{"hmbridge/38f59c/set/preset", "boost", Command{Addr: "38f59c", Kind: SetPreset, Preset: "boost"}, false},
		{"hmbridge/38f59c/set/preset", "none", Command{Addr: "38f59c", Kind: SetPreset, Preset: "none"}, false},
		{"hmbridge/38f59c/set/preset", "eco", Command{}, true},
		{"hmbridge/38f59c/set/fan", "on", Command{}, true},
		{"hmbridge/zz/set/mode", "auto", Command{}, true},
		{"hmbridge/38f59c/state", "", Command{}, true},
		{"other/38f59c/set/mode", "auto", Command{}, true},
	} {
		got, err := ParseCommand(cfg, tc.topic, []byte(tc.payload))
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseCommand(%q, %q) err = %v, wantErr %v", tc.topic, tc.payload, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseCommand(%q, %q) = %+v, want %+v", tc.topic, tc.payload, got, tc.want)
		}
	}
	// Custom base topic.
	got, err := ParseCommand(TopicConfig{Base: "hm/x"}, "hm/x/abcdef/set/mode", []byte("auto"))
	if err != nil || got.Addr != "abcdef" {
		t.Errorf("custom base: %+v, %v", got, err)
	}
}

func decode(t *testing.T, m Message) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(m.Payload, &v); err != nil {
		t.Fatalf("%s: invalid JSON: %v", m.Topic, err)
	}
	return v
}

func TestDiscoveryMessages(t *testing.T) {
	cfg := TopicConfig{}
	for _, tc := range []struct {
		d       DeviceDesc
		want    []string // topic suffixes that must exist
		notWant []string
	}{
		{
			d: DeviceDesc{Addr: "38F59C", Name: "Wohnzimmer", Model: "HM-CC-RT-DN", Serial: "MEQ0123456", Kind: RadiatorThermostat},
			want: []string{"climate/hmbridge_38f59c/config", "sensor/hmbridge_38f59c/valve_position/config",
				"sensor/hmbridge_38f59c/fault/config", "sensor/hmbridge_38f59c/battery_voltage/config",
				"sensor/hmbridge_38f59c/battery/config", "binary_sensor/hmbridge_38f59c/low_battery/config",
				"sensor/hmbridge_38f59c/rssi/config", "sensor/hmbridge_38f59c/boost_minutes/config"},
			notWant: []string{"humidity", "window_open", "comm_reporting", "boost_state"},
		},
		{
			d: DeviceDesc{Addr: "1a2b3c", Name: "Flur", Model: "HM-TC-IT-WM-W-EU", Kind: WallThermostat},
			want: []string{"climate/hmbridge_1a2b3c/config", "sensor/hmbridge_1a2b3c/humidity/config",
				"binary_sensor/hmbridge_1a2b3c/window_open/config", "binary_sensor/hmbridge_1a2b3c/comm_reporting/config",
				"sensor/hmbridge_1a2b3c/boost_state/config", "sensor/hmbridge_1a2b3c/battery_voltage/config"},
			notWant: []string{"valve_position", "fault", "boost_minutes"},
		},
	} {
		msgs := DiscoveryMessages(cfg, tc.d)
		byTopic := map[string]map[string]any{}
		uids := map[string]bool{}
		for _, m := range msgs {
			if !m.Retain {
				t.Errorf("%s not retained", m.Topic)
			}
			if !strings.HasPrefix(m.Topic, "homeassistant/") {
				t.Errorf("bad topic %s", m.Topic)
			}
			v := decode(t, m)
			byTopic[strings.TrimPrefix(m.Topic, "homeassistant/")] = v
			for _, k := range []string{"unique_id", "default_entity_id", "device", "availability_topic", "origin"} {
				if _, ok := v[k]; !ok {
					t.Errorf("%s: missing %s", m.Topic, k)
				}
			}
			uid := v["unique_id"].(string)
			if uids[uid] {
				t.Errorf("duplicate unique_id %s", uid)
			}
			uids[uid] = true
			component := strings.Split(m.Topic, "/")[1]
			if want := component + "." + uid; v["default_entity_id"] != want {
				t.Errorf("default_entity_id = %v, want %s", v["default_entity_id"], want)
			}
			dev := v["device"].(map[string]any)
			if ids := dev["identifiers"].([]any); len(ids) != 1 || ids[0] != "hmbridge_"+strings.ToLower(tc.d.Addr) {
				t.Errorf("identifiers = %v", ids)
			}
			if dev["manufacturer"] != "eQ-3" || dev["model"] != tc.d.Model || dev["name"] != tc.d.Name {
				t.Errorf("device = %v", dev)
			}
			if component != "climate" {
				if v["state_topic"] != "hmbridge/"+strings.ToLower(tc.d.Addr)+"/state" {
					t.Errorf("%s: state_topic = %v", m.Topic, v["state_topic"])
				}
				if _, ok := v["value_template"]; !ok {
					t.Errorf("%s: no value_template", m.Topic)
				}
			}
			if v["device_class"] == "enum" && v["state_class"] != nil {
				t.Errorf("%s: enum sensor must not have state_class", m.Topic)
			}
		}
		for _, w := range tc.want {
			if byTopic[w] == nil {
				t.Errorf("%s: missing %s", tc.d.Model, w)
			}
		}
		for topic := range byTopic {
			for _, nw := range tc.notWant {
				if strings.Contains(topic, "/"+nw+"/") {
					t.Errorf("%s: unexpected %s", tc.d.Model, topic)
				}
			}
		}

		addr := strings.ToLower(tc.d.Addr)
		c := byTopic["climate/hmbridge_"+addr+"/config"]
		for k, want := range map[string]any{
			"temperature_command_topic": "hmbridge/" + addr + "/set/temperature",
			"mode_command_topic":        "hmbridge/" + addr + "/set/mode",
			"preset_mode_command_topic": "hmbridge/" + addr + "/set/preset",
			"mode_state_topic":          "hmbridge/" + addr + "/state",
			"availability_topic":        "hmbridge/bridge/availability",
			"min_temp":                  4.5, "max_temp": 30.5, "temp_step": 0.5,
			"temperature_unit": "C", "optimistic": false,
		} {
			if c[k] != want {
				t.Errorf("climate %s = %v, want %v", k, c[k], want)
			}
		}
		if pm := c["preset_modes"].([]any); len(pm) != 1 || pm[0] != "boost" {
			t.Errorf("preset_modes = %v (must not contain none)", pm)
		}
		if modes := c["modes"].([]any); len(modes) != 3 {
			t.Errorf("modes = %v", modes)
		}
		for _, k := range []string{"current_temperature_template", "temperature_state_template", "mode_state_template", "preset_mode_value_template", "action_template"} {
			if !strings.Contains(c[k].(string), "is defined") {
				t.Errorf("%s does not tolerate missing fields: %v", k, c[k])
			}
		}
		_, hasHum := c["current_humidity_topic"]
		if hasHum != (tc.d.Kind == WallThermostat) {
			t.Errorf("current_humidity_topic present = %v", hasHum)
		}
		if r := byTopic["sensor/hmbridge_"+addr+"/rssi/config"]; r["enabled_by_default"] != false || r["entity_category"] != "diagnostic" {
			t.Errorf("rssi config = %v", r)
		}
		if b := byTopic["sensor/hmbridge_"+addr+"/battery_voltage/config"]; b["entity_category"] != nil || b["device_class"] != "voltage" {
			t.Errorf("battery_voltage config = %v", b)
		}

		// Removal covers every discovery topic of any kind.
		rm := map[string]bool{}
		for _, m := range RemovalMessages(cfg, tc.d.Addr) {
			if len(m.Payload) != 0 || !m.Retain {
				t.Errorf("removal %s: payload %q retain %v", m.Topic, m.Payload, m.Retain)
			}
			rm[m.Topic] = true
		}
		for _, m := range DiscoveryMessages(cfg, tc.d) {
			if !rm[m.Topic] {
				t.Errorf("RemovalMessages misses %s", m.Topic)
			}
		}
		for _, m := range StaleDiscoveryMessages(cfg, tc.d) {
			if byTopic[strings.TrimPrefix(m.Topic, "homeassistant/")] != nil {
				t.Errorf("stale message for live entity %s", m.Topic)
			}
		}
	}
}

func TestTopicConfig(t *testing.T) {
	cfg := TopicConfig{Base: "hb/", DiscoveryPrefix: "ha"}
	if got := cfg.AvailabilityTopic(); got != "hb/bridge/availability" {
		t.Errorf("got %s", got)
	}
	if got := cfg.HAStatusTopic(); got != "ha/status" {
		t.Errorf("got %s", got)
	}
	msgs := DiscoveryMessages(cfg, DeviceDesc{Addr: "abcdef", Kind: RadiatorThermostat, Model: "HM-CC-RT-DN"})
	if msgs[0].Topic != "ha/climate/hmbridge_abcdef/config" {
		t.Errorf("got %s", msgs[0].Topic)
	}
	for k, want := range map[string]string{
		"hb/bridge/availability":    "availability",
		"ha/sensor/x/y/config":      "discovery",
		"hb/abcdef/state":           "state",
		"hb/abcdef/set/temperature": "other",
	} {
		if got := cfg.messageKind(k); got != want {
			t.Errorf("messageKind(%s) = %s, want %s", k, got, want)
		}
	}
}

func TestMetricsInvalidCommand(t *testing.T) {
	c := &Client{cfg: TopicConfig{}, logf: t.Logf, commands: make(chan Command, 1)}
	before := testutil.ToFloat64(metricInvalidCommands)
	c.handleMessage(nil, fakeMsg{topic: "hmbridge/38f59c/set/mode", payload: []byte("cool")})
	if got := testutil.ToFloat64(metricInvalidCommands); got != before+1 {
		t.Errorf("invalid_commands_total = %v, want %v", got, before+1)
	}
	beforeOK := testutil.ToFloat64(metricCommands.WithLabelValues("mode"))
	c.handleMessage(nil, fakeMsg{topic: "hmbridge/38f59c/set/mode", payload: []byte("auto")})
	if got := testutil.ToFloat64(metricCommands.WithLabelValues("mode")); got != beforeOK+1 {
		t.Errorf("commands_received_total{kind=mode} = %v, want %v", got, beforeOK+1)
	}
	if cmd := <-c.commands; cmd.Mode != "auto" {
		t.Errorf("got %+v", cmd)
	}
}

type fakeMsg struct {
	topic    string
	payload  []byte
	retained bool
}

func (m fakeMsg) Duplicate() bool   { return false }
func (m fakeMsg) Qos() byte         { return 0 }
func (m fakeMsg) Retained() bool    { return m.retained }
func (m fakeMsg) Topic() string     { return m.topic }
func (m fakeMsg) MessageID() uint16 { return 0 }
func (m fakeMsg) Payload() []byte   { return m.payload }
func (m fakeMsg) Ack()              {}
