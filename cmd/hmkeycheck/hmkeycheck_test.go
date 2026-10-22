// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/fjaeckel/hmgo/internal/hmkey"
)

// A real exchange from the log of a first hmbridge run (HM-Sec-SCo 759111
// to central 458086), with the device's answer recomputed for a known key.
func TestParseAndMatch(t *testing.T) {
	key := hmkey.FromPassphrase("correct horse", 2)
	msg, _ := hex.DecodeString("70a241759111458086016100")
	var ch [6]byte
	hex.Decode(ch[:], []byte("53d5ef99d197"))
	resp, err := bidcosaes.NewSigner(key).Respond(2, ch, msg)
	if err != nil {
		t.Fatal(err)
	}
	log := strings.Join([]string{
		"2026/10/22 15:04:27.050113 uartgw: recv dst=LLMAC cnt=9 cmd=05 payload=62bb0054" + hex.EncodeToString(msg),
		"2026/10/22 15:04:27.050308 uartgw: send dst=LLMAC cnt=229 cmd=06 payload=631f8070a0024580867591110453d5ef99d19704",
		"2026/10/22 15:04:27.172566 uartgw: recv dst=LLMAC cnt=229 cmd=01 payload=01631f",
		fmt.Sprintf("2026/10/22 15:04:27.308225 uartgw: recv dst=LLMAC cnt=11 cmd=05 payload=63bc005470a203759111458086%x", resp),
	}, "\n")
	hs := parseLog(strings.NewReader(log))
	if len(hs) != 1 {
		t.Fatalf("got %d handshakes, want 1", len(hs))
	}
	if hs[0].keyIndex != 2 || hs[0].device != [3]byte{0x75, 0x91, 0x11} {
		t.Errorf("handshake %+v", hs[0])
	}
	if !hs[0].matches(key.Bytes) {
		t.Error("the right key does not match")
	}
	if hs[0].matches(hmkey.FromPassphrase("wrong", 2).Bytes) {
		t.Error("a wrong key matches")
	}

	keys := keysFromFile(strings.NewReader("Current Index = 2\nKey 2 = " + hex.EncodeToString(key.Bytes[:]) + "\n"))
	found := false
	for _, c := range keys {
		if c.label == "keys line 2" && hs[0].matches(c.key) {
			found = true
		}
	}
	if !found {
		t.Errorf("key file candidate not found among %d candidates", len(keys))
	}
}

// A malformed -key is reported without echoing any of its characters.
func TestBadKeyNotEchoed(t *testing.T) {
	old := hexKeys
	defer func() { hexKeys = old }()
	for _, k := range []string{"zq112233445566778899aabbccddeeff", "0011"} {
		hexKeys = multiFlag{k}
		_, err := candidates()
		if err == nil || err.Error() != "-key #1: must be 32 hex digits (16 bytes)" {
			t.Errorf("candidates() error = %v", err)
		}
	}
}
