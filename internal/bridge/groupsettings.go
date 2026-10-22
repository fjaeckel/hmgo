// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

// Heating group settings (OpenCCU's heating group parameters): the
// settings the group's thermostats share (params.ForGroup: comfort and
// eco temperature, window open temperature and duration, boost
// duration, minimum and maximum temperature, week profile program 1).
// A change on the group becomes, per thermostat that has the setting, a
// change through the device settings machinery (settings.go: pending
// change, settings job with burst, read before a write to shared
// registers, read-back verification), queued wall thermostats first,
// then radiator thermostats. Every member converts the value with its
// own model's description; the group validates against the
// intersection of the members' ranges first, so a value is written to
// all members or to none.
//
// The values shown on the group come from the members' cached
// settings (groupSettingsStateLocked): the value if all members agree,
// otherwise mixed or unknown, with the members' values in the
// attributes.
//
// Concurrency: a change on a member's own device entity and a group
// change of the same setting are independent writes; the last one
// written wins. A device change that replaces a group change still
// waiting for the device marks it “superseded” in the group's status.
//
// Ownership: groupSettings is changed on the event loop under b.mu.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm/params"
)

// Status of a member in a group change (besides the device's detail
// values pending, writing, ok, failed: …).
const (
	groupSuperseded    = "superseded by a change on the device"
	groupNotApplicable = "not applicable (no such setting)"
)

// groupSettings is the settings state of a heating group.
type groupSettings struct {
	list  []params.GroupSetting
	byKey map[string]*params.GroupSetting
	// members are the thermostats with settings, wall thermostats first
	// (the order of the writes).
	members []*device
	// last is the last group change (nil before the first).
	last *groupWrite
}

// groupWrite is a group change and its progress per member.
type groupWrite struct {
	what string
	// rejected: the value was invalid; nothing was sent.
	rejected error
	members  []*groupWriteMember
}

type groupWriteMember struct {
	dev     *device
	changes []params.Change
	// status per setting key: the device's detail value of the change
	// (pending, writing, ok, failed: …) or groupSuperseded; a member
	// without the setting has no changes and status groupNotApplicable.
	status map[string]string
}

// allGroupSettingDescs are the settings any group can have (stale
// entity removal).
var allGroupSettingDescs = groupSettingDescs(params.GroupCatalog())

func groupSettingDescs(list []params.GroupSetting) []ha.SettingDesc {
	res := make([]ha.SettingDesc, len(list))
	for i, gs := range list {
		res[i] = settingDesc(gs.Setting)
	}
	return res
}

// newGroupSettings returns the settings of g (nil if no thermostat of g
// has group settings).
func newGroupSettings(g *group) *groupSettings {
	gs := &groupSettings{byKey: map[string]*params.GroupSetting{}}
	var models []string
	for _, d := range g.thermostats {
		if d.settings != nil {
			gs.members = append(gs.members, d)
			models = append(models, d.settings.model.Name)
		}
	}
	// Wall thermostats first (config orders them so already).
	sort.SliceStable(gs.members, func(i, j int) bool {
		return gs.members[i].desc.Kind == ha.WallThermostat && gs.members[j].desc.Kind != ha.WallThermostat
	})
	gs.list = params.ForGroup(models)
	if len(gs.list) == 0 {
		return nil
	}
	for i := range gs.list {
		gs.byKey[gs.list[i].Key] = &gs.list[i]
	}
	return gs
}

// memberLabel is a member's name in the group's attributes (with its
// address if another member has the same name).
func (gs *groupSettings) memberLabel(d *device) string {
	for _, o := range gs.members {
		if o != d && o.cfg.Name == d.cfg.Name {
			return fmt.Sprintf("%s (%s)", d.cfg.Name, d.desc.Addr)
		}
	}
	return d.cfg.Name
}

// groupChanges validates a group SetSetting command and returns the
// changes per member (in write order; nil for members without the
// setting) and a description of the change.
func (gs *groupSettings) groupChanges(key, value string) ([][]params.Change, string, error) {
	var keys []string
	if key == weekProfileKey {
		var days map[string]string
		if err := json.Unmarshal([]byte(value), &days); err != nil {
			return nil, key, fmt.Errorf("week profile: want a JSON object {\"monday\": \"06:00 17.0, 24:00 21.0\", …}: %v", err)
		}
		if len(days) == 0 {
			return nil, key, errors.New("week profile: no days given")
		}
		for day, text := range days {
			s := gs.byKey["week_profile_"+strings.ToLower(day)]
			if s == nil {
				return nil, key, fmt.Errorf("week profile: unknown day %q", day)
			}
			if err := s.Validate(text); err != nil {
				return nil, key, err
			}
			keys = append(keys, strings.ToLower(day))
		}
		sort.Strings(keys)
	} else {
		s := gs.byKey[key]
		if s == nil {
			return nil, key, fmt.Errorf("the group has no setting %q", key)
		}
		if err := s.Validate(value); err != nil {
			return nil, key, err
		}
	}
	res := make([][]params.Change, len(gs.members))
	what := ""
	n := 0
	for i, d := range gs.members {
		applies := true
		if key != weekProfileKey {
			applies = gs.byKey[key].HasModel(d.settings.model.Name)
		}
		if !applies {
			continue
		}
		// Each member converts the value with its own model.
		chs, err := parseSettingChanges(d.settings.model, key, value)
		if err != nil {
			return nil, key, fmt.Errorf("%s: %v", d.cfg.Name, err)
		}
		res[i] = chs
		n++
		if what == "" {
			if key == weekProfileKey {
				what = fmt.Sprintf("%s (%s)", key, strings.Join(keys, ", "))
			} else {
				what = fmt.Sprintf("%s = %s", key, chs[0])
			}
		}
	}
	if n == 0 {
		return nil, key, fmt.Errorf("no thermostat of the group has the setting %q", key)
	}
	return res, what, nil
}

