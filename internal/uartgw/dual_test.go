// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package uartgw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeSigner is a stand-in for the BidCoS AES computation: the answer
// is a hash over key index, challenge and message.
type fakeSigner struct{}

func fakeAnswer(keyIndex byte, challenge [6]byte, msg []byte) [16]byte {
	h := sha256.New()
	h.Write([]byte{keyIndex})
	h.Write(challenge[:])
	h.Write(msg)
	var r [16]byte
	copy(r[:], h.Sum(nil))
	return r
}

func (fakeSigner) Respond(keyIndex byte, challenge [6]byte, msg []byte) ([16]byte, error) {
	return fakeAnswer(keyIndex, challenge, msg), nil
}

func (fakeSigner) Verify(keyIndex byte, challenge [6]byte, msg []byte, response [16]byte) bool {
	return fakeAnswer(keyIndex, challenge, msg) == response
}

// authSigner additionally implements AESAuthVerifier.
type authSigner struct{ fakeSigner }

func (s authSigner) VerifyAuth(keyIndex byte, challenge [6]byte, msg []byte, response [16]byte) ([4]byte, bool) {
	var auth [4]byte
	copy(auth[:], response[12:])
	return auth, s.Verify(keyIndex, challenge, msg, response)
}

var testOtherDevice = [3]byte{0x11, 0x22, 0x33}

func dualCfg(cfg Config) Config {
	if cfg.HMID == ([3]byte{}) {
		cfg.HMID = testHMID
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = time.Second
	}
	if cfg.AnnounceWait == 0 {
		cfg.AnnounceWait = 200 * time.Millisecond
	}
	if cfg.ResponseTimeout == 0 {
		cfg.ResponseTimeout = 100 * time.Millisecond
	}
	return cfg
}

