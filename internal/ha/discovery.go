// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

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

// entity describes one non-climate entity (or device trigger).
type entity struct {
	component string // "sensor", "binary_sensor", "switch", "light", "event", "select", "device_automation"
	key       string // unique suffix, also object id suffix
	match     func(DeviceDesc) bool
	config    map[string]any
	// command, if set, is the command topic name (…/set/<command>).
	command string
	// button, if > 0, makes the entity read the button's event topic
	// instead of the state topic (event entities, device triggers).
	button int
	// staleScope, if set, limits StaleDiscoveryMessages to devices for
	// which it returns true (so that a thermostat does not get removal
	// messages for 16 LEDs and 32 button triggers on every connect).
	staleScope func(DeviceDesc) bool
	// stateSuffix, if set, makes the entity read <base>/<addr>/<suffix>
	// instead of the state topic.
	stateSuffix string
	// noState: the entity has no state topic (buttons).
	noState bool
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
	allKinds    = kinds(RadiatorThermostat, WallThermostat, WindowContact, Switch, Dimmer, Remote, LEDDisplay, WeatherSensor)
	rtOnly      = kinds(RadiatorThermostat)
	tcOnly      = kinds(WallThermostat)
	windowKinds = kinds(WallThermostat, WindowContact)
	rhsOnly     = func(d DeviceDesc) bool { return d.Kind == WindowContact && IsThreeState(d.Model) }
	contactOnly = func(d DeviceDesc) bool { return d.Kind == WindowContact }

	// Battery powered devices that report a low battery.
	batteryKinds = kinds(RadiatorThermostat, WallThermostat, WindowContact, Remote, WeatherSensor)
	// Devices with AES (the weather sensors have none).
	aesKinds = kinds(RadiatorThermostat, WallThermostat, WindowContact, Switch, Dimmer, Remote, LEDDisplay)
	// Devices that take commands.
	commandKinds     = kinds(RadiatorThermostat, WallThermostat, Switch, Dimmer, LEDDisplay)
	temperatureKinds = kinds(RadiatorThermostat, WallThermostat, WeatherSensor)
	humidityKinds    = func(d DeviceDesc) bool {
		return d.Kind == WallThermostat || d.Kind == WeatherSensor && HasHumidity(d.Model)
	}
	faultKinds = kinds(RadiatorThermostat, Dimmer)
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
	list := []entity{
		{component: "sensor", key: "temperature", match: temperatureKinds, config: numSensor("Temperature", "current_temperature", "temperature", "°C",
			map[string]any{"suggested_display_precision": 1})},
		{component: "sensor", key: "target_temperature", match: thermostats, config: numSensor("Target temperature", "target_temperature", "temperature", "°C",
			map[string]any{"suggested_display_precision": 1})},
		{component: "sensor", key: "humidity", match: humidityKinds, config: numSensor("Humidity", "humidity", "humidity", "%", nil)},
		{component: "sensor", key: "valve_position", match: rtOnly, config: numSensor("Valve position", "valve_position", "", "%",
			map[string]any{"icon": "mdi:pipe-valve"})},
		// Contacts report low battery only, no voltage.
		{component: "sensor", key: "battery_voltage", match: thermostats, config: numSensor("Battery voltage", "battery_voltage", "voltage", "V",
			map[string]any{"suggested_display_precision": 1})},
		{component: "sensor", key: "battery", match: thermostats, config: numSensor("Battery (estimated)", "battery_percent", "battery", "%", nil)},
		{component: "binary_sensor", key: "low_battery", match: batteryKinds, config: binarySensor("Battery low", "low_battery", "battery", nil)},
		{component: "binary_sensor", key: "window_open", match: windowKinds, config: binarySensor("Window", "window_open", "window", nil)},
		{component: "sensor", key: "window_state", match: rhsOnly, config: map[string]any{
			"name":           "Window state",
			"device_class":   "enum",
			"options":        []string{WindowClosed, WindowTilted, WindowOpened},
			"icon":           "mdi:window-open-variant",
			"value_template": textTemplate("window_state", "None"),
		}},
		{component: "binary_sensor", key: "sabotage", match: contactOnly, config: binarySensor("Sabotage", "sabotage", "tamper", diagnostic)},
		{component: "sensor", key: "control_mode", match: thermostats, config: with(diagnostic, map[string]any{
			"name":           "Control mode",
			"device_class":   "enum",
			"options":        []string{"auto", "manual", "party", "boost"},
			"icon":           "mdi:thermostat-auto",
			"value_template": textTemplate("control_mode", "None"),
		})},
		// Both models report the remaining boost time in minutes
		// (BOOST_STATE, unit "min" in eQ-3's rftypes XML).
		{component: "sensor", key: "boost_minutes", match: thermostats, config: numSensor("Boost remaining", "boost_minutes", "duration", "min",
			with(diagnostic, map[string]any{"icon": "mdi:rocket-launch"}))},
		// Former TC-only raw boost sensor: matches nothing, so
		// StaleDiscoveryMessages removes it from existing installations.
		{component: "sensor", key: "boost_state", match: func(DeviceDesc) bool { return false }, config: nil},
		{component: "sensor", key: "last_command", match: commandKinds, config: with(diagnostic, map[string]any{
			"name":           "Last command",
			"icon":           "mdi:send-check-outline",
			"value_template": textTemplate("last_command", "None"),
		})},
		{component: "sensor", key: "fault", match: faultKinds, config: with(diagnostic, map[string]any{
			"name":           "Fault",
			"icon":           "mdi:alert-circle-outline",
			"value_template": textTemplate("fault", "None"),
		})},
		{component: "binary_sensor", key: "comm_reporting", match: tcOnly, config: binarySensor("Communication reporting", "comm_reporting", "",
			with(diagnostic, map[string]any{"icon": "mdi:access-point"}))},
		{component: "sensor", key: "auth_status", match: aesKinds, config: with(diagnostic, map[string]any{
			"name":           "AES authentication",
			"icon":           "mdi:shield-key-outline",
			"value_template": textTemplate("auth_status", "None"),
		})},
		{component: "sensor", key: "last_seen", match: allKinds, config: with(diagnostic, map[string]any{
			"name":           "Last seen",
			"device_class":   "timestamp",
			"value_template": textTemplate("last_seen", "None"),
		})},
		{component: "sensor", key: "rssi", match: allKinds, config: numSensor("RSSI", "rssi", "signal_strength", "dBm", with(diagnostic, disabled))},
		{component: "sensor", key: "messages_received", match: allKinds, config: with(diagnostic, disabled, map[string]any{
			"name":           "Messages received",
			"state_class":    "total_increasing",
			"icon":           "mdi:counter",
			"value_template": numTemplate("messages_received"),
		})},
	}
	list = append(list, pairedEntities()...)
	list = append(list, actuatorEntities()...)
	list = append(list, buttonEntities()...)
	return append(list, ledEntities()...)
}

