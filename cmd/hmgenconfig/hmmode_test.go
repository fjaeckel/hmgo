// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fjaeckel/hmgo/internal/config"
)

func TestReadHMModeRadio(t *testing.T) {
	for _, c := range []struct {
		name, hmMode, want string
	}{
		{"RPI-RF-MOD", "HM_HMRF_DEV='RPI-RF-MOD'\nHM_HMRF_VERSION='4.4.22'\n", config.RadioRPIRFMod},
		{"HM-MOD-RPI-PCB classic firmware", "HM_HMRF_DEV='HM-MOD-RPI-PCB'\nHM_HMRF_VERSION='1.4.1'\n", config.RadioHMModRPIPCB},
		// OpenCCU with Homematic IP on an HM-MOD-RPI-PCB: dual firmware,
		// which the hm-mod-rpi-pcb driver refuses; "rpi-rf-mod" would
		// pick the wrong reset line.
		{"HM-MOD-RPI-PCB dual firmware", "HM_HMRF_DEV='HM-MOD-RPI-PCB'\nHM_HMRF_VERSION='2.8.6'\n", ""},
		{"HM-MOD-RPI-PCB unknown firmware", "HM_HMRF_DEV='HM-MOD-RPI-PCB'\n", ""},
	} {
		path := filepath.Join(t.TempDir(), "hm_mode")
		if err := os.WriteFile(path, []byte(c.hmMode), 0o644); err != nil {
			t.Fatal(err)
		}
		g := &generator{src: Sources{HMModePath: path}}
		if got := g.readHMMode(); got != c.want {
			t.Errorf("%s: radio %q, want %q", c.name, got, c.want)
		}
	}
}
