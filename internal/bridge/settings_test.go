// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm/params"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/store"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

const settingsConfig = `{
  "central_address": "1A2B3C",
  "reset": "none",
  "state_dir": %q,
  "require_synced_clock": false,
  "security": {"passphrase": "secret", "index": 1},
  "mqtt": {"broker": ""},
  "devices": [
    {"name": "Bad", "model": "HM-CC-RT-DN", "address": "38F59C", "aes": true},
    {"name": "Fenster", "model": "HM-Sec-SCo", "address": "4C5D6E", "aes": true},
    {"name": "Stecker", "model": "HM-LC-Sw1-Pl-DN-R1", "address": "6A9D15", "aes": true}
  ]
}`

// rtDefaults loads the eQ-3 defaults of an HM-CC-RT-DN into dev (list 0
// and list 7 of channel 0).
func rtDefaults(dev *fakemod.Device) {
	for reg, v := range map[byte]byte{1: 1, 14: 10, 15: 0, 17: 0, 18: 21} {
		dev.SetReg(0, 0, reg, v)
	}
	l7 := map[byte]byte{
		1: 42, 2: 34, 3: 9, 4: 61, 5: 24, 6: 3, 7: 0, 8: 22, 9: 7,
		10: 1<<5 | 16, 11: 0, 12: 100, 13: 15,
		14: 1 | 2<<1, // daylight saving on, adaptive regulation on
		18: 1<<3 | 1, 19: 4<<5 | 14,
	}
	day, _ := params.ParseDay("06:00 17.0, 22:00 21.0, 24:00 17.0")
	for d := 0; d < 7; d++ {
		for a, v := range params.EncodeDay(day, params.ProfileBase, d) {
			l7[a] = v
		}
	}
	for a, v := range l7 {
		dev.SetReg(0, 7, a, v)
	}
}

type settingsEnv struct {
	*env
	rt, sco, plug *fakemod.Device

	hookMu sync.Mutex
	// before and after, if set, see every message the module transmits
	// before and after the emulated devices handled it.
	before, after func(m []byte)
}

func (se *settingsEnv) setHooks(before, after func(m []byte)) {
	se.hookMu.Lock()
	se.before, se.after = before, after
	se.hookMu.Unlock()
}

// newSettingsEnv starts a bridge whose (emulated HM-MOD-RPI-PCB) devices
// keep and report their registers.
func newSettingsEnv(t *testing.T, stateDir string, plainAES bool) *settingsEnv {
	t.Helper()
	se := &settingsEnv{
		rt:   fakemod.NewDevice(rtdn, 0x0095, "MEQ0000001"),
		sco:  fakemod.NewDevice(sco, 0x00c7, "NEQ0000001"),
		plug: fakemod.NewDevice(plug, 0x00d8, "NEQ0000002"),
	}
	rtDefaults(se.rt)
	se.sco.SetReg(1, 1, 33, 0)
	se.sco.SetReg(0, 0, 9, 0)
	se.sco.SetReg(0, 0, 16, 1)
	se.plug.SetReg(1, 1, 86, 0)
	devs := []*fakemod.Device{se.rt, se.sco, se.plug}
	onSend := func(m []byte) (byte, []byte) {
		se.hookMu.Lock()
		before, after := se.before, se.after
		se.hookMu.Unlock()
		if before != nil {
			before(m)
		}
		if after != nil {
			defer after(m)
		}
		for _, d := range devs {
			if len(m) >= 9 && [3]byte(m[6:9]) == d.Addr {
				return d.HandleClassic(m)
			}
		}
		return fakemod.DefaultOnSend(m)
	}
	cfg := fmt.Sprintf(settingsConfig, stateDir)
	if plainAES {
		se.env = newEnvPlainAES(t, cfg, fakemod.Options{OnSend: onSend}, nil)
	} else {
		se.env = newEnvWith(t, cfg, fakemod.Options{OnSend: onSend}, nil)
	}
	for _, d := range devs {
		d.SetInject(func(m []byte) { se.mod.InjectRecv(0x01, m) })
	}
	return se
}

func (e *env) settings(addr string) map[string]any {
	e.t.Helper()
	return e.state("hmbridge/" + addr + "/settings")
}

