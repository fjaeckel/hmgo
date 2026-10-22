// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package uartgw

// This file implements the RPI-RF-MOD (and other modules running eQ-3's
// “DualCoPro” dual-coprocessor firmware, e.g. 4.4.x) for classic
// BidCoS devices.
//
// The protocol knowledge comes from two sources:
//
//   - RFNETHM (https://github.com/tostmann/RFNETHM at 9092276,
//     GPL-2.0-or-later; used as a protocol reference only, no code was
//     copied): firmware/src/hmu_frame.h (frame format and destinations),
//     source_uart.c (boot_probe, uart_frame_cb: banners, IDENTIFY,
//     CHANGE_APP) and sink_hmuartlgw_legacy.c (LLMAC send/receive/ack).
//   - A 30 minute hmsniff capture of OpenCCU (multimacd/rfd) talking to
//     an RPI-RF-MOD with firmware 4.4.22 (hmsniff-20261022-094052.log,
//     “capture” below, with line numbers).
//
// Frames use the same format as the classic firmware (frame.go), but
// the packet has no separate command byte: [dst cnt payload…]. As the
// first payload byte acts as a command on every destination, the frame
// type is reused with frame.cmd = first payload byte.
//
// Unlike the classic firmware, which handles ACKs and AES for
// registered peers by itself, the dual-coprocessor firmware only
// offers a BidCoS MAC layer (“LLMAC”): send a frame at a given time,
// receive every frame on the air. The host does the BidCoS logic: ACKs,
// AES, retries (capture: the module never ACKs device messages, rfd
// does, e.g. lines 1318-1320; AES lines 694-701). So does DualConn
// (Config.SoftwareAck, Config.SoftwareAES).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	bidcosaes "github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/prometheus/client_golang/prometheus"
)

// COMMON (dst 0xfe) commands (from RFNETHM: HMU_PL_IDENTIFY,
// HMU_PL_CHANGE_APP; restart capture: SGTIN).
const (
	commonIdentify  = 0x01 // reply: [0x05 0x01 tag…] or [0x00 tag…]
	commonChangeApp = 0x03 // bootloader → app; announced ~2.2s later
	commonSGTIN     = 0x04 // reply: [0x05 0x01 sgtin(12)] (restart capture line 29)
	commonReply     = 0x05
)

// dstHMIPCrypto is used by OpenCCU's HMIPServer at its start (restart
// capture lines 96-104), apparently for Homematic IP key handling;
// hmbridge treats it like dstHMIP (Config.HMIPHandler).
const dstHMIPCrypto = 0x0c

// hmipQueueSize bounds the frames waiting for Config.HMIPHandler.
const hmipQueueSize = 64

// LLMAC (dst 0x03) commands (from RFNETHM, confirmed by the capture).
const (
	llmacAck  = 0x01 // module → host: [0x01 status ts_hi ts_lo], same frame counter as the LLMAC_SEND
	llmacRecv = 0x05 // module → host: [0x05 ts_hi ts_lo info rssi msg…]
	llmacSend = 0x06 // host → module: [0x06 t_hi t_lo mode msg…]

	llmacStatusOK = 0x01 // LLMAC_ACK: transmitted

	// Queries multimacd sends at its start (restart capture lines
	// 20-43), answered with an LLMAC_ACK [0x01 0x01 data…].
	llmacGetClock  = 0x02 // data: module clock (15-bit ms), e.g. 6e d7
	llmacGetSerial = 0x07 // data: serial number, e.g. “QEQ0684602”
	// 0x08 (data 00 00 00, perhaps an unset address) is not sent.
)

// TRX (dst 0x01 “APP”) commands multimacd sends while running
// (capture).
const (
	trxFirmware    = 0x02 // reply [0x04 0x01 major minor patch ?(6)], e.g. 04 04 16 = 4.4.22 (restart capture line 26)
	trxStatus      = 0x03 // every 10s, e.g. line 30: reply [0x04 0x01 xx], xx 0x02..0x0c
	trxUnknown0A   = 0x0a // every 5min (line 324), reply [0x04 0x01 0x03]; not used by hmbridge
	trxReply       = 0x04
	trxUnsolicited = 0x0b // module → host, [0x0b 14 02] (line 940), [0x0b 00 01] (line 1094); meaning unknown
)

// LLMAC_SEND options [t_hi t_lo mode] (capture):
//
//   - t is the absolute time of the transmission on the module's 15-bit
//     millisecond clock, the same clock as the ts of LLMAC_RECV and
//     LLMAC_ACK (max seen 0x7f43; 1 tick = 1ms). sendNow (t = 0x8000,
//     i.e. bit 15 set) means “as soon as possible”.
//   - mode is 0x80, plus 0x10 (the BidCoS Burst flag) for burst
//     transmissions (opts 80 00 90, e.g. line 96: set temperature on an
//     HM-CC-RT-DN; the LLMAC_ACK arrives ~380ms later, after the
//     preamble).
const (
	sendNow      = 0x8000
	modulePeriod = 0x8000 // the module clock wraps at 15 bits
	sendModeBase = 0x80
)

// Replies to a device message are scheduled replyDelay after its
// reception (module time). OpenCCU sends AES challenges and plain ACKs
// at rx+106/107ms, the final ACK after an AES answer at rx+97ms and
// time answers at rx+109ms (capture, e.g. lines 694-701, 1318-1324).
// If the host is so slow that the module time already passed
// t - replyLateMargin, the reply is sent at once instead.
const (
	replyDelay      = 100 * time.Millisecond
	replyLateMargin = 25 * time.Millisecond
	// replyWindow: a non-BiDi Send to a device that sent us a message
	// with the same message counter within this window is a reply (e.g.
	// the bridge's answer to a time request).
	replyWindow = time.Second
)

// Application tags of the dual-coprocessor firmware (from RFNETHM).
const (
	appTagDualBootloader = "HMIP_TRX_Bl"
	appTagDualApp        = "DualCoPro_App"
)

// BidCoS message layout (cnt flags cmd src(3) dst(3) payload…) and the
// values DualConn needs.
const (
	bidcosFlagBurst        = 0x10
	bidcosFlagBiDi         = 0x20
	bidcosFlagRepeatEnable = 0x80

	bidcosCmdDeviceInfo  = 0x00
	bidcosCmdAck         = 0x02 // payload 0x00 ACK, 0x01 ACK_STATUS, 0x04 AES challenge, 0x80 NACK
	bidcosCmdAESResponse = 0x03
	bidcosCmdTime        = 0x3f

	bidcosAckChallenge = 0x04
	bidcosNACK         = 0x80

	minMsgLen = 9 // cnt flags cmd src dst
)

// Flags of the frames DualConn sends on its own (capture, lines
// 694-701, 1318-1320): ACKs 0x80 (RPTEN), AES challenges 0xa0
// (BIDI|RPTEN). Our answer to a device's challenge (cmd 0x03) is not in
// the capture; it expects the device's ACK, hence BIDI|RPTEN.
const (
	ackFlags       = bidcosFlagRepeatEnable                  // 0x80
	challengeFlags = bidcosFlagRepeatEnable | bidcosFlagBiDi // 0xa0
	aesAnswerFlags = bidcosFlagRepeatEnable | bidcosFlagBiDi // 0xa0
)

// Defaults for the dual-coprocessor firmware.
const (
	// OpenCCU retransmits an unanswered command 3 times, 274-275ms
	// after each send request, even when the LLMAC_ACK only arrived
	// 195ms after the request (restart capture: HM-OU-LED16 at
	// 10:38:20; the first capture's lines 48-82 show the same spacing).
	defaultResponseTimeout = 275 * time.Millisecond
	// minWaitAfterAck is the least time to wait for a response after a
	// late LLMAC_ACK (OpenCCU retransmitted 79ms after one).
	minWaitAfterAck        = 75 * time.Millisecond
	defaultSendRetries     = 3
	defaultChangeAppWait   = 3500 * time.Millisecond // RFNETHM boot_probe: 3500ms
	defaultTRXPollInterval = 10 * time.Second        // like multimacd
	minAESHoldTimeout      = time.Second
	writeQueueSize         = 64
	maxOneshots            = 64

	// maxChallengeResends bounds how often a challenge is re-sent
	// because the device retransmitted the held message (it did not
	// receive our challenge); each re-send extends the hold by
	// aesHoldTimeout.
	maxChallengeResends = 3
	// At most challengeRateLimit new AES challenges are sent per peer
	// within challengeRateWindow; further messages needing one are
	// dropped. This protects the module's 1% duty cycle against spoofed
	// frames (anyone can send frames with a peer's address).
	challengeRateLimit  = 10
	challengeRateWindow = 10 * time.Second
)

// AESSigner implements the BidCoS AES computations (software AES; see
// package bidcosaes). Messages are BidCoS bytes starting at the message
// counter.
type AESSigner interface {
	// Respond computes the 16-byte payload of our cmd 0x03 answer to a
	// device's challenge for msg (the message we sent).
	Respond(keyIndex byte, challenge [6]byte, msg []byte) ([16]byte, error)
	// Verify checks a device's 16-byte cmd 0x03 answer to our challenge
	// for msg (the device's original message).
	Verify(keyIndex byte, challenge [6]byte, msg []byte, response [16]byte) bool
}

