// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

package bidcosaes

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/fjaeckel/hmgo/internal/hmkey"
)

// Test vectors in testdata/fhem_oracle.txt were generated independently of
// this package by a Perl script that runs the byte handling of FHEM's
// 10_CUL_HM.pm verbatim (CUL_HM_getKeys; the signing response to a
// device's ACK-AES challenge; the "Check AES response from device" code of
// the CUL aesCommReq handling) with Crypt::Rijndael (libcrypt-rijndael-perl
// 1.16) and stubs for the FHEM globals (gettimeofday replaced by fixed
// values). The script is not part of this repository because it contains
// GPL-licensed FHEM code; only its output is.
//
// Keys used by the oracle (hmKey attributes of the VCCU):
//
//	index 0: eQ-3 default key
//	index 1: MD5("hmbridge-test-passphrase")
//	index 5: MD5("second key")
//	index 2: not defined
var oracleKeys = []hmkey.Key{
	hmkey.FromPassphrase("hmbridge-test-passphrase", 1),
	hmkey.FromPassphrase("second key", 5),
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func hex6(t *testing.T, s string) (c [6]byte) {
	t.Helper()
	if copy(c[:], mustHex(t, s)) != 6 {
		t.Fatalf("not 6 bytes: %q", s)
	}
	return c
}

func hex16(t *testing.T, s string) (c [16]byte) {
	t.Helper()
	if copy(c[:], mustHex(t, s)) != 16 {
		t.Fatalf("not 16 bytes: %q", s)
	}
	return c
}

func hexKeyIndex(t *testing.T, s string) byte {
	t.Helper()
	v, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		t.Fatal(err)
	}
	return byte(v) / 2
}

func oracleLines(t *testing.T, kind string) [][]string {
	t.Helper()
	f, err := os.Open("testdata/fhem_oracle.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) > 0 && fs[0] == kind {
			out = append(out, fs)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("no %q lines in oracle output", kind)
	}
	return out
}

func TestOracleKeys(t *testing.T) {
	s := NewSigner(oracleKeys...)
	for _, l := range oracleLines(t, "K") {
		idx, _ := strconv.Atoi(l[1])
		k, ok := s.key(byte(idx))
		if !ok {
			t.Fatalf("key %d missing", idx)
		}
		if got := strings.ToUpper(hex.EncodeToString(k[:])); got != l[2] {
			t.Errorf("key %d mismatch", idx)
		}
	}
}

// TestRespondFHEMOracle checks the central's answer to a device challenge
// against FHEM ("R keyNbr challenge sec usec msg response authbytes").
func TestRespondFHEMOracle(t *testing.T) {
	s := NewSigner(oracleKeys...)
	n := 0
	for _, l := range oracleLines(t, "R") {
		keyIdx := hexKeyIndex(t, l[1])
		ch := hex6(t, l[2])
		sec, _ := strconv.ParseUint(l[3], 16, 32)
		usec, _ := strconv.ParseUint(l[4], 16, 16)
		var r [6]byte
		binary.BigEndian.PutUint32(r[0:4], uint32(sec))
		binary.BigEndian.PutUint16(r[4:6], uint16(usec))
		msg := mustHex(t, l[5])
		name := strings.Join(l[1:6], " ")

		if l[6] == "nokey" {
			if _, err := s.Respond(keyIdx, ch, msg); !errors.Is(err, ErrUnknownKey) {
				t.Errorf("%s: err = %v, want ErrUnknownKey", name, err)
			}
			continue
		}
		key, ok := s.key(keyIdx)
		if !ok {
			t.Fatalf("%s: key missing", name)
		}
		resp, auth := respond(key, ch, r, msg)
		if got := strings.ToUpper(hex.EncodeToString(resp[:])); got != l[6] {
			t.Errorf("%s: response %s, FHEM %s", name, got, l[6])
		}
		if got := strings.ToUpper(hex.EncodeToString(auth[:])); got != l[7] {
			t.Errorf("%s: auth %s, FHEM %s", name, got, l[7])
		}
		// The answer must also pass verification (the scheme is symmetric:
		// a device answers the central's challenge the same way).
		if a, ok := s.VerifyAuth(keyIdx, ch, msg, resp); !ok || a != auth {
			t.Errorf("%s: round trip failed (ok=%v)", name, ok)
		}
		n++
	}
	if n != 108 {
		t.Errorf("checked %d vectors, want 108", n)
	}
}

