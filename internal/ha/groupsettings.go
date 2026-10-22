// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Heating group settings: settings the group's thermostats share
// (comfort/eco temperature, week profile, …, see GroupDesc.Settings)
// appear as config entities on the group's HA device, unique_id
// hmbridge_group_<slug>_setting_<key>. A change on
// <base>/group/<slug>/set/setting_<key> (set/setting_week_profile: JSON
// with several days) is written by the bridge to every thermostat member
// that has the setting; set/settings_refresh reads all members' settings.
// The values shown come from the members' settings: published retained
// as GroupSettingsState on <base>/group/<slug>/settings.

// Settings consistency of a group (GroupSettingsState.Consistency and
// GroupSettingValue.Consistency).
const (
	// SettingsConsistent: every member that has the setting holds the
	// same value.
	SettingsConsistent = "consistent"
	// SettingsMixed: the members hold different values.
	SettingsMixed = "mixed"
	// SettingsUnknown: the value of at least one member is not known
	// (settings not read yet) and the known values agree.
	SettingsUnknown = "unknown"
)

// GroupSettingsTopic is the retained topic of a heating group's
// GroupSettingsState.
func (c TopicConfig) GroupSettingsTopic(slug string) string {
	return c.base() + "/group/" + slug + "/" + SettingsTopicSuffix
}

// ParseGroupSettingsTopic returns the slug of a GroupSettingsTopic.
func (c TopicConfig) ParseGroupSettingsTopic(topic string) (slug string, ok bool) {
	rest, found := strings.CutPrefix(topic, c.base()+"/group/")
	if !found {
		return "", false
	}
	slug, found = strings.CutSuffix(rest, "/"+SettingsTopicSuffix)
	return slug, found && ValidSlug(slug)
}

// GroupSettingValue is one group setting across the members.
type GroupSettingValue struct {
	// Consistency is SettingsConsistent, SettingsMixed or
	// SettingsUnknown.
	Consistency string `json:"consistency"`
	// Members holds the value of each member that has the setting, by
	// member name (nil: not known).
	Members map[string]any `json:"members"`
}

// GroupSettingsState is the published settings state of a heating
// group.
type GroupSettingsState struct {
	// Current holds the settings all members agree on (same format as
	// SettingsState.Current); mixed and unknown ones are missing: a
	// number entity then shows unknown, a text entity "mixed" (or
	// nothing while unknown).
	Current map[string]any `json:"current"`
	// Settings has the per-member values of every group setting.
	Settings map[string]GroupSettingValue `json:"settings"`
	// Consistency summarises all settings: mixed if any is mixed, else
	// unknown if any is unknown, else consistent.
	Consistency string `json:"consistency"`
	// Mixed and Unknown list the keys that are mixed and unknown.
	Mixed   []string `json:"mixed"`
	Unknown []string `json:"unknown"`
	// Status is the outcome of the last group change ("ok: …",
	// "pending: …", "partial: …", "failed: …"; "" before the first).
	Status string `json:"status"`
	// Detail is the status of the last group change per member name.
	Detail map[string]string `json:"detail"`
}

