// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package power

// Mains powered switch and dimmer actuators of the eQ-3 “switch” and
// “dimmer” families:
//
//	HM-LC-Sw1-Pl-DN-R1  switch plug, 1 channel        (rftypes rf_s_644.xml)
//	HM-LC-Dim1L-Pl-3    dimmer plug, channel 1 real,  (rftypes rf_dim_1l_644.xml)
//	                    channels 2 and 3 virtual
//
// Both listen all the time, so commands are sent without burst.
//
// Frames (eQ-3 rftypes XML; byte 0 below is the first payload byte):
//
//	LEVEL_GET    0x01:      [0] chn, [1] 0x0E                         (status request)
//	LEVEL_SET    0x11 0x02: [1] chn, [2] level, [3:5] ramp time,       (switch: ramp 0)
//	                        [5:7] on time (omitted if 0)
//	ACK_STATUS   0x02 0x01: [1] chn, [2] level, [3] flags, [4] RSSI, [5] LEVEL_REAL (dimmer)
//	INFO_LEVEL   0x10 0x06: [1] chn, [2] level, [3] flags, [5] LEVEL_REAL (dimmer)
//
// level: 0x00 … 0xC8 = 0 … 100 % in 0.5 % steps (the switch knows only
// 0x00 off and 0xC8 on). flags: bits 4-6 STATE_FLAGS (non-zero while the
// actuator is working, e.g. ramping or delayed), of which bits 4-5 are the
// DIRECTION_FLAGS (1 up, 2 down) on the dimmer; bits 1-3 ERROR on the
// dimmer (3 = LOAD_FAILURE, the only value eQ-3 names for this model).
//
// Times (ramp, on time) use eQ-3's 16-bit “tiny float”: mantissa in bits
// 5-15, exponent in bits 0-4, value = mantissa · 2^exponent · 0.1 s
// (rftypes cast integerTinyFloat, scale factor 10).
//
// The RSSI byte in ACK_STATUS and LEVEL_REAL are not used.

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"math"
	"sync"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
)

// Actuator models.
const (
	ModelSwitchPlug = "HM-LC-Sw1-Pl-DN-R1"
	ModelDimmerPlug = "HM-LC-Dim1L-Pl-3"
)

// Actuator constants.
const (
	// ActuatorChannel is the channel exposed to Home Assistant (the
	// switch's only channel, the dimmer's physical channel).
	ActuatorChannel = 0x01

	LevelOff = 0x00
	LevelOn  = 0xC8 // 100 %
	// LevelOld (OLD_LEVEL in the dimmer's XML) switches the dimmer on to
	// its previous level. Not used by hmbridge (it remembers the level
	// itself), listed for completeness.
	LevelOld = 0xC9

	subtypeLevelSet = 0x02
	statusRequest   = 0x0E
)

// ActuatorFlags are the flags of actuator commands: BiDi, no burst
// (mains powered devices are always listening).
const ActuatorFlags = bidcos.BiDi | bidcos.RepeatEnable

// LevelFrame identifies which frame a LevelEvent was decoded from.
type LevelFrame string

const (
	FrameAckStatus LevelFrame = "ack_status" // 0x02 0x01
	FrameInfoLevel LevelFrame = "info_level" // 0x10 0x06
)

// Direction is the dimmer's ramp direction.
type Direction byte

const (
	DirectionNone Direction = 0
	DirectionUp   Direction = 1
	DirectionDown Direction = 2
)

func (d Direction) String() string {
	switch d {
	case DirectionNone:
		return "none"
	case DirectionUp:
		return "up"
	case DirectionDown:
		return "down"
	}
	return fmt.Sprintf("direction(%d)", byte(d))
}

// ErrorLoadFailure is the dimmer's ERROR value LOAD_FAILURE.
const ErrorLoadFailure = 3

// LevelEvent is a decoded ACK_STATUS or INFO_LEVEL frame of an actuator.
type LevelEvent struct {
	Frame   LevelFrame
	Channel byte
	Raw     byte    // 0x00 … 0xC8
	Percent float64 // Raw / 2
	// Working is set while the actuator is still changing (STATE_FLAGS
	// non-zero): the level is an intermediate value and a final
	// INFO_LEVEL follows.
	Working   bool
	Direction Direction
	// Error is the dimmer's 3-bit ERROR value (0 = none). Always 0 for
	// the switch, whose frames have no error field.
	Error byte
	// HasFlags is false for frames without the flags byte.
	HasFlags bool
}

// On reports whether the actuator is on (level > 0).
func (e *LevelEvent) On() bool { return e.Raw > 0 }

// ErrorString renders Error for the fault sensor: "none",
// "load_failure" or "error_<n>".
func (e *LevelEvent) ErrorString() string {
	switch e.Error {
	case 0:
		return "none"
	case ErrorLoadFailure:
		return "load_failure"
	}
	return fmt.Sprintf("error_%d", e.Error)
}

