// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see licenses/.

package thermal

import (
	"context"
	"sync"
	"time"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
)

// Model is the eQ-3 model name.
const Model = "HM-TC-IT-WM-W-EU"

const prometheusNamespace = "hmthermal"

// channels
const (
	WeatherTransmit = 0x01
	// ThermalControlTransmit is the control channel: commands are sent
	// to it and INFO_LEVEL/THERMALCONTROL_EVENT report it.
	ThermalControlTransmit = 0x02
	WindowSwitchReceiver   = 0x03
	RemoteControlReceiver  = 0x06
	SwitchTransmit         = 0x07
)

const (
	WeekdayMask = 1<<uint(time.Monday) |
		1<<uint(time.Tuesday) |
		1<<uint(time.Wednesday) |
		1<<uint(time.Thursday) |
		1<<uint(time.Friday)
	WeekendMask = 1<<uint(time.Saturday) | 1<<uint(time.Sunday)
)

type Program struct {
	DayMask  int
	Endtimes [13]ProgramEntry
}

type ProgramEntry struct {
	Endtime     uint64
	Temperature float64
}

// ThermalControl represents a HM-TC-IT-WM-W-EU remote temperature
// sensor and thermostat control unit.
//
// Its manual can be found at
// http://www.eq-3.de/Downloads/eq3/downloads_produktkatalog/homematic/bda/HM-TC-IT-WM-W-EU_UM_GE_eQ-3_web.pdf
//
// Further details can be found at
// https://wiki.fhem.de/wiki/HM-TC-IT-WM-W-EU_Funk-Wandthermostat_AP
type ThermalControl struct {
	hm.StandardDevice

	latestWeatherEvent        *WeatherEvent
	latestThermalControlEvent *ThermalControlEvent
	latestInfoEvent           *InfoEvent
	latestMu                  sync.RWMutex
}

func (tc *ThermalControl) HomeMaticType() string { return "thermal" }

// NewThermalControl returns a ThermalControl. sd.BCS may be nil for
// decoding-only use.
func NewThermalControl(sd hm.StandardDevice) *ThermalControl {
	sd.NumChannels = 7
	sd.ModelName = Model
	return &ThermalControl{StandardDevice: sd}
}

// Decode decodes a packet received from the wall thermostat:
//
//	0x10 0x0B  INFO_LEVEL           → *InfoEvent
//	0x02 0x01  ACK_STATUS           → *AckStatusEvent
//	0x5A       THERMALCONTROL_EVENT → *ThermalControlEvent (to its RT-DN team, ~every 2-3 min)
//	0x70       WEATHER_EVENT        → *WeatherEvent
//	0x41       SWITCH_EVENT (ch 7)  → *SwitchEvent (to peers; not normally sent to the central)
//	0x3F       time request         → *hm.TimeRequest
//	0x00       DEVICE_INFO          → *hm.DeviceInfo
//
// It returns nil, nil for other frames. The TC sends neither
// INFO_ACTUATOR_STATUS (0x10 0x06) nor CLIMATE_EVENT (0x58) (eQ-3
// rftypes rf_tc_it_wm-w-eu.xml).
func (tc *ThermalControl) Decode(pkt *bidcos.Packet) (any, error) {
	if err := tc.CheckIncoming(pkt); err != nil {
		return nil, err
	}
	switch {
	case pkt.Cmd == bidcos.Info && len(pkt.Payload) > 0 && pkt.Payload[0] == InfoLevelSubtype:
		return tc.DecodeInfoEvent(pkt.Payload)
	case hm.IsAckStatus(pkt):
		return tc.DecodeAckStatus(pkt.Payload)
	case pkt.Cmd == bidcos.ThermalControl:
		return tc.DecodeThermalControlEvent(pkt.Payload)
	case pkt.Cmd == bidcos.WeatherEvent:
		return tc.DecodeWeatherEvent(pkt.Payload)
	case pkt.Cmd == CmdSwitchEvent:
		return DecodeSwitchEvent(pkt.Payload)
	}
	return hm.DecodeCommon(pkt)
}

// SetTemperaturePacket returns the packet setting the target
// temperature (4.5 = off … 30.5 = on, rounded to 0.5) without changing
// the mode.
func SetTemperaturePacket(dst [3]byte, temp float64) (*bidcos.Packet, error) {
	return hm.ClimateCommandPacket(dst, ThermalControlTransmit, hm.CmdSetTemperature, temp)
}

