// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package weather_test

import (
	"testing"

	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/hmtest"
	"github.com/fjaeckel/hmgo/internal/hm/weather"
)

var (
	th = [3]byte{0x6f, 0xca, 0x4a} // the user's HM-WDS10-TH-O
	to = [3]byte{0x78, 0x5b, 0x74} // the user's HM-WDS30-T-O
)

func TestDecodeCapturedFrames(t *testing.T) {
	sTH, _ := weather.NewSensor(hm.NewStandardDevice(nil, th, "Garten"), weather.ModelTH)
	sT, _ := weather.NewSensor(hm.NewStandardDevice(nil, to, "Teich"), weather.ModelT)
	for _, tc := range []struct {
		s        *weather.Sensor
		src      [3]byte
		payload  []byte
		temp     float64
		humidity int // -1: none
		lowbat   bool
	}{
		// hmsniff: WEATHER 6FCA4A(HM-WDS10-TH-O) → broadcast 006450 etc.
		{sTH, th, []byte{0x00, 0x64, 0x50}, 10.0, 80, false},
		{sTH, th, []byte{0x00, 0x62, 0x51}, 9.8, 81, false},
		// 785B74(HM-WDS30-T-O) → broadcast 008264: the third byte is not humidity.
		{sT, to, []byte{0x00, 0x82, 0x64}, 13.0, -1, false},
		{sT, to, []byte{0x00, 0x6f, 0x64}, 11.1, -1, false},
		{sT, to, []byte{0x00, 0x90}, 14.4, -1, false},
		// Frost, and the low battery bit.
		{sTH, th, []byte{0x7f, 0xce, 0x5f}, -5.0, 95, false},
		{sTH, th, []byte{0x80, 0x64, 0x50}, 10.0, 80, true},
		{sT, to, []byte{0xff, 0xf6, 0x64}, -1.0, -1, true},
	} {
		// Real frames: flags 0x86 (WKMEUP|BCAST|RPTEN) to 000000.
		ev, err := tc.s.Decode(hmtest.Packet(0x01, 0x4d, 0x45, 0x86, 0x70, tc.src, [3]byte{}, tc.payload...))
		if err != nil {
			t.Fatalf("%x: %v", tc.payload, err)
		}
		e := ev.(*weather.Event)
		h := -1
		if e.Humidity != nil {
			h = *e.Humidity
		}
		if e.Temperature != tc.temp || h != tc.humidity || e.LowBattery != tc.lowbat {
			t.Errorf("%x: got %v, want %.1f °C %d %% lowbat %v", tc.payload, e, tc.temp, tc.humidity, tc.lowbat)
		}
	}
}

func TestDecodeOddFrames(t *testing.T) {
	s, _ := weather.NewSensor(hm.NewStandardDevice(nil, th, "Garten"), weather.ModelTH)
	for _, p := range [][]byte{{}, {0x00}, {0x00, 0x64}, {0x03, 0x84, 0x50}, {0x00, 0x64, 0xc8}} {
		if _, err := s.Decode(hmtest.Packet(0x01, 0x40, 0x01, 0x86, 0x70, th, [3]byte{}, p...)); err == nil {
			t.Errorf("payload %x: no error", p)
		}
	}
	if ev, err := s.Decode(hmtest.Packet(0x01, 0x40, 0x01, 0x86, 0x41, th, [3]byte{}, 0x01, 0x02, 0x03)); ev != nil || err != nil {
		t.Errorf("cmd 41: %v, %v", ev, err)
	}
	if _, err := weather.NewSensor(hm.NewStandardDevice(nil, th, "x"), "HM-WDS40-TH-I"); err == nil {
		t.Error("unsupported model accepted")
	}
}
