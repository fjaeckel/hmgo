// Package bidcos implements the HomeMatic BidCoS (bidirectional
// communication standard) radio protocol.
package bidcos

import (
	"context"
	"fmt"
	"sync"
)

// cmd is top-level (e.g. SET), frames usually specify a subtype (e.g. MANU_MODE_SET)

// BidCoS commands
const (
	DeviceInfo byte = iota
	Config
	Ack
	Info             = 0x10
	ClimateEvent     = 0x58
	ThermalControl   = 0x5a
	PowerEventCyclic = 0x5e
	PowerEvent       = 0x5f
	WeatherEvent     = 0x70
	Timestamp        = 0x3f
)

// BidCoS Config subcommands
const (
	_ byte = iota
	ConfigPeerAdd
	ConfigPeerRemove
	ConfigPeerListReq
	ConfigParamReq
	ConfigStart
	ConfigEnd
	ConfigWriteIndexSeq
	ConfigWriteIndexPairs
	ConfigSerialReq
	ConfigPairSerial
	_
	_
	_
	ConfigStatusRequest
)

// BidCoS Info subcommands
const (
	InfoSerial byte = iota
	InfoPeerList
	InfoParamResponsePairs
	InfoParamResponseSeq
	InfoParamChange
	_
	InfoActuatorStatus
	InfoTemp = 0x0a
)

// Packet flags
const (
	// Wake up the destination device from power-save mode.
	WakeUp byte = 1 << iota
	// Device is awake, send messages now.
	WakeMeUp
	// Send message to all devices.
	Broadcast
	_
	// Wake up the destination device from power-save mode.
	Burst
	// Bi-directional, i.e. response expected.
	BiDi
	// Packet was repeated (not seen in the wild).
	Repeated
	// Packet can be repeated (always set).
	RepeatEnable
)

const DefaultFlags = RepeatEnable | BiDi

// Packet is a BidCoS packet.
type Packet struct {
	status  uint8
	info    uint8
	rssi    uint8
	Msgcnt  uint8
	Flags   uint8 // see Packet flags above
	Cmd     uint8 // see BidCoS commands above
	Source  [3]byte
	Dest    [3]byte
	Payload []byte // at most 17 bytes
}

var messageCounter struct {
	counter byte
	sync.RWMutex
}

func (p *Packet) Encode() []byte {
	// c.f. https://svn.fhem.de/trac/browser/trunk/fhem/FHEM/00_HMUARTLGW.pm?rev=13367#L1464
	// c.f. https://github.com/Homegear/Homegear-HomeMaticBidCoS/blob/5255288954f3da42e12fa72a06963b99089d323f/src/PhysicalInterfaces/Hm-Mod-Rpi-Pcb.cpp#L858
	messageCounter.Lock()
	defer messageCounter.Unlock()
	cnt := p.Msgcnt
	if cnt == 0 {
		cnt = messageCounter.counter
		// The Homematic CCU2 increments its message counter by 9 between
		// each message. My guess is that the resulting pattern has better
		// radio characteristics.
		messageCounter.counter += 9
	}
	var burst byte
	if p.Flags&0x10 == 0x10 {
		burst = 0x01
	}
	res := []byte{
		0x00, // status
		0x00, // info
		burst,
		cnt,
		p.Flags,
		p.Cmd,
	}
	res = append(res, p.Source[:]...)
	res = append(res, p.Dest[:]...)
	res = append(res, p.Payload...)
	return res
}

// Receive status values as reported by the HM-MOD-RPI-PCB in the
// first byte of a received frame (c.f. HMUARTLGW_RECV_* in FHEM).
const (
	recvResp          = 0x01
	recvRespWithAESOK = 0x02
	recvRespWithAESKO = 0x03
	recvTrig          = 0x11
	recvTrigWithAESOK = 0x12
)

// The gateway reports device responses to our own packets inside an
// AppAck frame, whose first byte uses a different numbering (e.g. 0x03
// means “ACK with response”, not “AES failed”). Transports must rewrite
// that byte to the receive status values above before handing the
// frame to Decode; see uartgw.Conn.Send.

// Auth describes whether the gateway verified the packet with AES.
type Auth uint8

const (
	// Unauthenticated means the packet was not AES-signed (the channel
	// is not configured for AES, or the frame type is never signed).
	Unauthenticated Auth = iota
	// AuthOK means the gateway completed an AES challenge successfully.
	AuthOK
	// AuthFailed means the gateway issued an AES challenge and the
	// device answered it incorrectly (or not at all). Such packets must
	// not be trusted.
	AuthFailed
)

func (a Auth) String() string {
	switch a {
	case Unauthenticated:
		return "unauthenticated"
	case AuthOK:
		return "aes-ok"
	case AuthFailed:
		return "aes-failed"
	default:
		return fmt.Sprintf("auth(%d)", uint8(a))
	}
}

// Auth reports the AES verification result for a packet received from
// the gateway (either as an AppRecv frame or as the response part of
// an AppAck frame).
func (p *Packet) Auth() Auth {
	switch p.status {
	case recvRespWithAESOK, recvTrigWithAESOK:
		return AuthOK
	case recvRespWithAESKO:
		return AuthFailed
	}
	return Unauthenticated
}

// RSSI returns the received signal strength in dBm (negative), or 0 if
// unknown.
func (p *Packet) RSSI() int {
	if p.rssi <= 1 {
		return 0
	}
	return -int(p.rssi)
}

// Decode parses a frame as delivered by the gateway:
//
//	uint8   status (see Auth)
//	uint8   info
//	uint8   rssi
//	uint8   message counter
//	uint8   flags
//	uint8   command
//	[3]byte source
//	[3]byte destination
//	[]byte  payload
func Decode(b []byte) (*Packet, error) {
	if got, want := len(b), 12; got < want {
		return nil, fmt.Errorf("too short for a bidcos packet: got %d, want >= %d", got, want)
	}
	return &Packet{
		status:  b[0],
		info:    b[1],
		rssi:    b[2],
		Msgcnt:  b[3],                        // hg: “message counter”
		Flags:   b[4],                        // hg: “control byte”
		Cmd:     b[5],                        // hg: “message type”
		Source:  [3]byte{b[6], b[7], b[8]},   // hg: “senderAddress”
		Dest:    [3]byte{b[9], b[10], b[11]}, // hg: “destinationAddress”
		Payload: b[12:],
	}, nil
}

// Transport sends encoded BidCoS packets. It is implemented by
// uartgw.Conn.
type Transport interface {
	// Send transmits pkt (as produced by Packet.Encode) and waits until
	// the gateway confirmed the transmission. If the packet requested a
	// response (BiDi flag), the device's response frame is returned in
	// the format accepted by Decode. Otherwise, the returned frame is
	// nil.
	Send(ctx context.Context, encoded []byte) ([]byte, error)
}

// Sender is a convenience wrapper around a Transport which fills in the
// BidCoS source address for outgoing packets and decodes responses.
type Sender struct {
	Transport Transport
	Addr      [3]byte
}

func NewSender(t Transport, addr [3]byte) *Sender {
	return &Sender{Transport: t, Addr: addr}
}

// Send transmits pkt and returns the decoded device response, or nil if
// no response was requested or received.
func (s *Sender) Send(ctx context.Context, pkt *Packet) (*Packet, error) {
	pkt.Source = s.Addr
	resp, err := s.Transport.Send(ctx, pkt.Encode())
	if err != nil {
		return nil, err
	}
	if len(resp) == 0 {
		return nil, nil
	}
	return Decode(resp)
}