// AESAuthResponder is optionally implemented by an AESSigner (package
// bidcosaes: Signer.RespondAuth). If it is, Send checks the 4 auth bytes
// the device must append to its final ACK after our AES answer (FHEM
// 10_CUL_HM.pm: AESreqAck, compared with the last 4 bytes of the ACK
// payload, so both ACK 02 00 auth(4) and ACK_STATUS 02 01 … auth(4)
// work); a final ACK without them or with wrong ones fails with
// ErrAESFailed (FHEM: “aesCommToDev:fail”). Without it, any final ACK
// which is not a NACK counts as AES OK.
type AESAuthResponder interface {
	RespondAuth(keyIndex byte, challenge [6]byte, msg []byte) (response [16]byte, auth [4]byte, err error)
}

var (
	_ AESSigner        = (*bidcosaes.Signer)(nil)
	_ AESAuthResponder = (*bidcosaes.Signer)(nil)
	_ AESAuthVerifier  = (*bidcosaes.Signer)(nil)
)

// AESAuthVerifier is optionally implemented by an AESSigner. If it is,
// the final ACK after a successful verification carries the 4 auth
// bytes (cmd 0x02 payload [0x00 auth…], c.f. FHEM aesAuthBytes /
// AESreqAck; capture line 701: “009d20665c”) instead of a bare 02 00.
type AESAuthVerifier interface {
	VerifyAuth(keyIndex byte, challenge [6]byte, msg []byte, response [16]byte) (auth [4]byte, ok bool)
}

// InitStep is a request sent to the dual-coprocessor firmware during
// initialization (Config.DualInit) or AddPeer (Config.DualPeerInit).
type InitStep struct {
	Name    string // for logs and errors
	Dst     byte   // destination, e.g. 0x00 OS/HMSYSTEM, 0x01 TRX, 0x03 LLMAC, 0xfe COMMON
	Payload []byte // complete payload (the first byte acts as the command)

	// ExpectAck makes the step wait (Config.Timeout) for the module's
	// reply: a frame on Dst (or DUAL_ERR, which fails the step) with the
	// same frame counter. Check, if non-nil, validates the reply payload
	// (first byte = command).
	ExpectAck bool
	Check     func(reply []byte) error
}

var (
	dualEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dual_events_total",
			Help: "RPI-RF-MOD software BidCoS events: software_ack, aes_challenge_sent, aes_challenge_resent (the device " +
				"retransmitted a held message), aes_challenge_rate_limited (message dropped), aes_ok, aes_failed, " +
				"aes_timeout, aes_duplicate (retransmission of an authenticated message, acknowledged again), " +
				"aes_stray_answer (AES answer without a pending challenge, dropped), aes_challenge_received, " +
				"response_timeout, retransmit, oneshot_failed, " +
				"reply_scheduled, reply_late (sent at once: the host was too slow to schedule it), hmip_ignored (Homematic IP frame, " +
				"not used for BidCoS), hmip_dropped (Homematic IP frame not passed on: handler too slow), trx_poll_failed.",
		},
		[]string{"event"})

	dualTRXStatus = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dual_trx_status",
			Help: "RPI-RF-MOD: raw value of the TRX status poll (APP cmd 0x03 reply [04 01 xx]), " +
				"probably the duty cycle or channel load; meaning unconfirmed.",
		})

	moduleInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "module_info",
			Help:      "Radio module kind, announced application and (RPI-RF-MOD) SGTIN (always 1).",
		},
		[]string{"kind", "app", "sgtin"})
)

func init() {
	prometheus.MustRegister(dualEvents, dualTRXStatus, moduleInfo)
}

// dualReq is an outstanding request, matched by frame counter.
type dualReq struct {
	dst  byte
	sent time.Time
	ch   chan frame // buffered (1)
	// arm, if non-nil, is armed by a successful LLMAC_ACK of this
	// request (in the reader goroutine, before any later frame is
	// dispatched).
	arm *respWaiter
}

// errDualErr marks a DUAL_ERR reply (wrapped together with ErrNACK).
var errDualErr = errors.New("DUAL_ERR")

// errNoCounter is returned when all 255 frame counters are in use.
var errNoCounter = errors.New("uartgw: no free frame counter (too many outstanding requests)")

// rxMsg is a received message as handed to Send.
type rxMsg struct {
	frame []byte // receive frame: status info rssi msg…
	ts    uint16 // module time of the reception
}

// respWaiter matches device responses to the packet Send transmits.
// Only frames received after the module confirmed the transmission
// (LLMAC_ACK, armed) are responses.
type respWaiter struct {
	dev, us [3]byte
	msgcnt  byte
	ch      chan rxMsg // buffered
	armed   bool       // guarded by DualConn.mu
}

type aesKey struct {
	src    [3]byte
	msgcnt byte
}

// heldMsg is a received message held back until the device answered
// our AES challenge.
type heldMsg struct {
	info, rssi byte
	msg        []byte
	keyIndex   byte
	challenge  [6]byte
	timer      *time.Timer
	resends    int // challenges re-sent for retransmissions
}

// aesDone remembers a successfully authenticated message, so that a
// retransmission (the device missed our final ACK) is acknowledged
// again with the same auth bytes instead of being challenged and
// delivered a second time.
type aesDone struct {
	msg  []byte
	auth []byte
	at   time.Time
}

// rxSeen records when a device's message to us was received, so that a
// reply sent via Send can be scheduled like OpenCCU does.
type rxSeen struct {
	ts uint16
	at time.Time
}

type queued struct {
	b        []byte
	dst, cmd byte
}

// DualConn is an initialized connection to an RPI-RF-MOD (or another
// module running the dual-coprocessor firmware), used for classic
// BidCoS only. It implements Module.
//
// Like Conn, it owns a reader goroutine; user requests (Send, AddPeer)
// are serialized. In addition, a writer goroutine writes all frames, so
// that the reader goroutine can queue software ACKs and AES challenges
// without ever blocking on the serial port or on a running Send. These
// replies are scheduled on the module clock (replyDelay after the
// device's message), so the host only has to queue them within ~90ms.
type DualConn struct {
	rw   io.ReadWriter
	next func() (frame, error)

	hmid            [3]byte
	timeout         time.Duration
	responseTimeout time.Duration
	aesHoldTimeout  time.Duration
	changeAppWait   time.Duration
	trxPollInterval time.Duration
	retries         int
	maxAckTimeouts  int
	softAck         bool
	softAES         bool
	signer          AESSigner
	rand            io.Reader
	debug           bool
	peerInit        func(Peer) []InitStep

	tag         string
	firmware    string
	serial      string
	sgtin       string
	cfgFirmware string

	sem      chan struct{} // serializes user requests
	wq, uq   chan queued   // normal / urgent writes
	announce chan string   // tags announced before the app was entered
	done     chan struct{}

	closeOnce sync.Once

	mu           sync.Mutex
	now          func() time.Time // local monotonic clock (tests may skew it)
	cnt          byte
	reqs         map[byte]*dualReq
	oneshots     map[byte]time.Time // fire-and-forget LLMAC_SENDs (software ACKs, challenges)
	lateIdentify map[byte]bool      // IDENTIFY requests we gave up on
	resp         *respWaiter
	held         map[aesKey]*heldMsg
	authed       map[aesKey]aesDone      // recently authenticated messages
	challenges   map[[3]byte][]time.Time // recent challenges per peer (rate limit)
	seen         map[aesKey]rxSeen       // recent messages to us, by sender and message counter
	peers        map[[3]byte]Peer
	clockTs      uint16    // module time of the last LLMAC_RECV …
	clockAt      time.Time // … and the local time it was read
	clockValid   bool
	entered      bool
	running      bool
	err          error
	recv         chan []byte
	ackTimeouts  int
	signerWarned bool

	hmip chan frame // to Config.HMIPHandler; nil without one
}

