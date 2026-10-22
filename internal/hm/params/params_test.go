// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package params

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/hm/pairing"
)

func setting(t *testing.T, model, key string) *Setting {
	t.Helper()
	m := ForModel(model)
	if m == nil {
		t.Fatalf("no settings for %s", model)
	}
	s := m.Setting(key)
	if s == nil {
		t.Fatalf("%s has no setting %s", model, key)
	}
	return s
}

func TestFieldGetPut(t *testing.T) {
	// RT-DN list 7 register 10: BOOST_POSITION bits 0-4 (%/5),
	// BOOST_TIME_PERIOD bits 5-7. eQ-3 defaults 80 % (16) and 5 min (1):
	// 0x30.
	r := Regs{10: 0x30}
	pos, period := Bits(10, 0, 5), Bits(10, 5, 3)
	if v, ok := pos.Get(r); !ok || v != 16 {
		t.Errorf("pos = %d, %v", v, ok)
	}
	if v, _ := period.Get(r); v != 1 {
		t.Errorf("period = %d", v)
	}
	if err := period.Put(r, 3); err != nil || r[10] != 0x70 {
		t.Errorf("Put = %v, reg %02x, want 70", err, r[10])
	}
	if err := period.Put(r, 8); err == nil {
		t.Error("8 fits into 3 bits")
	}
	if err := period.Put(Regs{}, 1); !errors.Is(err, ErrUnknownRegister) {
		t.Errorf("Put without the register: %v", err)
	}
	// A whole register needs no old value.
	r2 := Regs{}
	if err := Byte(15).Put(r2, 1); err != nil || r2[15] != 1 {
		t.Errorf("Byte.Put: %v %v", err, r2)
	}
	// 16-bit word with mask 511 (week profile end time), keeping bits 9-14.
	w := Word(20, 9)
	r3 := Regs{20: 0x44, 21: 0x48}
	if v, _ := w.Get(r3); v != 72 {
		t.Errorf("word = %d", v)
	}
	if err := w.Put(r3, 0x1ff); err != nil || r3[20] != 0x45 || r3[21] != 0xff {
		t.Errorf("word Put: %v %02x %02x", err, r3[20], r3[21])
	}
}

