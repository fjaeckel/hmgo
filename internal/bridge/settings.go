// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

// Device settings (the MASTER parameter set: list 0 of the device,
// list 1 of the channels, list 7 of the thermostats), see package
// params for the models' settings and docs/DESIGN.md, “Device
// settings”.
//
// Reading: CONFIG_PARAM_REQ (cmd 0x01, ch 04 000000 00 list); the
// device answers with INFO_PARAM_RESPONSE frames (cmd 0x10 02/03), the
// first as the response to the request (returned by Send), the others
// as received frames, each acknowledged by the radio module (DualConn:
// in software). The worker collects them through paramCollector until
// the end marker.
//
// Writing: CONFIG_START, CONFIG_WRITE_INDEX (only the registers that
// change), CONFIG_END, sent through hm.StandardDevice.Send, so that the
// ACK of a write to an AES channel must be authenticated like any
// command's; then the list is read back and every change verified.
//
// Settings are read lazily: after pairing, when a change needs the
// other bits of a register that are not cached yet, and on “Refresh
// settings”, never periodically. The registers last read are cached
// per device (settings.json in the state directory, written when they
// change).
//
// Battery devices: the thermostats (wake-on-radio) get every message
// with the burst flag, like the climate commands. Contacts and buttons
// only listen after their config button was pressed (DEVICE_INFO) or
// when they announce that they listen (flag WAKEMEUP 0x02 in a message;
// FHEM CUL_HM processes its command stack then): changes wait for that
// (“pending” in Home Assistant) and are sent at once, as a priority job.
//
// Ownership: deviceSettings is changed on the event loop under b.mu
// (published from it under b.mu); jobs carry snapshots of what the
// worker needs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/pairing"
	"github.com/fjaeckel/hmgo/internal/hm/params"
	"github.com/fjaeckel/hmgo/internal/store"
)

const (
	// settingsJobTimeout bounds a settings job: reading a thermostat's
	// list 7 (about 26 frames), writing and reading it back, each
	// request with a wake-up burst.
	settingsJobTimeout = 2 * time.Minute
	// paramFrameTimeout is the longest gap between two frames of a
	// register read.
	paramFrameTimeout = 3 * time.Second
	// maxParamFrames bounds the frames of one register read (a list has
	// at most 255 registers, 8 per frame).
	maxParamFrames = 40
	// weekProfileKey is the pseudo setting that sets several days of a
	// week profile at once: a JSON object {"monday": "06:00 17.0, …"}.
	weekProfileKey = "week_profile"
)

var metricSettings = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "hmbridge", Name: "settings_operations_total",
	Help: "Device settings operations by op (read: a refresh of all settings, write: a changed setting) and result " +
		"(ok, failed, pending: waits for a battery device, rejected: invalid value, never sent).",
}, []string{"op", "result"})

func init() {
	prometheus.MustRegister(metricSettings)
	for _, op := range []string{"read", "write"} {
		for _, r := range []string{"ok", "failed", "pending", "rejected"} {
			metricSettings.WithLabelValues(op, r)
		}
	}
}

// deviceSettings is the settings state of one device.
type deviceSettings struct {
	model *params.Model
	rx    pairing.RxMode
	// cache holds the register lists last read from the device.
	cache  map[params.Loc]params.Regs
	readAt time.Time
	// pending are the changes the device has not confirmed yet (the
	// newest value per setting key).
	pending map[string]params.Change
	// refresh: all lists are to be read (cleared once a read of all of
	// them succeeded).
	refresh bool
	// detail is the status per changed setting ("pending", "writing",
	// "ok", "failed: …").
	detail map[string]string
	// readErr is the outcome of the last failed refresh ("" after a
	// successful one).
	readErr string
	// jobs counts the settings jobs queued or running.
	jobs int
}

func newDeviceSettings(model string) *deviceSettings {
	m := params.ForModel(model)
	if m == nil {
		return nil
	}
	rx := pairing.Always
	if pm, ok := pairing.ModelByName(m.Name); ok {
		rx = pm.RX
	}
	return &deviceSettings{
		model: m, rx: rx,
		cache:   make(map[params.Loc]params.Regs),
		pending: make(map[string]params.Change),
		detail:  make(map[string]string),
	}
}

