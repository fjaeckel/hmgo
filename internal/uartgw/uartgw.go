// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see LICENSE.

// Package uartgw implements communicating with a HM-MOD-RPI-PCB
// HomeMatic radio module (“UARTGW”) running the classic (BidCoS-only)
// firmware 1.x.
//
// The protocol knowledge in this package comes from FHEM’s
// 00_HMUARTLGW.pm (https://svn.fhem.de/trac/browser/trunk/fhem/FHEM/00_HMUARTLGW.pm)
// and homegear’s Hm-Mod-Rpi-Pcb.cpp
// (https://github.com/Homegear/Homegear-HomeMaticBidCoS/blob/master/src/PhysicalInterfaces/Hm-Mod-Rpi-Pcb.cpp),
// plus captures of the module talking to hmgo. See frame.go for the
// frame format.
//
// A Conn owns a reader goroutine which reads all frames from the
// module. Requests (e.g. Send, AddPeer) are serialized: there is at most
// one outstanding request, whose ack the reader goroutine matches by
// the frame counter. Received radio frames are delivered on Recv.
package uartgw

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/fjaeckel/hmgo/internal/hmkey"
)

// Destinations (first byte of each packet).
// c.f. HMUARTLGW_DST_* in FHEM
const (
	dstOS      = 0x00 // bootloader (also used for “OS” commands while the app runs)
	dstApp     = 0x01 // BidCoS application
	dstHMIP    = 0x02 // dual-coprocessor firmware: Homematic IP (see dual.go)
	dstLLMAC   = 0x03 // dual-coprocessor firmware: BidCoS MAC (see dual.go)
	dstDual    = 0xfe // dual-coprocessor firmware “COMMON” (identify, change app)
	dstDualErr = 0xff // dual-coprocessor firmware error
)

func dstName(dst byte) string {
	switch dst {
	case dstOS:
		return "OS"
	case dstApp:
		return "App"
	case dstHMIP:
		return "HMIP"
	case dstLLMAC:
		return "LLMAC"
	case dstDual:
		return "Dual"
	case dstDualErr:
		return "DualErr"
	default:
		return fmt.Sprintf("<invalid dest %d>", dst)
	}
}

// OS commands (destination dstOS).
//
// NOTE(stapelberg): I’m not convinced the mental model of these
// constants is entirely correct. For example, we send osGetSerial while
// the device is in the App state, which doesn’t make sense to me. FHEM
// does the same though: all of HMUARTLGW_OS_{SET_TIME,GET_FIRMWARE,
// NORMAL_MODE,GET_SERIAL,ENABLE_CSMACA,ENABLE_CREDITS,GET_CREDITS} are sent with
// HMUARTLGW_DST_OS while the application is running, and the module
// acks them on dstOS.
const (
	osGetApp        = 0x00 // also: unsolicited announcement of the running app
	osGetFirmware   = 0x02
	osChangeApp     = 0x03
	osAck           = 0x04
	osCredits       = 0x05 // unsolicited credits report (module → host)
	osNormalMode    = 0x06
	osUpdateMode    = 0x07
	osGetCredits    = 0x08
	osEnableCredits = 0x09
	osEnableCSMACA  = 0x0a
	osGetSerial     = 0x0b
	osSetTime       = 0x0e
)

// App commands (destination dstApp).
const (
	appSetHMID        = 0x00
	appGetHMID        = 0x01
	appSend           = 0x02
	appSetCurrentKey  = 0x03 // key(16) + index, 17 zero bytes clear the slot
	appAck            = 0x04
	appRecv           = 0x05
	appAddPeer        = 0x06
	appRemovePeer     = 0x07
	appGetPeers       = 0x08
	appPeerAddAES     = 0x09
	appPeerRemoveAES  = 0x0a
	appSetTempKey     = 0x0b
	appSetPreviousKey = 0x0f
	appDefaultHMID    = 0x10
)

// Ack status values (first payload byte of an ack frame).
// c.f. HMUARTLGW_ACK_* in FHEM
const (
	ackNACK                = 0x00
	ackOK                  = 0x01
	ackInfo                = 0x02
	ackWithResponse        = 0x03
	ackEUnknown            = 0x04 // typically: the device did not answer
	ackENoCredits          = 0x05 // duty cycle exhausted
	ackECSMACA             = 0x06 // channel busy, retry
	ackWithMultipartData   = 0x07 // 04 07 XX YY: part XX of YY
	ackEInProgress         = 0x08 // module busy, retry
	ackWithResponseAESOK   = 0x0c
	ackWithResponseAESFail = 0x0d
)

func ackName(status byte) string {
	switch status {
	case ackNACK:
		return "nack"
	case ackOK:
		return "ack"
	case ackInfo:
		return "ack_info"
	case ackWithResponse:
		return "ack_with_response"
	case ackEUnknown:
		return "e_unknown"
	case ackENoCredits:
		return "e_no_credits"
	case ackECSMACA:
		return "e_csma_ca"
	case ackWithMultipartData:
		return "ack_multipart"
	case ackEInProgress:
		return "e_in_progress"
	case ackWithResponseAESOK:
		return "ack_with_response_aes_ok"
	case ackWithResponseAESFail:
		return "ack_with_response_aes_ko"
	default:
		return fmt.Sprintf("0x%02x", status)
	}
}

// Receive status values, as used in AppRecv frames and understood by
// bidcos.Decode. c.f. HMUARTLGW_RECV_* in FHEM
const (
	recvResp          = 0x01
	recvRespWithAESOK = 0x02
	recvRespWithAESKO = 0x03
)

// Application names announced by the module.
const (
	appNameBootloader = "Co_CPU_BL"
	appNameApp        = "Co_CPU_App"
	appNameDual       = "DualCoPro"
)

