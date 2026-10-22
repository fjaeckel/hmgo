// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/devstore"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm/pairing"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

var (
	newSCo  = [3]byte{0x4c, 0x5d, 0x70}
	newPlug = [3]byte{0x6a, 0x9d, 0x15}
)

func pairingConfig(devicesFile string) string {
	return fmt.Sprintf(`{
  "central_address": "1A2B3C",
  "reset": "none",
  "state_file": "",
  "devices_file": %q,
  "security": {"passphrase": "secret", "index": 2},
  "mqtt": {"broker": ""},
  "devices": [
    {"name": "Wohnzimmer Fenster", "model": "HM-Sec-SCo", "address": "4C5D6E", "serial": "NEQ0000001", "aes": true}
  ]
}`, devicesFile)
}

func deviceInfo(addr [3]byte, dst [3]byte, modelID uint16, serial string) []byte {
	p := pairing.EncodeDeviceInfo(pairing.DeviceInfo{Firmware: 0x14, ModelID: modelID, Serial: serial, Class: 0x80, Extra: []byte{0x01, 0x01, 0x00}})
	return msg(0x01, 0x84, 0x00, addr, dst, p...)
}

func (e *env) pairingState() map[string]any { return e.state("hmbridge/bridge/pairing") }

func (e *env) lastResult() string {
	s, _ := e.pairingState()["last_result"].(string)
	return s
}

// configSent returns the payloads (hex) of the frames of cmd sent to dst.
func (e *env) payloadsSent(dst [3]byte, cmd byte) []string {
	var res []string
	for _, m := range e.mod.Sent() {
		if len(m) >= 9 && m[2] == cmd && bytes.Equal(m[6:9], dst[:]) {
			res = append(res, hex.EncodeToString(m[9:]))
		}
	}
	return res
}

// cleared reports whether an empty (retained) message was the last one
// published on topic.
func (p *fakePub) cleared(topic string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.msgs[topic]
	return ok && len(b) == 0
}

func fastPairing(o *Options) {
	o.PairingTick = 10 * time.Millisecond
}

