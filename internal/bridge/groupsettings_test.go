// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm/params"
)

// newGroupBridge returns a bridge (not running) with the heating group
// "wohnzimmer" of testConfig: a wall thermostat and two radiator
// thermostats (and a contact).
func newGroupBridge(t *testing.T) (*Bridge, *group) {
	t.Helper()
	cfgJSON := strings.Replace(testConfig, `"state_file": ""`, fmt.Sprintf(`"state_dir": %q`, t.TempDir()), 1)
	cfg, err := config.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{Config: cfg, Radio: nopRadio{}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	g := b.bySlug["wohnzimmer"]
	if g == nil || g.settings == nil {
		t.Fatal("group without settings")
	}
	return b, g
}

// list7 returns a thermostat's list 7 with the comfort temperature
// comfort and the Monday profile monday.
func list7(comfort float64, monday string) params.Regs {
	r := params.Regs{1: byte(comfort * 2), 2: 34, 3: 9, 4: 61, 5: 24, 6: 3, 10: 1<<5 | 16}
	day, err := params.ParseDay(monday)
	if err != nil {
		panic(err)
	}
	for d := 0; d < 7; d++ {
		for a, v := range params.EncodeDay(day, params.ProfileBase, d) {
			r[a] = v
		}
	}
	return r
}

func TestGroupSettingsDescription(t *testing.T) {
	b, g := newGroupBridge(t)
	gs := g.settings
	if len(gs.members) != 3 || gs.members[0] != b.byAddr[tc] {
		t.Fatalf("members = %v (wall thermostat first)", gs.members)
	}
	var names []string
	for _, s := range g.desc.Settings {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, "|"); !strings.Contains(got, "Window open temperature (radiator thermostats only)|Window open duration (radiator thermostats only)|Boost duration") ||
		!strings.HasPrefix(got, "Comfort temperature|Eco temperature|") || !strings.HasSuffix(got, "Week profile Sunday") {
		t.Errorf("settings = %s", got)
	}
	for _, s := range g.desc.Settings {
		if strings.Contains(s.Key, "offset") || strings.Contains(s.Key, "valve") || strings.Contains(s.Key, "lock") {
			t.Errorf("device setting %s offered on the group", s.Key)
		}
	}
	// The contact has settings, but no group settings: not a member.
	if isGroupSettingsMember(b.byAddr[sco]) || !isGroupSettingsMember(b.byAddr[rtdn2]) {
		t.Error("isGroupSettingsMember")
	}
}

func TestGroupSettingsAggregation(t *testing.T) {
	b, g := newGroupBridge(t)
	state := func() ha.GroupSettingsState {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.groupSettingsStateLocked(g)
	}
	set := func(addr [3]byte, r params.Regs) { b.byAddr[addr].settings.cache[params.List7] = r }
	const prof = "06:00 17.0, 22:00 21.0, 24:00 17.0"

	// Nothing read yet: unknown.
	st := state()
	if st.Consistency != ha.SettingsUnknown || len(st.Current) != 0 || len(st.Mixed) != 0 || len(st.Unknown) != len(g.settings.list) {
		t.Errorf("unread: %+v", st)
	}
	if v := st.Settings["comfort_temperature"]; v.Consistency != ha.SettingsUnknown || len(v.Members) != 3 || v.Members["Wohnzimmer Wand"] != nil {
		t.Errorf("unread comfort = %+v", v)
	}

	// Two of three read, the same: still unknown.
	set(tc, list7(21, prof))
	set(rtdn, list7(21, prof))
	st = state()
	if v := st.Settings["comfort_temperature"]; v.Consistency != ha.SettingsUnknown || v.Members["Wohnzimmer Wand"] != 21.0 || v.Members["Wohnzimmer Heizung 2"] != nil {
		t.Errorf("partly read comfort = %+v", v)
	}
	if _, ok := st.Current["comfort_temperature"]; ok {
		t.Error("partly read value shown")
	}

	// All the same: consistent, shown.
	set(rtdn2, list7(21, prof))
	st = state()
	if st.Consistency != ha.SettingsConsistent || st.Current["comfort_temperature"] != 21.0 || st.Current["week_profile_monday"] != prof || len(st.Unknown) != 0 {
		t.Errorf("consistent: %+v", st)
	}
	// The window open setting counts the radiator thermostats only.
	if v := st.Settings["window_open_temperature"]; v.Consistency != ha.SettingsConsistent || len(v.Members) != 2 || st.Current["window_open_temperature"] != 12.0 {
		t.Errorf("window open = %+v", v)
	}

	// One differs: mixed, not shown, listed.
	set(rtdn2, list7(22, "06:00 18.0, 24:00 17.0"))
	st = state()
	if st.Consistency != ha.SettingsMixed || fmt.Sprint(st.Mixed) != "[comfort_temperature week_profile_monday week_profile_tuesday week_profile_wednesday week_profile_thursday week_profile_friday week_profile_saturday week_profile_sunday]" {
		t.Errorf("mixed: %v %v", st.Consistency, st.Mixed)
	}
	if _, ok := st.Current["comfort_temperature"]; ok || st.Current["eco_temperature"] != 17.0 {
		t.Errorf("mixed current = %v", st.Current)
	}
	if v := st.Settings["comfort_temperature"]; v.Members["Wohnzimmer Heizung 2"] != 22.0 || v.Members["Wohnzimmer Heizung"] != 21.0 {
		t.Errorf("mixed members = %v", v.Members)
	}
	// Mixed wins over unknown.
	delete(b.byAddr[rtdn].settings.cache, params.List7)
	if st = state(); st.Consistency != ha.SettingsMixed || len(st.Unknown) == 0 {
		t.Errorf("mixed and unknown: %v %v", st.Consistency, st.Unknown)
	}
}

// groupCommand runs a group settings command on the (not running)
// bridge and returns the settings jobs queued, in order.
func groupCommand(b *Bridge, cmd ha.Command) []job {
	b.handleGroupSettingsCommand(cmd)
	var jobs []job
	for {
		j, ok := b.queue.pop()
		if !ok {
			return jobs
		}
		jobs = append(jobs, j)
	}
}

func TestGroupSettingsFanOut(t *testing.T) {
	b, g := newGroupBridge(t)

	// Comfort temperature: every thermostat, the wall thermostat first,
	// each with its model's encoding.
	jobs := groupCommand(b, ha.Command{Group: g.slug, Kind: ha.SetSetting, Setting: "comfort_temperature", Value: "21.5"})
	var order []string
	for _, j := range jobs {
		order = append(order, j.key)
		if j.kind != jobSettings || len(j.settings.changes) != 1 || j.settings.changes[0].Raw != 43 ||
			j.settings.changes[0].Setting != j.dev.settings.model.Setting("comfort_temperature") {
			t.Errorf("%s: %+v", j.key, j.settings)
		}
	}
	if got := strings.Join(order, " "); got != "settings:2a1b3c settings:38f59c settings:38f5a1" {
		t.Errorf("order = %s", got)
	}
	for _, a := range [][3]byte{tc, rtdn, rtdn2} {
		if _, ok := b.byAddr[a].settings.pending["comfort_temperature"]; !ok {
			t.Errorf("%x: not pending", a)
		}
	}

	// Window open temperature: radiator thermostats only.
	jobs = groupCommand(b, ha.Command{Group: g.slug, Kind: ha.SetSetting, Setting: "window_open_temperature", Value: "10"})
	if len(jobs) != 2 || jobs[0].key != "settings:38f59c" || jobs[1].key != "settings:38f5a1" {
		t.Errorf("window open jobs = %v", jobs)
	}
	b.mu.Lock()
	_, detail := g.settings.last.status(g.settings)
	b.mu.Unlock()
	if detail["Wohnzimmer Wand"] != groupNotApplicable {
		t.Errorf("detail = %v", detail)
	}

	// Week profile JSON: all members, both days.
	jobs = groupCommand(b, ha.Command{Group: g.slug, Kind: ha.SetSetting, Setting: weekProfileKey,
		Value: `{"monday": "07:00 18.0, 24:00 20.0", "Tuesday": "24:00 19.0"}`})
	if len(jobs) != 3 || jobs[0].key != "settings:2a1b3c" {
		t.Fatalf("week profile jobs = %v", jobs)
	}
	if m := g.settings.last.members[0]; len(m.changes) != 2 || g.settings.last.what != "week_profile (monday, tuesday)" {
		t.Errorf("week profile changes = %+v, %q", m.changes, g.settings.last.what)
	}

	// Invalid values and settings: rejected for all, nothing queued.
	for _, c := range []struct{ key, value, want string }{
		{"comfort_temperature", "35", "out of range"},
		{"comfort_temperature", "21.3", "not a multiple"},
		{"temperature_offset", "1", "no setting"},
		{"week_profile_monday", "07:00 18.0", "24:00"},
		{weekProfileKey, `{"monday": "24:00 18.0", "funday": "24:00 18.0"}`, "unknown day"},
	} {
		if jobs := groupCommand(b, ha.Command{Group: g.slug, Kind: ha.SetSetting, Setting: c.key, Value: c.value}); len(jobs) != 0 {
			t.Errorf("%s=%s queued %v", c.key, c.value, jobs)
		}
		b.mu.Lock()
		st := b.groupSettingsStateLocked(g).Status
		b.mu.Unlock()
		if !strings.HasPrefix(st, "failed: ") || !strings.Contains(st, c.want) {
			t.Errorf("%s=%s: status %q", c.key, c.value, st)
		}
	}

	// Refresh: all members.
	jobs = groupCommand(b, ha.Command{Group: g.slug, Kind: ha.RefreshSettings})
	if len(jobs) != 3 || !jobs[0].settings.refresh || jobs[0].key != "settings:2a1b3c" {
		t.Errorf("refresh jobs = %v", jobs)
	}
}

func TestGroupSettingsStatus(t *testing.T) {
	b, g := newGroupBridge(t)
	groupCommand(b, ha.Command{Group: g.slug, Kind: ha.SetSetting, Setting: "eco_temperature", Value: "16"})
	w := g.settings.last
	status := func() (string, map[string]string) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return w.status(g.settings)
	}
	note := func(i int, st string) {
		b.mu.Lock()
		defer b.mu.Unlock()
		m := w.members[i]
		b.noteGroupSettingLocked(m.dev, m.changes[0], st)
	}
	if st, d := status(); st != "pending: eco_temperature = 16 °C (0 of 3 thermostats confirmed)" || d["Wohnzimmer Wand"] != ha.SettingsWriting {
		t.Errorf("start: %q %v", st, d)
	}
	note(0, ha.SettingsOK)
	note(1, ha.SettingsOK)
	if st, _ := status(); st != "pending: eco_temperature = 16 °C (2 of 3 thermostats confirmed)" {
		t.Errorf("2 of 3: %q", st)
	}
	note(2, ha.SettingsOK)
	if st, d := status(); st != "ok: eco_temperature = 16 °C" || d["Wohnzimmer Heizung 2"] != ha.SettingsOK {
		t.Errorf("all ok: %q %v", st, d)
	}
	note(2, "failed: no response from device")
	if st, d := status(); st != "partial: eco_temperature = 16 °C (1 of 3 thermostats failed, see attributes)" ||
		d["Wohnzimmer Heizung 2"] != "failed: no response from device" {
		t.Errorf("partial: %q %v", st, d)
	}
	for i := range w.members {
		note(i, "failed: x")
	}
	if st, _ := status(); st != "failed: eco_temperature = 16 °C (see attributes)" {
		t.Errorf("all failed: %q", st)
	}
	// A result of another value (an older group change, a device
	// change) does not count.
	other, _ := params.NewChange(w.members[0].changes[0].Setting, "18")
	b.mu.Lock()
	b.noteGroupSettingLocked(w.members[0].dev, other, ha.SettingsOK)
	b.mu.Unlock()
	if _, d := status(); d["Wohnzimmer Wand"] != "failed: x" {
		t.Errorf("other value counted: %v", d)
	}

	// A device change of the same setting while the group change is on
	// its way supersedes it there (last write wins).
	groupCommand(b, ha.Command{Group: g.slug, Kind: ha.SetSetting, Setting: "eco_temperature", Value: "16.5"})
	w = g.settings.last
	b.handleSettingsCommand(ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "eco_temperature", Value: "15"})
	note(0, ha.SettingsOK)
	note(2, ha.SettingsOK)
	note(1, ha.SettingsOK) // would not arrive: the device job carries 15 °C
	if st, d := status(); st != "partial: eco_temperature = 16.5 °C (1 of 3 thermostats failed, see attributes)" || d["Wohnzimmer Heizung"] != groupSuperseded {
		t.Errorf("superseded: %q %v", st, d)
	}
	if c := b.byAddr[rtdn].settings.pending["eco_temperature"]; c.String() != "15 °C" {
		t.Errorf("pending on the device = %v", c)
	}
}

