// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package params describes the device settings (eQ-3's MASTER parameter
// set: the device's list 0 and the channels' list 1, for the thermostats
// also list 7) of the supported models: where each setting lives in the
// device's configuration registers, how its value is encoded, and which
// values are valid. It also parses the device's answer to a register
// read (INFO_PARAM_RESPONSE) and encodes the thermostats' week profile.
//
// The register layouts are protocol facts taken from eQ-3's device
// descriptions (rftypes XML, as converted by Homegear in
// misc/Device Description Files) and cross-checked with FHEM
// (HMConfig.pm: culHmRegDefine, culHmRegModel; 10_CUL_HM.pm: register
// read and write). Both projects are GPL; they were only read as
// references, no code was copied. Every setting names its sources;
// settings whose sources disagree are left out (see the package
// documentation in docs/DESIGN.md, “Device settings”).
//
// Notation: eQ-3 and Homegear give a register position as
// “index.bit” and a size as “bytes.bits”: index 9.5 size 0.1 is bit 5
// of register 9; index 20.0 size 2.0 is the 16-bit big-endian value in
// registers 20 and 21.
package params

import (
	"errors"
	"fmt"
	"sort"
)

// Loc identifies a register list on the wire: the channel byte of the
// CONFIG messages and the list number. For the thermostats, list 7 is
// read and written with channel 0 (FHEM 10_CUL_HM.pm: shRegR/shRegW for
// HM-CC-RT-DN and HM-TC-IT-WM-W-EU, “shadowReg List 7 read from CH 0”;
// eQ-3's descriptions put the list 7 parameters into the device's
// MASTER set).
type Loc struct {
	Channel byte
	List    byte
}

func (l Loc) String() string { return fmt.Sprintf("channel %d list %d", l.Channel, l.List) }

// Regs are register values of one list (register address → value).
type Regs map[byte]byte

// Clone returns a copy of r (nil for nil).
func (r Regs) Clone() Regs {
	if r == nil {
		return nil
	}
	c := make(Regs, len(r))
	for k, v := range r {
		c[k] = v
	}
	return c
}

// Field is a bit field in a register list: Bits bits starting at bit
// Shift (counted from the least significant bit) of the big-endian
// value of Bytes registers starting at Index.
type Field struct {
	Index byte
	Bytes uint8 // 1 or 2
	Shift uint8
	Bits  uint8
}

// Bits returns the field “index.shift size 0.bits”.
func Bits(index byte, shift, bits uint8) Field {
	return Field{Index: index, Bytes: 1, Shift: shift, Bits: bits}
}

// Byte returns the field “index.0 size 1.0” (the whole register).
func Byte(index byte) Field { return Bits(index, 0, 8) }

// Word returns the low bits of the 16-bit value “index.0 size 2.0”
// (eQ-3: mask (1<<bits)-1).
func Word(index byte, bits uint8) Field {
	return Field{Index: index, Bytes: 2, Bits: bits}
}

// full reports whether the field covers all bits of its registers (it
// can be written without knowing the registers' other bits).
func (f Field) full() bool { return f.Shift == 0 && int(f.Bits) == 8*int(f.Bytes) }

func (f Field) mask() uint32 { return (1<<f.Bits - 1) << f.Shift }

// Max returns the largest raw value the field holds.
func (f Field) Max() uint32 { return 1<<f.Bits - 1 }

// valid reports whether the field description is consistent.
func (f Field) valid() bool {
	return (f.Bytes == 1 || f.Bytes == 2) && f.Bits > 0 && int(f.Shift)+int(f.Bits) <= 8*int(f.Bytes) &&
		int(f.Index)+int(f.Bytes) <= 256 && f.Index > 0
}

// Get returns the field's raw value; ok is false if a register is
// missing.
func (f Field) Get(r Regs) (raw uint32, ok bool) {
	var v uint32
	for i := 0; i < int(f.Bytes); i++ {
		b, ok := r[f.Index+byte(i)]
		if !ok {
			return 0, false
		}
		v = v<<8 | uint32(b)
	}
	return (v & f.mask()) >> f.Shift, true
}

// ErrUnknownRegister is returned by Field.Put when the field shares a
// register with other settings whose current value is not known (the
// list has to be read first).
var ErrUnknownRegister = errors.New("params: register value unknown (read the list first)")

// Put stores raw into r, keeping the other bits of the registers. It
// fails if raw does not fit, or if r lacks a register the field shares
// with others (ErrUnknownRegister).
func (f Field) Put(r Regs, raw uint32) error {
	if raw > f.Max() {
		return fmt.Errorf("params: value %d does not fit into %d bits", raw, f.Bits)
	}
	var v uint32
	for i := 0; i < int(f.Bytes); i++ {
		b, ok := r[f.Index+byte(i)]
		if !ok && !f.full() {
			return ErrUnknownRegister
		}
		v = v<<8 | uint32(b)
	}
	v = v&^f.mask() | raw<<f.Shift
	for i := int(f.Bytes) - 1; i >= 0; i-- {
		r[f.Index+byte(i)] = byte(v)
		v >>= 8
	}
	return nil
}

// Known reports whether r holds all registers Put needs.
func (f Field) Known(r Regs) bool {
	if f.full() {
		return true
	}
	_, ok := f.Get(r)
	return ok
}

// Reg is a register/value pair to write.
type Reg struct{ Addr, Value byte }

