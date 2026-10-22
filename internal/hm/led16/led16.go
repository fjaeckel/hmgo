// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package led16 implements the HM-OU-LED16 status display: 16 LEDs, each
// off, red, green or orange; mains powered, always listening (rftypes
// rf_ou_led16*.xml: 16 channels of type KEY, parameter LED_STATUS).
//
// Frames (first payload byte = byte 0):
//
//	SET_ONE_CHANNEL   0x11 0x80: [1] chn, [2] color          (one LED)
//	SET_ALL_CHANNELS  0x11 0x81: [1] chn, [2:6] colors        (not used)
//	GET_ONE_CHANNEL   0x01:      [0] chn, [1] 0x0E            (status request)
//	ACK_STATUS        0x02 0x01: [1] chn, [2] color
//	INFO_LEVEL        0x10 0x06: [1] chn, [2] color
//
// Colors: 0 off, 1 red, 2 green, 3 orange. OpenCCU sends SET_ONE_CHANNEL
// with flags 0xA0 (BiDi, no burst), e.g. “A0 11 … 80 01 02” = LED 1
// green (hmsniff capture of OpenCCU), which hmbridge does too.
package led16

import (
	"context"
	"fmt"
	"html/template"
	"strings"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/power"
)

// Model is the eQ-3 model name.
const Model = "HM-OU-LED16"

// LEDs is the number of LEDs (= channels 1..16).
const LEDs = 16

const (
	subtypeSetOne = 0x80
	statusRequest = 0x0E
)

// Color is an LED color.
type Color byte

const (
	Off    Color = 0
	Red    Color = 1
	Green  Color = 2
	Orange Color = 3
)

// Colors lists the color names in wire order.
var Colors = []string{"off", "red", "green", "orange"}

func (c Color) String() string {
	if int(c) < len(Colors) {
		return Colors[c]
	}
	return fmt.Sprintf("color(%d)", byte(c))
}

// ParseColor parses a color name (case-insensitive).
func ParseColor(s string) (Color, error) {
	for i, n := range Colors {
		if strings.EqualFold(strings.TrimSpace(s), n) {
			return Color(i), nil
		}
	}
	return 0, fmt.Errorf("unknown LED color %q (want one of %s)", s, strings.Join(Colors, ", "))
}

// Event is the color of one LED, from ACK_STATUS or INFO_LEVEL.
type Event struct {
	Channel byte // LED, 1..16
	Color   Color
}

func (e *Event) HTML() template.HTML {
	return template.HTML(template.HTMLEscapeString(fmt.Sprintf("LED %d: %s", e.Channel, e.Color)))
}

// DecodeStatus decodes an ACK_STATUS (02 01 …) or INFO_LEVEL (10 06 …)
// payload.
func DecodeStatus(payload []byte) (*Event, error) {
	if len(payload) < 3 {
		return nil, fmt.Errorf("led16 status: payload too short: got %d, want >= 3", len(payload))
	}
	if payload[0] != hm.AckStatus && payload[0] != bidcos.InfoActuatorStatus {
		return nil, fmt.Errorf("led16 status: unexpected subtype 0x%02x", payload[0])
	}
	ch := payload[1] & hm.Mask6Bit
	if ch < 1 || ch > LEDs {
		return nil, fmt.Errorf("led16 status: channel %d out of range", ch)
	}
	if payload[2] > byte(Orange) {
		return nil, fmt.Errorf("led16 status: color %d out of range", payload[2])
	}
	return &Event{Channel: ch, Color: Color(payload[2])}, nil
}

// SetPacket returns the SET_ONE_CHANNEL packet for LED chn.
func SetPacket(dst [3]byte, chn byte, c Color) *bidcos.Packet {
	return &bidcos.Packet{
		Flags:   bidcos.BiDi | bidcos.RepeatEnable,
		Cmd:     hm.CmdLevelSet,
		Dest:    dst,
		Payload: []byte{subtypeSetOne, chn, byte(c)},
	}
}

// StatusRequestPacket returns the GET_ONE_CHANNEL packet for LED chn.
func StatusRequestPacket(dst [3]byte, chn byte) *bidcos.Packet {
	return &bidcos.Packet{
		Flags:   bidcos.BiDi | bidcos.RepeatEnable,
		Cmd:     bidcos.Config,
		Dest:    dst,
		Payload: []byte{chn, statusRequest},
	}
}

// Display is an HM-OU-LED16.
type Display struct {
	hm.StandardDevice
}

// NewDisplay returns a Display. sd.BCS may be nil.
func NewDisplay(sd hm.StandardDevice) *Display {
	sd.NumChannels = LEDs
	sd.ModelName = Model
	return &Display{StandardDevice: sd}
}

func (d *Display) HomeMaticType() string        { return "led16" }
func (d *Display) MostRecentEvents() []hm.Event { return nil }

// Decode decodes ACK_STATUS / INFO_LEVEL → *Event and the common frames;
// nil, nil for anything else (e.g. KEY_EVENT 0x40, which the XML lists
// but hmbridge does not use).
func (d *Display) Decode(pkt *bidcos.Packet) (any, error) {
	if err := d.CheckIncoming(pkt); err != nil {
		return nil, err
	}
	if hm.IsAckStatus(pkt) || pkt.Cmd == bidcos.Info && len(pkt.Payload) > 0 && pkt.Payload[0] == bidcos.InfoActuatorStatus {
		return DecodeStatus(pkt.Payload)
	}
	return hm.DecodeCommon(pkt)
}

func checkLED(chn byte) error {
	if chn < 1 || chn > LEDs {
		return fmt.Errorf("LED %d out of range 1..%d", chn, LEDs)
	}
	return nil
}

// SetLED sets LED chn to c. It returns the ACK_STATUS, or nil for a
// plain ACK.
func (d *Display) SetLED(ctx context.Context, chn byte, c Color) (*Event, error) {
	if err := checkLED(chn); err != nil {
		return nil, err
	}
	if c > Orange {
		return nil, fmt.Errorf("invalid color %d", c)
	}
	resp, err := d.Send(ctx, SetPacket(d.Addr, chn, c))
	if err != nil {
		return nil, err
	}
	if !hm.IsAckStatus(resp) {
		return nil, nil
	}
	ev, err := DecodeStatus(resp.Payload)
	if err != nil {
		return nil, nil
	}
	return ev, nil
}

// RequestStatus asks LED chn for its color (answered with INFO_LEVEL).
func (d *Display) RequestStatus(ctx context.Context, chn byte) (*Event, error) {
	if err := checkLED(chn); err != nil {
		return nil, err
	}
	if d.BCS == nil {
		return nil, hm.ErrNoSender
	}
	req := StatusRequestPacket(d.Addr, chn)
	resp, err := d.BCS.Send(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%v: %w", &d.StandardDevice, err)
	}
	if err := d.CheckReplyAuth(req, resp); err != nil {
		return nil, fmt.Errorf("%v: %w", &d.StandardDevice, err)
	}
	return power.DecodeStatusResponse(d.Addr, resp, DecodeStatus)
}
