// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package fakemod

import (
	"time"
)

// Dual-coprocessor firmware (RPI-RF-MOD) emulation, see
// Options.DualCoPro. The protocol follows RFNETHM
// (https://github.com/tostmann/RFNETHM) and a capture of OpenCCU
// talking to an RPI-RF-MOD (firmware 4.4.22), as documented in
// internal/uartgw/dual.go.
//
// The module has a 15-bit millisecond clock (Clock), which stamps
// received frames (LLMAC_RECV) and transmissions (LLMAC_ACK).
// LLMAC_SEND requests carry [t_hi t_lo mode]: the module waits until
// its clock reaches t (unless bit 15 is set: at once), and mode 0x90
// means burst (Options.BurstDelay emulates the preamble).

// Destinations of the dual-coprocessor firmware.
const (
	DstHMIP   = 0x02
	DstLLMAC  = 0x03
	DstCommon = 0xfe // same as DstDual
)

// LLMAC commands and statuses.
const (
	LLMACAck  = 0x01
	LLMACRecv = 0x05
	LLMACSend = 0x06

	LLMACStatusOK = 0x01
)

// Application tags of the dual-coprocessor firmware.
const (
	TagDualBootloader = "HMIP_TRX_Bl"
	TagDualApp        = "DualCoPro_App"
)

// LLMACSent is a BidCoS message transmitted via LLMAC_SEND.
type LLMACSent struct {
	Msg  []byte    // cnt flags cmd src dst payload
	Opts [3]byte   // t_hi t_lo mode
	Ts   uint16    // module time of the transmission (as in the LLMAC_ACK)
	At   time.Time // local time of the transmission
}

// Injected is a frame received by the emulated radio (LLMAC_RECV).
type Injected struct {
	Ts  uint16
	Msg []byte
}

// Clock returns the module's 15-bit millisecond clock.
func (m *Module) Clock() uint16 {
	ms := time.Since(m.clockStart).Milliseconds()
	return uint16((int64(m.opts.ClockOffset) + ms) & 0x7fff)
}

// diff15 returns a - b on the 15-bit clock, in (-0x4000, 0x4000].
func diff15(a, b uint16) int {
	d := (int(a) - int(b)) & 0x7fff
	if d > 0x4000 {
		d -= 0x8000
	}
	return d
}

func (m *Module) dualTag() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inApp {
		return TagDualApp
	}
	return TagDualBootloader
}

// announceDual sends an unsolicited application announcement: on dst
// COMMON as [0x00 tag…], or with Options.BannerOnOS on dst OS.
func (m *Module) announceDual(tag string) error {
	m.mu.Lock()
	m.announced++
	m.mu.Unlock()
	dst := byte(DstCommon)
	if m.opts.BannerOnOS {
		dst = DstOS
	}
	return m.write(Frame{Dst: dst, Cnt: 0, Cmd: 0x00, Payload: []byte(tag)})
}

func (m *Module) dualErr(req Frame) error {
	return m.write(Frame{Dst: DstDualErr, Cnt: req.Cnt, Cmd: 0x04, Payload: []byte{AckNACK}})
}

// sleep waits for d unless the module stops.
func (m *Module) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-time.After(d):
	case <-m.done:
	}
}

