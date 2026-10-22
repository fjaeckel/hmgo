// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

package fakemod

import (
	"bufio"
	"errors"
	"fmt"
)

// This file deliberately does not reuse package uartgw’s codec, so that
// bugs in one implementation are caught by the other. The CRC is a
// straight port of HMUARTLGW_crc16 from FHEM’s 00_HMUARTLGW.pm.

func crc16(data []byte) uint16 {
	crc := uint16(0xd77f)
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

// Frame is a decoded HM-MOD-RPI-PCB frame.
type Frame struct {
	Dst     byte
	Cnt     byte
	Cmd     byte
	Payload []byte
}

func (f Frame) String() string {
	return fmt.Sprintf("dst=%d cnt=%d cmd=%02x payload=%x", f.Dst, f.Cnt, f.Cmd, f.Payload)
}

// Encode returns the escaped wire representation of f.
func (f Frame) Encode() []byte {
	n := 3 + len(f.Payload)
	raw := append([]byte{0xfd, byte(n >> 8), byte(n), f.Dst, f.Cnt, f.Cmd}, f.Payload...)
	crc := crc16(raw)
	raw = append(raw, byte(crc>>8), byte(crc))
	out := []byte{raw[0]}
	for _, b := range raw[1:] {
		switch b {
		case 0xfc, 0xfd:
			out = append(out, 0xfc, b&0x7f)
		default:
			out = append(out, b)
		}
	}
	return out
}

var errCRC = errors.New("fakemod: crc mismatch")

// readFrame reads one frame, skipping garbage before the delimiter.
func readFrame(br *bufio.Reader) (Frame, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return Frame{}, err
		}
		if b == 0xfd {
			break
		}
	}
	raw := []byte{0xfd}
	get := func() (byte, error) {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b == 0xfc {
			b, err = br.ReadByte()
			if err != nil {
				return 0, err
			}
			b |= 0x80
		}
		raw = append(raw, b)
		return b, nil
	}
	hi, err := get()
	if err != nil {
		return Frame{}, err
	}
	lo, err := get()
	if err != nil {
		return Frame{}, err
	}
	n := int(hi)<<8 | int(lo)
	if n < 3 {
		return Frame{}, fmt.Errorf("fakemod: short frame (len %d)", n)
	}
	for i := 0; i < n+2; i++ {
		if _, err := get(); err != nil {
			return Frame{}, err
		}
	}
	body := raw[:len(raw)-2]
	got := uint16(raw[len(raw)-2])<<8 | uint16(raw[len(raw)-1])
	if want := crc16(body); got != want {
		return Frame{}, fmt.Errorf("%w: got %04x, want %04x (frame %x)", errCRC, got, want, raw)
	}
	return Frame{
		Dst:     raw[3],
		Cnt:     raw[4],
		Cmd:     raw[5],
		Payload: append([]byte(nil), raw[6:len(raw)-2]...),
	}, nil
}
