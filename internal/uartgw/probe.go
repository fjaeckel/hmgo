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
// its application; the HM-MOD-RPI-PCB's classic firmware ignores it. It
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
				if kind := tagKind(tag); kind != "" {
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