func (e *env) current(addr, key string) any {
	if c, ok := e.settings(addr)["current"].(map[string]any); ok {
		return c[key]
	}
	return nil
}

func (e *env) detail(addr, key string) string {
	if c, ok := e.settings(addr)["detail"].(map[string]any); ok {
		s, _ := c[key].(string)
		return s
	}
	return ""
}

// configFrames returns the CONFIG (cmd 0x01) messages the module sent to
// dst, from index from on.
func (e *env) configFrames(dst [3]byte, from int) [][]byte {
	var res [][]byte
	for i, m := range e.mod.Sent() {
		if i >= from && len(m) >= 10 && m[2] == 0x01 && bytes.Equal(m[6:9], dst[:]) {
			res = append(res, m)
		}
	}
	return res
}

// withoutReads drops the register reads (CONFIG_PARAM_REQ, hex payload
// "cc04…") from CONFIG payloads: a newly paired device's settings are
// read after pairing.
func withoutReads(payloads []string) []string {
	var res []string
	for _, p := range payloads {
		if len(p) < 4 || p[2:4] != "04" {
			res = append(res, p)
		}
	}
	return res
}

// payloads renders frames as "flags payload" hex strings.
func payloads(frames [][]byte) []string {
	var res []string
	for _, m := range frames {
		res = append(res, fmt.Sprintf("%02x %x", m[1], m[9:]))
	}
	return res
}

