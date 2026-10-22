// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

package ha

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Manufacturer is reported in the HA device registry.
const Manufacturer = "eQ-3"

// Template fragments. Every state message carries the full merged state,
// so a missing field means "unknown". Numeric templates render "" for
// missing fields (HA ignores empty numeric updates); text and binary
// templates render "None", which HA maps to unknown.
func numTemplate(field string) string {
	return fmt.Sprintf("{{ value_json.%[1]s if value_json.%[1]s is defined else '' }}", field)
}

func textTemplate(field, missing string) string {
	return fmt.Sprintf("{{ value_json.%[1]s if value_json.%[1]s is defined else '%[2]s' }}", field, missing)
}

func boolTemplate(field string) string {
	return fmt.Sprintf("{{ ('ON' if value_json.%[1]s else 'OFF') if value_json.%[1]s is defined else 'None' }}", field)
}

// entity describes one non-climate entity.
type entity struct {
	component string // "sensor" | "binary_sensor"
	key       string // unique suffix, also object id suffix
	match     func(DeviceDesc) bool
	config    map[string]any
}

func kinds(ks ...Kind) func(DeviceDesc) bool {
	return func(d DeviceDesc) bool {
		for _, k := range ks {
			if d.Kind == k {
				return true
			}
		}
		return false
	}
}

var (
	thermostats = kinds(RadiatorThermostat, WallThermostat)
	allKinds    = kinds(RadiatorThermostat, WallThermostat, WindowContact)
	rtOnly      = kinds(RadiatorThermostat)
	tcOnly      = kinds(WallThermostat)
	windowKinds = kinds(WallThermostat, WindowContact)
	rhsOnly     = func(d DeviceDesc) bool { return d.Kind == WindowContact && IsThreeState(d.Model) }
	contactOnly = func(d DeviceDesc) bool { return d.Kind == WindowContact }
)

func binarySensor(name, field, deviceClass string, extra map[string]any) map[string]any {
	m := map[string]any{
		"name":           name,
		"payload_on":     "ON",
		"payload_off":    "OFF",
		"value_template": boolTemplate(field),
	}
	if deviceClass != "" {
		m["device_class"] = deviceClass
	}
	return with(m, extra)
}

// numSensor builds a numeric measurement sensor config.
func numSensor(name, field, deviceClass, unit string, extra map[string]any) map[string]any {
	m := map[string]any{
		"name":           name,
		"state_class":    "measurement",
		"value_template": numTemplate(field),
	}
	if deviceClass != "" {
		m["device_class"] = deviceClass
	}
	if unit != "" {
		m["unit_of_measurement"] = unit
	}
	return with(m, extra)
}

var diagnostic = map[string]any{"entity_category": "diagnostic"}