// AutoModePacket returns the packet switching to auto (week program) mode.
func AutoModePacket(dst [3]byte) *bidcos.Packet {
	p, _ := hm.ClimateCommandPacket(dst, ThermalControlTransmit, hm.CmdAuto, 0)
	return p
}

// ManualModePacket returns the packet switching to manual mode at temp.
func ManualModePacket(dst [3]byte, temp float64) (*bidcos.Packet, error) {
	return hm.ClimateCommandPacket(dst, ThermalControlTransmit, hm.CmdManual, temp)
}

// BoostPacket returns the packet starting boost mode.
func BoostPacket(dst [3]byte) *bidcos.Packet {
	p, _ := hm.ClimateCommandPacket(dst, ThermalControlTransmit, hm.CmdBoost, 0)
	return p
}

func (tc *ThermalControl) command(ctx context.Context, cmd hm.ClimateCommand, temp float64) (*AckStatusEvent, error) {
	resp, err := tc.SendClimateCommand(ctx, ThermalControlTransmit, cmd, temp)
	if err != nil {
		return nil, err
	}
	if !hm.IsAckStatus(resp) {
		return nil, nil
	}
	st, _ := tc.DecodeAckStatus(resp.Payload)
	return st, nil
}

// SetTemperature sets the target temperature (4.5 = off … 30.5 = on,
// rounded to 0.5 °C). The TC forwards it to its RT-DN team. It returns
// the device's ACK_STATUS, or nil if the device answered with a plain
// ACK.
func (tc *ThermalControl) SetTemperature(ctx context.Context, temp float64) (*AckStatusEvent, error) {
	return tc.command(ctx, hm.CmdSetTemperature, temp)
}

// SetAutoMode switches to auto mode (week program).
func (tc *ThermalControl) SetAutoMode(ctx context.Context) (*AckStatusEvent, error) {
	return tc.command(ctx, hm.CmdAuto, 0)
}

// SetManualMode switches to manual mode with target temperature temp.
func (tc *ThermalControl) SetManualMode(ctx context.Context, temp float64) (*AckStatusEvent, error) {
	return tc.command(ctx, hm.CmdManual, temp)
}

// SetBoost starts boost mode.
func (tc *ThermalControl) SetBoost(ctx context.Context) (*AckStatusEvent, error) {
	return tc.command(ctx, hm.CmdBoost, 0)
}

func (tc *ThermalControl) MostRecentEvents() []hm.Event {
	var result []hm.Event
	tc.latestMu.RLock()
	defer tc.latestMu.RUnlock()

	if tc.latestWeatherEvent != nil {
		result = append(result, tc.latestWeatherEvent)
	}

	if tc.latestThermalControlEvent != nil {
		result = append(result, tc.latestThermalControlEvent)
	}

	if tc.latestInfoEvent != nil {
		result = append(result, tc.latestInfoEvent)
	}

	return result
}

func (tc *ThermalControl) encodeProgramDay(entries [13]ProgramEntry) []byte {
	result := make([]byte, 26)

	for i := 0; i < 13; i++ {
		endtime := entries[i].Endtime
		if endtime == 0 {
			endtime = 1440
		}
		temperature := entries[i].Temperature
		if temperature == 0.0 {
			temperature = 17.0
		}
		result[(2 * i)] = byte(((uint16(endtime/5) & 0x0100) >> 8) | ((uint16(temperature*2.0) & hm.Mask6Bit) << 1))
		result[(2*i)+1] = byte(((uint16(endtime/5) & 0x00FF) >> 0))
	}

	return result
}

// programOffsets maps weekdays to device memory location offsets
var programOffsets = map[time.Weekday]int{
	time.Saturday:  20,
	time.Sunday:    46,
	time.Monday:    72,
	time.Tuesday:   98,
	time.Wednesday: 124,
	time.Thursday:  150,
	time.Friday:    176,
}

func (tc *ThermalControl) SetPrograms(mem []byte, programs []Program) {
	for _, day := range []time.Weekday{
		time.Saturday,
		time.Sunday,
		time.Monday,
		time.Tuesday,
		time.Wednesday,
		time.Thursday,
		time.Friday,
	} {
		for _, pg := range programs {
			if 1<<uint(day)&pg.DayMask != 0 {
				copy(mem[programOffsets[day]:], tc.encodeProgramDay(pg.Endtimes))
			}
		}
	}
}
