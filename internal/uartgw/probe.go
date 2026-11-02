// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package uartgw

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// DeadlineReadWriter is a connection whose reads can time out, like an
// *os.File of a serial port (opened with O_NONBLOCK) or a net.Conn.
type DeadlineReadWriter interface {
	io.ReadWriter
	SetReadDeadline(time.Time) error
}

// ProbeKind asks the module which family it belongs to before it is
// reset, so that the right reset line can be pulsed: the RPI-RF-MOD
// (dual-coprocessor firmware) answers IDENTIFY in its bootloader and in
// its application; the HM-MOD-RPI-PCB's classic firmware ignores it. An
// HM-MOD-RPI-PCB with the dual firmware answers in its application, too,
// and is told by its firmware version (boardForFirmware). It
// returns KindRPIRFMod, KindHMModRPIPCB (it announced itself as one),
// or "" if nothing identifying arrived within timeout (a classic module,
// or no module at all).
//
// ProbeKind consumes what it reads: the module must be reset (or the
// connection reopened) before OpenModule. The read deadline is cleared
// on return.
func ProbeKind(rw DeadlineReadWriter, timeout time.Duration) (string, error) {
	if err := rw.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", fmt.Errorf("uartgw: probe: %w", err)
	}
	defer rw.SetReadDeadline(time.Time{})
	f := frame{dst: dstDual, cnt: detectCnt, cmd: commonIdentify}
	if _, err := rw.Write(f.encode()); err != nil {
		return "", fmt.Errorf("uartgw: writing to radio module: %w", err)
	}
	fr := newFrameReader(rw)
	for {
		f, err := fr.next()
		switch {
		case errors.Is(err, errBadFrame):
			continue
		case errors.Is(err, os.ErrDeadlineExceeded):
			return "", nil
		case err != nil:
			return "", fmt.Errorf("uartgw: reading from radio module: %w", err)
		}
		if f.dst == dstDual && f.cnt == detectCnt {
			// The answer to our IDENTIFY (see detect).
			if tag, ok := parseTag(f); ok {
				if kind := tagKind(tag); kind == KindRPIRFMod && isAppTag(tag) {
					// The dual firmware runs on both modules.
					return probeDualBoard(rw, fr), nil
				} else if kind != "" {
					return kind, nil
				}
				return "", fmt.Errorf("%w: it announced %q", ErrUnknownModule, tag)
			}
			if f.cmd == osAck {
				// A classic-format ack on COMMON: a classic module
				// rejecting the unknown destination.
				return KindHMModRPIPCB, nil
			}
		}
		if kind, _ := classify(f); kind != "" {
			return kind, nil
		}
	}
}

// probeDualBoard asks the dual-coprocessor application for its firmware
// version and returns the module it implies (boardForFirmware), or
// KindRPIRFMod if it does not answer before the read deadline.
func probeDualBoard(rw io.Writer, fr *frameReader) string {
	req := frame{dst: dstApp, cnt: detectCnt + 2, cmd: trxFirmware}
	if _, err := rw.Write(req.encode()); err != nil {
		return KindRPIRFMod
	}
	for {
		f, err := fr.next()
		switch {
		case errors.Is(err, errBadFrame):
			continue
		case err != nil:
			return KindRPIRFMod
		}
		// Reply: [0x04 0x01 major minor patch …], see DualConn.queryFirmware.
		if f.dst == dstApp && f.cnt == req.cnt && f.cmd == trxReply && len(f.payload) >= 4 && f.payload[0] == 0x01 {
			return boardForFirmware(fmt.Sprintf("%d.%d.%d", f.payload[1], f.payload[2], f.payload[3]))
		}
	}
}
