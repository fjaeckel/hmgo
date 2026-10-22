package uartgw

// This file implements the RPI-RF-MOD (and other modules running eQ-3's
// “DualCoPro” dual-coprocessor firmware, e.g. 4.4.x) for classic
// BidCoS devices.
//
// The protocol knowledge comes from RFNETHM
// (https://github.com/tostmann/RFNETHM at 9092276, GPL-2.0-or-later;
// used as a protocol reference only, no code was copied): firmware/src/hmu_frame.h (frame
// format and destinations), source_uart.c (boot_probe, uart_frame_cb:
// banners, IDENTIFY, CHANGE_APP) and sink_hmuartlgw_legacy.c (LLMAC
// send/receive/ack translation).
//
// Frames use the same format as the classic firmware (frame.go), but
// the packet has no separate command byte: [dst cnt payload…]. As the
// first payload byte acts as a command on every destination, the frame
// type is reused with frame.cmd = first payload byte.
//
// Unlike the classic firmware, which handles ACKs and AES for
// registered peers by itself, the dual-coprocessor firmware only
// offers a BidCoS MAC layer (“LLMAC”): send a frame, receive frames.
// On a CCU, rfd implements the BidCoS logic (ACKs, AES, retries) on top
// of it in software, and so does DualConn (Config.SoftwareAck,
// Config.SoftwareAES). Everything marked ASSUMPTION below must be
// verified with a capture of OpenCCU's multimacd (docs/SNIFFING.md).

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// COMMON (dst 0xfe) commands (from RFNETHM: HMU_PL_IDENTIFY,
// HMU_PL_CHANGE_APP).
const (
	commonIdentify  = 0x01 // reply: [0x05 0x01 tag…] or [0x00 tag…]
	commonChangeApp = 0x03 // bootloader → app; announced ~2.2s later
)

// LLMAC (dst 0x03) commands (from RFNETHM sink_hmuartlgw_legacy.c).
const (
	llmacAck  = 0x01 // module → host: [0x01 status …], same frame counter as the LLMAC_SEND
	llmacRecv = 0x05 // module → host: [0x05 ts_hi ts_lo info rssi msg…]
	llmacSend = 0x06 // host → module: [0x06 opts(3) msg…]

	llmacStatusOK = 0x01 // LLMAC_ACK: transmitted
)

// llmacSendOpts are the three bytes between the LLMAC_SEND command and
// the BidCoS message. Their meaning is unknown: RFNETHM always sends
// 80 00 80 (from RFNETHM). The classic burst byte has no place in the
// LLMAC_SEND (RFNETHM drops it); ASSUMPTION — verify with capture: the
// module derives the burst preamble from the Burst flag (0x10), or one
// of these bytes controls it.
var llmacSendOpts = [3]byte{0x80, 0x00, 0x80}

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

// Flags of the frames DualConn sends on its own. ASSUMPTION — verify
// with capture (OCCU's values are not known; these follow FHEM/AskSin
// conventions): plain ACKs need no answer; challenges and our AES
// answer expect one.
const (
	ackFlags       = bidcosFlagRepeatEnable                  // 0x80
	challengeFlags = bidcosFlagRepeatEnable | bidcosFlagBiDi // 0xa0
	aesAnswerFlags = bidcosFlagRepeatEnable | bidcosFlagBiDi // 0xa0
)

// Defaults for the dual-coprocessor firmware.
const (
	defaultResponseTimeout = 1 * time.Second
	defaultSendRetries     = 2
	defaultChangeAppWait   = 3500 * time.Millisecond // RFNETHM boot_probe: 3500ms
	writeQueueSize         = 64
	maxOneshots            = 64
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

// AESAuthVerifier is optionally implemented by an AESSigner. If it is,
// the final ACK after a successful verification carries the 4 auth
// bytes (cmd 0x02 payload [0x00 auth…], c.f. FHEM aesAuthBytes /
// AESreqAck) instead of a bare 02 00.
type AESAuthVerifier interface {
	VerifyAuth(keyIndex byte, challenge [6]byte, msg []byte, response [16]byte) (auth [4]byte, ok bool)
}

// InitStep is a request sent to the dual-coprocessor firmware during
// initialization (Config.DualInit) or AddPeer (Config.DualPeerInit).
type InitStep struct {
	Name    string // for logs and errors
	Dst     byte   // destination, e.g. 0x00 OS/HMSYSTEM, 0x03 LLMAC, 0xfe COMMON
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
			Help: "RPI-RF-MOD software BidCoS events: software_ack, aes_challenge_sent, aes_ok, aes_failed, " +
				"aes_timeout, aes_challenge_received, response_timeout, retransmit, oneshot_failed.",
		},
		[]string{"event"})

	moduleInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "module_info",
			Help:      "Radio module kind and announced application (always 1).",
		},
		[]string{"kind", "app"})
)

