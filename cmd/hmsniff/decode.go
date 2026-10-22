// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/fjaeckel/hmgo/internal/bidcos"
	bidcosaes "github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/devices"
	"github.com/fjaeckel/hmgo/internal/hm/pairing"
	"github.com/fjaeckel/hmgo/internal/hmip"
	"github.com/fjaeckel/hmgo/internal/hmkey"
)

// Decoder turns module frames into human-readable lines.
type Decoder struct {
	Devices   map[[3]byte]DeviceInfo
	Central   [3]byte // CCU address, if known (for display)
	RedactAES bool

	stats *Stats
	hmip  hmip.Describer // HmIP: names responses after their requests

	// configList remembers the list opened by the last CONFIG_START per
	// device and channel, to name the registers of CONFIG_WRITE_INDEX.
	configList map[[4]byte]byte
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
	0x00: "DEVICE_INFO", 0x01: "CONFIG", 0x02: "ACK", 0x03: "AES_REPLY", 0x04: "AES_KEY_EXCHANGE",
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
	switch {
	case m.Cmd == 0x04 && len(m.Payload) > 2:
		// AES key exchange: the new key, encrypted with the device's
		// old key, which is usually the public eQ-3 default key. Always
		// redacted, even with -no-redact.
		payload = fmt.Sprintf("<%d bytes redacted>", len(m.Payload))
	case d.RedactAES && m.Cmd == 0x03:
		// AES responses: together with the challenge they would allow
		// an offline attack on a weak passphrase.
		payload = fmt.Sprintf("<%d bytes redacted>", len(m.Payload))
	}
	line := fmt.Sprintf("#%02X %s %s %s → %s %s", m.Cnt, flagString(m.Flags), cmd, d.name(m.Src), d.name(m.Dst), payload)
	if extra := d.interpret(m); extra != "" {
		line += " | " + extra
	}
	return line
}

// describeConfig decodes CONFIG (cmd 0x01) messages (subcommand in
// payload byte 1; layouts from FHEM/Homegear, see internal/hm/pairing).
func (d *Decoder) describeConfig(m Msg) string {
	p := m.Payload
	if len(p) < 2 {
		return ""
	}
	ch, sub := p[0], p[1]
	peer := func(b []byte) string {
		if len(b) < 4 {
			return hex.EncodeToString(b)
		}
		return fmt.Sprintf("%X:%d", b[:3], b[3])
	}
	key := [4]byte{m.Dst[0], m.Dst[1], m.Dst[2], ch}
	switch sub {
	case pairing.SubConfigPeerAdd:
		return fmt.Sprintf("CONFIG_PEER_ADD ch %d peer %s", ch, peer(p[2:]))
	case pairing.SubConfigPeerRemove:
		return fmt.Sprintf("CONFIG_PEER_REMOVE ch %d peer %s", ch, peer(p[2:]))
	case pairing.SubConfigPeerListReq:
		return fmt.Sprintf("CONFIG_PEER_LIST_REQ ch %d", ch)
	case pairing.SubConfigParamReq:
		if len(p) >= 7 {
			return fmt.Sprintf("CONFIG_PARAM_REQ ch %d peer %s list %d", ch, peer(p[2:6]), p[6])
		}
	case pairing.SubConfigStart:
		if len(p) >= 7 {
			if d.configList == nil {
				d.configList = make(map[[4]byte]byte)
			}
			d.configList[key] = p[6]
			return fmt.Sprintf("CONFIG_START ch %d peer %s list %d", ch, peer(p[2:6]), p[6])
		}
	case pairing.SubConfigEnd:
		return fmt.Sprintf("CONFIG_END ch %d", ch)
	case pairing.SubConfigWriteSeq:
		if len(p) >= 3 {
			return fmt.Sprintf("CONFIG_WRITE_SEQ ch %d from reg %02X: %s", ch, p[2], hex.EncodeToString(p[3:]))
		}
	case pairing.SubConfigWriteIndex:
		list, known := d.configList[key]
		var regs []string
		for i := 2; i+1 < len(p); i += 2 {
			regs = append(regs, fmt.Sprintf("%02X=%02X", p[i], p[i+1]))
		}
		s := fmt.Sprintf("CONFIG_WRITE_INDEX ch %d", ch)
		if known {
			s += fmt.Sprintf(" list %d", list)
		}
		s += " " + strings.Join(regs, " ")
		if notes := registerNotes(known, list, p[2:]); notes != "" {
			s += " (" + notes + ")"
		}
		return s
	case pairing.SubConfigSerialReq:
		return "CONFIG_SERIAL_REQ"
	case pairing.SubConfigPairSerial:
		return fmt.Sprintf("CONFIG_PAIR_SERIAL %q", p[2:])
	case pairing.SubConfigStatusReq:
		return fmt.Sprintf("CONFIG_STATUS_REQUEST ch %d", ch)
	}
	return fmt.Sprintf("CONFIG ch %d sub %02X", ch, sub)
}