func with(ms ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// entities returns a fresh list of all sensor / binary_sensor entities.
// Primary: temperatures, humidity, valve, battery, window. Diagnostic:
// everything technical. Disabled by default: RSSI, message counter.
func entities() []entity {
	disabled := map[string]any{"enabled_by_default": false}
	return []entity{
		{"sensor", "temperature", thermostats, numSensor("Temperature", "current_temperature", "temperature", "°C",
			map[string]any{"suggested_display_precision": 1})},
		{"sensor", "target_temperature", thermostats, numSensor("Target temperature", "target_temperature", "temperature", "°C",
			map[string]any{"suggested_display_precision": 1})},
		{"sensor", "humidity", tcOnly, numSensor("Humidity", "humidity", "humidity", "%", nil)},
		{"sensor", "valve_position", rtOnly, numSensor("Valve position", "valve_position", "", "%",
			map[string]any{"icon": "mdi:pipe-valve"})},
		// Contacts report low battery only, no voltage.
		{"sensor", "battery_voltage", thermostats, numSensor("Battery voltage", "battery_voltage", "voltage", "V",
			map[string]any{"suggested_display_precision": 1})},
		{"sensor", "battery", thermostats, numSensor("Battery (estimated)", "battery_percent", "battery", "%", nil)},
		{"binary_sensor", "low_battery", allKinds, binarySensor("Battery low", "low_battery", "battery", nil)},
		{"binary_sensor", "window_open", windowKinds, binarySensor("Window", "window_open", "window", nil)},
		{"sensor", "window_state", rhsOnly, map[string]any{
			"name":           "Window state",
			"device_class":   "enum",
			"options":        []string{WindowClosed, WindowTilted, WindowOpened},
			"icon":           "mdi:window-open-variant",
			"value_template": textTemplate("window_state", "None"),
		}},
		{"binary_sensor", "sabotage", contactOnly, binarySensor("Sabotage", "sabotage", "tamper", diagnostic)},
		{"sensor", "control_mode", thermostats, with(diagnostic, map[string]any{
			"name":           "Control mode",
			"device_class":   "enum",
			"options":        []string{"auto", "manual", "party", "boost"},
			"icon":           "mdi:thermostat-auto",
			"value_template": textTemplate("control_mode", "None"),
		})},
		// Both models report the remaining boost time in minutes
		// (BOOST_STATE, unit "min" in eQ-3's rftypes XML).
		{"sensor", "boost_minutes", thermostats, numSensor("Boost remaining", "boost_minutes", "duration", "min",
			with(diagnostic, map[string]any{"icon": "mdi:rocket-launch"}))},
		// Former TC-only raw boost sensor: matches nothing, so
		// StaleDiscoveryMessages removes it from existing installations.
		{"sensor", "boost_state", func(DeviceDesc) bool { return false }, nil},
		{"sensor", "last_command", thermostats, with(diagnostic, map[string]any{
			"name":           "Last command",
			"icon":           "mdi:send-check-outline",
			"value_template": textTemplate("last_command", "None"),
		})},
		{"sensor", "fault", rtOnly, with(diagnostic, map[string]any{
			"name":           "Fault",
			"icon":           "mdi:alert-circle-outline",
			"value_template": textTemplate("fault", "None"),
		})},
		{"binary_sensor", "comm_reporting", tcOnly, binarySensor("Communication reporting", "comm_reporting", "",
			with(diagnostic, map[string]any{"icon": "mdi:access-point"}))},
		{"sensor", "auth_status", allKinds, with(diagnostic, map[string]any{
			"name":           "AES authentication",
			"icon":           "mdi:shield-key-outline",
			"value_template": textTemplate("auth_status", "None"),
		})},
		{"sensor", "last_seen", allKinds, with(diagnostic, map[string]any{
			"name":           "Last seen",
			"device_class":   "timestamp",
			"value_template": textTemplate("last_seen", "None"),
		})},
		{"sensor", "rssi", allKinds, numSensor("RSSI", "rssi", "signal_strength", "dBm", with(diagnostic, disabled))},
		{"sensor", "messages_received", allKinds, with(diagnostic, disabled, map[string]any{
			"name":           "Messages received",
			"state_class":    "total_increasing",
			"icon":           "mdi:counter",
			"value_template": numTemplate("messages_received"),
		})},
	}
}

// node is an HA device: a BidCoS device or a heating group.
type node struct {
	id         string // "hmbridge_<addr>" or "hmbridge_group_<slug>"
	stateTopic string
	cmdTopic   func(what string) string
	device     map[string]any
	humidity   bool // climate shows current humidity
	// deviceAvail is the per-device availability topic ("" for groups).
	deviceAvail string
}

func deviceNode(cfg TopicConfig, d DeviceDesc) node {
	addr := strings.ToLower(d.Addr)
	dev := map[string]any{
		"identifiers":  []string{nodeID(addr)},
		"manufacturer": Manufacturer,
	}
	if d.Name != "" {
		dev["name"] = d.Name
	} else {
		dev["name"] = d.Model + " " + strings.ToUpper(addr)
	}
	if d.Model != "" {
		dev["model"] = d.Model
	}
	if d.Serial != "" {
		dev["serial_number"] = d.Serial
	}
	return node{
		id:          nodeID(addr),
		stateTopic:  cfg.StateTopic(addr),
		cmdTopic:    func(what string) string { return cfg.CommandTopic(addr, what) },
		device:      dev,
		humidity:    d.Kind == WallThermostat,
		deviceAvail: cfg.DeviceAvailabilityTopic(addr),
	}
}

func groupNode(cfg TopicConfig, g GroupDesc) node {
	name := g.Name
	if name == "" {
		name = g.Slug
	}
	return node{
		id:         groupNodeID(g.Slug),
		stateTopic: cfg.GroupStateTopic(g.Slug),
		cmdTopic:   func(what string) string { return cfg.GroupCommandTopic(g.Slug, what) },
		device: map[string]any{
			"identifiers":  []string{groupNodeID(g.Slug)},
			"name":         name + " (group)",
			"model":        "Heating group",
			"manufacturer": "hmbridge",
		},
		humidity:    true, // harmless when the group has no humidity
		deviceAvail: cfg.GroupAvailabilityTopic(g.Slug),
	}
}

func nodeID(addr string) string      { return "hmbridge_" + strings.ToLower(addr) }
func groupNodeID(slug string) string { return "hmbridge_group_" + slug }
func climateTopic(cfg TopicConfig, id string) string {
	return cfg.prefix() + "/climate/" + id + "/config"
}

func entityTopic(cfg TopicConfig, component, id, key string) string {
	return cfg.prefix() + "/" + component + "/" + id + "/" + key + "/config"
}

// bridgeAvailabilityOnly lists entities that stay available while their
// device is silent, so that one can still see when it was last heard.
var bridgeAvailabilityOnly = map[string]bool{"last_seen": true, "auth_status": true}

// common sets keys shared by all entities of a node. key is the entity
// key ("" for the climate entity).
func common(cfg TopicConfig, n node, component, uid, key string, m map[string]any) {
	m["unique_id"] = uid
	// default_entity_id is honoured since HA 2025.10. object_id is not
	// sent: it raises a deprecation repair on 2025.10 – 2026.3 and is
	// ignored since 2026.4; older releases just pick their own entity ID.
	m["default_entity_id"] = component + "." + uid
	m["device"] = n.device
	m["origin"] = map[string]any{
		"name":        "hmbridge",
		"support_url": "https://github.com/fjaeckel/hmgo",
	}
	m["payload_available"] = PayloadOnline
	m["payload_not_available"] = PayloadOffline
	if n.deviceAvail == "" || bridgeAvailabilityOnly[key] {
		m["availability_topic"] = cfg.AvailabilityTopic()
		return
	}
	m["availability"] = []map[string]any{
		{"topic": cfg.AvailabilityTopic()},
		{"topic": n.deviceAvail},
	}
	m["availability_mode"] = "all"
}

func climateConfig(cfg TopicConfig, n node) map[string]any {
	st := n.stateTopic
	m := map[string]any{
		"name":                         nil, // entity takes the device name
		"current_temperature_topic":    st,
		"current_temperature_template": numTemplate("current_temperature"),
		"temperature_state_topic":      st,
		"temperature_state_template":   numTemplate("target_temperature"),
		"temperature_command_topic":    n.cmdTopic("temperature"),
		"mode_state_topic":             st,
		"mode_state_template":          textTemplate("mode", "None"),
		"mode_command_topic":           n.cmdTopic("mode"),
		"modes":                        []string{ModeAuto, ModeHeat, ModeOff},
		"preset_mode_state_topic":      st,
		"preset_mode_value_template":   textTemplate("preset", PresetNone),
		"preset_mode_command_topic":    n.cmdTopic("preset"),
		// HA adds the "none" preset itself; it must not be listed.
		"preset_modes":     []string{PresetBoost},
		"action_topic":     st,
		"action_template":  textTemplate("action", "None"),
		"min_temp":         MinTemp,
		"max_temp":         MaxTemp,
		"temp_step":        TempStep,
		"temperature_unit": "C",
		"precision":        0.1,
		"optimistic":       false,
		"qos":              1,
	}
	if n.humidity {
		m["current_humidity_topic"] = st
		m["current_humidity_template"] = numTemplate("humidity")
	}
	common(cfg, n, "climate", n.id, "", m)
	return m
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only static, JSON-safe values are marshalled
	}
	return b
}

