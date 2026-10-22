// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see LICENSE.

package power_test

import (
	"testing"

	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/power"
)

func TestDecodePowerEvent(t *testing.T) {
	ps := power.NewPowerSwitch(hm.NewStandardDevice(nil, [3]byte{0xaa, 0xbb, 0xcc}, "test"))

	// payload captured measuring a Raspberry Pi 3 :)
	pe, err := ps.DecodePowerEvent([]byte{128, 3, 138, 0, 0, 187, 0, 16, 9, 8, 255})
	if err != nil {
		t.Fatal(err)
	}

	if got, want := pe.Boot, true; got != want {
		t.Fatalf("unexpected boot: got %v, want %v", got, want)
	}
	if got, want := pe.EnergyCounter, 90.6; got != want {
		t.Fatalf("unexpected energy counter: got %v, want %v", got, want)
	}
	if got, want := pe.Power, 1.87; got != want {
		t.Fatalf("unexpected power: got %v, want %v", got, want)
	}
	if got, want := pe.Current, 16.0; got != want {
		t.Fatalf("unexpected current: got %v, want %v", got, want)
	}
	if got, want := pe.Voltage, 231.2; got != want {
		t.Fatalf("unexpected voltage: got %v, want %v", got, want)
	}
	if got, want := pe.Frequency, 52.55; got != want {
		t.Fatalf("unexpected frequency: got %v, want %v", got, want)
	}
}