// GroupSettingsMessage returns the retained settings message of a
// heating group.
func GroupSettingsMessage(cfg TopicConfig, slug string, s GroupSettingsState) (Message, error) {
	if s.Current == nil {
		s.Current = map[string]any{}
	}
	if s.Settings == nil {
		s.Settings = map[string]GroupSettingValue{}
	}
	if s.Mixed == nil {
		s.Mixed = []string{}
	}
	if s.Unknown == nil {
		s.Unknown = []string{}
	}
	if s.Detail == nil {
		s.Detail = map[string]string{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return Message{}, err
	}
	return retained(cfg.GroupSettingsTopic(slug), b), nil
}

// groupSettingEntity is the entity of a group setting: like a device
// setting, but a value the members do not agree on renders as unknown
// (numbers, selects, switches) or "mixed" (text), and the attributes
// list the members' values.
func groupSettingEntity(s SettingDesc) entity {
	e := settingEntity(s)
	e.config = with(e.config)
	k := s.Key
	have := fmt.Sprintf("value_json.current is defined and '%[1]s' in value_json.current", k)
	setting := fmt.Sprintf("(value_json.settings['%[1]s'] if value_json.settings is defined and '%[1]s' in value_json.settings else {})", k)
	switch s.Component {
	case SettingText:
		e.config["value_template"] = fmt.Sprintf("{{ value_json.current['%[1]s'] if %[2]s else ('%[3]s' if %[4]s.consistency is defined and %[4]s.consistency == '%[3]s' else '') }}",
			k, have, SettingsMixed, setting)
	default:
		// HA's MQTT number, select and switch take "None" as unknown.
		e.config["value_template"] = fmt.Sprintf("{{ value_json.current['%[1]s'] if %[2]s else 'None' }}", k, have)
	}
	e.attrSuffix = SettingsTopicSuffix
	e.config["json_attributes_template"] = fmt.Sprintf("{%% set s = %[1]s %%}{{ {'consistency': s.consistency if s.consistency is defined else '%[2]s', "+
		"'members': s.members if s.members is defined else {}} | tojson }}", setting, SettingsUnknown)
	return e
}

// groupSettingsEntities: the “Settings” status sensor (the last group
// change, per member in the attributes), the “Settings consistency”
// sensor and the “Refresh settings” button of a group with settings.
func groupSettingsEntities() []entity {
	all := func(DeviceDesc) bool { return true }
	return []entity{
		{component: "sensor", key: "settings_status", match: all, stateSuffix: SettingsTopicSuffix, attrSuffix: SettingsTopicSuffix,
			bridgeOnly: true, config: map[string]any{
				"name":                     "Settings",
				"icon":                     "mdi:cog-sync-outline",
				"entity_category":          "diagnostic",
				"value_template":           "{{ (value_json.status | truncate(250, true)) if value_json.status is defined and value_json.status else 'None' }}",
				"json_attributes_template": "{{ {'detail': value_json.detail if value_json.detail is defined else {}} | tojson }}",
			}},
		{component: "sensor", key: "settings_consistency", match: all, stateSuffix: SettingsTopicSuffix, attrSuffix: SettingsTopicSuffix,
			bridgeOnly: true, config: map[string]any{
				"name":            "Settings consistency",
				"icon":            "mdi:approximately-equal",
				"entity_category": "diagnostic",
				"device_class":    "enum",
				"options":         []string{SettingsConsistent, SettingsMixed, SettingsUnknown},
				"value_template":  "{{ value_json.consistency if value_json.consistency is defined else 'None' }}",
				"json_attributes_template": "{{ {'mixed': value_json.mixed if value_json.mixed is defined else [], " +
					"'unknown': value_json.unknown if value_json.unknown is defined else []} | tojson }}",
			}},
		{component: "button", key: "settings_refresh", match: all, command: "settings_refresh", noState: true, bridgeOnly: true,
			config: map[string]any{
				"name":            "Refresh settings",
				"icon":            "mdi:cog-refresh-outline",
				"entity_category": "config",
				"payload_press":   "PRESS",
				"qos":             1,
			}},
	}
}

// groupSettingsDiscovery returns the discovery messages of g's settings
// (none for a group without settings).
func groupSettingsDiscovery(cfg TopicConfig, n node, g GroupDesc) []Message {
	if len(g.Settings) == 0 {
		return nil
	}
	var msgs []Message
	for _, e := range groupSettingsEntities() {
		msgs = append(msgs, entityMessage(cfg, n, e))
	}
	for _, s := range g.Settings {
		msgs = append(msgs, entityMessage(cfg, n, groupSettingEntity(s)))
	}
	return msgs
}

// groupSettingsRemovalTopics returns the discovery topics of the
// settings entities of group slug (the fixed ones and those of
// settings) and its settings topic.
func groupSettingsRemovalTopics(cfg TopicConfig, slug string, settings []SettingDesc) []string {
	id := groupNodeID(slug)
	var topics []string
	for _, e := range groupSettingsEntities() {
		topics = append(topics, entityTopic(cfg, e.component, id, e.key))
	}
	for _, s := range settings {
		e := groupSettingEntity(s)
		topics = append(topics, entityTopic(cfg, e.component, id, e.key))
	}
	return append(topics, cfg.GroupSettingsTopic(slug))
}
