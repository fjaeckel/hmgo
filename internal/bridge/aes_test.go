// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	bidcosaes "github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/uartgw"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// aesDevices emulates AES devices behind an RPI-RF-MOD: they answer the
// bridge's challenges to their messages with the configured key, and
// (switch) answer LEVEL_SET either with a forged plain ACK_STATUS or,
// like a real AES device, with a challenge.
type aesDevices struct {
	t      *testing.T
	signer *bidcosaes.Signer

	mu      sync.Mutex
	pending map[byte][]byte // message counter → our device message awaiting the bridge's challenge
	// plainSwitch: the switch answers LEVEL_SET with an unauthenticated
	// ACK_STATUS (as a forger would).
	plainSwitch bool
	// device, if set, answers first (an emulated device, e.g.
	// fakemod.Device.Handle; nil answers fall through).
	device func(m []byte) [][]byte
}

func (a *aesDevices) onLLMACSend(m []byte) [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(m) >= 17 && m[2] == 0x02 && m[9] == 0x04 { // the bridge's challenge
		orig := a.pending[m[0]]
		if orig == nil {
			return nil
		}
		var ch [6]byte
		copy(ch[:], m[10:16])
		r, err := a.signer.Respond(m[16]/2, ch, orig)
		if err != nil {
			a.t.Errorf("Respond: %v", err)
			return nil
		}
		return [][]byte{msg(m[0], 0xa0, 0x03, [3]byte(m[6:9]), central, r[:]...)}
	}
	if a.device != nil {
		if r := a.device(m); r != nil {
			return r
		}
	}
	if a.plainSwitch && len(m) >= 12 && m[2] == 0x11 && bytes.Equal(m[6:9], plug[:]) {
		return [][]byte{msg(m[0], 0x80, 0x02, plug, central, 0x01, m[10], m[11], 0x00, 0x41)}
	}
	return fakemod.DefaultOnLLMACSend(m)
}

// send makes the device send m (counter m[0]); a challenge of the bridge
// is answered.
func (a *aesDevices) send(mod *fakemod.Module, m []byte) {
	a.t.Helper()
	a.mu.Lock()
	if a.pending == nil {
		a.pending = map[byte][]byte{}
	}
	a.pending[m[0]] = m
	a.mu.Unlock()
	if err := mod.InjectLLMACRecv(fakemod.DefaultRSSI, m); err != nil {
		a.t.Fatal(err)
	}
}

// newDualEnv is newEnvWith with an emulated RPI-RF-MOD (software ACK
// and AES in uartgw.DualConn).
func newDualEnv(t *testing.T, cfgJSON string, devs *aesDevices, opt func(*Options)) *env {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	cur, prev := cfg.Keys()
	devs.t, devs.signer = t, bidcosaes.NewSigner(*cur)
	mod, rw := fakemod.New(fakemod.Options{DualCoPro: true, OnLLMACSend: devs.onLLMACSend})
	t.Cleanup(func() { mod.Close() })
	conn, err := uartgw.OpenModule(rw, uartgw.Config{
		HMID: cfg.CentralAddr(), Current: cur, Previous: prev, Signer: devs.signer,
		Timeout: time.Second, AnnounceWait: 200 * time.Millisecond, ResponseTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	opts := Options{
		Config:      cfg,
		Radio:       conn.(*uartgw.DualConn),
		RetryDelay:  50 * time.Millisecond,
		Logf:        t.Logf,
		ClockSynced: func() bool { return true },
	}
	if opt != nil {
		opt(&opts)
	}
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	pub := &fakePub{}
	b.SetPublisher(pub)
	e := &env{t: t, mod: mod, b: b, pub: pub, run: make(chan error, 1)}
	e.cancel = cancel
	go func() { e.run <- b.Run(ctx) }()
	t.Cleanup(e.stop)
	return e
}

// aesContactConfig is testConfig with AES on the window contact.
var aesContactConfig = strings.Replace(testConfig, `"address": "4C5D6E"}`, `"address": "4C5D6E", "aes": true}`, 1)

// Frames anyone can forge with the address of an AES contact, addressed
// to the central, which neither radio module challenges.
var forgedContactFrames = []struct {
	name   string
	m      []byte
	reason string
}{
	// No BiDi: the device would not wait for a challenge.
	{"event without BiDi", msg(0x40, 0x84, 0x41, sco, central, 0x01, 0x1c, 0xc8), aesRejectAESChannel},
	{"INFO_LEVEL without BiDi", msg(0x41, 0x84, 0x10, sco, central, 0x06, 0x01, 0xc8, 0x00), aesRejectAESChannel},
	// The channel comes from the payload: one without AES is not
	// challenged.
	{"event for channel 63", msg(0x42, 0xa4, 0x41, sco, central, 0x3f, 0x1c, 0xc8), aesRejectChannel},
	// ACKs are never challenged; the decoders turn ACK_STATUS into state.
	{"ACK_STATUS", msg(0x43, 0xa4, 0x02, sco, central, 0x01, 0x01, 0xc8, 0x00), aesRejectAck},
}

// checkForgedIgnored checks that none of the forged "open" frames was
// applied: the authenticated "closed" event, processed after them, is
// the only message counted.
func checkForgedIgnored(t *testing.T, e *env) {
	t.Helper()
	e.waitFor("authenticated closed event", func() bool {
		st := e.state("hmbridge/4c5d6e/state")
		return st != nil && st["window_state"] == "closed"
	})
	st := e.state("hmbridge/4c5d6e/state")
	if st["messages_received"] != 1.0 || st["auth_status"] != "ok" {
		t.Errorf("state %v: the forged frames must not be counted", st)
	}
	if st := e.state("hmbridge/group/wohnzimmer/state"); st["window_open"] == true {
		t.Errorf("group state %v", st)
	}
}

// H1: an AES device's forged frames to the central change nothing, with
// the HM-MOD-RPI-PCB (which does not challenge them either).
func TestAESForgedFramesToCentralHMModRPIPCB(t *testing.T) {
	for _, tc := range forgedContactFrames {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.ToFloat64(metricAESRejected.WithLabelValues(tc.reason))
			e := newEnv(t, aesContactConfig, fakemod.Options{})
			e.inject(0x01, tc.m) // status 0x01: not authenticated
			e.inject(0x12, msg(0x50, 0xa4, 0x41, sco, central, 0x01, 0x1d, 0x00))
			checkForgedIgnored(t, e)
			if got := testutil.ToFloat64(metricAESRejected.WithLabelValues(tc.reason)) - before; got != 1 {
				t.Errorf("aes_rejected_total{reason=%q} += %v, want 1", tc.reason, got)
			}
		})
	}
}