// OpenDual initializes an RPI-RF-MOD (dual-coprocessor firmware) on rw
// without auto-detection; OpenModule detects the module kind. The
// module may have just been reset (it announces its bootloader), run
// its application already, or announce nothing.
//
// If OpenDual fails and rw implements io.Closer, rw is closed.
func OpenDual(rw io.ReadWriter, cfg Config) (*DualConn, error) {
	announceWait, timeout := cfg.AnnounceWait, cfg.Timeout
	if announceWait <= 0 {
		announceWait = defaultAnnounceWait
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	p := newFramePump(rw)
	d, err := detect(p, rw, announceWait, timeout, cfg.Debug)
	if err != nil {
		p.stop()
		closeRW(rw)
		return nil, err
	}
	if d.kind != KindRPIRFMod {
		p.stop()
		closeRW(rw)
		if d.tag != "" {
			return nil, fmt.Errorf("%w: expected an RPI-RF-MOD, but the module announced %q (%s)", ErrUnknownModule, d.tag, d.kind)
		}
		return nil, fmt.Errorf("%w: expected an RPI-RF-MOD, but the module did not answer IDENTIFY", ErrUnknownModule)
	}
	return openDual(rw, cfg, p, d)
}

func boolDefault(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func openDual(rw io.ReadWriter, cfg Config, p *framePump, d detection) (*DualConn, error) {
	c := &DualConn{
		rw:              rw,
		next:            p.next,
		hmid:            cfg.HMID,
		timeout:         cfg.Timeout,
		responseTimeout: cfg.ResponseTimeout,
		changeAppWait:   cfg.ChangeAppWait,
		trxPollInterval: cfg.TRXPollInterval,
		retries:         cfg.SendRetries,
		maxAckTimeouts:  cfg.MaxAckTimeouts,
		softAck:         boolDefault(cfg.SoftwareAck, true),
		softAES:         boolDefault(cfg.SoftwareAES, true),
		signer:          cfg.Signer,
		rand:            cfg.Rand,
		debug:           cfg.Debug,
		peerInit:        cfg.DualPeerInit,
		cfgFirmware:     cfg.DualFirmwareVersion,
		sem:             make(chan struct{}, 1),
		wq:              make(chan queued, writeQueueSize),
		uq:              make(chan queued, writeQueueSize),
		announce:        make(chan string, 8),
		done:            make(chan struct{}),
		now:             time.Now,
		cnt:             detectCnt,
		reqs:            make(map[byte]*dualReq),
		oneshots:        make(map[byte]time.Time),
		// detect's IDENTIFY may be answered after an announcement.
		lateIdentify: map[byte]bool{detectCnt: true},
		held:         make(map[aesKey]*heldMsg),
		authed:       make(map[aesKey]aesDone),
		challenges:   make(map[[3]byte][]time.Time),
		seen:         make(map[aesKey]rxSeen),
		peers:        make(map[[3]byte]Peer),
		recv:         make(chan []byte, recvBufferSize),
	}
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	if c.responseTimeout <= 0 {
		c.responseTimeout = defaultResponseTimeout
	}
	// The device answers our challenge only after it was transmitted
	// (replyDelay) and it computed the answer (capture: ~150ms after the
	// challenge, lines 696-699).
	c.aesHoldTimeout = replyDelay + 2*c.responseTimeout
	if c.aesHoldTimeout < minAESHoldTimeout {
		c.aesHoldTimeout = minAESHoldTimeout
	}
	if c.changeAppWait <= 0 {
		c.changeAppWait = defaultChangeAppWait
	}
	if c.trxPollInterval == 0 {
		c.trxPollInterval = defaultTRXPollInterval
	}
	if c.retries == 0 {
		c.retries = defaultSendRetries
	} else if c.retries < 0 {
		c.retries = 0
	}
	if c.maxAckTimeouts == 0 {
		c.maxAckTimeouts = defaultMaxAckTimeouts
	}
	if c.rand == nil {
		c.rand = rand.Reader
	}
	if cfg.HMIPHandler != nil {
		c.hmip = make(chan frame, hmipQueueSize)
		go c.hmipLoop(cfg.HMIPHandler)
	}

	go c.readLoop()
	go c.writeLoop()

	if err := c.init(cfg, d); err != nil {
		c.fail(err)
		closeRW(rw)
		upGauge.Set(0)
		if cerr := c.Err(); cerr != nil && cerr != err && !errors.Is(err, cerr) {
			return nil, fmt.Errorf("%w (while: %v)", cerr, err)
		}
		return nil, err
	}

	c.mu.Lock()
	if err := c.err; err != nil {
		c.mu.Unlock()
		closeRW(rw)
		return nil, err
	}
	c.running = true
	upGauge.Set(1)
	c.mu.Unlock()

	infoGauge.Reset()
	infoGauge.WithLabelValues(c.firmware, c.SerialNumber()).Set(1)
	moduleInfo.Reset()
	moduleInfo.WithLabelValues(KindRPIRFMod, c.tag, c.sgtin).Set(1)
	if c.softAES && c.signer == nil {
		log.Printf("uartgw: no AES signer configured; messages of AES peers will fail authentication")
	}
	if c.trxPollInterval > 0 {
		go c.trxPollLoop()
	}
	return c, nil
}

// FirmwareVersion returns the module's firmware version, e.g. “4.4.22”
// (Config.DualFirmwareVersion overrides it).
func (c *DualConn) FirmwareVersion() string { return c.firmware }

// SerialNumber returns the module's serial number, e.g. “QEQ0684602”,
// or "unknown" if the module did not report it.
func (c *DualConn) SerialNumber() string {
	if c.serial == "" {
		return "unknown"
	}
	return c.serial
}

// SGTIN returns the module's SGTIN (Homematic IP device ID, 24 hex
// digits), or "" if the module did not report it.
func (c *DualConn) SGTIN() string { return c.sgtin }

// Kind returns KindRPIRFMod.
func (c *DualConn) Kind() string { return KindRPIRFMod }

// Application returns the application tag the module reported, e.g.
// “DualCoPro_App”.
func (c *DualConn) Application() string { return c.tag }

// Recv delivers received radio frames (status info rssi msg…; status
// 0x01, or 0x02/0x03 after software AES). Responses consumed by Send
// and AES answers are not delivered. Closed when the connection fails;
// see Err.
func (c *DualConn) Recv() <-chan []byte { return c.recv }

// Err returns the error which made the connection fail, ErrClosed after
// Close, or nil while the connection is healthy.
func (c *DualConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close stops the connection and closes rw if it implements io.Closer.
func (c *DualConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.fail(ErrClosed)
		err = closeRW(c.rw)
		upGauge.Set(0)
	})
	return err
}

func (c *DualConn) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	for k, h := range c.held {
		h.timer.Stop()
		delete(c.held, k)
	}
	running := c.running
	upGauge.Set(0)
	close(c.done)
	close(c.recv)
	c.mu.Unlock()
	if running && err != ErrClosed {
		log.Printf("uartgw: connection failed: %v", err)
	}
}

func (c *DualConn) logFrame(dir string, f frame) {
	if c.debug {
		log.Printf("uartgw: %s %v", dir, f)
	}
}

// ---- module clock and scheduling ----

// sendOpts returns the LLMAC_SEND options for a transmission at t
// (sendNow: at once) of a message with flags.
func sendOpts(t uint16, flags byte) [3]byte {
	return [3]byte{byte(t >> 8), byte(t), sendModeBase | flags&bidcosFlagBurst}
}

// diff15 returns a - b on the 15-bit module clock, in (-0x4000, 0x4000].
func diff15(a, b uint16) int {
	d := (int(a) - int(b)) & (modulePeriod - 1)
	if d > modulePeriod/2 {
		d -= modulePeriod
	}
	return d
}

// moduleTimeLocked estimates the module clock from the last LLMAC_RECV
// or clock query (LLMAC 02).
// LLMAC_ACK timestamps are not used: they are the transmission time,
// which for burst sends lies ~380ms before the ack arrives. c.mu must
// be held.
func (c *DualConn) moduleTimeLocked() (uint16, bool) {
	if !c.clockValid {
		return 0, false
	}
	ms := c.now().Sub(c.clockAt).Milliseconds()
	return uint16((int64(c.clockTs) + ms) & (modulePeriod - 1)), true
}

// replyOptsLocked returns the options for a reply to a message received
// at module time rxTs: replyDelay later, or at once if the host is too
// late for that. c.mu must be held.
func (c *DualConn) replyOptsLocked(rxTs uint16, flags byte) [3]byte {
	t := uint16((int(rxTs) + int(replyDelay/time.Millisecond)) & (modulePeriod - 1))
	if now, ok := c.moduleTimeLocked(); ok && diff15(t, now) < int(replyLateMargin/time.Millisecond) {
		dualEvents.WithLabelValues("reply_late").Inc()
		return sendOpts(sendNow, flags)
	}
	dualEvents.WithLabelValues("reply_scheduled").Inc()
	return sendOpts(t, flags)
}

// ---- writing ----

// writeLoop writes queued frames, urgent ones (software ACKs, AES
// challenges) first.
func (c *DualConn) writeLoop() {
	for {
		var q queued
		select {
		case q = <-c.uq:
		default:
			select {
			case q = <-c.uq:
			case q = <-c.wq:
			case <-c.done:
				return
			}
		}
		if _, err := c.rw.Write(q.b); err != nil {
			c.fail(fmt.Errorf("uartgw: writing to radio module: %w", err))
			return
		}
		framesSent.WithLabelValues(dstLabel(q.dst), cmdLabel(q.cmd)).Inc()
	}
}