// TestVerifyFHEMOracle checks device answers that FHEM's check code accepted
// ("V"), rejected after tampering ("T") or with the wrong key ("W").
func TestVerifyFHEMOracle(t *testing.T) {
	s := NewSigner(oracleKeys...)
	for _, l := range oracleLines(t, "V") {
		// V kNo challenge signedMsg receivedMsg response good auth signauth=...
		idx, _ := strconv.Atoi(l[1])
		if l[6] != "good" {
			t.Fatalf("oracle line not good: %v", l)
		}
		auth, ok := s.VerifyAuth(byte(idx), hex6(t, l[2]), mustHex(t, l[4]), hex16(t, l[5]))
		if !ok {
			t.Errorf("V %v: rejected", l[1:6])
		}
		if got := strings.ToUpper(hex.EncodeToString(auth[:])); got != l[7] {
			t.Errorf("V %v: auth %s, FHEM %s", l[1:6], got, l[7])
		}
	}
	for _, l := range oracleLines(t, "T") {
		// T kNo challenge receivedMsg tamperedResponse bad auth
		idx, _ := strconv.Atoi(l[1])
		if l[5] != "bad" {
			t.Fatalf("oracle line not bad: %v", l)
		}
		if s.Verify(byte(idx), hex6(t, l[2]), mustHex(t, l[3]), hex16(t, l[4])) {
			t.Errorf("T %v: tampered response accepted", l[1:5])
		}
	}
	for _, l := range oracleLines(t, "W") {
		// W otherKNo challenge receivedMsg response bad
		idx, _ := strconv.Atoi(l[1])
		if l[5] != "bad" {
			t.Fatalf("oracle line not bad: %v", l)
		}
		if s.Verify(byte(idx), hex6(t, l[2]), mustHex(t, l[3]), hex16(t, l[4])) {
			t.Errorf("W %v: response accepted with wrong key", l[1:5])
		}
	}
}

// A few FHEM oracle vectors inline, so the expected values are visible.
func TestRespondInlineVectors(t *testing.T) {
	cases := []struct {
		key       hmkey.Key
		challenge string
		r         string // FHEM: pack("Nn", sec, usec)
		msg       string
		resp      string
		auth      string
	}{
		// R 00 010203040506 5F5E1000 1234 0BA011F110341A2B3C0201C80000
		{hmkey.EQ3Default, "010203040506", "5F5E10001234", "0BA011F110341A2B3C0201C80000",
			"CEFFE16F4E6DD6D29E845EB09FCFD127", "4A410F5C"},
		// R 02 010203040506 5F5E1000 1234 (MD5 key, index 1)
		{oracleKeys[0], "010203040506", "5F5E10001234", "0BA011F110341A2B3C0201C80000",
			"2E3D3B55B08A1CF68B324A8F05CFC1E5", "8DA386EA"},
	}
	for _, c := range cases {
		var r [6]byte
		copy(r[:], mustHex(t, c.r))
		resp, auth := respond(c.key.Bytes, hex6(t, c.challenge), r, mustHex(t, c.msg))
		if got := strings.ToUpper(hex.EncodeToString(resp[:])); got != c.resp {
			t.Errorf("%v %s: response %s, want %s", c.key, c.msg, got, c.resp)
		}
		if got := strings.ToUpper(hex.EncodeToString(auth[:])); got != c.auth {
			t.Errorf("%v %s: auth %s, want %s", c.key, c.msg, got, c.auth)
		}
	}
}

func TestRespondRandomRoundTrip(t *testing.T) {
	s := NewSigner(oracleKeys...)
	msg := mustHex(t, "12A001F110341A2B3C0108000102030405060708090A0B0C0D0E0F101112")
	ch := hex6(t, "A1B2C3D4E5F6")
	r1, a1, err := s.RespondAuth(1, ch, msg)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Respond(1, ch, msg)
	if err != nil {
		t.Fatal(err)
	}
	if r1 == r2 {
		t.Error("two responses are identical; random prefix not used?")
	}
	if a, ok := s.VerifyAuth(1, ch, msg, r1); !ok || a != a1 {
		t.Error("round trip failed")
	}
	if !s.Verify(1, ch, msg, r2) {
		t.Error("round trip failed")
	}

	// Repeated flag set in transit is ignored.
	rep := append([]byte(nil), msg...)
	rep[1] |= 0x40
	if !s.Verify(1, ch, rep, r1) {
		t.Error("repeated flag not ignored")
	}
	// Any covered byte change is detected (bytes 0..25), others are not covered.
	for i := range msg {
		m := append([]byte(nil), msg...)
		m[i] ^= 0x01
		got := s.Verify(1, ch, m, r1)
		if want := i >= 26; got != want {
			t.Errorf("byte %d changed: Verify = %v, want %v", i, got, want)
		}
	}
	// Wrong challenge, wrong key.
	if s.Verify(1, hex6(t, "A1B2C3D4E5F7"), msg, r1) {
		t.Error("wrong challenge accepted")
	}
	if s.Verify(0, ch, msg, r1) || s.Verify(5, ch, msg, r1) {
		t.Error("wrong key accepted")
	}
	other := NewSigner(hmkey.FromPassphrase("other", 1))
	if other.Verify(1, ch, msg, r1) {
		t.Error("wrong key bytes accepted")
	}
}