// TestGroupSettingsSingleThermostat: a group with one radiator thermostat
// (and a contact) offers the settings of that model, none partial.
func TestGroupSettingsSingleThermostat(t *testing.T) {
	cfg, err := config.Parse([]byte(fmt.Sprintf(`{
  "central_address": "1A2B3C",
  "reset": "none",
  "state_dir": %q,
  "security": {"passphrase": "secret", "index": 1},
  "mqtt": {"broker": ""},
  "devices": [
    {"name": "Bad Heizung", "model": "HM-CC-RT-DN", "address": "38F59C"},
    {"name": "Bad Fenster", "model": "HM-Sec-SCo", "address": "4C5D6E"}
  ],
  "groups": [{"name": "Bad", "members": ["Bad Heizung", "Bad Fenster"]}]
}`, t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{Config: cfg, Radio: nopRadio{}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	g := b.bySlug["bad"]
	if len(g.settings.members) != 1 || len(g.desc.Settings) != 14 {
		t.Fatalf("settings = %v", g.desc.Settings)
	}
	for _, s := range g.desc.Settings {
		if strings.Contains(s.Name, "only") {
			t.Errorf("partial setting %s in a single-model group", s.Name)
		}
	}
	b.byAddr[rtdn].settings.cache[params.List7] = list7(20, "24:00 18.0")
	b.mu.Lock()
	st := b.groupSettingsStateLocked(g)
	b.mu.Unlock()
	if st.Consistency != ha.SettingsConsistent || st.Current["comfort_temperature"] != 20.0 || st.Current["window_open_temperature"] != 12.0 {
		t.Errorf("state = %+v", st)
	}
	jobs := groupCommand(b, ha.Command{Group: "bad", Kind: ha.SetSetting, Setting: "window_open_duration", Value: "15"})
	if len(jobs) != 1 || jobs[0].settings.changes[0].Raw != 3 {
		t.Errorf("jobs = %v", jobs)
	}
}
