// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package hmip

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// Describer turns HmIP serial packets into one-line descriptions. It
// remembers the host's requests to name the module's responses (they
// repeat the request's counter). The zero value is ready to use; it is
// not safe for concurrent use.
//
// Key material and values derived from it (see Command.Sensitive,
// destination DstKey, network management payloads) are replaced by
// their length unless ShowSensitive is set.
type Describer struct {
	ShowSensitive bool

	pending map[uint16]Command // dst<<8 | cnt → request
}

// Describe returns a description of p; fromHost is true for packets the
// host wrote to the module.
func (d *Describer) Describe(fromHost bool, p Packet) string {
	if p.Dst == DstKey {
		// Undocumented (see DstKey); the module answers with command
		// 0x03 and the request's counter.
		return fmt.Sprintf("KEY cmd=%02X %s", byte(p.Cmd), d.redact(p.Data))
	}
	key := uint16(p.Dst)<<8 | uint16(p.Cnt)
	if fromHost {
		if d.pending == nil {
			d.pending = make(map[uint16]Command)
		}
		d.pending[key] = p.Cmd // bounded by the 16-bit key
		return d.request(p)
	}
	switch p.Cmd {
	case CmdResponse:
		req, known := d.pending[key]
		delete(d.pending, key)
		return d.response(p, req, known)
	case CmdEvent:
		return d.event(p)
	}
	return fmt.Sprintf("%v %s", p.Cmd, d.data(p.Cmd, p.Data))
}

func (d *Describer) redact(b []byte) string {
	if d.ShowSensitive {
		return hex.EncodeToString(b)
	}
	if len(b) == 0 {
		return ""
	}
	return fmt.Sprintf("<%d bytes redacted>", len(b))
}

// data renders command data generically.
func (d *Describer) data(c Command, b []byte) string {
	if c.Sensitive() {
		return d.redact(b)
	}
	return hex.EncodeToString(b)
}

func (d *Describer) request(p Packet) string {
	var s string
	switch {
	case p.Cmd == CmdSend:
		burst, f, err := p.Send()
		s = fmt.Sprintf("burst=%02X %s", burst, d.frame(f, err, p.Data[min(1, len(p.Data)):]))
	case (p.Cmd == CmdSetAddress || p.Cmd == CmdRemoveLinkPartner) && len(p.Data) == 3:
		s = Address(p.Data).String()
	case p.Cmd == CmdAddLinkPartner && len(p.Data) == 7:
		s = fmt.Sprintf("%v mode=%02X router=%v", Address(p.Data[0:3]), p.Data[3], Address(p.Data[4:7]))
	case p.Cmd == CmdSetSecurityCounter && len(p.Data) == 4:
		s = fmt.Sprintf("counter=%08X", binary.BigEndian.Uint32(p.Data))
	default:
		s = d.data(p.Cmd, p.Data)
	}
	return strings.TrimRight(fmt.Sprintf("%v %s", p.Cmd, s), " ")
}

func (d *Describer) response(p Packet, req Command, known bool) string {
	st, res, err := p.Response()
	if err != nil {
		return fmt.Sprintf("RESPONSE (malformed: %v)", err)
	}
	var sb strings.Builder
	sb.WriteString("RESPONSE ")
	if known {
		fmt.Fprintf(&sb, "to %v ", req)
	}
	sb.WriteString(st.String())
	if len(res) == 0 {
		return sb.String()
	}
	sb.WriteByte(' ')
	switch {
	case !known:
		// Could answer a sensitive request we did not see.
		sb.WriteString(d.redact(res))
	case req == CmdGetLinkPartners && len(res)%3 == 0:
		as, _ := Addresses(res)
		sb.WriteString("partners=")
		for i, a := range as {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(a.String())
		}
	case (req == CmdGetSecurityCounter) && len(res) == 4:
		fmt.Fprintf(&sb, "counter=%08X", binary.BigEndian.Uint32(res))
	case req == CmdGetAddress && len(res) == 3:
		sb.WriteString(Address(res).String())
	default:
		sb.WriteString(d.data(req, res))
	}
	return sb.String()
}

func (d *Describer) event(p Packet) string {
	ev, err := p.Event()
	if len(p.Data) == 0 {
		return fmt.Sprintf("EVENT (malformed: %v)", err)
	}
	rssi := "none"
	if v, ok := ev.RSSI(); ok {
		rssi = fmt.Sprint(v)
	}
	s := fmt.Sprintf("EVENT rssi=%s ", rssi)
	if ev.Echo() {
		s += "(own transmission) "
	}
	return s + d.frame(ev.Frame, err, p.Data[1:])
}

// frame describes a MAC frame decoded from raw (with error err).
func (d *Describer) frame(f Frame, err error, raw []byte) string {
	if err != nil && len(raw) < MinMACHeaderLen {
		return fmt.Sprintf("(malformed: %v) %s", err, hex.EncodeToString(raw))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%v→%v %v sec=%d v%d hop=%d", f.Src, f.Dst, f.Content, f.Security, f.Version, f.HopLimit)
	for _, x := range []struct {
		on   bool
		name string
	}{{f.Fragmented, "frag"}, {f.PiggybackAck, "pback"}, {f.QoSLow, "qos"}} {
		if x.on {
			sb.WriteString(" " + x.name)
		}
	}
	if f.AddrExt != 0 {
		fmt.Fprintf(&sb, " ext=%d", f.AddrExt)
	}
	if f.SecurityFields {
		fmt.Fprintf(&sb, " sn=%08X mic=%x", f.SecurityNumber, f.MIC)
	}
	if f.Cyclic {
		fmt.Fprintf(&sb, " cyclic=%02X", f.CyclicCounter)
	}
	if err != nil {
		fmt.Fprintf(&sb, " (malformed: %v) %s", err, hex.EncodeToString(raw[min(f.Len, len(raw)):]))
		return sb.String()
	}
	sb.WriteString(" | ")
	switch f.Content {
	case ContentApplication:
		h, payload, err := f.App()
		if err != nil {
			fmt.Fprintf(&sb, "(malformed: %v) %s", err, hex.EncodeToString(f.Payload))
			break
		}
		fmt.Fprintf(&sb, "%v seq=%02X", h.Type, h.Seq)
		if h.ResponseRequested {
			sb.WriteString(" RESP_REQ")
		}
		if h.StayAwake {
			sb.WriteString(" STAY_AWAKE")
		}
		if len(payload) > 0 {
			sb.WriteString(" " + hex.EncodeToString(payload))
		}
	case ContentNetworkMgmt:
		if len(f.Payload) == 0 {
			sb.WriteString("(empty)")
			break
		}
		// Inclusion frames carry nonces, one-time keys and the
		// encrypted network key.
		sb.WriteString(NetMgmtTypeName(f.Payload[0]))
		if r := d.redact(f.Payload[1:]); r != "" {
			sb.WriteString(" " + r)
		}
	default:
		sb.WriteString(hex.EncodeToString(f.Payload))
	}
	return strings.TrimRight(sb.String(), " ")
}