// TestEncodingFromDescriptions checks the encoding of every exposed
// setting with values from eQ-3's descriptions (defaults, ranges, enum
// indexes).
func TestEncodingFromDescriptions(t *testing.T) {
	type tc struct {
		model, key, value string
		raw               uint32
	}
	ok := []tc{
		// TEMPERATURE_OFFSET enum: -3.5K=0, 0.0K=7 (default), 3.5K=14.
		{"HM-CC-RT-DN", "temperature_offset", "-3.5", 0},
		{"HM-CC-RT-DN", "temperature_offset", "0", 7},
		{"HM-CC-RT-DN", "temperature_offset", "3.5", 14},
		{"HM-TC-IT-WM-W-EU", "temperature_offset", "1.5", 10},
		// decimalIntegerScale factor 2, defaults 21 / 17 / 4.5 / 30.5 / 12.
		{"HM-CC-RT-DN", "comfort_temperature", "21", 42},
		{"HM-CC-RT-DN", "eco_temperature", "17", 34},
		{"HM-CC-RT-DN", "min_temperature", "4.5", 9},
		{"HM-CC-RT-DN", "max_temperature", "30.5", 61},
		{"HM-CC-RT-DN", "window_open_temperature", "12", 24},
		// integerIntegerScale division 5: default 15 min.
		{"HM-CC-RT-DN", "window_open_duration", "15", 3},
		{"HM-CC-RT-DN", "window_open_duration", "60", 12},
		// BOOST_TIME_PERIOD enum: 5 min = 1 (default), 30 min = 6.
		{"HM-CC-RT-DN", "boost_duration", "5", 1},
		{"HM-TC-IT-WM-W-EU", "boost_duration", "30", 6},
		// BOOST_POSITION division 5: default 80 %.
		{"HM-CC-RT-DN", "boost_valve_position", "80", 16},
		{"HM-CC-RT-DN", "valve_offset", "0", 0},
		{"HM-CC-RT-DN", "valve_max_position", "100", 100},
		// DECALCIFICATION_WEEKDAY: SATURDAY=0 (default) … FRIDAY=6;
		// DECALCIFICATION_TIME division 30: default 660 min = 11:00.
		{"HM-CC-RT-DN", "decalcification_day", "Saturday", 0},
		{"HM-CC-RT-DN", "decalcification_day", "friday", 6},
		{"HM-CC-RT-DN", "decalcification_time", "11:00", 22},
		{"HM-CC-RT-DN", "decalcification_time", "23:30", 47},
		{"HM-CC-RT-DN", "button_lock", "ON", 1},
		{"HM-CC-RT-DN", "button_lock", "OFF", 0},
		{"HM-CC-RT-DN", "low_battery_limit", "2.1", 21},
		{"HM-CC-RT-DN", "adaptive_regulation", "on", 2},
		{"HM-CC-RT-DN", "window_detection_mode", "on", 4},
		{"HM-CC-RT-DN", "backlight_on_time", "10", 10},
		{"HM-TC-IT-WM-W-EU", "display_temperature", "set temperature", 1},
		{"HM-TC-IT-WM-W-EU", "display_humidity", "temperature and humidity", 1},
		// EVENT_DELAYTIME decimalConfigTime, factors 1 and 60 (bit 7).
		{"HM-Sec-SCo", "event_delay", "0", 0},
		{"HM-Sec-SCo", "event_delay", "127", 127},
		{"HM-Sec-SCo", "event_delay", "180", 0x83},
		{"HM-Sec-SCo", "event_delay", "7620", 0xff},
		{"HM-Sec-SCo", "cyclic_info_message", "ON", 1},
		// LONG_PRESS_TIME factor 10 offset -0.3: default 0.4 s;
		// DBL_PRESS_TIME factor 10.
		{"HM-PB-2-WM55", "long_press_time_1", "0.4", 1},
		{"HM-RC-8", "long_press_time_8", "1.8", 15},
		{"HM-PB-2-WM55", "double_press_time_2", "1.5", 15},
		// POWERUP_ACTION, LOAD_APPEAR_BEHAVIOUR, DISPLAY_BRIGHTNESS
		// (default 15).
		{"HM-LC-Sw1-Pl-DN-R1", "power_on_action", "on", 1},
		{"HM-LC-Dim1L-Pl-3", "load_appear_behaviour", "button press if it was on", 3},
		{"HM-OU-LED16", "display_brightness", "15", 15},
	}
	for _, c := range ok {
		s := setting(t, c.model, c.key)
		raw, err := s.ParseValue(c.value)
		if err != nil || raw != c.raw {
			t.Errorf("%s %s %q: raw %d, %v; want %d", c.model, c.key, c.value, raw, err, c.raw)
			continue
		}
		// And back.
		r := Regs{}
		for i := 0; i < int(s.Field.Bytes); i++ {
			r[s.Field.Index+byte(i)] = 0
		}
		if err := s.Field.Put(r, raw); err != nil {
			t.Fatal(err)
		}
		v, ok := s.Value(r)
		if !ok {
			t.Errorf("%s %s: raw %d does not decode", c.model, c.key, raw)
			continue
		}
		raw2, err := s.ParseValue(fmt.Sprint(v))
		if err != nil || raw2 != raw {
			t.Errorf("%s %s: value %v → raw %d (%v), want %d", c.model, c.key, v, raw2, err, raw)
		}
	}

	bad := []tc{
		{"HM-CC-RT-DN", "temperature_offset", "4", 0},
		{"HM-CC-RT-DN", "temperature_offset", "0.25", 0},
		{"HM-CC-RT-DN", "comfort_temperature", "14.5", 0},
		{"HM-CC-RT-DN", "comfort_temperature", "30.5", 0},
		{"HM-CC-RT-DN", "comfort_temperature", "21.3", 0},
		{"HM-CC-RT-DN", "comfort_temperature", "NaN", 0},
		{"HM-CC-RT-DN", "comfort_temperature", "Inf", 0},
		{"HM-CC-RT-DN", "comfort_temperature", "warm", 0},
		{"HM-CC-RT-DN", "min_temperature", "4", 0},
		{"HM-CC-RT-DN", "max_temperature", "31", 0},
		{"HM-CC-RT-DN", "window_open_duration", "7", 0},
		{"HM-CC-RT-DN", "window_open_duration", "65", 0},
		{"HM-CC-RT-DN", "boost_duration", "35", 0},
		{"HM-CC-RT-DN", "valve_offset", "101", 0},
		{"HM-CC-RT-DN", "valve_offset", "-1", 0},
		{"HM-CC-RT-DN", "decalcification_day", "Someday", 0},
		{"HM-CC-RT-DN", "decalcification_time", "11:15", 0},
		{"HM-CC-RT-DN", "button_lock", "maybe", 0},
		{"HM-CC-RT-DN", "low_battery_limit", "2.6", 0},
		{"HM-Sec-SCo", "event_delay", "128", 0},
		{"HM-Sec-SCo", "event_delay", "7680", 0},
		{"HM-Sec-SCo", "event_delay", "1.5", 0},
		{"HM-PB-2-WM55", "long_press_time_1", "0.2", 0},
		{"HM-PB-2-WM55", "double_press_time_1", "1.6", 0},
		{"HM-OU-LED16", "display_brightness", "16", 0},
	}
	for _, c := range bad {
		if raw, err := setting(t, c.model, c.key).ParseValue(c.value); err == nil {
			t.Errorf("%s %s %q accepted (raw %d)", c.model, c.key, c.value, raw)
		}
	}
}