// openDualTest starts a fake RPI-RF-MOD and opens it via OpenModule.
func openDualTest(t *testing.T, opts fakemod.Options, cfg Config) (*DualConn, *fakemod.Module) {
	t.Helper()
	opts.DualCoPro = true
	mod, rw := fakemod.New(opts)
	t.Cleanup(func() { mod.Close() })
	m, err := OpenModule(rw, dualCfg(cfg))
	if err != nil {
		t.Fatalf("OpenModule: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	c, ok := m.(*DualConn)
	if !ok {
		t.Fatalf("OpenModule returned %T, want *DualConn", m)
	}
	return c, mod
}

// msgBytes builds a BidCoS message (cnt flags cmd src dst payload).
func msgBytes(cnt, flags, cmd byte, src, dst [3]byte, payload ...byte) []byte {
	b := []byte{cnt, flags, cmd}
	b = append(b, src[:]...)
	b = append(b, dst[:]...)
	return append(b, payload...)
}

// waitSent waits until the module transmitted at least n messages.
func waitSent(t *testing.T, mod *fakemod.Module, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		sent := mod.Sent()
		if len(sent) >= n {
			return sent
		}
		if time.Now().After(deadline) {
			t.Fatalf("module transmitted %d messages, want %d: %x", len(sent), n, sent)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recvOne(t *testing.T, c Module) []byte {
	t.Helper()
	select {
	case b, ok := <-c.Recv():
		if !ok {
			t.Fatalf("Recv closed: %v", c.Err())
		}
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("nothing received")
	}
	return nil
}

func expectNoRecv(t *testing.T, c Module, d time.Duration) {
	t.Helper()
	select {
	case b := <-c.Recv():
		t.Fatalf("unexpectedly received %x", b)
	case <-time.After(d):
	}
}

func TestOpenModuleDetect(t *testing.T) {
	for _, tt := range []struct {
		name       string
		opts       fakemod.Options
		kind       string
		changeApps int
	}{
		{"classic bootloader banner", fakemod.Options{}, KindHMModRPIPCB, 0},
		{"classic app running, no banner", fakemod.Options{StartInApp: true}, KindHMModRPIPCB, 0},
		{"dual bootloader banner", fakemod.Options{DualCoPro: true}, KindRPIRFMod, 1},
		{"dual bootloader banner on OS", fakemod.Options{DualCoPro: true, BannerOnOS: true}, KindRPIRFMod, 1},
		{"dual app banner", fakemod.Options{DualCoPro: true, AnnounceApp: true}, KindRPIRFMod, 0},
		{"dual app running, no banner", fakemod.Options{DualCoPro: true, StartInApp: true}, KindRPIRFMod, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mod, rw := fakemod.New(tt.opts)
			defer mod.Close()
			m, err := OpenModule(rw, dualCfg(Config{Timeout: 300 * time.Millisecond}))
			if err != nil {
				t.Fatalf("OpenModule: %v", err)
			}
			defer m.Close()
			if got := m.Kind(); got != tt.kind {
				t.Fatalf("Kind = %q, want %q", got, tt.kind)
			}
			if !mod.InApp() {
				t.Error("module not in application")
			}
			if tt.kind == KindRPIRFMod {
				if got := mod.ChangeApps(); got != tt.changeApps {
					t.Errorf("CHANGE_APP sent %d times, want %d", got, tt.changeApps)
				}
				dc := m.(*DualConn)
				if dc.Application() != fakemod.TagDualApp {
					t.Errorf("Application = %q", dc.Application())
				}
				if dc.FirmwareVersion() != "4.4.22" || dc.SerialNumber() != "FAKE000001" || dc.SGTIN() != "3014F711A0001F5A4993DA3A" {
					t.Errorf("FirmwareVersion %q, SerialNumber %q, SGTIN %q", dc.FirmwareVersion(), dc.SerialNumber(), dc.SGTIN())
				}
			} else {
				if _, ok := m.(*Conn); !ok {
					t.Fatalf("got %T, want *Conn", m)
				}
				if hmid, ok := mod.HMID(); !ok || hmid != testHMID {
					t.Errorf("HMID not set: %x %v", hmid, ok)
				}
				// The classic module must still work through the pump.
				if _, err := m.Send(context.Background(), bidiPacket(0x42).Encode()); err != nil {
					t.Errorf("Send: %v", err)
				}
			}
		})
	}
}

func TestOpenModuleSilent(t *testing.T) {
	mod, rw := fakemod.New(fakemod.Options{Mute: true})
	defer mod.Close()
	_, err := OpenModule(rw, Config{HMID: testHMID, Timeout: 100 * time.Millisecond, AnnounceWait: 50 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("OpenModule: got %v, want ErrTimeout", err)
	}
	select {
	case <-mod.Done():
	case <-time.After(time.Second):
		t.Error("module still running after failed OpenModule")
	}
}

func TestOpenLegacyOnDualCoPro(t *testing.T) {
	// The classic Open keeps refusing the dual-coprocessor firmware.
	for _, startInApp := range []bool{false, true} {
		t.Run(fmt.Sprintf("startInApp=%v", startInApp), func(t *testing.T) {
			mod, rw := fakemod.New(fakemod.Options{DualCoPro: true, StartInApp: startInApp})
			defer mod.Close()
			_, err := Open(rw, Config{HMID: testHMID, Timeout: time.Second, AnnounceWait: 200 * time.Millisecond})
			if !errors.Is(err, ErrDualCoProFirmware) {
				t.Fatalf("Open: got %v, want ErrDualCoProFirmware", err)
			}
		})
	}
}

func TestOpenDualOnClassic(t *testing.T) {
	mod, rw := fakemod.New(fakemod.Options{})
	defer mod.Close()
	_, err := OpenDual(rw, dualCfg(Config{}))
	if !errors.Is(err, ErrUnknownModule) {
		t.Fatalf("OpenDual: got %v, want ErrUnknownModule", err)
	}
}

func TestOpenDualInitSteps(t *testing.T) {
	var checked []byte
	c, mod := openDualTest(t, fakemod.Options{}, Config{
		DualInit: []InitStep{
			{Name: "identify again", Dst: dstDual, Payload: []byte{0x01}, ExpectAck: true,
				Check: func(reply []byte) error { checked = reply; return nil }},
		},
	})
	_ = c
	if want := append([]byte{0x05, 0x01}, fakemod.TagDualApp...); !bytes.Equal(checked, want) {
		t.Errorf("init step reply = %q, want %q", checked, want)
	}
	if mod.Identifies() < 1 {
		t.Error("no IDENTIFY")
	}

	// A step the module rejects (DUAL_ERR) fails OpenModule.
	mod2, rw := fakemod.New(fakemod.Options{DualCoPro: true})
	defer mod2.Close()
	_, err := OpenModule(rw, dualCfg(Config{DualInit: []InitStep{{Name: "bogus", Dst: dstOS, Payload: []byte{0x42}, ExpectAck: true}}}))
	if !errors.Is(err, ErrNACK) {
		t.Fatalf("OpenModule: got %v, want ErrNACK", err)
	}
}

func TestDualSendAck(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{})
	pkt := bidiPacket(0x42)
	pkt.Flags |= bidcos.Burst
	enc := pkt.Encode()
	resp, err := c.Send(context.Background(), enc)
	if err != nil {
		t.Fatal(err)
	}
	p, err := bidcos.Decode(resp)
	if err != nil {
		t.Fatal(err)
	}
	if resp[0] != 0x01 || p.Auth() != bidcos.Unauthenticated {
		t.Errorf("status %#x, auth %v", resp[0], p.Auth())
	}
	if p.Cmd != bidcos.Ack || p.Msgcnt != 0x42 || p.Source != testDevice || p.Dest != testHMID || p.RSSI() != -fakemod.DefaultRSSI {
		t.Errorf("unexpected response %+v", p)
	}
	sent := mod.Sent()
	if len(sent) != 1 || !bytes.Equal(sent[0], enc[3:]) {
		t.Errorf("module sent %x, want %x", sent, enc[3:])
	}
	// Burst packets: send now (80 00), burst mode (0x90).
	if opts := mod.LLMACOpts(); len(opts) != 1 || !bytes.Equal(opts[0], []byte{0x80, 0x00, 0x90}) {
		t.Errorf("LLMAC_SEND options = %x, want 800090", opts)
	}
	// The response is consumed by Send.
	expectNoRecv(t, c, 100*time.Millisecond)
}

func TestDualSendNoBiDi(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{})
	resp, err := c.Send(context.Background(), noBiDiPacket(0x10))
	if err != nil || resp != nil {
		t.Fatalf("Send = %x, %v", resp, err)
	}
	if n := len(mod.Sent()); n != 1 {
		t.Errorf("transmitted %d times", n)
	}
	if opts := mod.LLMACOpts(); len(opts) != 1 || !bytes.Equal(opts[0], []byte{0x80, 0x00, 0x80}) {
		t.Errorf("LLMAC_SEND options = %x, want 800080", opts)
	}
}

func TestDualSendAckStatus(t *testing.T) {
	c, _ := openDualTest(t, fakemod.Options{
		OnLLMACSend: func(msg []byte) [][]byte {
			// ACK_STATUS: 02 01 <chn> <level> <flags> <rssi>
			return [][]byte{msgBytes(msg[0], 0x80, 0x02, testDevice, testHMID, 0x01, 0x04, 0x22, 0x00, 0x48)}
		},
	}, Config{})
	resp, err := c.Send(context.Background(), bidiPacket(0x43).Encode())
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte{0x01, 0x00, fakemod.DefaultRSSI}, msgBytes(0x43, 0x80, 0x02, testDevice, testHMID, 0x01, 0x04, 0x22, 0x00, 0x48)...)
	if !bytes.Equal(resp, want) {
		t.Errorf("response %x, want %x", resp, want)
	}
	expectNoRecv(t, c, 100*time.Millisecond)
}

func TestDualSendRetransmit(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	c, mod := openDualTest(t, fakemod.Options{
		OnLLMACSend: func(msg []byte) [][]byte {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls < 3 {
				return nil
			}
			return fakemod.DefaultOnLLMACSend(msg)
		},
	}, Config{})
	enc := bidiPacket(0x44).Encode()
	resp, err := c.Send(context.Background(), enc)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp[3] != 0x44 {
		t.Errorf("response %x", resp)
	}
	sent := mod.Sent()
	if len(sent) != 3 {
		t.Fatalf("transmitted %d times, want 3", len(sent))
	}
	for _, s := range sent {
		if !bytes.Equal(s, enc[3:]) {
			t.Errorf("retransmission %x differs from %x", s, enc[3:])
		}
	}
}

func TestDualSendNoResponse(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{
		OnLLMACSend: func([]byte) [][]byte { return nil },
	}, Config{})
	start := time.Now()
	_, err := c.Send(context.Background(), bidiPacket(0x45).Encode())
	if !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Send: got %v, want ErrNoResponse", err)
	}
	if n := len(mod.Sent()); n != 4 {
		t.Errorf("transmitted %d times, want 4 (1 + 3 retries)", n)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("gave up after %v", d)
	}
	if c.Err() != nil {
		t.Errorf("connection failed: %v", c.Err())
	}

	// SendRetries < 0: no retransmissions.
	c2, mod2 := openDualTest(t, fakemod.Options{
		OnLLMACSend: func([]byte) [][]byte { return nil },
	}, Config{SendRetries: -1})
	if _, err := c2.Send(context.Background(), bidiPacket(0x45).Encode()); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Send: got %v, want ErrNoResponse", err)
	}
	if n := len(mod2.Sent()); n != 1 {
		t.Errorf("transmitted %d times, want 1", n)
	}
}

