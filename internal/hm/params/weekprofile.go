// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package params

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Week profile of HM-CC-RT-DN and HM-TC-IT-WM-W-EU (program 1), list 7
// (eQ-3 descriptions: ENDTIME_<DAY>_<n> index 20.0 + 2·(n-1) + 26·day,
// size 2.0, mask 511, minutes / 5, 5 … 1440; TEMPERATURE_<DAY>_<n> index
// … .1, size 0.6, °C · 2, 5 … 30; days in the order Saturday … Friday;
// FHEM HMConfig.pm tempListSat … tempListFri, “tempList” encoding). Each
// day has 13 switch times: slot n holds the temperature up to its end
// time, the slot ending at 24:00 is the day's last.
const (
	// ProfileBase is the first register of the week profile.
	ProfileBase = 20
	// SlotsPerDay is the number of switch times per day.
	SlotsPerDay = 13
	// dayRegs is the number of registers per day (13 16-bit words).
	dayRegs = 2 * SlotsPerDay
	// Temperature limits of a slot (°C, 0.5 steps).
	ProfileMinTemp = 5.0
	ProfileMaxTemp = 30.0
	// EndOfDay is 24:00 in minutes.
	EndOfDay = 1440
)

// RegisterDays are the days in register order (0 = Saturday).
var RegisterDays = []string{"saturday", "sunday", "monday", "tuesday", "wednesday", "thursday", "friday"}

// Slot is one switch time: Temp (°C) applies until End (minutes since
// midnight, a multiple of 5, 5 … 1440).
type Slot struct {
	End  int
	Temp float64
}

// Day is a day's profile: 1 … 13 slots with increasing end times, the
// last ending at 24:00.
type Day []Slot

// ValidateDay checks a day's profile.
func ValidateDay(d Day) error {
	if len(d) == 0 || len(d) > SlotsPerDay {
		return fmt.Errorf("a day has 1 … %d switch times, not %d", SlotsPerDay, len(d))
	}
	prev := 0
	for i, s := range d {
		switch {
		case s.End <= prev:
			return fmt.Errorf("switch time %s is not after %s", hhmm(s.End), hhmm(prev))
		case s.End%5 != 0:
			return fmt.Errorf("switch time %s is not a multiple of 5 minutes", hhmm(s.End))
		case s.End > EndOfDay:
			return fmt.Errorf("switch time %s is after 24:00", hhmm(s.End))
		case math.IsNaN(s.Temp) || s.Temp < ProfileMinTemp-eps || s.Temp > ProfileMaxTemp+eps:
			return fmt.Errorf("temperature %v is out of range %v … %v °C", s.Temp, ProfileMinTemp, ProfileMaxTemp)
		case math.Abs(s.Temp*2-math.Round(s.Temp*2)) > eps:
			return fmt.Errorf("temperature %v is not a multiple of 0.5 °C", s.Temp)
		case i == len(d)-1 && s.End != EndOfDay:
			return fmt.Errorf("the last switch time must be 24:00, not %s", hhmm(s.End))
		}
		prev = s.End
	}
	return nil
}

func dayStart(base byte, day int) int { return int(base) + day*dayRegs }

// EncodeDay returns the 26 registers of a day (day 0 = Saturday). Slots
// after the one ending at 24:00 are filled with 24:00 and the last
// temperature, like the devices' defaults. d must be valid (see
// ValidateDay).
func EncodeDay(d Day, base byte, day int) Regs {
	r := make(Regs, dayRegs)
	start := dayStart(base, day)
	for i := 0; i < SlotsPerDay; i++ {
		s := d[len(d)-1]
		if i < len(d) {
			s = d[i]
		}
		w := uint16(math.Round(s.Temp*2))<<9 | uint16(s.End/5)
		r[byte(start+2*i)] = byte(w >> 8)
		r[byte(start+2*i+1)] = byte(w)
	}
	return r
}

// DecodeDay returns a day's profile from the list's registers: the
// slots up to the first one ending at 24:00.
func DecodeDay(r Regs, base byte, day int) (Day, error) {
	start := dayStart(base, day)
	var d Day
	for i := 0; i < SlotsPerDay; i++ {
		hi, ok1 := r[byte(start+2*i)]
		lo, ok2 := r[byte(start+2*i+1)]
		if !ok1 || !ok2 {
			return nil, errors.New("week profile registers not known")
		}
		w := uint16(hi)<<8 | uint16(lo)
		s := Slot{End: int(w&0x1ff) * 5, Temp: float64(w>>9&0x3f) / 2}
		if len(d) > 0 && s.End <= d[len(d)-1].End || s.End > EndOfDay || s.End == 0 {
			return nil, fmt.Errorf("invalid switch time %s in slot %d", hhmm(s.End), i+1)
		}
		d = append(d, s)
		if s.End == EndOfDay {
			return d, nil
		}
	}
	return nil, errors.New("no switch time at 24:00")
}

func hhmm(min int) string { return fmt.Sprintf("%02d:%02d", min/60, min%60) }

// FormatDay renders a day as “06:00 17.0, 22:00 21.0, 24:00 17.0”:
// each entry is an end time and the temperature that applies until
// then (at most 13 entries, the last at 24:00; at most 154 characters).
func FormatDay(d Day) string {
	parts := make([]string, len(d))
	for i, s := range d {
		parts[i] = fmt.Sprintf("%s %.1f", hhmm(s.End), s.Temp)
	}
	return strings.Join(parts, ", ")
}

// ParseDay parses the format of FormatDay (entries separated by “,” or
// “;”, end time and temperature by spaces) and validates the result.
func ParseDay(s string) (Day, error) {
	if len(s) > 512 {
		return nil, errors.New("week profile text too long")
	}
	var d Day
	for _, e := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		f := strings.Fields(e)
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("invalid entry %q (want “HH:MM temperature”, e.g. “06:00 17.5”)", strings.TrimSpace(e))
		}
		end, err := parseHHMM(f[0])
		if err != nil {
			return nil, err
		}
		t, err := strconv.ParseFloat(strings.TrimSuffix(f[1], "°C"), 64)
		if err != nil || math.IsInf(t, 0) {
			return nil, fmt.Errorf("invalid temperature %q", f[1])
		}
		d = append(d, Slot{End: end, Temp: t})
	}
	if err := ValidateDay(d); err != nil {
		return nil, err
	}
	return d, nil
}

func parseHHMM(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || len(m) != 2 || hh < 0 || mm < 0 || mm > 59 || hh > 24 || hh == 24 && mm != 0 {
		return 0, fmt.Errorf("invalid time %q (want HH:MM, 00:05 … 24:00)", s)
	}
	return hh*60 + mm, nil
}