// registerNotes names the registers a central writes when pairing:
// list 0 02 (paired flag, bit 7 INTERNAL_KEYS_VISIBLE) and 0A-0C
// (central address); list 1 08 (AES_ACTIVE).
func registerNotes(known bool, list byte, pairs []byte) string {
	regs := map[byte]byte{}
	for i := 0; i+1 < len(pairs); i += 2 {
		regs[pairs[i]] = pairs[i+1]
	}
	var notes []string
	if !known || list == 0 {
		hi, ok1 := regs[pairing.RegCentralHi]
		mid, ok2 := regs[pairing.RegCentralMid]
		lo, ok3 := regs[pairing.RegCentralLo]
		if ok1 && ok2 && ok3 {
			if hi|mid|lo == 0 {
				notes = append(notes, "central 000000 = unpair")
			} else {
				notes = append(notes, fmt.Sprintf("central %02X%02X%02X", hi, mid, lo))
			}
		}
		if v, ok := regs[pairing.RegPairFlags]; ok {
			notes = append(notes, fmt.Sprintf("reg 02 paired=%d internal_keys_visible=%d", v&1, v>>7))
		}
	}
	if known && list == 1 {
		if v, ok := regs[pairing.RegAESActive]; ok {
			notes = append(notes, fmt.Sprintf("AES_ACTIVE=%d", v&1))
		}
	}
	return strings.Join(notes, ", ")
}

// describeKeyExchange decodes an AES key exchange (cmd 0x04) as far as
// possible without revealing key bytes: a 2-byte template for modules
// that encrypt themselves, or a block encrypted with the eQ-3 default
// key (a freshly paired device), whose new key index and half are shown.
func describeKeyExchange(p []byte) string {
	switch {
	case len(p) == 2 && p[0] == 0x01:
		return fmt.Sprintf("AES key exchange template (module fills in its key): old key index %d, part %d of 2", p[1]/2, p[1]%2+1)
	case len(p) == 16:
		var b [16]byte
		copy(b[:], p)
		if idx, half, _, ok := bidcosaes.DecodeKeyExchange(hmkey.EQ3Default.Bytes, b); ok {
			return fmt.Sprintf("AES key exchange (encrypted with the eQ-3 default key): new key index %d, part %d of 2", idx, half+1)
		}
		return "AES key exchange (encrypted with a non-default key)"
	}
	return "AES key exchange"
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
	switch m.Cmd {
	case 0x00:
		if di, err := pairing.ParseDeviceInfo(m.Payload); err == nil {
			model := "not supported by hmbridge"
			if pm, ok := pairing.ModelForID(di.ModelID); ok {
				model = pm.Name
			}
			s := fmt.Sprintf("DEVICE_INFO fw %s model %04X (%s) serial %s class %02X", di.FirmwareString(), di.ModelID, model, di.Serial, di.Class)
			if len(di.Extra) > 0 {
				s += " peer channels/info " + hex.EncodeToString(di.Extra)
			}
			if m.Dst == ([3]byte{}) {
				s += ", broadcast (not paired)"
			}
			return s
		}
	case 0x01:
		if s := d.describeConfig(m); s != "" {
			return s
		}
	case 0x04:
		return describeKeyExchange(m.Payload)
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