func TestDualSendLLMACNack(t *testing.T) {
	c, _ := openDualTest(t, fakemod.Options{
		LLMACAckStatus: func([]byte) byte { return 0x00 },
	}, Config{})
	if _, err := c.Send(context.Background(), bidiPacket(0x46).Encode()); !errors.Is(err, ErrNACK) {
		t.Fatalf("Send: got %v, want ErrNACK", err)
	}
}

func TestDualModuleUnresponsive(t *testing.T) {
	c, _ := openDualTest(t, fakemod.Options{
		LLMACAckStatus: func([]byte) byte { return fakemod.NoAck },
	}, Config{Timeout: 50 * time.Millisecond})
	for i := 0; i < 3; i++ {
		_, err := c.Send(context.Background(), noBiDiPacket(byte(i)))
		if i < 2 && !errors.Is(err, ErrTimeout) {
			t.Fatalf("Send %d: got %v, want ErrTimeout", i, err)
		}
	}
	if !errors.Is(c.Err(), ErrModuleUnresponsive) {
		t.Fatalf("Err = %v, want ErrModuleUnresponsive", c.Err())
	}
}

// aesDevice emulates a device which challenges every message of cmd
// 0x11 and checks our answer.
type aesDevice struct {
	mu        sync.Mutex
	challenge [6]byte
	orig      []byte
	answer    []byte
	nack      bool
}

func (d *aesDevice) onSend(msg []byte) [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch msg[2] {
	case 0x11:
		d.orig = append([]byte(nil), msg...)
		d.challenge = [6]byte{1, 2, 3, 4, 5, 6}
		p := append([]byte{0x04}, d.challenge[:]...)
		p = append(p, 0x02) // key index 1
		return [][]byte{msgBytes(msg[0], 0xa0, 0x02, testDevice, testHMID, p...)}
	case 0x03:
		d.answer = append([]byte(nil), msg...)
		want := fakeAnswer(1, d.challenge, d.orig)
		if d.nack || !bytes.Equal(msg[9:], want[:]) {
			return [][]byte{msgBytes(msg[0], 0x80, 0x02, testDevice, testHMID, 0x80)}
		}
		return [][]byte{msgBytes(msg[0], 0x80, 0x02, testDevice, testHMID, 0x00, 0xaa, 0xbb, 0xcc, 0xdd)}
	}
	return nil
}

