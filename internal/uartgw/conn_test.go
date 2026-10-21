package uartgw

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func unixUTC(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func unixIn(sec int64, name string, offset int) time.Time {
	return time.Unix(sec, 0).In(time.FixedZone(name, offset))
}

var (
	testHMID   = [3]byte{0x1a, 0x2b, 0x3c}
	testDevice = [3]byte{0x38, 0xf5, 0x9c}
	testKey    = hmkey.FromPassphrase("secret passphrase", 1)
)

// open starts a fake module and opens a Conn on it.
func open(t *testing.T, opts fakemod.Options, cfg Config) (*Conn, *fakemod.Module) {
	t.Helper()
	mod, rw := fakemod.New(opts)
	t.Cleanup(func() { mod.Close() })
	if cfg.HMID == ([3]byte{}) {
		cfg.HMID = testHMID
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = time.Second
	}
	if cfg.AnnounceWait == 0 {
		cfg.AnnounceWait = 200 * time.Millisecond
	}
	c, err := Open(rw, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, mod
}

func TestOpen(t *testing.T) {
	now := unixIn(1700000000, "CET", 3600)
	c, mod := open(t, fakemod.Options{Credits: 50}, Config{
		Current: &testKey,
		Now:     func() time.Time { return now },
	})

	if got, want := c.FirmwareVersion(), "1.4.1"; got != want {
		t.Errorf("FirmwareVersion = %q, want %q", got, want)
	}
	if got, want := c.SerialNumber(), "FAKE000001"; got != want {
		t.Errorf("SerialNumber = %q, want %q", got, want)
	}
	if !mod.InApp() {
		t.Errorf("module not in application after Open")
	}
	if hmid, ok := mod.HMID(); !ok || hmid != testHMID {
		t.Errorf("HMID = %X (set: %v), want %X", hmid, ok, testHMID)
	}

	wantCurrent := append(testKey.Bytes[:], 0x01)
	if got := mod.Key(fakemod.SlotCurrent); !bytes.Equal(got, wantCurrent) {
		t.Errorf("current key slot = %x, want %x", got, wantCurrent)
	}
	zero := make([]byte, 17)
	if got := mod.Key(fakemod.SlotPrevious); !bytes.Equal(got, zero) {
		t.Errorf("previous key slot = %x, want cleared", got)
	}
	if got := mod.Key(fakemod.SlotTemp); !bytes.Equal(got, zero) {
		t.Errorf("temp key slot = %x, want cleared", got)
	}
	if got, want := mod.TimePayload(), timePayload(now); !bytes.Equal(got, want) {
		t.Errorf("time payload = %x, want %x", got, want)
	}
	if err := c.Err(); err != nil {
		t.Errorf("Err() = %v", err)
	}
	if got := testutil.ToFloat64(upGauge); got != 1 {
		t.Errorf("up gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(infoGauge.WithLabelValues("1.4.1", "FAKE000001")); got != 1 {
		t.Errorf("info gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(dutyCycle); got != 25 {
		t.Errorf("duty cycle gauge = %v, want 25", got)
	}

	// The first frames must follow the documented handshake.
	var cmds []string
	for _, f := range mod.Frames() {
		cmds = append(cmds, fmt.Sprintf("%d/%02x", f.Dst, f.Cmd))
	}
	want := "0/03 0/02 0/06 0/0b 0/0a 0/0e 1/03 1/0f 1/0b 1/00"
	if got := strings.Join(cmds, " "); !strings.HasPrefix(got, want) {
		t.Errorf("handshake = %s, want prefix %s", got, want)
	}
	if !mod.NormalMode() {
		t.Errorf("module not switched to normal mode")
	}
	for _, f := range mod.Frames() {
		if f.Dst == 0 && f.Cmd == 0x06 && len(f.Payload) != 0 {
			t.Errorf("normal mode payload = %x, want empty", f.Payload)
		}
	}
	// CSMA/CA is disabled by default (FHEM csmaCa=0, Homegear 0A 00).
	if enabled, set := mod.CSMACA(); !set || enabled {
		t.Errorf("CSMA/CA = %v (set: %v), want disabled", enabled, set)
	}
}

func TestOpenCSMACA(t *testing.T) {
	for _, enable := range []bool{false, true} {
		t.Run(fmt.Sprintf("CSMACA=%v", enable), func(t *testing.T) {
			_, mod := open(t, fakemod.Options{}, Config{CSMACA: enable})
			var payloads []string
			for _, f := range mod.Frames() {
				if f.Dst == 0 && f.Cmd == 0x0a {
					payloads = append(payloads, fmt.Sprintf("%x", f.Payload))
				}
			}
			want := "00"
			if enable {
				want = "01"
			}
			if got := strings.Join(payloads, " "); got != want {
				t.Errorf("CSMA/CA payloads = %q, want %q", got, want)
			}
			if enabled, set := mod.CSMACA(); !set || enabled != enable {
				t.Errorf("module CSMA/CA = %v (set: %v), want %v", enabled, set, enable)
			}
		})
	}
}

func TestOpenNormalModeNACK(t *testing.T) {
	mod, rw := fakemod.New(fakemod.Options{
		OnOS: func(cmd byte, payload []byte) (byte, bool) {
			if cmd == 0x06 {
				return fakemod.AckNACK, true
			}
			return 0, false
		},
	})
	defer mod.Close()
	_, err := Open(rw, Config{HMID: testHMID, Timeout: time.Second, AnnounceWait: 200 * time.Millisecond})
	if !errors.Is(err, ErrNACK) || !strings.Contains(err.Error(), "normal mode") {
		t.Fatalf("Open = %v, want normal mode NACK", err)
	}
}

func TestOpenPreviousKey(t *testing.T) {
	prev := hmkey.FromPassphrase("old", 1)
	cur := hmkey.FromPassphrase("new", 2)
	_, mod := open(t, fakemod.Options{}, Config{Current: &cur, Previous: &prev})
	if got, want := mod.Key(fakemod.SlotCurrent), append(cur.Bytes[:], 2); !bytes.Equal(got, want) {
		t.Errorf("current = %x, want %x", got, want)
	}
	if got, want := mod.Key(fakemod.SlotPrevious), append(prev.Bytes[:], 1); !bytes.Equal(got, want) {
		t.Errorf("previous = %x, want %x", got, want)
	}
}

func TestOpenNoAES(t *testing.T) {
	_, mod := open(t, fakemod.Options{}, Config{})
	if got := mod.Key(fakemod.SlotCurrent); !bytes.Equal(got, make([]byte, 17)) {
		t.Errorf("current key slot = %x, want cleared", got)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestKeysNotLogged(t *testing.T) {
	var buf syncBuffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	prev := hmkey.FromPassphrase("previous", 1)
	c, _ := open(t, fakemod.Options{}, Config{Current: &testKey, Previous: &prev, Debug: true})
	c.Close()

	out := buf.String()
	if !strings.Contains(out, "redacted") {
		t.Errorf("debug log does not mention redacted key frames:\n%s", out)
	}
	for _, k := range []hmkey.Key{testKey, prev} {
		for _, enc := range []string{hex.EncodeToString(k.Bytes[:]), strings.ToUpper(hex.EncodeToString(k.Bytes[:]))} {
			if strings.Contains(out, enc) {
				t.Fatalf("key material %s found in log output:\n%s", enc, out)
			}
		}
		// Also guard against partial dumps (first 8 bytes).
		if strings.Contains(strings.ToLower(out), hex.EncodeToString(k.Bytes[:8])) {
			t.Fatalf("partial key material found in log output:\n%s", out)
		}
	}
}

func TestOpenAppAlreadyRunning(t *testing.T) {
	// The module was not reset: no announcement, application running.
	// Open must query the application, reboot into the bootloader and
	// re-enter the application.
	c, mod := open(t, fakemod.Options{StartInApp: true}, Config{})
	if !mod.InApp() {
		t.Fatal("module not in application")
	}
	var cmds []string
	for _, f := range mod.Frames() {
		cmds = append(cmds, fmt.Sprintf("%d/%02x", f.Dst, f.Cmd))
	}
	if got, want := strings.Join(cmds, " "), "0/00 0/03 0/03 0/02"; !strings.HasPrefix(got, want) {
		t.Errorf("frames = %s, want prefix %s", got, want)
	}
	if c.FirmwareVersion() != "1.4.1" {
		t.Errorf("FirmwareVersion = %q", c.FirmwareVersion())
	}
}

func TestOpenDualFirmware(t *testing.T) {
	for _, startInApp := range []bool{false, true} {
		t.Run(fmt.Sprintf("startInApp=%v", startInApp), func(t *testing.T) {
			mod, rw := fakemod.New(fakemod.Options{DualFirmware: true, StartInApp: startInApp})
			defer mod.Close()
			_, err := Open(rw, Config{HMID: testHMID, Timeout: time.Second, AnnounceWait: 200 * time.Millisecond})
			if !errors.Is(err, ErrDualCoProFirmware) {
				t.Fatalf("Open: got %v, want ErrDualCoProFirmware", err)
			}
			if !strings.Contains(err.Error(), "1.4") {
				t.Errorf("error message does not mention the classic firmware version: %v", err)
			}
		})
	}
}

func TestOpenSilentModule(t *testing.T) {
	mod, rw := fakemod.New(fakemod.Options{Mute: true})
	defer mod.Close()
	start := time.Now()
	_, err := Open(rw, Config{HMID: testHMID, Timeout: 100 * time.Millisecond, AnnounceWait: 50 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Open: got %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Open took %v", d)
	}
	// Open closes rw on failure, so the module must stop.
	select {
	case <-mod.Done():
	case <-time.After(time.Second):
		t.Error("module still running after failed Open")
	}
}

func TestAddPeer(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{Current: &testKey})
	ctx := context.Background()
	err := c.AddPeer(ctx, Peer{
		Addr:        testDevice,
		KeyIndex:    1,
		Channels:    7,
		AESChannels: []byte{1, 4},
		Wakeup:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := mod.Peers()[testDevice]
	if !ok {
		t.Fatal("peer not added")
	}
	if p.KeyIndex != 1 || !p.Wakeup {
		t.Errorf("peer = %+v, want key index 1, wakeup", p)
	}
	if got, want := p.AESChannels(7), []byte{1, 4}; !bytes.Equal(got, want) {
		t.Errorf("AES channels = %v, want %v", got, want)
	}

	// Exact payloads of the AES commands.
	var aesFrames []string
	for _, f := range mod.Frames() {
		if f.Dst == 1 && (f.Cmd == 0x09 || f.Cmd == 0x0a || f.Cmd == 0x06) {
			aesFrames = append(aesFrames, fmt.Sprintf("%02x:%x", f.Cmd, f.Payload))
		}
	}
	want := []string{"06:38f59c010100", "0a:38f59c0002030506", "09:38f59c0104"}
	if got := strings.Join(aesFrames, " "); got != strings.Join(want, " ") {
		t.Errorf("peer frames = %s, want %s", got, strings.Join(want, " "))
	}

	// No AES channels at all: only add + disable.
	other := [3]byte{1, 2, 3}
	if err := c.AddPeer(ctx, Peer{Addr: other, Channels: 3}); err != nil {
		t.Fatal(err)
	}
	if got := mod.Peers()[other].AESChannels(64); len(got) != 61 || got[0] != 3 {
		// channels 3..63 keep the module default (enabled), 0..2 disabled
		t.Errorf("AES channels = %v", got)
	}
	frames := mod.Frames()
	last := frames[len(frames)-1]
	if last.Cmd != 0x0a {
		t.Errorf("last frame = %v, want peer remove AES (no empty enable AES)", last)
	}
}

func bidiPacket(cnt byte) *bidcos.Packet {
	return &bidcos.Packet{
		Msgcnt:  cnt,
		Flags:   bidcos.BiDi | bidcos.RepeatEnable,
		Cmd:     0x11,
		Source:  testHMID,
		Dest:    testDevice,
		Payload: []byte{0x86, 0x04, 0x2b},
	}
}

func TestSendBiDi(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})
	resp, err := c.Send(context.Background(), bidiPacket(0x42).Encode())
	if err != nil {
		t.Fatal(err)
	}
	if resp[0] != 0x01 {
		t.Errorf("response status = %#x, want 0x01 (normalised)", resp[0])
	}
	pkt, err := bidcos.Decode(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := pkt.Auth(); got != bidcos.Unauthenticated {
		t.Errorf("Auth = %v, want unauthenticated", got)
	}
	if pkt.Cmd != bidcos.Ack || pkt.Msgcnt != 0x42 || pkt.Source != testDevice || pkt.Dest != testHMID {
		t.Errorf("unexpected response %+v", pkt)
	}
	if pkt.RSSI() != -fakemod.DefaultRSSI {
		t.Errorf("RSSI = %d", pkt.RSSI())
	}
	sent := mod.Sent()
	if len(sent) != 1 || sent[0][0] != 0x42 || sent[0][2] != 0x11 {
		t.Errorf("module sent %x", sent)
	}
}

func TestSendNoBiDi(t *testing.T) {
	c, _ := open(t, fakemod.Options{}, Config{})
	pkt := bidiPacket(1)
	pkt.Flags = bidcos.RepeatEnable
	resp, err := c.Send(context.Background(), pkt.Encode())
	if err != nil || resp != nil {
		t.Errorf("Send = %x, %v; want nil, nil", resp, err)
	}
}

func TestSendAckStatuses(t *testing.T) {
	for _, tt := range []struct {
		status   byte
		wantErr  error
		wantAuth bidcos.Auth
		wantResp bool
	}{
		{fakemod.AckWithResponseAESOK, nil, bidcos.AuthOK, true},
		{fakemod.AckWithResponseAESFail, ErrAESFailed, bidcos.AuthFailed, true},
		{fakemod.AckEUnknown, ErrNoResponse, 0, false},
		{fakemod.AckENoCredits, ErrNoCredits, 0, false},
		{fakemod.AckNACK, ErrNACK, 0, false},
	} {
		t.Run(fmt.Sprintf("%#02x", tt.status), func(t *testing.T) {
			c, _ := open(t, fakemod.Options{
				OnSend: func(msg []byte) (byte, []byte) {
					if tt.wantResp {
						return tt.status, fakemod.DeviceAck(msg)
					}
					return tt.status, nil
				},
			}, Config{})
			resp, err := c.Send(context.Background(), bidiPacket(7).Encode())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Send: got err %v, want %v", err, tt.wantErr)
			}
			if !tt.wantResp {
				if resp != nil {
					t.Errorf("unexpected response %x", resp)
				}
				return
			}
			pkt, err := bidcos.Decode(resp)
			if err != nil {
				t.Fatal(err)
			}
			if got := pkt.Auth(); got != tt.wantAuth {
				t.Errorf("Auth = %v, want %v", got, tt.wantAuth)
			}
		})
	}
}

func TestSendRetry(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	c, mod := open(t, fakemod.Options{
		OnSend: func(msg []byte) (byte, []byte) {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			switch attempts {
			case 1:
				return fakemod.AckEInProgress, nil
			case 2:
				return fakemod.AckECSMACA, nil
			}
			return fakemod.AckWithResponse, fakemod.DeviceAck(msg)
		},
	}, Config{})
	inProgress := testutil.ToFloat64(sendRetries.WithLabelValues("in_progress"))
	csma := testutil.ToFloat64(sendRetries.WithLabelValues("csma_ca"))
	resp, err := c.Send(context.Background(), bidiPacket(9).Encode())
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("no response")
	}
	if got := len(mod.Sent()); got != 3 {
		t.Errorf("module transmitted %d times, want 3", got)
	}
	if got := testutil.ToFloat64(sendRetries.WithLabelValues("in_progress")) - inProgress; got != 1 {
		t.Errorf("in_progress retries = %v, want 1", got)
	}
	if got := testutil.ToFloat64(sendRetries.WithLabelValues("csma_ca")) - csma; got != 1 {
		t.Errorf("csma_ca retries = %v, want 1", got)
	}
}

func TestSendBusyGivesUp(t *testing.T) {
	c, _ := open(t, fakemod.Options{
		OnSend: func([]byte) (byte, []byte) { return fakemod.AckECSMACA, nil },
	}, Config{})
	c.retryWindow = 500 * time.Millisecond
	c.retryBackoff = 50 * time.Millisecond
	_, err := c.Send(context.Background(), bidiPacket(1).Encode())
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
	if c.Err() != nil {
		t.Errorf("connection failed: %v", c.Err())
	}
}

func TestSendContextTimeout(t *testing.T) {
	var mu sync.Mutex
	swallow := true
	c, _ := open(t, fakemod.Options{
		OnSend: func(msg []byte) (byte, []byte) {
			mu.Lock()
			defer mu.Unlock()
			if swallow {
				return fakemod.NoAck, nil
			}
			return fakemod.AckOK, nil
		},
	}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Send(ctx, bidiPacket(1).Encode())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Send returned after %v", d)
	}

	// The connection stays usable.
	mu.Lock()
	swallow = false
	mu.Unlock()
	pkt := bidiPacket(2)
	pkt.Flags = bidcos.RepeatEnable
	if _, err := c.Send(context.Background(), pkt.Encode()); err != nil {
		t.Fatalf("Send after timeout: %v", err)
	}
}

func TestSendModuleTimeout(t *testing.T) {
	c, _ := open(t, fakemod.Options{
		OnSend: func([]byte) (byte, []byte) { return fakemod.NoAck, nil },
	}, Config{Timeout: 100 * time.Millisecond})
	c.sendTimeout = 200 * time.Millisecond
	_, err := c.Send(context.Background(), bidiPacket(1).Encode())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}
}

func TestStaleAckDropped(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})
	before := testutil.ToFloat64(staleAcks)
	// An ack for a counter which is not outstanding.
	stale := fakemod.Frame{Dst: 1, Cnt: 0xee, Cmd: 0x04, Payload: []byte{0x01}}
	if err := mod.WriteRaw(stale.Encode()); err != nil {
		t.Fatal(err)
	}
	pkt := bidiPacket(1)
	if _, err := c.Send(context.Background(), pkt.Encode()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(staleAcks) - before; got != 1 {
		t.Errorf("stale acks = %v, want 1", got)
	}
}

func TestRecv(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})
	// HM-CC-RT-DN INFO_LEVEL (status) event, AES-signed.
	msg := append([]byte{0x2d, 0xa2, 0x10}, testDevice[:]...)
	msg = append(msg, testHMID[:]...)
	msg = append(msg, 0x06, 0x01, 0x2a, 0x00, 0x20)
	if err := mod.InjectRecv(0x12, msg); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-c.Recv():
		pkt, err := bidcos.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		if pkt.Auth() != bidcos.AuthOK {
			t.Errorf("Auth = %v, want aes-ok", pkt.Auth())
		}
		if pkt.Source != testDevice || pkt.Cmd != bidcos.Info || pkt.Msgcnt != 0x2d {
			t.Errorf("unexpected packet %+v", pkt)
		}
	case <-time.After(time.Second):
		t.Fatal("no frame received")
	}

	if err := mod.InjectRecv(0x03, msg); err != nil {
		t.Fatal(err)
	}
	b := <-c.Recv()
	if pkt, _ := bidcos.Decode(b); pkt.Auth() != bidcos.AuthFailed {
		t.Errorf("Auth = %v, want aes-failed", pkt.Auth())
	}
}

