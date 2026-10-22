// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package remote implements battery powered push buttons and remote
// controls (eQ-3 “remote” family, channel type KEY):
//
//	HM-PB-2-WM55  2-button wall switch   (rftypes rf_pb-2-wm55*.xml, 2 channels)
//	HM-RC-8       8-button remote        (rftypes rf_rc-4-2.xml, 8 channels)
//
// They sleep and only send; they never receive commands from the
// central (rx modes CONFIG, WAKEUP, LAZY_CONFIG: only while the config
// button is pressed).
//
// A button press is a KEY_EVENT, BidCoS cmd 0x40, payload [btn, counter]
// (rftypes KEY_EVENT_SHORT / KEY_EVENT_LONG / KEY_EVENT_LONG_BIDI):
//
//	btn bits 0-5   channel (button number, 1-based)
//	btn bit 6      long press
//	btn bit 7      low battery (FHEM CUL_HM; eQ-3's XML derives LOWBAT
//	               “internally” and marks the bit as 0 only for simulated
//	               key presses, so this is the conventional reading)
//	counter        press counter: incremented per press; the repeats of
//	               one long press and its release carry the same counter
//
// The BidCoS flags tell the long-press phases apart: eQ-3 defines the
// long-press release (PRESS_LONG_RELEASE) as KEY_EVENT_LONG_BIDI, i.e. a
// long KEY_EVENT with the BiDi flag (0x20), while the repeats sent as
// long as the button is held (PRESS_LONG, PRESS_CONT) have no BiDi flag.
// A short press to the central is sent with BiDi as well. BiDi messages
// must be acknowledged (and AES-handshaked on AES channels) by the
// central; the radio module layer does that.
//
// Decode is stateless (one KeyEvent per frame); Tracker turns the frame
// sequence into press_short / press_long / press_long_release events.
package remote

import (
	"fmt"
	"html/template"
	"sync"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
)

// Models.
const (
	ModelPB2WM55 = "HM-PB-2-WM55"
	ModelRC8     = "HM-RC-8"
)

// CmdKeyEvent is BidCoS command 0x40 (KEY_EVENT / remote trigger).
const CmdKeyEvent = 0x40

var buttons = map[string]int{
	ModelPB2WM55: 2,
	ModelRC8:     8,
}

// Buttons returns the number of buttons (channels) of model, or 0 if it
// is not a supported remote.
func Buttons(model string) int { return buttons[model] }

// KeyEvent is a decoded KEY_EVENT frame.
type KeyEvent struct {
	Channel    byte // button, 1-based
	Long       bool
	LowBattery bool
	Counter    byte
	// BiDi is the frame's BiDi flag: set for short presses and for the
	// release of a long press.
	BiDi bool
}

// Release reports whether the frame ends a long press.
func (e *KeyEvent) Release() bool { return e.Long && e.BiDi }

func (e *KeyEvent) String() string {
	kind := "short"
	switch {
	case e.Release():
		kind = "long release"
	case e.Long:
		kind = "long"
	}
	return fmt.Sprintf("button %d %s (counter %d)", e.Channel, kind, e.Counter)
}

func (e *KeyEvent) HTML() template.HTML {
	return template.HTML(template.HTMLEscapeString(e.String()))
}

// DecodeKeyEvent decodes a KEY_EVENT (cmd 0x40) payload; flags are the
// frame's BidCoS flags.
func DecodeKeyEvent(flags byte, payload []byte) (*KeyEvent, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("key event: payload too short: got %d, want >= 2", len(payload))
	}
	ch := payload[0] & hm.Mask6Bit
	if ch == 0 {
		return nil, fmt.Errorf("key event: channel 0")
	}
	return &KeyEvent{
		Channel:    ch,
		Long:       payload[0]&0x40 != 0,
		LowBattery: payload[0]&0x80 != 0,
		Counter:    payload[1],
		BiDi:       flags&bidcos.BiDi != 0,
	}, nil
}

// Remote is a push button or remote control.
type Remote struct {
	hm.StandardDevice

	latestMu sync.RWMutex
	latest   *KeyEvent
}

// NewRemote returns a remote of the given model. sd.BCS may be nil (the
// device never receives commands).
func NewRemote(sd hm.StandardDevice, model string) (*Remote, error) {
	n := buttons[model]
	if n == 0 {
		return nil, fmt.Errorf("remote: unsupported model %q", model)
	}
	sd.NumChannels = n
	sd.ModelName = model
	return &Remote{StandardDevice: sd}, nil
}

func (r *Remote) HomeMaticType() string { return "remote" }

func (r *Remote) MostRecentEvents() []hm.Event {
	r.latestMu.RLock()
	defer r.latestMu.RUnlock()
	if r.latest == nil {
		return nil
	}
	return []hm.Event{r.latest}
}

// Decode decodes KEY_EVENT → *KeyEvent (buttons beyond the model's
// channel count are an error), DEVICE_INFO via hm.DecodeCommon, and
// returns nil, nil for anything else.
func (r *Remote) Decode(pkt *bidcos.Packet) (any, error) {
	if err := r.CheckIncoming(pkt); err != nil {
		return nil, err
	}
	if pkt.Cmd != CmdKeyEvent {
		return hm.DecodeCommon(pkt)
	}
	ev, err := DecodeKeyEvent(pkt.Flags, pkt.Payload)
	if err != nil {
		return nil, err
	}
	if int(ev.Channel) > r.NumChannels {
		return nil, fmt.Errorf("key event: button %d, but %s has %d", ev.Channel, r.ModelName, r.NumChannels)
	}
	r.latestMu.Lock()
	r.latest = ev
	r.latestMu.Unlock()
	return ev, nil
}
