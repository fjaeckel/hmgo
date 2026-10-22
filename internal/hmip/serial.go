// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package hmip decodes the Homematic IP (HmIP) traffic between a host and
// an eQ-3 radio module running the “DualCoPro” firmware (RPI-RF-MOD), as
// far as it is needed to see it: the HmIP serial commands on module
// destination 0x02, and the MAC (and application) header of the radio
// frames they carry.
//
// It is receive/visibility only. There is deliberately no encoder for
// anything that would be sent to the module, no key handling, no
// encryption and no pairing (see docs/HMIP.md).
//
// Sources:
//
//   - The community specification “HomematicIP Serial & Keyserver
//     Protocol Specification” (https://github.com/FrameShiftLab/hmip-protocol-specs
//     at 90a4c2d; docs/serial-protocol.md and
//     docs/mac-application-frames.md). Its prose is CC BY-NC-SA 4.0 and
//     was not copied; this package is an independent implementation of
//     the facts it reports (bit layouts, command numbers), restricted to
//     what it marks as verified on the wire.
//   - hmsniff captures of OpenCCU's HMIPServer talking to an RPI-RF-MOD
//     with firmware 4.4.22 (testdata/captures.txt), which agree with the
//     specification where both cover the same frames; the differences
//     are listed in docs/HMIP.md.
package hmip

import (
	"errors"
	"fmt"
)

// Module destinations carrying HmIP traffic.
const (
	// DstHMIP is the HmIP destination of the DualCoPro firmware.
	DstHMIP = 0x02
	// DstKey is used by HMIPServer only at its start, right after
	// loading the (encrypted) network key (captures). Undocumented by
	// the specification; its payloads look like key material, so they
	// are never shown.
	DstKey = 0x0c
)

// Command is the command byte of an HmIP serial packet (destination
// DstHMIP).
type Command byte

// HmIP serial commands (specification, serial-protocol.md). Values the
// captures exercised are marked “seen”.
const (
	CmdSetAddress           Command = 0x00 // host→module, data: own radio address (3); seen
	CmdGetAddress           Command = 0x01
	CmdSetNetworkKey        Command = 0x02
	CmdSend                 Command = 0x03 // host→module, data: burst mode, MAC frame; seen
	CmdAddLinkPartner       Command = 0x04 // host→module, data: address (3), mode, router address (3)
	CmdRemoveLinkPartner    Command = 0x05 // host→module, data: address (3)
	CmdResponse             Command = 0x06 // module→host, data: status, result…; seen
	CmdEvent                Command = 0x07 // module→host, data: RSSI, MAC frame; seen
	CmdSetSecurityCounter   Command = 0x08 // host→module, data: counter (4); seen
	CmdStoreSecurityCounter Command = 0x09
	CmdGetSecurityCounter   Command = 0x0a // response: status, counter (4); seen
	CmdAddListener          Command = 0x0f
	CmdRemoveListener       Command = 0x10
	CmdGetInclusionData     Command = 0x11
	CmdGetLinkPartners      Command = 0x12 // response: status, address (3)…; seen
	CmdGetEncNetworkKey     Command = 0x13
	CmdSetEncNetworkKey     Command = 0x14 // seen (66 bytes)
	CmdSendInclusionAccept  Command = 0x18
)

var commandNames = map[Command]string{
	CmdSetAddress:           "SET_ADDRESS",
	CmdGetAddress:           "GET_ADDRESS",
	CmdSetNetworkKey:        "SET_NETWORK_KEY",
	CmdSend:                 "SEND",
	CmdAddLinkPartner:       "ADD_LINK_PARTNER",
	CmdRemoveLinkPartner:    "REMOVE_LINK_PARTNER",
	CmdResponse:             "RESPONSE",
	CmdEvent:                "EVENT",
	CmdSetSecurityCounter:   "SET_SECURITY_COUNTER",
	CmdStoreSecurityCounter: "STORE_SECURITY_COUNTER",
	CmdGetSecurityCounter:   "GET_SECURITY_COUNTER",
	CmdAddListener:          "ADD_LISTENER",
	CmdRemoveListener:       "REMOVE_LISTENER",
	CmdGetInclusionData:     "GET_INCLUSION_DATA",
	CmdGetLinkPartners:      "GET_LINK_PARTNERS",
	CmdGetEncNetworkKey:     "GET_ENC_NETWORK_KEY",
	CmdSetEncNetworkKey:     "SET_ENC_NETWORK_KEY",
	CmdSendInclusionAccept:  "SEND_INCLUSION_ACCEPT",
}