// Diff returns the registers of want that differ from (or are missing
// in) have, sorted by address.
func Diff(have, want Regs) []Reg {
	var res []Reg
	for a, v := range want {
		if old, ok := have[a]; !ok || old != v {
			res = append(res, Reg{a, v})
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Addr < res[j].Addr })
	return res
}

// INFO subtypes of a register read answer (BidCoS cmd 0x10; FHEM
// 10_CUL_HM.pm, “RegisterRead”: format 02 = address/value pairs, 03 =
// start address and consecutive values).
const (
	InfoParamResponsePairs = 0x02
	InfoParamResponseSeq   = 0x03
)

// INFO_PARAM_CHANGE subtypes: a device reports registers that changed
// (e.g. a setting changed in its own menu): 04 ch peer(4) list pairs…,
// 05 ch peer(4) list start values… (FHEM 10_CUL_HM.pm, “ParamChange”).
const (
	InfoParamChangePairs = 0x04
	InfoParamChangeSeq   = 0x05
)

// IsParamChange reports whether an INFO (cmd 0x10) payload is a
// register change report.
func IsParamChange(payload []byte) bool {
	return len(payload) >= 7 && (payload[0] == InfoParamChangePairs || payload[0] == InfoParamChangeSeq)
}

// ParseParamChange returns the list (with the channel byte as sent),
// the peer (all zero for the MASTER lists) and the changed registers of
// an INFO_PARAM_CHANGE payload.
func ParseParamChange(payload []byte) (loc Loc, peer [4]byte, regs Regs, err error) {
	if !IsParamChange(payload) {
		return Loc{}, peer, nil, fmt.Errorf("params: not a register change report: %x", payload)
	}
	loc = Loc{Channel: payload[1], List: payload[6]}
	copy(peer[:], payload[2:6])
	regs = Regs{}
	resp := append([]byte{InfoParamResponsePairs}, payload[7:]...)
	if payload[0] == InfoParamChangeSeq {
		resp[0] = InfoParamResponseSeq
		if len(payload) == 7 {
			return loc, peer, regs, nil
		}
	}
	if _, err := MergeResponse(regs, resp); err != nil {
		return Loc{}, peer, nil, err
	}
	return loc, peer, regs, nil
}

// IsParamResponse reports whether an INFO (cmd 0x10) payload is a
// register read answer.
func IsParamResponse(payload []byte) bool {
	return len(payload) >= 1 && (payload[0] == InfoParamResponsePairs || payload[0] == InfoParamResponseSeq)
}

// MergeResponse adds the registers of one INFO_PARAM_RESPONSE frame
// (the payload of BidCoS cmd 0x10, starting with the subtype) to r. done
// is true once the frame holds the end marker: the pair 00 00 (pairs
// format) or start address 00 (sequence format). The device sends a
// list in as many frames as needed, each acknowledged by the central;
// a retransmitted frame is merged again, which changes nothing.
func MergeResponse(r Regs, payload []byte) (done bool, err error) {
	if !IsParamResponse(payload) {
		return false, fmt.Errorf("params: not a register read answer: %x", payload)
	}
	data := payload[1:]
	switch payload[0] {
	case InfoParamResponsePairs:
		for i := 0; i+1 < len(data); i += 2 {
			if data[i] == 0 {
				// Register 0 does not exist: 00 00 ends the list.
				return true, nil
			}
			r[data[i]] = data[i+1]
		}
		return false, nil
	default:
		if len(data) == 0 {
			return false, fmt.Errorf("params: empty register sequence")
		}
		start := data[0]
		if start == 0 {
			return true, nil
		}
		for i, v := range data[1:] {
			a := int(start) + i
			if a > 255 {
				return false, fmt.Errorf("params: register sequence beyond register 255")
			}
			r[byte(a)] = v
		}
		return false, nil
	}
}

// MergeResponseChecked is MergeResponse, but fails without changing r
// if the frame reports a register that r already holds with another
// value: a device reports every register of a list once (a
// retransmitted frame repeats the same values), so two values for one
// register in one read mean that someone else sent frames with the
// device's address.
func MergeResponseChecked(r Regs, payload []byte) (done bool, err error) {
	tmp := Regs{}
	done, err = MergeResponse(tmp, payload)
	if err != nil {
		return false, err
	}
	for a, v := range tmp {
		if old, ok := r[a]; ok && old != v {
			return false, fmt.Errorf("params: register %d reported as %02x and %02x in one read (forged frames?)", a, old, v)
		}
	}
	for a, v := range tmp {
		r[a] = v
	}
	return done, nil
}

// EncodeResponse returns the INFO_PARAM_RESPONSE_PAIRS payloads a device
// sends for r (8 pairs per frame, the end marker in the last one). Used
// by the device emulation in tests.
func EncodeResponse(r Regs) [][]byte {
	addrs := make([]int, 0, len(r))
	for a := range r {
		if a != 0 {
			addrs = append(addrs, int(a))
		}
	}
	sort.Ints(addrs)
	var frames [][]byte
	cur := []byte{InfoParamResponsePairs}
	for _, a := range addrs {
		if len(cur) == 17 {
			frames = append(frames, cur)
			cur = []byte{InfoParamResponsePairs}
		}
		cur = append(cur, byte(a), r[byte(a)])
	}
	if len(cur) == 17 {
		frames = append(frames, cur)
		cur = []byte{InfoParamResponsePairs}
	}
	cur = append(cur, 0, 0)
	return append(frames, cur)
}
