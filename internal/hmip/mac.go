// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package hmip

import (
	"encoding/binary"
	"fmt"
)

// MAC frame layout (specification, mac-application-frames.md, marked as
// verified; confirmed by the captures):
//
//	byte 0: fragmentation(1) cyclic(1) security control(2) content type(4)
//	byte 1: reserved(1) address extension(4) reserved(2) piggyback ack(1)
//	byte 2: version(4) local hop limit(3) QoS low(1)
//	source address (3), destination address (3)
//	security number (4) + integrity code (4 or 8), if secured and present
//	address extension data, if the extension is not 0 (not supported)
//	cyclic telegram counter (1), if the cyclic flag is set
//	payload…

// ContentType is the low nibble of MAC header byte 0.
type ContentType byte

// Content types named by the specification.
const (
	ContentApplication ContentType = 0
	ContentNetworkMgmt ContentType = 1
	ContentRouteMgmt   ContentType = 3
	ContentMACControl  ContentType = 4
)

func (c ContentType) String() string {
	switch c {
	case ContentApplication:
		return "APP"
	case ContentNetworkMgmt:
		return "NETMGMT"
	case ContentRouteMgmt:
		return "ROUTE"
	case ContentMACControl:
		return "MACCTL"
	}
	// 2 and 5-9: (compressed) IP variants per the specification, not
	// detailed; the captures show type 2 frames to F00001 at startup.
	return fmt.Sprintf("CT%d", byte(c))
}

// SecurityControl is the 2-bit security field of MAC header byte 0.
type SecurityControl byte

// Security control values.
const (
	SecurityOff      SecurityControl = 0 // no security fields
	SecurityOn       SecurityControl = 1 // security number (4) + integrity code (4)
	SecurityExtended SecurityControl = 2 // security number (4) + integrity code (8)
	SecurityReserved SecurityControl = 3
)

// MinMACHeaderLen is the length of a MAC header without optional fields.
const MinMACHeaderLen = 9

// MACHeader is a decoded HmIP MAC header.
type MACHeader struct {
	Fragmented   bool
	Cyclic       bool
	Security     SecurityControl
	Content      ContentType
	AddrExt      byte // address extension (0: single hop)
	PiggybackAck bool
	Version      byte // HomematicIP version (8 in all captures)
	HopLimit     byte
	QoSLow       bool
	Src, Dst     Address

	// SecurityFields reports whether the security number and integrity
	// code are present (received frames); hosts omit them in the
	// frames they hand to the module for sending.
	SecurityFields bool
	SecurityNumber uint32 // the transmitter's security counter (captures; opaque per the specification)
	MIC            []byte // integrity code (4 or 8 bytes)

	CyclicCounter byte // if Cyclic

	Len int // header length = offset of the payload
}

// Frame is a MAC frame: header and payload.
type Frame struct {
	MACHeader
	Payload []byte
}

// ParseFrame decodes a MAC frame. securityFields says whether the
// security number and integrity code of a secured frame are present:
// true for frames received off the air (Event), false for frames the
// host passes to the module (Send) — this cannot be told from the frame
// itself. On error, the fields decoded so far are returned. Payload and
// MIC alias b.
func ParseFrame(b []byte, securityFields bool) (Frame, error) {
	var f Frame
	if len(b) < MinMACHeaderLen {
		return f, fmt.Errorf("%w: MAC header of %d bytes", ErrShort, len(b))
	}
	f.Fragmented = b[0]&0x80 != 0
	f.Cyclic = b[0]&0x40 != 0
	f.Security = SecurityControl(b[0] >> 4 & 0x03)
	f.Content = ContentType(b[0] & 0x0f)
	f.AddrExt = b[1] >> 3 & 0x0f
	f.PiggybackAck = b[1]&0x01 != 0
	f.Version = b[2] >> 4
	f.HopLimit = b[2] >> 1 & 0x07
	f.QoSLow = b[2]&0x01 != 0
	copy(f.Src[:], b[3:6])
	copy(f.Dst[:], b[6:9])
	i := MinMACHeaderLen
	f.Len = i

	if f.Security == SecurityReserved {
		return f, fmt.Errorf("%w: reserved security control 3", ErrUnsupported)
	}
	if securityFields && f.Security != SecurityOff {
		micLen := 4
		if f.Security == SecurityExtended {
			micLen = 8
		}
		if len(b) < i+4+micLen {
			return f, fmt.Errorf("%w: security fields need %d bytes, have %d", ErrShort, 4+micLen, len(b)-i)
		}
		f.SecurityFields = true
		f.SecurityNumber = binary.BigEndian.Uint32(b[i:])
		f.MIC = b[i+4 : i+4+micLen]
		i += 4 + micLen
		f.Len = i
	}
	if f.AddrExt != 0 {
		// The specification leaves the length of the extension data
		// open (routed/IP traffic is out of its scope).
		return f, fmt.Errorf("%w: address extension %d", ErrUnsupported, f.AddrExt)
	}
	if f.Cyclic {
		if len(b) < i+1 {
			return f, fmt.Errorf("%w: missing cyclic counter", ErrShort)
		}
		f.CyclicCounter = b[i]
		i++
		f.Len = i
	}
	f.Payload = b[i:]
	return f, nil
}

// FrameType is the 6-bit frame type of an application header.
type FrameType byte

// Application frame types named by the specification.
var frameTypeNames = map[FrameType]string{
	1:  "CONFIG",
	2:  "ANSWER",
	5:  "STATUS",
	6:  "DIRECT_EXEC",
	8:  "SWITCH",
	9:  "COND_SWITCH",
	10: "LEVEL",
	14: "HEATING_MODE",
	16: "OEM",
	32: "SERVICE",
	33: "STATUS_REQUEST",
	34: "TRIGGER_STATUS",
	35: "TIME_INFO",
	62: "LIVE_UPDATE",
	63: "OTAU",
}

func (t FrameType) String() string {
	if n, ok := frameTypeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("TYPE_%02X", byte(t))
}

// AppHeader is the 2-byte header of an application frame (content type
// ContentApplication).
type AppHeader struct {
	ResponseRequested bool
	StayAwake         bool
	Type              FrameType
	Seq               byte
}

// App decodes the application header of an application frame and
// returns it with the rest of the payload.
func (f Frame) App() (AppHeader, []byte, error) {
	if f.Content != ContentApplication {
		return AppHeader{}, nil, fmt.Errorf("%w: content type %v is not an application frame", ErrUnsupported, f.Content)
	}
	if len(f.Payload) < 2 {
		return AppHeader{}, nil, fmt.Errorf("%w: application header of %d bytes", ErrShort, len(f.Payload))
	}
	b := f.Payload[0]
	return AppHeader{
		ResponseRequested: b&0x80 != 0,
		StayAwake:         b&0x40 != 0,
		Type:              FrameType(b & 0x3f),
		Seq:               f.Payload[1],
	}, f.Payload[2:], nil
}

// Network management frame types, from the specification's captured
// worked examples (inclusion request/accept, device removal).
var netMgmtNames = map[byte]string{
	0x10: "INCLUSION_REQUEST",
	0x20: "INCLUSION_ACCEPT",
	0xf0: "REMOVE_1",
	0xf2: "REMOVE_2",
}

// NetMgmtTypeName names the first payload byte of a network management
// frame.
func NetMgmtTypeName(t byte) string {
	if n, ok := netMgmtNames[t]; ok {
		return n
	}
	return fmt.Sprintf("NM_%02X", t)
}