func retained(topic string, payload []byte) Message {
	return Message{Topic: topic, Payload: payload, Retain: true, QoS: 1}
}

func entityMessage(cfg TopicConfig, n node, e entity) Message {
	m := e.config
	if m == nil {
		m = map[string]any{}
	}
	m["state_topic"] = n.stateTopic
	common(cfg, n, e.component, n.id+"_"+e.key, e.key, m)
	return retained(entityTopic(cfg, e.component, n.id, e.key), mustJSON(m))
}

// DiscoveryMessages returns the retained HA MQTT discovery config messages
// for d: a climate entity (thermostats only) plus the sensors and binary
// sensors for d.Kind / d.Model.
func DiscoveryMessages(cfg TopicConfig, d DeviceDesc) []Message {
	n := deviceNode(cfg, d)
	var msgs []Message
	if thermostats(d) {
		msgs = append(msgs, retained(climateTopic(cfg, n.id), mustJSON(climateConfig(cfg, n))))
	}
	for _, e := range entities() {
		if e.match(d) {
			msgs = append(msgs, entityMessage(cfg, n, e))
		}
	}
	return msgs
}

// RemovalMessages returns empty retained messages that remove all
// discovery configs (for any Kind) and the retained state of the device at
// addr, e.g. for devices that were removed from the configuration.
func RemovalMessages(cfg TopicConfig, addr string) []Message {
	id := nodeID(addr)
	msgs := []Message{retained(climateTopic(cfg, id), nil)}
	for _, e := range entities() {
		msgs = append(msgs, retained(entityTopic(cfg, e.component, id, e.key), nil))
	}
	return append(msgs, retained(cfg.StateTopic(addr), nil), retained(cfg.DeviceAvailabilityTopic(addr), nil))
}

