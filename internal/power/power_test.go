// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package power

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeSysfs creates hwmon sensors (name → alarm value, "" for none) as
// a Raspberry Pi's /sys has them.
func fakeSysfs(t *testing.T, sensors ...[2]string) string {
	t.Helper()
	root := t.TempDir()
	for i, s := range sensors {
		dir := filepath.Join(root, "class/hwmon", fmt.Sprintf("hwmon%d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "name"), []byte(s[0]+"\n"), 0o644)
		if s[1] != "" {
			os.WriteFile(filepath.Join(dir, "in0_lcrit_alarm"), []byte(s[1]+"\n"), 0o644)
		}
	}
	return root
}

func TestFindSensor(t *testing.T) {
	// A Pi 3 B+: the voltage sensor next to the CPU temperature.
	root := fakeSysfs(t, [2]string{"rpi_volt", "0"}, [2]string{"cpu_thermal", ""})
	if got, want := FindSensor(root), filepath.Join(root, "class/hwmon/hwmon0/in0_lcrit_alarm"); got != want {
		t.Errorf("FindSensor = %q, want %q", got, want)
	}
	// Not a Pi.
	if got := FindSensor(fakeSysfs(t, [2]string{"coretemp", ""})); got != "" {
		t.Errorf("FindSensor without rpi_volt = %q", got)
	}
	if got := FindSensor(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Errorf("FindSensor without sysfs = %q", got)
	}
}

func TestMonitor(t *testing.T) {
	root := fakeSysfs(t, [2]string{"cpu_thermal", ""}, [2]string{"rpi_volt", "0"})
	alarm := FindSensor(root)
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	var (
		mu    sync.Mutex
		lines []string
	)
	logf := func(format string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, a...))
		mu.Unlock()
	}
	m := NewMonitor(alarm, metrics, logf)
	var changes []bool
	m.OnChange = func(under bool) {
		mu.Lock()
		changes = append(changes, under)
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx, 5*time.Millisecond)

	set := func(v string) { os.WriteFile(alarm, []byte(v+"\n"), 0o644) }
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for start := time.Now(); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Since(start) > 5*time.Second {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	waitFor("first reading", func() bool { return testutil.ToFloat64(metrics.Undervoltage) == 0 && !m.Undervoltage() })
	set("1")
	waitFor("undervoltage", func() bool { return m.Undervoltage() && testutil.ToFloat64(metrics.Undervoltage) == 1 })
	set("0")
	waitFor("normal", func() bool { return !m.Undervoltage() && testutil.ToFloat64(metrics.Undervoltage) == 0 })
	set("1")
	waitFor("second undervoltage", func() bool { return testutil.ToFloat64(metrics.Events) == 2 })

	// A file that cannot be read keeps the last state.
	os.Remove(alarm)
	time.Sleep(30 * time.Millisecond)
	if !m.Undervoltage() {
		t.Errorf("unreadable alarm file reset the state")
	}

	mu.Lock()
	defer mu.Unlock()
	var warnings, normal int
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "WARNING: undervoltage"):
			warnings++
		case strings.HasPrefix(l, "undervoltage over"):
			normal++
		}
	}
	if want := []bool{true, false, true}; fmt.Sprint(changes) != fmt.Sprint(want) {
		t.Errorf("OnChange calls %v, want %v", changes, want)
	}
	if warnings != 2 || normal != 1 {
		t.Errorf("log: %d warnings, %d recoveries, want 2 and 1: %q", warnings, normal, lines)
	}
}