func TestDualSendAES(t *testing.T) {
	dev := &aesDevice{}
	c, mod := openDualTest(t, fakemod.Options{OnLLMACSend: dev.onSend}, Config{Signer: fakeSigner{}})
	enc := bidiPacket(0x47).Encode()
	resp, err := c.Send(context.Background(), enc)
	if err != nil {
		t.Fatal(err)
	}
	p, err := bidcos.Decode(resp)
	if err != nil {
		t.Fatal(err)
	}
	if p.Auth() != bidcos.AuthOK || p.Cmd != bidcos.Ack || p.Payload[0] != 0x00 {
		t.Errorf("response %x (auth %v)", resp, p.Auth())
	}
	sent := mod.Sent()
	if len(sent) != 2 {
		t.Fatalf("transmitted %x", sent)
	}
	ans := sent[1]
	want := fakeAnswer(1, [6]byte{1, 2, 3, 4, 5, 6}, enc[3:])
	if ans[0] != 0x47 || ans[2] != 0x03 || !bytes.Equal(ans[3:6], testHMID[:]) || !bytes.Equal(ans[6:9], testDevice[:]) ||
		!bytes.Equal(ans[9:], want[:]) {
		t.Errorf("AES answer %x, want counter 47, cmd 03, payload %x", ans, want)
	}
	expectNoRecv(t, c, 100*time.Millisecond)
}

func TestDualSendAESFailed(t *testing.T) {
	t.Run("nack", func(t *testing.T) {
		dev := &aesDevice{nack: true}
		c, _ := openDualTest(t, fakemod.Options{OnLLMACSend: dev.onSend}, Config{Signer: fakeSigner{}})
		resp, err := c.Send(context.Background(), bidiPacket(0x48).Encode())
		if !errors.Is(err, ErrAESFailed) {
			t.Fatalf("Send: got %v, want ErrAESFailed", err)
		}
		if resp == nil || resp[0] != 0x03 {
			t.Errorf("response %x, want status 0x03", resp)
		}
	})
	t.Run("no signer", func(t *testing.T) {
		dev := &aesDevice{}
		c, _ := openDualTest(t, fakemod.Options{OnLLMACSend: dev.onSend}, Config{})
		if _, err := c.Send(context.Background(), bidiPacket(0x49).Encode()); !errors.Is(err, ErrAESFailed) {
			t.Fatalf("Send: got %v, want ErrAESFailed", err)
		}
	})
}

func TestDualRecv(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{})
	msg := msgBytes(0x12, 0x84, 0x70, testDevice, [3]byte{}, 0x00, 0xd3)
	if err := mod.InjectLLMACRecv(0x52, msg); err != nil {
		t.Fatal(err)
	}
	got := recvOne(t, c)
	if want := append([]byte{0x01, 0x00, 0x52}, msg...); !bytes.Equal(got, want) {
		t.Errorf("received %x, want %x", got, want)
	}
	p, err := bidcos.Decode(got)
	if err != nil {
		t.Fatal(err)
	}
	if p.RSSI() != -0x52 || p.Auth() != bidcos.Unauthenticated || p.Source != testDevice {
		t.Errorf("decoded %+v", p)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(mod.Sent()); n != 0 {
		t.Errorf("transmitted %d messages for a broadcast", n)
	}
}

