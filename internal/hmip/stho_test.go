// SPDX-License-Identifier: BSD-3-Clause

package hmip

import (
	"encoding/hex"
	"errors"
	"testing"
)

func TestParseSTHOStatus(t *testing.T) {
	for _, c := range []struct {
		name, payload string
		want          STHOStatus
	}{
		// Recorded STATUS frame of an HmIP-STHO under OpenCCU, 2026-11-01
		// (application payload after the 2-byte header); OpenCCU showed
		// 10.4 °C, 77 % and an operating voltage of 2.6 V at the time.
		{"recorded", "008003001a060000010100684d", STHOStatus{Temperature: 10.4, Humidity: 77, Voltage: 2.6}},
		// Synthetic: −5.3 °C as two's complement (assumed, not yet recorded).
		{"negative", "008003001e0600000101ffcb5a", STHOStatus{Temperature: -5.3, Humidity: 90, Voltage: 3.0}},
	} {
		b, _ := hex.DecodeString(c.payload)
		got, err := ParseSTHOStatus(b)
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	for _, n := range []int{0, 12, 14} {
		if _, err := ParseSTHOStatus(make([]byte, n)); !errors.Is(err, ErrUnsupported) {
			t.Errorf("len %d: err %v, want ErrUnsupported", n, err)
		}
	}
}
