// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package hm

import (
	"context"
	"fmt"
	"math"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/prometheus/client_golang/prometheus"
)

// Climate control commands for HM-CC-RT-DN (control channel 4) and
// HM-TC-IT-WM-W-EU (control channel 2).
//
// Frame layout (eQ-3 rftypes rf_cc_rt_dn.xml / rf_tc_it_wm-w-eu.xml,
// frames AUTO_MODE_SET, MANU_MODE_SET, BOOST_MODE_SET,
// SET_TEMPERATURE_SET; all BidCoS cmd 0x11):
//
//	86 <chn> <temp*2>   SET_TEMPERATURE_SET (6 bit value)
//	80 <chn>            AUTO_MODE_SET
//	81 <chn> <temp*2>   MANU_MODE_SET
//	83 <chn>            BOOST_MODE_SET
//
// The XML defines no channel byte for AUTO_MODE_SET/BOOST_MODE_SET; we
// include it anyway, as FHEM does (CUL_HM_Set "controlMode": '8'.mode.chn),
// which is field-proven with both models.
//
// Flags: BiDi|RepeatEnable|Burst (0xB0). Both models are battery
// powered and only listen periodically (eQ-3 rx_modes: RT-DN
// "CONFIG,WAKEUP,BURST" with BURST_RX enforced, TC "CONFIG,BURST"). A
// burst frame (the HM-MOD-RPI-PCB sends a long preamble when the burst
// byte is set, see bidcos.Packet.Encode) wakes them up immediately.
// Homegear and the CCU set the burst bit (0x10) on every frame to
// devices with the BURST rx mode (BidCoSPeer: “if wakeOnRadio
// controlByte |= 0x10”). FHEM instead first sends a burst “have data”
// frame (B1 12) and then the command with A0 (rxType 0x02 = burst for
// the TC; for the RT-DN, rxType “c:w:f” = burstConditional, only with
// attr burstAccess=1, otherwise it waits for the next wakeup). Sending
// the command itself as burst is equivalent and needs one frame less.
// Bursts cost roughly 1 s of duty cycle each, so commands should not be
// sent in rapid succession.

// ClimateCommand names a climate control command. The values are used
// as the "command" label of the command metrics.
type ClimateCommand string

const (
	CmdSetTemperature ClimateCommand = "set_temperature"
	CmdAuto           ClimateCommand = "auto"
	CmdManual         ClimateCommand = "manual"
	CmdBoost          ClimateCommand = "boost"
)

// BidCoS cmd 0x11 subtypes for climate control.
const (
	SubtypeAutoMode       = 0x80
	SubtypeManualMode     = 0x81
	SubtypePartyMode      = 0x82
	SubtypeBoostMode      = 0x83
	SubtypeComfortMode    = 0x84
	SubtypeLoweringMode   = 0x85
	SubtypeSetTemperature = 0x86
)

// CmdLevelSet is BidCoS command 0x11 (“LEVEL_SET” / actor command).
const CmdLevelSet = 0x11

// CommandFlags are the flags used for climate commands.
const CommandFlags = bidcos.BiDi | bidcos.RepeatEnable | bidcos.Burst

// Temperature limits. 4.5 °C means “off” (valve closed), 30.5 °C means
// “on” (valve fully open).
const (
	TempOff = 4.5
	TempOn  = 30.5
)

// EncodeTemperature validates t (4.5..30.5 °C), rounds it to the
// nearest 0.5 °C and returns the wire value t*2 (9..61).
func EncodeTemperature(t float64) (byte, error) {
	if math.IsNaN(t) || math.IsInf(t, 0) {
		return 0, fmt.Errorf("invalid temperature %v", t)
	}
	r := math.Round(t*2) / 2
	if r < TempOff || r > TempOn {
		return 0, fmt.Errorf("temperature %v out of range %v..%v", t, TempOff, TempOn)
	}
	return byte(r * 2), nil
}

// ClimateCommandPacket builds the packet for cmd on control channel chn
// of the device dst. t is only used by CmdSetTemperature and
// CmdManual.
func ClimateCommandPacket(dst [3]byte, chn byte, cmd ClimateCommand, t float64) (*bidcos.Packet, error) {
	var payload []byte
	switch cmd {
	case CmdSetTemperature, CmdManual:
		v, err := EncodeTemperature(t)
		if err != nil {
			return nil, err
		}
		st := byte(SubtypeSetTemperature)
		if cmd == CmdManual {
			st = SubtypeManualMode
		}
		payload = []byte{st, chn, v}
	case CmdAuto:
		payload = []byte{SubtypeAutoMode, chn}
	case CmdBoost:
		payload = []byte{SubtypeBoostMode, chn}
	default:
		return nil, fmt.Errorf("unknown climate command %q", cmd)
	}
	return &bidcos.Packet{
		Flags:   CommandFlags,
		Cmd:     CmdLevelSet,
		Dest:    dst,
		Payload: payload,
	}, nil
}

var (
	commandsSent = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "hm",
			Name:      "commands_sent_total",
			Help:      "commands sent to devices",
		},
		[]string{"address", "name", "command"})

	commandErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "hm",
			Name:      "command_errors_total",
			Help:      "commands that failed (transport error, no response, NACK, invalid argument)",
		},
		[]string{"address", "name", "command"})
)

func init() {
	prometheus.MustRegister(commandsSent)
	prometheus.MustRegister(commandErrors)
}

// SendClimateCommand encodes and sends cmd to control channel chn,
// updating the hm_commands_sent_total / hm_command_errors_total
// metrics. It returns the device's ACK frame (cmd 0x02, payload[0] 0x00
// or 0x01), which the caller may decode as ACK_STATUS.
func (sd *StandardDevice) SendClimateCommand(ctx context.Context, chn byte, cmd ClimateCommand, t float64) (*bidcos.Packet, error) {
	labels := prometheus.Labels{"address": sd.AddrHex(), "name": sd.Name(), "command": string(cmd)}
	commandsSent.With(labels).Inc()
	pkt, err := ClimateCommandPacket(sd.Addr, chn, cmd, t)
	if err != nil {
		commandErrors.With(labels).Inc()
		return nil, err
	}
	resp, err := sd.Send(ctx, pkt)
	if err != nil {
		commandErrors.With(labels).Inc()
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	return resp, nil
}
