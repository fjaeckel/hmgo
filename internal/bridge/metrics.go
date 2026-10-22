// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fjaeckel/hmgo/internal/ha"
)

// Bridge-level metrics. Device-specific metrics (temperatures, valve,
// …) are exported by the hm packages under hmheating_*, hmthermal_* and
// hmcontact_*; radio and MQTT metrics by uartgw and ha.
var (
	deviceLabels = []string{"address", "name", "model"}

	metricLastSeen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "last_seen_timestamp_seconds",
		Help: "Unix time of the last message received from the device.",
	}, deviceLabels)
	metricRSSI = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "rssi_dbm",
		Help: "Signal strength of the last message received from the device.",
	}, deviceLabels)
	metricMessages = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "messages_received",
		Help: "Messages received from the device since the bridge started.",
	}, deviceLabels)
	metricBatteryVolts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "battery_volts",
		Help: "Battery voltage reported by the device.",
	}, deviceLabels)
	metricBatteryPercent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "battery_estimated_percent",
		Help: "Battery level estimated from the voltage (linear 2.2 V = 0 %, 3.0 V = 100 %).",
	}, deviceLabels)
	metricLowBattery = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "low_battery",
		Help: "1 if the device reports a low battery.",
	}, deviceLabels)
	metricWindowOpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "window_open",
		Help: "1 if the device reports an open (or tilted) window.",
	}, deviceLabels)
	metricCurrentTemp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "temperature_celsius",
		Help: "Measured temperature.",
	}, deviceLabels)
	metricTargetTemp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "target_temperature_celsius",
		Help: "Target temperature (4.5 = off).",
	}, deviceLabels)
	metricValve = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "valve_percent",
		Help: "Valve opening of radiator thermostats.",
	}, deviceLabels)
	metricHumidity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "humidity_percent",
		Help: "Relative humidity measured by wall thermostats.",
	}, deviceLabels)
	metricSabotage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "sabotage",
		Help: "1 if a window contact reports sabotage (cover opened).",
	}, deviceLabels)
	metricFault = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "fault",
		Help: "1 for the fault the device currently reports (label fault).",
	}, append(append([]string(nil), deviceLabels...), "fault"))

	metricAuth = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "messages_by_auth_total",
		Help: "Messages received per device by AES verification result.",
	}, []string{"address", "name", "auth"})
	metricForeignDest = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "messages_to_other_address_total",
		Help: "Messages from the device addressed to neither the central nor broadcast (direct links, or a wrong central_address).",
	}, []string{"address", "name"})
	metricAvailable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "available",
		Help: "1 while the device reports regularly, 0 once it has been silent for longer than stale_after.",
	}, deviceLabels)
	metricUnauthenticated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "unauthenticated_ignored_total",
		Help: "Messages from AES devices that were not addressed to the central (so not authenticated) and were ignored.",
	}, []string{"address", "name"})
	metricClockSynced = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "hmbridge", Name: "system_clock_synced",
		Help: "1 if the kernel reports the system clock as NTP-synchronised.",
	})
	metricTimeRequestsSkipped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "hmbridge", Name: "time_requests_skipped_total",
		Help: "Device time requests not answered because the system clock was not synchronised.",
	})
	metricDecodeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "decode_errors_total",
		Help: "Messages from the device that could not be decoded.",
	}, []string{"address", "name"})

	metricLevel = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "level_percent",
		Help: "Level of a switch (0 or 100) or dimmer (0-100).",
	}, deviceLabels)
	metricLEDColor = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "led_color",
		Help: "Color of an HM-OU-LED16 LED: 0 off, 1 red, 2 green, 3 orange.",
	}, append(append([]string(nil), deviceLabels...), "led"))
	metricButtonPresses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "button_presses_total",
		Help: "Button presses of remotes, by button and type (press_short, press_long, press_long_release).",
	}, append(append([]string(nil), deviceLabels...), "button", "type"))
	metricStatusPolls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "status_requests_total",
		Help: "Status requests to actuators by result (ok, error).",
	}, []string{"address", "name", "result"})

	metricFrames = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Name: "frames_received_total",
		Help: "BidCoS frames received, by sender (known, unknown, invalid).",
	}, []string{"sender"})
	metricUnknownSenders = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "hmbridge", Name: "unknown_senders",
		Help: "Number of unconfigured radio addresses that sent messages to the central.",
	})
	metricCommands = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Name: "commands_total",
		Help: "Commands from Home Assistant by action and result (ok, error, rejected).",
	}, []string{"action", "result"})
	metricQueueLength = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "hmbridge", Name: "command_queue_length",
		Help: "Radio jobs waiting to be executed.",
	})
)

func init() {
	prometheus.MustRegister(
		metricLastSeen, metricRSSI, metricMessages,
		metricBatteryVolts, metricBatteryPercent, metricLowBattery,
		metricWindowOpen, metricCurrentTemp, metricTargetTemp,
		metricValve, metricHumidity, metricSabotage, metricFault,
		metricAuth, metricDecodeErrors, metricForeignDest,
		metricAvailable, metricUnauthenticated, metricClockSynced, metricTimeRequestsSkipped,
		metricFrames, metricUnknownSenders, metricCommands, metricQueueLength,
		metricLevel, metricLEDColor, metricButtonPresses, metricStatusPolls,
	)
}

// updateMetrics exports the device state. Caller holds b.mu.
func (b *Bridge) updateMetrics(d *device) {
	l := []string{d.desc.Addr, d.cfg.Name, d.cfg.Model}
	st := &d.state
	if !st.LastSeen.IsZero() {
		metricLastSeen.WithLabelValues(l...).Set(float64(st.LastSeen.Unix()))
	}
	if st.RSSI != nil {
		metricRSSI.WithLabelValues(l...).Set(float64(*st.RSSI))
	}
	metricMessages.WithLabelValues(l...).Set(float64(d.msgs))
	if st.BatteryVoltage != nil {
		metricBatteryVolts.WithLabelValues(l...).Set(*st.BatteryVoltage)
		metricBatteryPercent.WithLabelValues(l...).Set(float64(ha.BatteryPercent(*st.BatteryVoltage)))
	}
	setBool(metricLowBattery, l, st.LowBattery)
	setBool(metricWindowOpen, l, st.WindowOpen)
	setBool(metricSabotage, l, st.Sabotage)
	if st.CurrentTemperature != nil {
		metricCurrentTemp.WithLabelValues(l...).Set(*st.CurrentTemperature)
	}
	if st.TargetTemperature != nil {
		metricTargetTemp.WithLabelValues(l...).Set(*st.TargetTemperature)
	}
	if st.ValvePosition != nil {
		metricValve.WithLabelValues(l...).Set(float64(*st.ValvePosition))
	}
	if st.Humidity != nil {
		metricHumidity.WithLabelValues(l...).Set(float64(*st.Humidity))
	}
	if st.Fault != "" {
		metricFault.DeletePartialMatch(prometheus.Labels{"address": d.desc.Addr})
		metricFault.WithLabelValues(append(l, st.Fault)...).Set(1)
	}
	if st.Level != nil {
		metricLevel.WithLabelValues(l...).Set(*st.Level)
	}
	for i, c := range st.LEDs {
		for v, name := range ha.LEDColors {
			if c == name {
				metricLEDColor.WithLabelValues(append(l, fmt.Sprint(i+1))...).Set(float64(v))
			}
		}
	}
}

func setBool(g *prometheus.GaugeVec, labels []string, v *bool) {
	if v == nil {
		return
	}
	f := 0.0
	if *v {
		f = 1
	}
	g.WithLabelValues(labels...).Set(f)
}
