// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package fakemod

import (
	"sync"
	"time"

	bidcosaes "github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/fjaeckel/hmgo/internal/hm/pairing"
	"github.com/fjaeckel/hmgo/internal/hm/params"
	"github.com/fjaeckel/hmgo/internal/hmkey"
)

// Device emulates a classic BidCoS device in config mode for pairing
// tests: it acknowledges configuration messages and keeps its
// registers, handles the AES key exchange (challenging it with its
// current key, like a real device) and answers the central's AES
// challenges to its own messages.
//
// Use Handle as Options.OnLLMACSend (RPI-RF-MOD: software AES, the key
// exchange arrives encrypted) or HandleClassic as Options.OnSend
// (HM-MOD-RPI-PCB: the module encrypts the 2-byte key exchange
// templates itself with ModuleKey and does the AES handshakes).
type Device struct {
	Addr     [3]byte
	ModelID  uint16
	Serial   string
	Firmware byte
	// ModuleKey is the HM-MOD-RPI-PCB's current key (HandleClassic).
	ModuleKey *hmkey.Key

	mu      sync.Mutex
	key     hmkey.Key
	open    *[2]byte                  // channel, list of the open CONFIG_START
	regs    map[[2]byte]map[byte]byte // (channel, list) → register → value
	newKey  [16]byte
	parts   int
	held    []byte  // key exchange message waiting for the central's AES answer
	chal    [6]byte // challenge for held
	sent    map[byte][]byte
	nacks   bool
	configs int
	asleep  bool
	reads   int
	inject  func(msg []byte)
	// configAES: CONFIG_START and CONFIG_WRITE_INDEX are challenged
	// (Handle).
	configAES bool
}

// NewDevice returns a device with the eQ-3 default key (factory state).
func NewDevice(addr [3]byte, modelID uint16, serial string) *Device {
	return &Device{Addr: addr, ModelID: modelID, Serial: serial, Firmware: 0x14, key: hmkey.EQ3Default,
		regs: make(map[[2]byte]map[byte]byte), sent: make(map[byte][]byte)}
}

// DeviceInfo returns the DEVICE_INFO message the device sends when its
// config button is pressed (dst 000000: not paired, a broadcast).
func (d *Device) DeviceInfo(cnt byte, dst [3]byte) []byte {
	p := pairing.EncodeDeviceInfo(pairing.DeviceInfo{Firmware: d.Firmware, ModelID: d.ModelID, Serial: d.Serial, Class: 0x80, Extra: []byte{0x01, 0x01, 0x00}})
	return d.Message(cnt, 0x84, pairing.CmdDeviceInfo, dst, p...)
}

// Message builds a message from the device and remembers it, so that
// the device can answer an AES challenge to it.
func (d *Device) Message(cnt, flags, cmd byte, dst [3]byte, payload ...byte) []byte {
	m := append([]byte{cnt, flags, cmd}, d.Addr[:]...)
	m = append(m, dst[:]...)
	m = append(m, payload...)
	d.mu.Lock()
	d.sent[cnt] = append([]byte(nil), m...)
	d.mu.Unlock()
	return m
}

// Key returns the device's current AES key.
func (d *Device) Key() hmkey.Key {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.key
}

// Reg returns a register value of a channel's list.
func (d *Device) Reg(ch, list, reg byte) (byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.regs[[2]byte{ch, list}][reg]
	return v, ok
}

// Central returns the central address in list 0 (registers 0A-0C).
func (d *Device) Central() [3]byte {
	var c [3]byte
	for i, r := range []byte{pairing.RegCentralHi, pairing.RegCentralMid, pairing.RegCentralLo} {
		c[i], _ = d.Reg(0, 0, r)
	}
	return c
}

// ConfigMessages returns the number of configuration messages received.
func (d *Device) ConfigMessages() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.configs
}

// SetReg sets a register of a channel's list (the device's current
// configuration).
func (d *Device) SetReg(ch, list, reg, val byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	regs := d.regs[[2]byte{ch, list}]
	if regs == nil {
		regs = make(map[byte]byte)
		d.regs[[2]byte{ch, list}] = regs
	}
	regs[reg] = val
}

