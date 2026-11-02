// SPDX-License-Identifier: BSD-3-Clause

package uartgw

import (
	"net"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// An HM-MOD-RPI-PCB with eQ-3's dual firmware (OpenCCU with Homematic
// IP; 2.8.6 on a real module): its bootloader is the classic one
// (“Co_CPU_BL”), its application the dual-coprocessor firmware's.
func dualPCB(opts fakemod.Options) fakemod.Options {
	opts.DualCoPro, opts.ClassicBootloader, opts.Firmware = true, true, [3]byte{2, 8, 6}
	return opts
}

func TestOpenDualFirmwareHMModRPIPCB(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts fakemod.Options
		open func(rw net.Conn, cfg Config) (Module, error)
	}{
		{"auto, cold start", dualPCB(fakemod.Options{}), func(rw net.Conn, cfg Config) (Module, error) { return OpenModule(rw, cfg) }},
		{"auto, app running", dualPCB(fakemod.Options{StartInApp: true}), func(rw net.Conn, cfg Config) (Module, error) { return OpenModule(rw, cfg) }},
		{"rpi-rf-mod, cold start", dualPCB(fakemod.Options{}), func(rw net.Conn, cfg Config) (Module, error) { return OpenDual(rw, cfg) }},
		{"rpi-rf-mod, app running", dualPCB(fakemod.Options{StartInApp: true}), func(rw net.Conn, cfg Config) (Module, error) { return OpenDual(rw, cfg) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mod, rw := fakemod.New(tt.opts)
			defer mod.Close()
			m, err := tt.open(rw.(net.Conn), dualCfg(Config{Timeout: 300 * time.Millisecond}))
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			dc, ok := m.(*DualConn)
			if !ok {
				t.Fatalf("got %T, want *DualConn", m)
			}
			if m.Kind() != KindHMModRPIPCB || dc.Application() != fakemod.TagDualApp || dc.FirmwareVersion() != "2.8.6" || !mod.InApp() {
				t.Errorf("kind %q, application %q, firmware %q, in app %v", m.Kind(), dc.Application(), dc.FirmwareVersion(), mod.InApp())
			}
		})
	}
}

func TestBoardForFirmware(t *testing.T) {
	for fw, want := range map[string]string{"2.8.6": KindHMModRPIPCB, "4.4.22": KindRPIRFMod, "5.1.0": KindRPIRFMod, "": KindRPIRFMod} {
		if got := boardForFirmware(fw); got != want {
			t.Errorf("%q: %q, want %q", fw, got, want)
		}
	}
}

func TestProbeKindDualFirmwareHMModRPIPCB(t *testing.T) {
	for name, opts := range map[string]fakemod.Options{
		"bootloader announcing": dualPCB(fakemod.Options{}),
		"app running":           dualPCB(fakemod.Options{StartInApp: true}),
	} {
		mod, rw := fakemod.New(opts)
		got, err := ProbeKind(rw.(net.Conn), 300*time.Millisecond)
		mod.Close()
		if err != nil || got != KindHMModRPIPCB {
			t.Errorf("%s: kind %q, %v; want %q", name, got, err, KindHMModRPIPCB)
		}
	}
}
