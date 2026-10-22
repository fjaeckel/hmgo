// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Prometheus metrics, registered with the default registry
// (namespace "hmbridge", subsystem "mqtt").
var (
	metricConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "hmbridge", Subsystem: "mqtt", Name: "connected",
		Help: "1 if the MQTT connection is up, 0 otherwise.",
	})
	metricConnectAttempts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "mqtt", Name: "connection_attempts_total",
		Help: "Initial MQTT connection attempts (automatic reconnects are not counted individually).",
	})
	metricConnectionLosses = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "mqtt", Name: "connection_losses_total",
		Help: "MQTT connection losses.",
	})
	metricPublished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "mqtt", Name: "messages_published_total",
		Help: "MQTT messages published successfully, by kind (discovery, state, availability, other).",
	}, []string{"kind"})
	metricPublishErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "mqtt", Name: "publish_errors_total",
		Help: "MQTT publish failures (including while disconnected).",
	})
	metricCommands = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "mqtt", Name: "commands_received_total",
		Help: "Valid commands received from Home Assistant, by kind (temperature, mode, preset).",
	}, []string{"kind"})
	metricInvalidCommands = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "mqtt", Name: "invalid_commands_total",
		Help: "Commands dropped because they were invalid (or retained, or the queue was full).",
	})
	metricHABirth = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "hmbridge", Subsystem: "mqtt", Name: "ha_birth_messages_total",
		Help: "Home Assistant birth (\"online\") messages received.",
	})
)

func init() {
	prometheus.MustRegister(
		metricConnected,
		metricConnectAttempts,
		metricConnectionLosses,
		metricPublished,
		metricPublishErrors,
		metricCommands,
		metricInvalidCommands,
		metricHABirth,
	)
	for _, k := range []string{"discovery", "state", "availability", "other"} {
		metricPublished.WithLabelValues(k)
	}
	for _, k := range []string{"temperature", "mode", "preset"} {
		metricCommands.WithLabelValues(k)
	}
}

// messageKind classifies a published topic for metrics.
func (c TopicConfig) messageKind(topic string) string {
	switch {
	case topic == c.AvailabilityTopic(), strings.HasPrefix(topic, c.base()+"/") && strings.HasSuffix(topic, "/availability"):
		return "availability"
	case strings.HasPrefix(topic, c.prefix()+"/"):
		return "discovery"
	case strings.HasPrefix(topic, c.base()+"/") && strings.HasSuffix(topic, "/state"):
		return "state"
	}
	return "other"
}

func (k CommandKind) metricLabel() string {
	switch k {
	case SetTemperature:
		return "temperature"
	case SetMode:
		return "mode"
	case SetPreset:
		return "preset"
	}
	return "unknown"
}
