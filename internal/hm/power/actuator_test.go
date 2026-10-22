// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package power_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/hmtest"
	"github.com/fjaeckel/hmgo/internal/hm/power"
)

var (
	central = [3]byte{0x45, 0x80, 0x86}
	plug    = [3]byte{0x6a, 0x9d, 0x15} // HM-LC-Sw1-Pl-DN-R1 of the user's installation
	dimmer  = [3]byte{0x41, 0xe9, 0x51} // HM-LC-Dim1L-Pl-3
)

func newActuator(t *testing.T, tr *hmtest.Transport, addr [3]byte, model string) *power.Actuator {
	t.Helper()
	var bcs *bidcos.Sender
	if tr != nil {
		bcs = bidcos.NewSender(tr, central)
	}
	a, err := power.NewActuator(hm.NewStandardDevice(bcs, addr, "test"), model)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDecodeLevelStatus(t *testing.T) {
	sw := newActuator(t, nil, plug, power.ModelSwitchPlug)
	dim := newActuator(t, nil, dimmer, power.ModelDimmerPlug)
	for _, tc := range []struct {
		name string
		a    *power.Actuator
		pkt  *bidcos.Packet
		want power.LevelEvent
	}{
		{"switch ACK_STATUS on", sw,
			hmtest.Packet(0x01, 0x3e, 0x12, 0x80, 0x02, plug, central, 0x01, 0x01, 0xc8, 0x00, 0x3e),
			power.LevelEvent{Frame: power.FrameAckStatus, Channel: 1, Raw: 0xc8, Percent: 100, HasFlags: true}},
		{"switch INFO_LEVEL off (local button)", sw,
			hmtest.Packet(0x01, 0x40, 0x13, 0xa4, 0x10, plug, central, 0x06, 0x01, 0x00, 0x00),
			power.LevelEvent{Frame: power.FrameInfoLevel, Channel: 1, Raw: 0, Percent: 0, HasFlags: true}},
		{"switch with on time running", sw,
			hmtest.Packet(0x01, 0x40, 0x14, 0x80, 0x02, plug, central, 0x01, 0x01, 0xc8, 0x40, 0x3e),
			power.LevelEvent{Frame: power.FrameAckStatus, Channel: 1, Raw: 0xc8, Percent: 100, HasFlags: true, Working: true}},
		{"dimmer ramping up", dim,
			hmtest.Packet(0x01, 0x40, 0x15, 0x80, 0x02, dimmer, central, 0x01, 0x01, 0x10, 0x10, 0x3e, 0x10),
			power.LevelEvent{Frame: power.FrameAckStatus, Channel: 1, Raw: 0x10, Percent: 8, HasFlags: true, Working: true, Direction: power.DirectionUp}},
		{"dimmer INFO_LEVEL 42.5 %", dim,
			hmtest.Packet(0x01, 0x40, 0x16, 0xa4, 0x10, dimmer, central, 0x06, 0x01, 0x55, 0x00, 0x00, 0x55),
			power.LevelEvent{Frame: power.FrameInfoLevel, Channel: 1, Raw: 0x55, Percent: 42.5, HasFlags: true}},
		{"dimmer load failure", dim,
			hmtest.Packet(0x01, 0x40, 0x17, 0xa4, 0x10, dimmer, central, 0x06, 0x01, 0x00, 0x06),
			power.LevelEvent{Frame: power.FrameInfoLevel, Channel: 1, HasFlags: true, Error: power.ErrorLoadFailure}},
		{"dimmer virtual channel 2", dim,
			hmtest.Packet(0x01, 0x40, 0x18, 0xa4, 0x10, dimmer, central, 0x06, 0x02, 0xc8, 0x00),
			power.LevelEvent{Frame: power.FrameInfoLevel, Channel: 2, Raw: 0xc8, Percent: 100, HasFlags: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := tc.a.Decode(tc.pkt)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := ev.(*power.LevelEvent)
			if !ok || *got != tc.want {
				t.Errorf("got %+v, want %+v", ev, tc.want)
			}
		})
	}
	if e := (&power.LevelEvent{Error: power.ErrorLoadFailure}); e.ErrorString() != "load_failure" {
		t.Errorf("ErrorString = %q", e.ErrorString())
	}
}

func TestDecodeLevelOddFrames(t *testing.T) {
	sw := newActuator(t, nil, plug, power.ModelSwitchPlug)
	// Not for the actuator decoder: nil, nil.
	for _, p := range []*bidcos.Packet{
		hmtest.Packet(0x01, 0x40, 0x01, 0x80, 0x02, plug, central, 0x00),             // plain ACK
		hmtest.Packet(0x01, 0x40, 0x01, 0xa0, 0x41, plug, central, 0x01, 0x02, 0x03), // something else
		hmtest.Packet(0x01, 0x40, 0x01, 0xa0, 0x10, plug, central, 0x02, 0x01),       // INFO_PARAM_RESPONSE
	} {
		if ev, err := sw.Decode(p); ev != nil || err != nil {
			t.Errorf("Decode(%x %x) = %v, %v", p.Cmd, p.Payload, ev, err)
		}
	}
	// Too short for ACK_STATUS: treated like any other ACK.
	if ev, err := sw.Decode(hmtest.Packet(0x01, 0x40, 0x01, 0x80, 0x02, plug, central, 0x01, 0x01)); ev != nil || err != nil {
		t.Errorf("short ACK_STATUS = %v, %v", ev, err)
	}
	// Malformed: errors, never panics.
	for _, p := range []*bidcos.Packet{
		hmtest.Packet(0x01, 0x40, 0x01, 0x80, 0x02, plug, central, 0x01, 0x01, 0xc9, 0x00),
		hmtest.Packet(0x01, 0x40, 0x01, 0xa0, 0x10, plug, central, 0x06),
	} {
		if _, err := sw.Decode(p); err == nil {
			t.Errorf("Decode(%x %x): no error", p.Cmd, p.Payload)
		}
	}
	// From another device.
	if _, err := sw.Decode(hmtest.Packet(0x01, 0x40, 0x01, 0x80, 0x02, dimmer, central, 0x01, 0x01, 0, 0)); err == nil {
		t.Error("frame of another device accepted")
	}
}

func TestTime16(t *testing.T) {
	for _, tc := range []struct {
		s    float64
		want uint16
	}{
		{0, 0},
		{-1, 0},
		{0.5, 5 << 5},
		{2, 20 << 5},
		{204.7, 2047 << 5},
		{300, 1500<<5 | 1},
		{3600, 1125<<5 | 5},
	} {
		got := power.EncodeTime16(tc.s)
		if got != tc.want {
			t.Errorf("EncodeTime16(%v) = %#04x, want %#04x", tc.s, got, tc.want)
		}
		if back := power.DecodeTime16(got); tc.s > 0 && math.Abs(back-tc.s) > tc.s/1000 {
			t.Errorf("DecodeTime16(%#04x) = %v, want %v", got, back, tc.s)
		}
	}
}

func TestLevelSetPacket(t *testing.T) {
	for _, tc := range []struct {
		name          string
		chn, level    byte
		ramp, onTime  float64
		wantPayload   []byte
		wantFlagsBits byte
	}{
		{"switch on", 1, 0xc8, 0, 0, []byte{0x02, 0x01, 0xc8, 0x00, 0x00}, 0xa0},
		{"switch off", 1, 0x00, 0, 0, []byte{0x02, 0x01, 0x00, 0x00, 0x00}, 0xa0},
		{"dimmer 50 % in 0.5 s", 1, 0x64, 0.5, 0, []byte{0x02, 0x01, 0x64, 0x00, 0xa0}, 0xa0},
		{"with on time 10 s", 1, 0xc8, 0, 10, []byte{0x02, 0x01, 0xc8, 0x00, 0x00, 0x0c, 0x80}, 0xa0},
	} {
		p := power.LevelSetPacket(plug, tc.chn, tc.level, tc.ramp, tc.onTime)
		if p.Cmd != 0x11 || p.Flags != tc.wantFlagsBits || !bytes.Equal(p.Payload, tc.wantPayload) || p.Dest != plug {
			t.Errorf("%s: cmd %02x flags %02x payload %x", tc.name, p.Cmd, p.Flags, p.Payload)
		}
	}
	p := power.StatusRequestPacket(plug, 1)
	if p.Cmd != 0x01 || p.Flags != 0xa0 || !bytes.Equal(p.Payload, []byte{0x01, 0x0e}) {
		t.Errorf("status request: cmd %02x flags %02x payload %x", p.Cmd, p.Flags, p.Payload)
	}
	if raw, err := power.LevelToRaw(42.4); err != nil || raw != 85 {
		t.Errorf("LevelToRaw(42.4) = %d, %v", raw, err)
	}
	if _, err := power.LevelToRaw(101); err == nil {
		t.Error("LevelToRaw(101): no error")
	}
}

func TestSetLevelAndStatus(t *testing.T) {
	tr := &hmtest.Transport{Respond: hmtest.AckWith(0x01, 0x01, 0xc8, 0x00, 0x3e)}
	sw := newActuator(t, tr, plug, power.ModelSwitchPlug)
	ev, err := sw.SetLevel(context.Background(), 1, 0x64, 3, 0) // switch: any level > 0 is on, no ramp
	if err != nil || ev == nil || !ev.On() || ev.Percent != 100 {
		t.Fatalf("SetLevel = %+v, %v", ev, err)
	}
	if got := tr.Sent[0]; got.Cmd != 0x11 || !bytes.Equal(got.Payload, []byte{0x02, 0x01, 0xc8, 0x00, 0x00}) || got.Flags&bidcos.Burst != 0 {
		t.Errorf("sent %02x %02x %x", got.Flags, got.Cmd, got.Payload)
	}

	// Plain ACK: no status.
	tr.Respond = hmtest.AckWith(0x00)
	if ev, err := sw.SetLevel(context.Background(), 1, 0, 0, 0); err != nil || ev != nil {
		t.Errorf("SetLevel with plain ACK = %+v, %v", ev, err)
	}
	// NACK.
	tr.Respond = hmtest.AckWith(0x80)
	var nack *hm.NackError
	if _, err := sw.SetLevel(context.Background(), 1, 0, 0, 0); !errors.As(err, &nack) {
		t.Errorf("SetLevel with NACK: %v", err)
	}
	// No response.
	tr.Respond = nil
	if _, err := sw.SetLevel(context.Background(), 1, 0, 0, 0); !errors.Is(err, hm.ErrNoResponse) {
		t.Errorf("SetLevel without response: %v", err)
	}

	// The status request is answered with INFO_LEVEL, not an ACK.
	tr.Respond = func(sent *bidcos.Packet) ([]byte, error) {
		return hmtest.Frame(0x01, 0x40, sent.Msgcnt, 0xa0, 0x10, sent.Dest, sent.Source, 0x06, 0x01, 0x00, 0x00), nil
	}
	dim := newActuator(t, tr, dimmer, power.ModelDimmerPlug)
	ev, err = dim.RequestStatus(context.Background(), 1)
	if err != nil || ev == nil || ev.On() || ev.Frame != power.FrameInfoLevel {
		t.Errorf("RequestStatus = %+v, %v", ev, err)
	}
	tr.Respond = hmtest.AckWith(0x00)
	if _, err := dim.RequestStatus(context.Background(), 1); err == nil {
		t.Error("RequestStatus accepted a plain ACK")
	}
}
