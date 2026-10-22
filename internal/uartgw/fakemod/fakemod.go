// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package fakemod emulates a HM-MOD-RPI-PCB radio module (classic
// firmware 1.x, or optionally the dual-coprocessor firmware as seen by
// the classic protocol) or an RPI-RF-MOD (dual-coprocessor firmware,
// Options.DualCoPro; see dual.go) for tests.
//
// New returns the module and the host end of an in-memory pipe, which
// can be passed to uartgw.Open:
//
//	mod, rw := fakemod.New(fakemod.Options{})
//	defer mod.Close()
//	conn, err := uartgw.Open(rw, uartgw.Config{HMID: [3]byte{0x12, 0x34, 0x56}})
//
// The module records the configuration it received (HMID, keys, peers,
// transmitted BidCoS frames) for assertions, and InjectRecv simulates
// radio frames received from devices.
package fakemod

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"
)

// Destinations.
const (
	DstOS      = 0x00
	DstApp     = 0x01
	DstDual    = 0xfe
	DstDualErr = 0xff
)

// Ack status values (see DESIGN.md / FHEM HMUARTLGW_ACK_*).
const (
	AckNACK                = 0x00
	AckOK                  = 0x01
	AckInfo                = 0x02
	AckWithResponse        = 0x03
	AckEUnknown            = 0x04
	AckENoCredits          = 0x05
	AckECSMACA             = 0x06
	AckWithMultipartData   = 0x07
	AckEInProgress         = 0x08
	AckWithResponseAESOK   = 0x0c
	AckWithResponseAESFail = 0x0d

	// NoAck is not a real ack status: returned from Options.OnSend, it
	// makes the module swallow the request without acknowledging it.
	NoAck = 0xff
)

// Key slots.
const (
	SlotCurrent  = 0x03
	SlotTemp     = 0x0b
	SlotPrevious = 0x0f
)

// DefaultRSSI is the raw RSSI byte used for injected frames and device
// responses (-65 dBm).
const DefaultRSSI = 0x41

// Options configures the emulated module.
type Options struct {
	// DualFirmware emulates the dual-coprocessor (Homematic IP)
	// firmware: the module announces “DualCoPro_App” on dst 254 and
	// answers every request on dst 255.
	DualFirmware bool

	// StartInApp emulates a module which was not reset: the
	// application is already running and no announcement is sent.
	StartInApp bool

	// Mute makes the module ignore all requests (and send no
	// announcement), emulating a module which does not respond.
	Mute bool

	// Firmware is the reported firmware version (default 1.4.1;
	// DualCoPro: 4.4.22).
	Firmware [3]byte

	// Serial is the reported serial number (default “FAKE000001”).
	// DualCoPro reports it via LLMAC 07.
	Serial string

	// Credits is the raw duty cycle value reported by OS get credits
	// (units of 0.5%).
	Credits byte

	// OnSend is called for every AppSend request with the BidCoS
	// message (cnt flags cmd src(3) dst(3) payload). It returns the ack
	// status and, for statuses 0x03/0x0c/0x0d, the device’s response
	// message (same format); the module prepends info and rssi. Return
	// NoAck to not acknowledge at all.
	//
	// The default replies with a device ACK (ack status 0x03) if the
	// BiDi flag (0x20) is set, otherwise with ack status 0x01.
	OnSend func(msg []byte) (ackStatus byte, response []byte)

	// OnOS, if non-nil, is called for every request on DstOS before the
	// built-in handling. If it returns handled, the module acks the
	// request with ackStatus (no payload; NoAck swallows it) instead.
	OnOS func(cmd byte, payload []byte) (ackStatus byte, handled bool)

	// DualCoPro emulates an RPI-RF-MOD running the dual-coprocessor
	// firmware with its own protocol (IDENTIFY/CHANGE_APP on COMMON,
	// LLMAC_SEND/ACK/RECV on dst 3; see dual.go). It starts in the
	// bootloader and announces “HMIP_TRX_Bl”, unless StartInApp (no
	// announcement) or AnnounceApp is set. Unlike DualFirmware, which
	// only answers with DUAL_ERR.
	DualCoPro bool

	// AnnounceApp (DualCoPro) starts the module in the application and
	// announces “DualCoPro_App”.
	AnnounceApp bool

	// BannerOnOS (DualCoPro) sends announcements on dst OS ([0x00
	// tag…]) instead of COMMON.
	BannerOnOS bool

	// IgnoreIdentify (DualCoPro) makes the module ignore IDENTIFY.
	IgnoreIdentify bool

	// AppSwitchDelay (DualCoPro) is the delay between CHANGE_APP and
	// the announcement of the new application (default 20ms; the real
	// module takes ~2.2s).
	AppSwitchDelay time.Duration

	// LLMACAckStatus (DualCoPro) returns the LLMAC_ACK status for a
	// transmitted BidCoS message (default 0x01 OK; NoAck: no ack).
	LLMACAckStatus func(msg []byte) byte

	// SGTIN (DualCoPro) is reported via COMMON 04 (default
	// DefaultSGTIN).
	SGTIN []byte

	// DualIgnore (DualCoPro), if non-nil, makes the module ignore the
	// requests for which it returns true (dst and first payload byte).
	DualIgnore func(dst, cmd byte) bool

	// ClockOffset (DualCoPro) is the module clock at start.
	ClockOffset uint16

	// BurstDelay (DualCoPro) delays the LLMAC_ACK of burst
	// transmissions (the real module: ~360ms preamble). Default 0.
	BurstDelay time.Duration

	// TRXStatus (DualCoPro) is the value of TRX status poll replies
	// ([04 01 xx], default 0x02). TRXNoReply ignores the polls.
	TRXStatus  byte
	TRXNoReply bool

	// OnLLMACSend (DualCoPro) emulates the devices: it is called for
	// every successfully transmitted BidCoS message (cnt flags cmd src
	// dst payload) and returns the messages the devices answer with,
	// which are received (LLMAC_RECV) right after the LLMAC_ACK.
	// Default: DefaultOnLLMACSend.
	OnLLMACSend func(msg []byte) (responses [][]byte)
}