var (
	// ErrDualCoProFirmware is returned by Open when the module runs the
	// dual-coprocessor firmware used by Homematic IP capable CCUs.
	ErrDualCoProFirmware = errors.New("uartgw: the HM-MOD-RPI-PCB runs the Homematic IP “DualCoPro” (dual-coprocessor) firmware, " +
		"which hmbridge does not support; hmbridge needs the classic HM-MOD-RPI-PCB firmware 1.4.x " +
		"(e.g. flash it with FHEM’s “set <hmuartlgw> updateCoPro” or eQ-3’s flash tool, " +
		"or switch the CCU/OpenCCU to classic BidCoS-RF mode)")

	// ErrNoResponse means the module transmitted the packet but the
	// device did not answer (ack 0x04).
	ErrNoResponse = errors.New("uartgw: device did not respond")

	// ErrNoCredits means the module refused to transmit because the
	// duty cycle (1% airtime) is exhausted (ack 0x05).
	ErrNoCredits = errors.New("uartgw: duty cycle exhausted (no credits)")

	// ErrAESFailed means the device answered, but failed the AES
	// challenge (ack 0x0d). The response must not be trusted.
	ErrAESFailed = errors.New("uartgw: AES authentication of the device response failed")

	// ErrNACK means the module rejected the request (ack 0x00).
	ErrNACK = errors.New("uartgw: request rejected by module (NACK)")

	// ErrBusy means the module kept reporting CSMA/CA or “in
	// progress” until the retry window elapsed.
	ErrBusy = errors.New("uartgw: radio busy")

	// ErrTimeout means the module did not acknowledge a request.
	ErrTimeout = errors.New("uartgw: radio module did not acknowledge request")

	// ErrModuleReset means the module announced its bootloader or
	// application while running, i.e. it crashed or was reset. All
	// state (keys, HMID, peers) is lost; the Conn is unusable.
	ErrModuleReset = errors.New("uartgw: radio module was reset unexpectedly")

	// ErrModuleUnresponsive means the module stopped acknowledging
	// requests (Config.MaxAckTimeouts consecutive requests timed out)
	// without announcing a reset, i.e. it is wedged. The Conn is
	// unusable; the module needs to be re-initialized (reopened).
	// c.f. HMUARTLGW_CheckCmdResp in FHEM: “did not respond after all,
	// reopening”.
	ErrModuleUnresponsive = errors.New("uartgw: radio module stopped responding")

	// ErrFirmwareTooOld is returned (wrapped) by Open when the module
	// runs a firmware version older than 1.0.7, which does not
	// understand the burst byte in AppSend requests.
	ErrFirmwareTooOld = errors.New("uartgw: radio module firmware is too old")

	// ErrClosed is returned after Close was called.
	ErrClosed = errors.New("uartgw: connection closed")
)

// Default timeouts. c.f. HMUARTLGW_CMD_TIMEOUT, HMUARTLGW_SEND_TIMEOUT,
// HMUARTLGW_SEND_RETRY_SECONDS and HMUARTLGW_CSMACA_RETRY_MS in FHEM.
const (
	defaultTimeout         = 3 * time.Second
	defaultSendTimeout     = 10 * time.Second
	defaultAnnounceWait    = 1 * time.Second
	defaultRetryWindow     = 3 * time.Second
	defaultRetryBackoff    = 200 * time.Millisecond
	defaultCreditsInterval = 60 * time.Second
	defaultMaxAckTimeouts  = 3
	recvBufferSize         = 64
)

// Config configures the module during Open.
type Config struct {
	HMID     [3]byte          // the central address to impersonate
	Current  *hmkey.Key       // nil: clear slot (no AES)
	Previous *hmkey.Key       // nil: clear slot
	Now      func() time.Time // nil: time.Now

	// Timeout bounds each request/response step with the module
	// (default 3s). Radio transmissions which expect a device
	// response (BiDi) use max(Timeout, 10s).
	Timeout time.Duration

	// AnnounceWait is how long Open waits for the unsolicited
	// announcement a freshly reset module sends (default 1s). If none
	// arrives (no reset GPIO, module already running), Open actively
	// queries the running application.
	AnnounceWait time.Duration

	// CreditsInterval is how often the duty cycle credits are polled
	// for the duty cycle metric (default 60s, negative disables).
	CreditsInterval time.Duration

	// MaxAckTimeouts is the number of consecutive requests the module
	// may leave unacknowledged (ErrTimeout, including credits polling)
	// before the connection fails with ErrModuleUnresponsive (default 3,
	// like FHEM’s CreditFailed < 3; negative disables). Any matched ack
	// resets the count. A device not answering (ErrNoResponse) and
	// requests aborted via their context do not count.
	MaxAckTimeouts int

	// CSMACA enables CSMA/CA (carrier sense multiple access with
	// collision avoidance, “listen before talk”) in the module
	// (HMUARTLGW_OS_ENABLE_CSMACA). Default false, like FHEM's csmaCa
	// attribute (default 0) and Homegear, which both send 0A 00. With
	// CSMA/CA enabled, transmissions on a busy channel are acked with
	// status 0x06 and retried.
	CSMACA bool

	// SkipSetTime leaves the module's clock alone during Open. The
	// caller sets it later with SetTime, e.g. once the system clock is
	// NTP-synchronised (a Raspberry Pi has no RTC; after a power cut its
	// clock may be wrong until NTP is reachable).
	SkipSetTime bool

	// Debug enables logging of every frame (key material is redacted).
	Debug bool

	// The following fields only apply to the RPI-RF-MOD (DualConn, see
	// dual.go); Open and the classic module ignore them.

	// SoftwareAck makes hmbridge acknowledge BiDi messages of registered
	// peers addressed to HMID itself (nil: true). The dual-coprocessor
	// firmware leaves this to the host (OpenCCU's rfd ACKs in software,
	// confirmed by a capture). Time requests are answered instead.
	SoftwareAck *bool

	// SoftwareAES makes hmbridge challenge BiDi messages of peers with
	// AES channels and verify the answers with Signer (nil: true).
	SoftwareAES *bool

	// Signer computes and verifies AES answers (software AES). nil: AES
	// is not available; AES exchanges fail (logged once).
	Signer AESSigner

	// ResponseTimeout is how long Send waits for a device's response
	// (RPI-RF-MOD): counted from the send request, but at least 75ms
	// after the module confirmed the transmission (LLMAC_ACK); for burst
	// packets, whose LLMAC_ACK comes after the ~360ms preamble, counted
	// from the LLMAC_ACK. Default 275ms, like OpenCCU. A received message is held at most
	// max(1s, 100ms + 2*ResponseTimeout) for the answer to our AES
	// challenge.
	ResponseTimeout time.Duration

	// SendRetries is how often an unanswered BiDi packet is retransmitted
	// with the same message counter (default 3, like OpenCCU; negative:
	// none).
	SendRetries int

	// TRXPollInterval is how often the module's TRX status is polled
	// (default 10s like multimacd; negative disables). It is the
	// liveness check (unanswered polls count towards MaxAckTimeouts) and
	// feeds the hmbridge_uartgw_dual_trx_status gauge.
	TRXPollInterval time.Duration

	// ChangeAppWait is how long to wait for the application announcement
	// after switching from the bootloader to the application (default
	// 3.5s; RFNETHM observed ~2.2s).
	ChangeAppWait time.Duration

	// DualInit is sent to the module after it entered the application.
	// Empty by default and not needed for receiving and sending: a
	// capture of OpenCCU shows no address or peer configuration, and the
	// module receives all traffic. A hook for anything multimacd sends
	// at its start (not captured yet, see docs/SNIFFING.md).
	DualInit []InitStep

	// DualPeerInit returns steps registering a peer with the module
	// (sent by AddPeer). nil: none (not needed, see DualInit).
	DualPeerInit func(p Peer) []InitStep

	// DualFirmwareVersion overrides the firmware version the RPI-RF-MOD
	// reports (FirmwareVersion).
	DualFirmwareVersion string

	// Rand is the source of AES challenges (nil: crypto/rand).
	Rand io.Reader

	// HMIPHandler, if set, receives every frame the RPI-RF-MOD sends on
	// its Homematic IP destinations (0x02, 0x0c): destination, counter
	// and packet (command byte, data…; not reused). It runs on its own
	// goroutine, in order; while it lags behind by more than 64 frames,
	// further ones are dropped (hmip_dropped), so it never delays the
	// BidCoS traffic. Receive only: nothing is ever sent on these
	// destinations.
	HMIPHandler func(dst, cnt byte, packet []byte)
}