func (c Command) String() string {
	if n, ok := commandNames[c]; ok {
		return n
	}
	switch {
	case c >= 0x0b && c <= 0x0e:
		// The specification names the range only (CSMA and send
		// attempt defaults, get/set), not the individual commands.
		return fmt.Sprintf("CSMA_DEFAULTS_%02X", byte(c))
	case c >= 0x15 && c <= 0x17:
		return fmt.Sprintf("OTAU_%02X", byte(c))
	case c >= 0x19 && c <= 0x1e:
		// adapter MIC / connection data / inclusion accept data
		// variants; not detailed by the specification.
		return fmt.Sprintf("ADAPTER_%02X", byte(c))
	}
	return fmt.Sprintf("CMD_%02X", byte(c))
}

// Sensitive reports whether the data of the command (and of the
// module's response to it) may contain key material or values derived
// from it. Such data is never shown.
func (c Command) Sensitive() bool {
	switch c {
	case CmdSetNetworkKey, CmdGetInclusionData, CmdGetEncNetworkKey, CmdSetEncNetworkKey, CmdSendInclusionAccept:
		return true
	}
	return c >= 0x19 && c <= 0x1e
}

// Status is the first data byte of a CmdResponse packet.
type Status byte

var statusNames = map[Status]string{
	1:  "OK",
	2:  "BUSY",
	3:  "TIMEOUT",
	4:  "NO_REPLY",
	5:  "ROUTED",
	6:  "DUTY_CYCLE",
	8:  "ERROR",
	9:  "OK", // seen after SEND (all other commands: 1)
	10: "WRONG_INPUT",
}

func (s Status) String() string {
	if n, ok := statusNames[s]; ok {
		return fmt.Sprintf("%s(%02X)", n, byte(s))
	}
	return fmt.Sprintf("STATUS_%02X", byte(s))
}

// OK reports whether s is one of the two success codes.
func (s Status) OK() bool { return s == 1 || s == 9 }

// Address is a 3-byte HmIP radio (MAC) address.
type Address [3]byte

func (a Address) String() string { return fmt.Sprintf("%02X%02X%02X", a[0], a[1], a[2]) }

// Errors returned by the decoders. They are wrapped with details.
var (
	ErrShort       = errors.New("hmip: too short")
	ErrCRC         = errors.New("hmip: bad checksum")
	ErrFraming     = errors.New("hmip: bad framing")
	ErrUnsupported = errors.New("hmip: unsupported layout")
	ErrCommand     = errors.New("hmip: unexpected command")
)

// Packet is one serial packet on an HmIP destination: the part of a
// module frame between the length and the checksum.
type Packet struct {
	Dst  byte
	Cnt  byte // sequence counter; a response repeats the request's
	Cmd  Command
	Data []byte
}

// ParsePacket builds a Packet from a frame's destination, counter and
// the bytes following them (command byte, data…). Data aliases b.
func ParsePacket(dst, cnt byte, b []byte) (Packet, error) {
	if len(b) == 0 {
		return Packet{}, fmt.Errorf("%w: packet without command byte", ErrShort)
	}
	return Packet{Dst: dst, Cnt: cnt, Cmd: Command(b[0]), Data: b[1:]}, nil
}