// settingDesc converts a setting for Home Assistant discovery.
func settingDesc(s params.Setting) ha.SettingDesc {
	d := ha.SettingDesc{Key: s.Key, Name: s.Name, Unit: s.Unit, Icon: s.Icon, Enabled: s.Enabled}
	switch s.Kind {
	case params.Number:
		d.Component, d.Min, d.Max, d.Step = ha.SettingNumber, s.Min, s.Max, s.Step
	case params.Select:
		d.Component = ha.SettingSelect
		for _, o := range s.Options {
			d.Options = append(d.Options, o.Label)
		}
	case params.Switch:
		d.Component = ha.SettingSwitch
	case params.Profile:
		d.Component, d.MaxLength = ha.SettingText, 255
	}
	return d
}

func settingDescs(ss []params.Setting) []ha.SettingDesc {
	res := make([]ha.SettingDesc, len(ss))
	for i, s := range ss {
		res[i] = settingDesc(s)
	}
	return res
}

// allSettingDescs are the settings of all models (stale entity removal).
var allSettingDescs = settingDescs(params.Catalog())

// paramCollector receives the further frames of a register read (the
// worker runs one job at a time, so there is at most one).
type paramCollector struct {
	addr [3]byte
	ch   chan *bidcos.Packet
}

// settingsJob is a settings job: read lists and/or write changes.
type settingsJob struct {
	addr  [3]byte
	ds    *deviceSettings // identity only (not touched by the worker)
	model *params.Model
	flags byte
	aes   []byte
	// refresh: read all lists of the model.
	refresh bool
	// woke: a battery device that only listens now (no retries).
	woke    bool
	changes []params.Change
	cache   map[params.Loc]params.Regs
}

// settingsOutcome is the result of a settings job.
type settingsOutcome struct {
	read    map[params.Loc]params.Regs // lists read from the device
	readErr error                      // a list of the refresh could not be read
	results map[string]error           // per change: nil = written and verified
}

// cloneCache copies a register cache.
func cloneCache(c map[params.Loc]params.Regs) map[params.Loc]params.Regs {
	res := make(map[params.Loc]params.Regs, len(c))
	for l, r := range c {
		res[l] = r.Clone()
	}
	return res
}

// loadSettings fills the settings cache from the settings file (New).
func (b *Bridge) loadSettings() {
	recs, err := b.store.LoadSettings()
	if err != nil {
		b.logf("settings cache %s: %v (the settings are read from the devices again when needed)", b.store.SettingsPath(), err)
		return
	}
	for _, d := range b.devices {
		rec, ok := recs[d.desc.Addr]
		if d.settings == nil || !ok || !strings.EqualFold(rec.Model, d.cfg.Model) {
			continue
		}
		for _, l := range rec.Lists {
			if len(l.Regs) > 0 {
				d.settings.cache[params.Loc{Channel: l.Channel, List: l.List}] = params.Regs(l.Regs)
			}
		}
		d.settings.readAt = rec.ReadAt
	}
}

// settingsRecordsLocked snapshots the settings cache. Caller holds b.mu.
func (b *Bridge) settingsRecordsLocked() map[string]store.DeviceSettings {
	res := map[string]store.DeviceSettings{}
	for _, d := range b.devices {
		ds := d.settings
		if ds == nil || len(ds.cache) == 0 {
			continue
		}
		rec := store.DeviceSettings{Model: d.cfg.Model, ReadAt: ds.readAt}
		for l, r := range ds.cache {
			rec.Lists = append(rec.Lists, store.RegList{Channel: l.Channel, List: l.List, Regs: r.Clone()})
		}
		res[d.desc.Addr] = rec
	}
	return res
}

// saveSettings writes the settings cache if it changed (event loop,
// without b.mu: the write fsyncs).
func (b *Bridge) saveSettings() {
	if b.store.SettingsPath() == "" {
		return
	}
	b.mu.Lock()
	recs := b.settingsRecordsLocked()
	b.mu.Unlock()
	if err := b.store.SaveSettings(recs); err != nil {
		b.logf("saving %s: %v", b.store.SettingsPath(), err)
	}
}

// deviceByAddrHex returns the device with the lowercase hex address.
func (b *Bridge) deviceByAddrHex(addr string) *device {
	for _, d := range b.devices {
		if d.desc.Addr == addr {
			return d
		}
	}
	return nil
}

