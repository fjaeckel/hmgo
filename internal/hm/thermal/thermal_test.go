// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see LICENSE.

package thermal_test

import (
	"testing"

	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/thermal"
)

func TestDecodeWeatherEvent(t *testing.T) {
	tc := thermal.NewThermalControl(hm.NewStandardDevice(nil, [3]byte{0xaa, 0xbb, 0xcc}, "test"))
	we, err := tc.DecodeWeatherEvent([]byte{0, 253, 57})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := we.Temperature, 25.3; got != want {
		t.Fatalf("unexpected temperature: got %v, want %v", got, want)
	}
	if got, want := we.Humidity, uint64(57); got != want {
		t.Fatalf("unexpected humidity: got %v, want %v", got, want)
	}
}

func TestDecodeThermalControlEvent(t *testing.T) {
	tc := thermal.NewThermalControl(hm.NewStandardDevice(nil, [3]byte{0xaa, 0xbb, 0xcc}, "test"))
	tce, err := tc.DecodeThermalControlEvent([]byte{200, 215, 65})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tce.SetTemperature, 25.0; got != want {
		t.Fatalf("unexpected set temperature: got %v, want %v", got, want)
	}
	if got, want := tce.ActualTemperature, 21.5; got != want {
		t.Fatalf("unexpected actual temperature: got %v, want %v", got, want)
	}
	if got, want := tce.ActualHumidity, 65.0; got != want {
		t.Fatalf("unexpected humidity: got %v, want %v", got, want)
	}
}

func TestDecodeInfoEvent(t *testing.T) {
	tc := thermal.NewThermalControl(hm.NewStandardDevice(nil, [3]byte{0xaa, 0xbb, 0xcc}, "test"))
	ie, err := tc.DecodeInfoEvent([]byte{0x0b, 0xb0, 0xdf, 0x0e, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("info event: %+v", ie)
	if got, want := ie.SetTemperature, 22.0; got != want {
		t.Fatalf("unexpected set temperature: got %v, want %v", got, want)
	}
	if got, want := ie.ActualTemperature, 22.3; got != want {
		t.Fatalf("unexpected actual temperature: got %v, want %v", got, want)
	}
	if got, want := ie.BatteryState, 2.9; got != want {
		t.Fatalf("unexpected battery state: got %v, want %v", got, want)
	}
}

// TestDecodeThermalControlEventHighTemperature exercises ActualTemperature
// at/above 25.6 °C — values that require the high 2 bits of the 10-bit
// field (payload[0] bits 1..0) to be combined with payload[1] via a
// left-shift by 8. The previous decoder OR-ed them at bit 0 instead,
// silently wrapping every reading >=25.6 °C to 0..3.2 °C.
func TestDecodeThermalControlEventHighTemperature(t *testing.T) {
	tc := thermal.NewThermalControl(hm.NewStandardDevice(nil, [3]byte{0xaa, 0xbb, 0xcc}, "test"))
	// payload[0] = 0b10110001: SetTemperature bits = 0b101100 = 44 -> 22.0 °C,
	// high 2 bits of ActualTemperature = 0b01 (carries 256).
	// payload[1] = 0x0b = 11. Combined: (1<<8)|11 = 267 -> 26.7 °C.
	tce, err := tc.DecodeThermalControlEvent([]byte{0xb1, 0x0b, 52})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tce.SetTemperature, 22.0; got != want {
		t.Fatalf("unexpected set temperature: got %v, want %v", got, want)
	}
	if got, want := tce.ActualTemperature, 26.7; got != want {
		t.Fatalf("unexpected actual temperature: got %v, want %v", got, want)
	}
	if got, want := tce.ActualHumidity, 52.0; got != want {
		t.Fatalf("unexpected humidity: got %v, want %v", got, want)
	}
}

// TestDecodeInfoEventHighTemperature is the InfoEvent counterpart to
// TestDecodeThermalControlEventHighTemperature — same 10-bit decode bug,
// different frame.
func TestDecodeInfoEventHighTemperature(t *testing.T) {
	tc := thermal.NewThermalControl(hm.NewStandardDevice(nil, [3]byte{0xaa, 0xbb, 0xcc}, "test"))
	// payload[1] = 0b10110001 -> SetTemperature 22.0 °C, high temp bits 0b01.
	// payload[2] = 0x09 -> low byte 9. Combined: (1<<8)|9 = 265 -> 26.5 °C.
	ie, err := tc.DecodeInfoEvent([]byte{0x0b, 0xb1, 0x09, 0x0e, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("info event: %+v", ie)
	if got, want := ie.SetTemperature, 22.0; got != want {
		t.Fatalf("unexpected set temperature: got %v, want %v", got, want)
	}
	if got, want := ie.ActualTemperature, 26.5; got != want {
		t.Fatalf("unexpected actual temperature: got %v, want %v", got, want)
	}
}
