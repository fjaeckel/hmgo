// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Device settings: the settings of a device's MASTER parameter set
// (temperature offset, button lock, …) appear as number, select, switch
// and text entities with entity_category "config". The bridge describes
// them per device (DeviceDesc.Settings); their values, and the status
// of reading and writing them, are published as one retained JSON
// SettingsState on <base>/<addr>/settings. Changes arrive on
// <base>/<addr>/set/setting_<key>; <base>/<addr>/set/settings_refresh
// reads the settings from the device again.

// SettingsTopicSuffix is the topic suffix of a device's settings.
const SettingsTopicSuffix = "settings"

// Setting entity components.
const (
	SettingNumber = "number"
	SettingSelect = "select"
	SettingSwitch = "switch"
	SettingText   = "text"
)

// SettingDesc describes one setting entity.
type SettingDesc struct {
	Key       string // [a-z0-9_]{1,64}, e.g. "temperature_offset"
	Name      string // e.g. "Temperature offset"
	Component string // SettingNumber, SettingSelect, SettingSwitch, SettingText
	Unit      string
	// Number: range and step.
	Min, Max, Step float64
	// Select: options.
	Options []string
	Icon    string
	// Enabled: enabled by default in Home Assistant.
	Enabled bool
	// MaxLength: text entities (default 255).
	MaxLength int
}

// SettingsTopic is the retained topic of a device's settings.
func (c TopicConfig) SettingsTopic(addr string) string {
	return c.base() + "/" + strings.ToLower(addr) + "/" + SettingsTopicSuffix
}

// Settings status values (SettingsState.Status starts with one of
// them; per-setting Detail values are "ok", "pending", "writing",
// "failed: …" or "invalid: …", a value on the device that hmbridge
// shows but would never write).
const (
	SettingsOK      = "ok"
	SettingsPending = "pending"
	SettingsWriting = "writing"
	SettingsReading = "reading"
	SettingsFailed  = "failed"
	SettingsUnread  = "not read"
	SettingsInvalid = "invalid"
)

// SettingsState is the published settings state of a device.
type SettingsState struct {
	// Current holds the values last read from (or verified on) the
	// device by setting key: float64 (number), string (select, text),
	// "ON"/"OFF" (switch). Unknown settings are missing.
	Current map[string]any `json:"current"`
	// Status summarises reading and writing, e.g. "ok", "pending: 1
	// change waits for the device (press its config button)",
	// "failed: temperature_offset (no response from device)".
	Status string `json:"status"`
	// Detail is the status per setting that was changed ("ok",
	// "pending", "writing", "failed: …").
	Detail map[string]string `json:"detail,omitempty"`
	// ReadAt is when the settings were last read from the device.
	ReadAt time.Time `json:"-"`
}

// MarshalJSON adds read_at (RFC 3339) when known.
func (s SettingsState) MarshalJSON() ([]byte, error) {
	type plain SettingsState
	v := struct {
		plain
		ReadAt string `json:"read_at,omitempty"`
	}{plain: plain(s)}
	if v.Current == nil {
		v.Current = map[string]any{}
	}
	if !s.ReadAt.IsZero() {
		v.ReadAt = s.ReadAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal(v)
}

// SettingsMessage returns the retained settings message of a device.
func SettingsMessage(cfg TopicConfig, addr string, s SettingsState) (Message, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return Message{}, err
	}
	return retained(cfg.SettingsTopic(addr), b), nil
}

// settingKeyRE is the syntax of setting keys (topic and unique_id safe).
var settingKeyRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ValidSettingKey reports whether k can be a setting key.
func ValidSettingKey(k string) bool { return settingKeyRE.MatchString(k) }

// settingEntityKey is the entity key (unique_id suffix) of a setting.
func settingEntityKey(key string) string { return "setting_" + key }

// MaxSettingValue is the longest accepted setting payload (a week
// profile day is at most 154 characters; the whole week as JSON more).
const MaxSettingValue = 2048

