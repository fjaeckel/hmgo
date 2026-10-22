// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

// Regression tests from the security review of pairing.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/store"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

func httpBody(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// onSendHook lets a test replace the radio's answers after the
// environment (and its module) exists.
type onSendHook struct {
	fn atomic.Pointer[func(msg []byte) (byte, []byte)]
}

func (h *onSendHook) onSend(msg []byte) (byte, []byte) {
	if f := h.fn.Load(); f != nil {
		return (*f)(msg)
	}
	return fakemod.DefaultOnSend(msg)
}

func (h *onSendHook) set(f func(msg []byte) (byte, []byte)) { h.fn.Store(&f) }

func to(msg []byte, addr [3]byte) bool { return len(msg) >= 9 && bytes.Equal(msg[6:9], addr[:]) }

// configSent returns the CONFIG payloads sent to dst, without status
// requests (01 0e, actuators are polled after startup).
func (e *env) configSent(dst [3]byte) []string {
	var res []string
	for _, p := range e.payloadsSent(dst, 0x01) {
		if p != "010e" {
			res = append(res, p)
		}
	}
	return res
}

// A rename to invalid UTF-8 used to panic in prometheus WithLabelValues
// with b.mu held (the event loop died). ParseCommand refuses it; the
// bridge refuses it too, and metric labels are always made valid.
func TestRenameInvalidNameDoesNotCrash(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	if err := saveDevices(file, []store.Device{{Name: "Stehlampe", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "6A9D15", Paired: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	e := newEnvWith(t, pairingConfig(filepath.Dir(file)), fakemod.Options{}, fastPairing)
	e.waitFor("name", func() bool { return string(e.pub.get("hmbridge/6a9d15/name")) == "Stehlampe" })
	for _, name := range []string{"K\xfcche", "a‮b", "a\x00b", strings.Repeat("x", 65), ""} {
		e.b.Commands() <- ha.Command{Kind: ha.Rename, Addr: "6a9d15", Name: name}
	}
	// The event loop still works and the name is unchanged.
	e.b.Commands() <- ha.Command{Kind: ha.Rename, Addr: "6a9d15", Name: "Leselampe"}
	e.waitFor("renamed", func() bool { return string(e.pub.get("hmbridge/6a9d15/name")) == "Leselampe" })
	e.waitDevices(file, "stored", func(d []store.Device) bool { return len(d) == 1 && d[0].Name == "Leselampe" })

	// A device whose name is not valid UTF-8 (however it got there)
	// still gets valid metric labels.
	dc := config.Device{Name: "Bad\xffName", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "6A9D16"}
	rc, err := e.b.cfg.ResolveStoreDevice(dc)
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.b.newDevice(rc, &store.Device{})
	if err != nil {
		t.Fatal(err)
	}
	e.b.mu.Lock()
	e.b.updateMetrics(d)
	e.b.mu.Unlock()
	if d.cfg.Name != "Bad�Name" {
		t.Errorf("name %q", d.cfg.Name)
	}
	deleteDeviceMetrics(d.desc.Addr)
	if labelValue("ok") != "ok" || labelValue("\xff") != "�" {
		t.Error("labelValue")
	}
}

// Priority jobs (time responses, pairing steps) run before other jobs,
// in the order they were pushed.
func TestPriorityJobsFIFO(t *testing.T) {
	q := newJobQueue()
	q.push(job{key: "a"})
	q.push(job{key: "p1", priority: true})
	q.push(job{key: "b"})
	q.push(job{key: "p2", priority: true})
	q.push(job{key: "p3", priority: true})
	q.push(job{key: "p1", priority: true}) // replaced: moves behind p3
	if _, ok := q.remove("p2"); !ok {
		t.Error("remove p2")
	}
	if _, ok := q.remove("p2"); ok {
		t.Error("removed p2 twice")
	}
	var got []string
	for {
		j, ok := q.pop()
		if !ok {
			break
		}
		got = append(got, j.key)
	}
	if fmt.Sprint(got) != "[p3 p1 a b]" {
		t.Errorf("order %v", got)
	}
}

// DEVICE_INFOs with impossible (spoofed) sender addresses are refused
// before anything is sent.
func TestPairSpoofedSource(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	// An unusable record (key index 1 is not configured) keeps its
	// address reserved for its model.
	if err := saveDevices(file, []store.Device{{Name: "Alter Taster", Model: "HM-PB-2-WM55", Address: "4C5D70", KeyIndex: 1}}); err != nil {
		t.Fatal(err)
	}
	e := newEnvWith(t, pairingConfig(filepath.Dir(file)), fakemod.Options{}, fastPairing)
	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
	before := testutil.ToFloat64(metricPairing.WithLabelValues("invalid_source"))
	for _, src := range [][3]byte{{}, central, {0xff, 0xff, 0xff}} {
		e.inject(0x01, deviceInfo(src, [3]byte{}, 0x00c7, "NEQ0000002"))
		want := fmt.Sprintf("ignoring DEVICE_INFO from %x", src[:])
		e.waitFor(want, func() bool { return strings.HasPrefix(e.lastResult(), want) })
	}
	if got := testutil.ToFloat64(metricPairing.WithLabelValues("invalid_source")) - before; got != 3 {
		t.Errorf("invalid_source counted %v times", got)
	}
	e.inject(0x01, deviceInfo(newSCo, [3]byte{}, 0x00c7, "NEQ0000002"))
	e.waitFor("kept record conflict", func() bool {
		return strings.Contains(e.lastResult(), "has the address of Alter Taster (HM-PB-2-WM55")
	})
	if n := len(e.mod.Sent()); n != 0 {
		t.Errorf("%d messages sent", n)
	}
	if _, ok := e.mod.Peers()[[3]byte{}]; ok {
		t.Error("peer 000000 registered")
	}
	if st := e.pairingState(); st["active"] != true {
		t.Errorf("pairing mode ended: %v", st)
	}
}

// Rejections anyone in radio range can trigger are logged at most once
// per minute and kind.
func TestPairingRejectionLogRateLimited(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	e := newEnvWith(t, pairingConfig(t.TempDir()), fakemod.Options{}, func(o *Options) {
		fastPairing(o)
		o.Logf = func(f string, a ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(f, a...))
			mu.Unlock()
		}
	})
	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
	before := testutil.ToFloat64(metricPairing.WithLabelValues("unsupported_model"))
	for i := 0; i < 20; i++ {
		e.inject(0x01, deviceInfo([3]byte{0x10, 0x20, byte(i)}, [3]byte{}, 0x1234, "XEQ0000001"))
	}
	e.waitFor("all handled", func() bool {
		return testutil.ToFloat64(metricPairing.WithLabelValues("unsupported_model")) >= before+20
	})
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "which hmbridge does not support") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d log lines for 20 rejected DEVICE_INFOs", n)
	}
}

// A NACK in the middle of the pairing sequence fails the pairing and
// removes the device from the radio module again.
func TestPairNACKMidSequence(t *testing.T) {
	hook := &onSendHook{}
	e := newEnvWith(t, pairingConfig(t.TempDir()), fakemod.Options{OnSend: hook.onSend}, fastPairing)
	var n atomic.Int32
	hook.set(func(msg []byte) (byte, []byte) {
		if to(msg, newSCo) && msg[2] == 0x01 && n.Add(1) == 2 {
			// The device refuses CONFIG_WRITE_INDEX.
			return fakemod.AckWithResponse, append(fakemod.DeviceAck(msg)[:9], 0x80)
		}
		return fakemod.DefaultOnSend(msg)
	})
	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
	e.inject(0x01, deviceInfo(newSCo, [3]byte{}, 0x00c7, "NEQ0000002"))
	e.waitFor("failed", func() bool { return strings.HasPrefix(e.lastResult(), "pairing HM-Sec-SCo NEQ0000002 (4c5d70) failed") })
	e.waitFor("peer removed", func() bool { _, ok := e.mod.Peers()[newSCo]; return !ok })
	if got := e.payloadsSent(newSCo, 0x04); len(got) != 0 {
		t.Errorf("key exchange after a failed configuration: %v", got)
	}
	// Pairing mode stays on for another try.
	if st := e.pairingState(); st["active"] != true || st["status"] != "active" {
		t.Errorf("pairing state %v", st)
	}
}

// The outcome of a failed key exchange: a NACK of part 2 means the
// device keeps its old key; a lost answer to part 2 leaves it unknown.
func TestPairKeyExchangePart2(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status byte
		want   string
	}{
		{"nack", fakemod.AckWithResponse, "AES key exchange failed in part 2"},
		{"lost", fakemod.AckEUnknown, "part 2 was not confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := &onSendHook{}
			file := filepath.Join(t.TempDir(), "devices.json")
			e := newEnvWith(t, pairingConfig(filepath.Dir(file)), fakemod.Options{OnSend: hook.onSend}, fastPairing)
			hook.set(func(msg []byte) (byte, []byte) {
				if to(msg, newSCo) && msg[2] == 0x04 && msg[len(msg)-1]%2 == 1 {
					if tc.status == fakemod.AckWithResponse {
						return tc.status, append(fakemod.DeviceAck(msg)[:9], 0x80)
					}
					return tc.status, nil
				}
				return fakemod.DefaultOnSend(msg)
			})
			e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
			e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
			e.inject(0x01, deviceInfo(newSCo, [3]byte{}, 0x00c7, "NEQ0000002"))
			e.waitFor("paired", func() bool { return strings.HasPrefix(e.lastResult(), "paired HM-Sec-SCo NEQ0000002") })
			res := e.lastResult()
			if !strings.Contains(res, tc.want) || strings.Contains(res, "AES on channels") ||
				!strings.Contains(res, "system key handed out to serial NEQ0000002 at RSSI") {
				t.Errorf("result %q", res)
			}
			devs := e.waitDevices(file, "stored", func(d []store.Device) bool { return len(d) == 1 })
			if devs[0].KeyIndex != 0 || len(devs[0].AESChannels) != 0 {
				t.Errorf("stored %+v", devs[0])
			}
		})
	}
}