// handleSettingsCommand handles SetSetting and RefreshSettings (event
// loop).
func (b *Bridge) handleSettingsCommand(cmd ha.Command) {
	d := b.deviceByAddrHex(cmd.Addr)
	if d == nil || d.settings == nil {
		b.logf("settings command for %s ignored: unknown device or no settings", cmd.Addr)
		metricCommands.WithLabelValues("unknown_target", "rejected").Inc()
		return
	}
	defer b.publishDevice(d)
	b.mu.Lock()
	defer b.mu.Unlock()
	ds := d.settings
	switch cmd.Kind {
	case ha.RefreshSettings:
		ds.refresh = true
		b.logf("%s: reading its settings%s", d.cfg.Name, waitNote(ds))
	case ha.SetSetting:
		changes, err := parseSettingChanges(ds.model, cmd.Setting, cmd.Value)
		if err != nil {
			if cmd.Setting == weekProfileKey || ds.model.Setting(cmd.Setting) != nil {
				ds.detail[cmd.Setting] = "failed: " + err.Error()
			}
			b.logf("%s: setting %s rejected: %v", d.cfg.Name, cmd.Setting, err)
			metricSettings.WithLabelValues("write", "rejected").Inc()
			b.applyUpdate(d, ha.State{LastCommand: truncate(fmt.Sprintf("failed: setting %s (%v)", cmd.Setting, err))})
			b.publishSettingsLocked(d)
			return
		}
		for _, c := range changes {
			ds.pending[c.Setting.Key] = c
			ds.detail[c.Setting.Key] = ha.SettingsPending
			b.logf("%s: setting %s to %s%s", d.cfg.Name, c.Setting.Key, c, waitNote(ds))
		}
		if ds.rx == pairing.ConfigOnly {
			metricSettings.WithLabelValues("write", "pending").Add(float64(len(changes)))
			b.applyUpdate(d, ha.State{LastCommand: truncate(fmt.Sprintf("pending: setting %s (press the device's config button)", cmd.Setting))})
		}
	}
	b.scheduleSettingsLocked(d, false)
	b.publishSettingsLocked(d)
}

func waitNote(ds *deviceSettings) string {
	if ds.rx == pairing.ConfigOnly {
		return " when its config button is pressed"
	}
	return ""
}

// parseSettingChanges validates a SetSetting command.
func parseSettingChanges(m *params.Model, key, value string) ([]params.Change, error) {
	if key == weekProfileKey {
		return parseWeekProfile(m, value)
	}
	s := m.Setting(key)
	if s == nil {
		return nil, fmt.Errorf("%s has no setting %q", m.Name, key)
	}
	c, err := params.NewChange(s, value)
	if err != nil {
		return nil, err
	}
	return []params.Change{c}, nil
}

// parseWeekProfile parses {"monday": "06:00 17.0, …", …}: all days or
// none.
func parseWeekProfile(m *params.Model, value string) ([]params.Change, error) {
	var days map[string]string
	if err := json.Unmarshal([]byte(value), &days); err != nil {
		return nil, fmt.Errorf("week profile: want a JSON object {\"monday\": \"06:00 17.0, 24:00 21.0\", …}: %v", err)
	}
	if len(days) == 0 {
		return nil, errors.New("week profile: no days given")
	}
	var res []params.Change
	for day, text := range days {
		s := m.Setting("week_profile_" + strings.ToLower(day))
		if s == nil || s.Kind != params.Profile {
			return nil, fmt.Errorf("week profile: unknown day %q", day)
		}
		c, err := params.NewChange(s, text)
		if err != nil {
			return nil, err
		}
		res = append(res, c)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Setting.Key < res[j].Setting.Key })
	return res, nil
}

// settingsJobKey is the queue key (and retry target) of a device's
// settings jobs.
func settingsJobKey(d *device) string { return "settings:" + d.desc.Addr }

