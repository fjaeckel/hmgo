// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see LICENSE.

package heating

import (
	"bytes"
	"fmt"
	"html/template"

	"github.com/fjaeckel/hmgo/internal/hm"
)

// CmdClimateTeam is BidCoS command 0x59, which an RT-DN sends to its
// climate team peers (other RT-DNs) when its set temperature or mode
// changes. The bridge only sees it when overhearing traffic between
// devices (the destination is the peer, not the central).
const CmdClimateTeam = 0x59

// AckStatusEvent is the RT-DN's ACK_STATUS frame (cmd 0x02, subtype
// 0x01), sent in response to a command. Unlike INFO_LEVEL it carries no
// actual temperature, valve position or battery voltage. Payload layout
// (rftypes rf_cc_rt_dn.xml ACK_STATUS, FHEM CUL_HM_Parse “ackInfo”):
//
//	[0] 0x01
//	[1] channel (4)
//	[2] SET_TEMPERATURE = [2]&0x3f (×0.5 °C)
//	[3] LOWBAT = [3]>>7, FAULT_REPORTING = ([3]>>1)&7
//	[4] RSSI of our frame as received by the device, -dBm (FHEM
//	    CUL_HM_parseCommon; 0x00/0x80 = unknown)
//	[5] CONTROL_MODE = [5]>>6, BOOST_STATE = [5]&0x3f (minutes)
//	[6..] party mode data (ignored)
type AckStatusEvent struct {
	Channel        byte
	SetTemperature float64 // in degC; 4.5 = off, 30.5 = on
	LowBattery     bool
	Fault          FaultReporting
	// HasControl is false if the frame was too short to carry
	// CONTROL_MODE/BOOST_STATE.
	HasControl bool
	Control    ControlMode
	BoostState uint64 // remaining boost minutes (only meaningful in BoostMode)
	// DeviceRSSI is the RSSI (negative dBm) at which the device received
	// our command, or 0 if not reported.
	DeviceRSSI int
}

var asTmpl = template.Must(template.New("ackstatus").Parse(`
<strong>Ack status:</strong><br>
Target temperature: {{ .SetTemperature }} ℃<br>
Low battery: {{ .LowBattery }}<br>
Fault: {{ .Fault }}<br>
{{ if .HasControl }}Control: {{ .Control }}<br>{{ end }}
`))

func (as *AckStatusEvent) HTML() template.HTML {
	var buf bytes.Buffer
	if err := asTmpl.Execute(&buf, as); err != nil {
		return template.HTML(template.HTMLEscapeString(err.Error()))
	}
	return template.HTML(buf.String())
}

// DecodeAckStatus decodes an ACK_STATUS payload (starting with 0x01).
func (t *Thermostat) DecodeAckStatus(payload []byte) (*AckStatusEvent, error) {
	return DecodeAckStatus(payload)
}

// DecodeAckStatus decodes an RT-DN ACK_STATUS payload (starting with 0x01).
func DecodeAckStatus(payload []byte) (*AckStatusEvent, error) {
	if len(payload) < 4 || payload[0] != hm.AckStatus {
		return nil, fmt.Errorf("not an ACK_STATUS payload: % x", payload)
	}
	as := &AckStatusEvent{
		Channel:        payload[1] & hm.Mask6Bit,
		SetTemperature: float64(payload[2]&hm.Mask6Bit) / 2,
		LowBattery:     payload[3]&0x80 != 0,
		Fault:          FaultReporting((payload[3] >> 1) & hm.Mask3Bit),
	}
	if len(payload) >= 5 && payload[4] != 0x00 && payload[4] != 0x80 {
		as.DeviceRSSI = -int(payload[4])
	}
	if len(payload) >= 6 {
		as.HasControl = true
		as.Control = ControlMode((payload[5] >> 6) & hm.Mask2Bit)
		as.BoostState = uint64(payload[5] & hm.Mask6Bit)
	}
	return as, nil
}

// TeamEvent is the climate team frame (cmd 0x59) an RT-DN sends to its
// team peers: payload[0] = SET_TEMPERATURE<<2 | CONTROL_MODE (FHEM
// CUL_HM_Parse “inform team about new value”).
type TeamEvent struct {
	SetTemperature float64 // in degC
	Control        ControlMode
}

func (te *TeamEvent) HTML() template.HTML {
	return template.HTML(template.HTMLEscapeString(fmt.Sprintf("Team: target %v ℃, mode %v", te.SetTemperature, te.Control)))
}

// DecodeTeamEvent decodes a cmd 0x59 payload.
func DecodeTeamEvent(payload []byte) (*TeamEvent, error) {
	if len(payload) < 1 {
		return nil, fmt.Errorf("team event: empty payload")
	}
	return &TeamEvent{
		SetTemperature: float64(payload[0]>>2) / 2,
		Control:        ControlMode(payload[0] & hm.Mask2Bit),
	}, nil
}