func init() {
	prometheus.MustRegister(dualEvents, moduleInfo)
}

// dualReq is an outstanding request, matched by frame counter.
type dualReq struct {
	dst  byte
	sent time.Time
	ch   chan frame // buffered (1)
}

// respWaiter matches device responses to the packet Send transmits.
type respWaiter struct {
	dev, us [3]byte
	msgcnt  byte
	ch      chan []byte // receive frames (status info rssi msg…); buffered
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
// without ever blocking on the serial port or on a running Send.
type DualConn struct {
	rw   io.ReadWriter
	next func() (frame, error)

	hmid            [3]byte
	timeout         time.Duration
	responseTimeout time.Duration
	changeAppWait   time.Duration
	retries         int
	maxAckTimeouts  int
	softAck         bool
	softAES         bool
	signer          AESSigner
	rand            io.Reader
	debug           bool
	peerInit        func(Peer) []InitStep

	tag      string
	firmware string

	sem      chan struct{} // serializes user requests
	wq, uq   chan queued   // normal / urgent writes
	announce chan string   // tags announced before the app was entered
	done     chan struct{}

	closeOnce sync.Once

	mu           sync.Mutex
	cnt          byte
	reqs         map[byte]*dualReq
	oneshots     map[byte]time.Time // fire-and-forget LLMAC_SENDs (software ACKs, challenges)
	lateIdentify map[byte]bool      // IDENTIFY requests we gave up on
	resp         *respWaiter
	held         map[aesKey]*heldMsg
	peers        map[[3]byte]Peer
	entered      bool
	running      bool
	err          error
	recv         chan []byte
	ackTimeouts  int
	signerWarned bool
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
		closeRW(rw)
		return nil, err
	}
	if d.kind != KindRPIRFMod {
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
		retries:         cfg.SendRetries,
		maxAckTimeouts:  cfg.MaxAckTimeouts,
		softAck:         boolDefault(cfg.SoftwareAck, true),
		softAES:         boolDefault(cfg.SoftwareAES, true),
		signer:          cfg.Signer,
		rand:            cfg.Rand,
		debug:           cfg.Debug,
		peerInit:        cfg.DualPeerInit,
		firmware:        cfg.DualFirmwareVersion,
		sem:             make(chan struct{}, 1),
		wq:              make(chan queued, writeQueueSize),
		uq:              make(chan queued, writeQueueSize),
		announce:        make(chan string, 8),
		done:            make(chan struct{}),
		cnt:             detectCnt,
		reqs:            make(map[byte]*dualReq),
		oneshots:        make(map[byte]time.Time),
		// detect's IDENTIFY may be answered after an announcement.
		lateIdentify: map[byte]bool{detectCnt: true},
		held:         make(map[aesKey]*heldMsg),
		peers:        make(map[[3]byte]Peer),
		recv:         make(chan []byte, recvBufferSize),
	}
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	if c.responseTimeout <= 0 {
		c.responseTimeout = defaultResponseTimeout
	}
	if c.changeAppWait <= 0 {
		c.changeAppWait = defaultChangeAppWait
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
	if c.firmware == "" {
		c.firmware = "unknown"
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
	moduleInfo.WithLabelValues(KindRPIRFMod, c.tag).Set(1)
	if c.softAES && c.signer == nil {
		log.Printf("uartgw: no AES signer configured; messages of AES peers will fail authentication")
	}
	return c, nil
}

// FirmwareVersion returns Config.DualFirmwareVersion or "unknown": the
// firmware's version query is not known yet.
func (c *DualConn) FirmwareVersion() string { return c.firmware }

// SerialNumber returns "unknown": the firmware's serial number query is
// not known yet.
func (c *DualConn) SerialNumber() string { return "unknown" }

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

// ---- writing ----

// writeLoop writes queued frames, urgent ones (software ACKs, AES
// challenges, which devices expect within ~100ms) first.
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

// nextCnt returns an unused frame counter. Counter 0 is skipped, as
// unsolicited frames (announcements) may use it. c.mu must be held.
func (c *DualConn) nextCnt() byte {
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
		break
	}
	return c.cnt
}

// request sends payload (first byte = command) to dst and, if wait,
// waits for the reply (a frame on dst or DUAL_ERR with the same
// counter). DUAL_ERR replies are returned with an error wrapping
// ErrNACK.
func (c *DualConn) request(ctx context.Context, dst byte, payload []byte, timeout time.Duration) (frame, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return frame{}, err
	}
	cnt := c.nextCnt()
	r := &dualReq{dst: dst, sent: time.Now(), ch: make(chan frame, 1)}
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
		c.mu.Lock()
		c.ackTimeouts = 0
		c.mu.Unlock()
		if rf.dst == dstDualErr {
			return rf, fmt.Errorf("%w: module answered with DUAL_ERR (dst %s, cnt %d): %x", ErrNACK, dstName(dst), cnt, append([]byte{rf.cmd}, rf.payload...))
		}
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

// ackTimeout counts consecutive requests the module did not ack (only
// once running, like Conn) and fails with ErrModuleUnresponsive.
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
		c.fail(fmt.Errorf("%w: %d consecutive requests were not acknowledged (last: %v)", ErrModuleUnresponsive, n, err))
	}
}

