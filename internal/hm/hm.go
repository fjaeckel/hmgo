// Package hm contains the parts of the HomeMatic device models that are
// shared between device types: the StandardDevice base type, response
// (ACK) checking, generic frames (device info, time request) and the
// climate control command encoders used by both HM-CC-RT-DN and
// HM-TC-IT-WM-W-EU.
//
// Device packages (heating, thermal, contact, power) build on this. They
// do not read from the radio themselves: received frames are pushed into
// their Decode methods by the bridge's event loop, and commands are sent
// with bidcos.Sender.Send, which returns the device's direct response.
package hm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"

	"github.com/stapelberg/hmgo/internal/bidcos"
)

const (
	Mask1Bit = 0x1
	Mask2Bit = 0x3
	Mask3Bit = 0x7
	Mask4Bit = 0xF
	Mask5Bit = 0x1F
	Mask6Bit = 0x3F
	Mask7Bit = 0x7F
	Mask8Bit = 0xFF
)

// Device is implemented by all device models.
type Device interface {
	// Name is the human readable name from the configuration.
	Name() string
	// Address is the BidCoS address.
	Address() [3]byte
	// AddrHex is the BidCoS address as 6 lowercase hex digits.
	AddrHex() string
	// Model is the eQ-3 model name, e.g. "HM-CC-RT-DN".
	Model() string
	// Channels is the highest channel number (channel 0 is the
	// maintenance channel).
	Channels() int
	// HomeMaticType is a short type name used for metrics/status
	// ("heating", "thermal", "contact", "power").
	HomeMaticType() string
	// MostRecentEvents returns the most recent decoded events for the
	// status page.
	MostRecentEvents() []Event
	// Decode decodes a packet received from this device. It returns a
	// typed event (a pointer to one of the device package's event
	// structs, or *hm.DeviceInfo / *hm.TimeRequest), or nil if the
	// packet type is not relevant for this device.
	Decode(pkt *bidcos.Packet) (any, error)
}

type Event interface {
	HTML() template.HTML
}

// FullyQualifiedChannel identifies a channel at a specific
// BidCoS-addressed peer.
type FullyQualifiedChannel struct {
	Peer    [3]byte
	Channel byte
}

// StandardDevice encapsulates behavior shared by all HomeMatic
// devices. BCS may be nil for devices that are only used for decoding
// (e.g. in tests); commands then fail with ErrNoSender.
type StandardDevice struct {
	BCS       *bidcos.Sender
	Addr      [3]byte
	HumanName string

	NumChannels int
	ModelName   string
}

// NewStandardDevice returns a StandardDevice. bcs may be nil.
func NewStandardDevice(bcs *bidcos.Sender, addr [3]byte, name string) StandardDevice {
	return StandardDevice{BCS: bcs, Addr: addr, HumanName: name}
}

func (sd *StandardDevice) Name() string     { return sd.HumanName }
func (sd *StandardDevice) Channels() int    { return sd.NumChannels }
func (sd *StandardDevice) Model() string    { return sd.ModelName }
func (sd *StandardDevice) Address() [3]byte { return sd.Addr }

func (sd *StandardDevice) AddrHex() string {
	return hex.EncodeToString(sd.Addr[:])
}

func (sd *StandardDevice) String() string {
	return fmt.Sprintf("[BidCoS:%s]", sd.AddrHex())
}

// ErrNoSender is returned by commands on a device without a Sender.
var ErrNoSender = errors.New("hm: device has no bidcos sender")

// Send sends pkt to the device (pkt.Dest is set to the device address)
// and checks the device's response with CheckAck. It returns the
// response packet (whose payload starts with the ACK subtype) on
// success.
func (sd *StandardDevice) Send(ctx context.Context, pkt *bidcos.Packet) (*bidcos.Packet, error) {
	if sd.BCS == nil {
		return nil, ErrNoSender
	}
	pkt.Dest = sd.Addr
	resp, err := sd.BCS.Send(ctx, pkt)
	if err != nil {
		return nil, fmt.Errorf("%v: %w", sd, err)
	}
	if pkt.Flags&bidcos.BiDi == 0 {
		return resp, nil
	}
	if err := CheckAck(sd.Addr, resp); err != nil {
		return nil, fmt.Errorf("%v: %w", sd, err)
	}
	return resp, nil
}

// BidCoS ACK (cmd 0x02) subtypes, c.f. CUL_HM_parseCommon in FHEM.
const (
	AckOK     = 0x00 // plain ACK
	AckStatus = 0x01 // ACK_STATUS: ACK + channel status
	AckAES    = 0x04 // AES challenge (handled by the radio module)
	// Subtypes 0x80 and above are NACKs (0x80 NACK, 0x82 peer list
	// full, 0x84 undefined register, 0x85 peer not accepted).
	NackMin = 0x80
)

// NackError is returned when a device answers with a NACK.
type NackError struct {
	Code byte
}