// SetInject sets where HandleClassic sends the further frames of a
// register read (INFO_PARAM_RESPONSE) after the first, which is the
// response to the request; typically a call of Module.InjectRecv (the
// real HM-MOD-RPI-PCB acknowledges them itself).
func (d *Device) SetInject(f func(msg []byte)) {
	d.mu.Lock()
	d.inject = f
	d.mu.Unlock()
}

// SetKey sets the device's current AES key (as if it had been paired
// with it).
func (d *Device) SetKey(k hmkey.Key) {
	d.mu.Lock()
	d.key = k
	d.mu.Unlock()
}

// SetConfigAES makes Handle challenge CONFIG_START and
// CONFIG_WRITE_INDEX with the device's current key, like a device with
// AES active (AskSinPP validSignature), and apply them only after a
// correct answer.
func (d *Device) SetConfigAES(on bool) {
	d.mu.Lock()
	d.configAES = on
	d.mu.Unlock()
}

// challenge holds msg until the central answers the device's AES
// challenge, and returns the challenge. Caller holds d.mu.
func (d *Device) challenge(msg []byte) []byte {
	d.held = append([]byte(nil), msg...)
	d.chal = [6]byte{0x11, 0x22, 0x33, 0x44, 0x55, byte(msg[0])}
	pl := append([]byte{0x04}, d.chal[:]...)
	return d.reply(msg, 0x02, 0xa0, append(pl, d.key.Index*2)...)
}

// SetAsleep makes the device ignore everything (a battery device that
// is not in config mode).
func (d *Device) SetAsleep(on bool) {
	d.mu.Lock()
	d.asleep = on
	d.mu.Unlock()
}

// ParamReads returns the number of register reads (CONFIG_PARAM_REQ)
// answered.
func (d *Device) ParamReads() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

// paramResponse returns the INFO_PARAM_RESPONSE messages answering the
// register read req (pairs format, the first with the request's message
// counter, flags BIDI|RPTEN like a device's), or nil if p is not a
// register read. Caller holds d.mu.
func (d *Device) paramResponse(req, p []byte) [][]byte {
	if len(p) < 7 || p[1] != pairing.SubConfigParamReq {
		return nil
	}
	d.reads++
	var msgs [][]byte
	for i, pl := range params.EncodeResponse(d.regs[[2]byte{p[0], p[6]}]) {
		m := d.reply(req, 0x10, 0xa0, pl...)
		m[0] = req[0] + byte(i)
		msgs = append(msgs, m)
	}
	return msgs
}

// SetNACK makes the device refuse configuration messages.
func (d *Device) SetNACK(on bool) {
	d.mu.Lock()
	d.nacks = on
	d.mu.Unlock()
}

func (d *Device) reply(req []byte, cmd byte, flags byte, payload ...byte) []byte {
	m := append([]byte{req[0], flags, cmd}, d.Addr[:]...)
	m = append(m, req[3:6]...)
	return append(m, payload...)
}

// config applies a CONFIG message. Caller holds d.mu.
func (d *Device) config(p []byte) bool {
	d.configs++
	if d.nacks || len(p) < 2 {
		return false
	}
	switch p[1] {
	case pairing.SubConfigStart:
		if len(p) < 7 {
			return false
		}
		d.open = &[2]byte{p[0], p[6]}
	case pairing.SubConfigWriteIndex:
		if d.open == nil || d.open[0] != p[0] {
			return false
		}
		regs := d.regs[*d.open]
		if regs == nil {
			regs = make(map[byte]byte)
			d.regs[*d.open] = regs
		}
		for i := 2; i+1 < len(p); i += 2 {
			regs[p[i]] = p[i+1]
		}
	case pairing.SubConfigEnd:
		d.open = nil
	}
	return true
}

// acceptKeyPart stores half of a new key. Caller holds d.mu.
func (d *Device) acceptKeyPart(idx byte, half int, part [8]byte) {
	copy(d.newKey[half*8:], part[:])
	d.parts |= 1 << half
	if d.parts == 3 {
		d.key = hmkey.Key{Bytes: d.newKey, Index: idx}
		d.parts = 0
	}
}