func TestDualSoftwareAck(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{})
	if err := c.AddPeer(context.Background(), Peer{Addr: testDevice, Channels: 2}); err != nil {
		t.Fatal(err)
	}

	// Not acked: from a non-peer, an ACK frame, a message without BiDi,
	// a message not addressed to us.
	for _, m := range [][]byte{
		msgBytes(0x01, 0xa0, 0x10, testOtherDevice, testHMID, 0x06, 0x01, 0x00),
		msgBytes(0x02, 0xa0, 0x02, testDevice, testHMID, 0x00),
		msgBytes(0x03, 0x80, 0x10, testDevice, testHMID, 0x06, 0x01, 0x00),
		msgBytes(0x04, 0xa0, 0x10, testDevice, testOtherDevice, 0x06, 0x01, 0x00),
	} {
		mod.InjectLLMACRecv(fakemod.DefaultRSSI, m)
		if got := recvOne(t, c); !bytes.Equal(got[3:], m) {
			t.Errorf("received %x, want %x", got[3:], m)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if sent := mod.Sent(); len(sent) != 0 {
		t.Fatalf("unexpected ACKs: %x", sent)
	}

	// Acked: BiDi message from a peer to us, scheduled 100ms after its
	// reception on the module clock.
	m := msgBytes(0x5c, 0xa2, 0x10, testDevice, testHMID, 0x06, 0x01, 0x00, 0x00)
	ts := mod.Clock()
	mod.InjectLLMACRecvAt(ts, fakemod.DefaultRSSI, m)
	sent := waitSent(t, mod, 1)
	want := msgBytes(0x5c, 0x80, 0x02, testHMID, testDevice, 0x00)
	if !bytes.Equal(sent[0], want) {
		t.Errorf("ACK %x, want %x", sent[0], want)
	}
	checkScheduled(t, mod.LLMACSent()[0], ts)
	if got := recvOne(t, c); got[0] != 0x01 || !bytes.Equal(got[3:], m) {
		t.Errorf("received %x", got)
	}

	// Disabled.
	off := false
	c2, mod2 := openDualTest(t, fakemod.Options{}, Config{SoftwareAck: &off})
	c2.AddPeer(context.Background(), Peer{Addr: testDevice})
	mod2.InjectLLMACRecv(fakemod.DefaultRSSI, m)
	recvOne(t, c2)
	time.Sleep(50 * time.Millisecond)
	if sent := mod2.Sent(); len(sent) != 0 {
		t.Errorf("ACK sent although disabled: %x", sent)
	}
}

// aesPeerDevice answers our challenges, optionally wrongly or not at
// all.
func aesPeerDevice(orig []byte, mode string) func([]byte) [][]byte {
	return func(msg []byte) [][]byte {
		if msg[2] != 0x02 || len(msg) < 17 || msg[9] != 0x04 {
			return nil // our final ACK
		}
		var ch [6]byte
		copy(ch[:], msg[10:16])
		r := fakeAnswer(msg[16]/2, ch, orig)
		switch mode {
		case "silent":
			return nil
		case "wrong":
			r[0] ^= 1
		}
		return [][]byte{msgBytes(msg[0], 0xa0, 0x03, testDevice, testHMID, r[:]...)}
	}
}

func TestDualSoftwareAES(t *testing.T) {
	orig := msgBytes(0x61, 0xa2, 0x10, testDevice, testHMID, 0x06, 0x01, 0xc8, 0x00)
	for _, tt := range []struct {
		mode   string
		signer AESSigner
		status byte
		ack    []byte // expected final ACK, nil: none
	}{
		{"ok", fakeSigner{}, 0x02, msgBytes(0x61, 0x80, 0x02, testHMID, testDevice, 0x00)},
		{"ok with auth", authSigner{}, 0x02, nil /* checked below */},
		{"wrong", fakeSigner{}, 0x03, nil},
		{"silent", fakeSigner{}, 0x03, nil},
		{"no signer", nil, 0x03, nil},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			mode := tt.mode
			if mode == "ok with auth" {
				mode = "ok"
			}
			c, m := openDualTest(t, fakemod.Options{OnLLMACSend: aesPeerDevice(orig, mode)}, Config{
				Signer: tt.signer,
				Rand:   bytes.NewReader(bytes.Repeat([]byte{0xa5}, 64)),
			})
			if err := c.AddPeer(context.Background(), Peer{Addr: testDevice, KeyIndex: 1, Channels: 2, AESChannels: []byte{1}}); err != nil {
				t.Fatal(err)
			}
			m.InjectLLMACRecv(fakemod.DefaultRSSI, orig)
			got := recvOne(t, c)
			if want := append([]byte{tt.status, 0x00, fakemod.DefaultRSSI}, orig...); !bytes.Equal(got, want) {
				t.Errorf("received %x, want %x", got, want)
			}
			p, _ := bidcos.Decode(got)
			wantAuth := bidcos.AuthOK
			if tt.status == 0x03 {
				wantAuth = bidcos.AuthFailed
			}
			if p.Auth() != wantAuth {
				t.Errorf("Auth = %v, want %v", p.Auth(), wantAuth)
			}
			// The device's 0x03 answer is not delivered.
			expectNoRecv(t, c, 100*time.Millisecond)

			var sent [][]byte
			switch tt.mode {
			case "ok", "ok with auth":
				sent = waitSent(t, m, 2) // the final ACK is scheduled 100ms after the answer
			default:
				time.Sleep(150 * time.Millisecond)
				sent = m.Sent()
			}
			if tt.signer == nil {
				if len(sent) != 0 {
					t.Errorf("transmitted %x without a signer", sent)
				}
				return
			}
			challenge := msgBytes(0x61, 0xa0, 0x02, testHMID, testDevice, 0x04, 0xa5, 0xa5, 0xa5, 0xa5, 0xa5, 0xa5, 0x02)
			if len(sent) < 1 || !bytes.Equal(sent[0], challenge) {
				t.Fatalf("challenge %x, want %x", sent, challenge)
			}
			ls, inj := m.LLMACSent(), m.Injected()
			ch := ls[0]
			want := (inj[0].Ts + 100) & 0x7fff
			if got := uint16(ch.Opts[0])<<8 | uint16(ch.Opts[1]); got != want || ch.Opts[2] != 0x80 {
				t.Errorf("challenge options %x, want t=%04x", ch.Opts, want)
			}
			if len(ls) == 2 {
				// final ACK: 100ms after the device's AES answer
				checkScheduled(t, ls[1], inj[1].Ts)
			}
			switch tt.mode {
			case "ok":
				if len(sent) != 2 || !bytes.Equal(sent[1], tt.ack) {
					t.Errorf("final ACK %x, want %x", sent[1:], tt.ack)
				}
			case "ok with auth":
				r := fakeAnswer(1, [6]byte{0xa5, 0xa5, 0xa5, 0xa5, 0xa5, 0xa5}, orig)
				want := msgBytes(0x61, 0x80, 0x02, testHMID, testDevice, append([]byte{0x00}, r[12:]...)...)
				if len(sent) != 2 || !bytes.Equal(sent[1], want) {
					t.Errorf("final ACK %x, want %x", sent[1:], want)
				}
			default:
				if len(sent) != 1 {
					t.Errorf("ACK sent after failed AES: %x", sent[1:])
				}
			}
		})
	}
}

// checkScheduled checks that s was scheduled 100ms after rxTs and that
// the (fake) module honoured that.
func checkScheduled(t *testing.T, s fakemod.LLMACSent, rxTs uint16) {
	t.Helper()
	want := (rxTs + 100) & 0x7fff
	if got := uint16(s.Opts[0])<<8 | uint16(s.Opts[1]); got != want || s.Opts[2] != 0x80 {
		t.Errorf("LLMAC_SEND options %x, want t=%04x (rx %04x + 100ms), mode 80", s.Opts, want, rxTs)
	}
	if d := diff15(s.Ts, want); d < 0 || d > 50 {
		t.Errorf("transmitted at %04x, scheduled for %04x", s.Ts, want)
	}
}

