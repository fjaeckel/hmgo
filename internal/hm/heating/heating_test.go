package heating_test

import (
	"testing"

	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/heating"
)

func TestDecodeInfoEvent(t *testing.T) {
	ts := heating.NewThermostat(hm.NewStandardDevice(nil, [3]byte{0xaa, 0xbb, 0xcc}, "test"))
	ie, err := ts.DecodeInfoEvent([]byte{0x0a, 0xb0, 0xe2, 0x08, 0x00, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("info event: %+v", ie)
	if got, want := ie.SetTemperature, 22.0; got != want {
		t.Fatalf("unexpected set temperature: got %v, want %v", got, want)
	}
	if got, want := ie.ActualTemperature, 22.6; got != want {
		t.Fatalf("unexpected actual temperature: got %v, want %v", got, want)
	}
	if got, want := ie.BatteryState, 2.3; got != want {
		t.Fatalf("unexpected battery state: got %v, want %v", got, want)
	}
}

// TestDecodeInfoEventHighTemperature exercises ActualTemperature values
// at/above 25.6 °C, which require the high 2 bits of the 10-bit field
// (in payload[1] bits 1..0) to be combined with payload[2] via a
// left-shift by 8. A previous decoder OR-ed them at bit 0 instead, which
// silently worked below 25.6 °C and wrapped to 0..3.2 °C above it.
func TestDecodeInfoEventHighTemperature(t *testing.T) {
	ts := heating.NewThermostat(hm.NewStandardDevice(nil, [3]byte{0xaa, 0xbb, 0xcc}, "test"))
	// payload[1] = 0b10110001: SetTemperature bits = 0b101100 = 44 -> 22.0 °C,
	// high 2 bits of ActualTemperature = 0b01 (carries 256).
	// payload[2] = 0x09 = 9. Combined: (1<<8)|9 = 265 -> 26.5 °C.
	ie, err := ts.DecodeInfoEvent([]byte{0x0a, 0xb1, 0x09, 0x08, 0x00, 0x00})
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