// Peer is the module’s state for a peer.
type Peer struct {
	Addr     [3]byte
	KeyIndex byte
	Wakeup   bool
	// AES holds the channels on which AES is enabled. Like the real
	// module (which reports an all-0xff bitmap after adding a peer),
	// a new peer has AES enabled on all 64 channels until it is
	// disabled via the peer remove AES command.
	AES map[byte]bool
}

// AESChannels returns the sorted list of AES-enabled channels below n.
func (p Peer) AESChannels(n int) []byte {
	var res []byte
	for ch := range p.AES {
		if int(ch) < n {
			res = append(res, ch)
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i] < res[j] })
	return res
}

// Module is an emulated HM-MOD-RPI-PCB.
type Module struct {
	opts Options
	conn net.Conn // module side of the pipe

	wmu sync.Mutex // serializes writes

	mu      sync.Mutex
	inApp   bool
	hmid    [3]byte
	hmidSet bool
	keys    map[byte][]byte // slot → 17 byte payload
	peers   map[[3]byte]*Peer
	sent    [][]byte
	frames  []Frame
	timeSet []byte
	readErr error

	llmacOpts  [][]byte
	llmacSent  []LLMACSent
	injected   []Injected
	trxPolls   int
	clockStart time.Time
	changeApps int
	identifies int
	dualErrAll bool
	ts         uint16

	normalMode bool // OS 0x06 received
	csmaCA     bool // last OS 0x0A setting
	csmaCASet  bool // OS 0x0A received
	announced  int

	done chan struct{}
	wg   sync.WaitGroup
}

// New starts an emulated module and returns it together with the host
// side of the connection. Closing either side stops the module.
func New(opts Options) (*Module, io.ReadWriteCloser) {
	if opts.Firmware == ([3]byte{}) {
		opts.Firmware = [3]byte{1, 4, 1}
		if opts.DualCoPro {
			opts.Firmware = [3]byte{4, 4, 22}
		}
	}
	if opts.SGTIN == nil {
		opts.SGTIN = DefaultSGTIN
	}
	if opts.Serial == "" {
		opts.Serial = "FAKE000001"
	}
	host, mod := net.Pipe()
	m := &Module{
		opts:  opts,
		conn:  mod,
		inApp: opts.StartInApp || (opts.DualCoPro && opts.AnnounceApp),
		keys:  make(map[byte][]byte),
		peers: make(map[[3]byte]*Peer),
		done:  make(chan struct{}),

		clockStart: time.Now(),
	}
	m.wg.Add(1)
	go m.serve()
	if !opts.StartInApp && !opts.Mute {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			if opts.DualCoPro {
				if opts.AnnounceApp {
					m.announceDual(TagDualApp)
				} else {
					m.announceDual(TagDualBootloader)
				}
			} else if opts.DualFirmware {
				m.write(Frame{Dst: DstDual, Cmd: 0x00, Payload: []byte("DualCoPro_App")})
			} else {
				// on the wire: FD000C000000436F5F4350555F424C7251
				m.announce("Co_CPU_BL")
			}
		}()
	}
	return m, host
}