func llmacSendPayload(msg []byte) []byte {
	p := make([]byte, 0, 4+len(msg))
	p = append(p, llmacSend)
	p = append(p, llmacSendOpts[:]...)
	return append(p, msg...)
}

// llmacSend transmits msg (cnt flags cmd src dst payload) and waits for
// the module's LLMAC_ACK.
func (c *DualConn) llmacSend(ctx context.Context, msg []byte) error {
	f, err := c.request(ctx, dstLLMAC, llmacSendPayload(msg), c.timeout)
	if err != nil {
		if errors.Is(err, ErrTimeout) {
			c.ackTimeout(err)
		}
		return err
	}
	// LLMAC_ACK: [0x01 status …]; in frame terms cmd=0x01, payload[0]=status.
	if f.cmd != llmacAck || len(f.payload) < 1 {
		acks.WithLabelValues("other").Inc()
		return fmt.Errorf("uartgw: unexpected reply to LLMAC_SEND: %v", f)
	}
	status := f.payload[0]
	if status != llmacStatusOK {
		// ASSUMPTION — verify with capture: which statuses exist (duty
		// cycle exhausted, CSMA/CA, …) and whether some should be
		// retried.
		acks.WithLabelValues(fmt.Sprintf("llmac_0x%02x", status)).Inc()
		return fmt.Errorf("%w: LLMAC_ACK status 0x%02x (%x)", ErrNACK, status, f.payload)
	}
	acks.WithLabelValues("llmac_ok").Inc()
	return nil
}