func csmaCAByte(enabled bool) byte {
	if enabled {
		return 0x01
	}
	return 0x00
}

// Peer describes a BidCoS device the module should talk to on our
// behalf (AES challenges, wakeup).
type Peer struct {
	Addr        [3]byte
	KeyIndex    byte   // key index the device uses (0 = eQ-3 default)
	Channels    int    // number of channels (AES is disabled on all others)
	AESChannels []byte // channels on which the module enforces AES
	Wakeup      bool
}

// pending is the outstanding request, whose ack the reader goroutine
// delivers on ack.
type pending struct {
	cnt  byte
	dst  byte
	cmd  byte
	sent time.Time
	ack  chan frame // buffered (1)
}

// Conn is an initialized connection to a HM-MOD-RPI-PCB.
type Conn struct {
	rw io.ReadWriter
	fr *frameReader

	// next, if non-nil, replaces fr as the source of frames (OpenModule:
	// the frames are read by the framePump which already served the
	// detection). replay holds frames received during detection, which
	// are dispatched before reading further frames.
	next   func() (frame, error)
	replay []frame

	timeout         time.Duration
	sendTimeout     time.Duration
	announceWait    time.Duration
	retryWindow     time.Duration
	retryBackoff    time.Duration
	creditsInterval time.Duration
	maxAckTimeouts  int // <= 0: disabled
	debug           bool

	firmware string
	serial   string

	// sem serializes requests (one outstanding request at a time). It
	// is a channel instead of a mutex so that waiting for it can be
	// aborted via context.
	sem chan struct{}

	// wmu serializes writes to rw.
	wmu sync.Mutex

	// announce receives application names announced by the module
	// (unsolicited osGetApp frames) during initialization.
	announce chan string

	done      chan struct{} // closed when the Conn fails or is closed
	closeOnce sync.Once

	mu          sync.Mutex
	cnt         byte     // last used frame counter
	cur         *pending // outstanding request or nil
	entered     bool     // application entered: announcements now mean a reset
	running     bool     // initialization completed
	err         error    // first fatal error
	recv        chan []byte
	lastSendLen [2]int // lengths of the two most recently sent packets
	ackTimeouts int    // consecutive requests the module did not ack
}

// Open performs the init handshake on the module and starts the reader
// goroutine. rw is typically the serial port (115200 8N1).
//
// Ideally, the module was just reset (via GPIO), in which case it
// announces its bootloader (“Co_CPU_BL”). Otherwise (no announcement
// within Config.AnnounceWait), Open queries the running application
// like FHEM does, and if the application is already running, reboots
// the module into the bootloader first so that we start from a clean
// state (no stale peers or keys).
//
// After entering the application, Open (in FHEM's order) reads the
// firmware version, switches the module to normal mode (OS 0x06),
// reads the serial number, configures CSMA/CA (Config.CSMACA), sets
// the time, the AES keys and the HMID, then enables duty cycle
// credits.
//
// If Open fails and rw implements io.Closer, rw is closed (so that the
// reader goroutine terminates).
func Open(rw io.ReadWriter, cfg Config) (*Conn, error) {
	return openConn(rw, cfg, legacyOpts{})
}

// legacyOpts are used by OpenModule to hand a detected HM-MOD-RPI-PCB
// over to the classic implementation.
type legacyOpts struct {
	pump             *framePump // nil: Conn reads rw itself
	replay           []frame    // frames received during detection
	skipAnnounceWait bool       // detection already waited for the announcement in vain
}

