// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package pairing

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/fjaeckel/hmgo/internal/config"
)

func TestParseDeviceInfo(t *testing.T) {
	// HM-CC-RT-DN, firmware 1.4, serial MEQ0123456, class 0x58, peer
	// channels 0x05 0x00 (layout as in a CCU pairing).
	p, _ := hex.DecodeString("140095" + hex.EncodeToString([]byte("MEQ0123456")) + "58050000")
	di, err := ParseDeviceInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if di.Firmware != 0x14 || di.FirmwareString() != "1.4" || di.ModelID != 0x0095 || di.Serial != "MEQ0123456" || di.Class != 0x58 {
		t.Errorf("got %+v", di)
	}
	if !bytes.Equal(di.Extra, []byte{0x05, 0x00, 0x00}) {
		t.Errorf("extra %x", di.Extra)
	}
	if got := EncodeDeviceInfo(di); !bytes.Equal(got, p) {
		t.Errorf("EncodeDeviceInfo = %x, want %x", got, p)
	}
	if _, err := ParseDeviceInfo(p[:13]); err == nil {
		t.Error("short payload accepted")
	}
	bad := append([]byte(nil), p...)
	bad[5] = 0x01
	if _, err := ParseDeviceInfo(bad); err == nil {
		t.Error("unprintable serial accepted")
	}
}

func TestModelTable(t *testing.T) {
	supported := map[string]bool{}
	for _, m := range config.Models() {
		supported[m] = true
	}
	covered := map[string]bool{}
	seen := map[uint16]bool{}
	for _, m := range Models() {
		if seen[m.ID] {
			t.Errorf("duplicate model ID %04x", m.ID)
		}
		seen[m.ID] = true
		if !supported[m.Name] {
			t.Errorf("model %04x maps to %q, which the configuration does not support", m.ID, m.Name)
		}
		covered[m.Name] = true
		if m.AES != config.SupportsAES(m.Name) {
			t.Errorf("%s: AES %v, config says %v", m.Name, m.AES, config.SupportsAES(m.Name))
		}
		for _, ch := range m.AESDefault {
			if int(ch) < 1 || int(ch) > config.ModelChannels(m.Name) {
				t.Errorf("%s: AES default channel %d out of range", m.Name, ch)
			}
		}
		if m.Source == "" {
			t.Errorf("%s: no source", m.Name)
		}
	}
	for name := range supported {
		if !covered[name] {
			t.Errorf("supported model %s has no model ID", name)
		}
	}
	for id, want := range map[uint16]string{0x0095: "HM-CC-RT-DN", 0x00ad: "HM-TC-IT-WM-W-EU", 0x00c7: "HM-Sec-SCo", 0x00d8: "HM-LC-Sw1-Pl-DN-R1", 0x00da: "HM-RC-8", 0x003e: "HM-WDS30-T-O"} {
		if m, ok := ModelForID(id); !ok || m.Name != want {
			t.Errorf("ModelForID(%04x) = %v, %v; want %s", id, m.Name, ok, want)
		}
	}
	if _, ok := ModelForID(0x0001); ok {
		t.Error("unknown model ID found")
	}
}

func TestConfigMessages(t *testing.T) {
	central := [3]byte{0x1a, 0x2b, 0x3c}
	for _, tc := range []struct {
		name string
		got  []Message
		want []string
	}{
		// Homegear handlePairingRequest / FHEM pushConfig “0201 0Axx 0Byy 0Czz”.
		{"pair", PairSequence(central, false), []string{"00050000000000", "000802010a1a0b2b0c3c", "0006"}},
		// Models with INTERNAL_KEYS_VISIBLE (default true): bit 7 of reg 2.
		{"pair actuator", PairSequence(central, true), []string{"00050000000000", "000802810a1a0b2b0c3c", "0006"}},
		// Homegear unpair: 02 00 0A 00 0B 00 0C 00.
		{"unpair", PairSequence([3]byte{}, true), []string{"00050000000000", "000802000a000b000c00", "0006"}},
		{"aes", AESActiveSequence(2, true), []string{"02050000000001", "02080801", "0206"}},
		{"aes off", AESActiveSequence(1, false), []string{"01050000000001", "01080800", "0106"}},
	} {
		if len(tc.got) != len(tc.want) {
			t.Fatalf("%s: %d messages", tc.name, len(tc.got))
		}
		for i, m := range tc.got {
			if m.Cmd != CmdConfig || hex.EncodeToString(m.Payload) != tc.want[i] {
				t.Errorf("%s[%d] (%s) = %02x %x, want 01 %s", tc.name, i, m.What, m.Cmd, m.Payload, tc.want[i])
			}
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("ConfigWriteIndex with 8 registers did not panic")
		}
	}()
	ConfigWriteIndex(0, make([]Reg, 8)...)
}

func TestConfigParamReqAndWriteListSequence(t *testing.T) {
	// FHEM CUL_HM_getConfig: <chn>04 00000000 <list>.
	if got := ConfigParamReq(0, 7); !bytes.Equal(got, []byte{0x00, 0x04, 0, 0, 0, 0, 0x07}) {
		t.Errorf("ConfigParamReq = %x", got)
	}
	var regs []Reg
	for a := byte(1); a <= 15; a++ {
		regs = append(regs, Reg{a, a})
	}
	msgs := WriteListSequence(1, 1, regs)
	if len(msgs) != 5 {
		t.Fatalf("%d messages", len(msgs))
	}
	if !bytes.Equal(msgs[0].Payload, ConfigStart(1, 1)) || !bytes.Equal(msgs[4].Payload, ConfigEnd(1)) {
		t.Errorf("start/end %x %x", msgs[0].Payload, msgs[4].Payload)
	}
	// 7 + 7 + 1 pairs.
	for i, n := range []int{7, 7, 1} {
		p := msgs[1+i].Payload
		if p[0] != 1 || p[1] != SubConfigWriteIndex || len(p) != 2+2*n {
			t.Errorf("write %d: %x", i, p)
		}
	}
}