// enqueue queues f for writing, waiting for space unless urgent (then
// the frame is dropped if the queue is full).
func (c *DualConn) enqueue(ctx context.Context, f frame, urgent bool) error {
	c.logFrame("send", f)
	q := queued{b: f.encode(), dst: f.dst, cmd: f.cmd}
	if urgent {
		select {
		case c.uq <- q:
			return nil
		default:
			return errors.New("uartgw: write queue full")
		}
	}
	select {
	case c.wq <- q:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

// nextCnt returns an unused frame counter, or errNoCounter if all are
// in use. Counter 0 is skipped, as unsolicited frames (announcements)
// may use it. c.mu must be held.
func (c *DualConn) nextCnt() (byte, error) {
	for i := 0; i < 256; i++ {
		c.cnt++
		if c.cnt == 0 {
			continue
		}
		if _, ok := c.reqs[c.cnt]; ok {
			continue
		}
		if _, ok := c.oneshots[c.cnt]; ok {
			continue
		}
		return c.cnt, nil
	}
	return 0, errNoCounter
}

// request sends payload (first byte = command) to dst and waits for the
// reply (a frame on dst or DUAL_ERR with the same counter). DUAL_ERR
// replies are returned with an error wrapping ErrNACK (and errDualErr).
func (c *DualConn) request(ctx context.Context, dst byte, payload []byte, timeout time.Duration) (frame, error) {
	return c.requestArm(ctx, dst, payload, timeout, nil)
}

// requestArm is request; a successful LLMAC_ACK arms w (see dualReq).
func (c *DualConn) requestArm(ctx context.Context, dst byte, payload []byte, timeout time.Duration, w *respWaiter) (frame, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return frame{}, err
	}
	cnt, err := c.nextCnt()
	if err != nil {
		c.mu.Unlock()
		return frame{}, err
	}
	r := &dualReq{dst: dst, sent: time.Now(), ch: make(chan frame, 1), arm: w}
	c.reqs[cnt] = r
	c.mu.Unlock()
	abandon := func() {
		c.mu.Lock()
		if c.reqs[cnt] == r {
			delete(c.reqs, cnt)
			if dst == dstDual && len(payload) > 0 && payload[0] == commonIdentify {
				c.lateIdentify[cnt] = true
			}
		}
		c.mu.Unlock()
	}

	f := frame{dst: dst, cnt: cnt, cmd: payload[0], payload: payload[1:]}
	if err := c.enqueue(ctx, f, false); err != nil {
		abandon()
		return frame{}, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case rf := <-r.ch:
		requestDuration.Observe(time.Since(r.sent).Seconds())
		if rf.dst == dstDualErr {
			// Not a sign of life: a module which dropped into its
			// bootloader answers everything with DUAL_ERR.
			return rf, fmt.Errorf("%w: %w: module answered with DUAL_ERR (dst %s, cnt %d): %x", ErrNACK, errDualErr, dstName(dst), cnt, append([]byte{rf.cmd}, rf.payload...))
		}
		c.mu.Lock()
		c.ackTimeouts = 0
		c.mu.Unlock()
		return rf, nil
	case <-t.C:
		abandon()
		return frame{}, fmt.Errorf("%w (dst %s, cmd %02x, cnt %d, after %v)", ErrTimeout, dstName(dst), payload[0], cnt, timeout)
	case <-ctx.Done():
		abandon()
		return frame{}, ctx.Err()
	case <-c.done:
		return frame{}, c.Err()
	}
}

// isLivenessFailure reports whether err means the module did not
// properly answer a request: no reply at all, or DUAL_ERR.
func isLivenessFailure(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, errDualErr)
}

// ackTimeout counts consecutive requests the module did not ack or
// answered with DUAL_ERR (only once running, like Conn) and fails with
// ErrModuleUnresponsive.
func (c *DualConn) ackTimeout(err error) {
	ackTimeouts.Inc()
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	c.ackTimeouts++
	n := c.ackTimeouts
	c.mu.Unlock()
	if c.maxAckTimeouts > 0 && n >= c.maxAckTimeouts {
		c.fail(fmt.Errorf("%w: %d consecutive requests were not acknowledged or rejected (last: %v)", ErrModuleUnresponsive, n, err))
	}
}

func llmacSendPayload(opts [3]byte, msg []byte) []byte {
	p := make([]byte, 0, 4+len(msg))
	p = append(p, llmacSend)
	p = append(p, opts[:]...)
	return append(p, msg...)
}

// llmacSend transmits msg (cnt flags cmd src dst payload) and waits for
// the module's LLMAC_ACK, which arms w (if non-nil).
func (c *DualConn) llmacSend(ctx context.Context, msg []byte, opts [3]byte, w *respWaiter) error {
	timeout := c.timeout
	if opts[0]&0x80 == 0 {
		// scheduled: the module waits up to ~replyDelay before sending
		timeout += replyDelay
	}
	f, err := c.requestArm(ctx, dstLLMAC, llmacSendPayload(opts, msg), timeout, w)
	if err != nil {
		if isLivenessFailure(err) {
			c.ackTimeout(err)
		}
		return err
	}
	// LLMAC_ACK: [0x01 status ts_hi ts_lo]; in frame terms cmd=0x01,
	// payload = status ts_hi ts_lo (ts = transmission time).
	if f.cmd != llmacAck || len(f.payload) < 1 {
		acks.WithLabelValues("other").Inc()
		return fmt.Errorf("uartgw: unexpected reply to LLMAC_SEND: %v", f)
	}
	status := f.payload[0]
	if status != llmacStatusOK {
		// The capture only shows status 0x01 (no duty cycle or CSMA/CA
		// failures happened); other statuses are mapped to ErrNACK.
		acks.WithLabelValues(fmt.Sprintf("llmac_0x%02x", status)).Inc()
		return fmt.Errorf("%w: LLMAC_ACK status 0x%02x (%x)", ErrNACK, status, f.payload)
	}
	acks.WithLabelValues("llmac_ok").Inc()
	return nil
}

// sendOneshot queues an urgent LLMAC_SEND without waiting for its
// LLMAC_ACK (the reader goroutine cannot wait for frames it reads
// itself). The ack is matched and counted in dispatch.
func (c *DualConn) sendOneshot(msg []byte, opts [3]byte, what string) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	now := time.Now()
	if len(c.oneshots) >= maxOneshots {
		for cnt, t := range c.oneshots {
			if now.Sub(t) > 10*time.Second {
				delete(c.oneshots, cnt)
			}
		}
	}
	cnt, err := c.nextCnt()
	if err != nil {
		c.mu.Unlock()
		log.Printf("uartgw: dropping %s: %v", what, err)
		return
	}
	c.oneshots[cnt] = now
	c.mu.Unlock()
	f := frame{dst: dstLLMAC, cnt: cnt, cmd: llmacSend, payload: llmacSendPayload(opts, msg)[1:]}
	if err := c.enqueue(context.Background(), f, true); err != nil {
		log.Printf("uartgw: dropping %s: %v", what, err)
		c.mu.Lock()
		delete(c.oneshots, cnt)
		c.mu.Unlock()
		return
	}
	dualEvents.WithLabelValues(what).Inc()
}

// ackMsg returns our ACK (cmd 0x02 payload 0x00 [+ auth]) to a message
// with counter msgcnt from dev.
func (c *DualConn) ackMsg(msgcnt byte, dev [3]byte, auth []byte) []byte {
	m := []byte{msgcnt, ackFlags, bidcosCmdAck}
	m = append(m, c.hmid[:]...)
	m = append(m, dev[:]...)
	m = append(m, 0x00)
	return append(m, auth...)
}

// sendReply queues msg as a reply to a message received at rxTs.
func (c *DualConn) sendReply(msg []byte, rxTs uint16, what string) {
	c.mu.Lock()
	opts := c.replyOptsLocked(rxTs, msg[1])
	c.mu.Unlock()
	c.sendOneshot(msg, opts, what)
}

// ---- TRX status poll ----

// trxPollLoop polls the TRX status like multimacd (every 10s, APP cmd
// 0x03, capture line 30 ff.). It is the liveness check: unanswered polls
// and polls answered with DUAL_ERR count towards Config.MaxAckTimeouts,
// and after each failed poll the module is asked for its application
// (checkBootloader).
func (c *DualConn) trxPollLoop() {
	t := time.NewTicker(c.trxPollInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-c.done:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout+time.Second)
		f, err := c.request(ctx, dstApp, []byte{trxStatus}, c.timeout)
		cancel()
		if err != nil {
			if c.Err() != nil {
				return
			}
			dualEvents.WithLabelValues("trx_poll_failed").Inc()
			log.Printf("uartgw: polling the TRX status: %v", err)
			if c.checkBootloader() {
				return
			}
			if isLivenessFailure(err) {
				c.ackTimeout(err)
			}
			continue
		}
		// [0x04 0x01 xx]
		if f.cmd == trxReply && len(f.payload) >= 2 && f.payload[0] == 0x01 {
			dualTRXStatus.Set(float64(f.payload[1]))
		} else {
			log.Printf("uartgw: unexpected TRX status reply: %v", f)
		}
		// Keep the module clock estimate fresh without radio traffic.
		c.mu.Lock()
		stale := !c.clockValid || c.now().Sub(c.clockAt) >= c.trxPollInterval
		c.mu.Unlock()
		if stale {
			if err := c.queryClock(); err != nil && c.Err() == nil {
				log.Printf("uartgw: refreshing the module clock: %v", err)
			}
		}
	}
}

// checkBootloader sends IDENTIFY after a failed TRX poll. A module
// which dropped into its bootloader without (or with a lost)
// announcement answers every request with DUAL_ERR; if it reports its
// bootloader (“HMIP_TRX_Bl”), the connection fails with ErrModuleReset
// at once. It reports whether it failed the connection.
func (c *DualConn) checkBootloader() bool {
	tag, err := c.identify()
	if err != nil {
		if c.debug {
			log.Printf("uartgw: IDENTIFY after a failed TRX poll: %v", err)
		}
		return false
	}
	if isBootloaderTag(tag) || !isAppTag(tag) {
		c.fail(fmt.Errorf("%w (the module reports %q)", ErrModuleReset, tag))
		return true
	}
	return false
}

