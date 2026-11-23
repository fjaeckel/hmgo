// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package power watches the Raspberry Pi's undervoltage alarm.
//
// The Pi's firmware detects when the supply voltage drops below about
// 4.63 V. Linux's raspberrypi-hwmon driver (hwmon sensor "rpi_volt")
// asks the firmware every 2 seconds and shows the result as
// in0_lcrit_alarm: 1 if the voltage was too low since the previous
// query. A Pi on a weak power supply corrupts SD cards and resets USB
// and radio hardware, so hmbridge reports it: as metrics, in the log
// and in its health report.
package power

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// SensorName is the hwmon name of the Raspberry Pi's voltage sensor.
const SensorName = "rpi_volt"

// PollInterval is how often the alarm is read: as often as the driver
// queries the firmware.
const PollInterval = 2 * time.Second

// FindSensor returns the path of the undervoltage alarm file under
// sysfsRoot (normally "/sys"), or "" if the system has no such sensor
// (not a Raspberry Pi, or a kernel without the driver).
func FindSensor(sysfsRoot string) string {
	names, _ := filepath.Glob(filepath.Join(sysfsRoot, "class/hwmon/hwmon*/name"))
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil || string(bytes.TrimSpace(b)) != SensorName {
			continue
		}
		alarm := filepath.Join(filepath.Dir(n), "in0_lcrit_alarm")
		if _, err := os.Stat(alarm); err == nil {
			return alarm
		}
	}
	return ""
}

// Metrics are the monitor's metrics. NewMetrics registers them.
type Metrics struct {
	Undervoltage prometheus.Gauge
	Events       prometheus.Counter
}

// NewMetrics creates the metrics and registers them with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Undervoltage: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "hmbridge", Subsystem: "system", Name: "undervoltage",
			Help: "1 while the Raspberry Pi reports undervoltage (its power supply is too weak), else 0.",
		}),
		Events: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "hmbridge", Subsystem: "system", Name: "undervoltage_events_total",
			Help: "Times the Raspberry Pi started reporting undervoltage.",
		}),
	}
	reg.MustRegister(m.Undervoltage, m.Events)
	return m
}

// Monitor polls the undervoltage alarm.
type Monitor struct {
	// OnChange, if set before Run, is called after the state changed.
	OnChange func(undervoltage bool)

	path    string
	metrics *Metrics
	logf    func(string, ...any)

	mu    sync.Mutex
	under bool
}

// NewMonitor returns a monitor of the alarm file path (see FindSensor).
func NewMonitor(path string, metrics *Metrics, logf func(string, ...any)) *Monitor {
	return &Monitor{path: path, metrics: metrics, logf: logf}
}

// Undervoltage reports whether the last reading was an undervoltage
// alarm.
func (m *Monitor) Undervoltage() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.under
}

// Run reads the alarm every interval until ctx is done.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	m.poll()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.poll()
		}
	}
}

// poll reads the alarm once. A file that cannot be read keeps the last
// state.
func (m *Monitor) poll() {
	b, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	under := string(bytes.TrimSpace(b)) == "1"
	m.mu.Lock()
	changed := under != m.under
	m.under = under
	m.mu.Unlock()
	if under {
		m.metrics.Undervoltage.Set(1)
	} else {
		m.metrics.Undervoltage.Set(0)
	}
	if !changed {
		return
	}
	if m.OnChange != nil {
		defer m.OnChange(under)
	}
	if under {
		m.metrics.Events.Inc()
		m.logf("WARNING: undervoltage: the Raspberry Pi's power supply is too weak (below about 4.63 V); this can corrupt the SD card and reset the radio module. Use a stronger power supply (2.5 A for a Pi 3, 3 A for a Pi 4) and a short, thick cable")
	} else {
		m.logf("undervoltage over: the supply voltage is normal again")
	}
}