func TestSettingsThermostatReadWrite(t *testing.T) {
	e := newSettingsEnv(t, t.TempDir(), false)
	// Discovery: config entities with their ranges, a status sensor and
	// a refresh button.
	var off map[string]any
	e.waitFor("discovery", func() bool {
		return json.Unmarshal(e.pub.get("homeassistant/number/hmbridge_38f59c/setting_temperature_offset/config"), &off) == nil &&
			e.settings("38f59c") != nil
	})
	if off["min"] != -3.5 || off["max"] != 3.5 || off["step"] != 0.5 || off["unit_of_measurement"] != "K" ||
		off["entity_category"] != "config" || off["command_topic"] != "hmbridge/38f59c/set/setting_temperature_offset" ||
		off["state_topic"] != "hmbridge/38f59c/settings" {
		t.Errorf("temperature offset entity: %v", off)
	}
	if e.pub.get("homeassistant/button/hmbridge_38f59c/settings_refresh/config") == nil ||
		e.pub.get("homeassistant/text/hmbridge_38f59c/setting_week_profile_monday/config") == nil {
		t.Error("refresh button or week profile entity missing")
	}
	if s := e.settings("38f59c")["status"]; !strings.HasPrefix(fmt.Sprint(s), "not read") {
		t.Errorf("status before reading: %v", s)
	}

	// Refresh: both lists are read, with a wake-up burst.
	start := len(e.mod.Sent())
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.RefreshSettings}
	e.waitFor("settings read", func() bool {
		return e.current("38f59c", "comfort_temperature") == 21.0 && e.settings("38f59c")["status"] == "ok"
	})
	if got := payloads(e.configFrames(rtdn, start)); fmt.Sprint(got) != "[b0 00040000000000 b0 00040000000007]" {
		t.Errorf("read requests %v", got)
	}
	for key, want := range map[string]any{
		"temperature_offset": 0.0, "eco_temperature": 17.0, "min_temperature": 4.5, "max_temperature": 30.5,
		"window_open_temperature": 12.0, "window_open_duration": 15.0, "boost_duration": 5.0,
		"boost_valve_position": 80.0, "decalcification_day": "Saturday", "decalcification_time": "11:00",
		"button_lock": "OFF", "valve_max_position": 100.0, "adaptive_regulation": "on", "daylight_saving_time": "ON",
		"window_detection_mode": "on", "low_battery_limit": 2.1, "backlight_on_time": 10.0,
		"week_profile_monday": "06:00 17.0, 22:00 21.0, 24:00 17.0",
	} {
		if got := e.current("38f59c", key); got != want {
			t.Errorf("%s = %v (%T), want %v", key, got, got, want)
		}
	}

	// Change the offset: it shares register 9 with the display
	// settings, so list 7 is read right before the write (never the
	// cache), only register 9 is written (burst), then the list is read
	// back.
	start = len(e.mod.Sent())
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "temperature_offset", Value: "1.5"}
	e.waitFor("offset written", func() bool { return e.detail("38f59c", "temperature_offset") == "ok" })
	if got := e.current("38f59c", "temperature_offset"); got != 1.5 {
		t.Errorf("offset after writing: %v", got)
	}
	if v, _ := e.rt.Reg(0, 7, 9); v != 10 {
		t.Errorf("device register 9 = %d, want 10", v)
	}
	want := "[b0 00040000000007 b0 00050000000007 b0 0008090a b0 0006 b0 00040000000007]"
	if got := payloads(e.configFrames(rtdn, start)); fmt.Sprint(got) != want {
		t.Errorf("write frames %v, want %s", got, want)
	}
	if lc := e.state("hmbridge/38f59c/state")["last_command"]; lc != "ok: setting temperature_offset = 1.5 K" {
		t.Errorf("last_command %v", lc)
	}

	// Out of range: rejected, nothing sent.
	start = len(e.mod.Sent())
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "comfort_temperature", Value: "31"}
	e.waitFor("rejection", func() bool {
		return strings.HasPrefix(e.detail("38f59c", "comfort_temperature"), "failed: comfort_temperature: 31 is out of range 15 … 30")
	})
	if s := e.settings("38f59c")["status"]; !strings.HasPrefix(fmt.Sprint(s), "failed: comfort_temperature") {
		t.Errorf("status %v", s)
	}
	if lc := fmt.Sprint(e.state("hmbridge/38f59c/state")["last_command"]); !strings.HasPrefix(lc, "failed: setting comfort_temperature") {
		t.Errorf("last_command %v", lc)
	}
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "no_such_setting", Value: "1"}
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "week_profile_monday", Value: "06:00 17"}
	e.waitFor("profile rejection", func() bool {
		return strings.HasPrefix(e.detail("38f59c", "week_profile_monday"), "failed:")
	})
	time.Sleep(50 * time.Millisecond)
	if got := e.configFrames(rtdn, start); len(got) != 0 {
		t.Errorf("invalid values were sent: %v", payloads(got))
	}

	// The week profile of Monday (registers 72 … 97) and the button lock
	// (list 0).
	start = len(e.mod.Sent())
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "week_profile_monday", Value: "05:30 20.5, 08:00 17, 24:00 16"}
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "button_lock", Value: "ON"}
	e.waitFor("profile and lock written", func() bool {
		return e.detail("38f59c", "week_profile_monday") == "ok" && e.detail("38f59c", "button_lock") == "ok"
	})
	if got := e.current("38f59c", "week_profile_monday"); got != "05:30 20.5, 08:00 17.0, 24:00 16.0" {
		t.Errorf("monday = %v", got)
	}
	if v, _ := e.rt.Reg(0, 0, 15); v != 1 {
		t.Errorf("button lock register = %d", v)
	}
	if got := e.current("38f59c", "week_profile_tuesday"); got != "06:00 17.0, 22:00 21.0, 24:00 17.0" {
		t.Errorf("tuesday changed: %v", got)
	}
	// The comfort temperature failure is history after a refresh.
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.RefreshSettings}
	e.waitFor("status ok", func() bool {
		return e.settings("38f59c")["status"] == "ok" && e.detail("38f59c", "comfort_temperature") == ""
	})
	for _, m := range e.configFrames(rtdn, start) {
		if m[1] != 0xb0 {
			t.Errorf("frame without burst: %x", m)
		}
	}
	// A change in the device's menu (INFO_PARAM_CHANGE): only taken when
	// authenticated (AES device).
	e.inject(0x02, msg(0x41, 0xa0, 0x10, rtdn, central, 0x04, 0x00, 0, 0, 0, 0, 0x07, 0x02, 0x24))
	e.waitFor("menu change", func() bool { return e.current("38f59c", "eco_temperature") == 18.0 })
	e.inject(0x01, msg(0x42, 0xa0, 0x10, rtdn, central, 0x04, 0x00, 0, 0, 0, 0, 0x07, 0x02, 0x26))
	time.Sleep(50 * time.Millisecond)
	if got := e.current("38f59c", "eco_temperature"); got != 18.0 {
		t.Errorf("unauthenticated change taken: %v", got)
	}

	// The whole week as JSON.
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "week_profile",
		Value: `{"saturday": "24:00 19", "Sunday": "12:00 18, 24:00 19"}`}
	e.waitFor("week written", func() bool {
		return e.current("38f59c", "week_profile_saturday") == "24:00 19.0" && e.current("38f59c", "week_profile_sunday") == "12:00 18.0, 24:00 19.0"
	})
}