func TestDiff15(t *testing.T) {
	for _, tt := range []struct {
		a, b uint16
		want int
	}{
		{0x0010, 0x0000, 0x10},
		{0x0000, 0x0010, -0x10},
		{0x0034, 0x7fd0, 100},
		{0x7fd0, 0x0034, -100},
	} {
		if got := diff15(tt.a, tt.b); got != tt.want {
			t.Errorf("diff15(%04x, %04x) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestDualSoftwareAckClockWrap(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{ClockOffset: 0x7f00}, Config{})
	c.AddPeer(context.Background(), Peer{Addr: testDevice})
	// Wait until the module clock is just before the wrap.
	for diff15(mod.Clock(), 0x7fc0) < 0 {
		time.Sleep(5 * time.Millisecond)
	}
	ts := mod.Clock()
	mod.InjectLLMACRecvAt(ts, fakemod.DefaultRSSI, msgBytes(0x01, 0xa0, 0x10, testDevice, testHMID, 0x06, 0x01, 0x00, 0x00))
	waitSent(t, mod, 1)
	checkScheduled(t, mod.LLMACSent()[0], ts)
}

func TestDualReplyLate(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{})
	c.AddPeer(context.Background(), Peer{Addr: testDevice})
	// Every reading of the local clock is 200ms later than the previous
	// one: when the reply is scheduled, rx+100ms has passed.
	var calls int
	c.setNowForTest(func() time.Time {
		calls++
		return time.Now().Add(time.Duration(calls) * 200 * time.Millisecond)
	})
	mod.InjectLLMACRecv(fakemod.DefaultRSSI, msgBytes(0x02, 0xa0, 0x10, testDevice, testHMID, 0x06, 0x01, 0x00, 0x00))
	waitSent(t, mod, 1)
	if s := mod.LLMACSent()[0]; s.Opts != [3]byte{0x80, 0x00, 0x80} {
		t.Errorf("late reply options %x, want 800080 (now)", s.Opts)
	}
}

func TestDualTimeRequest(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{Signer: fakeSigner{}})
	c.AddPeer(context.Background(), Peer{Addr: testDevice, KeyIndex: 1, AESChannels: []byte{1}})
	// Time requests are neither challenged nor acked: the bridge
	// answers them (capture lines 1322-1324).
	ts := mod.Clock()
	mod.InjectLLMACRecvAt(ts, fakemod.DefaultRSSI, msgBytes(0x70, 0xa0, 0x3f, testDevice, testHMID))
	if got := recvOne(t, c); got[0] != 0x01 {
		t.Errorf("received %x", got)
	}

	// The bridge's answer (no BiDi, same counter) is scheduled
	// relative to the request.
	answer := &bidcos.Packet{Msgcnt: 0x70, FixedMsgcnt: true, Flags: bidcos.RepeatEnable, Cmd: 0x3f,
		Source: testHMID, Dest: testDevice, Payload: []byte{0x02, 0x04, 0x32, 0x6c, 0x7f, 0x6b}}
	if _, err := c.Send(context.Background(), answer.Encode()); err != nil {
		t.Fatal(err)
	}
	ls := mod.LLMACSent()
	if len(ls) != 1 || ls[0].Msg[2] != 0x3f {
		t.Fatalf("sent %v, want only the time answer (no ACK)", ls)
	}
	checkScheduled(t, ls[0], ts)

	// Any other non-BiDi packet is sent at once.
	if _, err := c.Send(context.Background(), noBiDiPacket(0x71)); err != nil {
		t.Fatal(err)
	}
	if s := mod.LLMACSent()[1]; s.Opts != [3]byte{0x80, 0x00, 0x80} {
		t.Errorf("options %x, want 800080", s.Opts)
	}
}

func TestDualRetryDefaults(t *testing.T) {
	mod, rw := fakemod.New(fakemod.Options{DualCoPro: true, OnLLMACSend: func([]byte) [][]byte { return nil }})
	defer mod.Close()
	m, err := OpenModule(rw, Config{HMID: testHMID, Timeout: time.Second, AnnounceWait: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.Send(context.Background(), bidiPacket(0x50).Encode()); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Send: got %v, want ErrNoResponse", err)
	}
	sent := mod.LLMACSent()
	if len(sent) != 4 {
		t.Fatalf("transmitted %d times, want 4 (1 + 3 retries, like OpenCCU)", len(sent))
	}
	for i := 1; i < len(sent); i++ {
		if d := sent[i].At.Sub(sent[i-1].At); d < 265*time.Millisecond || d > 600*time.Millisecond {
			t.Errorf("retransmission %d after %v, want ~275ms", i, d)
		}
		if !bytes.Equal(sent[i].Msg, sent[0].Msg) {
			t.Errorf("retransmission %x differs", sent[i].Msg)
		}
	}
}

func TestDualResponseWait(t *testing.T) {
	// Restart capture, HM-OU-LED16 at 10:38:20: retransmissions 274-275ms
	// after each send request, even after an LLMAC_ACK 195ms late.
	c := &DualConn{responseTimeout: defaultResponseTimeout}
	normal := bidiPacket(0x52)
	burst := bidiPacket(0x53)
	burst.Flags |= bidcos.Burst
	t0 := time.Unix(0, 0)
	ms := time.Millisecond
	for _, tt := range []struct {
		msg  []byte
		ack  time.Duration
		want time.Duration
	}{
		{normal.Encode()[3:], 23 * ms, 252 * ms},
		{normal.Encode()[3:], 195 * ms, 80 * ms},
		{normal.Encode()[3:], 260 * ms, minWaitAfterAck},
		{burst.Encode()[3:], 384 * ms, defaultResponseTimeout},
	} {
		if got := c.responseWait(tt.msg, t0, t0.Add(tt.ack)); got != tt.want {
			t.Errorf("ack after %v (flags %02x): wait %v, want %v", tt.ack, tt.msg[1], got, tt.want)
		}
	}
}

func TestDualBurstResponseTimeoutFromAck(t *testing.T) {
	// The LLMAC_ACK of a burst transmission comes after the preamble;
	// the response timeout starts there.
	c, _ := openDualTest(t, fakemod.Options{BurstDelay: 300 * time.Millisecond}, Config{})
	pkt := bidiPacket(0x51)
	pkt.Flags |= bidcos.Burst
	if _, err := c.Send(context.Background(), pkt.Encode()); err != nil {
		t.Fatal(err)
	}
}

func TestDualTRXPoll(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{TRXStatus: 0x07}, Config{TRXPollInterval: 20 * time.Millisecond})
	deadline := time.Now().Add(2 * time.Second)
	for testutil.ToFloat64(dualTRXStatus) != 7 || mod.TRXPolls() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("gauge %v after %d polls", testutil.ToFloat64(dualTRXStatus), mod.TRXPolls())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.Err() != nil {
		t.Fatal(c.Err())
	}

	// Unanswered polls make the connection fail.
	c2, _ := openDualTest(t, fakemod.Options{TRXNoReply: true}, Config{TRXPollInterval: 20 * time.Millisecond, Timeout: 50 * time.Millisecond})
	select {
	case <-c2.done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection still up")
	}
	if !errors.Is(c2.Err(), ErrModuleUnresponsive) {
		t.Fatalf("Err = %v, want ErrModuleUnresponsive", c2.Err())
	}
}

func TestDualIgnoredFrames(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{})
	// HMIP traffic and unsolicited TRX frames (capture lines 940, 1094).
	mod.WriteRaw(fakemod.Frame{Dst: fakemod.DstHMIP, Cnt: 0x02, Cmd: 0x07, Payload: []byte{0x80, 0x10, 0x00, 0x8e}}.Encode())
	mod.WriteRaw(fakemod.Frame{Dst: fakemod.DstApp, Cnt: 0x01, Cmd: 0x0b, Payload: []byte{0x00, 0x01}}.Encode())
	mod.WriteRaw(fakemod.Frame{Dst: 0x0c, Cnt: 0x11, Cmd: 0x03, Payload: []byte{0x01}}.Encode())
	expectNoRecv(t, c, 100*time.Millisecond)
	if c.Err() != nil {
		t.Fatal(c.Err())
	}
	if _, err := c.Send(context.Background(), bidiPacket(0x52).Encode()); err != nil {
		t.Fatal(err)
	}
}