// A device that announces itself to our central address without being
// configured (paired by OpenCCU, or by hmbridge before an unpairing it
// never confirmed) is not factory-new: it already has the system key.
// It is not given the key again and is stored with the current index.
func TestPairAlreadyPairedToCentralNotConfigured(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	e := newEnvWith(t, pairingConfig(filepath.Dir(file)), fakemod.Options{}, fastPairing)
	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
	e.inject(0x01, deviceInfo(newSCo, central, 0x00c7, "NEQ0000002"))
	e.waitFor("paired", func() bool { return strings.HasPrefix(e.lastResult(), "paired HM-Sec-SCo NEQ0000002") })
	if got := e.payloadsSent(newSCo, 0x04); len(got) != 0 {
		t.Errorf("key exchange: %v", got)
	}
	res := e.lastResult()
	if strings.Contains(res, "handed out") || !strings.Contains(res, "assumed to use the system key (index 2)") ||
		!strings.Contains(res, "AES on channels [1] with key index 2") {
		t.Errorf("result %q", res)
	}
	if got := withoutReads(e.payloadsSent(newSCo, 0x01)); fmt.Sprint(got) != "[00050000000000 000802010a1a0b2b0c3c 0006 01050000000001 01080801 0106]" {
		t.Errorf("config messages %v", got)
	}
	devs := e.waitDevices(file, "stored", func(d []store.Device) bool { return len(d) == 1 })
	if devs[0].KeyIndex != 2 || fmt.Sprint(devs[0].AESChannels) != "[1]" {
		t.Errorf("stored %+v", devs[0])
	}
	if p := e.mod.Peers()[newSCo]; p.KeyIndex != 2 || fmt.Sprint(p.AESChannels(2)) != "[1]" {
		t.Errorf("peer %+v", p)
	}
}