func TestUnknownKeyAndShortMessage(t *testing.T) {
	secret := hmkey.FromPassphrase("top secret", 3)
	s := NewSigner(secret)
	ch := hex6(t, "010203040506")
	msg := mustHex(t, "0BA011F110341A2B3C0201C80000")

	_, err := s.Respond(2, ch, msg)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
	unknownErr := err.Error()
	if s.Verify(2, ch, msg, [16]byte{}) {
		t.Error("Verify with unknown key returned true")
	}
	if _, err := s.Respond(3, ch, msg[:8]); !errors.Is(err, ErrShortMessage) {
		t.Errorf("err = %v, want ErrShortMessage", err)
	}
	resp, err := s.Respond(3, ch, msg[:9])
	if err != nil {
		t.Fatal(err)
	}
	if !s.Verify(3, ch, msg[:9], resp) || s.Verify(3, ch, msg[:8], resp) {
		t.Error("9 byte message round trip / short message rejection failed")
	}
	if !s.HasKey(0) || !s.HasKey(3) || s.HasKey(1) {
		t.Error("HasKey wrong")
	}

	// Key material must never appear in errors or formatted output.
	keyHex := hex.EncodeToString(secret.Bytes[:])
	def := hmkey.EQ3Default.Bytes
	defHex := hex.EncodeToString(def[:])
	texts := []string{
		unknownErr,
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s", s, s, s, s),
	}
	if _, err := s.Respond(9, ch, msg); err != nil {
		texts = append(texts, err.Error())
	}
	if _, err := s.Respond(3, ch, nil); err != nil {
		texts = append(texts, err.Error())
	}
	for _, txt := range texts {
		low := strings.ToLower(txt)
		if strings.Contains(low, keyHex) || strings.Contains(low, defHex) ||
			strings.Contains(txt, string(secret.Bytes[:])) ||
			strings.Contains(txt, fmt.Sprint(secret.Bytes[:4])) ||
			strings.Contains(txt, fmt.Sprint(def[:4])) {
			t.Errorf("key material in %q", txt)
		}
	}
}

func TestNewSignerOverrideAndSetKey(t *testing.T) {
	s := NewSigner()
	if !s.HasKey(0) {
		t.Fatal("default key missing")
	}
	k0, _ := s.key(0)
	if k0 != hmkey.EQ3Default.Bytes {
		t.Fatal("index 0 is not the eQ-3 default key")
	}
	s.SetKey(hmkey.FromPassphrase("x", 7))
	if !s.HasKey(7) {
		t.Fatal("SetKey failed")
	}
}

func TestNewChallenge(t *testing.T) {
	for _, idx := range []byte{0, 1, 2, 5} {
		p, c := NewChallenge(idx)
		if len(p) != 8 || p[0] != 0x04 || p[7] != idx*2 {
			t.Errorf("index %d: payload % X", idx, p)
		}
		if string(p[1:7]) != string(c[:]) {
			t.Errorf("index %d: challenge % X not in payload % X", idx, c, p)
		}
		gi, gc, ok := ParseChallenge(p)
		if !ok || gi != idx || gc != c {
			t.Errorf("ParseChallenge(% X) = %d % X %v", p, gi, gc, ok)
		}
	}
	p1, c1 := NewChallenge(1)
	p2, c2 := NewChallenge(1)
	if c1 == c2 || string(p1) == string(p2) {
		t.Error("challenges not random")
	}
}

func TestParseChallenge(t *testing.T) {
	// Device ACK-AES payload as in FHEM (unpack 'A2A12A2': 04 challenge keyNbr).
	idx, c, ok := ParseChallenge(mustHex(t, "04A1B2C3D4E5F602"))
	if !ok || idx != 1 || c != hex6(t, "A1B2C3D4E5F6") {
		t.Errorf("got %d % X %v", idx, c, ok)
	}
	for _, bad := range []string{"", "04A1B2C3D4E5F6", "05A1B2C3D4E5F602", "04A1B2C3D4E5F60200", "04A1B2C3D4E5F603"} {
		if _, _, ok := ParseChallenge(mustHex(t, bad)); ok {
			t.Errorf("ParseChallenge(%s) accepted", bad)
		}
	}
}
