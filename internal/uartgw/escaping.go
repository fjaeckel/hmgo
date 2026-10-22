// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see LICENSE.

package uartgw

// The HM-MOD-RPI-PCB escapes two bytes within a frame (i.e. after the
// leading 0xfd frame delimiter):
//
//	0xfc 0x7d represents 0xfd
//	0xfc 0x7c represents 0xfc
//
// 0xfd (frame delimiter) must be escaped within a frame. 0xfc
// introduces an escaped byte, so bytes which happen to be 0xfc need to
// be escaped as well. As a result, 0xfd always means “start of a
// frame”, which means we can re-synchronize on 0xfd after reading
// invalid data.
//
// c.f. HMUARTLGW_send_frame and HMUARTLGW_Read in FHEM’s 00_HMUARTLGW.pm
const (
	frameDelimiter = 0xfd
	escapeByte     = 0xfc
)

// escape returns raw (a complete, unescaped frame including the leading
// frame delimiter and trailing CRC) with every byte after the frame
// delimiter escaped.
func escape(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	// In the worst case, every byte needs to be escaped.
	escaped := make([]byte, 1, len(raw)*2)
	escaped[0] = raw[0]
	for _, b := range raw[1:] {
		if b == frameDelimiter || b == escapeByte {
			escaped = append(escaped, escapeByte, b&0x7f)
		} else {
			escaped = append(escaped, b)
		}
	}
	return escaped
}