func openConn(rw io.ReadWriter, cfg Config, opts legacyOpts) (*Conn, error) {
	c := &Conn{
		rw:              rw,
		fr:              newFrameReader(rw),
		timeout:         cfg.Timeout,
		sendTimeout:     defaultSendTimeout,
		announceWait:    cfg.AnnounceWait,
		retryWindow:     defaultRetryWindow,
		retryBackoff:    defaultRetryBackoff,
		creditsInterval: cfg.CreditsInterval,
		maxAckTimeouts:  cfg.MaxAckTimeouts,
		debug:           cfg.Debug,
		sem:             make(chan struct{}, 1),
		announce:        make(chan string, 8),
		done:            make(chan struct{}),
		recv:            make(chan []byte, recvBufferSize),
	}
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	if c.sendTimeout < c.timeout {
		c.sendTimeout = c.timeout
	}
	if c.announceWait <= 0 {
		c.announceWait = defaultAnnounceWait
	}
	if c.creditsInterval == 0 {
		c.creditsInterval = defaultCreditsInterval
	}
	if c.maxAckTimeouts == 0 {
		c.maxAckTimeouts = defaultMaxAckTimeouts
	}
	if opts.pump != nil {
		c.fr = nil
		c.next = opts.pump.next
		c.replay = opts.replay
		opts.pump.setAltInits(c.altCRCInits)
	} else {
		c.fr.altInits = c.altCRCInits
	}
	if opts.skipAnnounceWait {
		c.announceWait = time.Millisecond
	}

	go c.readLoop()

	if err := c.init(cfg); err != nil {
		c.fail(err)
		c.closeRW()
		upGauge.Set(0)
		// Prefer the error which made the connection fail (e.g.
		// ErrDualCoProFirmware detected by the reader goroutine).
		if cerr := c.Err(); cerr != nil && cerr != err && !errors.Is(err, cerr) {
			return nil, fmt.Errorf("%w (while: %v)", cerr, err)
		}
		return nil, err
	}

	c.mu.Lock()
	if err := c.err; err != nil {
		// The module failed between the last init step and now.
		c.mu.Unlock()
		c.closeRW()
		return nil, err
	}
	c.running = true
	upGauge.Set(1)
	c.mu.Unlock()

	infoGauge.Reset()
	infoGauge.WithLabelValues(c.firmware, c.serial).Set(1)

	if c.creditsInterval > 0 {
		go c.creditsLoop()
	}
	return c, nil
}

// FirmwareVersion returns the module’s firmware version, e.g. “1.4.1”.
func (c *Conn) FirmwareVersion() string { return c.firmware }

// SerialNumber returns the module’s serial number, e.g. “NEQ1330980”.
func (c *Conn) SerialNumber() string { return c.serial }

// Kind returns KindHMModRPIPCB.
func (c *Conn) Kind() string { return KindHMModRPIPCB }

// Recv delivers received radio frames (AppRecv payloads: status, info,
// rssi, msg…; decodable with bidcos.Decode). The channel is buffered;
// frames are dropped (and logged) if the consumer does not keep up.
// It is closed when the connection fails or is closed; see Err.
func (c *Conn) Recv() <-chan []byte { return c.recv }

// Err returns the error which made the connection fail, ErrClosed after
// Close, or nil while the connection is healthy.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close stops the connection. If the underlying io.ReadWriter
// implements io.Closer, it is closed, which also terminates the reader
// goroutine. Otherwise, closing it is the caller’s responsibility.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.fail(ErrClosed)
		err = c.closeRW()
		upGauge.Set(0)
	})
	return err
}

func (c *Conn) closeRW() error {
	if cl, ok := c.rw.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}

// fail marks the connection as failed (first error wins), wakes up all
// waiters and closes the Recv channel.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	c.cur = nil
	running := c.running
	// Update the gauge before waking up anybody, so that observers
	// of Recv/Err see a consistent state.
	upGauge.Set(0)
	close(c.done)
	close(c.recv)
	c.mu.Unlock()

	if running && err != ErrClosed {
		log.Printf("uartgw: connection failed: %v", err)
	}
}

// altCRCInits works around a firmware bug: “When writing to the device
// while it prepares to write a frame to the host, the device seems to
// initialize the crc with 0x827f or 0x8281 plus the length of the frame
// being received” (FHEM, HMUARTLGW_Read). Like FHEM, we only accept
// such frames during initialization.
func (c *Conn) altCRCInits() []uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return nil
	}
	var inits []uint16
	for _, slen := range []int{c.lastSendLen[1], c.lastSendLen[0]} {
		if slen == 0 {
			continue
		}
		inits = append(inits, uint16(0x827f+slen), uint16(0x8281+slen))
	}
	return inits
}

// logFrame logs f if debugging is enabled, never logging key material.
func (c *Conn) logFrame(dir string, f frame) {
	if !c.debug {
		return
	}
	if dir == "send" && f.dst == dstApp &&
		(f.cmd == appSetCurrentKey || f.cmd == appSetPreviousKey || f.cmd == appSetTempKey) {
		// c.f. HMUARTLGW_send: “Key, do not log”
		idx := -1
		if len(f.payload) == 17 {
			idx = int(f.payload[16])
		}
		log.Printf("uartgw: %s dst=%s cnt=%d cmd=%02x payload=<key redacted, index %d>", dir, dstName(f.dst), f.cnt, f.cmd, idx)
		return
	}
	log.Printf("uartgw: %s %v", dir, f)
}

// readLoop reads and dispatches all frames until a read error occurs.
func (c *Conn) readLoop() {
	for _, f := range c.replay {
		c.logFrame("recv", f)
		c.dispatch(f)
	}
	c.replay = nil
	for {
		var f frame
		var err error
		if c.next != nil {
			f, err = c.next()
		} else {
			f, err = c.fr.next()
		}
		if err != nil {
			if errors.Is(err, errBadFrame) {
				frameErrors.Inc()
				log.Printf("uartgw: dropping frame: %v", err)
				continue
			}
			if c.Err() == nil {
				c.fail(fmt.Errorf("uartgw: reading from radio module: %w", err))
			}
			return
		}
		if c.fr != nil && c.fr.skipped > 0 {
			log.Printf("uartgw: skipped %d bytes of garbage before frame", c.fr.skipped)
			c.fr.skipped = 0
		}
		framesReceived.WithLabelValues(dstLabel(f.dst), cmdLabel(f.cmd)).Inc()
		c.logFrame("recv", f)
		c.dispatch(f)
	}
}