// Handle answers a message from the central (RPI-RF-MOD:
// Options.OnLLMACSend).
func (d *Device) Handle(msg []byte) [][]byte {
	if len(msg) < 9 || [3]byte(msg[6:9]) != d.Addr {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.asleep {
		return nil
	}
	cmd, p := msg[2], msg[9:]
	switch cmd {
	case pairing.CmdConfig:
		if r := d.paramResponse(msg, p); r != nil {
			return r
		}
		if d.configAES && len(p) >= 2 && (p[1] == pairing.SubConfigStart || p[1] == pairing.SubConfigWriteIndex) {
			return [][]byte{d.challenge(msg)}
		}
		if !d.config(p) {
			return [][]byte{d.reply(msg, 0x02, 0x80, 0x80)}
		}
		return [][]byte{d.reply(msg, 0x02, 0x80, 0x00)}
	case pairing.CmdKeyExchange:
		// A key change must be authenticated: challenge it with the
		// current key.
		return [][]byte{d.challenge(msg)}
	case 0x03: // our challenge answered
		if d.held == nil || len(p) < 16 {
			return nil
		}
		held := d.held
		d.held = nil
		var r [16]byte
		copy(r[:], p)
		auth, ok := bidcosaes.NewSigner(d.key).VerifyAuth(d.key.Index, d.chal, held, r)
		if !ok {
			return [][]byte{d.reply(msg, 0x02, 0x80, 0x80)}
		}
		if held[2] == pairing.CmdConfig {
			if !d.config(held[9:]) {
				return [][]byte{d.reply(msg, 0x02, 0x80, 0x80)}
			}
			return [][]byte{d.reply(msg, 0x02, 0x80, append([]byte{0x00}, auth[:]...)...)}
		}
		var b [16]byte
		copy(b[:], held[9:])
		idx, half, part, ok := bidcosaes.DecodeKeyExchange(d.key.Bytes, b)
		if !ok {
			return [][]byte{d.reply(msg, 0x02, 0x80, 0x80)}
		}
		d.acceptKeyPart(idx, half, part)
		return [][]byte{d.reply(msg, 0x02, 0x80, append([]byte{0x00}, auth[:]...)...)}
	case 0x02: // the central's AES challenge to one of our messages
		keyIndex, chal, ok := bidcosaes.ParseChallenge(p)
		orig := d.sent[msg[0]]
		if !ok || orig == nil {
			return nil
		}
		r, err := bidcosaes.NewSigner(d.key).Respond(keyIndex, chal, orig)
		if err != nil {
			return nil
		}
		return [][]byte{d.reply(msg, 0x03, 0xa0, r[:]...)}
	}
	if msg[1]&0x20 != 0 {
		return [][]byte{d.reply(msg, 0x02, 0x80, 0x00)}
	}
	return nil
}

// HandleClassic answers a message the HM-MOD-RPI-PCB transmits
// (Options.OnSend). The module does the AES handshakes and fills the key
// exchange templates with ModuleKey.
func (d *Device) HandleClassic(msg []byte) (byte, []byte) {
	if len(msg) < 9 || [3]byte(msg[6:9]) != d.Addr {
		return DefaultOnSend(msg)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.asleep {
		return AckEUnknown, nil // the module: no answer from the device
	}
	cmd, p := msg[2], msg[9:]
	switch {
	case cmd == pairing.CmdConfig:
		if r := d.paramResponse(msg, p); r != nil {
			if inject := d.inject; inject != nil && len(r) > 1 {
				go func() {
					for _, m := range r[1:] {
						time.Sleep(2 * time.Millisecond)
						inject(m)
					}
				}()
			}
			return AckWithResponse, r[0]
		}
		if !d.config(p) {
			return AckWithResponse, d.reply(msg, 0x02, 0x80, 0x80)
		}
	case cmd == pairing.CmdKeyExchange && len(p) == 2 && p[0] == 0x01 && d.ModuleKey != nil:
		var part [8]byte
		half := int(p[1] % 2)
		copy(part[:], d.ModuleKey.Bytes[half*8:])
		d.acceptKeyPart(d.ModuleKey.Index, half, part)
		return AckWithResponseAESOK, d.reply(msg, 0x02, 0x80, 0x00)
	case msg[1]&0x20 == 0:
		return AckOK, nil
	}
	return AckWithResponse, d.reply(msg, 0x02, 0x80, 0x00)
}
