package uartgw

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricsNamespace = "hmbridge"
	metricsSubsystem = "uartgw"
)

var (
	framesReceived = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "frames_received_total",
			Help:      "Frames received from the radio module, by destination and command.",
		},
		[]string{"dst", "cmd"})

	framesSent = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "frames_sent_total",
			Help:      "Frames sent to the radio module, by destination and command.",
		},
		[]string{"dst", "cmd"})

	frameErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "frame_errors_total",
			Help:      "Frames from the radio module which could not be decoded (CRC, length, framing).",
		})

	acks = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "acks_total",
			Help:      "Acks matched to requests, by ack status.",
		},
		[]string{"status"})

	sendRetries = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "send_retries_total",
			Help:      "Radio transmissions retried, by reason (csma_ca, in_progress).",
		},
		[]string{"reason"})

	sendErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "send_errors_total",
			Help:      "Failed radio transmissions, by kind (no_response, no_credits, aes_failed, nack, timeout, busy, other).",
		},
		[]string{"kind"})

	ackTimeouts = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "ack_timeouts_total",
			Help:      "Requests the radio module did not acknowledge in time (module-level timeouts, not devices failing to answer).",
		})

	staleAcks = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "stale_acks_total",
			Help:      "Acks dropped because no request with their counter was outstanding (late or duplicate).",
		})

	recvDropped = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "recv_dropped_total",
			Help:      "Received radio frames dropped because the consumer was too slow.",
		})

	requestDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "request_duration_seconds",
			Help:      "Time between sending a request to the radio module and receiving its ack.",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		})

	upGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "up",
			Help:      "1 if the radio module connection is initialized and healthy, 0 otherwise.",
		})

	infoGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "info",
			Help:      "Radio module firmware version and serial number (always 1).",
		},
		[]string{"firmware", "serial"})

	// FHEM displays the reported value as msgLoadCurrent =
	// int((credits+1)/2) percent and considers the module overloaded
	// at >= 199, i.e. the raw value is the duty cycle usage in units of
	// 0.5%.
	dutyCycle = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "duty_cycle_percent",
			Help:      "Duty cycle usage as reported by the radio module (100 = transmissions blocked).",
		})
)

func init() {
	prometheus.MustRegister(
		framesReceived,
		framesSent,
		frameErrors,
		acks,
		sendRetries,
		sendErrors,
		ackTimeouts,
		staleAcks,
		recvDropped,
		requestDuration,
		upGauge,
		infoGauge,
		dutyCycle,
	)
}

func dstLabel(dst byte) string {
	switch dst {
	case dstOS:
		return "os"
	case dstApp:
		return "app"
	case dstDual:
		return "dual"
	case dstDualErr:
		return "dual_err"
	default:
		return fmt.Sprintf("%d", dst)
	}
}

func cmdLabel(cmd byte) string {
	return fmt.Sprintf("0x%02x", cmd)
}
