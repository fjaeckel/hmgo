// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package uartgw

import (
	"net"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

func TestProbeKind(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts fakemod.Options
		want string
	}{
		{"rpi-rf-mod app", fakemod.Options{DualCoPro: true, StartInApp: true}, KindRPIRFMod},
		{"rpi-rf-mod bootloader", fakemod.Options{DualCoPro: true}, KindRPIRFMod},
		// The emulation NACKs the unknown destination (the real classic
		// firmware may ignore it instead: then "" after the timeout).
		{"classic app", fakemod.Options{StartInApp: true}, KindHMModRPIPCB},
		{"classic bootloader announcing", fakemod.Options{}, KindHMModRPIPCB},
		{"mute", fakemod.Options{Mute: true}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mod, rw := fakemod.New(tt.opts)
			defer mod.Close()
			start := time.Now()
			got, err := ProbeKind(rw.(net.Conn), 300*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("kind %q, want %q", got, tt.want)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("took %v", d)
			}
		})
	}
}

// After the probe, the module is reset (here: emulated) and opened
// normally.
func TestProbeThenOpen(t *testing.T) {
	mod, rw := fakemod.New(fakemod.Options{DualCoPro: true, StartInApp: true})
	defer mod.Close()
	kind, err := ProbeKind(rw.(net.Conn), 300*time.Millisecond)
	if err != nil || kind != KindRPIRFMod {
		t.Fatalf("probe: %q %v", kind, err)
	}
	go mod.Reset() // the announcement is written once OpenModule reads
	m, err := OpenModule(rw, Config{HMID: [3]byte{1, 2, 3}, Timeout: time.Second, AnnounceWait: time.Second, TRXPollInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Kind() != KindRPIRFMod || m.FirmwareVersion() != "4.4.22" {
		t.Errorf("%s %s", m.Kind(), m.FirmwareVersion())
	}
}