func TestDualModuleReset(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{})
	if err := mod.Reset(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-c.Recv():
		if ok {
			t.Fatal("unexpected frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv not closed after reset")
	}
	if !errors.Is(c.Err(), ErrModuleReset) {
		t.Fatalf("Err = %v, want ErrModuleReset", c.Err())
	}
	if _, err := c.Send(context.Background(), bidiPacket(1).Encode()); !errors.Is(err, ErrModuleReset) {
		t.Errorf("Send after reset: %v", err)
	}
}

func TestDualClose(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c.Err(), ErrClosed) {
		t.Errorf("Err = %v", c.Err())
	}
	select {
	case <-mod.Done():
	case <-time.After(time.Second):
		t.Error("module still running")
	}
}

func TestDualConcurrentSendRecv(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{})
	c.AddPeer(context.Background(), Peer{Addr: testOtherDevice})
	const n = 30
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			enc := bidiPacket(byte(i + 1)).Encode() // 0 would mean “assign a counter”
			resp, err := c.Send(context.Background(), enc)
			if err != nil {
				errs <- err
				return
			}
			if resp[3] != enc[3] {
				errs <- fmt.Errorf("response %x for counter %d", resp, enc[3])
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			// BiDi messages of a peer: delivered and software-acked.
			if err := mod.InjectLLMACRecv(0x40, msgBytes(byte(i), 0xa0, 0x10, testOtherDevice, testHMID, 0x06, 0x01)); err != nil {
				errs <- err
			}
		}(i)
	}
	received := 0
	timeout := time.After(5 * time.Second)
	for received < n {
		select {
		case b := <-c.Recv():
			if !bytes.Equal(b[6:9], testOtherDevice[:]) {
				t.Errorf("Send response leaked to Recv: %x", b)
			}
			received++
		case <-timeout:
			t.Fatalf("received %d of %d", received, n)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// n Sends + n software ACKs.
	waitSent(t, mod, 2*n)
}

// requests returns the (dst, cmd) of the frames the host sent.
func requests(mod *fakemod.Module) [][2]byte {
	var res [][2]byte
	for _, f := range mod.Frames() {
		res = append(res, [2]byte{f.Dst, f.Cmd})
	}
	return res
}

func TestDualStartupSequence(t *testing.T) {
	// Like multimacd (restart capture lines 20-43), minus LLMAC 08.
	for _, tt := range []struct {
		name string
		opts fakemod.Options
		want [][2]byte
	}{
		{"app running", fakemod.Options{StartInApp: true},
			[][2]byte{{0xfe, 0x01}, {0x01, 0x02}, {0xfe, 0x04}, {0x03, 0x07}, {0x03, 0x02}}},
		{"bootloader", fakemod.Options{},
			[][2]byte{{0xfe, 0x03}, {0x01, 0x02}, {0xfe, 0x04}, {0x03, 0x07}, {0x03, 0x02}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, mod := openDualTest(t, tt.opts, Config{})
			if got := requests(mod); fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("requests %x, want %x", got, tt.want)
			}
		})
	}
}

