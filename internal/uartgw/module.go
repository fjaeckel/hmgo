// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package uartgw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

// Module kinds, as returned by Module.Kind.
const (
	KindHMModRPIPCB = "HM-MOD-RPI-PCB" // classic firmware (“Co_CPU_App”), see Conn
	KindRPIRFMod    = "RPI-RF-MOD"     // eQ-3 dual-coprocessor firmware (“DualCoPro_App”), see DualConn
)

// Module is a radio module connection: *Conn (HM-MOD-RPI-PCB, classic
// firmware) or *DualConn (RPI-RF-MOD, dual-coprocessor firmware).
// OpenModule detects which one is attached.
type Module interface {
	// Send transmits encoded (as produced by bidcos.Packet.Encode) and,
	// for BiDi packets, returns the device's response as a receive frame
	// (status info rssi msg…; status 0x01 plain, 0x02 AES OK, 0x03 AES
	// failed), or nil.
	Send(ctx context.Context, encoded []byte) ([]byte, error)
	// Recv delivers received radio frames (status info rssi msg…,
	// decodable with bidcos.Decode). Closed when the connection fails.
	Recv() <-chan []byte
	Err() error
	AddPeer(ctx context.Context, p Peer) error
	SetTime(ctx context.Context, t time.Time) error
	Close() error
	FirmwareVersion() string
	SerialNumber() string
	// Kind returns KindHMModRPIPCB or KindRPIRFMod.
	Kind() string
}

var (
	_ Module = (*Conn)(nil)
	_ Module = (*DualConn)(nil)
)

// ErrUnknownModule is returned by OpenModule when the module announces
// an application which belongs to neither supported module family.
var ErrUnknownModule = errors.New("uartgw: unknown radio module")

// framePump reads frames from the module in its own goroutine and
// hands them out via next. It lets OpenModule look at the first frames
// (detection) and then hand the very same stream over to Conn or
// DualConn without losing buffered bytes.
type framePump struct {
	fr   *frameReader
	ch   chan pumped
	quit chan struct{} // closed by stop
	once sync.Once

	mu  sync.Mutex
	alt func() []uint16
}

type pumped struct {
	f   frame
	err error
}

func newFramePump(r io.Reader) *framePump {
	p := &framePump{fr: newFrameReader(r), ch: make(chan pumped), quit: make(chan struct{})}
	p.fr.altInits = func() []uint16 {
		p.mu.Lock()
		alt := p.alt
		p.mu.Unlock()
		if alt == nil {
			return nil
		}
		return alt()
	}
	go p.run()
	return p
}

func (p *framePump) setAltInits(fn func() []uint16) {
	p.mu.Lock()
	p.alt = fn
	p.mu.Unlock()
}

// stop makes run return instead of waiting for a reader of the next
// frame (detection failed: nobody will call next again). The read in
// progress, if any, ends when rw is closed.
func (p *framePump) stop() { p.once.Do(func() { close(p.quit) }) }

// run reads frames until a read error, which is handed out as the last
// item, or until stop. Undecodable frames are logged and skipped.
func (p *framePump) run() {
	for {
		f, err := p.fr.next()
		if err != nil && errors.Is(err, errBadFrame) {
			frameErrors.Inc()
			log.Printf("uartgw: dropping frame: %v", err)
			continue
		}
		if p.fr.skipped > 0 {
			log.Printf("uartgw: skipped %d bytes of garbage before frame", p.fr.skipped)
			p.fr.skipped = 0
		}
		select {
		case p.ch <- pumped{f: f, err: err}:
		case <-p.quit:
			return
		}
		if err != nil {
			return
		}
	}
}

// next returns the next frame. After it returned an error, it must not
// be called again.
func (p *framePump) next() (frame, error) {
	r := <-p.ch
	return r.f, r.err
}

// detectCnt is the frame counter of the IDENTIFY request sent during
// detection. It is far away from the counters the classic Conn uses
// during its initialization (1, 2, …), so that a late or unexpected
// answer of a classic module can never be mistaken for the ack of one
// of its requests.
const detectCnt = 0xa0