// Close stops the module and closes its side of the pipe. It waits
// for the module’s goroutines to exit.
func (m *Module) Close() error {
	err := m.conn.Close()
	m.wg.Wait()
	return err
}

// Done is closed when the module stopped serving (pipe closed).
func (m *Module) Done() <-chan struct{} { return m.done }

func (m *Module) write(f Frame) error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	_, err := m.conn.Write(f.Encode())
	return err
}

// WriteRaw writes raw bytes to the host (e.g. garbage or hand-crafted
// frames).
func (m *Module) WriteRaw(b []byte) error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	_, err := m.conn.Write(b)
	return err
}

func (m *Module) announce(name string) error {
	m.mu.Lock()
	m.announced++
	m.mu.Unlock()
	return m.write(Frame{Dst: DstOS, Cnt: 0, Cmd: 0x00, Payload: []byte(name)})
}

func (m *Module) ack(req Frame, payload ...byte) error {
	return m.write(Frame{Dst: req.Dst, Cnt: req.Cnt, Cmd: 0x04, Payload: payload})
}

func (m *Module) serve() {
	defer m.wg.Done()
	defer close(m.done)
	defer m.conn.Close()
	br := bufio.NewReader(m.conn)
	for {
		f, err := readFrame(br)
		if err != nil {
			if errors.Is(err, errCRC) {
				m.mu.Lock()
				m.readErr = err
				m.mu.Unlock()
				continue
			}
			return
		}
		m.mu.Lock()
		m.frames = append(m.frames, f)
		m.mu.Unlock()
		if m.opts.Mute {
			continue
		}
		if err := m.handle(f); err != nil {
			return
		}
	}
}

