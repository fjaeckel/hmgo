// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ratelog

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(3, time.Minute)
	l.SetNow(func() time.Time { return now })
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }

	for i := 0; i < 100; i++ {
		l.Logf(logf, "aes", "line %d", i)
	}
	l.Logf(logf, "other", "other line")
	// 3 lines, one "rate limit reached" note, the other key.
	if len(lines) != 5 || lines[2] != "line 2" || !strings.Contains(lines[3], "rate limit") || lines[4] != "other line" {
		t.Fatalf("lines = %q", lines)
	}
	now = now.Add(time.Minute)
	lines = nil
	l.Logf(logf, "aes", "new window")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "97 further") || lines[1] != "new window" {
		t.Fatalf("lines = %q", lines)
	}
}