// StaleDiscoveryMessages returns removal messages for the entities that
// exist for some Kind/Model but not for d (useful when a device's model was
// corrected in the configuration). Publishing them is optional.
func StaleDiscoveryMessages(cfg TopicConfig, d DeviceDesc) []Message {
	id := nodeID(d.Addr)
	var msgs []Message
	if !thermostats(d) {
		msgs = append(msgs, retained(climateTopic(cfg, id), nil))
	}
	for _, e := range entities() {
		if !e.match(d) {
			msgs = append(msgs, retained(entityTopic(cfg, e.component, id, e.key), nil))
		}
	}
	return msgs
}

func groupWindowEntity() entity {
	return entity{"binary_sensor", "window_open", nil, binarySensor("Window", "window_open", "window", nil)}
}

// GroupDiscoveryMessages returns the discovery messages for a heating
// group: a climate entity (commands on <base>/group/<slug>/set/...) and a
// window binary sensor, on their own HA device.
func GroupDiscoveryMessages(cfg TopicConfig, g GroupDesc) []Message {
	n := groupNode(cfg, g)
	return []Message{
		retained(climateTopic(cfg, n.id), mustJSON(climateConfig(cfg, n))),
		entityMessage(cfg, n, groupWindowEntity()),
	}
}

// GroupRemovalMessages removes a heating group's discovery configs and
// retained state.
func GroupRemovalMessages(cfg TopicConfig, slug string) []Message {
	id := groupNodeID(slug)
	e := groupWindowEntity()
	return []Message{
		retained(climateTopic(cfg, id), nil),
		retained(entityTopic(cfg, e.component, id, e.key), nil),
		retained(cfg.GroupStateTopic(slug), nil),
		retained(cfg.GroupAvailabilityTopic(slug), nil),
	}
}