// detection is the result of detect.
type detection struct {
	kind   string  // KindHMModRPIPCB or KindRPIRFMod
	tag    string  // announced/identified application, "" if unknown
	banner bool    // the module announced itself (as opposed to: answered IDENTIFY, or nothing at all)
	frames []frame // other frames received during detection
}

// parseTag extracts an application tag (“Co_CPU_BL”, “HMIP_TRX_Bl”,
// “DualCoPro_App”, …) from an announcement or IDENTIFY reply. The
// formats are from RFNETHM (firmware/src/source_uart.c, uart_frame_cb):
//
//	dst OS     [0x00, tag…]        boot banner (both families)
//	dst COMMON [0x05, 0x01, tag…]  IDENTIFY reply
//	dst COMMON [0x00, tag…]        IDENTIFY reply (variant) / announcement
//
// In frame terms, the first payload byte is frame.cmd.
func parseTag(f frame) (string, bool) {
	var b []byte
	switch {
	case f.dst == dstOS && f.cmd == 0x00:
		b = f.payload
	case f.dst == dstDual && f.cmd == 0x05 && len(f.payload) >= 1 && f.payload[0] == 0x01:
		b = f.payload[1:]
	case f.dst == dstDual && f.cmd == 0x00:
		b = f.payload
	default:
		return "", false
	}
	tag := strings.TrimRight(string(b), "\x00 \r\n")
	if len(tag) < 3 {
		return "", false
	}
	for _, r := range tag {
		if r < 0x20 || r > 0x7e {
			return "", false
		}
	}
	return tag, true
}

// tagKind maps an application tag to the module kind
// (c.f. RFNETHM family_from_tag).
func tagKind(tag string) string {
	switch {
	case strings.HasPrefix(tag, "Co_CPU"):
		return KindHMModRPIPCB
	case strings.HasPrefix(tag, "HMIP_TRX"), strings.HasPrefix(tag, "DualCoPro"):
		return KindRPIRFMod
	}
	return ""
}

// isBootloaderTag reports whether tag names a bootloader (“Co_CPU_BL”,
// “HMIP_TRX_Bl”); c.f. RFNETHM classify_tag.
func isBootloaderTag(tag string) bool {
	return strings.HasSuffix(tag, "_BL") || strings.HasSuffix(tag, "_Bl")
}

// isAppTag reports whether tag names an application (“Co_CPU_App”,
// “DualCoPro_App”).
func isAppTag(tag string) bool { return strings.HasSuffix(tag, "_App") }

// classify returns the module kind a frame received during detection
// implies, or "" if it implies nothing.
func classify(f frame) (kind, tag string) {
	if tag, ok := parseTag(f); ok {
		return tagKind(tag), tag
	}
	switch {
	case f.dst == dstDual, f.dst == dstDualErr, f.dst == dstLLMAC, f.dst == dstHMIP:
		// The classic firmware never uses these destinations (c.f.
		// HMUARTLGW_Parse in FHEM: “Minimally handle
		// DualCopro-Firmware”).
		return KindRPIRFMod, ""
	}
	// Frames on dst OS/APP with cmd 0x04 imply nothing: the classic
	// firmware acks with them, but they are also the dual-coprocessor
	// firmware's APP replies (e.g. a late answer to a TRX status poll of
	// a previous session). Only the answer to our IDENTIFY (see detect)
	// or an announcement identifies a classic module.
	return "", ""
}