// H1 with the RPI-RF-MOD (software AES): the forged frames are not
// challenged by DualConn and reach the bridge unauthenticated.
func TestAESForgedFramesToCentralRPIRFMod(t *testing.T) {
	for _, tc := range forgedContactFrames {
		t.Run(tc.name, func(t *testing.T) {
			devs := &aesDevices{}
			e := newDualEnv(t, aesContactConfig, devs, nil)
			if err := e.mod.InjectLLMACRecv(fakemod.DefaultRSSI, tc.m); err != nil {
				t.Fatal(err)
			}
			// The real contact's event is challenged and authenticated.
			devs.send(e.mod, msg(0x50, 0xa4, 0x41, sco, central, 0x01, 0x1d, 0x00))
			checkForgedIgnored(t, e)
		})
	}
}

// Frames of the same kinds from a device without AES are used as before
// (except the channel the contact does not have).
func TestNonAESDeviceFramesStillApplied(t *testing.T) {
	e := newEnv(t, testConfig, fakemod.Options{})
	e.inject(0x01, msg(0x40, 0x84, 0x41, sco, central, 0x3f, 0x1c, 0xc8)) // channel 63: decode error
	e.inject(0x01, msg(0x41, 0x84, 0x41, sco, central, 0x01, 0x1c, 0xc8))
	e.waitFor("open", func() bool { return e.state("hmbridge/4c5d6e/state")["window_state"] == "open" })
	e.inject(0x01, msg(0x42, 0x80, 0x02, sco, central, 0x01, 0x01, 0x00, 0x00))
	e.waitFor("closed by ACK_STATUS", func() bool { return e.state("hmbridge/4c5d6e/state")["window_state"] == "closed" })
}

// Remotes: an unauthenticated KEY_EVENT is only accepted on a button
// without AES; a button number the remote does not have is rejected.
func TestAESRemoteChannels(t *testing.T) {
	cfg := strings.Replace(devConf, `"address": "73788E", "aes": true}`, `"address": "73788E", "aes_channels": [1]}`, 1)
	e := newEnv(t, cfg, fakemod.Options{})
	e.inject(0x01, msg(0x01, 0xa4, 0x40, pb, central, 0x01, 0x01)) // button 1 (AES), forged
	e.inject(0x01, msg(0x02, 0xa4, 0x40, pb, central, 0x05, 0x02)) // button 5: the PB-2 has 2
	e.inject(0x01, msg(0x03, 0xa4, 0x40, pb, central, 0x02, 0x03)) // button 2: no AES
	e.waitFor("button 2", func() bool { return len(e.pub.eventLog()) > 0 })
	time.Sleep(50 * time.Millisecond)
	if got := fmt.Sprint(e.pub.eventLog()); got != "[button_2 press_short]" {
		t.Errorf("events %s", got)
	}
}

