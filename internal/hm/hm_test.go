// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package hm_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/hmtest"
)

var (
	central = [3]byte{0x1a, 0x2b, 0x3c}
	dev     = [3]byte{0x38, 0xf5, 0x9c}
)

func TestEncodeTemperature(t *testing.T) {
	for _, tt := range []struct {
		in   float64
		want byte
		err  bool
	}{
		{4.5, 9, false},
		{30.5, 61, false},
		{21.0, 42, false},
		{21.2, 42, false},
		{21.3, 43, false},
		{21.75, 44, false}, // rounds half away from zero: 43.5 → 44
		{4.3, 9, false},    // rounds to 4.5
		{4.2, 0, true},
		{30.8, 0, true},
		{-1, 0, true},
	} {
		got, err := hm.EncodeTemperature(tt.in)
		if (err != nil) != tt.err {
			t.Errorf("EncodeTemperature(%v): err = %v, want error %v", tt.in, err, tt.err)
			continue
		}
		if got != tt.want {
			t.Errorf("EncodeTemperature(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestClimateCommandPacket(t *testing.T) {
	for _, tt := range []struct {
		cmd  hm.ClimateCommand
		temp float64
		want []byte
	}{
		{hm.CmdSetTemperature, 21.5, []byte{0x86, 0x04, 0x2b}},
		{hm.CmdManual, 4.5, []byte{0x81, 0x04, 0x09}},
		{hm.CmdAuto, 0, []byte{0x80, 0x04}},
		{hm.CmdBoost, 0, []byte{0x83, 0x04}},
	} {
		p, err := hm.ClimateCommandPacket(dev, 4, tt.cmd, tt.temp)
		if err != nil {
			t.Fatal(err)
		}
		if p.Cmd != 0x11 || p.Flags != 0xb0 || p.Dest != dev || !bytes.Equal(p.Payload, tt.want) {
			t.Errorf("%s: got cmd %02x flags %02x dest %x payload % x, want 11 b0 %x % x", tt.cmd, p.Cmd, p.Flags, p.Dest, p.Payload, dev, tt.want)
		}
	}
	if _, err := hm.ClimateCommandPacket(dev, 4, "party", 0); err == nil {
		t.Error("unknown command: want error")
	}
}

func TestCheckAck(t *testing.T) {
	for _, tt := range []struct {
		name string
		resp *bidcos.Packet
		ok   bool
		nack bool
	}{
		{"ack", hmtest.Packet(1, 0x40, 1, 0x80, 0x02, dev, central, 0x00), true, false},
		{"ack status", hmtest.Packet(1, 0x40, 1, 0x80, 0x02, dev, central, 0x01, 0x04, 0x2a, 0x00, 0x40, 0x40), true, false},
		{"ack aes ok", hmtest.Packet(2, 0x40, 1, 0x80, 0x02, dev, central, 0x00), true, false},
		{"nack", hmtest.Packet(1, 0x40, 1, 0x80, 0x02, dev, central, 0x80), false, true},
		{"nack 84", hmtest.Packet(1, 0x40, 1, 0x80, 0x02, dev, central, 0x84), false, true},
		{"aes failed", hmtest.Packet(3, 0x40, 1, 0x80, 0x02, dev, central, 0x00), false, false},
		{"other device", hmtest.Packet(1, 0x40, 1, 0x80, 0x02, [3]byte{1, 2, 3}, central, 0x00), false, false},
		{"not an ack", hmtest.Packet(1, 0x40, 1, 0x80, 0x10, dev, central, 0x0a), false, false},
		{"empty", hmtest.Packet(1, 0x40, 1, 0x80, 0x02, dev, central), false, false},
		{"nil", nil, false, false},
	} {
		err := hm.CheckAck(dev, tt.resp)
		if (err == nil) != tt.ok {
			t.Errorf("%s: CheckAck = %v, want ok %v", tt.name, err, tt.ok)
		}
		var ne *hm.NackError
		if got := errors.As(err, &ne); got != tt.nack {
			t.Errorf("%s: NackError = %v, want %v", tt.name, got, tt.nack)
		}
	}
}

func TestSendClimateCommand(t *testing.T) {
	tr := &hmtest.Transport{Respond: hmtest.AckWith(0x00)}
	sd := hm.NewStandardDevice(bidcos.NewSender(tr, central), dev, "test")
	resp, err := sd.SendClimateCommand(context.Background(), 4, hm.CmdSetTemperature, 22)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Cmd != 0x02 {
		t.Errorf("resp cmd %02x", resp.Cmd)
	}
	if len(tr.Sent) != 1 {
		t.Fatalf("sent %d packets", len(tr.Sent))
	}
	s := tr.Sent[0]
	if s.Source != central || s.Dest != dev || s.Flags != 0xb0 || !bytes.Equal(s.Payload, []byte{0x86, 0x04, 0x2c}) {
		t.Errorf("sent %+v", s)
	}

	tr.Respond = hmtest.AckWith(0x80)
	if _, err := sd.SendClimateCommand(context.Background(), 4, hm.CmdAuto, 0); err == nil {
		t.Error("NACK: want error")
	}
	tr.Respond = nil
	if _, err := sd.SendClimateCommand(context.Background(), 4, hm.CmdAuto, 0); !errors.Is(err, hm.ErrNoResponse) {
		t.Errorf("no response: got %v, want ErrNoResponse", err)
	}
	if _, err := sd.SendClimateCommand(context.Background(), 4, hm.CmdManual, 99); err == nil {
		t.Error("invalid temperature: want error")
	}
	if len(tr.Sent) != 3 {
		t.Errorf("invalid temperature must not be sent; sent %d", len(tr.Sent))
	}

	nosend := hm.NewStandardDevice(nil, dev, "x")
	if _, err := nosend.SendClimateCommand(context.Background(), 4, hm.CmdAuto, 0); !errors.Is(err, hm.ErrNoSender) {
		t.Errorf("nil sender: got %v", err)
	}
}

func TestTimestampResponse(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*3600)
	// 2024-07-01 12:00:00 UTC = Unix 1719835200 = 773150400 s since 2000 = 0x2E1556C0.
	now := time.Date(2024, 7, 1, 14, 0, 0, 0, berlin)
	p := hm.TimestampResponse(0x42, central, dev, now)
	if p.Msgcnt != 0x42 || p.Flags != 0x80 || p.Cmd != 0x3f || p.Source != central || p.Dest != dev {
		t.Errorf("header: %+v", p)
	}
	if want := []byte{0x02, 0x04, 0x2e, 0x15, 0x56, 0xc0}; !bytes.Equal(p.Payload, want) {
		t.Errorf("payload: got % x, want % x", p.Payload, want)
	}
	utc := hm.TimestampResponse(1, central, dev, now.UTC())
	if utc.Payload[1] != 0 || !bytes.Equal(utc.Payload[2:], p.Payload[2:]) {
		t.Errorf("UTC payload: % x", utc.Payload)
	}
	// negative offsets
	ny := hm.TimestampResponse(1, central, dev, now.In(time.FixedZone("EDT", -4*3600)))
	if ny.Payload[1] != 0xf8 {
		t.Errorf("EDT offset byte: %02x, want f8", ny.Payload[1])
	}
	// Encode round trip
	enc := p.Encode()
	if enc[3] != 0x42 || enc[4] != 0x80 || enc[5] != 0x3f {
		t.Errorf("encoded: % x", enc)
	}
}

func TestDecodeCommon(t *testing.T) {
	// DEVICE_INFO of an HM-CC-RT-DN, firmware 1.4, serial MEQ0123456.
	pl := append([]byte{0x14, 0x00, 0x95}, []byte("MEQ0123456")...)
	pl = append(pl, 0x58, 0x06, 0x00, 0x00)
	ev, err := hm.DecodeCommon(hmtest.Packet(1, 0x40, 1, 0x84, 0x00, dev, [3]byte{}, pl...))
	if err != nil {
		t.Fatal(err)
	}
	di, ok := ev.(*hm.DeviceInfo)
	if !ok {
		t.Fatalf("got %T", ev)
	}
	if di.Firmware != "1.4" || di.ModelID != 0x95 || di.Serial != "MEQ0123456" || di.Class != 0x58 {
		t.Errorf("device info: %+v", di)
	}

	ev, err = hm.DecodeCommon(hmtest.Packet(1, 0x40, 0x17, 0xa0, 0x3f, dev, central, 0x04, 0x00))
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := ev.(*hm.TimeRequest)
	if !ok || tr.Msgcnt != 0x17 || tr.Dest != central {
		t.Errorf("time request: %#v", ev)
	}

	ev, err = hm.DecodeCommon(hmtest.Packet(1, 0x40, 1, 0xa0, 0x10, dev, central, 0x0a))
	if ev != nil || err != nil {
		t.Errorf("other frame: %v, %v", ev, err)
	}
}