// detect determines the module family:
//
//  1. Wait up to announceWait for the announcement a freshly reset
//     module sends (“Co_CPU_BL” or “HMIP_TRX_Bl”).
//  2. Otherwise send IDENTIFY (dst COMMON, payload 0x01; from RFNETHM
//     boot_probe: “kein Banner — IDENTIFY explicit”), which the
//     dual-coprocessor firmware answers with its tag, and wait up to
//     identifyWait. A late announcement is accepted as well.
//  3. No answer at all: assume the classic module (which ignores
//     COMMON) and let Conn query the running application the classic
//     way.
func detect(p *framePump, w io.Writer, announceWait, identifyWait time.Duration, debug bool) (detection, error) {
	var d detection
	wait := func(timeout time.Duration) (bool, error) {
		t := time.NewTimer(timeout)
		defer t.Stop()
		for {
			select {
			case r := <-p.ch:
				if r.err != nil {
					return false, fmt.Errorf("uartgw: reading from radio module: %w", r.err)
				}
				f := r.f
				framesReceived.WithLabelValues(dstLabel(f.dst), cmdLabel(f.cmd)).Inc()
				if debug {
					log.Printf("uartgw: detect: recv %v", f)
				}
				if f.dst == dstDual && f.cnt == detectCnt {
					// The answer to our IDENTIFY. Not replayed: the
					// classic Conn would take any COMMON frame for the
					// dual-coprocessor firmware.
					if tag, ok := parseTag(f); ok {
						if d.kind = tagKind(tag); d.kind == "" {
							return false, fmt.Errorf("%w: it announced %q", ErrUnknownModule, tag)
						}
						d.tag = tag
						return true, nil
					}
					if f.cmd == osAck {
						// A classic-format ack (cmd 0x04 + status) on
						// COMMON: a classic module rejecting the unknown
						// destination. ASSUMPTION — the real classic
						// firmware may also ignore it (then detection
						// times out and assumes classic, too).
						d.kind = KindHMModRPIPCB
						return true, nil
					}
				}
				kind, tag := classify(f)
				if tag != "" && kind == "" {
					return false, fmt.Errorf("%w: it announced %q", ErrUnknownModule, tag)
				}
				if kind == "" {
					d.frames = append(d.frames, f)
					continue
				}
				d.kind, d.tag = kind, tag
				d.frames = append(d.frames, f)
				return true, nil
			case <-t.C:
				return false, nil
			}
		}
	}

	ok, err := wait(announceWait)
	if err != nil {
		return d, err
	}
	if ok {
		d.banner = d.tag != ""
		return d, nil
	}

	f := frame{dst: dstDual, cnt: detectCnt, cmd: commonIdentify}
	if debug {
		log.Printf("uartgw: detect: send %v (IDENTIFY)", f)
	}
	if _, err := w.Write(f.encode()); err != nil {
		return d, fmt.Errorf("uartgw: writing to radio module: %w", err)
	}
	framesSent.WithLabelValues(dstLabel(f.dst), cmdLabel(f.cmd)).Inc()
	ok, err = wait(identifyWait)
	if err != nil {
		return d, err
	}
	if ok {
		return d, nil
	}
	d.kind = KindHMModRPIPCB
	return d, nil
}

// OpenModule detects which radio module is attached and initializes it:
// an HM-MOD-RPI-PCB with the classic firmware (*Conn, like Open) or an
// RPI-RF-MOD with eQ-3's dual-coprocessor firmware (*DualConn, like
// OpenDual). Detection works whether the module was just reset (it
// announces its bootloader), already runs its application, or does not
// announce anything (see detect). Without an announcement, detecting a
// classic module takes up to Config.Timeout longer (the IDENTIFY query
// it ignores).
//
// If OpenModule fails and rw implements io.Closer, rw is closed.
func OpenModule(rw io.ReadWriter, cfg Config) (Module, error) {
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
	log.Printf("uartgw: detected %s (application %q)", d.kind, d.tag)
	switch d.kind {
	case KindRPIRFMod:
		c, err := openDual(rw, cfg, p, d)
		if err != nil {
			return nil, err
		}
		return c, nil
	default:
		c, err := openConn(rw, cfg, legacyOpts{
			pump:             p,
			replay:           d.frames,
			skipAnnounceWait: !d.banner,
		})
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

func closeRW(rw io.ReadWriter) error {
	if cl, ok := rw.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}