func TestPairNewContact(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	e := newEnvWith(t, pairingConfig(file), fakemod.Options{}, fastPairing)
	e.waitFor("bridge discovery", func() bool {
		return e.state("homeassistant/button/hmbridge_bridge/pair/config")["command_topic"] == "hmbridge/bridge/set/pair"
	})
	e.waitFor("pairing state idle", func() bool { return e.pairingState()["status"] == "idle" })

	// Outside pairing mode a DEVICE_INFO changes nothing.
	e.inject(0x01, deviceInfo(newSCo, [3]byte{}, 0x00c7, "NEQ0000002"))
	time.Sleep(50 * time.Millisecond)
	if n := len(e.payloadsSent(newSCo, 0x01)); n != 0 {
		t.Fatalf("%d config messages outside pairing mode", n)
	}

	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool {
		st := e.pairingState()
		return st["status"] == "active" && st["remaining"].(float64) > 50
	})
	e.inject(0x01, deviceInfo(newSCo, [3]byte{}, 0x00c7, "NEQ0000002"))
	e.waitFor("paired", func() bool { return strings.HasPrefix(e.lastResult(), "paired HM-Sec-SCo NEQ0000002") })

	// CCU-style configuration: central address, key exchange (2-byte
	// templates: the HM-MOD-RPI-PCB encrypts itself), AES_ACTIVE on
	// channel 1.
	want := []string{"00050000000000", "000802010a1a0b2b0c3c", "0006", "01050000000001", "01080801", "0106"}
	if got := e.payloadsSent(newSCo, 0x01); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("config messages %v, want %v", got, want)
	}
	if got := e.payloadsSent(newSCo, 0x04); fmt.Sprint(got) != "[0100 0101]" {
		t.Errorf("key exchange %v", got)
	}
	for _, m := range e.mod.Sent() {
		if bytes.Equal(m[6:9], newSCo[:]) && m[1] != 0xa0 {
			t.Errorf("flags %02x, want a0: %x", m[1], m)
		}
	}
	peer, ok := e.mod.Peers()[newSCo]
	if !ok || peer.KeyIndex != 2 || fmt.Sprint(peer.AESChannels(2)) != "[1]" {
		t.Errorf("module peer %+v (ok %v)", peer, ok)
	}
	devs, err := devstore.Load(file)
	if err != nil || len(devs) != 1 {
		t.Fatalf("devices file: %v %v", devs, err)
	}
	if d := devs[0]; d.Address != "4C5D70" || d.Model != "HM-Sec-SCo" || d.Serial != "NEQ0000002" || d.KeyIndex != 2 ||
		fmt.Sprint(d.AESChannels) != "[1]" || d.Firmware != "1.4" || d.ModelID != "00C7" || d.Name != "HM-Sec-SCo NEQ0000002" {
		t.Errorf("stored %+v", d)
	}
	if st := e.pairingState(); st["status"] != "idle" || st["active"] != false {
		t.Errorf("pairing mode still on: %v", st)
	}

	// Home Assistant knows the device at once, with the pairing entities.
	e.waitFor("discovery", func() bool {
		return e.state("homeassistant/binary_sensor/hmbridge_4c5d70/window_open/config") != nil &&
			e.state("homeassistant/button/hmbridge_4c5d70/unpair/config") != nil &&
			string(e.pub.get("hmbridge/4c5d70/name")) == "HM-Sec-SCo NEQ0000002" &&
			string(e.pub.get("hmbridge/4c5d70/availability")) == "online"
	})
	// … and the device is usable: an authenticated window event.
	e.inject(0x02, msg(0x10, 0xa4, 0x41, newSCo, central, 0x01, 0x05, 0xc8))
	e.waitFor("window open", func() bool { return e.state("hmbridge/4c5d70/state")["window_open"] == true })
	if s := e.b.Snapshot(); len(s.Unknown) != 0 || !s.Devices[len(s.Devices)-1].Paired {
		t.Errorf("snapshot %+v", s)
	}
}

func TestPairUnsupportedAndForeign(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	e := newEnvWith(t, pairingConfig(file), fakemod.Options{}, fastPairing)
	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })

	e.inject(0x01, deviceInfo(stranger, [3]byte{}, 0x1234, "XEQ0000001"))
	e.waitFor("unsupported", func() bool { return strings.Contains(e.lastResult(), "model ID 1234, which hmbridge does not support") })
	e.inject(0x01, deviceInfo(stranger, [3]byte{9, 9, 9}, 0x00c7, "XEQ0000001"))
	e.waitFor("foreign central", func() bool { return strings.Contains(e.lastResult(), "paired to another central (090909)") })
	// A different model at a configured address is not taken over.
	e.inject(0x01, deviceInfo(sco, [3]byte{}, 0x0095, "XEQ0000001"))
	e.waitFor("conflict", func() bool {
		return strings.Contains(e.lastResult(), "has the address of the configured Wohnzimmer Fenster")
	})
	if n := len(e.mod.Sent()); n != 0 {
		t.Errorf("%d messages sent", n)
	}
	if devs, _ := devstore.Load(file); len(devs) != 0 {
		t.Errorf("stored %v", devs)
	}
	if st := e.pairingState(); st["active"] != true {
		t.Errorf("pairing mode ended by a rejected device: %v", st)
	}
}

func TestPairingTimeout(t *testing.T) {
	e := newEnvWith(t, pairingConfig(filepath.Join(t.TempDir(), "d.json")), fakemod.Options{}, func(o *Options) {
		fastPairing(o)
		o.PairingDuration = 150 * time.Millisecond
	})
	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
	e.waitFor("timeout", func() bool {
		st := e.pairingState()
		return st["active"] == false && strings.Contains(e.lastResult(), "no device announced itself")
	})
	// Too late.
	e.inject(0x01, deviceInfo(newSCo, [3]byte{}, 0x00c7, "NEQ0000002"))
	time.Sleep(50 * time.Millisecond)
	if n := len(e.mod.Sent()); n != 0 {
		t.Errorf("%d messages sent after the timeout", n)
	}
}