func TestSettingsContactWaitsForConfigButton(t *testing.T) {
	e := newSettingsEnv(t, t.TempDir(), false)
	e.sco.SetAsleep(true)
	start := len(e.mod.Sent())
	e.b.Commands() <- ha.Command{Addr: "4c5d6e", Kind: ha.SetSetting, Setting: "event_delay", Value: "180"}
	e.waitFor("pending", func() bool {
		return e.detail("4c5d6e", "event_delay") == "pending" &&
			strings.HasPrefix(fmt.Sprint(e.settings("4c5d6e")["status"]), "pending: 1 change waiting for the device")
	})
	if lc := e.state("hmbridge/4c5d6e/state")["last_command"]; lc != "pending: setting event_delay (press the device's config button)" {
		t.Errorf("last_command %v", lc)
	}
	time.Sleep(50 * time.Millisecond)
	if got := e.configFrames(sco, start); len(got) != 0 {
		t.Fatalf("sent to a sleeping contact: %v", payloads(got))
	}
	// An ordinary event does not wake it up.
	e.inject(0x02, msg(0x30, 0xa4, 0x41, sco, central, 0x01, 0x01, 0x00))
	time.Sleep(50 * time.Millisecond)
	if got := e.configFrames(sco, start); len(got) != 0 {
		t.Fatalf("sent after an event: %v", payloads(got))
	}

	// The config button: DEVICE_INFO. The device listens now. AES is on
	// for channel 1, so the ACKs must be authenticated (the emulated
	// module challenges, see fakemod.Options.ChallengeAES).
	// Real devices send it as a broadcast (flags 84 to 000000, seen on
	// an HM-Sec-SCo), which must wake it although it isn't signed.
	e.sco.SetAsleep(false)
	e.inject(0x01, e.sco.DeviceInfo(0x31, [3]byte{}))
	e.waitFor("written", func() bool { return e.detail("4c5d6e", "event_delay") == "ok" })
	if v, _ := e.sco.Reg(1, 1, 33); v != 0x83 {
		t.Errorf("EVENT_DELAYTIME register = %02x, want 83", v)
	}
	// A full register needs no read before the write; read back after.
	want := "[a0 01050000000001 a0 01082183 a0 0106 a0 01040000000001]"
	if got := payloads(e.configFrames(sco, start)); fmt.Sprint(got) != want {
		t.Errorf("frames %v, want %s", got, want)
	}
	if got := e.current("4c5d6e", "event_delay"); got != 180.0 {
		t.Errorf("event_delay = %v", got)
	}

	// Refresh waits for the device too; WAKEMEUP (flag 0x02) wakes it.
	e.sco.SetAsleep(true)
	e.b.Commands() <- ha.Command{Addr: "4c5d6e", Kind: ha.RefreshSettings}
	e.waitFor("refresh pending", func() bool {
		return e.settings("4c5d6e")["status"] == "pending: read at the next config button press"
	})
	e.sco.SetAsleep(false)
	e.inject(0x02, msg(0x32, 0xa6, 0x41, sco, central, 0x01, 0x02, 0x00))
	e.waitFor("read", func() bool {
		return e.settings("4c5d6e")["status"] == "ok" && e.current("4c5d6e", "cyclic_info_message") == "OFF"
	})
	if n := e.sco.ParamReads(); n != 3 { // read-back, list 0 and list 1
		t.Errorf("%d register reads", n)
	}
}