// challengingDevices emulates devices with the system key and AES that
// challenge every message sent to them: the radio module can answer
// only while the device is registered as its peer with its key index.
// Without the peer, the device's challenge goes unanswered and it does
// not act.
func challengingDevices(e **env, addrs ...[3]byte) func([]byte) (byte, []byte) {
	return func(msg []byte) (byte, []byte) {
		for _, a := range addrs {
			if !to(msg, a) || *e == nil {
				continue
			}
			if p, ok := (*e).mod.Peers()[a]; !ok || p.KeyIndex != 2 {
				return fakemod.AckWithResponseAESFail, fakemod.DeviceAck(msg)
			}
			return fakemod.AckWithResponseAESOK, fakemod.DeviceAck(msg)
		}
		return fakemod.DefaultOnSend(msg)
	}
}

// Unpairing an AES device: its peer entry must stay in the radio
// module until the unpair messages were sent, and be removed afterwards
// (also when the config button is never pressed).
func TestUnpairAESDeviceKeepsPeerUntilSent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	paired := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := saveDevices(file, []store.Device{
		{Name: "Stehlampe", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "6A9D15", KeyIndex: 2, AESChannels: []int{1}, Paired: paired},
		{Name: "Bad Fenster", Model: "HM-Sec-SCo", Address: "4C5D70", Serial: "NEQ0000002", KeyIndex: 2, AESChannels: []int{1}, Paired: paired},
		{Name: "Flur Fenster", Model: "HM-Sec-SCo", Address: "4C5D71", Serial: "NEQ0000003", KeyIndex: 2, AESChannels: []int{1}, Paired: paired},
	}); err != nil {
		t.Fatal(err)
	}
	var e *env
	hook := &onSendHook{}
	e = newEnvWith(t, pairingConfig(filepath.Dir(file)), fakemod.Options{OnSend: hook.onSend}, func(o *Options) {
		fastPairing(o)
		o.PairingDuration = 2 * time.Second
	})
	flur := [3]byte{0x4c, 0x5d, 0x71}
	hook.set(challengingDevices(&e, newPlug, newSCo, flur))

	// Mains powered: unpaired at once, then forgotten by the module.
	e.b.Commands() <- ha.Command{Kind: ha.Unpair, Addr: "6a9d15"}
	e.waitFor("plug unpaired", func() bool { return strings.HasPrefix(e.lastResult(), "unpaired Stehlampe (6a9d15)") })
	e.waitFor("plug peer removed", func() bool { _, ok := e.mod.Peers()[newPlug]; return !ok })

	// Config-only: the peer stays until its config button is pressed.
	e.b.Commands() <- ha.Command{Kind: ha.Unpair, Addr: "4c5d70"}
	e.waitFor("waiting", func() bool { return strings.Contains(e.lastResult(), "press its config button within") })
	time.Sleep(50 * time.Millisecond)
	if _, ok := e.mod.Peers()[newSCo]; !ok {
		t.Fatal("contact's peer removed before its unpair messages")
	}
	// Another model claiming its address does not get them.
	conflicts := testutil.ToFloat64(metricPairing.WithLabelValues("address_conflict"))
	e.inject(0x01, deviceInfo(newSCo, central, 0x00d8, "XEQ0000009"))
	e.waitFor("conflict", func() bool {
		return testutil.ToFloat64(metricPairing.WithLabelValues("address_conflict")) == conflicts+1
	})
	if n := len(e.payloadsSent(newSCo, 0x01)); n != 0 {
		t.Fatalf("%d messages after a DEVICE_INFO of another model", n)
	}
	e.inject(0x01, deviceInfo(newSCo, central, 0x00c7, "NEQ0000002"))
	e.waitFor("contact unpaired", func() bool { return strings.HasPrefix(e.lastResult(), "unpaired Bad Fenster (4c5d70)") })
	if got := e.payloadsSent(newSCo, 0x01); fmt.Sprint(got) != "[00050000000000 000802000a000b000c00 0006]" {
		t.Errorf("unpair messages %v", got)
	}
	e.waitFor("contact peer removed", func() bool { _, ok := e.mod.Peers()[newSCo]; return !ok })

	// Config button never pressed: the peer is removed when the wait
	// ends.
	e.b.Commands() <- ha.Command{Kind: ha.Unpair, Addr: "4c5d71"}
	e.waitFor("waiting 2", func() bool { return strings.Contains(e.lastResult(), "Flur Fenster") })
	if _, ok := e.mod.Peers()[flur]; !ok {
		t.Fatal("peer removed before the wait ended")
	}
	if s := e.b.Snapshot().Pairing.UnpairWaiting; len(s) != 1 || !strings.HasPrefix(s[0], "Flur Fenster (4c5d71") {
		t.Errorf("unpair waiting %q", s)
	}
	e.waitFor("wait ended", func() bool { return strings.Contains(e.lastResult(), "config button was not pressed in time") })
	e.waitFor("expired peer removed", func() bool { _, ok := e.mod.Peers()[flur]; return !ok })
}

