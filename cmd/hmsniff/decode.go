// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/devices"
	"github.com/fjaeckel/hmgo/internal/hmip"
)

// Decoder turns module frames into human-readable lines.
type Decoder struct {
	Devices   map[[3]byte]DeviceInfo
	Central   [3]byte // CCU address, if known (for display)
	RedactAES bool

	stats *Stats
	hmip  hmip.Describer // HmIP: names responses after their requests
}

// Msg is a BidCoS message: cnt flags cmd src(3) dst(3) payload.
type Msg struct {
	Cnt, Flags, Cmd byte
	Src, Dst        [3]byte
	Payload         []byte
}

func parseMsg(b []byte) (Msg, bool) {
	if len(b) < 9 {
		return Msg{}, false
	}
	m := Msg{Cnt: b[0], Flags: b[1], Cmd: b[2], Payload: b[9:]}
	copy(m.Src[:], b[3:6])
	copy(m.Dst[:], b[6:9])
	return m, true
}

// Describe returns one log line for f (without timestamp/direction).
func (d *Decoder) Describe(dir string, f Frame) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-6s cnt=%02X", dstName(f.Dst), f.Cnt)
	if !f.CRCOK {
		sb.WriteString(" BAD-CRC")
	}
	if len(f.Payload) == 0 {
		return sb.String()
	}
	cmd := f.Payload[0]
	p := f.Payload[1:]

	switch f.Dst {
	case dstLLMAC:
		switch {
		case cmd == 0x05 && len(p) >= 4: // RECV: ts(2) info rssi msg…
			fmt.Fprintf(&sb, " RECV ts=%02X%02X info=%02X rssi=%d ", p[0], p[1], p[2], -int(p[3]))
			sb.WriteString(d.describeMsg(dir, p[4:], int(p[3])))
			return sb.String()
		case cmd == 0x06 && len(p) >= 4: // SEND: opts(3) cnt flags cmd src dst payload
			fmt.Fprintf(&sb, " SEND opts=%s ", hex.EncodeToString(p[:3]))
			sb.WriteString(d.describeMsg(dir, p[3:], 0))
			return sb.String()
		case cmd == 0x01:
			fmt.Fprintf(&sb, " ACK %s", hex.EncodeToString(p))
			return sb.String()
		}
	case dstApp:
		switch {
		case cmd == 0x05 && len(p) >= 3: // HM-MOD-RPI-PCB RECV: status info rssi msg…
			fmt.Fprintf(&sb, " RECV status=%02X info=%02X rssi=%d ", p[0], p[1], -int(p[2]))
			sb.WriteString(d.describeMsg(dir, p[3:], int(p[2])))
			return sb.String()
		case cmd == 0x02 && len(p) >= 4: // HM-MOD-RPI-PCB SEND: 00 00 burst msg…
			fmt.Fprintf(&sb, " SEND %s ", hex.EncodeToString(p[:3]))
			sb.WriteString(d.describeMsg(dir, p[3:], 0))
			return sb.String()
		case cmd >= 0x03 && cmd <= 0x0F && cmd != 0x04 && cmd != 0x05 && cmd != 0x06 && cmd != 0x08 && len(p) >= 16:
			// key slots (set current/temp/previous key): never log keys
			fmt.Fprintf(&sb, " cmd=%02X <%d bytes redacted (key material)>", cmd, len(p))
			return sb.String()
		}
	case dstCommon, dstOS:
		if tag := printable(f.Payload); tag != "" {
			fmt.Fprintf(&sb, " cmd=%02X %s %q", cmd, hex.EncodeToString(p), tag)
			return sb.String()
		}
	case dstHmIP, hmip.DstKey:
		// Homematic IP: serial command and MAC header (internal/hmip);
		// key material is always redacted.
		pkt, _ := hmip.ParsePacket(f.Dst, f.Cnt, f.Payload) // non-empty
		sb.WriteString(" " + d.hmip.Describe(dir == "TX", pkt))
		return sb.String()
	}
	if looksLikeKey(f) {
		fmt.Fprintf(&sb, " cmd=%02X <%d bytes redacted (possible key material)>", cmd, len(p))
		return sb.String()
	}
	fmt.Fprintf(&sb, " cmd=%02X %s", cmd, hex.EncodeToString(p))
	return sb.String()
}

// looksLikeKey is a conservative guess for unknown host→module frames that
// may carry an AES key (16 bytes + index). They are always redacted
// (-no-redact only affects AES responses).
func looksLikeKey(f Frame) bool {
	n := len(f.Payload) - 1
	return (f.Dst == dstLLMAC || f.Dst == dstCommon || f.Dst == dstOS) && (n == 16 || n == 17 || n == 18)
}