func TestSharedRegisters(t *testing.T) {
	// TC list 7 register 9: TEMPERATURE_OFFSET bits 0-3,
	// SHOW_SET_TEMPERATUR bit 5, SHOW_HUMIDITY bit 6,
	// SENDE_WEATHER_DATA bit 7 (not exposed, must be kept).
	off := setting(t, "HM-TC-IT-WM-W-EU", "temperature_offset")
	hum := setting(t, "HM-TC-IT-WM-W-EU", "display_humidity")
	r := Regs{9: 0x87} // offset 0.0K, weather data on
	c, err := NewChange(hum, "temperature and humidity")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Known(r) || c.Known(Regs{}) {
		t.Error("Known")
	}
	if err := c.Apply(r); err != nil || r[9] != 0xc7 {
		t.Errorf("Apply: %v, %02x want c7", err, r[9])
	}
	c2, _ := NewChange(off, "-1")
	c2.Apply(r)
	if r[9] != 0xc5 {
		t.Errorf("offset -1: %02x want c5", r[9])
	}
	if err := c2.Verify(r); err != nil {
		t.Error(err)
	}
	if err := c2.Verify(Regs{9: 0xc7}); err == nil || !strings.Contains(err.Error(), "device reports 0 K after writing -1 K") {
		t.Errorf("Verify mismatch: %v", err)
	}
	if v, ok := off.Value(Regs{9: 0x0f}); ok || v != nil {
		// 15 is not a valid offset (enum ends at 14).
		t.Errorf("raw 15 decodes to %v", v)
	}
}