func TestRecvSlowConsumer(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})
	before := testutil.ToFloat64(recvDropped)
	msg := append([]byte{0x01, 0x84, 0x10}, testDevice[:]...)
	msg = append(msg, testHMID[:]...)
	for i := 0; i < recvBufferSize+10; i++ {
		if err := mod.InjectRecv(0x11, msg); err != nil {
			t.Fatal(err)
		}
	}
	// Acks keep flowing although nobody reads Recv.
	pkt := bidiPacket(3)
	if _, err := c.Send(context.Background(), pkt.Encode()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(recvDropped) - before; got != 10 {
		t.Errorf("dropped = %v, want 10", got)
	}
}

func TestConcurrentSend(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})

	stop := make(chan struct{})
	var injectWG sync.WaitGroup
	injectWG.Add(1)
	go func() {
		defer injectWG.Done()
		msg := append([]byte{0x01, 0x84, 0x10}, testDevice[:]...)
		msg = append(msg, testHMID[:]...)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := mod.InjectRecv(0x01, msg); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	var recvd int
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		for range c.Recv() {
			recvd++
		}
	}()

	const goroutines, perG = 8, 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perG)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				cnt := byte(g*perG + i + 1)
				resp, err := c.Send(context.Background(), bidiPacket(cnt).Encode())
				if err != nil {
					errs <- err
					continue
				}
				pkt, err := bidcos.Decode(resp)
				if err != nil {
					errs <- err
					continue
				}
				// The response must belong to our request.
				if pkt.Msgcnt != cnt {
					errs <- fmt.Errorf("got response for counter %d, want %d", pkt.Msgcnt, cnt)
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	injectWG.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := len(mod.Sent()); got != goroutines*perG {
		t.Errorf("module transmitted %d frames, want %d", got, goroutines*perG)
	}
	c.Close()
	<-recvDone
	if recvd == 0 {
		t.Error("no frames received during concurrent sends")
	}
}