// Pairing an address supersedes its pending unpairing, and an
// unpairing supersedes a pending re-confirmation (pair job).
func TestPairDropsPendingUnpair(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	paired := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	other := [3]byte{0x6a, 0x9d, 0x16}
	if err := saveDevices(file, []store.Device{
		{Name: "Stehlampe", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "6A9D15", Paired: paired},
		{Name: "Leselampe", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "6A9D16", Paired: paired},
	}); err != nil {
		t.Fatal(err)
	}
	hook := &onSendHook{}
	release := make(chan struct{})
	var blocked atomic.Bool
	e := newEnvWith(t, pairingConfig(filepath.Dir(file)), fakemod.Options{OnSend: hook.onSend}, fastPairing)
	hook.set(func(msg []byte) (byte, []byte) {
		if to(msg, other) && msg[2] == 0x11 && blocked.CompareAndSwap(false, true) {
			<-release // keeps the worker busy
		}
		return fakemod.DefaultOnSend(msg)
	})
	defer func() {
		if !blocked.Load() {
			close(release)
		}
	}()
	e.waitFor("discovery", func() bool { return e.state("homeassistant/switch/hmbridge_6a9d16/switch/config") != nil })
	e.b.Commands() <- ha.Command{Kind: ha.SetSwitch, Addr: "6a9d16", On: true}
	e.waitFor("worker blocked", blocked.Load)

	e.b.Commands() <- ha.Command{Kind: ha.Unpair, Addr: "6a9d15"}
	e.waitFor("unpair queued", func() bool { return strings.Contains(e.lastResult(), "removed Stehlampe (6a9d15); unpairing") })
	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
	e.inject(0x01, deviceInfo(newPlug, [3]byte{}, 0x00d8, "NEQ0000005"))
	e.waitFor("configuring", func() bool { return e.pairingState()["status"] == "configuring" })
	close(release)
	e.waitFor("paired again", func() bool { return strings.HasPrefix(e.lastResult(), "paired HM-LC-Sw1-Pl-DN-R1 NEQ0000005") })
	if got := e.configSent(newPlug); len(got) < 3 || fmt.Sprint(got[:3]) != "[00050000000000 000802810a1a0b2b0c3c 0006]" ||
		strings.Contains(fmt.Sprint(got), "000802000a000b000c00") {
		t.Errorf("messages to the plug %v (the unpair messages must have been dropped)", got)
	}
	if _, ok := e.mod.Peers()[newPlug]; !ok {
		t.Error("peer of the re-paired plug missing")
	}
	if q := e.b.queue.len(); q != 0 {
		t.Errorf("queue %d", q)
	}
}