// scheduleSettingsLocked queues a settings job for d if there is
// something to do and the device listens (woke: a battery device that
// only listens in config mode has just announced that it does). It
// reports whether a job was queued. Caller holds b.mu.
func (b *Bridge) scheduleSettingsLocked(d *device, woke bool) bool {
	ds := d.settings
	if ds == nil || len(ds.pending) == 0 && !ds.refresh {
		return false
	}
	if ds.rx == pairing.ConfigOnly && !woke {
		return false
	}
	sj := &settingsJob{
		addr: d.cfg.Addr(), ds: ds, model: ds.model, aes: d.cfg.AESChannelList(),
		refresh: ds.refresh, woke: woke, flags: pairing.ConfigFlags, cache: cloneCache(ds.cache),
	}
	if ds.rx == pairing.WakeOnRadio {
		// Like the climate commands: a burst wakes the thermostat.
		sj.flags |= pairing.BurstFlag
	}
	keys := make([]string, 0, len(ds.pending))
	for k := range ds.pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sj.changes = append(sj.changes, ds.pending[k])
		ds.detail[k] = ha.SettingsWriting
	}
	key := settingsJobKey(d)
	if !b.queue.has(key) {
		ds.jobs++
	}
	b.queue.push(job{key: key, target: key, kind: jobSettings, dev: d, settings: sj, priority: woke})
	return true
}

// settingsWake sends what waits for a battery device that only listens
// in config mode, when it signals that it listens: DEVICE_INFO (config
// button) or a message with the WAKEMEUP flag. Event loop.
func (b *Bridge) settingsWake(d *device, pkt *bidcos.Packet) {
	ds := d.settings
	if ds == nil || ds.rx != pairing.ConfigOnly || pkt.Cmd != bidcos.DeviceInfo && pkt.Flags&bidcos.WakeMeUp == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ds.jobs > 0 {
		return // already on its way
	}
	if b.scheduleSettingsLocked(d, true) {
		what := fmt.Sprintf("sending %d settings changes", len(ds.pending))
		if ds.refresh {
			what += " and reading its settings"
		}
		b.logf("%s is awake: %s", d.cfg.Name, what)
		b.publishSettingsLocked(d)
	}
}

// forwardParamResponse hands a register read frame to the waiting
// worker. It reports whether one was waiting for this device.
func (b *Bridge) forwardParamResponse(pkt *bidcos.Packet) bool {
	b.paramMu.Lock()
	defer b.paramMu.Unlock()
	c := b.paramCol
	if c == nil || c.addr != pkt.Source {
		return false
	}
	select {
	case c.ch <- pkt:
	default: // a flood: the read fails by timeout or frame count
	}
	return true
}

func (b *Bridge) startParamCollect(addr [3]byte) *paramCollector {
	c := &paramCollector{addr: addr, ch: make(chan *bidcos.Packet, maxParamFrames)}
	b.paramMu.Lock()
	b.paramCol = c
	b.paramMu.Unlock()
	return c
}

func (b *Bridge) stopParamCollect(c *paramCollector) {
	b.paramMu.Lock()
	if b.paramCol == c {
		b.paramCol = nil
	}
	b.paramMu.Unlock()
}

// readList reads a register list from the device (worker goroutine).
func (b *Bridge) readList(ctx context.Context, sj *settingsJob, loc params.Loc) (params.Regs, error) {
	col := b.startParamCollect(sj.addr)
	defer b.stopParamCollect(col)
	pkt := &bidcos.Packet{Flags: sj.flags, Cmd: bidcos.Config, Dest: sj.addr, Payload: pairing.ConfigParamReq(loc.Channel, loc.List)}
	resp, err := b.bcs.Send(ctx, pkt)
	if err != nil {
		return nil, fmt.Errorf("reading %v: %w", loc, err)
	}
	switch {
	case resp == nil:
		return nil, fmt.Errorf("reading %v: %w", loc, hm.ErrNoResponse)
	case resp.Source != sj.addr:
		return nil, fmt.Errorf("reading %v: answer from %x", loc, resp.Source[:])
	case resp.Auth() == bidcos.AuthFailed:
		return nil, fmt.Errorf("reading %v: answer failed AES authentication", loc)
	case resp.Cmd == bidcos.Ack:
		if err := hm.CheckAck(sj.addr, resp); err != nil {
			return nil, fmt.Errorf("reading %v: %w", loc, err)
		}
		return nil, fmt.Errorf("reading %v: the device answered with an ACK instead of its registers", loc)
	case resp.Cmd != bidcos.Info || !params.IsParamResponse(resp.Payload):
		return nil, fmt.Errorf("reading %v: unexpected answer cmd %02x %x", loc, resp.Cmd, resp.Payload)
	}
	regs := params.Regs{}
	done, err := params.MergeResponse(regs, resp.Payload)
	frames := 1
	timer := time.NewTimer(b.paramTimeout)
	defer timer.Stop()
	for !done && err == nil {
		select {
		case p := <-col.ch:
			if frames++; frames > maxParamFrames {
				return nil, fmt.Errorf("reading %v: more than %d frames", loc, maxParamFrames)
			}
			done, err = params.MergeResponse(regs, p.Payload)
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(b.paramTimeout)
		case <-timer.C:
			return nil, fmt.Errorf("reading %v: %w (list incomplete after %d frames)", loc, hm.ErrNoResponse, frames)
		case <-ctx.Done():
			return nil, fmt.Errorf("reading %v: %w", loc, ctx.Err())
		}
	}
	if err != nil {
		return nil, fmt.Errorf("reading %v: %w", loc, err)
	}
	return regs, nil
}