func TestPairAlreadyKnown(t *testing.T) {
	e := newEnvWith(t, pairingConfig(filepath.Join(t.TempDir(), "d.json")), fakemod.Options{}, fastPairing)
	e.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
	// The contact from OpenCCU, already paired to our central address.
	e.inject(0x01, deviceInfo(sco, central, 0x00c7, "NEQ0000001"))
	e.waitFor("already known", func() bool {
		return strings.HasPrefix(e.lastResult(), "already known: Wohnzimmer Fenster (4c5d6e, HM-Sec-SCo, from config.json)")
	})
	if got := e.payloadsSent(sco, 0x01); fmt.Sprint(got) != "[00050000000000 000802010a1a0b2b0c3c 0006]" {
		t.Errorf("config messages %v", got)
	}
	if got := e.payloadsSent(sco, 0x04); len(got) != 0 {
		t.Errorf("key exchange for a known device: %v", got)
	}
	// Configured devices cannot be unpaired from Home Assistant.
	e.b.Commands() <- ha.Command{Kind: ha.Unpair, Addr: "4c5d6e"}
	e.waitFor("unpair rejected", func() bool {
		return strings.Contains(e.lastResult(), "is defined in config.json and cannot be unpaired")
	})
	if e.b.Snapshot().Devices[0].Name != "Wohnzimmer Fenster" {
		t.Error("configured device removed")
	}
}

