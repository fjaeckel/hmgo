// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package devices_test

import (
	"testing"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/devices"
)

// Every model the configuration accepts has a decoder and a Home
// Assistant kind, with the same channel count.
func TestAllConfigModels(t *testing.T) {
	for _, m := range config.Models() {
		d, err := devices.New(hm.NewStandardDevice(nil, [3]byte{1, 2, 3}, "x"), m)
		if err != nil {
			t.Errorf("%s: %v", m, err)
			continue
		}
		if d.Model() != m || d.Channels() != config.ModelChannels(m) {
			t.Errorf("%s: model %q, %d channels, config says %d", m, d.Model(), d.Channels(), config.ModelChannels(m))
		}
		if _, ok := ha.KindForModel(m); !ok {
			t.Errorf("%s: no Home Assistant kind", m)
		}
	}
	if _, err := devices.New(hm.NewStandardDevice(nil, [3]byte{1, 2, 3}, "x"), "HM-RCV-50"); err == nil {
		t.Error("HM-RCV-50 accepted")
	}
}
