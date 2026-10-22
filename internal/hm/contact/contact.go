// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see LICENSE.

// Package contact implements the classic HomeMatic window/door contacts
// (eQ-3 “threeStateSensor” family):
//
//	HM-Sec-SCo   optical shutter contact      (rftypes rf_sec_sco.xml)
//	HM-Sec-SC-2  magnetic shutter contact     (rftypes rf_sc.xml)
//	HM-Sec-SC    magnetic shutter contact     (rftypes rf_sc.xml)
//	HM-Sec-RHS   rotary handle sensor         (rftypes rf_rhs.xml)
//
// All have a single functional channel 1 and are battery powered
// (rx_modes CONFIG,WAKEUP; the SCo additionally LAZY_CONFIG); they only
// send, so there are no commands.
//
// Frames sent to the central (rftypes XML, FHEM CUL_HM_Parse
// “threeStateSensor”):
//
//	0x41       EVENT        [0] chn | 0x80 lowbat, [1] counter, [2] state
//	0x10 0x06  INFO_LEVEL   [0] 06, [1] chn, [2] state, [3] error<<1 | 0x80 lowbat
//	0x02 0x01  ACK_STATUS   same layout as INFO_LEVEL with [0] 01
//
// State: 0x00 closed, 0x64 tilted (RHS only), 0xC8 open.
//
// Sabotage: all four models report the ERROR value SABOTAGE (option
// index 7 in the 3-bit field at payload[3] bits 1..3) in INFO_LEVEL /
// ACK_STATUS, and all have the SABOTAGE_MSG config parameter (rftypes
// XML; FHEM HMConfig lists sabotageMsg for HM-SEC-SC, -SCO and -RHS, and
// CUL_HM_Parse reports sabotageError for /HM-SEC-SC.*|HM-SEC-RHS/). The
// SCo's sabotage contact detects removal from the mounting plate.
// Sabotage is only reported in INFO_LEVEL/ACK_STATUS, never in EVENT.
//
// Events sent to the central with the BiDi flag are acknowledged by the
// HM-MOD-RPI-PCB itself for configured peers (FHEM 00_HMUARTLGW: “Acks
// are generally send by HMUARTLGW autonomously”), so the bridge needs
// not answer them.
package contact

import (
	"bytes"
	"fmt"
	"html/template"
	"sync"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/prometheus/client_golang/prometheus"
)

// Supported models.
const (
	ModelSCo = "HM-Sec-SCo"
	ModelSC2 = "HM-Sec-SC-2"
	ModelSC  = "HM-Sec-SC"
	ModelRHS = "HM-Sec-RHS"
)

// Channel is the contact's only functional channel.
const Channel = 0x01

// Capabilities describes what a model reports, so that the HA layer can
// decide which entities to create.
type Capabilities struct {
	// TriState: the model reports closed/tilted/open (RHS); otherwise
	// only closed/open.
	TriState bool
	// Sabotage: the model reports a sabotage (tamper) error.
	Sabotage bool
	// BatteryVoltage: the model reports a battery voltage. False for all
	// contacts: they only report a low battery flag.
	BatteryVoltage bool
	// Wakeup: the device listens for wakeup frames (relevant for the
	// radio module's peer table).
	Wakeup bool
}

var capabilities = map[string]Capabilities{
	ModelSCo: {Sabotage: true, Wakeup: true},
	ModelSC2: {Sabotage: true, Wakeup: true},
	ModelSC:  {Sabotage: true, Wakeup: true},
	ModelRHS: {TriState: true, Sabotage: true, Wakeup: true},
}

// ModelCapabilities returns the capabilities of model and whether the
// model is supported.
func ModelCapabilities(model string) (Capabilities, bool) {
	c, ok := capabilities[model]
	return c, ok
}

// State is the contact state.
type State byte

const (
	Closed State = 0x00
	Tilted State = 0x64
	Open   State = 0xc8
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Tilted:
		return "tilted"
	case Open:
		return "open"
	default:
		return fmt.Sprintf("unknown(0x%02x)", byte(s))
	}
}

// IsOpen reports whether the window is not closed (open or tilted).
func (s State) IsOpen() bool { return s != Closed }

// ErrorSabotage is the ERROR value meaning sabotage.
const ErrorSabotage = 7

// Frame identifies which frame an Event was decoded from.
type Frame string

const (
	FrameEvent     Frame = "event"      // 0x41
	FrameInfoLevel Frame = "info_level" // 0x10 0x06
	FrameAckStatus Frame = "ack_status" // 0x02 0x01
)

// Event is a decoded contact frame.
type Event struct {
	Frame      Frame
	Channel    byte
	State      State
	LowBattery bool
	// HasError is true for INFO_LEVEL/ACK_STATUS frames, which carry the
	// ERROR field; EVENT frames don't (Error/Sabotage are then unknown
	// and must not reset a previously reported sabotage).
	HasError bool
	Error    byte // raw 3 bit ERROR value; 0 = no error, 7 = sabotage
	// Sabotage is true if any error is reported (as FHEM does: eQ-3 only
	// defines 7 = SABOTAGE). Only meaningful if HasError.
	Sabotage bool
	// Counter is the EVENT frame's message counter (EVENT only).
	Counter byte
}

var evTmpl = template.Must(template.New("contact").Parse(`
<strong>Contact ({{ .Frame }}):</strong><br>
State: {{ .State }}<br>
Low battery: {{ .LowBattery }}<br>
{{ if .HasError }}Sabotage: {{ .Sabotage }}<br>{{ end }}
`))