// ---- reading ----

func (c *DualConn) readLoop() {
	for {
		f, err := c.next()
		if err != nil {
			if c.Err() == nil {
				c.fail(fmt.Errorf("uartgw: reading from radio module: %w", err))
			}
			return
		}
		framesReceived.WithLabelValues(dstLabel(f.dst), cmdLabel(f.cmd)).Inc()
		c.logFrame("recv", f)
		c.dispatch(f)
	}
}

// deliverReq hands f to the outstanding request with its counter, if
// any.
func (c *DualConn) deliverReq(f frame) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.reqs[f.cnt]
	if r == nil || (r.dst != f.dst && f.dst != dstDualErr) {
		return false
	}
	delete(c.reqs, f.cnt)
	if r.arm != nil && f.dst == dstLLMAC && f.cmd == llmacAck && len(f.payload) > 0 && f.payload[0] == llmacStatusOK {
		r.arm.armed = true
	}
	r.ch <- f // buffered, written once
	return true
}

func (c *DualConn) dispatch(f frame) {
	if tag, ok := parseTag(f); ok {
		if c.deliverReq(f) {
			return // IDENTIFY reply
		}
		c.mu.Lock()
		entered := c.entered
		late := c.lateIdentify[f.cnt] && f.dst == dstDual
		delete(c.lateIdentify, f.cnt)
		c.mu.Unlock()
		if late {
			log.Printf("uartgw: ignoring late IDENTIFY reply %q", tag)
			return
		}
		if entered {
			c.fail(fmt.Errorf("%w (announced %q)", ErrModuleReset, tag))
			return
		}
		select {
		case c.announce <- tag:
		default:
			log.Printf("uartgw: dropping announcement %q", tag)
		}
		return
	}

	switch {
	case f.dst == dstLLMAC && f.cmd == llmacRecv:
		c.handleRecv(f)

	case f.dst == dstLLMAC && f.cmd == llmacAck:
		if c.deliverReq(f) {
			return
		}
		c.mu.Lock()
		_, ok := c.oneshots[f.cnt]
		delete(c.oneshots, f.cnt)
		if ok {
			c.ackTimeouts = 0
		}
		c.mu.Unlock()
		if !ok {
			staleAcks.Inc()
			if c.debug {
				log.Printf("uartgw: dropping LLMAC_ACK with unknown counter: %v", f)
			}
			return
		}
		if len(f.payload) < 1 || f.payload[0] != llmacStatusOK {
			dualEvents.WithLabelValues("oneshot_failed").Inc()
			log.Printf("uartgw: module did not transmit our ACK/challenge: %v", f)
		}

	case f.dst == dstHMIP, f.dst == dstHMIPCrypto:
		// Homematic IP traffic; not used for BidCoS, only passed on.
		dualEvents.WithLabelValues("hmip_ignored").Inc()
		if c.hmip != nil {
			select {
			case c.hmip <- f:
			default:
				dualEvents.WithLabelValues("hmip_dropped").Inc()
			}
		}

	case f.dst == dstApp && f.cmd == trxUnsolicited:
		if c.debug {
			log.Printf("uartgw: unsolicited TRX frame (meaning unknown): %v", f)
		}

	case f.dst == dstApp && f.cmd != trxReply:
		// Only replies (cmd 0x04) answer our requests; the counter of
		// other module-initiated frames may collide with ours.
		log.Printf("uartgw: ignoring unexpected frame: %v", f)

	default:
		if c.deliverReq(f) {
			return
		}
		log.Printf("uartgw: ignoring unexpected frame: %v", f)
	}
}

// hmipLoop passes Homematic IP frames to h until the connection ends.
func (c *DualConn) hmipLoop(h func(dst, cnt byte, packet []byte)) {
	for {
		select {
		case f := <-c.hmip:
			h(f.dst, f.cnt, append([]byte{f.cmd}, f.payload...))
		case <-c.done:
			return
		}
	}
}

// recvFrame builds a receive frame in the classic APP_RECV format.
func recvFrame(status, info, rssi byte, msg []byte) []byte {
	return append([]byte{status, info, rssi}, msg...)
}

// deliverLocked delivers b on Recv. c.mu must be held.
func (c *DualConn) deliverLocked(b []byte) {
	if c.err != nil {
		return
	}
	select {
	case c.recv <- b:
	default:
		recvDropped.Inc()
		log.Printf("uartgw: consumer too slow, dropping received frame %x", b)
	}
}

// aesExempt lists commands which are never challenged. Conservative:
// per-channel rules are only applied where the channel is unambiguous
// (aesChannelOf); any other BiDi message of a peer with AES channels is
// challenged.
func aesExempt(cmd byte) bool {
	switch cmd {
	case bidcosCmdDeviceInfo, bidcosCmdAck, bidcosCmdAESResponse, bidcosCmdTime:
		return true
	}
	return false
}

// aesChannelOf returns the channel a message reports on, for the
// commands where it is unambiguous: KEY_EVENT (0x40, remotes and push
// buttons) and SENSOR_EVENT (0x41, contacts) carry it in the low 6 bits
// of the first payload byte (eQ-3 rftypes: channelIndex 9:0.6).
func aesChannelOf(cmd byte, payload []byte) (byte, bool) {
	if (cmd == 0x40 || cmd == 0x41) && len(payload) > 0 {
		return payload[0] & 0x3f, true
	}
	return 0, false
}

// aesRequired reports whether a BiDi message from peer must be
// challenged: always, unless its channel is known (aesChannelOf) and
// not among the peer's AES channels (e.g. a remote with AES on some
// buttons only; the radio modules only challenge AES channels too).
func aesRequired(peer Peer, cmd byte, payload []byte) bool {
	if len(peer.AESChannels) == 0 || aesExempt(cmd) {
		return false
	}
	ch, ok := aesChannelOf(cmd, payload)
	if !ok {
		return true
	}
	for _, c := range peer.AESChannels {
		if c == ch {
			return true
		}
	}
	return false
}

// ackExempt lists commands which are not acknowledged: ACKs, time
// requests, which are answered with the time instead (capture, lines
// 1322-1324: no ACK, only the 0x3f answer), and AES answers, which are
// only acknowledged after a successful verification (finishAES).
func ackExempt(cmd byte) bool {
	return cmd == bidcosCmdAck || cmd == bidcosCmdTime || cmd == bidcosCmdAESResponse
}

// sameMsg reports whether a and b are the same message, ignoring the
// “repeated” flag (0x40) repeaters may set.
func sameMsg(a, b []byte) bool {
	if len(a) != len(b) || len(a) < 2 {
		return false
	}
	return a[0] == b[0] && a[1]&^0x40 == b[1]&^0x40 && bytes.Equal(a[2:], b[2:])
}

// challengeMsg returns our AES challenge for h (a message from dev):
// cnt flags 0x02 us dev 04 challenge(6) keyIndex*2 (capture line 694:
// key index 2 → 04).
func (c *DualConn) challengeMsg(dev [3]byte, h *heldMsg) []byte {
	ch := []byte{h.msg[0], challengeFlags, bidcosCmdAck}
	ch = append(ch, c.hmid[:]...)
	ch = append(ch, dev[:]...)
	ch = append(ch, bidcosAckChallenge)
	ch = append(ch, h.challenge[:]...)
	return append(ch, h.keyIndex*2)
}

// challengeAllowedLocked applies the per-peer challenge rate limit and
// records a challenge if allowed. c.mu must be held.
func (c *DualConn) challengeAllowedLocked(dev [3]byte, now time.Time) bool {
	ts := c.challenges[dev]
	n := 0
	for _, t := range ts {
		if now.Sub(t) < challengeRateWindow {
			ts[n] = t
			n++
		}
	}
	ts = ts[:n]
	if len(ts) >= challengeRateLimit {
		c.challenges[dev] = ts
		return false
	}
	c.challenges[dev] = append(ts, now)
	return true
}

