// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

package fakemod

import (
	"time"
)

// Dual-coprocessor firmware (RPI-RF-MOD) emulation, see
// Options.DualCoPro. The protocol follows RFNETHM
// (https://github.com/tostmann/RFNETHM), as documented in
// internal/uartgw/dual.go; parts of it are assumptions.

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

	case f.Dst == DstLLMAC && f.Cmd == LLMACSend:
		// opts(3) msg…
		if !inApp || len(f.Payload) < 3+9 {
			return m.dualErr(f)
		}
		msg := append([]byte(nil), f.Payload[3:]...)
		m.mu.Lock()
		m.sent = append(m.sent, msg)
		m.llmacOpts = append(m.llmacOpts, append([]byte(nil), f.Payload[:3]...))
		m.mu.Unlock()
		status := byte(LLMACStatusOK)
		if m.opts.LLMACAckStatus != nil {
			status = m.opts.LLMACAckStatus(append([]byte(nil), msg...))
		}
		if status == NoAck {
			return nil
		}
		if err := m.write(Frame{Dst: DstLLMAC, Cnt: f.Cnt, Cmd: LLMACAck, Payload: []byte{status}}); err != nil {
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

// InjectLLMACRecv emulates the reception of a radio frame by the
// dual-coprocessor firmware: LLMAC_RECV [0x05 ts_hi ts_lo info rssi
// msg…], msg being the BidCoS message (cnt flags cmd src dst payload).
func (m *Module) InjectLLMACRecv(rssi byte, msg []byte) error {
	m.mu.Lock()
	m.ts += 37
	ts := m.ts
	m.mu.Unlock()
	payload := append([]byte{byte(ts >> 8), byte(ts), 0x00, rssi}, msg...)
	return m.write(Frame{Dst: DstLLMAC, Cnt: 0, Cmd: LLMACRecv, Payload: payload})
}

// LLMACOpts returns the three option bytes of every LLMAC_SEND.
func (m *Module) LLMACOpts() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([][]byte, len(m.llmacOpts))
	for i, o := range m.llmacOpts {
		res[i] = append([]byte(nil), o...)
	}
	return res
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
