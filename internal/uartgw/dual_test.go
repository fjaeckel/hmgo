// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

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
				if dc.FirmwareVersion() != "unknown" {
					t.Errorf("FirmwareVersion = %q", dc.FirmwareVersion())
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
	if opts := mod.LLMACOpts(); len(opts) != 1 || !bytes.Equal(opts[0], []byte{0x80, 0x00, 0x80}) {
		t.Errorf("LLMAC_SEND options = %x", opts)
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
	if n := len(mod.Sent()); n != 3 {
		t.Errorf("transmitted %d times, want 3 (1 + 2 retries)", n)
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

	// Acked: BiDi message from a peer to us.
	m := msgBytes(0x5c, 0xa2, 0x10, testDevice, testHMID, 0x06, 0x01, 0x00, 0x00)
	start := time.Now()
	mod.InjectLLMACRecv(fakemod.DefaultRSSI, m)
	sent := waitSent(t, mod, 1)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("ACK took %v", d)
	}
	want := msgBytes(0x5c, 0x80, 0x02, testHMID, testDevice, 0x00)
	if !bytes.Equal(sent[0], want) {
		t.Errorf("ACK %x, want %x", sent[0], want)
	}
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

			sent := m.Sent()
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

func TestDualSoftwareAESExempt(t *testing.T) {
	c, mod := openDualTest(t, fakemod.Options{}, Config{Signer: fakeSigner{}})
	c.AddPeer(context.Background(), Peer{Addr: testDevice, KeyIndex: 1, AESChannels: []byte{1}})
	// Time requests are not challenged (but acked).
	m := msgBytes(0x70, 0xa0, 0x3f, testDevice, testHMID)
	mod.InjectLLMACRecv(fakemod.DefaultRSSI, m)
	if got := recvOne(t, c); got[0] != 0x01 {
		t.Errorf("received %x", got)
	}
	sent := waitSent(t, mod, 1)
	if sent[0][2] != 0x02 || sent[0][9] != 0x00 {
		t.Errorf("sent %x, want a plain ACK", sent[0])
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
