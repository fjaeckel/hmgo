// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see licenses/.

package uartgw

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/sigurn/crc16"
)

/*

Each frame has the following format:

    uint8  frame delimiter (always 0xfd)
    uint16 length (big endian), counting the packet bytes
    []byte packet
    uint16 crc (big endian)

The CRC is computed over the whole unescaped frame, starting with the
frame delimiter. See the bidcosTable variable for the CRC16 parameters.

Each packet has the following format:

    uint8  destination (see the dst constants)
    uint8  message counter
    uint8  command (see the os and app constants)
    []byte payload

Note that command values depend on the destination, i.e. the same
value means something different for the bootloader (OS) vs. the
application code (App).

*/

var bidcosTable = crc16.MakeTable(crc16.Params{
	Poly:   0x8005,
	Init:   0xd77f,
	RefIn:  false,
	RefOut: false,
	XorOut: 0x0000,
	Check:  0x0000,
	Name:   "BidCoS",
})

// maxFrameLen is a sanity limit for the length field. Real frames are
// well below 100 bytes; anything larger is line noise and we would
// rather re-synchronize than wait for kilobytes of data.
const maxFrameLen = 512

// frame is a packet exchanged with the HM-MOD-RPI-PCB.
type frame struct {
	dst     byte
	cnt     byte
	cmd     byte
	payload []byte
}

func (f frame) String() string {
	return fmt.Sprintf("dst=%s cnt=%d cmd=%02x payload=%x", dstName(f.dst), f.cnt, f.cmd, f.payload)
}

// encode returns the frame as it needs to be written to the wire
// (including frame delimiter, CRC and escaping).
func (f frame) encode() []byte {
	length := 3 + len(f.payload)
	raw := make([]byte, 0, 1+2+length+2)
	raw = append(raw,
		frameDelimiter,
		byte(length>>8),
		byte(length),
		f.dst,
		f.cnt,
		f.cmd)
	raw = append(raw, f.payload...)
	crc := crc16.Checksum(raw, bidcosTable)
	raw = append(raw, byte(crc>>8), byte(crc))
	return escape(raw)
}

// errBadFrame is returned (wrapped) by frameReader.next for frames
// which were received but could not be decoded (bad CRC, bad length).
// Reading can continue after such an error.
var errBadFrame = errors.New("bad frame")

// errResync signals that a frame delimiter was found in the middle of
// a frame; the partial frame is discarded and a new one starts.
var errResync = errors.New("unexpected frame delimiter within frame")

// frameReader decodes frames from a byte stream, re-synchronizing on
// the frame delimiter after invalid data.
type frameReader struct {
	br *bufio.Reader

	// altInits returns additional CRC initialization values to try
	// when a frame fails the CRC check, or nil. See
	// Conn.altCRCInits for why this is necessary.
	altInits func() []uint16

	// pendingDelim is set when the previous frame was aborted because
	// a frame delimiter was encountered (which then starts the next
	// frame).
	pendingDelim bool

	// skipped counts bytes skipped while looking for a frame delimiter.
	skipped int
}

func newFrameReader(r io.Reader) *frameReader {
	return &frameReader{br: bufio.NewReader(r)}
}

// readUnescaped reads one (unescaped) byte of a frame.
func (fr *frameReader) readUnescaped() (byte, error) {
	b, err := fr.br.ReadByte()
	if err != nil {
		return 0, err
	}
	if b == frameDelimiter {
		return 0, errResync
	}
	if b != escapeByte {
		return b, nil
	}
	b, err = fr.br.ReadByte()
	if err != nil {
		return 0, err
	}
	if b == frameDelimiter {
		return 0, errResync
	}
	return b | 0x80, nil
}

// next returns the next frame. Errors wrapping errBadFrame are not
// fatal: the caller should log them and call next again. All other
// errors are read errors from the underlying reader.
func (fr *frameReader) next() (frame, error) {
	if !fr.pendingDelim {
		// Skip until the frame delimiter.
		for {
			b, err := fr.br.ReadByte()
			if err != nil {
				return frame{}, err
			}
			if b == frameDelimiter {
				break
			}
			fr.skipped++
		}
	}
	fr.pendingDelim = false

	raw := []byte{frameDelimiter}
	read := func() (byte, error) {
		b, err := fr.readUnescaped()
		if err == errResync {
			fr.pendingDelim = true
			return 0, fmt.Errorf("%w: %v after %x", errBadFrame, err, raw)
		}
		if err != nil {
			return 0, err
		}
		raw = append(raw, b)
		return b, nil
	}

	var hdr [2]byte
	for i := range hdr {
		b, err := read()
		if err != nil {
			return frame{}, err
		}
		hdr[i] = b
	}
	length := int(hdr[0])<<8 | int(hdr[1])
	if length < 3 || length > maxFrameLen {
		return frame{}, fmt.Errorf("%w: implausible length %d", errBadFrame, length)
	}
	for i := 0; i < length+2; i++ {
		if _, err := read(); err != nil {
			return frame{}, err
		}
	}

	// Computing the CRC over the frame including its CRC yields 0
	// (no reflection, no final xor).
	if crc16.Checksum(raw, bidcosTable) != 0 {
		ok := false
		if fr.altInits != nil {
			for _, init := range fr.altInits() {
				if crc16.Update(init, raw, bidcosTable) == 0 {
					ok = true
					break
				}
			}
		}
		if !ok {
			return frame{}, fmt.Errorf("%w: invalid checksum: %X", errBadFrame, raw)
		}
	}

	pkt := raw[3 : len(raw)-2]
	return frame{
		dst:     pkt[0],
		cnt:     pkt[1],
		cmd:     pkt[2],
		payload: append([]byte(nil), pkt[3:]...),
	}, nil
}
