// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/ratelog"
)

// AES enforcement for received frames (see DESIGN.md, “AES
// enforcement”).
//
// Anyone in radio range can send frames with a device's address. For a
// device with AES, a frame may only change hmbridge's state if the radio
// layer authenticated it (bidcos.AuthOK), whatever its destination,
// flags or command. Neither radio module challenges every frame: frames
// without BiDi (the device does not wait for a challenge), ACKs and
// frames whose payload names a channel without AES pass unauthenticated
// (status 0x01). So the bridge decides, per frame, whether an
// unauthenticated frame of an AES device may be used:
//
//   - DEVICE_INFO (0x00, pairing; its handling never trusts it) and time
//     requests (0x3F, answered with the time, rate-limited) never change
//     the state and pass.
//   - ACKs (0x02) arriving on Recv are unsolicited: the answers to our
//     own commands are consumed by the radio layer's Send. They are
//     dropped (the decoders would turn an ACK_STATUS into state).
//   - Frames whose channel is in the payload (KEY_EVENT 0x40,
//     SENSOR_EVENT 0x41: byte 0; INFO_LEVEL 0x10 06: byte 1) pass only
//     if that channel exists and has no AES (e.g. a remote with AES on
//     some buttons only).
//   - Everything else is dropped (conservative: e.g. a thermostat's
//     INFO, whose channel is not in the frame). Frames not addressed to
//     the central (broadcasts, direct links) cannot be challenged at all
//     and are always dropped (like Homegear, BidCoSPeer::packetReceived).

// Reasons of hmbridge_aes_rejected_total.
const (
	aesRejectNotToCentral = "not_to_central"
	aesRejectAck          = "unsolicited_ack"
	aesRejectChannel      = "invalid_channel"
	aesRejectAESChannel   = "aes_channel"
	aesRejectUnknownChan  = "unknown_channel"
	aesRejectFailed       = "aes_failed"
)

// Kinds of hmbridge_rate_limited_total.
const rateKindTimeResponse = "time_response"

// Time responses: at most timeRespBurst at once per device, refilled
// with one per timeRespRefill. A thermostat asks every few hours and
// retries at once when an answer was lost; a spoofed flood of time
// requests costs one transmission per minute and device.
const (
	timeRespBurst  = 2
	timeRespRefill = time.Minute
)

var (
	metricAESRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Name: "aes_rejected_total",
		Help: "Frames of AES devices that were not used because they were not AES-authenticated (or failed), by reason: " +
			"not_to_central (broadcast or direct link: cannot be authenticated), unsolicited_ack (ACK/ACK_STATUS outside " +
			"our own commands), invalid_channel (the payload names a channel the device does not have), aes_channel " +
			"(unauthenticated frame of an AES channel, e.g. sent without BiDi), unknown_channel (the frame does not name " +
			"its channel), aes_failed (the AES handshake failed).",
	}, []string{"reason"})
	metricRateLimited = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "hmbridge", Name: "rate_limited_total",
		Help: "Transmissions hmbridge did not make because a per-device rate limit protecting the duty cycle was " +
			"reached (anyone can send frames with a device's address), by kind: time_response.",
	}, []string{"kind"})
)

func init() {
	prometheus.MustRegister(metricAESRejected, metricRateLimited)
	for _, r := range []string{aesRejectNotToCentral, aesRejectAck, aesRejectChannel, aesRejectAESChannel, aesRejectUnknownChan, aesRejectFailed} {
		metricAESRejected.WithLabelValues(r)
	}
	metricRateLimited.WithLabelValues(rateKindTimeResponse)
}

// frameChannel returns the channel a received frame reports on, where
// the frame names it: KEY_EVENT (0x40) and SENSOR_EVENT (0x41) in the
// low 6 bits of payload byte 0, INFO_LEVEL (0x10 06) in payload byte 1
// (c.f. FHEM CUL_HM_Parse, Homegear IBidCoSInterface).
func frameChannel(pkt *bidcos.Packet) (byte, bool) {
	switch {
	case (pkt.Cmd == 0x40 || pkt.Cmd == 0x41) && len(pkt.Payload) >= 1:
		return pkt.Payload[0] & 0x3f, true
	case pkt.Cmd == bidcos.Info && len(pkt.Payload) >= 2 && pkt.Payload[0] == bidcos.InfoActuatorStatus:
		return pkt.Payload[1] & 0x3f, true
	}
	return 0, false
}

// aesRejection returns why an unauthenticated (or failed) frame of the
// AES device d must not be used, or "" if it may (see the comment at the
// top of this file).
func aesRejection(d *device, pkt *bidcos.Packet, central [3]byte) string {
	if pkt.Auth() == bidcos.AuthFailed {
		return aesRejectFailed
	}
	if pkt.Dest != central {
		return aesRejectNotToCentral
	}
	switch pkt.Cmd {
	case bidcos.DeviceInfo, bidcos.Timestamp:
		return ""
	case bidcos.Ack:
		return aesRejectAck
	}
	ch, ok := frameChannel(pkt)
	switch {
	case !ok:
		return aesRejectUnknownChan
	case ch == 0 || int(ch) > d.cfg.Channels():
		return aesRejectChannel
	case d.cfg.HasAESChannel(ch):
		return aesRejectAESChannel
	}
	return ""
}

// tokenBucket is a simple token bucket.
type tokenBucket struct {
	tokens float64
	at     time.Time
}

// take refills b up to burst (one token per refill since the last call)
// and takes a token if one is left.
func (b *tokenBucket) take(now time.Time, burst int, refill time.Duration) bool {
	if b.at.IsZero() {
		b.tokens = float64(burst)
	} else if d := now.Sub(b.at); d > 0 {
		b.tokens += float64(d) / float64(refill)
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// floodLog rate-limits the bridge's log lines that anyone in radio
// range can trigger (failed AES, undecodable frames of configured
// devices, unknown senders): per kind 10 lines a minute, then a count.
func newFloodLog() *ratelog.Limiter { return ratelog.New(10, time.Minute) }

// Keys of Bridge.floodLog.
const (
	floodKeyAESFailed = "aes_failed"
	floodKeyDecode    = "decode_error"
	floodKeyUnknown   = "unknown_sender"
	floodKeyInvalid   = "invalid_frame"
)
