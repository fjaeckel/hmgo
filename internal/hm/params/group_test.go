// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package params

import (
	"strings"
	"testing"
)

func groupKeysOf(list []GroupSetting) []string {
	var keys []string
	for _, gs := range list {
		keys = append(keys, gs.Key)
	}
	return keys
}

func TestForGroup(t *testing.T) {
	days := "week_profile_monday week_profile_tuesday week_profile_wednesday week_profile_thursday week_profile_friday week_profile_saturday week_profile_sunday"
	for _, tc := range []struct {
		models []string
		want   string
	}{
		{nil, ""},
		{[]string{"HM-Sec-SCo"}, ""},
		{[]string{"HM-CC-RT-DN"}, "comfort_temperature eco_temperature window_open_temperature window_open_duration boost_duration min_temperature max_temperature " + days},
		{[]string{"HM-TC-IT-WM-W-EU"}, "comfort_temperature eco_temperature boost_duration min_temperature max_temperature " + days},
		{[]string{"HM-CC-RT-DN", "HM-TC-IT-WM-W-EU", "HM-CC-RT-DN"}, "comfort_temperature eco_temperature window_open_temperature window_open_duration boost_duration min_temperature max_temperature " + days},
	} {
		list := ForGroup(tc.models)
		if got := strings.Join(groupKeysOf(list), " "); got != tc.want {
			t.Errorf("ForGroup(%v) = %s\nwant %s", tc.models, got, tc.want)
		}
	}

	// A group of both models: the window open settings apply to the
	// radiator thermostats only, and say so.
	list := ForGroup([]string{"HM-TC-IT-WM-W-EU", "HM-CC-RT-DN"})
	byKey := map[string]GroupSetting{}
	for _, gs := range list {
		byKey[gs.Key] = gs
	}
	w := byKey["window_open_temperature"]
	if !w.Partial || w.Name != "Window open temperature (radiator thermostats only)" || !w.HasModel("HM-CC-RT-DN") || w.HasModel("HM-TC-IT-WM-W-EU") {
		t.Errorf("window_open_temperature = %+v", w)
	}
	c := byKey["comfort_temperature"]
	if c.Partial || c.Name != "Comfort temperature" || len(c.Models) != 2 || c.Min != 15 || c.Max != 30 || c.Step != 0.5 || !c.Enabled {
		t.Errorf("comfort_temperature = %+v", c)
	}
	if err := c.Validate("21.5"); err != nil {
		t.Errorf("Validate(21.5): %v", err)
	}
	for _, bad := range []string{"14.5", "30.5", "21.3", "x"} {
		if err := c.Validate(bad); err == nil {
			t.Errorf("Validate(%s) accepted", bad)
		}
	}
	// Only RT-DN: not partial.
	if w := ForGroup([]string{"HM-CC-RT-DN"})[2]; w.Key != "window_open_temperature" || w.Partial || w.Name != "Window open temperature" {
		t.Errorf("RT-DN only: %+v", w)
	}
	// The catalog covers every group's settings.
	cat := map[string]bool{}
	for _, gs := range GroupCatalog() {
		cat[gs.Key] = true
	}
	for _, gs := range list {
		if !cat[gs.Key] {
			t.Errorf("GroupCatalog misses %s", gs.Key)
		}
	}
}

// TestGroupConversionPerModel: where the models' descriptions differ,
// the group offers the intersection of the ranges (and the coarser
// step), and every member converts the value with its own description
// (another register, another scale).
func TestGroupConversionPerModel(t *testing.T) {
	a := &Model{Name: "A", Settings: []Setting{
		{Key: "comfort_temperature", Name: "Comfort", Kind: Number, Loc: List7, Field: Bits(1, 0, 6), Min: 15, Max: 30, Step: 0.5, Scale: 2, Unit: "°C"},
		{Key: "eco_temperature", Name: "Eco", Kind: Number, Loc: List7, Field: Bits(2, 0, 6), Min: 5, Max: 25, Step: 0.5, Scale: 2, Unit: "°C"},
		{Key: "boost_duration", Name: "Boost", Kind: Number, Loc: List7, Field: Bits(10, 5, 3), Min: 0, Max: 30, Step: 5, Scale: 0.2, Unit: "min"},
	}}
	b := &Model{Name: "B", Settings: []Setting{
		{Key: "comfort_temperature", Name: "Comfort", Kind: Number, Loc: List0, Field: Byte(40), Min: 10, Max: 25, Step: 1, Offset: 10, Scale: 1, Unit: "°C"},
		{Key: "eco_temperature", Name: "Eco", Kind: Select, Loc: List7, Field: Bits(2, 0, 2), Options: []Option{{"low", 0}}},
		{Key: "boost_duration", Name: "Boost", Kind: Number, Loc: List7, Field: Bits(10, 5, 3), Min: 0, Max: 30, Step: 5, Scale: 0.2, Unit: "s"},
	}}
	ms := []*Model{a, b}
	c, ok := mergeGroupSetting("comfort_temperature", ms)
	if !ok || c.Min != 15 || c.Max != 25 || c.Step != 1 || c.Partial {
		t.Fatalf("comfort = %+v, %v", c, ok)
	}
	if err := c.Validate("26"); err == nil {
		t.Error("26 °C accepted (outside B's range)")
	}
	// The same value, two encodings.
	ca, err := NewChange(a.Setting("comfort_temperature"), "21")
	if err != nil || ca.Raw != 42 {
		t.Errorf("A: %+v, %v", ca, err)
	}
	cb, err := NewChange(b.Setting("comfort_temperature"), "21")
	if err != nil || cb.Raw != 11 {
		t.Errorf("B: %+v, %v", cb, err)
	}
	// Different kinds or units: not a group setting.
	if _, ok := mergeGroupSetting("eco_temperature", ms); ok {
		t.Error("eco_temperature with different kinds merged")
	}
	if _, ok := mergeGroupSetting("boost_duration", ms); ok {
		t.Error("boost_duration with different units merged")
	}
	// Disjoint ranges: not a group setting.
	b.Settings[0].Min, b.Settings[0].Max = 31, 40
	if _, ok := mergeGroupSetting("comfort_temperature", ms); ok {
		t.Error("disjoint ranges merged")
	}
}