func TestDualStartupOptionalQueries(t *testing.T) {
	c, _ := openDualTest(t, fakemod.Options{
		DualIgnore: func(dst, cmd byte) bool {
			return (dst == 0xfe && cmd == 0x04) || (dst == 0x03 && cmd == 0x07)
		},
	}, Config{Timeout: 100 * time.Millisecond, DualFirmwareVersion: "9.9.9"})
	if c.SerialNumber() != "unknown" || c.SGTIN() != "" {
		t.Errorf("SerialNumber %q, SGTIN %q", c.SerialNumber(), c.SGTIN())
	}
	if c.FirmwareVersion() != "9.9.9" {
		t.Errorf("FirmwareVersion %q, want the override", c.FirmwareVersion())
	}
}

func TestDualStartupRequiredQueries(t *testing.T) {
	for _, q := range [][2]byte{{0x01, 0x02}, {0x03, 0x02}} {
		t.Run(fmt.Sprintf("%x", q), func(t *testing.T) {
			mod, rw := fakemod.New(fakemod.Options{DualCoPro: true, StartInApp: true,
				DualIgnore: func(dst, cmd byte) bool { return dst == q[0] && cmd == q[1] }})
			defer mod.Close()
			_, err := OpenModule(rw, dualCfg(Config{Timeout: 100 * time.Millisecond}))
			if !errors.Is(err, ErrTimeout) {
				t.Fatalf("OpenModule: got %v, want ErrTimeout", err)
			}
		})
	}
}

func TestDualClockFromQuery(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{ClockOffset: 0x1234}, Config{TRXPollInterval: 30 * time.Millisecond})
	// Without any received frame, the estimate comes from LLMAC 02.
	c.mu.Lock()
	est, ok := c.moduleTimeLocked()
	c.mu.Unlock()
	if d := diff15(est, mod.Clock()); !ok || d < -50 || d > 50 {
		t.Errorf("module clock estimate %04x (valid %v), module %04x", est, ok, mod.Clock())
	}
	// Without radio traffic, the TRX poll refreshes it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := 0
		for _, r := range requests(mod) {
			if r == [2]byte{0x03, 0x02} {
				n++
			}
		}
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("clock queried %d times", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A remote with AES on button 1 only: a key event of button 2 is
// acknowledged without a challenge, one of button 1 is challenged.
func TestDualSoftwareAESPerChannel(t *testing.T) {
	short2 := msgBytes(0x21, 0xa0, 0x40, testDevice, testHMID, 0x02, 0x05)
	c, m := openDualTest(t, fakemod.Options{OnLLMACSend: func([]byte) [][]byte { return nil }}, Config{
		Signer: fakeSigner{},
		Rand:   bytes.NewReader(bytes.Repeat([]byte{0xa5}, 64)),
	})
	if err := c.AddPeer(context.Background(), Peer{Addr: testDevice, KeyIndex: 1, Channels: 3, AESChannels: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	m.InjectLLMACRecv(fakemod.DefaultRSSI, short2)
	if got := recvOne(t, c); got[0] != 0x01 || !bytes.Equal(got[3:], short2) {
		t.Errorf("received %x", got)
	}
	sent := waitSent(t, m, 1)
	if want := msgBytes(0x21, 0x80, 0x02, testHMID, testDevice, 0x00); !bytes.Equal(sent[0], want) {
		t.Errorf("sent %x, want plain ACK %x", sent[0], want)
	}

	short1 := msgBytes(0x22, 0xa0, 0x40, testDevice, testHMID, 0x01, 0x06)
	m.InjectLLMACRecv(fakemod.DefaultRSSI, short1)
	sent = waitSent(t, m, 2)
	if want := msgBytes(0x22, 0xa0, 0x02, testHMID, testDevice, 0x04, 0xa5, 0xa5, 0xa5, 0xa5, 0xa5, 0xa5, 0x02); !bytes.Equal(sent[1], want) {
		t.Errorf("sent %x, want challenge %x", sent[1], want)
	}
	// Not answered: delivered as AES failed after the hold timeout.
	if got := recvOne(t, c); got[0] != 0x03 || !bytes.Equal(got[3:], short1) {
		t.Errorf("received %x", got)
	}

	for _, tc := range []struct {
		cmd     byte
		payload []byte
		want    bool
	}{
		{0x40, []byte{0x41, 0x01}, true},  // long press, button 1
		{0x40, []byte{0xc2, 0x01}, false}, // long press, button 2, low battery
		{0x41, []byte{0x81, 0x01, 0xc8}, true},
		{0x10, []byte{0x06, 0x02, 0x00}, true}, // channel not parsed: challenged
		{0x02, []byte{0x00}, false},
		{0x40, nil, true},
	} {
		if got := aesRequired(Peer{AESChannels: []byte{1}}, tc.cmd, tc.payload); got != tc.want {
			t.Errorf("aesRequired(%02x %x) = %v", tc.cmd, tc.payload, got)
		}
	}
	if aesRequired(Peer{}, 0x40, []byte{0x01, 0x01}) {
		t.Error("challenge for a peer without AES")
	}
}
