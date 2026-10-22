// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package hmip

import (
	"bufio"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

type captured struct {
	capture, time, dir string
	raw                []byte
}

func loadCaptures(t testing.TB) []captured {
	t.Helper()
	f, err := os.Open("testdata/captures.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []captured
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) != 4 {
			t.Fatalf("bad line %q", line)
		}
		raw, err := hex.DecodeString(fs[3])
		if err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		out = append(out, captured{fs[0], fs[1], fs[2], raw})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// describeCaptures decodes all captured frames, one Describer per
// capture, and returns the descriptions by "capture time dir".
func describeCaptures(t *testing.T) map[string][]string {
	got := make(map[string][]string)
	ds := make(map[string]*Describer)
	for _, c := range loadCaptures(t) {
		p, err := ParseRaw(c.raw)
		if err != nil {
			t.Errorf("%s %s: %v", c.capture, c.time, err)
			continue
		}
		if p.Dst != DstHMIP {
			t.Errorf("%s %s: dst %02x", c.capture, c.time, p.Dst)
		}
		d := ds[c.capture]
		if d == nil {
			d = &Describer{}
			ds[c.capture] = d
		}
		s := d.Describe(c.dir == "TX", p)
		if strings.Contains(s, "malformed") {
			t.Errorf("%s %s: %s", c.capture, c.time, s)
		}
		k := c.capture + " " + c.time + " " + c.dir
		got[k] = append(got[k], s)
	}
	return got
}

func TestCaptures(t *testing.T) {
	got := describeCaptures(t)
	for _, tc := range []struct{ key, want string }{
		// HMIPServer start: own address, security counter, link partners.
		{"restart1 10:35:11.437 TX", "SET_ADDRESS B4E47A"},
		{"restart1 10:35:11.440 RX", "RESPONSE to SET_ADDRESS OK(01)"},
		{"restart1 10:35:11.505 TX", "CSMA_DEFAULTS_0D 03"},
		{"restart1 10:35:11.894 RX", "RESPONSE to GET_SECURITY_COUNTER OK(01) counter=4D2F954F"},
		{"restart1 10:35:12.031 TX", "SET_SECURITY_COUNTER counter=4D2FBFC7"},
		{"restart1 10:35:12.191 RX", "RESPONSE to GET_LINK_PARTNERS OK(01) partners=47DEF5"},
		// Startup broadcasts (content type 2, not detailed by the spec);
		// the host omits the security fields, the module's report of the
		// transmission has them (zero).
		{"restart1 10:35:18.067 TX", "SEND burst=00 B4E47A→F00001 CT2 sec=1 v8 hop=1 | 88c0"},
		{"restart1 10:35:18.163 RX", "EVENT rssi=none (own transmission) B4E47A→F00001 CT2 sec=1 v8 hop=1 sn=00000000 mic=00000000 | 88c0"},
		{"restart1 10:35:18.193 RX", "RESPONSE to SEND OK(09)"},
		// Application frame, burst 3: reported three times, the
		// repetitions with the real security number and MIC.
		{"restart1 10:35:22.330 TX", "SEND burst=03 B4E47A→F00001 APP sec=1 v8 hop=7 | TRIGGER_STATUS seq=01 64d0"},
		{"restart1 10:35:23.559 RX", "EVENT rssi=none (own transmission) B4E47A→F00001 APP sec=1 v8 hop=7 qos sn=4D2FBFC9 mic=c0374c75 | TRIGGER_STATUS seq=01 64d0"},
		// The only frame from another radio: the module's link partner.
		{"restart2 10:39:37.247 RX", "EVENT rssi=-69 47DEF5→F00001 CT2 sec=1 v8 hop=1 qos sn=0589067D mic=f060023e | 8804"},
		{"restart2 10:40:44.904 TX", "SEND burst=00 B4E47A→F00002 ROUTE sec=1 v8 hop=1 | 0147def50000000411"},
	} {
		if g := got[tc.key]; len(g) != 1 || g[0] != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.key, g, tc.want)
		}
	}
}

