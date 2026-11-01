// SPDX-License-Identifier: BSD-3-Clause

package ha

import "testing"

func TestSTHODiscovery(t *testing.T) {
	kind, ok := KindForModel("HmIP-STHO")
	if !ok || kind != WeatherSensor || !HasHumidity("HmIP-STHO") {
		t.Fatalf("kind %v %v, humidity %v", kind, ok, HasHumidity("HmIP-STHO"))
	}
	got := discovered(t, DeviceDesc{Addr: "20a4ce", Name: "Balcony", Model: "HmIP-STHO", Kind: WeatherSensor})
	for _, w := range []string{"sensor/hmbridge_20a4ce/temperature", "sensor/hmbridge_20a4ce/humidity",
		"sensor/hmbridge_20a4ce/battery_voltage", "sensor/hmbridge_20a4ce/rssi", "sensor/hmbridge_20a4ce/last_seen"} {
		if got[w] == nil {
			t.Errorf("STHO lacks %s", w)
		}
	}
	// Its status frame has no known low battery flag, and no AES.
	for _, nw := range []string{"binary_sensor/hmbridge_20a4ce/low_battery", "sensor/hmbridge_20a4ce/auth_status"} {
		if got[nw] != nil {
			t.Errorf("STHO has %s", nw)
		}
	}
	// The BidCoS weather sensors keep low battery and get no voltage.
	th := discovered(t, DeviceDesc{Addr: "6fca4a", Name: "Garten", Model: "HM-WDS10-TH-O", Kind: WeatherSensor})
	if th["binary_sensor/hmbridge_6fca4a/low_battery"] == nil || th["sensor/hmbridge_6fca4a/battery_voltage"] != nil {
		t.Errorf("TH-O entities: %v", keys(th))
	}
}