func TestSettingsAESWritesNeedAuthenticatedACK(t *testing.T) {
	// The emulated module does not challenge: the plug's plain ACKs to
	// writes on its AES channel 1 could have been forged.
	e := newSettingsEnv(t, "", true)
	e.b.Commands() <- ha.Command{Addr: "6a9d15", Kind: ha.SetSetting, Setting: "power_on_action", Value: "on"}
	e.waitFor("failure", func() bool {
		return strings.Contains(e.detail("6a9d15", "power_on_action"), "not AES-authenticated")
	})
}

// TestSettingsChannel0WritesNeedAuthenticatedACK: list 7 of an AES
// thermostat is written with channel 0, which is not among its AES
// channels; the device challenges CONFIG_START and CONFIG_WRITE_INDEX
// of channel 0 anyway (AES is active on a channel), so a plain ACK is
// refused (it could have been forged).
func TestSettingsChannel0WritesNeedAuthenticatedACK(t *testing.T) {
	e := newSettingsEnv(t, "", true)
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "comfort_temperature", Value: "22"}
	e.waitFor("failure", func() bool {
		d := e.detail("38f59c", "comfort_temperature")
		return strings.Contains(d, "CONFIG_START") && strings.Contains(d, "not AES-authenticated")
	})
}

func TestConfigNeedsAuth(t *testing.T) {
	for _, tt := range []struct {
		aes     []byte
		payload []byte
		want    bool
	}{
		{nil, []byte{0, 0x05}, false},
		{[]byte{1}, []byte{0, 0x05}, true},          // CONFIG_START, list 0/7
		{[]byte{1}, []byte{0, 0x08, 9, 10}, true},   // CONFIG_WRITE_INDEX
		{[]byte{1}, []byte{0, 0x06}, false},         // CONFIG_END: not challenged
		{[]byte{1}, []byte{0, 0x04}, false},         // register read
		{[]byte{1}, []byte{1, 0x05}, true},          // an AES channel
		{[]byte{1}, []byte{2, 0x08, 1, 1}, false},   // a channel without AES
		{[]byte{2, 3}, []byte{3, 0x08, 1, 1}, true}, // …
		{[]byte{1}, []byte{0}, false},               // malformed
	} {
		if got := configNeedsAuth(tt.aes, tt.payload); got != tt.want {
			t.Errorf("aes %v payload %x: %v", tt.aes, tt.payload, got)
		}
	}
}

// TestSettingsWriteBaseIsFreshRead: a change to a shared register is
// based on the list read from the device in the same job, never on the
// cache (here: settings.json from before a change in the device's menu
// that hmbridge did not see; a poisoned cache looks the same).
func TestSettingsWriteBaseIsFreshRead(t *testing.T) {
	dir := t.TempDir()
	e := newSettingsEnv(t, dir, false)
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.RefreshSettings}
	e.waitFor("read", func() bool { return e.settings("38f59c")["status"] == "ok" })
	e.waitFor("settings file", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
		return err == nil && bytes.Contains(b, []byte("38f59c"))
	})
	e.stop()

	e2 := newSettingsEnv(t, dir, false)
	e2.waitFor("cached values", func() bool { return e2.current("38f59c", "boost_duration") == 5.0 })
	// The device's register 10 changed meanwhile: boost duration 20 min
	// (bits 5-7 = 4), valve position 80 % (bits 0-4 = 16).
	e2.rt.SetReg(0, 7, 10, 4<<5|16)
	e2.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "boost_valve_position", Value: "50"}
	e2.waitFor("written", func() bool { return e2.detail("38f59c", "boost_valve_position") == "ok" })
	if v, _ := e2.rt.Reg(0, 7, 10); v != 4<<5|10 {
		t.Errorf("register 10 = %08b, want the device's boost duration kept (%08b)", v, 4<<5|10)
	}
	if got := e2.current("38f59c", "boost_duration"); got != 20.0 {
		t.Errorf("boost_duration = %v", got)
	}
}