// writeList writes registers to a list (worker goroutine). The ACKs go
// through hm.StandardDevice.Send: for an AES channel they must be
// authenticated.
func (b *Bridge) writeList(ctx context.Context, sj *settingsJob, loc params.Loc, regs []params.Reg) error {
	sd := hm.StandardDevice{BCS: b.bcs, Addr: sj.addr, AESChannels: sj.aes}
	pr := make([]pairing.Reg, len(regs))
	for i, r := range regs {
		pr[i] = pairing.Reg{Addr: r.Addr, Value: r.Value}
	}
	for _, m := range pairing.WriteListSequence(loc.Channel, loc.List, pr) {
		if _, err := sd.Send(ctx, &bidcos.Packet{Flags: sj.flags, Cmd: m.Cmd, Payload: m.Payload}); err != nil {
			return fmt.Errorf("%s: %w", m.What, err)
		}
	}
	return nil
}

// executeSettings runs a settings job (worker goroutine): per list,
// read it if needed (refresh, or a change needs the other bits of a
// shared register), write what changes, read it back and verify every
// change. The first communication failure ends the job; its error is
// returned (and decides about a retry).
func (b *Bridge) executeSettings(ctx context.Context, sj *settingsJob) (any, error) {
	out := &settingsOutcome{read: map[params.Loc]params.Regs{}, results: map[string]error{}}
	byLoc := map[params.Loc][]params.Change{}
	locSet := map[params.Loc]bool{}
	var locs []params.Loc
	add := func(l params.Loc) {
		if !locSet[l] {
			locSet[l] = true
			locs = append(locs, l)
		}
	}
	if sj.refresh {
		for _, l := range sj.model.Locs() {
			add(l)
		}
	}
	for _, c := range sj.changes {
		byLoc[c.Setting.Loc] = append(byLoc[c.Setting.Loc], c)
		add(c.Setting.Loc)
	}
	var commErr error
	fail := func(chs []params.Change, err error) {
		for _, c := range chs {
			out.results[c.Setting.Key] = err
		}
	}
	for _, loc := range locs {
		chs := byLoc[loc]
		if commErr != nil {
			fail(chs, commErr)
			if sj.refresh && out.readErr == nil {
				out.readErr = commErr
			}
			continue
		}
		regs := sj.cache[loc].Clone()
		need := sj.refresh
		for _, c := range chs {
			need = need || !c.Known(regs)
		}
		if need {
			r, err := b.readList(ctx, sj, loc)
			if err != nil {
				commErr = err
				if sj.refresh {
					out.readErr = err
				}
				fail(chs, err)
				continue
			}
			regs = r
			out.read[loc] = r.Clone()
		}
		if len(chs) == 0 {
			continue
		}
		if regs == nil {
			regs = params.Regs{}
		}
		want := regs.Clone()
		var applied []params.Change
		for _, c := range chs {
			if err := c.Apply(want); err != nil {
				out.results[c.Setting.Key] = err
				continue
			}
			applied = append(applied, c)
		}
		if len(applied) == 0 {
			continue
		}
		if diff := params.Diff(regs, want); len(diff) > 0 {
			if err := b.writeList(ctx, sj, loc, diff); err != nil {
				commErr = err
				fail(applied, err)
				continue
			}
		}
		back, err := b.readList(ctx, sj, loc)
		if err != nil {
			commErr = err
			fail(applied, fmt.Errorf("written, but reading it back failed: %w", err))
			continue
		}
		out.read[loc] = back
		for _, c := range applied {
			out.results[c.Setting.Key] = c.Verify(back)
		}
	}
	return out, commErr
}