var levelTmpl = template.Must(template.New("level").Parse(`
<strong>Level ({{ .Frame }}):</strong> channel {{ .Channel }}, {{ .Percent }} %{{ if .Working }} (working){{ end }}<br>
`))

func (e *LevelEvent) HTML() template.HTML {
	var buf bytes.Buffer
	if err := levelTmpl.Execute(&buf, e); err != nil {
		return template.HTML(template.HTMLEscapeString(err.Error()))
	}
	return template.HTML(buf.String())
}

// DecodeLevelStatus decodes an ACK_STATUS (02 01 …) or INFO_LEVEL
// (10 06 …) payload. dimmer selects the dimmer's flag layout (error and
// direction bits).
func DecodeLevelStatus(payload []byte, dimmer bool) (*LevelEvent, error) {
	if len(payload) < 3 {
		return nil, fmt.Errorf("level status: payload too short: got %d, want >= 3", len(payload))
	}
	var fr LevelFrame
	switch payload[0] {
	case hm.AckStatus:
		fr = FrameAckStatus
	case bidcos.InfoActuatorStatus:
		fr = FrameInfoLevel
	default:
		return nil, fmt.Errorf("level status: unexpected subtype 0x%02x", payload[0])
	}
	raw := payload[2]
	if raw > LevelOn {
		// Not a valid level; don't let it reach Home Assistant.
		return nil, fmt.Errorf("level status: level 0x%02x out of range", raw)
	}
	ev := &LevelEvent{
		Frame:   fr,
		Channel: payload[1] & hm.Mask6Bit,
		Raw:     raw,
		Percent: float64(raw) / 2,
	}
	if len(payload) >= 4 {
		f := payload[3]
		ev.HasFlags = true
		ev.Working = (f>>4)&hm.Mask3Bit != 0
		if dimmer {
			ev.Direction = Direction((f >> 4) & hm.Mask2Bit)
			ev.Error = (f >> 1) & hm.Mask3Bit
		}
	}
	return ev, nil
}

// EncodeTime16 encodes a duration in seconds in eQ-3's 16-bit tiny float
// (see the package comment): the smallest exponent whose mantissa fits
// in 11 bits, so short times keep 0.1 s resolution. Negative and NaN
// values encode as 0; values beyond the range as the maximum.
func EncodeTime16(seconds float64) uint16 {
	if !(seconds > 0) {
		return 0
	}
	v := seconds * 10 // units of 0.1 s
	for exp := 0; exp < 32; exp++ {
		m := math.Round(v / math.Ldexp(1, exp))
		if m <= 0x7FF {
			return uint16(m)<<5 | uint16(exp)
		}
	}
	return 0xFFFF
}

// DecodeTime16 is the inverse of EncodeTime16 (seconds).
func DecodeTime16(v uint16) float64 {
	return math.Ldexp(float64(v>>5), int(v&0x1F)) / 10
}

// LevelSetPacket returns the LEVEL_SET packet for channel chn of dst.
// ramp and onTime are in seconds; onTime 0 is omitted (stay on).
func LevelSetPacket(dst [3]byte, chn, level byte, ramp, onTime float64) *bidcos.Packet {
	r := EncodeTime16(ramp)
	p := []byte{subtypeLevelSet, chn, level, byte(r >> 8), byte(r)}
	if onTime > 0 {
		o := EncodeTime16(onTime)
		p = append(p, byte(o>>8), byte(o))
	}
	return &bidcos.Packet{Flags: ActuatorFlags, Cmd: hm.CmdLevelSet, Dest: dst, Payload: p}
}

// StatusRequestPacket returns the status request (LEVEL_GET) for chn.
// The device answers with INFO_LEVEL.
func StatusRequestPacket(dst [3]byte, chn byte) *bidcos.Packet {
	return &bidcos.Packet{Flags: ActuatorFlags, Cmd: bidcos.Config, Dest: dst, Payload: []byte{chn, statusRequest}}
}

// LevelToRaw converts a percentage (0 … 100) to the raw level, rounded
// to the device's 0.5 % steps.
func LevelToRaw(percent float64) (byte, error) {
	if math.IsNaN(percent) || percent < 0 || percent > 100 {
		return 0, fmt.Errorf("level %v %% out of range 0..100", percent)
	}
	return byte(math.Round(percent * 2)), nil
}

// Actuator is an HM-LC-Sw1-Pl-DN-R1 or HM-LC-Dim1L-Pl-3.
type Actuator struct {
	hm.StandardDevice
	dimmer bool

	latestMu sync.RWMutex
	latest   *LevelEvent
}

// NewActuator returns an actuator of the given model. sd.BCS may be nil.
func NewActuator(sd hm.StandardDevice, model string) (*Actuator, error) {
	switch model {
	case ModelSwitchPlug:
		sd.NumChannels = 1
	case ModelDimmerPlug:
		sd.NumChannels = 3
	default:
		return nil, fmt.Errorf("power: unsupported actuator model %q", model)
	}
	sd.ModelName = model
	return &Actuator{StandardDevice: sd, dimmer: model == ModelDimmerPlug}, nil
}