// An unpairing drops a queued re-confirmation (pair job) of the device.
func TestUnpairDropsPendingPair(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	paired := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	other := [3]byte{0x6a, 0x9d, 0x16}
	if err := saveDevices(file, []store.Device{
		{Name: "Stehlampe", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "6A9D15", Paired: paired},
		{Name: "Leselampe", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "6A9D16", Paired: paired},
	}); err != nil {
		t.Fatal(err)
	}
	hook := &onSendHook{}
	release := make(chan struct{})
	var blocked atomic.Bool
	e := newEnvWith(t, pairingConfig(filepath.Dir(file)), fakemod.Options{OnSend: hook.onSend}, fastPairing)
	hook.set(func(msg []byte) (byte, []byte) {
		if to(msg, other) && msg[2] == 0x11 && blocked.CompareAndSwap(false, true) {
			<-release
		}
		return fakemod.DefaultOnSend(msg)
	})
	defer func() {
		if !blocked.Load() {
			close(release)
		}
	}()
	e.waitFor("discovery", func() bool { return e.state("homeassistant/switch/hmbridge_6a9d16/switch/config") != nil })
	e.b.Commands() <- ha.Command{Kind: ha.SetSwitch, Addr: "6a9d16", On: true}
	e.waitFor("worker blocked", blocked.Load)

	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
	e.inject(0x01, deviceInfo(newPlug, central, 0x00d8, "NEQ0000005"))
	e.waitFor("configuring", func() bool { return e.pairingState()["status"] == "configuring" })
	e.b.Commands() <- ha.Command{Kind: ha.Unpair, Addr: "6a9d15"}
	e.waitFor("pair job dropped", func() bool { return e.pairingState()["status"] == "active" })
	close(release)
	e.waitFor("unpaired", func() bool { return strings.HasPrefix(e.lastResult(), "unpaired Stehlampe (6a9d15)") })
	if got := e.configSent(newPlug); fmt.Sprint(got) != "[00050000000000 000802000a000b000c00 0006]" {
		t.Errorf("messages to the plug %v (only the unpair messages)", got)
	}
	e.waitFor("peer removed", func() bool { _, ok := e.mod.Peers()[newPlug]; return !ok })
}