func TestModuleCrash(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})
	if err := mod.Crash(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-c.Recv():
		if ok {
			t.Fatal("unexpected frame")
		}
	case <-time.After(time.Second):
		t.Fatal("Recv not closed after module crash")
	}
	if err := c.Err(); !errors.Is(err, ErrModuleReset) {
		t.Errorf("Err() = %v, want ErrModuleReset", err)
	}
	if _, err := c.Send(context.Background(), bidiPacket(1).Encode()); !errors.Is(err, ErrModuleReset) {
		t.Errorf("Send after crash: %v, want ErrModuleReset", err)
	}
	if got := testutil.ToFloat64(upGauge); got != 0 {
		t.Errorf("up gauge = %v, want 0", got)
	}
}

func TestReadError(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})
	mod.Close() // module side of the pipe goes away
	select {
	case <-c.Recv():
	case <-time.After(time.Second):
		t.Fatal("Recv not closed after read error")
	}
	if c.Err() == nil {
		t.Error("Err() = nil after read error")
	}
}

func TestClose(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c.Err(), ErrClosed) {
		t.Errorf("Err() = %v, want ErrClosed", c.Err())
	}
	if _, ok := <-c.Recv(); ok {
		t.Error("Recv not closed")
	}
	select {
	case <-mod.Done():
	case <-time.After(time.Second):
		t.Error("module still running after Close")
	}
	if _, err := c.Send(context.Background(), bidiPacket(1).Encode()); !errors.Is(err, ErrClosed) {
		t.Errorf("Send after Close: %v", err)
	}
}

func TestSetTime(t *testing.T) {
	c, mod := open(t, fakemod.Options{}, Config{})
	ts := unixUTC(1487343971)
	if err := c.SetTime(context.Background(), ts); err != nil {
		t.Fatal(err)
	}
	if got, want := mod.TimePayload(), []byte{0x58, 0xa7, 0x11, 0x63, 0x00}; !bytes.Equal(got, want) {
		t.Errorf("time payload = %x, want %x", got, want)
	}
}

func TestCreditsPolling(t *testing.T) {
	_, _ = open(t, fakemod.Options{Credits: 9}, Config{CreditsInterval: 20 * time.Millisecond})
	deadline := time.Now().Add(time.Second)
	for testutil.ToFloat64(dutyCycle) != 4.5 {
		if time.Now().After(deadline) {
			t.Fatalf("duty cycle gauge = %v, want 4.5", testutil.ToFloat64(dutyCycle))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
