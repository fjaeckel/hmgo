// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package devices creates the device model (decoder and command
// encoder) for an eQ-3 model name. It is shared by hmbridge and
// hmsniff.
package devices

import (
	"fmt"

	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/contact"
	"github.com/fjaeckel/hmgo/internal/hm/heating"
	"github.com/fjaeckel/hmgo/internal/hm/led16"
	"github.com/fjaeckel/hmgo/internal/hm/power"
	"github.com/fjaeckel/hmgo/internal/hm/remote"
	"github.com/fjaeckel/hmgo/internal/hm/thermal"
	"github.com/fjaeckel/hmgo/internal/hm/weather"
)

// New returns the device model for model. sd.BCS may be nil for
// decoding only.
func New(sd hm.StandardDevice, model string) (hm.Device, error) {
	switch model {
	case heating.Model:
		return heating.NewThermostat(sd), nil
	case thermal.Model:
		return thermal.NewThermalControl(sd), nil
	case contact.ModelSCo, contact.ModelSC2, contact.ModelSC, contact.ModelRHS:
		return contact.NewContact(sd, model)
	case power.ModelSwitchPlug, power.ModelDimmerPlug:
		return power.NewActuator(sd, model)
	case remote.ModelPB2WM55, remote.ModelRC8:
		return remote.NewRemote(sd, model)
	case led16.Model:
		return led16.NewDisplay(sd), nil
	case weather.ModelTH, weather.ModelT:
		return weather.NewSensor(sd, model)
	}
	return nil, fmt.Errorf("unsupported model %q", model)
}
