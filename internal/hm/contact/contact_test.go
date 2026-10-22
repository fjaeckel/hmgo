// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package contact_test

import (
	"testing"

	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/contact"
	"github.com/fjaeckel/hmgo/internal/hm/hmtest"
)

var (
	central = [3]byte{0x1a, 0x2b, 0x3c}
	scoAddr = [3]byte{0x4c, 0x5d, 0x6e}
)

func newContact(t *testing.T, model string) *contact.Contact {
	t.Helper()
	c, err := contact.NewContact(hm.NewStandardDevice(nil, scoAddr, "Fenster"), model)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCapabilities(t *testing.T) {
	for _, tt := range []struct {
		model    string
		triState bool
	}{
		{"HM-Sec-SCo", false},
		{"HM-Sec-SC-2", false},
		{"HM-Sec-SC", false},
		{"HM-Sec-RHS", true},
	} {
		c := newContact(t, tt.model)
		var _ hm.Device = c
		caps := c.Capabilities()
		if caps.TriState != tt.triState || !caps.Sabotage || caps.BatteryVoltage {
			t.Errorf("%s: %+v", tt.model, caps)
		}
		if c.Model() != tt.model || c.Channels() != 1 {
			t.Errorf("%s: model %q channels %d", tt.model, c.Model(), c.Channels())
		}
	}
	if _, err := contact.NewContact(hm.NewStandardDevice(nil, scoAddr, "x"), "HM-Sec-WDS"); err == nil {
		t.Error("unsupported model: want error")
	}
	if _, ok := contact.ModelCapabilities("HM-CC-RT-DN"); ok {
		t.Error("ModelCapabilities(RT-DN) ok")
	}
}

func TestDecodeSCo(t *testing.T) {
	c := newContact(t, "HM-Sec-SCo")
	for _, tt := range []struct {
		name  string
		cmd   byte
		flags byte
		pl    []byte
		want  contact.Event
	}{
		{
			// EVENT to the central (A4 41 when AES/BiDi): chn 1, counter 0x1c, open
			name: "event open", cmd: 0x41, flags: 0xa4,
			pl:   []byte{0x01, 0x1c, 0xc8},
			want: contact.Event{Frame: contact.FrameEvent, Channel: 1, Counter: 0x1c, State: contact.Open},
		},
		{
			name: "event closed lowbat", cmd: 0x41, flags: 0xa4,
			pl:   []byte{0x81, 0x1d, 0x00},
			want: contact.Event{Frame: contact.FrameEvent, Channel: 1, Counter: 0x1d, State: contact.Closed, LowBattery: true},
		},
		{
			// cyclic INFO_LEVEL (~every 50 min for the SCo): closed, ok
			name: "info closed", cmd: 0x10, flags: 0x84,
			pl:   []byte{0x06, 0x01, 0x00, 0x00},
			want: contact.Event{Frame: contact.FrameInfoLevel, Channel: 1, State: contact.Closed, HasError: true},
		},
		{
			// INFO_LEVEL: open, sabotage (error 7 → 0x0e), lowbat
			name: "info sabotage lowbat", cmd: 0x10, flags: 0x84,
			pl:   []byte{0x06, 0x01, 0xc8, 0x8e},
			want: contact.Event{Frame: contact.FrameInfoLevel, Channel: 1, State: contact.Open, LowBattery: true, HasError: true, Error: 7, Sabotage: true},
		},
		{
			name: "ack status", cmd: 0x02, flags: 0x80,
			pl:   []byte{0x01, 0x01, 0xc8, 0x00, 0x40},
			want: contact.Event{Frame: contact.FrameAckStatus, Channel: 1, State: contact.Open, HasError: true},
		},
	} {
		ev, err := c.Decode(hmtest.Packet(0x02, 0x45, 0x10, tt.flags, tt.cmd, scoAddr, central, tt.pl...))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		got, ok := ev.(*contact.Event)
		if !ok {
			t.Fatalf("%s: got %T", tt.name, ev)
		}
		if *got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.name, *got, tt.want)
		}
	}
	if got := c.MostRecentEvents(); len(got) != 1 {
		t.Errorf("MostRecentEvents: %d", len(got))
	}
}

func TestDecodeRHS(t *testing.T) {
	c := newContact(t, "HM-Sec-RHS")
	ev, err := c.Decode(hmtest.Packet(0x01, 0x45, 0x10, 0xa4, 0x41, scoAddr, central, 0x01, 0x05, 0x64))
	if err != nil {
		t.Fatal(err)
	}
	e := ev.(*contact.Event)
	if e.State != contact.Tilted || !e.State.IsOpen() || e.State.String() != "tilted" {
		t.Errorf("tilted: %+v", e)
	}
}

func TestDecodeErrors(t *testing.T) {
	c := newContact(t, "HM-Sec-SCo")
	if _, err := c.Decode(hmtest.Packet(0x01, 0x45, 0x10, 0xa4, 0x41, scoAddr, central, 0x01)); err == nil {
		t.Error("short event: want error")
	}
	if _, err := c.Decode(hmtest.Packet(0x03, 0x45, 0x10, 0xa4, 0x41, scoAddr, central, 0x01, 0x05, 0xc8)); err == nil {
		t.Error("AES failed: want error")
	}
	if ev, err := c.Decode(hmtest.Packet(0x01, 0x45, 0x10, 0x80, 0x02, scoAddr, central, 0x00)); ev != nil || err != nil {
		t.Errorf("plain ack: %v %v", ev, err)
	}
}