func TestWeekProfileEncoding(t *testing.T) {
	// eQ-3 defaults for Saturday: ENDTIME_SATURDAY_1 360, TEMPERATURE 17;
	// _2 1320, 21; _3 … _13 1440, 17.
	d, err := ParseDay("06:00 17.0, 22:00 21, 24:00 17")
	if err != nil {
		t.Fatal(err)
	}
	r := EncodeDay(d, ProfileBase, 0)
	if len(r) != 26 {
		t.Fatalf("%d registers", len(r))
	}
	// 17 °C · 2 = 34 in bits 9-14, 360 / 5 = 72 in bits 0-8.
	if r[20] != 0x44 || r[21] != 0x48 || r[22] != 0x55 || r[23] != 0x08 || r[24] != 0x45 || r[25] != 0x20 || r[44] != 0x45 || r[45] != 0x20 {
		t.Errorf("registers %v", r)
	}
	got, err := DecodeDay(r, ProfileBase, 0)
	if err != nil || !reflect.DeepEqual(got, d) {
		t.Errorf("DecodeDay = %v, %v", got, err)
	}
	if s := FormatDay(got); s != "06:00 17.0, 22:00 21.0, 24:00 17.0" {
		t.Errorf("FormatDay = %q", s)
	}
	// Friday (day 6) starts at 20 + 6·26 = 176, its last slot at 200.
	full := Day{}
	for i := 1; i <= 13; i++ {
		end := i * 110
		if i == 13 {
			end = 1440
		}
		full = append(full, Slot{End: end, Temp: 5 + float64(i)*1.5})
	}
	if err := ValidateDay(full); err != nil {
		t.Fatal(err)
	}
	rf := EncodeDay(full, ProfileBase, 6)
	if _, ok := rf[176]; !ok {
		t.Error("Friday does not start at 176")
	}
	if _, ok := rf[201]; !ok {
		t.Error("Friday does not end at 201")
	}
	back, err := DecodeDay(rf, ProfileBase, 6)
	if err != nil || !reflect.DeepEqual(back, full) {
		t.Errorf("13 slots: %v, %v", back, err)
	}
	reparsed, err := ParseDay(FormatDay(full))
	if err != nil || !reflect.DeepEqual(reparsed, full) {
		t.Errorf("format round trip: %v, %v", reparsed, err)
	}
	if n := len(FormatDay(full)); n > 154 {
		t.Errorf("13 slots need %d characters", n)
	}

	for _, bad := range []string{
		"",
		"06:00 17",                     // does not end at 24:00
		"06:00 17, 05:00 18, 24:00 17", // not increasing
		"06:03 17, 24:00 17",           // not a multiple of 5
		"06:00 4.5, 24:00 17",          // below 5 °C
		"06:00 30.5, 24:00 17",         // above 30 °C
		"06:00 17.25, 24:00 17",        // not a multiple of 0.5
		"25:00 17",                     // no such time
		"24:30 17",                     // after 24:00
		"06:00, 24:00 17",              // no temperature
		"6 17, 24:00 17",               // no minutes
		"06:00 warm, 24:00 17",         // not a number
		strings.Repeat("01:00 17, ", 13) + "24:00 17", // 14 slots
	} {
		if d, err := ParseDay(bad); err == nil {
			t.Errorf("ParseDay(%q) = %v", bad, d)
		}
	}
	// Separators and units are tolerated.
	if d, err := ParseDay(" 06:00  17°C ;24:00 21.5,"); err != nil || len(d) != 2 || d[1].Temp != 21.5 {
		t.Errorf("lenient parse: %v %v", d, err)
	}
	// A list whose day has no 24:00 slot, or decreasing times, does not
	// decode.
	broken := EncodeDay(d, ProfileBase, 2)
	broken[72+2*12+1] = 0 // last slot of Monday ends at 00:00
	for a := byte(72); a < 72+26; a += 2 {
		broken[a], broken[a+1] = 0x44, 0x48
	}
	if _, err := DecodeDay(broken, ProfileBase, 2); err == nil {
		t.Error("broken day decoded")
	}
	if _, err := DecodeDay(Regs{}, ProfileBase, 0); err == nil {
		t.Error("missing registers decoded")
	}
}

func TestProfileChange(t *testing.T) {
	s := setting(t, "HM-CC-RT-DN", "week_profile_monday")
	if s.Day != 2 || s.Loc != List7 {
		t.Fatalf("monday: %+v", s)
	}
	c, err := NewChange(s, "05:30 20.5, 24:00 16")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Known(nil) {
		t.Error("a day is written completely")
	}
	r := Regs{}
	if err := c.Apply(r); err != nil {
		t.Fatal(err)
	}
	if len(r) != 26 || r[72] != 0x52 || r[73] != 0x42 {
		t.Errorf("monday registers: %v", r)
	}
	if v, ok := s.Value(r); !ok || v != "05:30 20.5, 24:00 16.0" {
		t.Errorf("Value = %v", v)
	}
	if err := c.Verify(r); err != nil {
		t.Error(err)
	}
	r[73] = 0x43
	if err := c.Verify(r); err == nil {
		t.Error("changed register verified")
	}
	if _, err := NewChange(s, "05:30 20.5"); err == nil {
		t.Error("day without 24:00 accepted")
	}
}