func (c *Conn) dispatch(f frame) {
	switch {
	case f.dst == dstDual || f.dst == dstDualErr:
		// The classic firmware never uses these destinations; the
		// dual-coprocessor firmware announces itself on dstDual and
		// answers our OS commands on dstDualErr (c.f. HMUARTLGW_Parse:
		// “Minimally handle DualCopro-Firmware”).
		c.fail(ErrDualCoProFirmware)

	case f.cmd == osAck && (f.dst == dstOS || f.dst == dstApp) && len(f.payload) == 0:
		log.Printf("uartgw: ignoring ack without status: %v", f)

	case f.cmd == osAck && (f.dst == dstOS || f.dst == dstApp):
		c.mu.Lock()
		p := c.cur
		if p != nil && p.cnt == f.cnt {
			c.cur = nil
			c.ackTimeouts = 0 // the module is alive
		} else {
			p = nil
		}
		cnt := c.cnt
		c.mu.Unlock()
		if p == nil {
			// Delayed ack for a request we gave up on, or: “Firmware
			// sometimes send additional ACK when receiving the next
			// frame from a device after a command, even if it has
			// already ACKed the command.” (FHEM)
			staleAcks.Inc()
			if c.debug {
				log.Printf("uartgw: dropping ack with invalid/old counter (ours: %d): %v", cnt, f)
			}
			return
		}
		requestDuration.Observe(time.Since(p.sent).Seconds())
		p.ack <- f // buffered, only ever written once

	case f.dst == dstOS && f.cmd == osGetApp:
		name := string(f.payload)
		if strings.Contains(name, appNameDual) {
			c.fail(ErrDualCoProFirmware)
			return
		}
		c.mu.Lock()
		entered := c.entered
		c.mu.Unlock()
		if entered {
			// c.f. HMUARTLGW_Parse: “unexpected info about ${running}
			// received (module crashed?), reopening”
			c.fail(fmt.Errorf("%w (announced %q)", ErrModuleReset, name))
			return
		}
		select {
		case c.announce <- name:
		default:
			log.Printf("uartgw: dropping announcement %q", name)
		}

	case f.dst == dstOS && f.cmd == osCredits && len(f.payload) == 1:
		c.setCredits(f.payload)

	case f.dst == dstApp && f.cmd == appRecv:
		c.mu.Lock()
		if c.err == nil {
			select {
			case c.recv <- f.payload:
			default:
				recvDropped.Inc()
				log.Printf("uartgw: consumer too slow, dropping received frame %x", f.payload)
			}
		}
		c.mu.Unlock()

	default:
		log.Printf("uartgw: ignoring unexpected frame: %v", f)
	}
}

func (c *Conn) setCredits(b []byte) {
	if len(b) == 0 {
		return
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	// FHEM: msgLoadCurrent = int((load + 1) / 2) percent
	dutyCycle.Set(float64(v) / 2)
}

// acquire obtains the request semaphore.
func (c *Conn) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

func (c *Conn) release() { <-c.sem }

// start writes a request frame and registers it as the outstanding
// request. The caller must hold the request semaphore.
func (c *Conn) start(dst, cmd byte, payload []byte) (*pending, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	// c.f. HMUARTLGW_send: the counter is incremented before sending.
	c.cnt++
	p := &pending{
		cnt:  c.cnt,
		dst:  dst,
		cmd:  cmd,
		sent: time.Now(),
		ack:  make(chan frame, 1),
	}
	c.cur = p
	c.lastSendLen[0], c.lastSendLen[1] = c.lastSendLen[1], 3+len(payload)
	c.mu.Unlock()

	f := frame{dst: dst, cnt: p.cnt, cmd: cmd, payload: payload}
	c.logFrame("send", f)
	c.wmu.Lock()
	_, err := c.rw.Write(f.encode())
	c.wmu.Unlock()
	if err != nil {
		err = fmt.Errorf("uartgw: writing to radio module: %w", err)
		c.fail(err)
		return nil, err
	}
	framesSent.WithLabelValues(dstLabel(dst), cmdLabel(cmd)).Inc()
	return p, nil
}

// abandon unregisters p if it is still the outstanding request, so that
// its ack (if it ever arrives) is dropped as stale.
func (c *Conn) abandon(p *pending) {
	c.mu.Lock()
	if c.cur == p {
		c.cur = nil
	}
	c.mu.Unlock()
}

// wait waits for the ack of p.
func (c *Conn) wait(ctx context.Context, p *pending, timeout time.Duration) (frame, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case f := <-p.ack:
		return f, nil
	case <-t.C:
		c.abandon(p)
		return frame{}, fmt.Errorf("%w (dst %s, cmd %02x, cnt %d, after %v)", ErrTimeout, dstName(p.dst), p.cmd, p.cnt, timeout)
	case <-ctx.Done():
		c.abandon(p)
		return frame{}, ctx.Err()
	case <-c.done:
		return frame{}, c.Err()
	}
}

// do sends a request and returns its ack, retrying (with a new frame
// counter, like FHEM) while the module reports CSMA/CA failure or “in
// progress”. The caller must hold the request semaphore.
func (c *Conn) do(ctx context.Context, dst, cmd byte, payload []byte, timeout time.Duration) (frame, error) {
	var retryStart time.Time
	for {
		p, err := c.start(dst, cmd, payload)
		if err != nil {
			return frame{}, err
		}
		f, err := c.wait(ctx, p, timeout)
		if err != nil {
			if errors.Is(err, ErrTimeout) {
				c.ackTimeout(err)
			}
			return frame{}, err
		}
		status := f.payload[0]
		acks.WithLabelValues(ackName(status)).Inc()
		var reason string
		switch status {
		case ackECSMACA:
			reason = "csma_ca"
		case ackEInProgress:
			reason = "in_progress"
		default:
			return f, nil
		}
		if retryStart.IsZero() {
			retryStart = time.Now()
		}
		if time.Since(retryStart) >= c.retryWindow {
			return frame{}, fmt.Errorf("%w (%s for %v)", ErrBusy, reason, c.retryWindow)
		}
		sendRetries.WithLabelValues(reason).Inc()
		if c.debug {
			log.Printf("uartgw: module reports %s, retrying in %v", reason, c.retryBackoff)
		}
		t := time.NewTimer(c.retryBackoff)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return frame{}, ctx.Err()
		case <-c.done:
			t.Stop()
			return frame{}, c.Err()
		}
	}
}

// ackTimeout records that the module did not acknowledge a request
// (err) and fails the connection with ErrModuleUnresponsive after
// maxAckTimeouts consecutive ones.
func (c *Conn) ackTimeout(err error) {
	ackTimeouts.Inc()
	c.mu.Lock()
	c.ackTimeouts++
	n := c.ackTimeouts
	c.mu.Unlock()
	if c.maxAckTimeouts > 0 && n >= c.maxAckTimeouts {
		c.fail(fmt.Errorf("%w: %d consecutive requests were not acknowledged (last: %v)", ErrModuleUnresponsive, n, err))
	}
}

