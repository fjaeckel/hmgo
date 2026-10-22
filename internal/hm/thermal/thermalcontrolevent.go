// Copyright (c) 2017 Michael Stapelberg. All rights reserved.
// Copyright (c) 2026 Frederic Jung.
// SPDX-License-Identifier: BSD-3-Clause
//
// Derived from hmgo (https://github.com/stapelberg/hmgo); see licenses/.

package thermal

import (
	"bytes"
	"fmt"
	"html/template"

	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	thermalControlEventSetTemperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "ThermalControlEventSetTemperature",
			Help:      "Temperature in degC",
		},
		[]string{"address", "name"})

	thermalControlEventActualTemperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "ThermalControlEventActualTemperature",
			Help:      "Temperature in degC",
		},
		[]string{"address", "name"})

	thermalControlEventActualHumidity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "ThermalControlEventActualHumidity",
			Help:      "Humidity in percentage points",
		},
		[]string{"address", "name"})
)

func init() {
	prometheus.MustRegister(thermalControlEventSetTemperature)
	prometheus.MustRegister(thermalControlEventActualTemperature)
	prometheus.MustRegister(thermalControlEventActualHumidity)
}

type ThermalControlEvent struct {
	SetTemperature    float64 // in degC
	ActualTemperature float64 // in degC
	ActualHumidity    float64 // in percentage points
}

var tceTmpl = template.Must(template.New("thermalcontrolevent").Parse(`
<strong>ThermalControl:</strong><br>
Target temperature: {{ .SetTemperature }} ℃<br>
Current temperature: {{ .ActualTemperature }} ℃<br>
Humidity: {{ .ActualHumidity }}%<br>
`))

func (tce *ThermalControlEvent) HTML() template.HTML {
	var buf bytes.Buffer
	if err := tceTmpl.Execute(&buf, tce); err != nil {
		return template.HTML(template.HTMLEscapeString(err.Error()))
	}
	return template.HTML(buf.String())
}

func (tc *ThermalControl) DecodeThermalControlEvent(payload []byte) (*ThermalControlEvent, error) {
	// c.f. <frame id="THERMALCONTROL_EVENT"> in rftypes/tc.xml:
	// SET_TEMPERATURE = [0]>>2 (×0.5 °C), ACTUAL_TEMPERATURE = 10 bit
	// signed ([0]&3)<<8|[1] (×0.1 °C), ACTUAL_HUMIDITY = [2] (%).
	if got, want := len(payload), 3; got != want {
		return nil, fmt.Errorf("unexpected payload size: got %d, want %d", got, want)
	}
	tce := &ThermalControlEvent{
		SetTemperature:    float64((uint64(payload[0])>>2)&hm.Mask6Bit) / 2,
		ActualTemperature: signed10(payload[0], payload[1]),
		ActualHumidity:    float64(payload[2]),
	}

	thermalControlEventSetTemperature.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(tce.SetTemperature)
	thermalControlEventActualTemperature.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(tce.ActualTemperature)
	thermalControlEventActualHumidity.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(float64(tce.ActualHumidity))

	tc.latestMu.Lock()
	defer tc.latestMu.Unlock()
	tc.latestThermalControlEvent = tce
	return tce, nil
}

// signed10 decodes the 10 bit signed temperature (×0.1 °C) in the low 2
// bits of hi and all of lo (rftypes: signed="true" size="1.2").
func signed10(hi, lo byte) float64 {
	v := int(hi&hm.Mask2Bit)<<8 | int(lo)
	if v&0x200 != 0 {
		v -= 0x400
	}
	return float64(v) / 10
}