func (c *DualConn) handleRecv(f frame) {
	// LLMAC_RECV payload after the command: ts_hi ts_lo info rssi msg…
	// (from RFNETHM route_llmac_recv_to_fhem; capture)
	p := f.payload
	if len(p) < 4+minMsgLen {
		log.Printf("uartgw: dropping short LLMAC_RECV: %v", f)
		return
	}
	ts := uint16(p[0])<<8 | uint16(p[1])
	info, rssi := p[2], p[3]
	msg := append([]byte(nil), p[4:]...)
	msgcnt, flags, cmd := msg[0], msg[1], msg[2]
	var src, dst [3]byte
	copy(src[:], msg[3:6])
	copy(dst[:], msg[6:9])
	toUs := dst == c.hmid
	bidi := flags&bidcosFlagBiDi != 0
	k := aesKey{src, msgcnt}

	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	now := c.now()
	c.clockTs, c.clockAt, c.clockValid = ts&(modulePeriod-1), now, true
	if toUs {
		if len(c.seen) >= 32 {
			for k, s := range c.seen {
				if now.Sub(s.at) > replyWindow {
					delete(c.seen, k)
				}
			}
		}
		c.seen[k] = rxSeen{ts: ts, at: now}
	}
	peer, isPeer := c.peers[src]
	wantAck := c.softAck && toUs && bidi && isPeer && !ackExempt(cmd)

	// 1. Answer to our AES challenge. A device only sends cmd 0x03 when
	// we challenged it, so it is never a response to Send (whose
	// message counter may collide with a held message's).
	if toUs && cmd == bidcosCmdAESResponse {
		if h := c.held[k]; h != nil {
			delete(c.held, k)
			h.timer.Stop()
			c.mu.Unlock()
			c.finishAES(src, h, msg[minMsgLen:], ts)
			return
		}
		if a, ok := c.authedLocked(k, now); ok {
			// We verified this answer already, but the device missed our
			// final ACK: acknowledge it again.
			c.mu.Unlock()
			dualEvents.WithLabelValues("aes_duplicate").Inc()
			c.sendReply(c.ackMsg(msgcnt, src, a.auth), ts, "software_ack")
			return
		}
		c.mu.Unlock()
		dualEvents.WithLabelValues("aes_stray_answer").Inc()
		if c.debug {
			log.Printf("uartgw: dropping AES answer without a pending challenge: %x", msg)
		}
		return
	}

	// 2. Response to the packet Send is transmitting (only after the
	// module transmitted it): consumed by Send.
	if w := c.resp; w != nil && w.armed && src == w.dev && dst == w.us && msgcnt == w.msgcnt {
		select {
		case w.ch <- rxMsg{frame: recvFrame(recvResp, info, rssi, msg), ts: ts}:
		default:
		}
		c.mu.Unlock()
		// e.g. a BiDi config read response (INFO 0x10) needs our ACK;
		// it is not challenged (Send's caller asked for it).
		if wantAck {
			c.sendReply(c.ackMsg(msgcnt, src, nil), ts, "software_ack")
		}
		return
	}

	// 3. Software AES: challenge before accepting the message.
	if c.softAES && toUs && bidi && isPeer && aesRequired(peer, cmd, msg[minMsgLen:]) {
		if h := c.held[k]; h != nil {
			// A retransmission: the device did not get our challenge (or
			// its answer was lost). Challenge again, with the same
			// challenge, and extend the hold (bounded).
			if c.signer == nil || h.resends >= maxChallengeResends || !h.timer.Stop() {
				c.mu.Unlock()
				if c.debug {
					log.Printf("uartgw: ignoring retransmission while challenging: %x", msg)
				}
				return
			}
			h.resends++
			h.timer = time.AfterFunc(c.aesHoldTimeout, func() { c.aesTimeout(k, h) })
			ch := c.challengeMsg(src, h)
			c.mu.Unlock()
			c.sendReply(ch, ts, "aes_challenge_resent")
			return
		}
		if a, ok := c.authedLocked(k, now); ok && sameMsg(a.msg, msg) {
			// Authenticated and delivered already; the device missed our
			// final ACK.
			c.mu.Unlock()
			dualEvents.WithLabelValues("aes_duplicate").Inc()
			c.sendReply(c.ackMsg(msgcnt, src, a.auth), ts, "software_ack")
			return
		}
		if c.signer == nil {
			if !c.signerWarned {
				c.signerWarned = true
				log.Printf("uartgw: AES peer %X: no AES signer configured, treating its messages as AES failed", src)
			}
			dualEvents.WithLabelValues("aes_failed").Inc()
			c.deliverLocked(recvFrame(recvRespWithAESKO, info, rssi, msg))
			c.mu.Unlock()
			return
		}
		if !c.challengeAllowedLocked(src, now) {
			c.mu.Unlock()
			dualEvents.WithLabelValues("aes_challenge_rate_limited").Inc()
			if c.debug {
				log.Printf("uartgw: AES challenge rate limit for %X reached, dropping %x", src, msg)
			}
			return
		}
		h := &heldMsg{info: info, rssi: rssi, msg: msg, keyIndex: peer.KeyIndex}
		if _, err := io.ReadFull(c.rand, h.challenge[:]); err != nil {
			log.Printf("uartgw: generating AES challenge: %v", err)
			c.deliverLocked(recvFrame(recvRespWithAESKO, info, rssi, msg))
			c.mu.Unlock()
			return
		}
		c.held[k] = h
		h.timer = time.AfterFunc(c.aesHoldTimeout, func() { c.aesTimeout(k, h) })
		ch := c.challengeMsg(src, h)
		c.mu.Unlock()
		c.sendReply(ch, ts, "aes_challenge_sent")
		return
	}

	// 4. Everything else.
	c.deliverLocked(recvFrame(recvResp, info, rssi, msg))
	c.mu.Unlock()
	if wantAck {
		c.sendReply(c.ackMsg(msgcnt, src, nil), ts, "software_ack")
	}
}

// aesDoneWindow is how long an authenticated message is remembered for
// re-acknowledging retransmissions.
func (c *DualConn) aesDoneWindow() time.Duration { return 2 * c.aesHoldTimeout }

// authedLocked returns the recently authenticated message k. c.mu must
// be held.
func (c *DualConn) authedLocked(k aesKey, now time.Time) (aesDone, bool) {
	a, ok := c.authed[k]
	if !ok || now.Sub(a.at) > c.aesDoneWindow() {
		return aesDone{}, false
	}
	return a, true
}

// finishAES verifies the device's answer (received at ts) to our
// challenge.
func (c *DualConn) finishAES(src [3]byte, h *heldMsg, payload []byte, ts uint16) {
	var ok bool
	var auth []byte
	if len(payload) >= 16 {
		var r [16]byte
		copy(r[:], payload)
		if v, isAuth := c.signer.(AESAuthVerifier); isAuth {
			var a [4]byte
			a, ok = v.VerifyAuth(h.keyIndex, h.challenge, h.msg, r)
			auth = a[:]
		} else {
			ok = c.signer.Verify(h.keyIndex, h.challenge, h.msg, r)
		}
	}
	status := byte(recvRespWithAESKO)
	if ok {
		status = recvRespWithAESOK
		c.sendReply(c.ackMsg(h.msg[0], src, auth), ts, "software_ack")
		dualEvents.WithLabelValues("aes_ok").Inc()
	} else {
		dualEvents.WithLabelValues("aes_failed").Inc()
		log.Printf("uartgw: AES authentication of %X failed (message %x)", src, h.msg)
	}
	c.mu.Lock()
	if ok {
		now := c.now()
		if len(c.authed) >= 32 {
			for k, a := range c.authed {
				if now.Sub(a.at) > c.aesDoneWindow() {
					delete(c.authed, k)
				}
			}
		}
		c.authed[aesKey{src, h.msg[0]}] = aesDone{msg: h.msg, auth: auth, at: now}
	}
	c.deliverLocked(recvFrame(status, h.info, h.rssi, h.msg))
	c.mu.Unlock()
}

func (c *DualConn) aesTimeout(k aesKey, h *heldMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held[k] != h {
		return
	}
	delete(c.held, k)
	dualEvents.WithLabelValues("aes_timeout").Inc()
	log.Printf("uartgw: %X did not answer our AES challenge (message %x)", k.src, h.msg)
	c.deliverLocked(recvFrame(recvRespWithAESKO, h.info, h.rssi, h.msg))
}

// ---- initialization ----

// init brings the module into the application and reads its identity
// like multimacd does at its start (restart capture lines 20-43):
// IDENTIFY → [CHANGE_APP if in the bootloader] → APP 02 (firmware) →
// COMMON 04 (SGTIN) → LLMAC 07 (serial) → LLMAC 02 (clock). Nothing
// else is configured: no address, peers or keys (the module receives
// everything, frames arrive before rfd even starts, lines 44-57).
func (c *DualConn) init(cfg Config, d detection) error {
	tag := d.tag
	if tag == "" {
		// Detected via DUAL_ERR/LLMAC frames only: ask.
		t, err := c.identify()
		if err != nil {
			return fmt.Errorf("uartgw: identifying the dual-coprocessor firmware: %w", err)
		}
		tag = t
	}
	if tagKind(tag) != KindRPIRFMod {
		return fmt.Errorf("%w: it announced %q", ErrUnknownModule, tag)
	}
	if isBootloaderTag(tag) {
		t, err := c.changeApp()
		if err != nil {
			return fmt.Errorf("uartgw: switching from bootloader to application: %w", err)
		}
		tag = t
	} else if !isAppTag(tag) {
		return fmt.Errorf("%w: unknown application %q", ErrUnknownModule, tag)
	}
	// A running application is not rebooted (unlike Conn): the module
	// keeps no BidCoS state, and multimacd does not reboot it either
	// (restart capture line 20: IDENTIFY answered with DualCoPro_App,
	// no CHANGE_APP).
	c.tag = tag

	c.mu.Lock()
	c.entered = true
	c.mu.Unlock()

	// Required: firmware version and module clock (they also prove the
	// application answers).
	if err := c.queryFirmware(); err != nil {
		return fmt.Errorf("uartgw: getting the firmware version: %w", err)
	}
	// Optional: SGTIN and serial number.
	if err := c.querySGTIN(); err != nil {
		if c.Err() != nil {
			return c.Err()
		}
		log.Printf("uartgw: getting the SGTIN: %v", err)
	}
	if err := c.querySerial(); err != nil {
		if c.Err() != nil {
			return c.Err()
		}
		log.Printf("uartgw: getting the serial number: %v", err)
	}
	if err := c.queryClock(); err != nil {
		return fmt.Errorf("uartgw: getting the module clock: %w", err)
	}
	log.Printf("uartgw: RPI-RF-MOD %s, firmware %s, serial %s, SGTIN %s", c.tag, c.firmware, c.SerialNumber(), c.sgtin)

	// Config.DualInit stays as a hook; nothing else is needed for
	// receiving and sending.
	for _, s := range cfg.DualInit {
		if err := c.runStep(s); err != nil {
			return err
		}
	}
	return nil
}