// handleGroupSettingsCommand handles SetSetting and RefreshSettings for
// a heating group (event loop).
func (b *Bridge) handleGroupSettingsCommand(cmd ha.Command) {
	g := b.bySlug[cmd.Group]
	if g == nil || g.settings == nil {
		b.logf("settings command for group %q ignored: unknown group or no settings", cmd.Group)
		metricCommands.WithLabelValues("unknown_target", "rejected").Inc()
		return
	}
	gs := g.settings
	b.mu.Lock()
	defer b.mu.Unlock()
	switch cmd.Kind {
	case ha.RefreshSettings:
		b.logf("group %s: reading the settings of its %d thermostats", g.name, len(gs.members))
		for _, d := range gs.members {
			d.settings.refresh = true
			b.scheduleSettingsLocked(d, false)
			b.publishSettingsLocked(d)
		}
	case ha.SetSetting:
		perMember, what, err := gs.groupChanges(cmd.Setting, cmd.Value)
		if err != nil {
			gs.last = &groupWrite{what: "setting " + what, rejected: err}
			b.logf("group %s: setting %s rejected: %v", g.name, cmd.Setting, err)
			metricSettings.WithLabelValues("write", "rejected").Inc()
			b.publishGroupSettingsLocked(g)
			return
		}
		w := &groupWrite{what: what}
		for i, d := range gs.members {
			m := &groupWriteMember{dev: d, changes: perMember[i], status: map[string]string{}}
			w.members = append(w.members, m)
			if len(m.changes) == 0 {
				continue
			}
			ds := d.settings
			for _, c := range m.changes {
				ds.pending[c.Setting.Key] = c
				ds.detail[c.Setting.Key] = ha.SettingsPending
			}
			b.logf("group %s: setting %s on %s", g.name, what, d.cfg.Name)
			b.scheduleSettingsLocked(d, false)
			for _, c := range m.changes {
				m.status[c.Setting.Key] = ds.detail[c.Setting.Key]
			}
		}
		// The newest group change is reported; results of an older one
		// still on its way no longer match (noteGroupSettingLocked).
		gs.last = w
		for _, d := range gs.members {
			b.publishSettingsLocked(d) // publishes the group's settings too
		}
	}
}

// lastMember returns d's record in the group's last change (nil if
// none).
func (gs *groupSettings) lastMember(d *device) *groupWriteMember {
	if gs == nil || gs.last == nil {
		return nil
	}
	for _, m := range gs.last.members {
		if m.dev == d {
			return m
		}
	}
	return nil
}

// noteGroupSettingLocked records the device status of change c of d in
// its group's last change, if c belongs to it. Caller holds b.mu.
func (b *Bridge) noteGroupSettingLocked(d *device, c params.Change, status string) {
	if d.group == nil {
		return
	}
	m := d.group.settings.lastMember(d)
	if m == nil {
		return
	}
	for _, own := range m.changes {
		if own.Setting.Key == c.Setting.Key && sameChange(own, c) {
			if m.status[own.Setting.Key] != groupSuperseded {
				m.status[own.Setting.Key] = status
			}
			return
		}
	}
}

// supersedeGroupSettingLocked marks the group change of key on d as
// superseded by a change on the device itself, if it has not
// completed yet (last write wins). Caller holds b.mu.
func (b *Bridge) supersedeGroupSettingLocked(d *device, key string) {
	if d.group == nil {
		return
	}
	m := d.group.settings.lastMember(d)
	if m == nil {
		return
	}
	if st, ok := m.status[key]; ok && (st == ha.SettingsPending || st == ha.SettingsWriting) {
		m.status[key] = groupSuperseded
	}
}