// TestSettingsVerifyWholeRegister: the read-back must hold the whole
// written register, not only the changed setting's bits.
func TestSettingsVerifyWholeRegister(t *testing.T) {
	e := newSettingsEnv(t, "", false)
	// The device keeps the boost valve position but changes the boost
	// duration bits of the same register (a write that went wrong).
	e.setHooks(nil, func(m []byte) {
		if len(m) >= 12 && m[2] == 0x01 && [3]byte(m[6:9]) == rtdn && m[10] == 0x08 && m[11] == 10 {
			v, _ := e.rt.Reg(0, 7, 10)
			e.rt.SetReg(0, 7, 10, v^0x20)
		}
	})
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "boost_valve_position", Value: "50"}
	e.waitFor("failure", func() bool {
		return strings.Contains(e.detail("38f59c", "boost_valve_position"), "bits of settings sharing it differ")
	})
}

// TestSettingsReadRejectsConflictingFrames: frames with the device's
// address that report a register twice with different values in one
// read (someone else sends frames) fail the read; nothing is written.
func TestSettingsReadRejectsConflictingFrames(t *testing.T) {
	e := newSettingsEnv(t, "", false)
	e.setHooks(func(m []byte) {
		if isParamReq(m, rtdn) {
			forged := msg(0x77, 0xa0, 0x10, rtdn, central, 0x02, 10, 0xff)
			go e.mod.InjectRecv(0x01, forged)
		}
	}, nil)
	start := len(e.mod.Sent())
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "boost_valve_position", Value: "50"}
	e.waitFor("failure", func() bool {
		return strings.Contains(e.detail("38f59c", "boost_valve_position"), "forged frames")
	})
	for _, p := range payloads(e.configFrames(rtdn, start)) {
		if strings.HasPrefix(p, "b0 0005") || strings.HasPrefix(p, "b0 0008") {
			t.Errorf("written after a conflicting read: %s", p)
		}
	}
}

// isParamReq reports whether m is a CONFIG_PARAM_REQ to dst.
func isParamReq(m []byte, dst [3]byte) bool {
	return len(m) >= 11 && m[2] == 0x01 && [3]byte(m[6:9]) == dst && m[10] == 0x04
}

func TestSettingsPluginWrite(t *testing.T) {
	e := newSettingsEnv(t, "", false)
	e.b.Commands() <- ha.Command{Addr: "6a9d15", Kind: ha.SetSetting, Setting: "power_on_action", Value: "on"}
	e.waitFor("written", func() bool { return e.detail("6a9d15", "power_on_action") == "ok" })
	if v, _ := e.plug.Reg(1, 1, 86); v != 1 {
		t.Errorf("POWERUP_ACTION register = %d", v)
	}
	// Mains powered: no burst.
	for _, m := range e.configFrames(plug, 0) {
		if m[1]&0x10 != 0 {
			t.Errorf("burst to a plug: %x", m)
		}
	}
}

func TestSettingsCachePersisted(t *testing.T) {
	dir := t.TempDir()
	e := newSettingsEnv(t, dir, false)
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.RefreshSettings}
	e.waitFor("read", func() bool { return e.settings("38f59c")["status"] == "ok" })
	path := filepath.Join(dir, "settings.json")
	var f struct {
		Version int `json:"version"`
		Devices map[string]struct {
			Model string `json:"model"`
			Lists []struct {
				List byte   `json:"list"`
				Regs string `json:"regs"`
			} `json:"lists"`
		} `json:"devices"`
	}
	e.waitFor("settings file", func() bool {
		b, err := os.ReadFile(path)
		return err == nil && json.Unmarshal(b, &f) == nil && len(f.Devices["38f59c"].Lists) == 2
	})
	if f.Version != 1 || f.Devices["38f59c"].Model != "HM-CC-RT-DN" || !strings.HasPrefix(f.Devices["38f59c"].Lists[1].Regs, "012a0222") {
		t.Errorf("settings file %+v", f)
	}
	fi, _ := os.Stat(path)
	// Unchanged registers are not written again (read_at in the file is
	// when they last changed).
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.RefreshSettings}
	reads := e.rt.ParamReads()
	e.waitFor("second read", func() bool { return e.rt.ParamReads() >= reads+2 && e.settings("38f59c")["status"] == "ok" })
	time.Sleep(50 * time.Millisecond)
	if fi2, _ := os.Stat(path); !fi2.ModTime().Equal(fi.ModTime()) {
		t.Error("unchanged settings were written again")
	}
	e.stop()

	// A new bridge shows the cached values without asking the device.
	e2 := newSettingsEnv(t, dir, false)
	e2.waitFor("cached values", func() bool { return e2.current("38f59c", "comfort_temperature") == 21.0 })
	if n := e2.rt.ParamReads(); n != 0 {
		t.Errorf("%d reads after restart", n)
	}
}

