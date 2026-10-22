// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package params

import (
	"math"
	"sort"
	"strings"
)

// Heating group settings: like OpenCCU's heating group parameters, a
// group offers the settings its thermostats share with the same meaning,
// and a change is written to every member that has the setting (each
// member converts the value with its own model's description, see
// GroupSetting.Members). Only these keys are group settings; settings
// that describe one device (temperature offset, valve settings, display,
// button lock, …) stay device settings.
var groupKeys = []string{
	"comfort_temperature", "eco_temperature",
	"window_open_temperature", "window_open_duration",
	"boost_duration", "min_temperature", "max_temperature",
}

// GroupKeys returns the keys of the group settings in display order
// (the week profile days, Monday first, last).
func GroupKeys() []string {
	keys := append([]string(nil), groupKeys...)
	for _, s := range weekProfile("") {
		keys = append(keys, s.Key)
	}
	return keys
}

// modelPlural names the members of a model in a group setting's name.
var modelPlural = map[string]string{
	"HM-CC-RT-DN":      "radiator thermostats",
	"HM-TC-IT-WM-W-EU": "wall thermostats",
}

// GroupSetting is a setting of a heating group.
type GroupSetting struct {
	// Setting is the merged description: number ranges are the
	// intersection of the members' ranges (step: the largest step), the
	// name says which members it applies to when not all thermostat
	// models of the group have it. Field and Loc are those of the first
	// model and are never used to write: every member writes with its own
	// model's setting.
	Setting
	// Models are the member models that have the setting (sorted).
	Models []string
	// Partial: some thermostat models of the group lack the setting
	// (e.g. the wall thermostat has no window open settings); the change
	// goes to the members of Models only.
	Partial bool
}

// ForGroup returns the group settings of a heating group whose
// thermostats have the given models (duplicates allowed; models
// without settings are ignored), in the order of GroupKeys. A key is
// offered if at least one model has it and every model that has it
// agrees on its kind and unit, and the number ranges overlap.
func ForGroup(models []string) []GroupSetting {
	seen := map[string]bool{}
	var ms []*Model
	for _, name := range models {
		m := ForModel(name)
		if m == nil || seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		ms = append(ms, m)
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	var res []GroupSetting
	for _, key := range GroupKeys() {
		if gs, ok := mergeGroupSetting(key, ms); ok {
			res = append(res, gs)
		}
	}
	return res
}

func mergeGroupSetting(key string, ms []*Model) (GroupSetting, bool) {
	var gs GroupSetting
	var have []*Setting
	for _, m := range ms {
		if s := m.Setting(key); s != nil {
			have = append(have, s)
			gs.Models = append(gs.Models, m.Name)
		}
	}
	if len(have) == 0 {
		return gs, false
	}
	gs.Setting = *have[0]
	for _, s := range have[1:] {
		if s.Kind != gs.Kind || s.Unit != gs.Unit {
			return gs, false
		}
		if s.Kind == Number {
			gs.Min = math.Max(gs.Min, s.Min)
			gs.Max = math.Min(gs.Max, s.Max)
			gs.Step = math.Max(gs.Step, s.Step)
		}
	}
	if gs.Kind == Number && gs.Min > gs.Max+eps {
		return gs, false
	}
	gs.Partial = len(gs.Models) < len(ms)
	if gs.Partial {
		var who []string
		for _, m := range gs.Models {
			if p, ok := modelPlural[m]; ok {
				who = append(who, p)
			} else {
				who = append(who, m)
			}
		}
		gs.Name += " (" + strings.Join(who, ", ") + " only)"
	}
	gs.Enabled = true
	return gs, true
}

// Validate checks a group value against the merged description (the
// intersection of the members' ranges) before anything is sent.
func (gs *GroupSetting) Validate(value string) error {
	_, err := NewChange(&gs.Setting, value)
	return err
}

// HasModel reports whether members of model take the setting.
func (gs *GroupSetting) HasModel(model string) bool {
	for _, m := range gs.Models {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return false
}

// GroupCatalog returns every group setting a group of any combination of
// models can have (for removing the Home Assistant entities a group no
// longer has).
func GroupCatalog() []GroupSetting {
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name
	}
	return ForGroup(names)
}
