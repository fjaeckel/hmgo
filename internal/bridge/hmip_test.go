// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"encoding/hex"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

func hexPacket(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHMIPMonitor(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 39, 37, 0, time.UTC)
	var logged []string
	m := NewHMIPMonitor(func() time.Time { return now }, func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	before := map[string]float64{}
	for _, k := range []string{"device", "own", "other", "malformed"} {
		before[k] = testutil.ToFloat64(metricHMIPFrames.WithLabelValues(k))
	}

	// Captured (restart captures): the module reporting its own
	// transmission, a response, a frame from a device, and a frame on
	// the key destination.
	m.Handle(0x02, 0x0c, hexPacket(t, "0780120082b4e47af00001000000000000000088c0"))
	m.Handle(0x02, 0x14, hexPacket(t, "060147def5"))
	m.Handle(0x02, 0x16, hexPacket(t, "074512008347def5f000010589067df060023e8804"))
	m.Handle(0x0c, 0x13, hexPacket(t, "0301c4ed252209d658b106e28996f1bfd9eb"))
	m.Handle(0x02, 0x17, nil)
	m.Handle(0x02, 0x18, hexPacket(t, "07451200")) // truncated

	for k, want := range map[string]float64{"device": 1, "own": 1, "other": 2, "malformed": 2} {
		if got := testutil.ToFloat64(metricHMIPFrames.WithLabelValues(k)) - before[k]; got != want {
			t.Errorf("%s frames: %v, want %v", k, got, want)
		}
	}
	heard := m.Heard()
	if len(heard) != 1 {
		t.Fatalf("heard %+v", heard)
	}
	h := heard[0]
	if h.Address != "47DEF5" || h.Frames != 1 || !h.LastSeen.Equal(now) || h.RSSI == nil || *h.RSSI != -69 || h.To != "F00001" || h.Last != "CT2" {
		t.Errorf("heard %+v", h)
	}
	log := strings.Join(logged, "\n")
	if !strings.Contains(log, "hmip: EVENT rssi=-69 47DEF5→F00001 CT2") || strings.Contains(log, "c4ed2522") {
		t.Errorf("debug log:\n%s", log)
	}

	// The status page lists it.
	e := newEnvWith(t, testConfig, fakemod.Options{}, func(o *Options) { o.HMIP = m })
	srv := httptest.NewServer(e.b.StatusHandler(Info{}))
	defer srv.Close()
	for path, want := range map[string]string{"/": "HomematicIP devices heard (not supported yet)", "/api/state": `"hmip_heard"`} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), want) || !strings.Contains(string(body), "47DEF5") {
			t.Errorf("%s lacks %q / 47DEF5:\n%s", path, want, body)
		}
	}
	// Nothing about it reaches MQTT.
	e.pub.mu.Lock()
	defer e.pub.mu.Unlock()
	for topic, payload := range e.pub.msgs {
		if strings.Contains(strings.ToLower(topic+string(payload)), "47def5") {
			t.Errorf("published %s", topic)
		}
	}
}

func TestHMIPMonitorBounded(t *testing.T) {
	m := NewHMIPMonitor(nil, nil)
	for i := 0; i < maxHMIPDevices+10; i++ {
		m.Handle(0x02, 0, hexPacket(t, fmt.Sprintf("0745120083%06xf000010589067df060023e8804", i)))
	}
	if n := len(m.Heard()); n != maxHMIPDevices {
		t.Errorf("%d heard, want %d", n, maxHMIPDevices)
	}
}