// CRC16 is the frame checksum: CRC-16, polynomial 0x8005, MSB first,
// initial value 0xffff, over length..data (the specification's
// formulation; uartgw's “init 0xd77f including the 0xfd delimiter” is
// the same computation).
func CRC16(b []byte) uint16 {
	crc := uint16(0xffff)
	for _, x := range b {
		crc ^= uint16(x) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// ParseRaw decodes a complete unescaped frame (0xfd, length (2), dst,
// cnt, command, data…, CRC (2)), as hmsniff logs it in its “raw”
// lines. Data aliases raw.
func ParseRaw(raw []byte) (Packet, error) {
	if len(raw) < 8 {
		return Packet{}, fmt.Errorf("%w: frame of %d bytes", ErrShort, len(raw))
	}
	if raw[0] != 0xfd {
		return Packet{}, fmt.Errorf("%w: frame does not start with 0xfd", ErrFraming)
	}
	n := int(raw[1])<<8 | int(raw[2])
	if n < 3 || 3+n+2 != len(raw) {
		return Packet{}, fmt.Errorf("%w: length field %d does not match %d frame bytes", ErrFraming, n, len(raw))
	}
	body := raw[1 : 3+n]
	if got, want := uint16(raw[3+n])<<8|uint16(raw[4+n]), CRC16(body); got != want {
		return Packet{}, fmt.Errorf("%w: %04x, computed %04x", ErrCRC, got, want)
	}
	return ParsePacket(raw[3], raw[4], raw[5:3+n])
}

// rssiNone is the RSSI byte of the module's reports of its own
// transmissions (captures; the specification uses the same value as the
// “no reading” sentinel of a Status frame's RSSI).
const rssiNone = 0x80

// Event is a CmdEvent packet: a radio frame the module hands to the host.
type Event struct {
	RSSIRaw byte
	Frame   Frame
}

// RSSI returns the module's signal strength measurement in dBm; ok is
// false for the “no reading” value 0x80.
func (e Event) RSSI() (dbm int, ok bool) {
	if e.RSSIRaw == rssiNone {
		return 0, false
	}
	return -int(e.RSSIRaw), true
}

// Echo reports whether the event is the module reporting a frame the
// host made it transmit, rather than a frame heard from a device: in
// the captures every such report carries RSSI 0x80 (and the host's own
// address as source), while the one device frame has a real reading.
// This is an observation, not documented by the specification.
func (e Event) Echo() bool { return e.RSSIRaw == rssiNone }

// Event decodes a CmdEvent packet. Received frames carry their security
// fields (specification and captures). If only the MAC frame is
// malformed, the returned Event still has RSSIRaw and the partial frame.
func (p Packet) Event() (Event, error) {
	if p.Cmd != CmdEvent {
		return Event{}, fmt.Errorf("%w: %v, want EVENT", ErrCommand, p.Cmd)
	}
	if len(p.Data) < 1 {
		return Event{}, fmt.Errorf("%w: event without RSSI", ErrShort)
	}
	f, err := ParseFrame(p.Data[1:], true)
	return Event{RSSIRaw: p.Data[0], Frame: f}, err
}

// Send decodes a CmdSend packet (host→module): burst mode and the MAC
// frame. The host omits the security fields of secured frames (the
// module inserts them; specification walkthrough and all captured
// sends), so they are not expected here.
func (p Packet) Send() (burst byte, f Frame, err error) {
	if p.Cmd != CmdSend {
		return 0, Frame{}, fmt.Errorf("%w: %v, want SEND", ErrCommand, p.Cmd)
	}
	if len(p.Data) < 1 {
		return 0, Frame{}, fmt.Errorf("%w: send without burst mode", ErrShort)
	}
	f, err = ParseFrame(p.Data[1:], false)
	return p.Data[0], f, err
}

// Response decodes a CmdResponse packet into its status and result.
func (p Packet) Response() (Status, []byte, error) {
	if p.Cmd != CmdResponse {
		return 0, nil, fmt.Errorf("%w: %v, want RESPONSE", ErrCommand, p.Cmd)
	}
	if len(p.Data) < 1 {
		return 0, nil, fmt.Errorf("%w: response without status", ErrShort)
	}
	return Status(p.Data[0]), p.Data[1:], nil
}

// Addresses splits b into 3-byte addresses (link partner list, add/
// remove link partner); a trailing partial address is an error.
func Addresses(b []byte) ([]Address, error) {
	if len(b)%3 != 0 {
		return nil, fmt.Errorf("%w: %d bytes is not a list of addresses", ErrFraming, len(b))
	}
	out := make([]Address, 0, len(b)/3)
	for i := 0; i+3 <= len(b); i += 3 {
		out = append(out, Address{b[i], b[i+1], b[i+2]})
	}
	return out, nil
}