// pairedEntities: for devices paired with hmbridge, an “Unpair” button
// (diagnostic, disabled by default, so that it is not pressed by
// accident) and a “Name” text entity to rename the device.
func pairedEntities() []entity {
	paired := func(d DeviceDesc) bool { return d.Paired }
	return []entity{
		{component: "button", key: "unpair", match: paired, command: "unpair", noState: true, config: map[string]any{
			"name":               "Unpair",
			"icon":               "mdi:link-variant-remove",
			"entity_category":    "diagnostic",
			"enabled_by_default": false,
			"payload_press":      "PRESS",
			"qos":                1,
		}},
		{component: "text", key: "name", match: paired, command: "name", stateSuffix: NameTopicSuffix, config: map[string]any{
			"name":            "Name",
			"icon":            "mdi:rename-outline",
			"entity_category": "config",
			"min":             1,
			"max":             MaxNameLength,
			"mode":            "text",
			"qos":             1,
		}},
	}
}

// actuatorEntities: the switch plug's switch and the dimmer's light.
func actuatorEntities() []entity {
	return []entity{
		{component: "switch", key: "switch", match: kinds(Switch), command: "switch", config: map[string]any{
			"name":           nil, // entity takes the device name
			"value_template": boolTemplate("on"),
			"payload_on":     "ON",
			"payload_off":    "OFF",
			"state_on":       "ON",
			"state_off":      "OFF",
			"optimistic":     false,
			"qos":            1,
		}},
		// Template schema: the state comes from the shared state topic,
		// and the command carries brightness and transition as JSON.
		{component: "light", key: "light", match: kinds(Dimmer), command: "light", config: map[string]any{
			"name":                 nil,
			"schema":               "template",
			"command_on_template":  `{"state": "on"{% if brightness is defined %}, "brightness": {{ brightness }}{% endif %}{% if transition is defined %}, "transition": {{ transition }}{% endif %}}`,
			"command_off_template": `{"state": "off"{% if transition is defined %}, "transition": {{ transition }}{% endif %}}`,
			"state_template":       "{{ ('on' if value_json.on else 'off') if value_json.on is defined else 'None' }}",
			"brightness_template":  "{{ (value_json.level * 2.55) | round(0) | int if value_json.level is defined else '' }}",
			"optimistic":           false,
			"qos":                  1,
		}},
	}
}

// MaxButtons is the largest number of buttons of a supported remote.
const MaxButtons = 8

// Device trigger types per press type (HA's names for button triggers).
var triggerTypes = map[string]string{
	PressShort:       "button_short_press",
	PressLong:        "button_long_press",
	PressLongRelease: "button_long_release",
}

