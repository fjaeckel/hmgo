// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import "fmt"

// Frame layout on the serial line (HM-MOD-RPI-PCB and RPI-RF-MOD share
// it; c.f. FHEM 00_HMUARTLGW.pm and RFNETHM hmu_frame.h):
//
//	0xFD | len (2, BE) | dst | cnt | payload… | crc16 (2, BE)
//
// len counts dst+cnt+payload. After the leading 0xFD, 0xFC and 0xFD are
// escaped as 0xFC, b&0x7F. CRC16: poly 0x8005, init 0xD77F, computed over
// the unescaped frame including the leading 0xFD.

// Frame is one decoded module frame.
type Frame struct {
	Dst     byte
	Cnt     byte
	Payload []byte
	Raw     []byte // unescaped, including 0xFD and the CRC
	CRCOK   bool
}

// destinations
const (
	dstOS      = 0x00 // HM-MOD-RPI-PCB bootloader / OS; HMSYSTEM on the dual firmware
	dstApp     = 0x01 // HM-MOD-RPI-PCB application / TRX
	dstHmIP    = 0x02 // dual firmware: Homematic IP
	dstLLMAC   = 0x03 // dual firmware: BidCoS (classic HomeMatic) MAC layer
	dstCommon  = 0xFE // dual firmware: identify / change app
	dstDualErr = 0xFF
)

func dstName(d byte) string {
	switch d {
	case dstOS:
		return "OS"
	case dstApp:
		return "APP"
	case dstHmIP:
		return "HMIP"
	case dstLLMAC:
		return "LLMAC"
	case dstCommon:
		return "COMMON"
	case dstDualErr:
		return "DUAL_ERR"
	}
	return fmt.Sprintf("dst%02X", d)
}

func crc16(data []byte, crc uint16) uint16 {
	for _, b := range data {
		crc ^= uint16(b) << 8
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

// maxFrameLen bounds the length field; longer values mean we are out of
// sync.
const maxFrameLen = 1024

// StreamDecoder reassembles frames from a byte stream that may arrive in
// arbitrary chunks (the kernel dumps 32 bytes per log line).
type StreamDecoder struct {
	buf      []byte // unescaped bytes of the current frame, from 0xFD
	inFrame  bool
	escape   bool
	Skipped  int // bytes outside of frames
	BadFrame int // frames aborted by a resync
}

// Feed adds bytes and returns the frames completed by them.
func (d *StreamDecoder) Feed(data []byte) []Frame {
	var out []Frame
	for _, b := range data {
		if b == 0xFD {
			// 0xFD never occurs inside a frame (it is escaped): start
			// of a new frame, possibly aborting an incomplete one.
			if d.inFrame && len(d.buf) > 1 {
				d.BadFrame++
			}
			d.buf = append(d.buf[:0], 0xFD)
			d.inFrame, d.escape = true, false
			continue
		}
		if !d.inFrame {
			d.Skipped++
			continue
		}
		if b == 0xFC {
			d.escape = true
			continue
		}
		if d.escape {
			b |= 0x80
			d.escape = false
		}
		d.buf = append(d.buf, b)
		if len(d.buf) >= 3 {
			n := int(d.buf[1])<<8 | int(d.buf[2])
			if n < 2 || n > maxFrameLen {
				d.BadFrame++
				d.inFrame = false
				continue
			}
			if len(d.buf) == 1+2+n+2 {
				raw := append([]byte(nil), d.buf...)
				f := Frame{
					Dst:     raw[3],
					Cnt:     raw[4],
					Payload: raw[5 : 3+n],
					Raw:     raw,
					CRCOK:   crc16(raw, 0xD77F) == 0,
				}
				out = append(out, f)
				d.inFrame = false
			}
		}
	}
	return out
}
