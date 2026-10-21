package thermal

import (
	"bytes"
	"fmt"
	"html/template"

	"github.com/fjaeckel/hmgo/internal/hm"
)

// CmdSwitchEvent is BidCoS command 0x41 (SWITCH_EVENT on channel 7).
const CmdSwitchEvent = 0x41

// AckStatusEvent is the TC's ACK_STATUS frame (cmd 0x02, subtype 0x01),
// sent in response to a command. It carries no actual temperature or
// battery voltage. Payload layout (rftypes rf_tc_it_wm-w-eu.xml
// ACK_STATUS, FHEM CUL_HM_Parse “ack-status”):
//
//	[0] 0x01
//	[1] channel (2)
//	[2] SET_TEMPERATURE = [2]&0x3f (×0.5 °C)
//	[3] LOWBAT_REPORTING bit 7, COMMUNICATION_REPORTING bit 2,
//	    WINDOW_OPEN_REPORTING bit 1
//	[4] RSSI of our frame as received by the device, -dBm (0x00/0x80 = unknown)
//	[5] CONTROL_MODE = [5]>>6, BOOST_STATE = [5]&0x3f (minutes)
//	[6..] party mode data (ignored)
type AckStatusEvent struct {
	Channel                byte
	SetTemperature         float64 // in degC
	LowbatReporting        bool
	CommunicationReporting bool
	WindowOpenReporting    bool
	// HasControl is false if the frame was too short to carry
	// CONTROL_MODE/BOOST_STATE.
	HasControl bool
	Control    ControlMode
	BoostState uint64
	// DeviceRSSI is the RSSI (negative dBm) at which the device received
	// our command, or 0 if not reported.
	DeviceRSSI int
}

var asTmpl = template.Must(template.New("ackstatus").Parse(`
<strong>Ack status:</strong><br>
Target temperature: {{ .SetTemperature }} ℃<br>
Low battery: {{ .LowbatReporting }}<br>
Window open: {{ .WindowOpenReporting }}<br>
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
func (tc *ThermalControl) DecodeAckStatus(payload []byte) (*AckStatusEvent, error) {
	return DecodeAckStatus(payload)
}

// DecodeAckStatus decodes a TC ACK_STATUS payload (starting with 0x01).
func DecodeAckStatus(payload []byte) (*AckStatusEvent, error) {
	if len(payload) < 4 || payload[0] != hm.AckStatus {
		return nil, fmt.Errorf("not an ACK_STATUS payload: % x", payload)
	}
	as := &AckStatusEvent{
		Channel:                payload[1] & hm.Mask6Bit,
		SetTemperature:         float64(payload[2]&hm.Mask6Bit) / 2,
		LowbatReporting:        payload[3]&0x80 != 0,
		CommunicationReporting: payload[3]&0x04 != 0,
		WindowOpenReporting:    payload[3]&0x02 != 0,
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

// SwitchEvent is the TC's SWITCH_EVENT (cmd 0x41, channel 7), sent to
// peers when the configured switch condition changes.
type SwitchEvent struct {
	Channel       byte
	LowBattery    bool // bit 7 of the channel byte
	Counter       byte
	DecisionValue byte
}

func (se *SwitchEvent) HTML() template.HTML {
	return template.HTML(template.HTMLEscapeString(fmt.Sprintf("Switch event: channel %d, counter %d, value %d", se.Channel, se.Counter, se.DecisionValue)))
}

// DecodeSwitchEvent decodes a cmd 0x41 payload: [0] channel (bit 7
// lowbat), [1] counter, [2] decision value.
func DecodeSwitchEvent(payload []byte) (*SwitchEvent, error) {
	if len(payload) < 3 {
		return nil, fmt.Errorf("switch event: payload too short: got %d, want >= 3", len(payload))
	}
	return &SwitchEvent{
		Channel:       payload[0] & hm.Mask6Bit,
		LowBattery:    payload[0]&0x80 != 0,
		Counter:       payload[1],
		DecisionValue: payload[2],
	}, nil
}