func TestMergeResponse(t *testing.T) {
	r := Regs{}
	done, err := MergeResponse(r, []byte{0x02, 0x01, 0x2a, 0x02, 0x22})
	if err != nil || done || r[1] != 0x2a || r[2] != 0x22 {
		t.Fatalf("pairs: %v %v %v", done, err, r)
	}
	// A retransmission changes nothing; the end marker ends the list.
	MergeResponse(r, []byte{0x02, 0x01, 0x2a, 0x02, 0x22})
	if done, _ := MergeResponse(r, []byte{0x02, 0x09, 0x07, 0x00, 0x00}); !done || r[9] != 7 || len(r) != 3 {
		t.Errorf("end: %v %v", done, r)
	}
	// Sequence format: start address, values.
	r2 := Regs{}
	if done, err := MergeResponse(r2, []byte{0x03, 0x14, 0x44, 0x48, 0x54}); done || err != nil || r2[0x14] != 0x44 || r2[0x16] != 0x54 {
		t.Errorf("seq: %v %v %v", done, err, r2)
	}
	if done, _ := MergeResponse(r2, []byte{0x03, 0x00}); !done {
		t.Error("seq end not recognised")
	}
	if _, err := MergeResponse(r2, []byte{0x03, 0xff, 1, 2}); err == nil {
		t.Error("sequence beyond 255 accepted")
	}
	for _, p := range [][]byte{nil, {0x06, 0x01, 0x00}, {0x03}} {
		if _, err := MergeResponse(Regs{}, p); err == nil {
			t.Errorf("%x accepted", p)
		}
	}

	// EncodeResponse (the emulated device) and MergeResponse agree; 8
	// pairs per frame, the end marker last.
	src := Regs{}
	for a := 1; a <= 202; a++ {
		src[byte(a)] = byte(a * 7)
	}
	frames := EncodeResponse(src)
	if len(frames) != 26 {
		t.Errorf("%d frames for 202 registers", len(frames))
	}
	got := Regs{}
	for i, f := range frames {
		if len(f) > 17 {
			t.Errorf("frame %d has %d bytes", i, len(f))
		}
		done, err := MergeResponse(got, f)
		if err != nil || done != (i == len(frames)-1) {
			t.Errorf("frame %d: %v %v", i, done, err)
		}
	}
	if !reflect.DeepEqual(got, src) {
		t.Error("round trip differs")
	}
	if f := EncodeResponse(Regs{}); len(f) != 1 || !bytes.Equal(f[0], []byte{0x02, 0, 0}) {
		t.Errorf("empty list: %x", f)
	}
}

func TestDiff(t *testing.T) {
	d := Diff(Regs{1: 1, 2: 2}, Regs{1: 1, 2: 3, 5: 0})
	if !reflect.DeepEqual(d, []Reg{{2, 3}, {5, 0}}) {
		t.Errorf("Diff = %v", d)
	}
}