// applyParamChange updates the cache from an INFO_PARAM_CHANGE the
// device sent by itself (a setting changed in its menu). Only lists
// that are cached are updated (a partial list is not a cache). Event
// loop; pkt passed the AES checks.
func (b *Bridge) applyParamChange(d *device, payload []byte) {
	ds := d.settings
	if ds == nil {
		return
	}
	loc, peer, regs, err := params.ParseParamChange(payload)
	if err != nil || peer != ([4]byte{}) || len(regs) == 0 {
		return
	}
	b.mu.Lock()
	cur := ds.cache[loc]
	if cur == nil {
		b.mu.Unlock()
		return
	}
	changed := false
	for a, v := range regs {
		if old, ok := cur[a]; !ok || old != v {
			cur[a] = v
			changed = true
		}
	}
	if changed {
		b.logf("%s reports changed settings (%v)", d.cfg.Name, loc)
		b.publishSettingsLocked(d)
	}
	b.mu.Unlock()
	if changed {
		b.saveSettings()
	}
}

// sameChange reports whether a and b set the same value.
func sameChange(a, b params.Change) bool {
	if a.Setting != b.Setting || a.Raw != b.Raw || len(a.Slots) != len(b.Slots) {
		return false
	}
	for i := range a.Slots {
		if a.Slots[i] != b.Slots[i] {
			return false
		}
	}
	return true
}

// handleSettingsResult applies the outcome of a settings job (event
// loop).
func (b *Bridge) handleSettingsResult(res jobResult) {
	sj := res.job.settings
	d := b.byAddr[sj.addr]
	if d == nil || d.settings != sj.ds {
		return // unpaired (or re-paired) meanwhile
	}
	save := false
	defer func() {
		if save {
			b.saveSettings()
		}
	}()
	defer b.publishDevice(d)
	b.mu.Lock()
	defer b.mu.Unlock()
	ds := d.settings
	if ds.jobs > 0 {
		ds.jobs--
	}
	out, _ := res.ev.(*settingsOutcome)
	if out == nil {
		out = &settingsOutcome{results: map[string]error{}, readErr: res.err}
	}
	for loc, regs := range out.read {
		if !reflect.DeepEqual(ds.cache[loc], regs) {
			// Written only when the registers change (read_at in the
			// file is the time of the last change).
			save = true
		}
		ds.cache[loc] = regs
	}
	if len(out.read) > 0 {
		ds.readAt = b.now()
	}
	retry := res.err != nil && isRetryable(res.err) && ds.rx != pairing.ConfigOnly && res.job.attempt < b.retries
	if sj.refresh {
		switch {
		case out.readErr == nil && res.err == nil:
			ds.refresh, ds.readErr = false, ""
			// A successful refresh starts afresh: old failures are history.
			for k, v := range ds.detail {
				if strings.HasPrefix(v, ha.SettingsFailed) {
					delete(ds.detail, k)
				}
			}
			metricSettings.WithLabelValues("read", "ok").Inc()
			b.logf("%s: settings read", d.cfg.Name)
		case retry:
		case ds.rx == pairing.ConfigOnly && isRetryable(errOr(out.readErr, res.err)):
			metricSettings.WithLabelValues("read", "pending").Inc()
			b.logf("%s: reading its settings failed (%v); trying again at the next config button press", d.cfg.Name, errOr(out.readErr, res.err))
		default:
			ds.refresh = false
			ds.readErr = errOr(out.readErr, res.err).Error()
			metricSettings.WithLabelValues("read", "failed").Inc()
			b.logf("%s: reading its settings failed: %v", d.cfg.Name, errOr(out.readErr, res.err))
		}
	}
	var last string
	for _, c := range sj.changes {
		key := c.Setting.Key
		cur, still := ds.pending[key]
		if !still || !sameChange(cur, c) {
			continue // a newer value is on its way; its job reports
		}
		err, attempted := out.results[key]
		if !attempted {
			err = errOr(res.err, errors.New("not attempted"))
		}
		switch {
		case err == nil:
			delete(ds.pending, key)
			ds.detail[key] = ha.SettingsOK
			metricSettings.WithLabelValues("write", "ok").Inc()
			b.logf("%s: setting %s = %s written and verified", d.cfg.Name, key, c)
			last = fmt.Sprintf("ok: setting %s = %s", key, c)
		case retry:
			ds.detail[key] = ha.SettingsPending
		case ds.rx == pairing.ConfigOnly && isRetryable(err):
			ds.detail[key] = ha.SettingsPending
			metricSettings.WithLabelValues("write", "pending").Inc()
			b.logf("%s: setting %s not confirmed (%v); sending it again at the next config button press", d.cfg.Name, key, err)
			last = fmt.Sprintf("pending: setting %s (press the device's config button)", key)
		default:
			delete(ds.pending, key)
			ds.detail[key] = "failed: " + err.Error()
			metricSettings.WithLabelValues("write", "failed").Inc()
			b.logf("%s: setting %s = %s failed: %v", d.cfg.Name, key, c, err)
			last = fmt.Sprintf("failed: setting %s (%v)", key, err)
		}
	}
	if retry {
		j := res.job
		j.attempt++
		b.logf("%s: settings job not acknowledged (%v), retrying in %v (attempt %d of %d)", d.cfg.Name, res.err, b.retryDelay, j.attempt, b.retries)
		time.AfterFunc(b.retryDelay, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.queue.retry(j) {
				ds.jobs++
			}
		})
	}
	if last != "" {
		b.applyUpdate(d, ha.State{LastCommand: truncate(last)})
	}
	b.publishSettingsLocked(d)
}

