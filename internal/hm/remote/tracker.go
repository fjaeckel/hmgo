// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package remote

import (
	"sort"
	"time"
)

// Press types, as used for Home Assistant event types.
const (
	PressShort       = "press_short"
	PressLong        = "press_long"
	PressLongRelease = "press_long_release"
)

// PressTypes lists all press types in display order.
var PressTypes = []string{PressShort, PressLong, PressLongRelease}

// Press is a button press derived from KEY_EVENT frames.
type Press struct {
	Button int
	Type   string // PressShort, PressLong or PressLongRelease
	// Synthetic is set for a release the remote never sent (see
	// Tracker), and for a long press first seen with its release.
	Synthetic bool
}

// Defaults for Tracker.
const (
	// DefaultDuplicateWindow: a frame with the same button and counter
	// as the previous one is a retransmission (or a long-press repeat)
	// only within this time; after it, the counter has wrapped around.
	DefaultDuplicateWindow = 10 * time.Second
	// DefaultReleaseTimeout: a long press without a further repeat or
	// release for this long is considered released.
	DefaultReleaseTimeout = 5 * time.Second
)

type phase int

const (
	phaseIdle phase = iota
	phaseShort
	phaseLong
	phaseReleased
)

type buttonState struct {
	known   bool
	counter byte
	phase   phase
	// synthReleased: the long press was closed by a synthetic release
	// (its real release may still arrive).
	synthReleased bool
	last          time.Time
}

// Tracker turns the KEY_EVENT frames of one remote into presses. It is
// not safe for concurrent use.
//
// The heuristic (per button):
//
//   - short (long bit clear): press_short, unless it repeats the previous
//     frame's counter (a retransmission, e.g. after a lost ACK).
//   - long without BiDi: the first frame with a new counter is
//     press_long; further frames with the same counter are the repeats
//     sent while the button is held and are ignored.
//   - long with BiDi: press_long_release (once per counter). If no long
//     frame was seen for that counter (repeats lost, or not accepted
//     because they were not authenticated), press_long is emitted first.
//   - a long press with no repeat or release for ReleaseTimeout, or
//     followed by a frame with another counter, gets a synthetic
//     press_long_release, so that an automation dimming while the button
//     is held always stops.
//   - long frames with the counter of a long press that is still open
//     or was only released synthetically belong to that press even
//     after DuplicateWindow (frames lost while the button was held):
//     they never start a second press_long.
type Tracker struct {
	DuplicateWindow time.Duration // 0: DefaultDuplicateWindow
	ReleaseTimeout  time.Duration // 0: DefaultReleaseTimeout

	buttons map[byte]*buttonState
}

func (t *Tracker) dupWindow() time.Duration {
	if t.DuplicateWindow > 0 {
		return t.DuplicateWindow
	}
	return DefaultDuplicateWindow
}

func (t *Tracker) releaseTimeout() time.Duration {
	if t.ReleaseTimeout > 0 {
		return t.ReleaseTimeout
	}
	return DefaultReleaseTimeout
}

// Process returns the presses ev (received at now) stands for, in order.
func (t *Tracker) Process(ev *KeyEvent, now time.Time) []Press {
	if t.buttons == nil {
		t.buttons = make(map[byte]*buttonState)
	}
	b := t.buttons[ev.Channel]
	if b == nil {
		b = &buttonState{}
		t.buttons[ev.Channel] = b
	}
	btn := int(ev.Channel)
	sameCounter := b.known && b.counter == ev.Counter
	same := sameCounter && now.Sub(b.last) < t.dupWindow()
	// samePress: a long frame continuing the current long press, however
	// long the gap (the counter only changes with a new press).
	samePress := same || sameCounter && (b.phase == phaseLong || b.phase == phaseReleased && b.synthReleased)
	defer func() {
		b.known, b.counter, b.last = true, ev.Counter, now
	}()

	var out []Press
	closeOpen := func() {
		if b.phase == phaseLong {
			out = append(out, Press{Button: btn, Type: PressLongRelease, Synthetic: true})
			b.phase, b.synthReleased = phaseReleased, true
		}
	}
	switch {
	case !ev.Long:
		if same && b.phase == phaseShort {
			return nil // retransmission
		}
		closeOpen()
		out = append(out, Press{Button: btn, Type: PressShort})
		b.phase = phaseShort

	case !ev.BiDi: // long press, held
		if samePress && (b.phase == phaseLong || b.phase == phaseReleased) {
			return nil // repeat (or a late one after the release)
		}
		closeOpen()
		out = append(out, Press{Button: btn, Type: PressLong})
		b.phase = phaseLong

	default: // long press released
		switch {
		case samePress && b.phase == phaseReleased:
			// retransmitted release, or the real release of a press
			// already released synthetically
			b.synthReleased = false
			return nil
		case samePress && b.phase == phaseLong:
		default:
			closeOpen()
			out = append(out, Press{Button: btn, Type: PressLong, Synthetic: true})
		}
		out = append(out, Press{Button: btn, Type: PressLongRelease})
		b.phase, b.synthReleased = phaseReleased, false
	}
	return out
}

// Expire returns synthetic releases for long presses that have been
// silent for ReleaseTimeout at now.
func (t *Tracker) Expire(now time.Time) []Press {
	var out []Press
	for ch, b := range t.buttons {
		if b.phase == phaseLong && now.Sub(b.last) >= t.releaseTimeout() {
			b.phase, b.synthReleased = phaseReleased, true
			out = append(out, Press{Button: int(ch), Type: PressLongRelease, Synthetic: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Button < out[j].Button })
	return out
}

// Held reports whether a long press is in progress on any button.
func (t *Tracker) Held() bool {
	for _, b := range t.buttons {
		if b.phase == phaseLong {
			return true
		}
	}
	return false
}
