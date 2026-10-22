// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package ratelog limits log lines that anyone in radio range can
// trigger (forged frames, random sender addresses, failed AES): per
// category, the first Burst lines of each Window are logged, further
// ones only counted and summarised in one line once the window has
// passed.
package ratelog

import (
	"sync"
	"time"
)

// Limiter rate-limits log lines per category key. Keys must come from
// a small fixed set (never from received data), so that the limiter's
// memory stays bounded. The zero value is not usable; use New.
type Limiter struct {
	burst  int
	window time.Duration

	mu   sync.Mutex
	now  func() time.Time
	keys map[string]*window
}

type window struct {
	start      time.Time
	n          int // lines logged in this window
	suppressed int // lines dropped in this window
}

// New returns a Limiter logging at most burst lines per key within
// each period per.
func New(burst int, per time.Duration) *Limiter {
	return &Limiter{burst: burst, window: per, now: time.Now, keys: make(map[string]*window)}
}

// SetNow replaces the clock (tests).
func (l *Limiter) SetNow(now func() time.Time) {
	l.mu.Lock()
	l.now = now
	l.mu.Unlock()
}

// Logf logs format/args with logf unless key's budget for the current
// window is used up. The first line of a new window is preceded by a
// summary of the lines suppressed in the previous one.
func (l *Limiter) Logf(logf func(string, ...any), key, format string, args ...any) {
	l.mu.Lock()
	now := l.now()
	w := l.keys[key]
	var summary int
	var span time.Duration
	if w == nil || now.Sub(w.start) >= l.window {
		if w != nil {
			summary, span = w.suppressed, now.Sub(w.start)
		}
		w = &window{start: now}
		l.keys[key] = w
	}
	allow := w.n < l.burst
	if allow {
		w.n++
	} else {
		w.suppressed++
	}
	first := !allow && w.suppressed == 1
	rest := w.start.Add(l.window).Sub(now)
	l.mu.Unlock()
	if summary > 0 {
		logf("%d further %q log lines were suppressed in the last %v (rate limit)", summary, key, span.Round(time.Second))
	}
	if allow {
		logf(format, args...)
	} else if first {
		logf("rate limit for %q log lines reached; suppressing them for %v", key, rest.Round(time.Second))
	}
}