// memberStatus summarises a member's part of a group change.
func (m *groupWriteMember) memberStatus() string {
	if len(m.changes) == 0 {
		return groupNotApplicable
	}
	var failed, superseded, writing, pending bool
	var failure string
	for _, c := range m.changes {
		switch st := m.status[c.Setting.Key]; {
		case strings.HasPrefix(st, ha.SettingsFailed):
			if !failed {
				failure = st
			}
			failed = true
		case st == groupSuperseded:
			superseded = true
		case st == ha.SettingsWriting:
			writing = true
		case st == ha.SettingsOK:
		default:
			pending = true
		}
	}
	switch {
	case failed:
		return failure
	case superseded:
		return groupSuperseded
	case writing:
		return ha.SettingsWriting
	case pending:
		return ha.SettingsPending
	}
	return ha.SettingsOK
}

// status returns the group change's status and the per-member detail.
func (w *groupWrite) status(gs *groupSettings) (string, map[string]string) {
	detail := map[string]string{}
	if w.rejected != nil {
		return fmt.Sprintf("%s: %s (%v)", ha.SettingsFailed, w.what, w.rejected), detail
	}
	var total, ok, bad, busy int
	for _, m := range w.members {
		st := m.memberStatus()
		detail[gs.memberLabel(m.dev)] = st
		if st == groupNotApplicable {
			continue
		}
		total++
		switch {
		case st == ha.SettingsOK:
			ok++
		case st == ha.SettingsPending || st == ha.SettingsWriting:
			busy++
		default:
			bad++
		}
	}
	switch {
	case busy > 0:
		return fmt.Sprintf("%s: %s (%d of %d %s confirmed)", ha.SettingsPending, w.what, ok, total, plural(total, "thermostat", "thermostats")), detail
	case bad == 0:
		return ha.SettingsOK + ": " + w.what, detail
	case ok == 0:
		return fmt.Sprintf("%s: %s (see attributes)", ha.SettingsFailed, w.what), detail
	}
	return fmt.Sprintf("partial: %s (%d of %d %s failed, see attributes)", w.what, bad, total, plural(total, "thermostat", "thermostats")), detail
}

// values returns the values of setting s on the group's members that
// have it (by member label; nil: unknown), their consistency and, if
// consistent, the value.
func (gs *groupSettings) values(s *params.GroupSetting) (map[string]any, string, any) {
	vals := map[string]any{}
	var first any
	known, unknown, mixed := 0, 0, false
	for _, d := range gs.members {
		ds := d.settings
		if !s.HasModel(ds.model.Name) {
			continue
		}
		var v any
		if ms := ds.model.Setting(s.Key); ms != nil {
			if r := ds.cache[ms.Loc]; r != nil {
				if val, ok := ms.Value(r); ok {
					v = val
				}
			}
		}
		vals[gs.memberLabel(d)] = v
		if v == nil {
			unknown++
			continue
		}
		if known > 0 && v != first {
			mixed = true
		}
		if known == 0 {
			first = v
		}
		known++
	}
	switch {
	case mixed:
		return vals, ha.SettingsMixed, nil
	case unknown > 0 || known == 0:
		return vals, ha.SettingsUnknown, nil
	}
	return vals, ha.SettingsConsistent, first
}

// groupSettingsStateLocked returns the published settings of g. Caller
// holds b.mu.
func (b *Bridge) groupSettingsStateLocked(g *group) ha.GroupSettingsState {
	gs := g.settings
	st := ha.GroupSettingsState{
		Current:     map[string]any{},
		Settings:    map[string]ha.GroupSettingValue{},
		Consistency: ha.SettingsConsistent,
	}
	for i := range gs.list {
		s := &gs.list[i]
		vals, cons, v := gs.values(s)
		st.Settings[s.Key] = ha.GroupSettingValue{Consistency: cons, Members: vals}
		switch cons {
		case ha.SettingsConsistent:
			st.Current[s.Key] = v
		case ha.SettingsMixed:
			st.Mixed = append(st.Mixed, s.Key)
		default:
			st.Unknown = append(st.Unknown, s.Key)
		}
	}
	switch {
	case len(st.Mixed) > 0:
		st.Consistency = ha.SettingsMixed
	case len(st.Unknown) > 0:
		st.Consistency = ha.SettingsUnknown
	}
	if gs.last != nil {
		s, detail := gs.last.status(gs)
		st.Status, st.Detail = truncate(s), detail
	}
	return st
}

// publishGroupSettingsLocked queues the settings message of g. Caller
// holds b.mu.
func (b *Bridge) publishGroupSettingsLocked(g *group) {
	if b.pub == nil || g.settings == nil {
		return
	}
	if m, err := ha.GroupSettingsMessage(b.pub.Topics(), g.slug, b.groupSettingsStateLocked(g)); err == nil {
		b.outbox.put(m)
	} else {
		b.logf("encoding settings of group %s: %v", g.name, err)
	}
}

// isGroupSettingsMember reports whether d's settings feed its group's
// settings.
func isGroupSettingsMember(d *device) bool {
	if d.group == nil || d.group.settings == nil {
		return false
	}
	for _, m := range d.group.settings.members {
		if m == d {
			return true
		}
	}
	return false
}