func (e *NackError) Error() string {
	return fmt.Sprintf("device answered NACK (0x%02x)", e.Code)
}

// ErrNoResponse is returned by CheckAck when a response was expected but
// none was received.
var ErrNoResponse = errors.New("no response from device")

// CheckAck validates a device response to a BiDi request sent to dst.
// It returns nil for ACK (0x02 00) and ACK_STATUS (0x02 01), a
// *NackError for NACKs, and an error for missing responses, responses
// from another device, non-ACK frames and responses whose AES
// authentication failed.
func CheckAck(dst [3]byte, resp *bidcos.Packet) error {
	if resp == nil {
		return ErrNoResponse
	}
	if resp.Source != dst {
		return fmt.Errorf("response from unexpected device %x", resp.Source)
	}
	if resp.Auth() == bidcos.AuthFailed {
		return fmt.Errorf("response failed AES authentication")
	}
	if resp.Cmd != bidcos.Ack {
		return fmt.Errorf("unexpected response command 0x%02x (want ACK 0x02)", resp.Cmd)
	}
	if len(resp.Payload) < 1 {
		return fmt.Errorf("empty ACK payload")
	}
	switch st := resp.Payload[0]; {
	case st == AckOK, st == AckStatus:
		return nil
	case st >= NackMin:
		return &NackError{Code: st}
	default:
		return fmt.Errorf("unexpected ACK subtype 0x%02x", st)
	}
}

// IsAckStatus reports whether pkt is an ACK_STATUS frame (cmd 0x02,
// payload[0] 0x01) with at least channel and one status byte.
func IsAckStatus(pkt *bidcos.Packet) bool {
	return pkt != nil && pkt.Cmd == bidcos.Ack && len(pkt.Payload) >= 3 && pkt.Payload[0] == AckStatus
}

// DeviceInfo is the DEVICE_INFO frame (cmd 0x00) a device sends when its
// config button is pressed (pairing request). Decoded as in FHEM's
// CUL_HM_infoUpdtDevData.
type DeviceInfo struct {
	Firmware string // e.g. "1.4"
	ModelID  uint16 // e.g. 0x0095 for HM-CC-RT-DN
	Serial   string // e.g. "MEQ0123456"
	Class    byte   // device class / subtype byte
	Extra    []byte // remaining device info bytes
}

func (di *DeviceInfo) HTML() template.HTML {
	return template.HTML(template.HTMLEscapeString(fmt.Sprintf("Device info: firmware %s, model 0x%04x, serial %s", di.Firmware, di.ModelID, di.Serial)))
}

// DecodeDeviceInfo decodes a DEVICE_INFO (cmd 0x00) payload.
func DecodeDeviceInfo(payload []byte) (*DeviceInfo, error) {
	if len(payload) < 14 {
		return nil, fmt.Errorf("device info: payload too short: got %d, want >= 14", len(payload))
	}
	return &DeviceInfo{
		Firmware: fmt.Sprintf("%d.%d", payload[0]>>4, payload[0]&0xf),
		ModelID:  uint16(payload[1])<<8 | uint16(payload[2]),
		Serial:   string(payload[3:13]),
		Class:    payload[13],
		Extra:    append([]byte(nil), payload[14:]...),
	}, nil
}

// TimeRequest is the time request (cmd 0x3F) sent by HM-CC-RT-DN and
// HM-TC-IT-WM-W-EU to their central. The central should answer with
// TimestampResponse(Msgcnt, ...).
type TimeRequest struct {
	Msgcnt byte    // message counter to use for the response
	Dest   [3]byte // addressee; only answer requests addressed to the central
}

func (tr *TimeRequest) HTML() template.HTML { return "Time request" }

// DecodeCommon decodes the frames any device may send: DEVICE_INFO
// (0x00) → *DeviceInfo, time request (0x3F) → *TimeRequest. It returns
// nil, nil for other frames.
func DecodeCommon(pkt *bidcos.Packet) (any, error) {
	switch pkt.Cmd {
	case bidcos.DeviceInfo:
		return DecodeDeviceInfo(pkt.Payload)
	case bidcos.Timestamp:
		return &TimeRequest{Msgcnt: pkt.Msgcnt, Dest: pkt.Dest}, nil
	}
	return nil, nil
}

// CheckIncoming verifies that pkt was sent by sd and did not fail AES
// authentication. Device Decode methods call it first.
func (sd *StandardDevice) CheckIncoming(pkt *bidcos.Packet) error {
	if pkt == nil {
		return fmt.Errorf("%v: nil packet", sd)
	}
	if pkt.Source != sd.Addr {
		return fmt.Errorf("%v: packet from other device %x", sd, pkt.Source)
	}
	if pkt.Auth() == bidcos.AuthFailed {
		return fmt.Errorf("%v: packet failed AES authentication, ignoring", sd)
	}
	return nil
}