// sendOneshot queues an urgent LLMAC_SEND without waiting for its
// LLMAC_ACK (the reader goroutine cannot wait for frames it reads
// itself). The ack is matched and counted in dispatch.
func (c *DualConn) sendOneshot(msg []byte, what string) {
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
	cnt := c.nextCnt()
	c.oneshots[cnt] = now
	c.mu.Unlock()
	f := frame{dst: dstLLMAC, cnt: cnt, cmd: llmacSend, payload: llmacSendPayload(msg)[1:]}
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

	default:
		if c.deliverReq(f) {
			return
		}
		log.Printf("uartgw: ignoring unexpected frame: %v", f)
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
// the real per-channel rules (channel in the payload of 0x10/0x40/0x41/
// 0x5x/0x70 …) are not implemented; any other BiDi message of a peer
// with AES channels is challenged.
func aesExempt(cmd byte) bool {
	switch cmd {
	case bidcosCmdDeviceInfo, bidcosCmdAck, bidcosCmdAESResponse, bidcosCmdTime:
		return true
	}
	return false
}

func (c *DualConn) handleRecv(f frame) {
	// LLMAC_RECV payload after the command: ts_hi ts_lo info rssi msg…
	// (from RFNETHM route_llmac_recv_to_fhem)
	p := f.payload
	if len(p) < 4+minMsgLen {
		log.Printf("uartgw: dropping short LLMAC_RECV: %v", f)
		return
	}
	info, rssi := p[2], p[3]
	msg := append([]byte(nil), p[4:]...)
	msgcnt, flags, cmd := msg[0], msg[1], msg[2]
	var src, dst [3]byte
	copy(src[:], msg[3:6])
	copy(dst[:], msg[6:9])
	toUs := dst == c.hmid
	bidi := flags&bidcosFlagBiDi != 0

	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	peer, isPeer := c.peers[src]
	wantAck := c.softAck && toUs && bidi && isPeer && cmd != bidcosCmdAck

	// 1. Response to the packet Send is transmitting: consumed by Send.
	if w := c.resp; w != nil && src == w.dev && dst == w.us && msgcnt == w.msgcnt {
		select {
		case w.ch <- recvFrame(recvResp, info, rssi, msg):
		default:
		}
		c.mu.Unlock()
		// e.g. a BiDi config read response (INFO 0x10) needs our ACK;
		// it is not challenged (Send's caller asked for it).
		if wantAck && cmd != bidcosCmdAESResponse {
			c.sendOneshot(c.ackMsg(msgcnt, src, nil), "software_ack")
		}
		return
	}

	// 2. Answer to our AES challenge.
	if toUs && cmd == bidcosCmdAESResponse {
		k := aesKey{src, msgcnt}
		if h := c.held[k]; h != nil {
			delete(c.held, k)
			h.timer.Stop()
			c.mu.Unlock()
			c.finishAES(src, h, msg[minMsgLen:])
			return
		}
	}

	// 3. Software AES: challenge before accepting the message.
	if c.softAES && toUs && bidi && isPeer && len(peer.AESChannels) > 0 && !aesExempt(cmd) {
		k := aesKey{src, msgcnt}
		if _, dup := c.held[k]; dup {
			c.mu.Unlock()
			if c.debug {
				log.Printf("uartgw: ignoring retransmission while challenging: %x", msg)
			}
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
		h := &heldMsg{info: info, rssi: rssi, msg: msg, keyIndex: peer.KeyIndex}
		if _, err := io.ReadFull(c.rand, h.challenge[:]); err != nil {
			log.Printf("uartgw: generating AES challenge: %v", err)
			c.deliverLocked(recvFrame(recvRespWithAESKO, info, rssi, msg))
			c.mu.Unlock()
			return
		}
		c.held[k] = h
		h.timer = time.AfterFunc(c.responseTimeout, func() { c.aesTimeout(k, h) })
		c.mu.Unlock()

		// cnt flags 0x02 us dev 04 challenge(6) keyIndex*2
		ch := []byte{msgcnt, challengeFlags, bidcosCmdAck}
		ch = append(ch, c.hmid[:]...)
		ch = append(ch, src[:]...)
		ch = append(ch, bidcosAckChallenge)
		ch = append(ch, h.challenge[:]...)
		ch = append(ch, h.keyIndex*2)
		c.sendOneshot(ch, "aes_challenge_sent")
		return
	}

	// 4. Everything else.
	c.deliverLocked(recvFrame(recvResp, info, rssi, msg))
	c.mu.Unlock()
	if wantAck {
		c.sendOneshot(c.ackMsg(msgcnt, src, nil), "software_ack")
	}
}

// finishAES verifies the device's answer to our challenge.
func (c *DualConn) finishAES(src [3]byte, h *heldMsg, payload []byte) {
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
		c.sendOneshot(c.ackMsg(h.msg[0], src, auth), "software_ack")
		dualEvents.WithLabelValues("aes_ok").Inc()
	} else {
		dualEvents.WithLabelValues("aes_failed").Inc()
		log.Printf("uartgw: AES authentication of %X failed (message %x)", src, h.msg)
	}
	c.mu.Lock()
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
	// ASSUMPTION — verify with capture: the module keeps no BidCoS state
	// (address, peers) worth resetting when it already ran its
	// application. The classic Conn reboots into the bootloader in that
	// case; it is not known whether CHANGE_APP toggles back here.
	c.tag = tag

	// Verify (non-fatal, like RFNETHM boot_probe “Verify auf
	// COMMON-Pfad”).
	if t, err := c.identify(); err != nil {
		if c.Err() != nil {
			return c.Err()
		}
		log.Printf("uartgw: verifying the application: %v", err)
	} else if !isAppTag(t) {
		return fmt.Errorf("uartgw: module reports %q after entering the application", t)
	} else {
		c.tag = t
	}

	c.mu.Lock()
	c.entered = true
	c.mu.Unlock()

	// TODO(docs/SNIFFING.md): the BidCoS address, RX filtering, AES and
	// duty cycle configuration multimacd sends are not known yet; they
	// go into Config.DualInit once a capture shows them.
	for _, s := range cfg.DualInit {
		if err := c.runStep(s); err != nil {
			return err
		}
	}
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
		cnt := c.nextCnt()
		c.mu.Unlock()
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
	cnt := c.nextCnt()
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
		cnt := c.nextCnt()
		// Register the request so that a possible reply (unknown
		// format) is swallowed instead of logged as unexpected.
		c.reqs[cnt] = &dualReq{dst: dstDual, sent: time.Now(), ch: make(chan frame, 1)}
		c.mu.Unlock()
		err := c.enqueue(context.Background(), frame{dst: dstDual, cnt: cnt, cmd: commonChangeApp}, false)
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
// Config.SoftwareAES) only apply to registered peers. Config.DualPeerInit
// may send further requests (unknown yet, see docs/SNIFFING.md).
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

// SetTime does nothing: the classic module needs its clock for AES and
// its own time answers, but the dual-coprocessor firmware's equivalent
// (likely an OS/HMSYSTEM command) is not known, and software AES does
// not need it. TODO(docs/SNIFFING.md).
func (c *DualConn) SetTime(ctx context.Context, t time.Time) error {
	return c.Err()
}

func isChallenge(resp []byte) bool {
	// status info rssi | cnt flags cmd src dst | 04 c(6) k
	return len(resp) >= 3+minMsgLen+8 && resp[3+2] == bidcosCmdAck && resp[3+minMsgLen] == bidcosAckChallenge
}

// Send implements bidcos.Transport: it transmits encoded (as produced
// by bidcos.Packet.Encode: status info burst cnt flags cmd src dst
// payload) via LLMAC_SEND and waits for the module's LLMAC_ACK.
//
// For BiDi packets, it then waits Config.ResponseTimeout for the
// device's response (src/dst swapped, same message counter) and
// retransmits up to Config.SendRetries times before failing with
// ErrNoResponse. The response is returned as a receive frame (status
// 0x01) and not delivered on Recv. If the device answers with an AES
// challenge, Send answers it with Config.Signer and returns the final
// response with status 0x02, or ErrAESFailed.
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

	var w *respWaiter
	if bidi {
		w = &respWaiter{msgcnt: msg[0], ch: make(chan []byte, 8)}
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

	resp, err := c.transmit(ctx, msg, w, nil)
	if err != nil {
		countSendError(err)
		return nil, err
	}
	if resp == nil || !isChallenge(resp) {
		return resp, nil
	}
	resp, err = c.answerChallenge(ctx, msg, resp, w)
	if err != nil {
		countSendError(err)
	}
	return resp, err
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

// transmit sends msg and, if w is non-nil, waits for a response
// accepted by accept (nil: any), retransmitting msg (same message
// counter) on timeout.
func (c *DualConn) transmit(ctx context.Context, msg []byte, w *respWaiter, accept func([]byte) bool) ([]byte, error) {
	attempts := 1
	if w != nil {
		attempts += c.retries
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			dualEvents.WithLabelValues("retransmit").Inc()
		}
		if err := c.llmacSend(ctx, msg); err != nil {
			return nil, err
		}
		if w == nil {
			return nil, nil
		}
		t := time.NewTimer(c.responseTimeout)
	wait:
		for {
			select {
			case r := <-w.ch:
				if accept == nil || accept(r) {
					t.Stop()
					return r, nil
				}
			case <-t.C:
				dualEvents.WithLabelValues("response_timeout").Inc()
				break wait
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-c.done:
				t.Stop()
				return nil, c.Err()
			}
		}
	}
	return nil, fmt.Errorf("%w (%d attempts)", ErrNoResponse, attempts)
}

// answerChallenge answers a device's AES challenge (in chal) to msg and
// waits for the device's final response.
func (c *DualConn) answerChallenge(ctx context.Context, msg, chal []byte, w *respWaiter) ([]byte, error) {
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
	p := chal[3+minMsgLen:] // 04 c(6) k
	var challenge [6]byte
	copy(challenge[:], p[1:7])
	keyIndex := p[7] / 2
	r, err := c.signer.Respond(keyIndex, challenge, msg)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAESFailed, err)
	}
	// cnt flags 0x03 src dst response(16), same counter as msg.
	answer := []byte{msg[0], aesAnswerFlags, bidcosCmdAESResponse}
	answer = append(answer, msg[3:9]...)
	answer = append(answer, r[:]...)
	final, err := c.transmit(ctx, answer, w, func(b []byte) bool { return !isChallenge(b) })
	if err != nil {
		if errors.Is(err, ErrNoResponse) {
			return nil, fmt.Errorf("%w: no response to our AES answer: %v", ErrAESFailed, err)
		}
		return nil, err
	}
	if final[3+2] == bidcosCmdAck && len(final) > 3+minMsgLen && final[3+minMsgLen] == bidcosNACK {
		final[0] = recvRespWithAESKO
		return final, fmt.Errorf("%w: device NACKed our AES answer", ErrAESFailed)
	}
	final[0] = recvRespWithAESOK
	return final, nil
}