func (m *Module) handle(f Frame) error {
	if m.opts.DualCoPro {
		return m.handleDualCoPro(f)
	}
	if m.opts.DualFirmware {
		// The dual firmware does not understand the classic protocol;
		// it answers on its own destinations.
		return m.write(Frame{Dst: DstDualErr, Cnt: f.Cnt, Cmd: 0x04, Payload: []byte{AckNACK}})
	}

	m.mu.Lock()
	inApp := m.inApp
	m.mu.Unlock()

	if f.Dst == DstOS {
		if m.opts.OnOS != nil {
			if status, handled := m.opts.OnOS(f.Cmd, f.Payload); handled {
				if status == NoAck {
					return nil
				}
				return m.ack(f, status)
			}
		}
		switch f.Cmd {
		case 0x00: // get app
			name := "Co_CPU_BL"
			if inApp {
				name = "Co_CPU_App"
			}
			return m.ack(f, append([]byte{AckInfo}, name...)...)
		case 0x02: // get firmware
			fw := m.opts.Firmware
			return m.ack(f, AckInfo, 0x01, 0x00, 0x03, fw[0], fw[1], fw[2])
		case 0x03: // change app
			m.mu.Lock()
			m.inApp = !m.inApp
			inApp = m.inApp
			m.mu.Unlock()
			// on the wire: FD000400000401993D
			if err := m.ack(f, AckOK); err != nil {
				return err
			}
			if inApp {
				// on the wire: FD000D000000436F5F4350555F417070D831
				return m.announce("Co_CPU_App")
			}
			return m.announce("Co_CPU_BL")
		case 0x06: // normal mode (leave update mode)
			if len(f.Payload) != 0 {
				return m.ack(f, AckNACK)
			}
			m.mu.Lock()
			m.normalMode = true
			m.mu.Unlock()
			return m.ack(f, AckOK)
		case 0x08: // get credits
			return m.ack(f, AckInfo, m.opts.Credits)
		case 0x09: // enable credits
			return m.ack(f, AckOK)
		case 0x0a: // enable/disable CSMA/CA
			if len(f.Payload) != 1 || f.Payload[0] > 1 {
				return m.ack(f, AckNACK)
			}
			m.mu.Lock()
			m.csmaCA = f.Payload[0] == 1
			m.csmaCASet = true
			m.mu.Unlock()
			return m.ack(f, AckOK)
		case 0x0b: // get serial
			return m.ack(f, append([]byte{AckInfo}, m.opts.Serial...)...)
		case 0x0e: // set time
			m.mu.Lock()
			m.timeSet = append([]byte(nil), f.Payload...)
			m.mu.Unlock()
			return m.ack(f, AckOK)
		}
		return m.ack(f, AckNACK)
	}

	if f.Dst != DstApp || !inApp {
		return m.ack(f, AckNACK)
	}

	switch f.Cmd {
	case 0x00: // set HMID
		if len(f.Payload) != 3 {
			return m.ack(f, AckNACK)
		}
		m.mu.Lock()
		copy(m.hmid[:], f.Payload)
		m.hmidSet = true
		m.mu.Unlock()
		return m.ack(f, AckOK)

	case SlotCurrent, SlotPrevious, SlotTemp:
		if len(f.Payload) != 17 {
			return m.ack(f, AckNACK)
		}
		m.mu.Lock()
		m.keys[f.Cmd] = append([]byte(nil), f.Payload...)
		m.mu.Unlock()
		return m.ack(f, AckOK)

	case 0x06: // add peer
		if len(f.Payload) < 6 {
			return m.ack(f, AckNACK)
		}
		var addr [3]byte
		copy(addr[:], f.Payload)
		m.mu.Lock()
		p, ok := m.peers[addr]
		if !ok {
			p = &Peer{Addr: addr, AES: make(map[byte]bool)}
			for ch := 0; ch < 64; ch++ {
				p.AES[byte(ch)] = true
			}
			m.peers[addr] = p
		}
		p.KeyIndex = f.Payload[3]
		p.Wakeup = f.Payload[4] != 0
		resp := []byte{AckWithMultipartData, 0x01, 0x01, byte(len(m.peers) >> 8), byte(len(m.peers))}
		resp = append(resp, aesBitmap(p.AES)...)
		m.mu.Unlock()
		// on the wire: FD00100108040701010001FFFFFFFFFFFFFFFFCAAF
		return m.ack(f, resp...)

	case 0x07: // remove peer: addr(3)
		if len(f.Payload) != 3 {
			return m.ack(f, AckNACK)
		}
		var addr [3]byte
		copy(addr[:], f.Payload)
		m.mu.Lock()
		_, ok := m.peers[addr]
		delete(m.peers, addr)
		m.mu.Unlock()
		if !ok {
			return m.ack(f, AckNACK)
		}
		return m.ack(f, AckOK)

	case 0x09, 0x0a: // peer add / remove AES
		if len(f.Payload) < 3 {
			return m.ack(f, AckNACK)
		}
		var addr [3]byte
		copy(addr[:], f.Payload)
		m.mu.Lock()
		p, ok := m.peers[addr]
		if ok {
			for _, ch := range f.Payload[3:] {
				if f.Cmd == 0x09 {
					p.AES[ch] = true
				} else {
					delete(p.AES, ch)
				}
			}
		}
		m.mu.Unlock()
		if !ok {
			return m.ack(f, AckNACK)
		}
		return m.ack(f, AckOK)

	case 0x02: // send
		// status info burst cnt flags cmd src(3) dst(3) payload
		if len(f.Payload) < 3+10 {
			return m.ack(f, AckNACK)
		}
		msg := append([]byte(nil), f.Payload[3:]...)
		m.mu.Lock()
		m.sent = append(m.sent, msg)
		m.mu.Unlock()
		onSend := m.opts.OnSend
		if onSend == nil {
			onSend = DefaultOnSend
		}
		status, resp := onSend(append([]byte(nil), msg...))
		if status == NoAck {
			return nil
		}
		payload := []byte{status}
		if len(resp) > 0 {
			payload = append(payload, 0x00, DefaultRSSI)
			payload = append(payload, resp...)
		}
		return m.ack(f, payload...)
	}
	return m.ack(f, AckNACK)
}