// TestSettingsReadRPIRFMod reads an AES thermostat's settings through the
// RPI-RF-MOD driver: the 26 frames of list 7 are acknowledged in
// software, without AES challenges (their rate limit would drop them).
func TestSettingsReadRPIRFMod(t *testing.T) {
	rt := fakemod.NewDevice(rtdn, 0x0095, "MEQ0000001")
	rtDefaults(rt)
	// AES is active: the thermostat challenges the writes to list 7
	// (channel 0) with the system key.
	rt.SetKey(hmkey.FromPassphrase("secret", 1))
	rt.SetConfigAES(true)
	devs := &aesDevices{device: rt.Handle}
	e := newDualEnv(t, fmt.Sprintf(settingsConfig, ""), devs, nil)
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.RefreshSettings}
	e.waitFor("settings read", func() bool {
		return e.settings("38f59c")["status"] == "ok" && e.current("38f59c", "week_profile_friday") == "06:00 17.0, 22:00 21.0, 24:00 17.0"
	})
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.SetSetting, Setting: "eco_temperature", Value: "16.5"}
	e.waitFor("written", func() bool { return e.detail("38f59c", "eco_temperature") == "ok" })
	if v, _ := rt.Reg(0, 7, 2); v != 33 {
		t.Errorf("eco register %d", v)
	}
}