// The security number of the module's transmissions continues from the
// counter HMIPServer sets at its start (an observation; the
// specification treats the field as opaque).
func TestCapturedSecurityCounter(t *testing.T) {
	var set uint32
	for _, c := range loadCaptures(t) {
		if c.capture != "restart1" {
			continue
		}
		p, err := ParseRaw(c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if p.Cmd == CmdSetSecurityCounter && c.dir == "TX" {
			set = uint32(p.Data[0])<<24 | uint32(p.Data[1])<<16 | uint32(p.Data[2])<<8 | uint32(p.Data[3])
		}
		if p.Cmd != CmdEvent || c.dir != "RX" {
			continue
		}
		ev, err := p.Event()
		if err != nil {
			t.Fatal(err)
		}
		if sn := ev.Frame.SecurityNumber; sn != 0 && (sn <= set || sn > set+16) {
			t.Errorf("%s: security number %08X, counter set to %08X", c.time, sn, set)
		}
	}
	if set == 0 {
		t.Fatal("no SET_SECURITY_COUNTER in the capture")
	}
}

// The status frame of the specification's worked example (a device's
// report as received by the access point, RSSI byte 0x39).
func TestSpecStatusFrame(t *testing.T) {
	data, _ := hex.DecodeString("39" + "10018e2cf049b1235a000000671052057c850204800101" + "00de4303001a04016800000d0100")
	ev, err := Packet{Dst: DstHMIP, Cmd: CmdEvent, Data: data}.Event()
	if err != nil {
		t.Fatal(err)
	}
	f := ev.Frame
	if rssi, ok := ev.RSSI(); !ok || rssi != -0x39 || ev.Echo() {
		t.Errorf("rssi %d %v", rssi, ok)
	}
	if f.Security != SecurityOn || f.Content != ContentApplication || !f.PiggybackAck || f.Version != 8 || f.HopLimit != 7 ||
		f.Src != (Address{0x2c, 0xf0, 0x49}) || f.Dst != (Address{0xb1, 0x23, 0x5a}) ||
		f.SecurityNumber != 0x67 || hex.EncodeToString(f.MIC) != "1052057c" || f.Len != 17 {
		t.Errorf("header %+v", f.MACHeader)
	}
	h, payload, err := f.App()
	if err != nil {
		t.Fatal(err)
	}
	if h != (AppHeader{ResponseRequested: true, Type: 5, Seq: 2}) || payload[0] != 0x04 || len(payload) != 18 {
		t.Errorf("app %+v %x", h, payload)
	}
}

func TestMACHeaderFields(t *testing.T) {
	// fragmented, cyclic, extended security, network management;
	// piggyback; version 8, hop 0, QoS; then 4+8 security bytes and the
	// cyclic counter.
	b, _ := hex.DecodeString("e1018111111122222200000005" + "0102030405060708" + "7f" + "10aa")
	f, err := ParseFrame(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Fragmented || !f.Cyclic || f.Security != SecurityExtended || f.Content != ContentNetworkMgmt ||
		f.SecurityNumber != 5 || len(f.MIC) != 8 || f.CyclicCounter != 0x7f || f.Len != 22 || hex.EncodeToString(f.Payload) != "10aa" {
		t.Errorf("%+v", f)
	}
	var d Describer
	if s := d.frame(f, nil, b); s != "111111→222222 NETMGMT sec=2 v8 hop=0 frag pback qos sn=00000005 mic=0102030405060708 cyclic=7F | INCLUSION_REQUEST <1 bytes redacted>" {
		t.Errorf("describe: %s", s)
	}
	// Without security fields (frames the host sends).
	if f, err := ParseFrame(b, false); err != nil || f.Len != 10 || f.SecurityFields {
		t.Errorf("without security fields: %+v %v", f, err)
	}
	// Address extension: length unknown.
	b[1] = 0x08
	if _, err := ParseFrame(b, true); !errors.Is(err, ErrUnsupported) {
		t.Errorf("address extension: %v", err)
	}
	// Truncated security fields.
	if _, err := ParseFrame(b[:12], true); !errors.Is(err, ErrShort) {
		t.Errorf("short: %v", err)
	}
}

func TestRawErrors(t *testing.T) {
	good, _ := hex.DecodeString("fd000402090601bd89")
	if _, err := ParseRaw(good); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func([]byte) []byte{
		"crc":    func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		"length": func(b []byte) []byte { b[2]++; return b },
		"sync":   func(b []byte) []byte { b[0] = 0; return b },
		"short":  func(b []byte) []byte { return b[:5] },
	} {
		if _, err := ParseRaw(mod(append([]byte(nil), good...))); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRedaction(t *testing.T) {
	var d Describer
	secret := strings.Repeat("ab", 20)
	b, _ := hex.DecodeString(secret)
	// network management, address extension 1, version 8
	routed := append([]byte{0x01, 0x08, 0x80, 1, 2, 3, 4, 5, 6}, b...)
	// network management, extended security, security fields cut short
	secured := append([]byte{0x21, 0x00, 0x80, 1, 2, 3, 4, 5, 6}, b[:6]...)
	for _, s := range []string{
		d.Describe(true, Packet{Dst: DstHMIP, Cnt: 1, Cmd: CmdSetEncNetworkKey, Data: b}),
		d.Describe(false, Packet{Dst: DstHMIP, Cnt: 1, Cmd: CmdResponse, Data: append([]byte{1}, b...)}),
		d.Describe(true, Packet{Dst: DstHMIP, Cnt: 2, Cmd: 0x19, Data: b}),
		d.Describe(false, Packet{Dst: DstHMIP, Cnt: 2, Cmd: CmdResponse, Data: append([]byte{1}, b...)}),
		d.Describe(false, Packet{Dst: DstHMIP, Cnt: 3, Cmd: CmdResponse, Data: append([]byte{1}, b...)}), // unknown request
		d.Describe(true, Packet{Dst: DstKey, Cnt: 4, Cmd: 0x0e, Data: b}),
		d.Describe(false, Packet{Dst: DstKey, Cnt: 4, Cmd: 0x03, Data: b}),
		// Routed network management (address extension): ParseFrame
		// fails, the bytes after the header must not be dumped.
		d.Describe(false, Packet{Dst: DstHMIP, Cnt: 5, Cmd: CmdEvent, Data: append([]byte{0x40}, routed...)}),
		d.Describe(true, Packet{Dst: DstHMIP, Cnt: 6, Cmd: CmdSend, Data: append([]byte{0x00}, routed...)}),
		// Truncated security fields.
		d.Describe(false, Packet{Dst: DstHMIP, Cnt: 7, Cmd: CmdEvent, Data: append([]byte{0x40}, secured...)}),
	} {
		if strings.Contains(s, "abab") || !strings.Contains(s, "redacted") {
			t.Errorf("not redacted: %s", s)
		}
	}
	for _, p := range []Packet{
		{Dst: DstHMIP, Cnt: 5, Cmd: CmdEvent, Data: append([]byte{0x40}, routed...)},
		{Dst: DstHMIP, Cnt: 1, Cmd: CmdSetEncNetworkKey, Data: b},
	} {
		if _, sensitive := d.DescribeSensitive(p.Cmd != CmdEvent, p); !sensitive {
			t.Errorf("%v: not reported as sensitive", p.Cmd)
		}
	}
	if _, sensitive := d.DescribeSensitive(false, Packet{Dst: DstHMIP, Cnt: 8, Cmd: CmdEvent, Data: []byte{0x40, 0x00, 0x00, 0x80, 1, 2, 3, 4, 5, 6, 0x01}}); sensitive {
		t.Error("plain application frame reported as sensitive")
	}
	d.ShowSensitive = true
	if s := d.Describe(true, Packet{Dst: DstKey, Cmd: 0x0e, Data: b}); !strings.Contains(s, secret) {
		t.Errorf("ShowSensitive: %s", s)
	}
}

func FuzzDecode(f *testing.F) {
	for _, c := range loadCaptures(f) {
		f.Add(c.raw)
	}
	f.Add([]byte{0xfd, 0x00, 0x03, 0x02, 0x00, 0x07, 0x00, 0x00})
	f.Fuzz(func(t *testing.T, raw []byte) {
		var d Describer
		if p, err := ParseRaw(raw); err == nil {
			d.Describe(true, p)
			d.Describe(false, p)
		}
		// The payload as a packet on both destinations and as a MAC
		// frame in every interpretation.
		if len(raw) > 0 {
			for _, dst := range []byte{DstHMIP, DstKey} {
				for _, cmd := range []Command{CmdSend, CmdResponse, CmdEvent, CmdGetLinkPartners, CmdSetAddress, CmdAddLinkPartner, CmdSetSecurityCounter} {
					p := Packet{Dst: dst, Cnt: raw[0], Cmd: cmd, Data: raw[1:]}
					d.Describe(true, p)
					d.Describe(false, p)
				}
			}
		}
		for _, sec := range []bool{false, true} {
			fr, err := ParseFrame(raw, sec)
			if err == nil {
				if fr.Len > len(raw) || fr.Len+len(fr.Payload) != len(raw) {
					t.Fatalf("lengths: header %d payload %d of %d", fr.Len, len(fr.Payload), len(raw))
				}
				fr.App()
			}
			d.frame(fr, err, raw)
		}
	})
}