func settingTemplate(key, missing string) string {
	return fmt.Sprintf("{{ value_json.current['%[1]s'] if value_json.current is defined and '%[1]s' in value_json.current else '%[2]s' }}", key, missing)
}

// settingEntity returns the entity of a setting.
func settingEntity(s SettingDesc) entity {
	m := map[string]any{
		"name":            s.Name,
		"entity_category": "config",
		"qos":             1,
	}
	if s.Icon != "" {
		m["icon"] = s.Icon
	}
	if !s.Enabled {
		m["enabled_by_default"] = false
	}
	switch s.Component {
	case SettingNumber:
		m["min"], m["max"], m["step"] = s.Min, s.Max, s.Step
		m["mode"] = "box"
		m["value_template"] = settingTemplate(s.Key, "")
		if s.Unit != "" {
			m["unit_of_measurement"] = s.Unit
		}
	case SettingSelect:
		m["options"] = s.Options
		m["value_template"] = settingTemplate(s.Key, "None")
	case SettingSwitch:
		m["value_template"] = fmt.Sprintf("{{ value_json.current['%[1]s'] if value_json.current is defined and '%[1]s' in value_json.current else 'None' }}", s.Key)
		m["payload_on"], m["payload_off"] = "ON", "OFF"
		m["state_on"], m["state_off"] = "ON", "OFF"
	case SettingText:
		max := s.MaxLength
		if max <= 0 || max > 255 {
			max = 255
		}
		m["min"], m["max"], m["mode"] = 0, max, "text"
		m["value_template"] = settingTemplate(s.Key, "")
	}
	if s.Component != SettingText {
		m["optimistic"] = false
	}
	k := settingEntityKey(s.Key)
	return entity{component: s.Component, key: k, command: k, stateSuffix: SettingsTopicSuffix, config: m, bridgeOnly: true,
		match: func(DeviceDesc) bool { return true }}
}

// settingsEntities: the “Settings” status sensor and the “Refresh
// settings” button of devices with settings.
func settingsEntities() []entity {
	has := func(d DeviceDesc) bool { return len(d.Settings) > 0 }
	return []entity{
		{component: "sensor", key: "settings_status", match: has, stateSuffix: SettingsTopicSuffix, attrSuffix: SettingsTopicSuffix,
			bridgeOnly: true, config: map[string]any{
				"name":            "Settings",
				"icon":            "mdi:cog-sync-outline",
				"entity_category": "diagnostic",
				"value_template":  "{{ (value_json.status | truncate(250, true)) if value_json.status is defined else 'None' }}",
				"json_attributes_template": "{{ {'detail': value_json.detail if value_json.detail is defined else {}, " +
					"'read_at': value_json.read_at if value_json.read_at is defined else None} | tojson }}",
			}},
		{component: "button", key: "settings_refresh", match: has, command: "settings_refresh", noState: true, bridgeOnly: true,
			config: map[string]any{
				"name":            "Refresh settings",
				"icon":            "mdi:cog-refresh-outline",
				"entity_category": "config",
				"payload_press":   "PRESS",
				"qos":             1,
			}},
	}
}

// SettingsRemovalMessages removes the setting entities (of every setting
// in d.AllSettings and d.Settings) and the retained settings of a
// device that was removed.
func SettingsRemovalMessages(cfg TopicConfig, d DeviceDesc) []Message {
	id := nodeID(d.Addr)
	var msgs []Message
	seen := map[string]bool{}
	for _, s := range append(append([]SettingDesc{}, d.Settings...), d.AllSettings...) {
		e := settingEntity(s)
		t := entityTopic(cfg, e.component, id, e.key)
		if !seen[t] {
			seen[t] = true
			msgs = append(msgs, retained(t, nil))
		}
	}
	return append(msgs, retained(cfg.SettingsTopic(d.Addr), nil))
}