// simple performs a request whose ack carries no information beyond
// success/failure.
func (c *Conn) simple(ctx context.Context, dst, cmd byte, payload []byte) (frame, error) {
	f, err := c.do(ctx, dst, cmd, payload, c.timeout)
	if err != nil {
		return f, err
	}
	if f.payload[0] == ackNACK {
		return f, ErrNACK
	}
	return f, nil
}

func (c *Conn) stepCtx() (context.Context, context.CancelFunc) {
	// Each step has its own timeout via wait; this context only exists
	// so that the retry logic has something to select on.
	return context.WithTimeout(context.Background(), c.timeout+c.retryWindow+time.Second)
}

func (c *Conn) init(cfg Config) error {
	if err := c.enterApp(); err != nil {
		return err
	}
	c.mu.Lock()
	c.entered = true
	c.mu.Unlock()

	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}

	steps := []struct {
		name string
		fn   func(ctx context.Context) error
	}{
		{"getting firmware version", c.getFirmwareVersion},
		{"setting normal mode", func(ctx context.Context) error {
			// FHEM (HMUARTLGW_STATE_SET_NORMAL_MODE) and Homegear
			// (“Disable update mode”) leave update mode right after
			// reading the firmware version.
			_, err := c.simple(ctx, dstOS, osNormalMode, nil)
			return err
		}},
		{"getting serial number", c.getSerialNumber},
		{"configuring CSMA/CA", func(ctx context.Context) error {
			// on the wire: FD000400020A003D10 (disabled; counter 2 in old hmgo)
			// ack:         FD0004000204011916
			_, err := c.simple(ctx, dstOS, osEnableCSMACA, []byte{csmaCAByte(cfg.CSMACA)})
			return err
		}},
		{"setting time", func(ctx context.Context) error {
			if cfg.SkipSetTime {
				return nil
			}
			return c.setTime(ctx, now())
		}},
		{"setting current key", func(ctx context.Context) error {
			// on the wire: FD001401050300112233445566778899AABBCCDDEEFF024C6D
			// ack:         FD0004010504010D7A
			_, err := c.simple(ctx, dstApp, appSetCurrentKey, keyPayload(cfg.Current))
			return err
		}},
		{"setting previous key", func(ctx context.Context) error {
			_, err := c.simple(ctx, dstApp, appSetPreviousKey, keyPayload(cfg.Previous))
			return err
		}},
		{"clearing temp key", func(ctx context.Context) error {
			_, err := c.simple(ctx, dstApp, appSetTempKey, keyPayload(nil))
			return err
		}},
		{"setting HMID", func(ctx context.Context) error {
			// on the wire: FD0006010600FC7DB02CD166
			// ack:         FD0004010604010D46
			_, err := c.simple(ctx, dstApp, appSetHMID, cfg.HMID[:])
			return err
		}},
	}
	for _, step := range steps {
		ctx, cancel := c.stepCtx()
		err := func() error {
			if err := c.acquire(ctx); err != nil {
				return err
			}
			defer c.release()
			return step.fn(ctx)
		}()
		cancel()
		if err != nil {
			return fmt.Errorf("uartgw: %s: %w", step.name, err)
		}
	}

	// Duty cycle: FHEM enables credits (HMUARTLGW_OS_ENABLE_CREDITS,
	// attribute dutyCycle, default 1) and queries them periodically.
	// Failures are not fatal.
	ctx, cancel := c.stepCtx()
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	if _, err := c.simple(ctx, dstOS, osEnableCredits, []byte{0x01}); err != nil {
		if c.Err() != nil {
			return err
		}
		log.Printf("uartgw: enabling duty cycle credits: %v", err)
	}
	if err := c.getCredits(ctx); err != nil {
		if c.Err() != nil {
			return err
		}
		log.Printf("uartgw: getting duty cycle credits: %v", err)
	}
	return nil
}

// enterApp brings the module from whatever state it is in into the
// application (“Co_CPU_App”), just after it (re)started.
//
// c.f. HMUARTLGW_StartInit and HMUARTLGW_Parse (STATE_QUERY_APP,
// STATE_ENTER_APP) in FHEM
func (c *Conn) enterApp() error {
	var running string
	unsolicited := false

	// A freshly reset module announces its bootloader on its own:
	// on the wire: FD000C000000436F5F4350555F424C7251 (“Co_CPU_BL”)
	t := time.NewTimer(c.announceWait)
	select {
	case running = <-c.announce:
		unsolicited = true
	case <-t.C:
	case <-c.done:
		t.Stop()
		return c.Err()
	}
	t.Stop()

	if !unsolicited {
		// No reset (e.g. no GPIO access, or the module runs on
		// OpenCCU’s raw-uart): ask which application runs. The module
		// answers with ack 0x02 (ACK_INFO) + application name.
		name, err := c.queryApp()
		if err != nil {
			return fmt.Errorf("uartgw: querying running application: %w", err)
		}
		running = name
	}
	if strings.Contains(running, appNameDual) {
		return ErrDualCoProFirmware
	}

	switch running {
	case appNameBootloader:
		// fallthrough to switching into the application below

	case appNameApp:
		if unsolicited {
			// The application just started (e.g. the bootloader timed
			// out on its own), i.e. it is in a pristine state. FHEM
			// continues with the parameters in this case, too.
			return nil
		}
		// The application has been running for an unknown time and
		// may have peers and keys configured. Like FHEM, reboot into
		// the bootloader (“Reset module”) to start from a clean
		// state.
		log.Printf("uartgw: module is already running %s, rebooting it into the bootloader", appNameApp)
		if err := c.changeApp(appNameBootloader); err != nil {
			return fmt.Errorf("uartgw: rebooting into the bootloader: %w", err)
		}

	default:
		return fmt.Errorf("uartgw: radio module runs unknown application %q", running)
	}

	if err := c.changeApp(appNameApp); err != nil {
		return fmt.Errorf("uartgw: switching from bootloader to application: %w", err)
	}
	return nil
}

