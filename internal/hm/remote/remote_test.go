// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package remote_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/hmtest"
	"github.com/fjaeckel/hmgo/internal/hm/remote"
)

var (
	central = [3]byte{0x45, 0x80, 0x86}
	pb      = [3]byte{0x73, 0x78, 0x8e} // HM-PB-2-WM55 of the user's installation
	rc8     = [3]byte{0x6c, 0x10, 0xce} // HM-RC-8
)

func TestDecodeKeyEvent(t *testing.T) {
	r, err := remote.NewRemote(hm.NewStandardDevice(nil, pb, "Taster"), remote.ModelPB2WM55)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		flags byte
		p     []byte
		want  remote.KeyEvent
	}{
		{"short button 1", 0xa0, []byte{0x01, 0x2e}, remote.KeyEvent{Channel: 1, Counter: 0x2e, BiDi: true}},
		{"short button 2, low battery", 0xa0, []byte{0x82, 0x2f}, remote.KeyEvent{Channel: 2, Counter: 0x2f, BiDi: true, LowBattery: true}},
		{"long held", 0x84, []byte{0x41, 0x30}, remote.KeyEvent{Channel: 1, Long: true, Counter: 0x30}},
		{"long release", 0xa4, []byte{0x41, 0x30}, remote.KeyEvent{Channel: 1, Long: true, Counter: 0x30, BiDi: true}},
	} {
		ev, err := r.Decode(hmtest.Packet(0x01, 0x40, 0x10, tc.flags, remote.CmdKeyEvent, pb, central, tc.p...))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := ev.(*remote.KeyEvent); *got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, *got, tc.want)
		}
	}
	if !(&remote.KeyEvent{Long: true, BiDi: true}).Release() || (&remote.KeyEvent{BiDi: true}).Release() {
		t.Error("Release")
	}

	// Malformed or impossible frames are errors, never panics.
	for _, p := range [][]byte{{0x01}, {}, {0x00, 0x01}, {0x03, 0x01}} {
		if _, err := r.Decode(hmtest.Packet(0x01, 0x40, 0x10, 0xa0, remote.CmdKeyEvent, pb, central, p...)); err == nil {
			t.Errorf("payload %x: no error", p)
		}
	}
	// Other frames are not for this decoder.
	if ev, err := r.Decode(hmtest.Packet(0x01, 0x40, 0x10, 0xa0, 0x41, pb, central, 0x01, 0x02, 0x03)); ev != nil || err != nil {
		t.Errorf("cmd 41: %v, %v", ev, err)
	}

	r8, _ := remote.NewRemote(hm.NewStandardDevice(nil, rc8, "RC"), remote.ModelRC8)
	if ev, err := r8.Decode(hmtest.Packet(0x01, 0x40, 0x10, 0xa0, remote.CmdKeyEvent, rc8, central, 0x08, 0x01)); err != nil || ev.(*remote.KeyEvent).Channel != 8 {
		t.Errorf("RC-8 button 8: %v, %v", ev, err)
	}
	if _, err := remote.NewRemote(hm.NewStandardDevice(nil, rc8, "x"), "HM-RC-4"); err == nil {
		t.Error("unsupported model accepted")
	}
}

func key(ch byte, long, bidi bool, cnt byte) *remote.KeyEvent {
	return &remote.KeyEvent{Channel: ch, Long: long, BiDi: bidi, Counter: cnt}
}

func TestTracker(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	p := func(btn int, typ string, synth bool) remote.Press {
		return remote.Press{Button: btn, Type: typ, Synthetic: synth}
	}
	type step struct {
		ev   *remote.KeyEvent // nil: Expire
		at   int
		want []remote.Press
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"short press with retransmission", []step{
			{key(1, false, true, 5), 0, []remote.Press{p(1, remote.PressShort, false)}},
			{key(1, false, true, 5), 300, nil},
			{key(1, false, true, 6), 2000, []remote.Press{p(1, remote.PressShort, false)}},
		}},
		{"long press: first frame, repeats, release, retransmitted release, late repeat", []step{
			{key(2, true, false, 7), 0, []remote.Press{p(2, remote.PressLong, false)}},
			{key(2, true, false, 7), 250, nil},
			{key(2, true, false, 7), 500, nil},
			{nil, 1000, nil},
			{key(2, true, true, 7), 700, []remote.Press{p(2, remote.PressLongRelease, false)}},
			{key(2, true, true, 7), 900, nil},
			{key(2, true, false, 7), 950, nil},
			{nil, 20000, nil},
		}},
		{"release only (repeats lost or unauthenticated)", []step{
			{key(1, true, true, 9), 0, []remote.Press{p(1, remote.PressLong, true), p(1, remote.PressLongRelease, false)}},
		}},
		{"release lost: timeout", []step{
			{key(3, true, false, 1), 0, []remote.Press{p(3, remote.PressLong, false)}},
			{key(3, true, false, 1), 300, nil},
			{nil, 5000, nil},
			{nil, 5300, []remote.Press{p(3, remote.PressLongRelease, true)}},
			{nil, 9000, nil},
		}},
		{"release lost: next press closes the long press", []step{
			{key(1, true, false, 1), 0, []remote.Press{p(1, remote.PressLong, false)}},
			{key(1, false, true, 2), 1000, []remote.Press{p(1, remote.PressLongRelease, true), p(1, remote.PressShort, false)}},
		}},
		{"buttons are independent", []step{
			{key(1, true, false, 1), 0, []remote.Press{p(1, remote.PressLong, false)}},
			{key(2, false, true, 2), 100, []remote.Press{p(2, remote.PressShort, false)}},
			{key(1, true, true, 1), 200, []remote.Press{p(1, remote.PressLongRelease, false)}},
		}},
		{"release after frames lost for longer than the duplicate window", []step{
			{key(1, true, false, 4), 0, []remote.Press{p(1, remote.PressLong, false)}},
			{key(1, true, true, 4), 12000, []remote.Press{p(1, remote.PressLongRelease, false)}},
			{key(1, true, true, 4), 12100, nil},
		}},
		{"real release after the synthetic one, beyond the duplicate window", []step{
			{key(1, true, false, 4), 0, []remote.Press{p(1, remote.PressLong, false)}},
			{nil, 6000, []remote.Press{p(1, remote.PressLongRelease, true)}},
			{key(1, true, false, 4), 11000, nil},
			{key(1, true, true, 4), 22000, nil},
			{key(1, true, true, 5), 23000, []remote.Press{p(1, remote.PressLong, true), p(1, remote.PressLongRelease, false)}},
		}},
		{"counter reused after the duplicate window", []step{
			{key(1, false, true, 5), 0, []remote.Press{p(1, remote.PressShort, false)}},
			{key(1, false, true, 5), 11000, []remote.Press{p(1, remote.PressShort, false)}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr remote.Tracker
			for i, s := range tc.steps {
				var got []remote.Press
				if s.ev == nil {
					got = tr.Expire(at(s.at))
				} else {
					got = tr.Process(s.ev, at(s.at))
				}
				if !reflect.DeepEqual(got, s.want) {
					t.Errorf("step %d: got %+v, want %+v", i, got, s.want)
				}
			}
		})
	}
}
