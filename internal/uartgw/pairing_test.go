// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package uartgw

import (
	"context"
	"encoding/hex"
	"log"
	"strings"
	"testing"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// TestDualKeyExchangeNotLogged: the AES key exchange (cmd 0x04) carries
// the new key encrypted with the device's old key (usually the public
// eQ-3 default key), so the debug log must never show its payload.
func TestDualKeyExchangeNotLogged(t *testing.T) {
	var buf syncBuffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	c, _ := openDualTest(t, fakemod.Options{}, Config{Debug: true})
	if !c.HostKeyExchange() {
		t.Error("HostKeyExchange = false with software AES")
	}
	secret := []byte{0xde, 0xad, 0xbe, 0xef, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b}
	pkt := &bidcos.Packet{Msgcnt: 0x31, Flags: 0xa0, Cmd: 0x04, Source: testHMID, Dest: testDevice, Payload: secret}
	if _, err := c.Send(context.Background(), pkt.Encode()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "AES key exchange redacted") {
		t.Errorf("no redaction note in the log:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), hex.EncodeToString(secret[:6])) {
		t.Fatalf("key exchange payload in the log:\n%s", out)
	}
}

func TestRemovePeer(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})
	if c.HostKeyExchange() {
		t.Error("HM-MOD-RPI-PCB: HostKeyExchange = true")
	}
	ctx := context.Background()
	if err := c.AddPeer(ctx, Peer{Addr: testDevice, Channels: 2}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mod.Peers()[testDevice]; !ok {
		t.Fatal("peer not added")
	}
	if err := c.RemovePeer(ctx, testDevice); err != nil {
		t.Fatal(err)
	}
	if _, ok := mod.Peers()[testDevice]; ok {
		t.Error("peer not removed")
	}

	d, _ := openDualTest(t, fakemod.Options{}, Config{})
	d.AddPeer(ctx, Peer{Addr: testDevice})
	if err := d.RemovePeer(ctx, testDevice); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, ok := d.peers[testDevice]
	d.mu.Unlock()
	if ok {
		t.Error("dual peer not removed")
	}
}