func (c *Conn) queryApp() (string, error) {
	var lastErr error
	// FHEM re-sends unanswered init commands HMUARTLGW_CMD_RETRY_CNT
	// (3) times.
	for attempt := 0; attempt < 3; attempt++ {
		name, err := func() (string, error) {
			ctx, cancel := c.stepCtx()
			defer cancel()
			if err := c.acquire(ctx); err != nil {
				return "", err
			}
			defer c.release()
			p, err := c.start(dstOS, osGetApp, nil)
			if err != nil {
				return "", err
			}
			// The module might also send an unsolicited announcement
			// (e.g. it was just reset after all), so wait for both.
			t := time.NewTimer(c.timeout)
			defer t.Stop()
			select {
			case f := <-p.ack:
				if f.payload[0] != ackInfo || len(f.payload) < 2 {
					return "", fmt.Errorf("unexpected ack %s", ackName(f.payload[0]))
				}
				return string(f.payload[1:]), nil
			case name := <-c.announce:
				c.abandon(p)
				return name, nil
			case <-t.C:
				c.abandon(p)
				return "", ErrTimeout
			case <-c.done:
				return "", c.Err()
			}
		}()
		if err == nil {
			return name, nil
		}
		if c.Err() != nil {
			return "", c.Err()
		}
		lastErr = err
		if !errors.Is(err, ErrTimeout) {
			break
		}
		log.Printf("uartgw: radio module did not answer application query, retrying")
	}
	return "", lastErr
}

// changeApp sends OSChangeApp and waits until the module announces
// want. OSChangeApp toggles between bootloader and application.
func (c *Conn) changeApp(want string) error {
	// Discard announcements which arrived earlier.
	for drained := false; !drained; {
		select {
		case name := <-c.announce:
			log.Printf("uartgw: discarding stale announcement %q", name)
		default:
			drained = true
		}
	}

	ctx, cancel := c.stepCtx()
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()

	// on the wire: FD0003000003180A (counter 0 in old hmgo)
	p, err := c.start(dstOS, osChangeApp, nil)
	if err != nil {
		return err
	}
	defer c.abandon(p)
	// ack:          FD000400000401993D
	// then, e.g.:   FD000D000000436F5F4350555F417070D831 (“Co_CPU_App”)
	t := time.NewTimer(c.timeout)
	defer t.Stop()
	for {
		select {
		case f := <-p.ack:
			if f.payload[0] == ackNACK {
				// c.f. FHEM: “application switch failed,
				// application-firmware probably corrupted!”
				return fmt.Errorf("%w: application switch failed, application firmware probably corrupted", ErrNACK)
			}
		case name := <-c.announce:
			if name == want {
				return nil
			}
			log.Printf("uartgw: ignoring announcement %q while waiting for %q", name, want)
		case <-t.C:
			return fmt.Errorf("%w: no %q announcement within %v", ErrTimeout, want, c.timeout)
		case <-c.done:
			return c.Err()
		}
	}
}

func (c *Conn) getFirmwareVersion(ctx context.Context) error {
	// on the wire: FD00030001021E0C
	// ack:         FD000A00010402010003010201AA8A
	f, err := c.simple(ctx, dstOS, osGetFirmware, nil)
	if err != nil {
		return err
	}
	// payload: 02 (ACK_INFO) 01 00 03 <major> <minor> <patch>
	if f.payload[0] != ackInfo || len(f.payload) < 7 {
		return fmt.Errorf("unexpected firmware ack: %x", f.payload)
	}
	v := f.payload[4:7]
	c.firmware = fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
	fw := uint32(v[0])<<16 | uint32(v[1])<<8 | uint32(v[2])
	// bidcos.Packet.Encode always includes the burst byte in AppSend
	// requests, which FHEM only sends if ($hash->{Helper}{FW} >
	// 0x010006).
	if fw <= 0x010006 {
		return firmwareTooOldError(c.firmware)
	}
	// c.f. FHEM: $fw .= " (outdated)" if ($hash->{Helper}{FW} < 0x010401);
	if fw < 0x010401 {
		log.Printf("uartgw: radio module firmware %s is outdated, please update to 1.4.1", c.firmware)
	}
	return nil
}

// firmwareTooOldError is the firmware version of a module whose
// firmware is too old; it matches ErrFirmwareTooOld.
type firmwareTooOldError string

func (e firmwareTooOldError) Error() string {
	return fmt.Sprintf("radio module firmware %s is too old; hmbridge needs 1.4.1 or newer (1.0.7 at the very least) — "+
		"update it with OpenCCU or the eQ-3 flash tool", string(e))
}

func (e firmwareTooOldError) Is(target error) bool { return target == ErrFirmwareTooOld }

func (c *Conn) getSerialNumber(ctx context.Context) error {
	// on the wire: FD000300030B9239
	// ack:         FD000E000304024E4551313333303938306AB9 (“NEQ1330980”)
	f, err := c.simple(ctx, dstOS, osGetSerial, nil)
	if err != nil {
		return err
	}
	if f.payload[0] != ackInfo {
		return fmt.Errorf("unexpected serial ack: %x", f.payload)
	}
	c.serial = string(f.payload[1:])
	return nil
}

func (c *Conn) getCredits(ctx context.Context) error {
	f, err := c.simple(ctx, dstOS, osGetCredits, nil)
	if err != nil {
		return err
	}
	if f.payload[0] == ackInfo && len(f.payload) > 1 {
		c.setCredits(f.payload[1:])
	}
	return nil
}

func (c *Conn) creditsLoop() {
	t := time.NewTicker(c.creditsInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-c.done:
			return
		}
		ctx, cancel := c.stepCtx()
		if err := c.acquire(ctx); err != nil {
			cancel()
			continue // done or timeout; the select above handles done
		}
		err := c.getCredits(ctx)
		c.release()
		cancel()
		if err != nil && c.Err() == nil {
			log.Printf("uartgw: polling duty cycle credits: %v", err)
		}
	}
}

func timePayload(t time.Time) []byte {
	// on the wire: FD000800040E58A7116300548E
	// uint32 seconds since epoch (big endian), int8 UTC offset in
	// units of 30 minutes
	_, offset := t.Zone()
	b := binary.BigEndian.AppendUint32(nil, uint32(t.Unix()))
	return append(b, byte(int8(offset/1800)))
}

func (c *Conn) setTime(ctx context.Context, t time.Time) error {
	// ack: FD000400040401196E
	_, err := c.simple(ctx, dstOS, osSetTime, timePayload(t))
	return err
}

