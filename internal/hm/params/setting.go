// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package params

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind is how a setting is presented (and becomes a Home Assistant
// entity: number, select, switch or text).
type Kind int

const (
	// Number: a value in [Min, Max] in steps of Step.
	Number Kind = iota
	// Select: one of Options.
	Select
	// Switch: on or off (raw 1 / 0; any non-zero raw value reads as on).
	Switch
	// Profile: one day of a thermostat's week profile (text, see
	// ParseDay).
	Profile
)

func (k Kind) String() string {
	switch k {
	case Number:
		return "number"
	case Select:
		return "select"
	case Switch:
		return "switch"
	case Profile:
		return "text"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Option is a value of a Select setting.
type Option struct {
	Label string
	Raw   uint32
}

// Setting is one device setting.
type Setting struct {
	Key  string // stable identifier, e.g. "temperature_offset"
	Name string // human readable, e.g. "Temperature offset"
	Kind Kind
	Loc  Loc
	// Field is the setting's place in the list (unused for Profile).
	Field Field

	// Number: valid values are Min … Max in steps of Step; the raw
	// value is round((v - Offset) · Scale) (eQ-3 decimalIntegerScale /
	// integerIntegerScale; Scale 0 means 1).
	Min, Max, Step float64
	Offset, Scale  float64
	// ConfigTime60 selects eQ-3's “config time” encoding with the
	// factors 1 and 60 (decimalConfigTime, value size 0.7; FHEM
	// fltCvT60): bit 7 selects the factor, bits 0-6 hold the value.
	ConfigTime60 bool
	Unit         string

	// Select: the options in display order.
	Options []Option

	// Profile: the day (0 = Saturday … 6 = Friday, the register order)
	// and the first register of the week profile.
	Day         int
	ProfileBase byte

	// Enabled: shown in Home Assistant by default (rarely used settings
	// are disabled by default).
	Enabled bool
	Icon    string
	// Source names where the layout comes from.
	Source string
}

func (s *Setting) scale() float64 {
	if s.Scale == 0 {
		return 1
	}
	return s.Scale
}

// eps is the tolerance for comparing decimal values.
const eps = 1e-6

// ParseValue validates a value as sent by Home Assistant (a number, an
// option label, ON/OFF) and returns its raw register value. Values
// outside the valid range or between steps are rejected: nothing
// out-of-range is ever written.
func (s *Setting) ParseValue(p string) (uint32, error) {
	p = strings.TrimSpace(p)
	switch s.Kind {
	case Number:
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("%s: invalid number %q", s.Key, p)
		}
		return s.EncodeNumber(v)
	case Select:
		for _, o := range s.Options {
			if strings.EqualFold(o.Label, p) {
				return o.Raw, nil
			}
		}
		labels := make([]string, len(s.Options))
		for i, o := range s.Options {
			labels[i] = o.Label
		}
		return 0, fmt.Errorf("%s: invalid option %q (want one of %s)", s.Key, p, strings.Join(labels, ", "))
	case Switch:
		switch strings.ToLower(p) {
		case "on", "true", "1":
			return 1, nil
		case "off", "false", "0":
			return 0, nil
		}
		return 0, fmt.Errorf("%s: invalid switch value %q (want ON or OFF)", s.Key, p)
	}
	return 0, fmt.Errorf("%s: not a register setting", s.Key)
}

// EncodeNumber validates v and returns the raw value of a Number
// setting.
func (s *Setting) EncodeNumber(v float64) (uint32, error) {
	if s.Kind != Number {
		return 0, fmt.Errorf("%s: not a number", s.Key)
	}
	if math.IsNaN(v) || v < s.Min-eps || v > s.Max+eps {
		return 0, fmt.Errorf("%s: %v is out of range %v … %v%s", s.Key, v, trim(s.Min), trim(s.Max), unitSuffix(s.Unit))
	}
	if s.Step > 0 {
		n := (v - s.Min) / s.Step
		if math.Abs(n-math.Round(n)) > eps*1e3 {
			return 0, fmt.Errorf("%s: %v is not a multiple of %v%s (from %v)", s.Key, v, trim(s.Step), unitSuffix(s.Unit), trim(s.Min))
		}
		v = s.Min + math.Round(n)*s.Step
	}
	var raw uint32
	if s.ConfigTime60 {
		r := math.Round(v)
		switch {
		case math.Abs(v-r) > eps:
			return 0, fmt.Errorf("%s: %v is not a whole number of seconds", s.Key, v)
		case r <= 127:
			raw = uint32(r)
		case math.Mod(r, 60) == 0 && r/60 <= 127:
			raw = 0x80 | uint32(r/60)
		default:
			return 0, fmt.Errorf("%s: %v s cannot be stored: above 127 s only whole minutes are possible", s.Key, v)
		}
	} else {
		f := math.Round((v - s.Offset) * s.scale())
		if f < 0 {
			return 0, fmt.Errorf("%s: %v is out of range", s.Key, v)
		}
		raw = uint32(f)
	}
	if raw > s.Field.Max() {
		return 0, fmt.Errorf("%s: %v does not fit the register", s.Key, v)
	}
	return raw, nil
}

// DecodeNumber returns the value of a raw Number register value; ok is
// false if it lies outside the valid range (the device holds a value a
// CCU would not write).
func (s *Setting) DecodeNumber(raw uint32) (float64, bool) {
	var v float64
	if s.ConfigTime60 {
		v = float64(raw & 0x7f)
		if raw&0x80 != 0 {
			v *= 60
		}
	} else {
		v = float64(raw)/s.scale() + s.Offset
	}
	v = math.Round(v*1e6) / 1e6
	if v < s.Min-eps || v > s.Max+eps {
		return v, false
	}
	return v, true
}

// Value returns the setting's current value from the registers of its
// list, as published to Home Assistant: float64 (Number), the option
// label (Select), "ON"/"OFF" (Switch), the formatted day (Profile). ok
// is false if the registers are not known or hold an invalid value.
func (s *Setting) Value(r Regs) (any, bool) {
	if s.Kind == Profile {
		d, err := DecodeDay(r, s.ProfileBase, s.Day)
		if err != nil {
			return nil, false
		}
		return FormatDay(d), true
	}
	raw, ok := s.Field.Get(r)
	if !ok {
		return nil, false
	}
	return s.valueOf(raw)
}

func (s *Setting) valueOf(raw uint32) (any, bool) {
	switch s.Kind {
	case Number:
		if v, ok := s.DecodeNumber(raw); ok {
			return v, true
		}
		return nil, false
	case Select:
		for _, o := range s.Options {
			if o.Raw == raw {
				return o.Label, true
			}
		}
		return nil, false
	case Switch:
		if raw != 0 {
			return "ON", true
		}
		return "OFF", true
	}
	return nil, false
}

// FormatRaw renders a raw value for logs and status messages.
func (s *Setting) FormatRaw(raw uint32) string {
	v, ok := s.valueOf(raw)
	if !ok {
		return fmt.Sprintf("raw %d", raw)
	}
	if f, isNum := v.(float64); isNum {
		return trim(f) + unitSuffix(s.Unit)
	}
	return fmt.Sprint(v)
}

func trim(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func unitSuffix(u string) string {
	if u == "" {
		return ""
	}
	return " " + u
}

// Change is a validated change of one setting.
type Change struct {
	Setting *Setting
	Raw     uint32 // register settings
	Slots   Day    // Profile settings
}

// NewChange validates a value (as sent by Home Assistant) for s.
func NewChange(s *Setting, value string) (Change, error) {
	if s.Kind == Profile {
		d, err := ParseDay(value)
		if err != nil {
			return Change{}, fmt.Errorf("%s: %w", s.Key, err)
		}
		return Change{Setting: s, Slots: d}, nil
	}
	raw, err := s.ParseValue(value)
	if err != nil {
		return Change{}, err
	}
	return Change{Setting: s, Raw: raw}, nil
}

// Known reports whether r holds the registers needed to apply c (the
// other bits of shared registers).
func (c Change) Known(r Regs) bool {
	if c.Setting.Kind == Profile {
		return true // a day's registers are written completely
	}
	return c.Setting.Field.Known(r)
}

// Apply stores the change in r.
func (c Change) Apply(r Regs) error {
	if c.Setting.Kind == Profile {
		for a, v := range EncodeDay(c.Slots, c.Setting.ProfileBase, c.Setting.Day) {
			r[a] = v
		}
		return nil
	}
	return c.Setting.Field.Put(r, c.Raw)
}

// Verify checks that r (read back from the device) holds the change.
func (c Change) Verify(r Regs) error {
	if c.Setting.Kind == Profile {
		want := EncodeDay(c.Slots, c.Setting.ProfileBase, c.Setting.Day)
		for a, v := range want {
			if got, ok := r[a]; !ok || got != v {
				return fmt.Errorf("read back register %d = %s, wrote %02x", a, regString(r, a), v)
			}
		}
		return nil
	}
	raw, ok := c.Setting.Field.Get(r)
	if !ok {
		return fmt.Errorf("register %d missing in the read-back list", c.Setting.Field.Index)
	}
	if raw != c.Raw {
		return fmt.Errorf("device reports %s after writing %s", c.Setting.FormatRaw(raw), c.Setting.FormatRaw(c.Raw))
	}
	return nil
}

// String renders the new value.
func (c Change) String() string {
	if c.Setting.Kind == Profile {
		return FormatDay(c.Slots)
	}
	return c.Setting.FormatRaw(c.Raw)
}

func regString(r Regs, a byte) string {
	if v, ok := r[a]; ok {
		return fmt.Sprintf("%02x", v)
	}
	return "missing"
}