// query performs one of the startup queries with a short timeout.
func (c *DualConn) query(dst byte, cmd byte) (frame, error) {
	timeout := c.timeout
	if timeout > time.Second {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Second)
	defer cancel()
	return c.request(ctx, dst, []byte{cmd}, timeout)
}

// queryFirmware: APP 02 → [0x04 0x01 major minor patch …].
func (c *DualConn) queryFirmware() error {
	f, err := c.query(dstApp, trxFirmware)
	if err != nil {
		return err
	}
	if f.cmd != trxReply || len(f.payload) < 4 || f.payload[0] != 0x01 {
		return fmt.Errorf("unexpected reply %v", f)
	}
	fw := fmt.Sprintf("%d.%d.%d", f.payload[1], f.payload[2], f.payload[3])
	if c.debug {
		// The remaining bytes (01 00 01 01 42 00 on 4.4.22) are unknown.
		log.Printf("uartgw: firmware %s, further version data %x", fw, f.payload[4:])
	}
	c.firmware = fw
	if c.cfgFirmware != "" {
		c.firmware = c.cfgFirmware
	}
	return nil
}

// querySGTIN: COMMON 04 → [0x05 0x01 sgtin(12)].
func (c *DualConn) querySGTIN() error {
	f, err := c.query(dstDual, commonSGTIN)
	if err != nil {
		return err
	}
	if f.cmd != commonReply || len(f.payload) < 2 || f.payload[0] != 0x01 {
		return fmt.Errorf("unexpected reply %v", f)
	}
	c.sgtin = fmt.Sprintf("%X", f.payload[1:])
	return nil
}

// llmacQuery performs an LLMAC query, returning the data after the
// LLMAC_ACK status.
func (c *DualConn) llmacQuery(cmd byte) ([]byte, error) {
	f, err := c.query(dstLLMAC, cmd)
	if err != nil {
		return nil, err
	}
	if f.cmd != llmacAck || len(f.payload) < 1 || f.payload[0] != llmacStatusOK {
		return nil, fmt.Errorf("unexpected reply %v", f)
	}
	return f.payload[1:], nil
}

// querySerial: LLMAC 07 → [0x01 0x01 serial…].
func (c *DualConn) querySerial() error {
	b, err := c.llmacQuery(llmacGetSerial)
	if err != nil {
		return err
	}
	c.serial = string(b)
	return nil
}

// queryClock: LLMAC 02 → [0x01 0x01 t_hi t_lo], the module's 15-bit ms
// clock (verified against LLMAC_RECV timestamps). It initializes the
// clock estimate before any frame was received, and refreshes it when
// no radio traffic arrives.
func (c *DualConn) queryClock() error {
	b, err := c.llmacQuery(llmacGetClock)
	if err != nil {
		return err
	}
	if len(b) < 2 {
		return fmt.Errorf("short clock reply %x", b)
	}
	ts := (uint16(b[0])<<8 | uint16(b[1])) & (modulePeriod - 1)
	c.mu.Lock()
	c.clockTs, c.clockAt, c.clockValid = ts, c.now(), true
	c.mu.Unlock()
	return nil
}

func (c *DualConn) runStep(s InitStep) error {
	if len(s.Payload) == 0 {
		return fmt.Errorf("uartgw: init step %q: empty payload", s.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout+time.Second)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	if !s.ExpectAck {
		c.mu.Lock()
		cnt, err := c.nextCnt()
		c.mu.Unlock()
		if err != nil {
			return fmt.Errorf("uartgw: init step %q: %w", s.Name, err)
		}
		return c.enqueue(ctx, frame{dst: s.Dst, cnt: cnt, cmd: s.Payload[0], payload: s.Payload[1:]}, false)
	}
	f, err := c.request(ctx, s.Dst, s.Payload, c.timeout)
	if err != nil {
		return fmt.Errorf("uartgw: init step %q: %w", s.Name, err)
	}
	if s.Check != nil {
		if err := s.Check(append([]byte{f.cmd}, f.payload...)); err != nil {
			return fmt.Errorf("uartgw: init step %q: %w", s.Name, err)
		}
	}
	return nil
}

// identify sends IDENTIFY and returns the reported tag. RFNETHM treats
// any tag frame as the answer, so an announcement is accepted, too.
func (c *DualConn) identify() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout+time.Second)
	defer cancel()
	c.mu.Lock()
	cnt, err := c.nextCnt()
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	r := &dualReq{dst: dstDual, sent: time.Now(), ch: make(chan frame, 1)}
	c.reqs[cnt] = r
	c.mu.Unlock()
	abandon := func() {
		c.mu.Lock()
		if c.reqs[cnt] == r {
			delete(c.reqs, cnt)
			c.lateIdentify[cnt] = true
		}
		c.mu.Unlock()
	}
	if err := c.enqueue(ctx, frame{dst: dstDual, cnt: cnt, cmd: commonIdentify}, false); err != nil {
		abandon()
		return "", err
	}
	t := time.NewTimer(c.timeout)
	defer t.Stop()
	select {
	case f := <-r.ch:
		if tag, ok := parseTag(f); ok {
			return tag, nil
		}
		return "", fmt.Errorf("unexpected IDENTIFY reply: %v", f)
	case tag := <-c.announce:
		abandon()
		return tag, nil
	case <-t.C:
		abandon()
		return "", fmt.Errorf("%w: no IDENTIFY reply within %v", ErrTimeout, c.timeout)
	case <-c.done:
		return "", c.Err()
	}
}

// changeApp switches from the bootloader to the application (COMMON
// CHANGE_APP, from RFNETHM boot_probe) and waits for its announcement.
// If the module announces the bootloader again (e.g. it had started the
// application on its own and CHANGE_APP toggled back), it retries.
func (c *DualConn) changeApp() (string, error) {
	for drained := false; !drained; {
		select {
		case tag := <-c.announce:
			log.Printf("uartgw: discarding stale announcement %q", tag)
		default:
			drained = true
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		c.mu.Lock()
		cnt, err := c.nextCnt()
		if err != nil {
			c.mu.Unlock()
			return "", err
		}
		// Register the request so that a possible reply (unknown
		// format) is swallowed instead of logged as unexpected.
		c.reqs[cnt] = &dualReq{dst: dstDual, sent: time.Now(), ch: make(chan frame, 1)}
		c.mu.Unlock()
		err = c.enqueue(context.Background(), frame{dst: dstDual, cnt: cnt, cmd: commonChangeApp}, false)
		if err != nil {
			return "", err
		}
		t := time.NewTimer(c.changeAppWait)
		var tag string
		select {
		case tag = <-c.announce:
		case <-t.C:
		case <-c.done:
			t.Stop()
			return "", c.Err()
		}
		t.Stop()
		c.mu.Lock()
		delete(c.reqs, cnt)
		c.mu.Unlock()
		if tag == "" {
			// No announcement: maybe we missed it. Ask.
			var err error
			tag, err = c.identify()
			if err != nil {
				return "", fmt.Errorf("%w: no application announcement within %v and %v", ErrTimeout, c.changeAppWait, err)
			}
		}
		if isAppTag(tag) {
			return tag, nil
		}
		log.Printf("uartgw: module announced %q after CHANGE_APP, retrying", tag)
	}
	return "", errors.New("uartgw: the module does not leave its bootloader")
}

// ---- requests ----

func (c *DualConn) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

func (c *DualConn) release() { <-c.sem }

// AddPeer records p: software ACKs and AES (Config.SoftwareAck,
// Config.SoftwareAES) only apply to registered peers. The module itself
// needs no peer configuration (capture); Config.DualPeerInit may send
// requests anyway.
func (c *DualConn) AddPeer(ctx context.Context, p Peer) error {
	if err := c.Err(); err != nil {
		return err
	}
	cp := p
	cp.AESChannels = append([]byte(nil), p.AESChannels...)
	c.mu.Lock()
	c.peers[p.Addr] = cp
	c.mu.Unlock()
	if c.peerInit == nil {
		return nil
	}
	for _, s := range c.peerInit(cp) {
		if err := c.runStep(s); err != nil {
			return fmt.Errorf("uartgw: adding peer %X: %w", p.Addr, err)
		}
	}
	return nil
}

// SetTime does nothing: the dual-coprocessor firmware has no clock to
// set (the host answers the devices' time requests, capture lines
// 1322-1324; software AES does not need it).
func (c *DualConn) SetTime(ctx context.Context, t time.Time) error {
	return c.Err()
}

func isChallenge(resp []byte) bool {
	// status info rssi | cnt flags cmd src dst | 04 c(6) k
	return len(resp) >= 3+minMsgLen+8 && resp[3+2] == bidcosCmdAck && resp[3+minMsgLen] == bidcosAckChallenge
}

// Send implements bidcos.Transport: it transmits encoded (as produced
// by bidcos.Packet.Encode: status info burst cnt flags cmd src dst
// payload) via LLMAC_SEND and waits for the module's LLMAC_ACK. Burst
// packets (flag 0x10) are sent with the module's burst mode (preamble).
//
// A packet without BiDi which answers a message the device sent us
// within the last second (same address, same message counter; e.g. a
// time answer) is scheduled like OpenCCU does: 100ms after that
// message on the module clock. Everything else is sent at once.
//
// For BiDi packets, Send then waits Config.ResponseTimeout for the
// device's response (src/dst swapped, same message counter) and
// retransmits up to Config.SendRetries times before failing with
// ErrNoResponse. Only frames received after the module's LLMAC_ACK
// count as the response. The response is returned as a receive frame
// (status 0x01) and not delivered on Recv. If the device answers with an
// AES challenge, Send answers it with Config.Signer and returns the
// final ACK with status 0x02, or ErrAESFailed (NACK, or wrong/missing
// auth bytes, see AESAuthResponder); a final response other than an ACK
// cannot be authenticated and is returned with status 0x01.
func (c *DualConn) Send(ctx context.Context, encoded []byte) ([]byte, error) {
	if len(encoded) < 3+minMsgLen {
		return nil, fmt.Errorf("uartgw: packet too short: %x", encoded)
	}
	msg := append([]byte(nil), encoded[3:]...) // drop status, info, burst
	bidi := msg[1]&bidcosFlagBiDi != 0

	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()

	opts := sendOpts(sendNow, msg[1])
	if !bidi {
		var dev [3]byte
		copy(dev[:], msg[6:9])
		c.mu.Lock()
		if s, ok := c.seen[aesKey{dev, msg[0]}]; ok && c.now().Sub(s.at) <= replyWindow {
			opts = c.replyOptsLocked(s.ts, msg[1])
		}
		c.mu.Unlock()
	}

	var w *respWaiter
	if bidi {
		w = &respWaiter{msgcnt: msg[0], ch: make(chan rxMsg, 8)}
		copy(w.us[:], msg[3:6])
		copy(w.dev[:], msg[6:9])
		c.mu.Lock()
		c.resp = w
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			if c.resp == w {
				c.resp = nil
			}
			c.mu.Unlock()
		}()
	}

	resp, err := c.transmit(ctx, msg, opts, w, nil)
	if err != nil {
		countSendError(err)
		return nil, err
	}
	if resp.frame == nil || !isChallenge(resp.frame) {
		return resp.frame, nil
	}
	final, err := c.answerChallenge(ctx, msg, resp, w)
	if err != nil {
		countSendError(err)
	}
	return final, err
}