func (m *Module) handleDualCoPro(f Frame) error {
	m.mu.Lock()
	inApp := m.inApp
	m.mu.Unlock()

	switch {
	case f.Dst == DstCommon && f.Cmd == 0x01: // IDENTIFY
		m.mu.Lock()
		m.identifies++
		swallow := m.opts.IgnoreIdentify
		m.mu.Unlock()
		if swallow {
			return nil
		}
		return m.write(Frame{Dst: DstCommon, Cnt: f.Cnt, Cmd: 0x05, Payload: append([]byte{0x01}, m.dualTag()...)})

	case f.Dst == DstCommon && f.Cmd == 0x03: // CHANGE_APP (toggles, like the classic firmware)
		m.mu.Lock()
		m.inApp = !m.inApp
		m.changeApps++
		inApp = m.inApp
		m.mu.Unlock()
		tag := TagDualBootloader
		if inApp {
			tag = TagDualApp
		}
		delay := m.opts.AppSwitchDelay
		if delay == 0 {
			delay = 20 * time.Millisecond
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			select {
			case <-time.After(delay):
				m.announceDual(tag)
			case <-m.done:
			}
		}()
		return nil

	case f.Dst == DstApp && f.Cmd == 0x03 && inApp: // TRX status poll
		m.mu.Lock()
		m.trxPolls++
		noReply := m.opts.TRXNoReply
		status := m.opts.TRXStatus
		m.mu.Unlock()
		if noReply {
			return nil
		}
		if status == 0 {
			status = 0x02
		}
		return m.write(Frame{Dst: DstApp, Cnt: f.Cnt, Cmd: 0x04, Payload: []byte{0x01, status}})

	case f.Dst == DstApp && f.Cmd == 0x0a && inApp: // unknown, every 5min by multimacd
		return m.write(Frame{Dst: DstApp, Cnt: f.Cnt, Cmd: 0x04, Payload: []byte{0x01, 0x03}})

	case f.Dst == DstLLMAC && f.Cmd == LLMACSend:
		// t_hi t_lo mode msg…
		if !inApp || len(f.Payload) < 3+9 {
			return m.dualErr(f)
		}
		var opts [3]byte
		copy(opts[:], f.Payload)
		msg := append([]byte(nil), f.Payload[3:]...)
		status := byte(LLMACStatusOK)
		if m.opts.LLMACAckStatus != nil {
			status = m.opts.LLMACAckStatus(append([]byte(nil), msg...))
		}
		if status == NoAck {
			m.mu.Lock()
			m.sent = append(m.sent, msg)
			m.llmacSent = append(m.llmacSent, LLMACSent{Msg: msg, Opts: opts, Ts: m.Clock(), At: time.Now()})
			m.mu.Unlock()
			return nil
		}
		if t := uint16(opts[0])<<8 | uint16(opts[1]); t&0x8000 == 0 {
			// Scheduled transmission: wait for the module clock (a time
			// in the past is sent at once).
			if d := diff15(t, m.Clock()); d > 0 {
				m.sleep(time.Duration(d) * time.Millisecond)
			}
		}
		ts := m.Clock()
		m.mu.Lock()
		m.sent = append(m.sent, msg)
		m.llmacOpts = append(m.llmacOpts, opts[:])
		m.llmacSent = append(m.llmacSent, LLMACSent{Msg: msg, Opts: opts, Ts: ts, At: time.Now()})
		m.mu.Unlock()
		if opts[2]&0x10 != 0 {
			m.sleep(m.opts.BurstDelay)
		}
		if err := m.write(Frame{Dst: DstLLMAC, Cnt: f.Cnt, Cmd: LLMACAck, Payload: []byte{status, byte(ts >> 8), byte(ts)}}); err != nil {
			return err
		}
		if status != LLMACStatusOK {
			return nil
		}
		onSend := m.opts.OnLLMACSend
		if onSend == nil {
			onSend = DefaultOnLLMACSend
		}
		for _, resp := range onSend(append([]byte(nil), msg...)) {
			if err := m.InjectLLMACRecv(DefaultRSSI, resp); err != nil {
				return err
			}
		}
		return nil
	}
	return m.dualErr(f)
}

// DefaultOnLLMACSend is the default Options.OnLLMACSend: a device ACK
// for BiDi messages, nothing otherwise.
func DefaultOnLLMACSend(msg []byte) [][]byte {
	if len(msg) < 9 || msg[1]&0x20 == 0 {
		return nil
	}
	return [][]byte{DeviceAck(msg)}
}

// InjectLLMACRecv emulates the reception of a radio frame now: an
// LLMAC_RECV [0x05 ts_hi ts_lo info rssi msg…], msg being the BidCoS
// message (cnt flags cmd src dst payload).
func (m *Module) InjectLLMACRecv(rssi byte, msg []byte) error {
	return m.InjectLLMACRecvAt(m.Clock(), rssi, msg)
}

// InjectLLMACRecvAt is InjectLLMACRecv with an explicit module time.
func (m *Module) InjectLLMACRecvAt(ts uint16, rssi byte, msg []byte) error {
	m.mu.Lock()
	m.injected = append(m.injected, Injected{Ts: ts, Msg: append([]byte(nil), msg...)})
	m.mu.Unlock()
	payload := append([]byte{byte(ts >> 8), byte(ts), 0x00, rssi}, msg...)
	return m.write(Frame{Dst: DstLLMAC, Cnt: 0, Cmd: LLMACRecv, Payload: payload})
}

// Injected returns all frames received by the emulated radio.
func (m *Module) Injected() []Injected {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Injected(nil), m.injected...)
}

// LLMACOpts returns the three option bytes of every transmitted
// LLMAC_SEND.
func (m *Module) LLMACOpts() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([][]byte, len(m.llmacOpts))
	for i, o := range m.llmacOpts {
		res[i] = append([]byte(nil), o...)
	}
	return res
}

// LLMACSent returns all LLMAC_SEND requests with their options and
// transmission times (including ones not acknowledged because of
// Options.LLMACAckStatus = NoAck).
func (m *Module) LLMACSent() []LLMACSent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]LLMACSent(nil), m.llmacSent...)
}

// ChangeApps returns how many CHANGE_APP requests (dual-coprocessor
// firmware) the module received.
func (m *Module) ChangeApps() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changeApps
}

// Identifies returns how many IDENTIFY requests the module received.
func (m *Module) Identifies() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.identifies
}

// TRXPolls returns how many TRX status polls (APP cmd 0x03) the module
// received.
func (m *Module) TRXPolls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.trxPolls
}