// printable returns the trailing ASCII run of b (e.g. "DualCoPro_App").
func printable(b []byte) string {
	i := len(b)
	for i > 0 && b[i-1] >= 0x20 && b[i-1] < 0x7F {
		i--
	}
	if len(b)-i >= 4 {
		return string(b[i:])
	}
	return ""
}

var cmdNames = map[byte]string{
	0x00: "DEVICE_INFO", 0x01: "CONFIG", 0x02: "ACK", 0x03: "AES_REPLY", 0x04: "AES_KEY",
	0x10: "INFO", 0x11: "SET", 0x12: "HAVE_DATA", 0x3E: "SWITCH", 0x3F: "TIMESTAMP",
	0x40: "REMOTE", 0x41: "SENSOR_EVENT", 0x53: "SENSOR_DATA", 0x58: "CLIMATE_EVENT",
	0x59: "CLIMATE_TEAM", 0x5A: "THERMALCONTROL", 0x5E: "POWER_CYCLIC", 0x5F: "POWER",
	0x70: "WEATHER",
}

func flagString(f byte) string {
	var s []string
	for _, x := range []struct {
		bit  byte
		name string
	}{{0x01, "WKUP"}, {0x02, "WKMEUP"}, {0x04, "BCAST"}, {0x10, "BURST"}, {0x20, "BIDI"}, {0x40, "RPTED"}, {0x80, "RPTEN"}} {
		if f&x.bit != 0 {
			s = append(s, x.name)
		}
	}
	return strings.Join(s, "|")
}

func (d *Decoder) name(a [3]byte) string {
	if a == ([3]byte{}) {
		return "000000(broadcast)"
	}
	s := fmt.Sprintf("%02X%02X%02X", a[0], a[1], a[2])
	if a == d.Central {
		return s + "(central)"
	}
	if di, ok := d.Devices[a]; ok {
		return s + "(" + di.String() + ")"
	}
	return s
}

// describeMsg decodes a BidCoS message as carried in RECV/SEND frames.
func (d *Decoder) describeMsg(dir string, b []byte, rssi int) string {
	m, ok := parseMsg(b)
	if !ok {
		return "short message " + hex.EncodeToString(b)
	}
	if d.stats != nil {
		d.stats.message(dir, m, rssi, d)
	}
	cmd := cmdNames[m.Cmd]
	if cmd == "" {
		cmd = fmt.Sprintf("cmd%02X", m.Cmd)
	}
	payload := hex.EncodeToString(m.Payload)
	if d.RedactAES && (m.Cmd == 0x03 || (m.Cmd == 0x04 && len(m.Payload) > 0 && m.Payload[0] == 0x01)) {
		// AES responses / key exchange: together with the challenge
		// they would allow an offline attack on a weak passphrase.
		payload = fmt.Sprintf("<%d bytes redacted>", len(m.Payload))
	}
	line := fmt.Sprintf("#%02X %s %s %s → %s %s", m.Cnt, flagString(m.Flags), cmd, d.name(m.Src), d.name(m.Dst), payload)
	if extra := d.interpret(m); extra != "" {
		line += " | " + extra
	}
	return line
}

// interpret decodes the payload for device types hmbridge knows.
func (d *Decoder) interpret(m Msg) string {
	if m.Cmd == 0x02 && len(m.Payload) > 0 {
		switch m.Payload[0] {
		case 0x00:
			return "ACK"
		case 0x01:
			// ACK_STATUS; decoded below by device type
		case 0x04:
			return "AES challenge"
		default:
			if m.Payload[0] >= 0x80 {
				return fmt.Sprintf("NACK %02X", m.Payload[0])
			}
		}
	}
	if m.Cmd == 0x00 {
		if di, err := hm.DecodeDeviceInfo(m.Payload); err == nil {
			return fmt.Sprintf("device info: fw %s model %04X serial %s", di.Firmware, di.ModelID, di.Serial)
		}
	}
	di, ok := d.Devices[m.Src]
	if !ok {
		return ""
	}
	pkt := &bidcos.Packet{Msgcnt: m.Cnt, Flags: m.Flags, Cmd: m.Cmd, Source: m.Src, Dest: m.Dst, Payload: m.Payload}
	sd := hm.NewStandardDevice(nil, m.Src, di.Serial)
	dev, err := devices.New(sd, di.Type)
	if err != nil {
		return "" // a model hmbridge does not support
	}
	ev, err := dev.Decode(pkt)
	if err != nil {
		return "decode error: " + err.Error()
	}
	if ev == nil {
		return ""
	}
	return fmt.Sprintf("%+v", ev)
}