// L3: a plain (unauthenticated) ACK to a command on an AES channel is
// not taken as success; the device's real, authenticated answer is.
func TestAESCommandNeedsAuthenticatedAck(t *testing.T) {
	check := func(t *testing.T, e *env) {
		t.Helper()
		e.b.Commands() <- ha.Command{Addr: "6a9d15", Kind: ha.SetSwitch, On: true}
		e.waitFor("command failed", func() bool {
			st := e.state("hmbridge/6a9d15/state")
			return st != nil && strings.HasPrefix(fmt.Sprint(st["last_command"]), "failed")
		})
		st := e.state("hmbridge/6a9d15/state")
		if st["on"] == true || !strings.Contains(fmt.Sprint(st["last_command"]), "not AES-authenticated") {
			t.Errorf("state %v", st)
		}
	}
	t.Run("HM-MOD-RPI-PCB", func(t *testing.T) {
		a := &actuatorDevices{}
		e := newEnvPlainAES(t, devConf, fakemod.Options{OnSend: a.onSend}, func(o *Options) { o.StatusPollStagger = time.Hour })
		check(t, e)
	})
	t.Run("RPI-RF-MOD", func(t *testing.T) {
		e := newDualEnv(t, devConf, &aesDevices{plainSwitch: true}, func(o *Options) { o.StatusPollStagger = time.Hour })
		check(t, e)
	})
	t.Run("authenticated", func(t *testing.T) {
		a := &actuatorDevices{}
		e := newEnvWith(t, devConf, fakemod.Options{OnSend: a.onSend}, func(o *Options) { o.StatusPollStagger = time.Hour })
		e.b.Commands() <- ha.Command{Addr: "6a9d15", Kind: ha.SetSwitch, On: true}
		e.waitFor("switched on", func() bool {
			st := e.state("hmbridge/6a9d15/state")
			return st != nil && st["on"] == true && st["last_command"] == "ok: switch on"
		})
	})
}

// M1: spoofed time requests are answered at most timeRespBurst times,
// then once a minute per device.
func TestTimeRequestRateLimit(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	e := newEnvWith(t, testConfig, fakemod.Options{}, func(o *Options) {
		o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	})
	before := testutil.ToFloat64(metricRateLimited.WithLabelValues(rateKindTimeResponse))
	responses := func() int {
		n := 0
		for _, m := range e.mod.Sent() {
			if m[2] == 0x3f && bytes.Equal(m[6:9], tc[:]) {
				n++
			}
		}
		return n
	}
	for i := 0; i < 10; i++ {
		e.inject(0x01, msg(byte(0x40+i), 0xa6, 0x3f, tc, central))
		e.waitFor("processed", func() bool {
			return testutil.ToFloat64(metricRateLimited.WithLabelValues(rateKindTimeResponse))-before+float64(responses()) >= float64(i+1)
		})
	}
	if n := responses(); n != timeRespBurst {
		t.Errorf("%d time responses, want %d", n, timeRespBurst)
	}
	mu.Lock()
	now = now.Add(timeRespRefill)
	mu.Unlock()
	e.inject(0x01, msg(0x60, 0xa6, 0x3f, tc, central))
	e.waitFor("answered again after a minute", func() bool { return responses() == timeRespBurst+1 })
}

// L6: random unknown sender addresses do not log a line each.
func TestUnknownSenderLogRateLimited(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	e := newEnvWith(t, testConfig, fakemod.Options{}, func(o *Options) {
		o.Logf = func(f string, a ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(f, a...))
			mu.Unlock()
		}
	})
	seen := func(addr string) bool {
		for _, u := range e.b.Snapshot().Unknown {
			if u.Address == addr {
				return true
			}
		}
		return false
	}
	for i := 0; i < 300; i++ {
		e.inject(0x01, msg(0x01, 0x84, 0x41, [3]byte{0x55, byte(i >> 8), byte(i)}, central, 0x01, 0x01, 0x00))
		if i%32 == 31 {
			// Stay below the radio layer's receive buffer (64 frames): a
			// slow (race-instrumented) event loop must not lose frames.
			addr := fmt.Sprintf("55%04x", i)
			e.waitFor("batch processed", func() bool { return seen(addr) })
		}
	}
	e.waitFor("all processed", func() bool { return seen("55012b") }) // the last one
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "unconfigured device") {
			n++
		}
	}
	if n != 10 {
		t.Errorf("%d log lines about unconfigured devices, want 10 (rate limit)", n)
	}
}
