// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see licenses/.

package uartgw

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// capturedFrames were captured on the wire between hmgo and a real
// HM-MOD-RPI-PCB (firmware 1.0.3; serial NEQ1330980).
var capturedFrames = []struct {
	wire string
	want frame
}{
	{"FD000C000000436F5F4350555F424C7251", frame{dstOS, 0, osGetApp, []byte("Co_CPU_BL")}},
	{"FD0003000003180A", frame{dstOS, 0, osChangeApp, nil}},
	{"FD000400000401993D", frame{dstOS, 0, osAck, []byte{ackOK}}},
	{"FD000D000000436F5F4350555F417070D831", frame{dstOS, 0, osGetApp, []byte("Co_CPU_App")}},
	{"FD00030001021E0C", frame{dstOS, 1, osGetFirmware, nil}},
	{"FD000A00010402010003010201AA8A", frame{dstOS, 1, osAck, []byte{ackInfo, 0x01, 0x00, 0x03, 0x01, 0x02, 0x01}}},
	{"FD000400020A003D10", frame{dstOS, 2, osEnableCSMACA, []byte{0x00}}},
	{"FD0004000204011916", frame{dstOS, 2, osAck, []byte{ackOK}}},
	{"FD000300030B9239", frame{dstOS, 3, osGetSerial, nil}},
	{"FD000E000304024E4551313333303938306AB9", frame{dstOS, 3, osAck, append([]byte{ackInfo}, "NEQ1330980"...)}},
	{"FD000800040E58A7116300548E", frame{dstOS, 4, osSetTime, []byte{0x58, 0xa7, 0x11, 0x63, 0x00}}},
	{"FD000400040401196E", frame{dstOS, 4, osAck, []byte{ackOK}}},
	{"FD001401050300112233445566778899AABBCCDDEEFF024C6D", frame{dstApp, 5, appSetCurrentKey, []byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x02}}},
	{"FD0004010504010D7A", frame{dstApp, 5, appAck, []byte{ackOK}}},
	// contains an escaped 0xfd (FC 7D)
	{"FD0006010600FC7DB02CD166", frame{dstApp, 6, appSetHMID, []byte{0xfd, 0xb0, 0x2c}}},
	{"FD0004010604010D46", frame{dstApp, 6, appAck, []byte{ackOK}}},
	{"FD000901080640C2A8000000022E", frame{dstApp, 8, appAddPeer, []byte{0x40, 0xc2, 0xa8, 0, 0, 0}}},
	{"FD00100108040701010001FFFFFFFFFFFFFFFFCAAF", frame{dstApp, 8, appAck, []byte{
		ackWithMultipartData, 0x01, 0x01, 0x00, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}},
	{"FD000D010A0A40C2A8000102030405068B17", frame{dstApp, 10, appPeerRemoveAES, []byte{0x40, 0xc2, 0xa8, 0, 1, 2, 3, 4, 5, 6}}},
	{"FD0004010A04010DB6", frame{dstApp, 10, appAck, []byte{ackOK}}},
	{"FD0010010B040701010001FFFFFFFFFFFFFFFFC9A5", frame{dstApp, 11, appAck, []byte{
		ackWithMultipartData, 0x01, 0x01, 0x00, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}},
	{"FD0009010C0640C2A80000004236", frame{dstApp, 12, appAddPeer, []byte{0x40, 0xc2, 0xa8, 0, 0, 0}}},
	{"FD0010010C040701010001FFFFFFFFFFFFFFFFCEB7", frame{dstApp, 12, appAck, []byte{
		ackWithMultipartData, 0x01, 0x01, 0x00, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}},
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func framesEqual(a, b frame) bool {
	return a.dst == b.dst && a.cnt == b.cnt && a.cmd == b.cmd && bytes.Equal(a.payload, b.payload)
}

func TestEncodeCaptured(t *testing.T) {
	for _, tt := range capturedFrames {
		if got, want := tt.want.encode(), mustHex(t, tt.wire); !bytes.Equal(got, want) {
			t.Errorf("encode(%v) = %X, want %s", tt.want, got, tt.wire)
		}
	}
}

func TestDecodeCaptured(t *testing.T) {
	// All frames back to back, with some garbage in between.
	var stream []byte
	for _, tt := range capturedFrames {
		stream = append(stream, 0x00, 0x42)
		stream = append(stream, mustHex(t, tt.wire)...)
	}
	fr := newFrameReader(bytes.NewReader(stream))
	for _, tt := range capturedFrames {
		got, err := fr.next()
		if err != nil {
			t.Fatalf("decoding %s: %v", tt.wire, err)
		}
		if !framesEqual(got, tt.want) {
			t.Errorf("decode(%s) = %v, want %v", tt.wire, got, tt.want)
		}
	}
	if _, err := fr.next(); err != io.EOF {
		t.Errorf("at end of stream: got %v, want io.EOF", err)
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	f := frame{dstApp, 0xfc, 0xfd, []byte{0xfc, 0xfd, 0xfe, 0x7c, 0x7d, 0xfc, 0xfc}}
	enc := f.encode()
	if bytes.Count(enc, []byte{0xfd}) != 1 {
		t.Fatalf("encoded frame contains unescaped 0xfd: %X", enc)
	}
	got, err := newFrameReader(bytes.NewReader(enc)).next()
	if err != nil {
		t.Fatal(err)
	}
	if !framesEqual(got, f) {
		t.Errorf("round trip: got %v, want %v", got, f)
	}
}

func TestDecodeBadCRC(t *testing.T) {
	bad := mustHex(t, "FD0004010604010D47") // last CRC byte modified
	good := mustHex(t, "FD0004010504010D7A")
	fr := newFrameReader(bytes.NewReader(append(bad, good...)))
	if _, err := fr.next(); !errors.Is(err, errBadFrame) {
		t.Fatalf("bad CRC: got err %v, want errBadFrame", err)
	}
	got, err := fr.next()
	if err != nil {
		t.Fatal(err)
	}
	if got.cnt != 5 {
		t.Errorf("after bad frame: got %v, want cnt 5", got)
	}
}

func TestDecodeResyncOnTruncatedFrame(t *testing.T) {
	// A truncated frame followed by a complete one: the frame delimiter
	// of the second frame must not be swallowed.
	trunc := mustHex(t, "FD000A000104020100")
	good := mustHex(t, "FD0004010504010D7A")
	fr := newFrameReader(bytes.NewReader(append(trunc, good...)))
	if _, err := fr.next(); !errors.Is(err, errBadFrame) {
		t.Fatalf("truncated frame: got err %v, want errBadFrame", err)
	}
	got, err := fr.next()
	if err != nil {
		t.Fatal(err)
	}
	if got.cnt != 5 || got.cmd != appAck {
		t.Errorf("after truncated frame: got %v", got)
	}
}

func TestTimePayload(t *testing.T) {
	// The captured frame FD000800040E58A7116300548E carries
	// 0x58a71163 = 1487343971 and offset 0 (UTC).
	got := timePayload(unixUTC(1487343971))
	if want := []byte{0x58, 0xa7, 0x11, 0x63, 0x00}; !bytes.Equal(got, want) {
		t.Errorf("timePayload = %x, want %x", got, want)
	}
	got = timePayload(unixIn(1487343971, "CET", 3600))
	if want := []byte{0x58, 0xa7, 0x11, 0x63, 0x02}; !bytes.Equal(got, want) {
		t.Errorf("timePayload(CET) = %x, want %x", got, want)
	}
	got = timePayload(unixIn(1487343971, "X", -3*3600))
	if want := []byte{0x58, 0xa7, 0x11, 0x63, 0xfa}; !bytes.Equal(got, want) {
		t.Errorf("timePayload(UTC-3) = %x, want %x", got, want)
	}
}
