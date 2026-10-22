// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package uartgw

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// silent emulates devices which never answer.
func silent([]byte) [][]byte { return nil }

func countSent(mod *fakemod.Module) (challenges, acks int) {
	for _, s := range mod.Sent() {
		switch {
		case s[2] == 0x02 && len(s) > 9 && s[9] == 0x04:
			challenges++
		case s[2] == 0x02:
			acks++
		}
	}
	return challenges, acks
}

// Frames of an AES peer that cannot be authenticated (no BiDi, a channel
// without AES named in the payload, ACKs) are not challenged and are
// delivered with status 0x01 (unauthenticated): the bridge must reject
// them for AES devices (internal/bridge/aes.go).
func TestDualUnchallengedFramesAreUnauthenticated(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    []byte
		ack  bool // software-acknowledged
	}{
		{"no BiDi", msgBytes(0x61, 0x84, 0x41, testDevice, testHMID, 0x01, 0x1c, 0xc8), false},
		{"channel without AES", msgBytes(0x62, 0xa4, 0x41, testDevice, testHMID, 0x3f, 0x1c, 0xc8), true},
		{"ACK_STATUS", msgBytes(0x63, 0xa4, 0x02, testDevice, testHMID, 0x01, 0x01, 0xc8, 0x00), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, mod := openDualTest(t, fakemod.Options{OnLLMACSend: silent}, Config{
				Signer: fakeSigner{}, Rand: bytes.NewReader(bytes.Repeat([]byte{0xa5}, 64)),
			})
			if err := c.AddPeer(context.Background(), Peer{Addr: testDevice, KeyIndex: 1, Channels: 2, AESChannels: []byte{1}}); err != nil {
				t.Fatal(err)
			}
			mod.InjectLLMACRecv(fakemod.DefaultRSSI, tc.m)
			if got := recvOne(t, c); got[0] != recvResp {
				t.Errorf("delivered with status 0x%02x, want 0x01", got[0])
			}
			time.Sleep(150 * time.Millisecond)
			ch, acks := countSent(mod)
			if ch != 0 || acks != map[bool]int{false: 0, true: 1}[tc.ack] {
				t.Errorf("transmitted %d challenges and %d ACKs: %x", ch, acks, mod.Sent())
			}
		})
	}
}

// M1: challenges re-sent for retransmissions count towards the per-peer
// challenge limit, and software ACKs have a per-peer budget, so spoofed
// frames cannot burn the module's duty cycle.
func TestDualDutyCycleLimits(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{OnLLMACSend: silent}, Config{
		Signer: fakeSigner{}, Rand: bytes.NewReader(bytes.Repeat([]byte{0xa5}, 4096)),
	})
	other := [3]byte{0x11, 0x22, 0x33}
	c.AddPeer(context.Background(), Peer{Addr: testDevice, KeyIndex: 1, Channels: 2, AESChannels: []byte{1}})
	c.AddPeer(context.Background(), Peer{Addr: other, Channels: 2})
	go func() {
		for range c.Recv() {
		}
	}()
	chLimited := testutil.ToFloat64(dualRateLimited.WithLabelValues(rateKindChallenge))
	ackLimited := testutil.ToFloat64(dualRateLimited.WithLabelValues(rateKindAck))

	// 15 messages of the AES peer, each repeated 4 times (as if our
	// challenges got lost): without the limit, 15 challenges + 45
	// re-sent ones.
	for cnt := 0; cnt < 15; cnt++ {
		m := msgBytes(byte(cnt), 0xa0, 0x10, testDevice, testHMID, 0x06, 0x01, 0x00, 0x00)
		for r := 0; r < 4; r++ {
			mod.InjectLLMACRecv(fakemod.DefaultRSSI, m)
			time.Sleep(5 * time.Millisecond)
		}
	}
	// 100 BiDi messages of a peer without AES within ~0.3s.
	for cnt := 0; cnt < 100; cnt++ {
		mod.InjectLLMACRecv(fakemod.DefaultRSSI, msgBytes(byte(cnt), 0xa0, 0x10, other, testHMID, 0x06, 0x01, 0x00, 0x00))
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	ch, acks := countSent(mod)
	if ch > challengeRateLimit {
		t.Errorf("%d challenges transmitted, want at most %d", ch, challengeRateLimit)
	}
	if acks < ackBurst || acks > ackBurst+2 {
		t.Errorf("%d software ACKs transmitted, want %d (+ refill)", acks, ackBurst)
	}
	if testutil.ToFloat64(dualRateLimited.WithLabelValues(rateKindChallenge)) == chLimited ||
		testutil.ToFloat64(dualRateLimited.WithLabelValues(rateKindAck)) == ackLimited {
		t.Error("hmbridge_uartgw_dual_rate_limited_total not counted")
	}
}

// The real devices' traffic stays within the limits: a remote whose
// button is pressed every 2s for a minute gets every ACK.
func TestDualSoftwareAckBudgetRefills(t *testing.T) {
	b := tokenBucket{}
	t0 := time.Unix(0, 0)
	for i := 0; i < 30; i++ {
		if !b.take(t0.Add(time.Duration(i)*2*time.Second), ackBurst, ackRefill) {
			t.Fatalf("ACK %d refused", i)
		}
	}
	b = tokenBucket{}
	n := 0
	for i := 0; i < 100; i++ {
		if b.take(t0, ackBurst, ackRefill) {
			n++
		}
	}
	if n != ackBurst {
		t.Errorf("burst: %d ACKs, want %d", n, ackBurst)
	}
}

// L3: a device's challenge to our message is only answered with the key
// index configured for the device; anyone could send a challenge naming
// another key (e.g. the publicly known default key, index 0).
func TestDualAnswerChallengeOnlyWithConfiguredKeyIndex(t *testing.T) {
	challenger := func(keyIndex byte) func([]byte) [][]byte {
		return func(msg []byte) [][]byte {
			if msg[2] == 0x11 {
				return [][]byte{msgBytes(msg[0], 0xa0, 0x02, testDevice, testHMID, 0x04, 1, 2, 3, 4, 5, 6, keyIndex*2)}
			}
			if msg[2] == 0x03 {
				return [][]byte{msgBytes(msg[0], 0x80, 0x02, testDevice, testHMID, 0x00)}
			}
			return nil
		}
	}
	for _, tc := range []struct {
		name     string
		keyIndex byte // in the challenge
		peer     bool
		removed  bool
		ok       bool
	}{
		{"configured key index", 1, true, false, true},
		{"default key index 0", 0, true, false, false},
		{"other key index", 2, true, false, false},
		{"not a peer", 1, false, false, false},
		{"removed peer (unpairing)", 1, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, mod := openDualTest(t, fakemod.Options{OnLLMACSend: challenger(tc.keyIndex)}, Config{Signer: fakeSigner{}})
			if tc.peer {
				addKeyPeer(t, c)
			}
			if tc.removed {
				c.RemovePeer(context.Background(), testDevice)
			}
			resp, err := c.Send(context.Background(), bidiPacket(0x47).Encode())
			answers := 0
			for _, s := range mod.Sent() {
				if s[2] == 0x03 {
					answers++
				}
			}
			if tc.ok {
				if err != nil || resp[0] != recvRespWithAESOK || answers != 1 {
					t.Errorf("Send = %x, %v; %d AES answers", resp, err, answers)
				}
				return
			}
			if !errors.Is(err, ErrAESFailed) || answers != 0 {
				t.Errorf("Send = %x, %v; %d AES answers, want ErrAESFailed and none", resp, err, answers)
			}
		})
	}
}