func countSendError(err error) {
	switch {
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		sendErrors.WithLabelValues("timeout").Inc()
	case errors.Is(err, ErrNoResponse):
		sendErrors.WithLabelValues("no_response").Inc()
	case errors.Is(err, ErrAESFailed):
		sendErrors.WithLabelValues("aes_failed").Inc()
	case errors.Is(err, ErrNACK):
		sendErrors.WithLabelValues("nack").Inc()
	default:
		sendErrors.WithLabelValues("other").Inc()
	}
}

// responseWait returns how long to wait for a response to msg, whose
// send request was written at sent and confirmed (LLMAC_ACK) at acked.
// Like OpenCCU, the timeout of a normal message sent at once counts from
// the send request, but never ends sooner than minWaitAfterAck after a
// late LLMAC_ACK. A burst message's LLMAC_ACK arrives only after the
// ~360ms wake-up preamble, and a scheduled message (e.g. our AES answer,
// replyDelay after the challenge) is only transmitted at its time, so
// their timeouts count from the LLMAC_ACK.
func (c *DualConn) responseWait(msg []byte, scheduled bool, sent, acked time.Time) time.Duration {
	if scheduled || (len(msg) > 1 && msg[1]&0x10 != 0) { // BURST
		return c.responseTimeout
	}
	d := c.responseTimeout - acked.Sub(sent)
	if d < minWaitAfterAck {
		d = minWaitAfterAck
	}
	return d
}

// transmit sends msg (the first time with opts, retransmissions at
// once) and, if w is non-nil, waits for a response accepted by accept
// (nil: any), retransmitting msg (same message counter) on timeout.
func (c *DualConn) transmit(ctx context.Context, msg []byte, opts [3]byte, w *respWaiter, accept func([]byte) bool) (rxMsg, error) {
	attempts := 1
	if w != nil {
		attempts += c.retries
	}
	if w != nil {
		// Responses to a previous message (e.g. the challenge we are
		// answering) do not count before this one was transmitted.
		c.mu.Lock()
		w.armed = false
		c.mu.Unlock()
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			dualEvents.WithLabelValues("retransmit").Inc()
			opts = sendOpts(sendNow, msg[1])
		}
		scheduled := opts[0]&0x80 == 0
		sent := time.Now()
		if err := c.llmacSend(ctx, msg, opts, w); err != nil {
			return rxMsg{}, err
		}
		if w == nil {
			return rxMsg{}, nil
		}
		t := time.NewTimer(c.responseWait(msg, scheduled, sent, time.Now()))
	wait:
		for {
			select {
			case r := <-w.ch:
				if accept == nil || accept(r.frame) {
					t.Stop()
					return r, nil
				}
			case <-t.C:
				dualEvents.WithLabelValues("response_timeout").Inc()
				break wait
			case <-ctx.Done():
				t.Stop()
				return rxMsg{}, ctx.Err()
			case <-c.done:
				t.Stop()
				return rxMsg{}, c.Err()
			}
		}
	}
	return rxMsg{}, fmt.Errorf("%w (%d attempts)", ErrNoResponse, attempts)
}

// answerChallenge answers a device's AES challenge (chal) to msg and
// waits for the device's final response. With an AESAuthResponder
// signer, a final ACK must carry the expected auth bytes (see
// AESAuthResponder).
func (c *DualConn) answerChallenge(ctx context.Context, msg []byte, chal rxMsg, w *respWaiter) ([]byte, error) {
	dualEvents.WithLabelValues("aes_challenge_received").Inc()
	if c.signer == nil {
		c.mu.Lock()
		warn := !c.signerWarned
		c.signerWarned = true
		c.mu.Unlock()
		if warn {
			log.Printf("uartgw: device %X sent an AES challenge, but no AES signer is configured", w.dev)
		}
		return nil, fmt.Errorf("%w: no AES signer configured", ErrAESFailed)
	}
	p := chal.frame[3+minMsgLen:] // 04 c(6) k
	if len(p) > 8 {
		p = p[:8]
	}
	keyIndex, challenge, ok := bidcosaes.ParseChallenge(p)
	if !ok {
		return nil, fmt.Errorf("%w: malformed AES challenge %x", ErrAESFailed, chal.frame[3+minMsgLen:])
	}
	var r [16]byte
	var wantAuth []byte
	var err error
	if ar, isAuth := c.signer.(AESAuthResponder); isAuth {
		var a [4]byte
		r, a, err = ar.RespondAuth(keyIndex, challenge, msg)
		wantAuth = a[:]
	} else {
		r, err = c.signer.Respond(keyIndex, challenge, msg)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAESFailed, err)
	}
	// cnt flags 0x03 src dst response(16), same counter as msg.
	answer := []byte{msg[0], aesAnswerFlags, bidcosCmdAESResponse}
	answer = append(answer, msg[3:9]...)
	answer = append(answer, r[:]...)
	// Scheduled like our other replies (this direction is not in the
	// capture).
	c.mu.Lock()
	opts := c.replyOptsLocked(chal.ts, answer[1])
	c.mu.Unlock()
	final, err := c.transmit(ctx, answer, opts, w, func(b []byte) bool { return !isChallenge(b) })
	if err != nil {
		if errors.Is(err, ErrNoResponse) {
			return nil, fmt.Errorf("%w: no response to our AES answer: %v", ErrAESFailed, err)
		}
		return nil, err
	}
	f := final.frame
	if f[3+2] != bidcosCmdAck {
		// Not an ACK (e.g. INFO): nothing authenticates it (FHEM only
		// checks the auth bytes of ACKs), so it is not reported as AES
		// OK.
		f[0] = recvResp
		return f, nil
	}
	pl := f[3+minMsgLen:]
	if len(pl) > 0 && pl[0] >= bidcosNACK {
		f[0] = recvRespWithAESKO
		return f, fmt.Errorf("%w: device NACKed our AES answer", ErrAESFailed)
	}
	if wantAuth != nil && (len(pl) < 1+len(wantAuth) || subtle.ConstantTimeCompare(pl[len(pl)-len(wantAuth):], wantAuth) != 1) {
		f[0] = recvRespWithAESKO
		return f, fmt.Errorf("%w: the device's ACK lacks the expected auth bytes (%x)", ErrAESFailed, pl)
	}
	f[0] = recvRespWithAESOK
	return f, nil
}

// setNowForTest replaces the local clock (tests only).
func (c *DualConn) setNowForTest(now func() time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}