// TestSettingsReadTimeout: a device that stops in the middle of a list.
func TestSettingsReadTimeout(t *testing.T) {
	e := newSettingsEnv(t, "", false)
	e.b.paramTimeout = 100 * time.Millisecond
	e.rt.SetInject(func([]byte) {}) // further frames are lost
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.RefreshSettings}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if s := fmt.Sprint(e.settings("38f59c")["status"]); strings.HasPrefix(s, "failed:") {
			if !strings.Contains(s, "list incomplete") {
				t.Errorf("status %q", s)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no failure reported")
}

// TestSettingsRetryDroppedAfterUnpair: a settings job that failed is
// retried later; a device unpaired meanwhile gets nothing more.
func TestSettingsRetryDroppedAfterUnpair(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	if err := saveDevices(file, []store.Device{{Name: "Stehlampe", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "6A9D15", Paired: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	failList1 := true
	onSend := func(m []byte) (byte, []byte) {
		mu.Lock()
		fail := failList1
		mu.Unlock()
		// Messages for channel 1 (the setting's list) are not answered.
		if fail && len(m) >= 10 && m[2] == 0x01 && [3]byte(m[6:9]) == plug && m[9] == 0x01 {
			return fakemod.AckEUnknown, nil
		}
		return fakemod.DefaultOnSend(m)
	}
	e := newEnvWith(t, pairingConfig(filepath.Dir(file)), fakemod.Options{OnSend: onSend}, func(o *Options) {
		fastPairing(o)
		o.RetryDelay = 400 * time.Millisecond
	})
	e.waitFor("name", func() bool { return string(e.pub.get("hmbridge/6a9d15/name")) == "Stehlampe" })
	e.b.Commands() <- ha.Command{Addr: "6a9d15", Kind: ha.SetSetting, Setting: "power_on_action", Value: "on"}
	e.waitFor("first attempt", func() bool { return len(e.payloadsSent(plug, 0x01)) > 0 })
	// Unpair it while the retry waits.
	e.b.Commands() <- ha.Command{Kind: ha.Unpair, Addr: "6a9d15"}
	e.waitFor("unpaired", func() bool { return strings.HasPrefix(e.lastResult(), "unpaired Stehlampe") })
	mu.Lock()
	failList1 = false
	mu.Unlock()
	n := len(e.payloadsSent(plug, 0x01))
	time.Sleep(1200 * time.Millisecond)
	if got := e.payloadsSent(plug, 0x01); len(got) != n {
		t.Errorf("sent to the unpaired device: %v", got[n:])
	}
}

// TestSettingsWakeNeedsValidFrame: only wake-up frames that pass the AES
// checks and are addressed to the central (or broadcast) send what waits
// for a config-only device, and after a failed job the next wake-up
// waits a minimum gap (anyone can send DEVICE_INFO with its address).
func TestSettingsWakeNeedsValidFrame(t *testing.T) {
	e := newSettingsEnv(t, "", false)
	e.b.wakeRetryGap = 700 * time.Millisecond
	e.sco.SetAsleep(true)
	start := len(e.mod.Sent())
	e.b.Commands() <- ha.Command{Addr: "4c5d6e", Kind: ha.SetSetting, Setting: "event_delay", Value: "180"}
	e.waitFor("pending", func() bool { return e.detail("4c5d6e", "event_delay") == "pending" })
	quiet := func(what string) {
		t.Helper()
		time.Sleep(100 * time.Millisecond)
		if got := e.configFrames(sco, start); len(got) != 0 {
			t.Fatalf("%s: sent %v", what, payloads(got))
		}
	}
	// WAKEMEUP on its AES channel, not authenticated: forged.
	e.inject(0x01, msg(0x30, 0xa6, 0x41, sco, central, 0x01, 0x01, 0x00))
	quiet("unauthenticated WAKEMEUP")
	// Authenticated, but to another central.
	other := [3]byte{0x11, 0x22, 0x33}
	e.inject(0x02, msg(0x31, 0xa6, 0x41, sco, other, 0x01, 0x02, 0x00))
	e.inject(0x01, e.sco.DeviceInfo(0x32, other))
	quiet("frames to another address")

	// A DEVICE_INFO while the device does not listen (forged, or the
	// device went back to sleep): the job fails, the change stays
	// pending, and wake-ups are ignored for the gap.
	e.inject(0x01, e.sco.DeviceInfo(0x33, central))
	e.waitFor("failed attempt", func() bool { return len(e.configFrames(sco, start)) > 0 && e.settings("4c5d6e")["status"] != "writing" })
	e.waitFor("pending again", func() bool { return e.detail("4c5d6e", "event_delay") == "pending" })
	start = len(e.mod.Sent())
	e.sco.SetAsleep(false)
	e.inject(0x01, e.sco.DeviceInfo(0x34, central))
	quiet("wake-up within the gap")
	time.Sleep(700 * time.Millisecond)
	e.inject(0x01, e.sco.DeviceInfo(0x35, central))
	e.waitFor("written", func() bool { return e.detail("4c5d6e", "event_delay") == "ok" })
}

func TestParseWeekProfileDuplicateDays(t *testing.T) {
	m := params.ForModel("HM-CC-RT-DN")
	_, err := parseWeekProfile(m, `{"monday": "24:00 17", "Monday": "24:00 21"}`)
	if err == nil || !strings.Contains(err.Error(), "name the same day") {
		t.Errorf("duplicate day: %v", err)
	}
	if ch, err := parseWeekProfile(m, `{"Monday": "24:00 21", "tuesday": "24:00 17"}`); err != nil || len(ch) != 2 {
		t.Errorf("%d changes, %v", len(ch), err)
	}
}

// A week profile day the device holds with a temperature hmbridge would
// never write is shown as stored and flagged in the detail.
func TestSettingsInvalidProfileFlagged(t *testing.T) {
	e := newSettingsEnv(t, "", false)
	for a, v := range params.EncodeDay(params.Day{{End: params.EndOfDay, Temp: 31.5}}, params.ProfileBase, 2) {
		e.rt.SetReg(0, 7, a, v)
	}
	e.b.Commands() <- ha.Command{Addr: "38f59c", Kind: ha.RefreshSettings}
	e.waitFor("read", func() bool { return e.settings("38f59c")["status"] == "ok" })
	if got := e.current("38f59c", "week_profile_monday"); got != "24:00 31.5" {
		t.Errorf("monday = %v", got)
	}
	if d := e.detail("38f59c", "week_profile_monday"); !strings.HasPrefix(d, "invalid: temperature 31.5 is out of range") {
		t.Errorf("detail %q", d)
	}
}
