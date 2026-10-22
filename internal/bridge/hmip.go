// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fjaeckel/hmgo/internal/hmip"
)

// Homematic IP, phase 1 (docs/HMIP.md): the frames the RPI-RF-MOD
// passes on are decoded for the debug log, and the radio addresses they
// come from are listed by the API (/api/v1/unknown). Nothing is sent to the
// module, nothing is published to MQTT.

var metricHMIPFrames = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "hmbridge", Subsystem: "hmip", Name: "frames_total",
	Help: "Homematic IP frames from the radio module (not supported yet), by kind: device (radio frame from " +
		"another device), own (the module reporting its own transmission), other (other module packets), malformed.",
}, []string{"kind"})

func init() {
	prometheus.MustRegister(metricHMIPFrames)
	for _, k := range []string{"device", "own", "other", "malformed"} {
		metricHMIPFrames.WithLabelValues(k)
	}
}

// maxHMIPDevices bounds the list of heard Homematic IP addresses.
const maxHMIPDevices = 100

// HMIPDevice is a Homematic IP radio address heard by the module.
type HMIPDevice struct {
	Address  string    `json:"address"`
	Frames   uint64    `json:"frames"`
	LastSeen time.Time `json:"last_seen"`
	RSSI     *int      `json:"rssi,omitempty"` // dBm, as measured by the module
	To       string    `json:"to"`             // destination of the last frame
	Last     string    `json:"last_frame"`     // content (and application frame) type of the last frame
}

// HMIPMonitor collects the Homematic IP frames of the radio module. Its
// Handle method is meant for uartgw.Config.HMIPHandler.
type HMIPMonitor struct {
	now    func() time.Time
	debugf func(string, ...any)

	mu    sync.Mutex
	desc  hmip.Describer // never shows key material
	heard map[hmip.Address]*HMIPDevice
}

// NewHMIPMonitor returns a monitor; nil now means time.Now, nil debugf
// discards the decoded frames.
func NewHMIPMonitor(now func() time.Time, debugf func(string, ...any)) *HMIPMonitor {
	if now == nil {
		now = time.Now
	}
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	return &HMIPMonitor{now: now, debugf: debugf, heard: make(map[hmip.Address]*HMIPDevice)}
}

// Handle processes one packet from the module (destination, counter,
// command byte and data).
func (m *HMIPMonitor) Handle(dst, cnt byte, packet []byte) {
	p, err := hmip.ParsePacket(dst, cnt, packet)
	if err != nil {
		metricHMIPFrames.WithLabelValues("malformed").Inc()
		m.debugf("hmip: dst %02x: %v", dst, err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.debugf("hmip: %s", m.desc.Describe(false, p))
	if p.Dst != hmip.DstHMIP || p.Cmd != hmip.CmdEvent {
		metricHMIPFrames.WithLabelValues("other").Inc()
		return
	}
	ev, err := p.Event()
	if len(p.Data) < 1+hmip.MinMACHeaderLen {
		metricHMIPFrames.WithLabelValues("malformed").Inc()
		return
	}
	if ev.Echo() {
		metricHMIPFrames.WithLabelValues("own").Inc()
		return
	}
	metricHMIPFrames.WithLabelValues("device").Inc()
	f := ev.Frame
	d := m.heard[f.Src]
	if d == nil {
		if len(m.heard) >= maxHMIPDevices {
			m.evictOldest()
		}
		d = &HMIPDevice{Address: f.Src.String()}
		m.heard[f.Src] = d
	}
	d.Frames++
	d.LastSeen = m.now()
	d.To = f.Dst.String()
	d.RSSI = nil
	if v, ok := ev.RSSI(); ok {
		d.RSSI = &v
	}
	d.Last = f.Content.String()
	if err != nil {
		d.Last += " (malformed)"
	} else if h, _, err := f.App(); err == nil {
		d.Last += " " + h.Type.String()
	} else if f.Content == hmip.ContentNetworkMgmt && len(f.Payload) > 0 {
		d.Last += " " + hmip.NetMgmtTypeName(f.Payload[0])
	}
}

func (m *HMIPMonitor) evictOldest() {
	var oldest hmip.Address
	var t time.Time
	first := true
	for a, d := range m.heard {
		if first || d.LastSeen.Before(t) {
			oldest, t, first = a, d.LastSeen, false
		}
	}
	delete(m.heard, oldest)
}

// Heard returns the heard addresses, most recent first.
func (m *HMIPMonitor) Heard() []HMIPDevice {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]HMIPDevice, 0, len(m.heard))
	for _, d := range m.heard {
		c := *d
		if d.RSSI != nil {
			c.RSSI = ptr(*d.RSSI)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}
