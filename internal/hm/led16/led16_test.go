// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package led16_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/hmtest"
	"github.com/fjaeckel/hmgo/internal/hm/led16"
)

var (
	central = [3]byte{0x45, 0x80, 0x86}
	led     = [3]byte{0x75, 0x64, 0xea} // the user's HM-OU-LED16
)

func TestSetPacketMatchesOpenCCU(t *testing.T) {
	// OpenCCU, captured with hmsniff:
	// SEND … #01 BIDI|RPTEN SET 458086(central) → 7564EA(HM-OU-LED16) 800102
	p := led16.SetPacket(led, 1, led16.Green)
	p.Source, p.Msgcnt = central, 0x01
	want := []byte{0x00, 0x00, 0x00, 0x01, 0xa0, 0x11, 0x45, 0x80, 0x86, 0x75, 0x64, 0xea, 0x80, 0x01, 0x02}
	if got := p.Encode(); !bytes.Equal(got, want) {
		t.Errorf("encoded %x, want %x", got, want)
	}
	if p := led16.StatusRequestPacket(led, 3); p.Cmd != 0x01 || !bytes.Equal(p.Payload, []byte{0x03, 0x0e}) {
		t.Errorf("status request %02x %x", p.Cmd, p.Payload)
	}
}

func TestColors(t *testing.T) {
	for i, n := range led16.Colors {
		c, err := led16.ParseColor(" " + string(bytes.ToUpper([]byte(n))))
		if err != nil || int(c) != i || c.String() != n {
			t.Errorf("ParseColor(%q) = %v, %v", n, c, err)
		}
	}
	if _, err := led16.ParseColor("blue"); err == nil {
		t.Error("blue accepted")
	}
}

func TestDecode(t *testing.T) {
	d := led16.NewDisplay(hm.NewStandardDevice(nil, led, "LED"))
	for _, tc := range []struct {
		pkt  *bidcos.Packet
		want led16.Event
	}{
		{hmtest.Packet(0x01, 0x40, 0x01, 0x80, 0x02, led, central, 0x01, 0x01, 0x02, 0x00, 0x40), led16.Event{Channel: 1, Color: led16.Green}},
		{hmtest.Packet(0x01, 0x40, 0x01, 0xa0, 0x10, led, central, 0x06, 0x10, 0x03), led16.Event{Channel: 16, Color: led16.Orange}},
	} {
		ev, err := d.Decode(tc.pkt)
		if err != nil || *ev.(*led16.Event) != tc.want {
			t.Errorf("Decode(%x) = %v, %v", tc.pkt.Payload, ev, err)
		}
	}
	for _, p := range [][]byte{{0x01, 0x11, 0x00}, {0x01, 0x00, 0x00}, {0x01, 0x01, 0x04}} {
		if _, err := d.Decode(hmtest.Packet(0x01, 0x40, 0x01, 0x80, 0x02, led, central, p...)); err == nil {
			t.Errorf("payload %x: no error", p)
		}
	}
	// Key events of the display are not used.
	if ev, err := d.Decode(hmtest.Packet(0x01, 0x40, 0x01, 0xa0, 0x40, led, central, 0x01, 0x05)); ev != nil || err != nil {
		t.Errorf("key event: %v, %v", ev, err)
	}
}

func TestSetLEDAndTimeout(t *testing.T) {
	tr := &hmtest.Transport{Respond: hmtest.AckWith(0x01, 0x05, 0x01, 0x00, 0x40)}
	d := led16.NewDisplay(hm.NewStandardDevice(bidcos.NewSender(tr, central), led, "LED"))
	ev, err := d.SetLED(context.Background(), 5, led16.Red)
	if err != nil || ev == nil || ev.Channel != 5 || ev.Color != led16.Red {
		t.Fatalf("SetLED = %+v, %v", ev, err)
	}
	if s := tr.Sent[0]; s.Flags != 0xa0 || !bytes.Equal(s.Payload, []byte{0x80, 0x05, 0x01}) {
		t.Errorf("sent %02x %x", s.Flags, s.Payload)
	}
	if _, err := d.SetLED(context.Background(), 17, led16.Red); err == nil {
		t.Error("LED 17 accepted")
	}
	// The user's display never answers: the command fails cleanly.
	tr.Respond = nil
	if _, err := d.SetLED(context.Background(), 1, led16.Off); !errors.Is(err, hm.ErrNoResponse) {
		t.Errorf("unanswered SetLED: %v", err)
	}
	if _, err := d.RequestStatus(context.Background(), 1); !errors.Is(err, hm.ErrNoResponse) {
		t.Errorf("unanswered RequestStatus: %v", err)
	}
}