// IsDimmer reports whether the actuator is a dimmer.
func (a *Actuator) IsDimmer() bool { return a.dimmer }

func (a *Actuator) HomeMaticType() string {
	if a.dimmer {
		return "dimmer"
	}
	return "switch"
}

func (a *Actuator) MostRecentEvents() []hm.Event {
	a.latestMu.RLock()
	defer a.latestMu.RUnlock()
	if a.latest == nil {
		return nil
	}
	return []hm.Event{a.latest}
}

// Decode decodes ACK_STATUS / INFO_LEVEL → *LevelEvent (all channels;
// the caller picks ActuatorChannel), DEVICE_INFO and time requests via
// hm.DecodeCommon, and returns nil, nil for anything else.
func (a *Actuator) Decode(pkt *bidcos.Packet) (any, error) {
	if err := a.CheckIncoming(pkt); err != nil {
		return nil, err
	}
	switch {
	case hm.IsAckStatus(pkt),
		pkt.Cmd == bidcos.Info && len(pkt.Payload) > 0 && pkt.Payload[0] == bidcos.InfoActuatorStatus:
		ev, err := DecodeLevelStatus(pkt.Payload, a.dimmer)
		if err != nil {
			return nil, err
		}
		if ev.Channel == ActuatorChannel {
			a.latestMu.Lock()
			a.latest = ev
			a.latestMu.Unlock()
		}
		return ev, nil
	}
	return hm.DecodeCommon(pkt)
}

// SetLevel sets channel chn to level (raw, 0 … 0xC8) with the given ramp
// time (seconds; ignored by the switch, which is always sent 0) and on
// time (0 = stay). It returns the device's ACK_STATUS, or nil for a
// plain ACK.
func (a *Actuator) SetLevel(ctx context.Context, chn, level byte, ramp, onTime float64) (*LevelEvent, error) {
	if level > LevelOld {
		return nil, fmt.Errorf("level 0x%02x out of range", level)
	}
	if !a.dimmer {
		ramp = 0
		if level != LevelOff {
			level = LevelOn
		}
	}
	resp, err := a.Send(ctx, LevelSetPacket(a.Addr, chn, level, ramp, onTime))
	if err != nil {
		return nil, err
	}
	if !hm.IsAckStatus(resp) {
		return nil, nil
	}
	ev, err := DecodeLevelStatus(resp.Payload, a.dimmer)
	if err != nil {
		return nil, nil // ACKed; the status part is unusable
	}
	return ev, nil
}

// RequestStatus asks channel chn for its level. The device answers the
// request directly with INFO_LEVEL (not with an ACK), which is returned
// decoded.
func (a *Actuator) RequestStatus(ctx context.Context, chn byte) (*LevelEvent, error) {
	return requestStatus(ctx, &a.StandardDevice, StatusRequestPacket(a.Addr, chn), func(p []byte) (*LevelEvent, error) {
		return DecodeLevelStatus(p, a.dimmer)
	})
}

// requestStatus sends a status request and decodes the INFO_LEVEL (or
// ACK_STATUS) answer with decode.
func requestStatus[E any](ctx context.Context, sd *hm.StandardDevice, pkt *bidcos.Packet, decode func([]byte) (E, error)) (E, error) {
	var zero E
	if sd.BCS == nil {
		return zero, hm.ErrNoSender
	}
	pkt.Dest = sd.Addr
	resp, err := sd.BCS.Send(ctx, pkt)
	if err != nil {
		return zero, fmt.Errorf("%v: %w", sd, err)
	}
	return DecodeStatusResponse(sd.Addr, resp, decode)
}

// DecodeStatusResponse checks the answer resp of device dst to a status
// request and decodes it with decode: INFO_LEVEL (10 06) or ACK_STATUS
// (02 01) are decoded, a NACK or a missing answer is an error.
func DecodeStatusResponse[E any](dst [3]byte, resp *bidcos.Packet, decode func([]byte) (E, error)) (E, error) {
	var zero E
	if resp == nil {
		return zero, hm.ErrNoResponse
	}
	if resp.Source != dst {
		return zero, fmt.Errorf("response from unexpected device %x", resp.Source)
	}
	if resp.Auth() == bidcos.AuthFailed {
		return zero, fmt.Errorf("response failed AES authentication")
	}
	switch {
	case resp.Cmd == bidcos.Info && len(resp.Payload) > 0 && resp.Payload[0] == bidcos.InfoActuatorStatus,
		hm.IsAckStatus(resp):
		return decode(resp.Payload)
	case resp.Cmd == bidcos.Ack:
		if err := hm.CheckAck(dst, resp); err != nil {
			return zero, err
		}
		return zero, fmt.Errorf("status request answered with a plain ACK")
	}
	return zero, fmt.Errorf("unexpected answer to status request: cmd 0x%02x payload %x", resp.Cmd, resp.Payload)
}