// aesBitmap encodes the AES channel set like the module does: the last
// byte holds channels 0-7 (c.f. HMUARTLGW_UpdatePeerReq, which iterates
// over the reversed bytes).
func aesBitmap(aes map[byte]bool) []byte {
	b := make([]byte, 8)
	for ch := range aes {
		if ch < 64 {
			b[7-ch/8] |= 1 << (ch % 8)
		}
	}
	return b
}

// DefaultOnSend is the default Options.OnSend behavior: a device ACK
// (BidCoS cmd 0x02, payload 0x00, addresses swapped, same message
// counter) if the BiDi flag is set, otherwise ack status 0x01.
func DefaultOnSend(msg []byte) (byte, []byte) {
	if len(msg) < 9 || msg[1]&0x20 == 0 {
		return AckOK, nil
	}
	return AckWithResponse, DeviceAck(msg)
}

// DeviceAck returns the ACK message a device would send in response to
// msg (cnt flags cmd src(3) dst(3) payload).
func DeviceAck(msg []byte) []byte {
	resp := []byte{msg[0], 0x80, 0x02}
	resp = append(resp, msg[6:9]...) // src = original dst
	resp = append(resp, msg[3:6]...) // dst = original src
	return append(resp, 0x00)
}

// InjectRecv emulates the reception of a radio frame. msg is the BidCoS
// message (cnt flags cmd src(3) dst(3) payload), status the receive
// status (0x01/0x11 normal, 0x02/0x12 AES OK, 0x03 AES failed).
func (m *Module) InjectRecv(status byte, msg []byte) error {
	payload := append([]byte{status, 0x00, DefaultRSSI}, msg...)
	return m.write(Frame{Dst: DstApp, Cnt: 0, Cmd: 0x05, Payload: payload})
}

// Reset emulates a module reset (or crash): the module falls back to
// the bootloader and announces “Co_CPU_BL” (DualCoPro: “HMIP_TRX_Bl”),
// losing its configuration.
func (m *Module) Reset() error {
	m.mu.Lock()
	m.inApp = false
	m.hmidSet = false
	m.normalMode = false
	m.csmaCA = false
	m.csmaCASet = false
	m.keys = make(map[byte][]byte)
	m.peers = make(map[[3]byte]*Peer)
	m.mu.Unlock()
	if m.opts.DualCoPro {
		return m.announceDual(TagDualBootloader)
	}
	return m.announce("Co_CPU_BL")
}

// Crash is an alias for Reset.
func (m *Module) Crash() error { return m.Reset() }

// InApp reports whether the application (as opposed to the bootloader)
// is running.
func (m *Module) InApp() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inApp
}

// HMID returns the configured HMID and whether it was set.
func (m *Module) HMID() ([3]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hmid, m.hmidSet
}

// Key returns the 17 byte payload (key + index) last written to slot
// (SlotCurrent, SlotPrevious, SlotTemp), or nil.
func (m *Module) Key(slot byte) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.keys[slot]...)
}

// Peers returns a copy of all peers.
func (m *Module) Peers() map[[3]byte]Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make(map[[3]byte]Peer, len(m.peers))
	for addr, p := range m.peers {
		cp := *p
		cp.AES = make(map[byte]bool, len(p.AES))
		for ch := range p.AES {
			cp.AES[ch] = true
		}
		res[addr] = cp
	}
	return res
}

// Sent returns all BidCoS messages (cnt flags cmd src dst payload) the
// host asked the module to transmit, including retried attempts.
func (m *Module) Sent() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([][]byte, len(m.sent))
	for i, s := range m.sent {
		res[i] = append([]byte(nil), s...)
	}
	return res
}

// Frames returns all frames received from the host.
func (m *Module) Frames() []Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Frame(nil), m.frames...)
}

// TimePayload returns the payload of the last set time request.
func (m *Module) TimePayload() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.timeSet...)
}

// NormalMode reports whether the host switched the module to normal
// mode (OS 0x06) since the last reset.
func (m *Module) NormalMode() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.normalMode
}

// CSMACA returns the CSMA/CA setting last received (OS 0x0A) and
// whether one was received since the last reset.
func (m *Module) CSMACA() (enabled, set bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.csmaCA, m.csmaCASet
}

// Err returns the last frame decoding error (e.g. bad CRC from the
// host), or nil.
func (m *Module) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readErr
}

// String implements fmt.Stringer.
func (m *Module) String() string {
	return fmt.Sprintf("fakemod(app=%v)", m.InApp())
}
