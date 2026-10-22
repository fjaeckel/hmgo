// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see LICENSE.

// Package heating implements the HM-CC-RT-DN radiator thermostat.
//
// Frames the device sends to the central (eQ-3 rftypes rf_cc_rt_dn.xml,
// FHEM CUL_HM_Parse “HM-CC-RT-DN”):
//
//	0x10 0x0A  INFO_LEVEL      → *InfoEvent (cyclic, ~every 3 min, and on change)
//	0x02 0x01  ACK_STATUS      → *AckStatusEvent (response to our commands)
//	0x59       climate team    → *TeamEvent (to its team peers, if any)
//	0x3F       time request    → *hm.TimeRequest
//	0x00       DEVICE_INFO     → *hm.DeviceInfo (config button pressed)
//
// The RT-DN does not send INFO_ACTUATOR_STATUS (0x10 0x06) or
// CLIMATE_EVENT (0x58; that is the older HM-CC-TC/HM-CC-VD pair).
package heating

import (
	"context"
	"sync"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
)

// Model is the eQ-3 model name.
const Model = "HM-CC-RT-DN"

// channels
const (
	WeatherReceiver        = 0x01
	ClimateControlReceiver = 0x02
	WindowSwitchReceiver   = 0x03
	// ClimateControl is the control channel (CLIMATECONTROL_RT_TRANSCEIVER):
	// commands are sent to it and INFO_LEVEL reports it.
	ClimateControl           = 0x04
	ClimateControlRTReceiver = 0x05
	RemoteControlReceiver    = 0x06
)

// Thermostat represents a HM-CC-RT-DN heating thermostat. Its manual
// can be found at
// http://www.eq-3.de/Downloads/eq3/downloads_produktkatalog/homematic/bda/HM-CC-RT-DN_UM_GE_eQ-3_web.pdf
type Thermostat struct {
	hm.StandardDevice

	latestInfoEvent *InfoEvent
	latestMu        sync.RWMutex
}

func (t *Thermostat) HomeMaticType() string { return "heating" }

// NewThermostat returns a Thermostat. sd.BCS may be nil for
// decoding-only use.
func NewThermostat(sd hm.StandardDevice) *Thermostat {
	sd.NumChannels = 6
	sd.ModelName = Model
	return &Thermostat{StandardDevice: sd}
}

func (t *Thermostat) MostRecentEvents() []hm.Event {
	var result []hm.Event
	t.latestMu.RLock()
	defer t.latestMu.RUnlock()

	if t.latestInfoEvent != nil {
		result = append(result, t.latestInfoEvent)
	}

	return result
}

// Decode decodes a packet received from the thermostat. See the package
// documentation for the returned types. It returns nil, nil for frames
// that carry no information for the bridge (e.g. plain ACKs).
func (t *Thermostat) Decode(pkt *bidcos.Packet) (any, error) {
	if err := t.CheckIncoming(pkt); err != nil {
		return nil, err
	}
	switch {
	case pkt.Cmd == bidcos.Info && len(pkt.Payload) > 0 && pkt.Payload[0] == bidcos.InfoTemp:
		return t.DecodeInfoEvent(pkt.Payload)
	case hm.IsAckStatus(pkt):
		return t.DecodeAckStatus(pkt.Payload)
	case pkt.Cmd == CmdClimateTeam:
		return DecodeTeamEvent(pkt.Payload)
	}
	return hm.DecodeCommon(pkt)
}

// SetTemperaturePacket returns the packet setting the target
// temperature (4.5 = off … 30.5 = on, rounded to 0.5) without changing
// the mode.
func SetTemperaturePacket(dst [3]byte, temp float64) (*bidcos.Packet, error) {
	return hm.ClimateCommandPacket(dst, ClimateControl, hm.CmdSetTemperature, temp)
}

// AutoModePacket returns the packet switching to auto (week program) mode.
func AutoModePacket(dst [3]byte) *bidcos.Packet {
	p, _ := hm.ClimateCommandPacket(dst, ClimateControl, hm.CmdAuto, 0)
	return p
}

// ManualModePacket returns the packet switching to manual mode at temp.
func ManualModePacket(dst [3]byte, temp float64) (*bidcos.Packet, error) {
	return hm.ClimateCommandPacket(dst, ClimateControl, hm.CmdManual, temp)
}

// BoostPacket returns the packet starting boost mode (valve fully open
// for the configured boost duration, default 5 minutes).
func BoostPacket(dst [3]byte) *bidcos.Packet {
	p, _ := hm.ClimateCommandPacket(dst, ClimateControl, hm.CmdBoost, 0)
	return p
}

func (t *Thermostat) command(ctx context.Context, cmd hm.ClimateCommand, temp float64) (*AckStatusEvent, error) {
	resp, err := t.SendClimateCommand(ctx, ClimateControl, cmd, temp)
	if err != nil {
		return nil, err
	}
	if !hm.IsAckStatus(resp) {
		return nil, nil
	}
	// A malformed status does not make the command fail.
	st, _ := t.DecodeAckStatus(resp.Payload)
	return st, nil
}

// SetTemperature sets the target temperature (4.5 = off … 30.5 = on,
// rounded to 0.5 °C). It returns the device's ACK_STATUS, or nil if the
// device answered with a plain ACK.
func (t *Thermostat) SetTemperature(ctx context.Context, temp float64) (*AckStatusEvent, error) {
	return t.command(ctx, hm.CmdSetTemperature, temp)
}

// SetAutoMode switches to auto mode (week program).
func (t *Thermostat) SetAutoMode(ctx context.Context) (*AckStatusEvent, error) {
	return t.command(ctx, hm.CmdAuto, 0)
}

// SetManualMode switches to manual mode with target temperature temp.
func (t *Thermostat) SetManualMode(ctx context.Context, temp float64) (*AckStatusEvent, error) {
	return t.command(ctx, hm.CmdManual, temp)
}

// SetBoost starts boost mode.
func (t *Thermostat) SetBoost(ctx context.Context) (*AckStatusEvent, error) {
	return t.command(ctx, hm.CmdBoost, 0)
}
