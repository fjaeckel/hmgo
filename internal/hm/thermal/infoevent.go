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
	infoEventSetTemperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventSetTemperature",
			Help:      "target temperature in degC",
		},
		[]string{"address", "name"})

	infoEventActualTemperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventActualTemperature",
			Help:      "current temperature in degC",
		},
		[]string{"address", "name"})

	infoEventLowbatReporting = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventLowbatReporting",
			Help:      "low battery as bool",
		},
		[]string{"address", "name"})

	infoEventCommunicationReporting = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventCommunicationReporting",
			Help:      "communication reporting as bool",
		},
		[]string{"address", "name"})

	infoEventWindowOpenReporting = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventWindowOpenReporting",
			Help:      "window open reporting as bool",
		},
		[]string{"address", "name"})

	infoEventBatteryState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventBatteryState",
			Help:      "battery state in V",
		},
		[]string{"address", "name", "hmtype"})

	infoEventControl = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventControl",
			Help:      "control mode",
		},
		[]string{"address", "name"})

	infoEventBoostState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "InfoEventBoostState",
			Help:      "boost state",
		},
		[]string{"address", "name"})
)

func init() {
	prometheus.MustRegister(infoEventSetTemperature)
	prometheus.MustRegister(infoEventActualTemperature)
	prometheus.MustRegister(infoEventLowbatReporting)
	prometheus.MustRegister(infoEventCommunicationReporting)
	prometheus.MustRegister(infoEventWindowOpenReporting)
	prometheus.MustRegister(infoEventBatteryState)
	prometheus.MustRegister(infoEventControl)
	prometheus.MustRegister(infoEventBoostState)
}

type ControlMode uint

const (
	AutoMode ControlMode = iota
	ManuMode
	PartyMode
	BoostMode
)

func (cm ControlMode) String() string {
	switch cm {
	case AutoMode:
		return "auto"
	case ManuMode:
		return "manual"
	case PartyMode:
		return "party"
	case BoostMode:
		return "boost"
	default:
		return fmt.Sprintf("unknown mode (%d dec, %x hex)", uint(cm), uint(cm))
	}
}

// InfoLevelSubtype is the INFO_LEVEL subtype of the TC (cmd 0x10).
const InfoLevelSubtype = 0x0b

// InfoEvent is the TC's INFO_LEVEL frame (cmd 0x10, subtype 0x0B),
// reporting channel 2. Payload layout (rftypes rf_tc_it_wm-w-eu.xml,
// byte offset = index - 9):
//
//	[0]    0x0B
//	[1..2] SET_TEMPERATURE = [1]>>2 (×0.5 °C),
//	       ACTUAL_TEMPERATURE = 10 bit signed ([1]&3)<<8 | [2] (×0.1 °C)
//	[3]    LOWBAT_REPORTING bit 7, COMMUNICATION_REPORTING bit 6,
//	       WINDOW_OPEN_REPORTING bit 5, BATTERY_STATE = ([3]&0x1f)/10+1.5 V
//	[4]    CONTROL_MODE = [4]>>6, BOOST_STATE = [4]&0x3f (minutes)
//	[5..]  party mode data (ignored)
type InfoEvent struct {
	SetTemperature    float64 // in degC; 4.5 = off, 30.5 = on
	ActualTemperature float64 // in degC
	// LowbatReporting is the device's low battery flag.
	LowbatReporting bool
	// CommunicationReporting: the TC could not reach a team peer.
	CommunicationReporting bool
	// WindowOpenReporting: window open (via peered contact or the
	// temperature-fall detection).
	WindowOpenReporting bool
	BatteryState        float64 // in V
	Control             ControlMode
	BoostState          uint64 // remaining boost minutes (only meaningful in BoostMode)
	// Not using party mode, so ignoring the remaining fields.
}

var ieTmpl = template.Must(template.New("infoevent").Parse(`
<strong>Info:</strong><br>
Target temperature: {{ .SetTemperature }} ℃<br>
Current temperature: {{ .ActualTemperature }} ℃<br>
Low battery: {{ .LowbatReporting }}<br>
TODO: Communication: {{ .CommunicationReporting }}<br>
Window open: {{ .WindowOpenReporting }}<br>
Battery state: {{ .BatteryState }} V<br>
Control: {{ .Control }}<br>
Boost state: {{ .BoostState }}<br>
`))

func (ie *InfoEvent) HTML() template.HTML {
	var buf bytes.Buffer
	if err := ieTmpl.Execute(&buf, ie); err != nil {
		return template.HTML(template.HTMLEscapeString(err.Error()))
	}
	return template.HTML(buf.String())
}

func (tc *ThermalControl) DecodeInfoEvent(payload []byte) (*InfoEvent, error) {
	// c.f. <frame id="INFO_LEVEL"> in rftypes/rf_tc_it_wm-w-eu.xml.
	// Frames in party mode are longer; the extra bytes are ignored.
	if got, want := len(payload), 5; got < want {
		return nil, fmt.Errorf("unexpected payload size: got %d, want >= %d", got, want)
	}
	if payload[0] != InfoLevelSubtype {
		return nil, fmt.Errorf("not an INFO_LEVEL frame: subtype 0x%02x", payload[0])
	}
	ie := &InfoEvent{
		SetTemperature:         float64((uint64(payload[1])>>2)&hm.Mask6Bit) / 2,
		ActualTemperature:      signed10(payload[1], payload[2]),
		LowbatReporting:        payload[3]&0x80 != 0,
		CommunicationReporting: payload[3]&0x40 != 0,
		WindowOpenReporting:    payload[3]&0x20 != 0,
		BatteryState:           float64(payload[3]&hm.Mask5Bit)/10 + 1.5,
		Control:                ControlMode((payload[4] >> 6) & hm.Mask2Bit),
		BoostState:             uint64(payload[4] & hm.Mask6Bit),
	}

	infoEventSetTemperature.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(ie.SetTemperature)
	infoEventActualTemperature.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(ie.ActualTemperature)
	infoEventLowbatReporting.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(boolToFloat64(ie.LowbatReporting))
	infoEventCommunicationReporting.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(boolToFloat64(ie.CommunicationReporting))
	infoEventWindowOpenReporting.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(boolToFloat64(ie.WindowOpenReporting))
	infoEventBatteryState.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex(), "hmtype": "thermal"}).Set(ie.BatteryState)
	infoEventControl.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(float64(ie.Control))
	infoEventBoostState.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(float64(ie.BoostState))

	tc.latestMu.Lock()
	defer tc.latestMu.Unlock()
	tc.latestInfoEvent = ie
	return ie, nil
}

func boolToFloat64(val bool) float64 {
	var converted float64
	if val {
		converted = 1
	}
	return converted
}
