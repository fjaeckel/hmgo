package bridge

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stapelberg/hmgo/internal/ha"
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
	metricDecodeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "device", Name: "decode_errors_total",
		Help: "Messages from the device that could not be decoded.",
	}, []string{"address", "name"})

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
		Help: "Climate commands from Home Assistant by action and result (ok, error, rejected).",
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
		metricAuth, metricDecodeErrors,
		metricFrames, metricUnknownSenders, metricCommands, metricQueueLength,
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