func errOr(err, def error) error {
	if err != nil {
		return err
	}
	return def
}

// truncate bounds a status text for Home Assistant (states have at
// most 255 characters).
func truncate(s string) string {
	const max = 250
	if len(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if i > max-3 {
			break
		}
		n = i
	}
	return s[:n] + "..."
}

// settingsStateLocked returns the published settings of d. Caller holds
// b.mu.
func (b *Bridge) settingsStateLocked(d *device) ha.SettingsState {
	ds := d.settings
	st := ha.SettingsState{Current: map[string]any{}, Detail: map[string]string{}, ReadAt: ds.readAt}
	for i := range ds.model.Settings {
		s := &ds.model.Settings[i]
		if r := ds.cache[s.Loc]; r != nil {
			if v, ok := s.Value(r); ok {
				st.Current[s.Key] = v
			}
		}
	}
	for k, v := range ds.detail {
		st.Detail[k] = v
	}
	st.Status = truncate(settingsSummary(ds))
	return st
}

// settingsSummary is the “Settings” sensor's state.
func settingsSummary(ds *deviceSettings) string {
	var failed []string
	for k, v := range ds.detail {
		if strings.HasPrefix(v, ha.SettingsFailed) {
			failed = append(failed, k)
		}
	}
	sort.Strings(failed)
	n := len(ds.pending)
	switch {
	case len(failed) > 0:
		return fmt.Sprintf("%s: %s (see attributes)", ha.SettingsFailed, strings.Join(failed, ", "))
	case ds.jobs > 0 && n > 0:
		return ha.SettingsWriting
	case ds.jobs > 0:
		return ha.SettingsReading
	case n > 0:
		return fmt.Sprintf("%s: %d %s waiting for the device (press its config button)", ha.SettingsPending, n, plural(n, "change", "changes"))
	case ds.refresh && ds.rx == pairing.ConfigOnly:
		return ha.SettingsPending + ": read at the next config button press"
	case ds.readErr != "":
		return ha.SettingsFailed + ": " + ds.readErr
	case len(ds.cache) == 0:
		return ha.SettingsUnread + " (press Refresh settings)"
	}
	return ha.SettingsOK
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// publishSettingsLocked queues the settings message of d. Caller holds
// b.mu.
func (b *Bridge) publishSettingsLocked(d *device) {
	if b.pub == nil || d.settings == nil || d.removed {
		return
	}
	if m, err := ha.SettingsMessage(b.pub.Topics(), d.desc.Addr, b.settingsStateLocked(d)); err == nil {
		b.outbox.put(m)
	} else {
		b.logf("encoding settings of %s: %v", d.cfg.Name, err)
	}
}