// SetTime sets the module’s clock (used for AES challenges). FHEM does
// this about every 2 hours.
func (c *Conn) SetTime(ctx context.Context, t time.Time) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	if err := c.setTime(ctx, t); err != nil {
		return fmt.Errorf("uartgw: setting time: %w", err)
	}
	return nil
}

// keyPayload returns the payload for the set {current,previous,temp}
// key commands: 16 key bytes + key index, or 17 zero bytes to clear the
// slot.
func keyPayload(k *hmkey.Key) []byte {
	b := make([]byte, 17)
	if k != nil {
		copy(b, k.Bytes[:])
		b[16] = k.Index
	}
	return b
}

// AddPeer registers a device with the module, so that the module
// performs AES challenges on the configured channels and handles
// wakeup.
//
// c.f. HMUARTLGW_UpdatePeerReq in FHEM: add peer, disable AES on the
// channels which should not use it, enable AES on AESChannels.
func (c *Conn) AddPeer(ctx context.Context, p Peer) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()

	// on the wire: FD000901080640C2A8000000022E
	// ack:         FD00100108040701010001FFFFFFFFFFFFFFFFCAAF
	var wakeup byte
	if p.Wakeup {
		wakeup = 0x01
	}
	payload := []byte{
		p.Addr[0], p.Addr[1], p.Addr[2],
		p.KeyIndex,
		wakeup,
		0x00, // FHEM: “setting this causes "0013" messages for thermostats on wakeup ?!”
	}
	f, err := c.do(ctx, dstApp, appAddPeer, payload, c.timeout)
	if err != nil {
		return fmt.Errorf("uartgw: adding peer %X: %w", p.Addr, err)
	}
	// A successful add returns ACK_WITH_MULTIPART_DATA:
	// 07 <part> <parts> <assigned peer count (uint16)> <AES bitmap>
	// FHEM considers everything else a failure (“You have probably
	// forced an unknown aesKey for this device.”).
	if f.payload[0] != ackWithMultipartData {
		return fmt.Errorf("uartgw: adding peer %X failed (ack %s %x); unknown key index %d?",
			p.Addr, ackName(f.payload[0]), f.payload[1:], p.KeyIndex)
	}

	aes := make(map[byte]bool)
	var enable []byte
	for _, ch := range p.AESChannels {
		if !aes[ch] {
			aes[ch] = true
			enable = append(enable, ch)
		}
	}
	var disable []byte
	for ch := 0; ch < p.Channels && ch < 256; ch++ {
		if !aes[byte(ch)] {
			disable = append(disable, byte(ch))
		}
	}

	// on the wire: FD000D010A0A40C2A8000102030405068B17
	// ack:         FD0004010A04010DB6
	if len(disable) > 0 {
		if _, err := c.simple(ctx, dstApp, appPeerRemoveAES, append(p.Addr[:], disable...)); err != nil {
			return fmt.Errorf("uartgw: disabling AES for peer %X channels %v: %w", p.Addr, disable, err)
		}
	}
	if len(enable) > 0 {
		if _, err := c.simple(ctx, dstApp, appPeerAddAES, append(p.Addr[:], enable...)); err != nil {
			return fmt.Errorf("uartgw: enabling AES for peer %X channels %v: %w", p.Addr, enable, err)
		}
	}
	return nil
}

// Send implements bidcos.Transport: it transmits encoded (as produced
// by bidcos.Packet.Encode) via AppSend and waits for the module’s ack.
//
// It retries (for up to 3s, every 200ms) while the module reports
// CSMA/CA failure or “in progress”. Ack statuses map as follows:
//
//	0x01, 0x02  → nil, nil (transmitted; no response requested)
//	0x03        → response frame (status rewritten to 0x01), nil
//	0x0c        → response frame (status rewritten to 0x02, AES OK), nil
//	0x0d        → response frame (status rewritten to 0x03), ErrAESFailed
//	0x00        → ErrNACK
//	0x04        → ErrNoResponse
//	0x05        → ErrNoCredits
//
// The rewritten status makes bidcos.Decode(resp).Auth() report the AES
// result correctly.
func (c *Conn) Send(ctx context.Context, encoded []byte) ([]byte, error) {
	// status info burst cnt flags cmd src(3) dst(3)
	if len(encoded) < 12 {
		return nil, fmt.Errorf("uartgw: packet too short: %x", encoded)
	}
	timeout := c.timeout
	if encoded[4]&0x20 != 0 { // BiDi: the module waits for the device response
		timeout = c.sendTimeout
	}

	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()

	f, err := c.do(ctx, dstApp, appSend, encoded, timeout)
	if err != nil {
		switch {
		case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
			sendErrors.WithLabelValues("timeout").Inc()
		case errors.Is(err, ErrBusy):
			sendErrors.WithLabelValues("busy").Inc()
		default:
			sendErrors.WithLabelValues("other").Inc()
		}
		return nil, err
	}

	status := f.payload[0]
	switch status {
	case ackOK, ackInfo:
		return nil, nil

	case ackWithResponse, ackWithResponseAESOK, ackWithResponseAESFail:
		// payload: status info rssi msg…
		if len(f.payload) < 12 {
			sendErrors.WithLabelValues("other").Inc()
			return nil, fmt.Errorf("uartgw: ack %s with truncated response: %x", ackName(status), f.payload)
		}
		resp := append([]byte(nil), f.payload...)
		switch status {
		case ackWithResponse:
			resp[0] = recvResp
		case ackWithResponseAESOK:
			resp[0] = recvRespWithAESOK
		case ackWithResponseAESFail:
			resp[0] = recvRespWithAESKO
			sendErrors.WithLabelValues("aes_failed").Inc()
			return resp, ErrAESFailed
		}
		return resp, nil

	case ackNACK:
		sendErrors.WithLabelValues("nack").Inc()
		return nil, ErrNACK
	case ackEUnknown:
		sendErrors.WithLabelValues("no_response").Inc()
		return nil, ErrNoResponse
	case ackENoCredits:
		// c.f. FHEM: “IO in overload!”
		sendErrors.WithLabelValues("no_credits").Inc()
		return nil, ErrNoCredits
	default:
		sendErrors.WithLabelValues("other").Inc()
		return nil, fmt.Errorf("uartgw: unexpected ack %s: %x", ackName(status), f.payload)
	}
}
