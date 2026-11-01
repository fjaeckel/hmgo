// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// testConfigHMIP is testConfig with an HmIP-STHO.
var testConfigHMIP = strings.Replace(testConfig, `"groups"`,
	`"hmip": {"address": "B3E83A", "devices": [{"name": "Balcony", "model": "HmIP-STHO", "address": "20A4CE"}]},
  "groups"`, 1)

// sthoEvent is a recorded STATUS frame of an HmIP-STHO (10.4 °C, 77 %,
// 2.6 V) as the module passes it on (event, RSSI -60), from src to dst.
func sthoEvent(src, dst string) string {
	return "07" + "3c" + "50008e" + src + dst + "050c1ff1" + "788320a6" + "00" + "05d4" + "008003001a060000010100684d"
}

func TestHMIPSensor(t *testing.T) {
	m := NewHMIPMonitor(nil, nil)
	e := newEnvWith(t, testConfigHMIP, fakemod.Options{}, func(o *Options) { o.HMIP = m })

	e.waitFor("STHO discovery", func() bool {
		return e.pub.get("homeassistant/sensor/hmbridge_20a4ce/temperature/config") != nil
	})
	if e.pub.get("homeassistant/sensor/hmbridge_20a4ce/battery_voltage/config") == nil {
		t.Error("no battery voltage for the STHO")
	}

	// Not ours: another device, and our device to another address.
	m.Handle(0x02, 0x10, hexPacket(t, sthoEvent("20a4cf", "b3e83a")))
	m.Handle(0x02, 0x11, hexPacket(t, sthoEvent("20a4ce", "b3e83b")))
	m.Handle(0x02, 0x12, hexPacket(t, sthoEvent("20a4ce", "b3e83a")))
	e.waitFor("STHO state", func() bool { return e.pub.get("hmbridge/20a4ce/state") != nil })
	s := e.state("hmbridge/20a4ce/state")
	if s["current_temperature"] != 10.4 || s["humidity"] != 77.0 || s["battery_voltage"] != 2.6 || s["rssi"] != -60.0 ||
		s["messages_received"] != 1.0 || s["last_seen"] == nil {
		t.Errorf("state %v", s)
	}
	if got := string(e.pub.get("hmbridge/20a4ce/availability")); got != "online" {
		t.Errorf("availability %q", got)
	}
	// Only the unconfigured device is listed as heard.
	if h := m.Heard(); len(h) != 1 || h[0].Address != "20A4CF" {
		t.Errorf("heard %+v", h)
	}
	l := []string{"20a4ce", "Balcony", "HmIP-STHO"}
	if v := testutil.ToFloat64(metricBatteryVolts.WithLabelValues(l...)); v != 2.6 {
		t.Errorf("battery volts metric %v", v)
	}
	if v := testutil.ToFloat64(metricHumidity.WithLabelValues(l...)); v != 77 {
		t.Errorf("humidity metric %v", v)
	}

	srv := httptest.NewServer(e.b.API(APIOptions{}))
	defer srv.Close()
	for _, path := range []string{"/api/v1/devices/20a4ce", "/api/v1/devices"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), `"source": "hmip"`) || !strings.Contains(string(body), `"Balcony"`) {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
}