// TestModelTable checks the settings descriptions for consistency.
func TestModelTable(t *testing.T) {
	supported := map[string]bool{}
	for _, m := range config.Models() {
		supported[m] = true
	}
	for _, m := range models {
		if !supported[m.Name] {
			t.Errorf("%s is not a supported model", m.Name)
		}
		pm, ok := pairing.ModelByName(m.Name)
		if !ok {
			t.Errorf("%s: no pairing model (receive mode)", m.Name)
		}
		_ = pm
		keys := map[string]bool{}
		for _, s := range m.Settings {
			if keys[s.Key] || s.Key == "" || s.Name == "" || s.Source == "" {
				t.Errorf("%s %s: duplicate or incomplete", m.Name, s.Key)
			}
			keys[s.Key] = true
			if strings.Trim(s.Key, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" || len(s.Key) > 64 {
				t.Errorf("%s: key %q", m.Name, s.Key)
			}
			if s.Loc.Channel > byte(config.ModelChannels(m.Name)) {
				t.Errorf("%s %s: channel %d", m.Name, s.Key, s.Loc.Channel)
			}
			if s.Loc.List != 0 && s.Loc.List != 1 && s.Loc.List != 7 {
				t.Errorf("%s %s: list %d is not MASTER", m.Name, s.Key, s.Loc.List)
			}
			switch s.Kind {
			case Profile:
				if s.Day < 0 || s.Day > 6 || s.ProfileBase != ProfileBase {
					t.Errorf("%s %s", m.Name, s.Key)
				}
				continue
			case Number:
				for _, v := range []float64{s.Min, s.Max} {
					raw, err := s.EncodeNumber(v)
					if err != nil {
						t.Errorf("%s %s: limit %v: %v", m.Name, s.Key, v, err)
						continue
					}
					if got, ok := s.DecodeNumber(raw); !ok || math.Abs(got-v) > 1e-9 {
						t.Errorf("%s %s: %v → %d → %v", m.Name, s.Key, v, raw, got)
					}
				}
			case Select:
				if len(s.Options) < 2 {
					t.Errorf("%s %s: options", m.Name, s.Key)
				}
				for _, o := range s.Options {
					if o.Raw > s.Field.Max() {
						t.Errorf("%s %s: option %s does not fit", m.Name, s.Key, o.Label)
					}
				}
			}
			if !s.Field.valid() {
				t.Errorf("%s %s: field %+v", m.Name, s.Key, s.Field)
			}
			// AES_ACTIVE (list 1 register 8) is set by pairing, never here.
			if s.Loc.List == 1 && s.Field.Index == pairing.RegAESActive {
				t.Errorf("%s %s overlaps AES_ACTIVE", m.Name, s.Key)
			}
			// Nor the central address (list 0 registers 2, 10-12).
			if s.Loc.List == 0 && (s.Field.Index == 2 || s.Field.Index >= 10 && s.Field.Index <= 12) {
				t.Errorf("%s %s overlaps the pairing registers", m.Name, s.Key)
			}
		}
		if len(m.Locs()) == 0 {
			t.Errorf("%s: no lists", m.Name)
		}
	}
	if ForModel("HM-WDS10-TH-O") != nil {
		t.Error("weather sensors have no settings worth exposing")
	}
	if len(Catalog()) < 30 {
		t.Errorf("catalog has %d settings", len(Catalog()))
	}
}

func TestParseParamChange(t *testing.T) {
	// FHEM: “0500 00000000 07 62 2120…” (sequence) and the pairs form.
	loc, peer, r, err := ParseParamChange([]byte{0x04, 0x00, 0, 0, 0, 0, 0x07, 0x02, 0x24, 0x09, 0x08})
	if err != nil || loc != List7 || peer != ([4]byte{}) || !reflect.DeepEqual(r, Regs{2: 0x24, 9: 8}) {
		t.Errorf("pairs: %v %v %v %v", loc, peer, r, err)
	}
	loc, _, r, err = ParseParamChange([]byte{0x05, 0x01, 0, 0, 0, 0, 0x01, 0x21, 0x83, 0x10})
	if err != nil || loc != ChannelList1(1) || !reflect.DeepEqual(r, Regs{0x21: 0x83, 0x22: 0x10}) {
		t.Errorf("seq: %v %v %v", loc, r, err)
	}
	if _, _, r, err := ParseParamChange([]byte{0x05, 0x00, 0, 0, 0, 0, 0x07}); err != nil || len(r) != 0 {
		t.Errorf("finish: %v %v", r, err)
	}
	if _, _, _, err := ParseParamChange([]byte{0x02, 0x01, 0x02}); err == nil {
		t.Error("register read answer accepted")
	}
}