func (ev *Event) HTML() template.HTML {
	var buf bytes.Buffer
	if err := evTmpl.Execute(&buf, ev); err != nil {
		return template.HTML(template.HTMLEscapeString(err.Error()))
	}
	return template.HTML(buf.String())
}

const prometheusNamespace = "hmcontact"

var (
	contactState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "State",
			Help:      "contact state: 0 closed, 1 tilted, 2 open",
		},
		[]string{"address", "name"})

	contactLowbat = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "Lowbat",
			Help:      "low battery as bool",
		},
		[]string{"address", "name"})

	contactSabotage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "Sabotage",
			Help:      "sabotage as bool",
		},
		[]string{"address", "name"})
)

func init() {
	prometheus.MustRegister(contactState)
	prometheus.MustRegister(contactLowbat)
	prometheus.MustRegister(contactSabotage)
}

// Contact is a window/door contact.
type Contact struct {
	hm.StandardDevice
	caps Capabilities

	latest   *Event
	latestMu sync.RWMutex
}

// NewContact returns a Contact of the given model. sd.BCS may be nil.
func NewContact(sd hm.StandardDevice, model string) (*Contact, error) {
	caps, ok := capabilities[model]
	if !ok {
		return nil, fmt.Errorf("contact: unsupported model %q", model)
	}
	sd.NumChannels = 1
	sd.ModelName = model
	return &Contact{StandardDevice: sd, caps: caps}, nil
}

func (c *Contact) HomeMaticType() string { return "contact" }

// Capabilities returns what this contact's model reports.
func (c *Contact) Capabilities() Capabilities { return c.caps }

func (c *Contact) MostRecentEvents() []hm.Event {
	c.latestMu.RLock()
	defer c.latestMu.RUnlock()
	if c.latest == nil {
		return nil
	}
	return []hm.Event{c.latest}
}

// Decode decodes a packet received from the contact: EVENT, INFO_LEVEL
// and ACK_STATUS → *Event; DEVICE_INFO → *hm.DeviceInfo; nil otherwise.
func (c *Contact) Decode(pkt *bidcos.Packet) (any, error) {
	if err := c.CheckIncoming(pkt); err != nil {
		return nil, err
	}
	var ev *Event
	var err error
	switch {
	case pkt.Cmd == CmdEvent:
		ev, err = DecodeEvent(pkt.Payload)
	case pkt.Cmd == bidcos.Info && len(pkt.Payload) > 0 && pkt.Payload[0] == bidcos.InfoActuatorStatus,
		hm.IsAckStatus(pkt):
		ev, err = DecodeStatus(pkt.Payload)
	default:
		return hm.DecodeCommon(pkt)
	}
	if err != nil {
		return nil, err
	}
	if ev.Channel != Channel {
		// The channel comes from the sender's payload: a frame for
		// another channel is malformed or forged (it may even bypass a
		// per-channel AES check).
		return nil, fmt.Errorf("contact: frame for channel %d, but the contact has only channel %d", ev.Channel, Channel)
	}
	if !c.caps.Sabotage {
		ev.HasError, ev.Sabotage = false, false
	}
	c.record(ev)
	return ev, nil
}

func (c *Contact) record(ev *Event) {
	l := prometheus.Labels{"name": c.Name(), "address": c.AddrHex()}
	var st float64
	switch ev.State {
	case Tilted:
		st = 1
	case Open:
		st = 2
	}
	contactState.With(l).Set(st)
	contactLowbat.With(l).Set(b2f(ev.LowBattery))
	if ev.HasError {
		contactSabotage.With(l).Set(b2f(ev.Sabotage))
	}
	c.latestMu.Lock()
	defer c.latestMu.Unlock()
	c.latest = ev
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// CmdEvent is BidCoS command 0x41 (sensor EVENT).
const CmdEvent = 0x41

// DecodeEvent decodes an EVENT (cmd 0x41) payload.
func DecodeEvent(payload []byte) (*Event, error) {
	if len(payload) < 3 {
		return nil, fmt.Errorf("contact event: payload too short: got %d, want >= 3", len(payload))
	}
	return &Event{
		Frame:      FrameEvent,
		Channel:    payload[0] & hm.Mask6Bit,
		LowBattery: payload[0]&0x80 != 0,
		Counter:    payload[1],
		State:      State(payload[2]),
	}, nil
}

// DecodeStatus decodes an INFO_LEVEL (0x10 06 …) or ACK_STATUS
// (0x02 01 …) payload.
func DecodeStatus(payload []byte) (*Event, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("contact status: payload too short: got %d, want >= 4", len(payload))
	}
	var fr Frame
	switch payload[0] {
	case bidcos.InfoActuatorStatus:
		fr = FrameInfoLevel
	case hm.AckStatus:
		fr = FrameAckStatus
	default:
		return nil, fmt.Errorf("contact status: unexpected subtype 0x%02x", payload[0])
	}
	errv := (payload[3] >> 1) & hm.Mask3Bit
	return &Event{
		Frame:      fr,
		Channel:    payload[1] & hm.Mask6Bit,
		State:      State(payload[2]),
		LowBattery: payload[3]&0x80 != 0,
		HasError:   true,
		Error:      errv,
		Sabotage:   errv != 0,
	}, nil
}