// buttonEntities: per button an event entity and a device trigger per
// press type, all reading the button's event topic.
func buttonEntities() []entity {
	var list []entity
	remoteScope := kinds(Remote)
	for n := 1; n <= MaxButtons; n++ {
		n := n
		has := func(d DeviceDesc) bool { return d.Kind == Remote && Buttons(d.Model) >= n }
		list = append(list, entity{
			component: "event", key: fmt.Sprintf("button_%d", n), match: has, button: n, staleScope: remoteScope,
			config: map[string]any{
				"name":         fmt.Sprintf("Button %d", n),
				"device_class": "button",
				"event_types":  PressTypes,
				"qos":          1,
			},
		})
		for _, pt := range PressTypes {
			list = append(list, entity{
				component: "device_automation", key: fmt.Sprintf("button_%d_%s", n, pt), match: has, button: n, staleScope: remoteScope,
				config: map[string]any{
					"automation_type": "trigger",
					"type":            triggerTypes[pt],
					"subtype":         fmt.Sprintf("button_%d", n),
					"payload":         pt,
					"value_template":  "{{ value_json.event_type }}",
					"qos":             1,
				},
			})
		}
	}
	return list
}

// ledEntities: one select per LED of the HM-OU-LED16.
func ledEntities() []entity {
	var list []entity
	for n := 1; n <= LEDCount; n++ {
		i := n - 1
		list = append(list, entity{
			component: "select", key: fmt.Sprintf("led_%d", n), match: kinds(LEDDisplay),
			command: fmt.Sprintf("led_%d", n), staleScope: kinds(LEDDisplay),
			config: map[string]any{
				"name":           fmt.Sprintf("LED %d", n),
				"options":        LEDColors,
				"icon":           "mdi:led-on",
				"value_template": fmt.Sprintf("{{ value_json.leds[%[1]d] if value_json.leds is defined and value_json.leds|length > %[1]d and value_json.leds[%[1]d] else 'None' }}", i),
				"optimistic":     false,
				"qos":            1,
			},
		})
	}
	return list
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
	// eventTopic returns a button's event topic (nil for groups).
	eventTopic func(button int) string
	// topic returns <base>/<addr>/<suffix> (nil for groups).
	topic func(suffix string) string
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
		eventTopic:  func(button int) string { return cfg.EventTopic(addr, button) },
		topic:       func(suffix string) string { return cfg.base() + "/" + addr + "/" + suffix },
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
var bridgeAvailabilityOnly = map[string]bool{"last_seen": true, "auth_status": true, "unpair": true, "name": true}

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
	m := with(e.config) // copy: configs may be shared
	topic := entityTopic(cfg, e.component, n.id, e.key)
	if e.component == "device_automation" {
		// A device trigger is no entity: no unique_id or availability.
		m["topic"] = n.eventTopic(e.button)
		m["device"] = n.device
		return retained(topic, mustJSON(m))
	}
	switch {
	case e.noState:
	case e.button > 0:
		m["state_topic"] = n.eventTopic(e.button)
	case e.stateSuffix != "" && n.topic != nil:
		m["state_topic"] = n.topic(e.stateSuffix)
	default:
		m["state_topic"] = n.stateTopic
	}
	if e.command != "" {
		m["command_topic"] = n.cmdTopic(e.command)
	}
	common(cfg, n, e.component, n.id+"_"+e.key, e.key, m)
	return retained(topic, mustJSON(m))
}

// DiscoveryMessages returns the retained HA MQTT discovery config messages
// for d: a climate entity (thermostats only) plus the entities (sensors,
// binary sensors, switch, light, selects, events) and device triggers for
// d.Kind / d.Model.
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
	return append(msgs, retained(cfg.StateTopic(addr), nil), retained(cfg.DeviceAvailabilityTopic(addr), nil),
		retained(cfg.NameTopic(addr), nil))
}

// StaleDiscoveryMessages returns removal messages for the entities that
// exist for some Kind/Model but not for d (useful when a device's model was
// corrected in the configuration). Publishing them is optional. The
// per-button and per-LED entities of other kinds are left out (see
// entity.staleScope); AllStaleDiscoveryMessages includes them.
func StaleDiscoveryMessages(cfg TopicConfig, d DeviceDesc) []Message {
	return staleMessages(cfg, d, true)
}

// AllStaleDiscoveryMessages is StaleDiscoveryMessages including the
// entity sets of all other kinds (8 buttons' events and triggers, 16 LED
// selects), so that a device whose kind changed (e.g. a remote
// reconfigured as a switch) loses all of its old entities. Meant for
// once per start, not every reconnect.
func AllStaleDiscoveryMessages(cfg TopicConfig, d DeviceDesc) []Message {
	return staleMessages(cfg, d, false)
}

func staleMessages(cfg TopicConfig, d DeviceDesc, scoped bool) []Message {
	id := nodeID(d.Addr)
	var msgs []Message
	if !thermostats(d) {
		msgs = append(msgs, retained(climateTopic(cfg, id), nil))
	}
	for _, e := range entities() {
		if !e.match(d) && (!scoped || e.staleScope == nil || e.staleScope(d)) {
			msgs = append(msgs, retained(entityTopic(cfg, e.component, id, e.key), nil))
		}
	}
	return msgs
}

func groupWindowEntity() entity {
	return entity{component: "binary_sensor", key: "window_open", config: binarySensor("Window", "window_open", "window", nil)}
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