func TestUnpairRenameAndReload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devices.json")
	paired := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	err := devstore.Save(file, []devstore.Device{
		{Name: "Stehlampe", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "6A9D15", KeyIndex: 0, Paired: paired},
		{Name: "Bad Fenster", Model: "HM-Sec-SCo", Address: "4C5D70", Serial: "NEQ0000002", KeyIndex: 2, AESChannels: []int{1}, Paired: paired},
		// Also in config.json: config.json wins.
		{Name: "Duplicate", Model: "HM-Sec-SCo", Address: "4C5D6E", KeyIndex: 2},
		// Name used in config.json: renamed in memory.
		{Name: "wohnzimmer fenster", Model: "HM-Sec-SCo", Address: "4C5D71", KeyIndex: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWith(t, pairingConfig(file), fakemod.Options{}, fastPairing)
	names := map[string]bool{}
	for _, d := range e.b.Snapshot().Devices {
		names[d.Name+"/"+d.Address+"/"+fmt.Sprint(d.Paired)] = true
	}
	for _, want := range []string{"Wohnzimmer Fenster/4c5d6e/false", "Stehlampe/6a9d15/true", "Bad Fenster/4c5d70/true", "wohnzimmer fenster 4C5D71/4c5d71/true"} {
		if !names[want] {
			t.Errorf("devices %v lack %s", names, want)
		}
	}
	if len(names) != 4 {
		t.Errorf("devices %v", names)
	}
	// Peers registered with the module, with their stored key and AES.
	if p := e.mod.Peers()[newSCo]; p.KeyIndex != 2 || fmt.Sprint(p.AESChannels(2)) != "[1]" {
		t.Errorf("peer %+v", p)
	}
	e.waitFor("unpair button", func() bool { return e.state("homeassistant/button/hmbridge_6a9d15/unpair/config") != nil })

	// Rename.
	e.b.Commands() <- ha.Command{Kind: ha.Rename, Addr: "6a9d15", Name: "Leselampe"}
	e.waitFor("renamed", func() bool { return string(e.pub.get("hmbridge/6a9d15/name")) == "Leselampe" })
	if devs, _ := devstore.Load(file); devs[2].Name != "Leselampe" || !devs[2].Paired.Equal(paired) {
		t.Errorf("stored after rename %+v", devs)
	}
	// A name in use is refused.
	e.b.Commands() <- ha.Command{Kind: ha.Rename, Addr: "6a9d15", Name: "Bad Fenster"}

	// Unpair the switch plug (always listening): unpaired at once.
	e.b.Commands() <- ha.Command{Kind: ha.Unpair, Addr: "6a9d15"}
	e.waitFor("unpaired", func() bool { return strings.HasPrefix(e.lastResult(), "unpaired Leselampe (6a9d15)") })
	if got := e.payloadsSent(newPlug, 0x01); fmt.Sprint(got) != "[00050000000000 000802000a000b000c00 0006]" {
		t.Errorf("unpair messages %v", got)
	}
	if _, ok := e.mod.Peers()[newPlug]; ok {
		t.Error("peer not removed from the module")
	}
	e.waitFor("discovery removed", func() bool {
		return e.pub.cleared("homeassistant/switch/hmbridge_6a9d15/switch/config") && e.pub.cleared("hmbridge/6a9d15/name") &&
			e.pub.cleared("hmbridge/6a9d15/state")
	})

	// Unpair the contact: it only listens after its config button.
	e.b.Commands() <- ha.Command{Kind: ha.Unpair, Addr: "4c5d70"}
	e.waitFor("waiting", func() bool { return strings.Contains(e.lastResult(), "press its config button within") })
	if n := len(e.payloadsSent(newSCo, 0x01)); n != 0 {
		t.Fatalf("%d messages before the config button", n)
	}
	e.inject(0x01, deviceInfo(newSCo, central, 0x00c7, "NEQ0000002"))
	e.waitFor("contact unpaired", func() bool { return strings.HasPrefix(e.lastResult(), "unpaired Bad Fenster (4c5d70)") })
	if got := e.payloadsSent(newSCo, 0x01); fmt.Sprint(got) != "[00050000000000 000802000a000b000c00 0006]" {
		t.Errorf("unpair messages %v", got)
	}

	// The entry shadowed by config.json is dropped on the next write.
	devs, _ := devstore.Load(file)
	if len(devs) != 1 || devs[0].Name != "wohnzimmer fenster 4C5D71" {
		t.Errorf("stored after unpairing %+v", devs)
	}

	// Restart: the devices file is read again.
	e.stop()
	e2 := newEnvWith(t, pairingConfig(file), fakemod.Options{}, fastPairing)
	if n := len(e2.b.Snapshot().Devices); n != 2 {
		t.Errorf("%d devices after restart", n)
	}
}

func TestPairingStatusPageAndDisabled(t *testing.T) {
	e := newEnvWith(t, pairingConfig(filepath.Join(t.TempDir(), "d.json")), fakemod.Options{}, fastPairing)
	srv := httptest.NewServer(e.b.StatusHandler(nil))
	defer srv.Close()
	post := func(origin string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/pairing/start", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("http://evil.example"); c != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d", c)
	}
	if c := post(""); c != http.StatusForbidden {
		t.Errorf("POST without Origin: %d", c)
	}
	if resp, _ := http.Get(srv.URL + "/pairing/start"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", resp.StatusCode)
	}
	if e.pairingState()["active"] == true {
		t.Fatal("pairing started by a refused request")
	}
	if c := post(srv.URL); c != http.StatusSeeOther {
		t.Errorf("same-origin POST: %d", c)
	}
	e.waitFor("pairing mode", func() bool { return e.pairingState()["active"] == true })
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	var page bytes.Buffer
	page.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(page.String(), "Status: <b>active</b>") || !strings.Contains(page.String(), `action="/pairing/start"`) {
		t.Errorf("status page:\n%s", page.String())
	}

	// devices_file "": pairing is disabled.
	d := newEnvWith(t, strings.Replace(pairingConfig(""), `"devices_file": ""`, `"devices_file": ""`, 1), fakemod.Options{}, fastPairing)
	d.b.Commands() <- ha.Command{Kind: ha.StartPairing}
	d.waitFor("disabled", func() bool {
		return d.pairingState()["status"] == "disabled" && strings.Contains(d.lastResult(), "pairing is disabled")
	})
	if b := d.pub.get("homeassistant/button/hmbridge_bridge/pair/config"); len(b) != 0 {
		t.Errorf("pair button published while disabled: %s", b)
	}
}
