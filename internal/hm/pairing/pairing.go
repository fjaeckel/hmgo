// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package pairing contains the BidCoS protocol pieces hmbridge needs to
// pair (“teach in”) and unpair classic HomeMatic devices like a CCU
// does: the model table for the model IDs in DEVICE_INFO, and the
// encoders for the configuration messages (CONFIG_START,
// CONFIG_WRITE_INDEX, CONFIG_END) with the registers a central writes.
//
// The message layouts are protocol facts taken from FHEM (10_CUL_HM.pm:
// CUL_HM_pushConfig, the pairing branch for DEVICE_INFO, assignHmKey;
// HMConfig.pm: register definitions) and Homegear
// (HomeMaticCentral.cpp: handlePairingRequest, unpair, reset;
// BidCoSPeer.cpp: the AES key change queue) and from eQ-3's device
// descriptions (rftypes XML, as shipped with Homegear in
// misc/Device Description Files). Both projects are GPL; they were only
// read as references, no code was copied.
//
// Configuration messages (cmd 0x01) are sent BiDi with flags 0xA0
// (BIDI|RPTEN) by both FHEM and Homegear; a device in config mode (its
// config button was pressed, it sent DEVICE_INFO) is awake, so no burst
// is needed. Every message is acknowledged by the device (cmd 0x02,
// payload 0x00); a NACK (0x80) aborts the sequence (Homegear: “Try
// resetting the device to factory defaults before pairing it”).
package pairing

import (
	"fmt"
	"sort"
)

// BidCoS command and CONFIG subcommands used here.
const (
	CmdDeviceInfo  = 0x00
	CmdConfig      = 0x01
	CmdKeyExchange = 0x04 // AES key exchange (see package bidcosaes)
	CmdSet         = 0x11

	SubConfigPeerAdd     = 0x01
	SubConfigPeerRemove  = 0x02
	SubConfigPeerListReq = 0x03
	SubConfigParamReq    = 0x04
	SubConfigStart       = 0x05
	SubConfigEnd         = 0x06
	SubConfigWriteSeq    = 0x07
	SubConfigWriteIndex  = 0x08
	SubConfigSerialReq   = 0x09
	SubConfigPairSerial  = 0x0a
	SubConfigStatusReq   = 0x0e
)

// ConfigFlags are the flags of configuration messages: BIDI|RPTEN
// (FHEM CUL_HM_pushConfig: '++A001…'; Homegear: 0xA0).
const ConfigFlags = 0xa0

// BurstFlag is added for devices that only listen to wake-up bursts
// (wake-on-radio thermostats) when they are not in config mode
// (Homegear unpair: configByte |= 0x10 for wakeOnRadio).
const BurstFlag = 0x10

// List 0 (device list) registers a central writes when pairing
// (FHEM: pushConfig “0201” + 0A/0B/0C = IO id; Homegear
// handlePairingRequest; HMConfig.pm: pairCentral a=>10.0 s=>3.0,
// intKeyVisib a=>2.7).
const (
	RegPairFlags   = 0x02 // bit 0 set while paired (both write 0x01), bit 7 INTERNAL_KEYS_VISIBLE
	RegCentralHi   = 0x0a // PAIRED_TO_CENTRAL / pairCentral, 3 bytes
	RegCentralMid  = 0x0b
	RegCentralLo   = 0x0c
	internalKeysOn = 0x80
)

// List 1 (channel list) register 8, bit 0: AES_ACTIVE (Homegear device
// descriptions: physical index 8.0 list 1; FHEM HMConfig.pm: sign
// a=>8.0 s=>0.1 l=>1). eQ-3's own rftypes describe AES_ACTIVE of the
// contacts as “internal” (central side only); FHEM and Homegear write
// the register on all channels anyway.
const (
	RegAESActive = 0x08
	AESActiveOn  = 0x01
	AESActiveOff = 0x00
)

// ConfigStart returns the payload of CONFIG_START for list of channel
// ch (peer 000000:00): ch 05 00 00 00 00 list.
func ConfigStart(ch, list byte) []byte {
	return []byte{ch, SubConfigStart, 0, 0, 0, 0, list}
}

// ConfigEnd returns the payload of CONFIG_END: ch 06.
func ConfigEnd(ch byte) []byte { return []byte{ch, SubConfigEnd} }

// Reg is one register/value pair of CONFIG_WRITE_INDEX.
type Reg struct{ Addr, Value byte }

// MaxRegsPerWrite is the number of register pairs per CONFIG_WRITE_INDEX
// message (FHEM splits after 28 hex characters = 14 bytes = 7 pairs).
const MaxRegsPerWrite = 7

// ConfigWriteIndex returns the payload of CONFIG_WRITE_INDEX for channel
// ch: ch 08 reg value [reg value…]. It panics if regs is empty or longer
// than MaxRegsPerWrite.
func ConfigWriteIndex(ch byte, regs ...Reg) []byte {
	if len(regs) == 0 || len(regs) > MaxRegsPerWrite {
		panic(fmt.Sprintf("pairing: ConfigWriteIndex with %d registers", len(regs)))
	}
	p := []byte{ch, SubConfigWriteIndex}
	for _, r := range regs {
		p = append(p, r.Addr, r.Value)
	}
	return p
}

// CentralRegs returns the list 0 registers that pair a device to
// central (Homegear handlePairingRequest): 02 = 0x01 (plus 0x80 for
// models with INTERNAL_KEYS_VISIBLE, whose default is true), 0A/0B/0C =
// central address. The zero address unpairs (Homegear unpair: 02 = 00,
// 0A-0C = 00).
func CentralRegs(central [3]byte, internalKeysVisible bool) []Reg {
	flags := byte(0x01)
	if central == ([3]byte{}) {
		flags = 0x00
	} else if internalKeysVisible {
		flags |= internalKeysOn
	}
	return []Reg{
		{RegPairFlags, flags},
		{RegCentralHi, central[0]},
		{RegCentralMid, central[1]},
		{RegCentralLo, central[2]},
	}
}

// Message is one configuration message to send: BidCoS cmd and payload.
type Message struct {
	Cmd     byte
	Payload []byte
	What    string // for logs and errors, e.g. "CONFIG_START list 0"
}

// PairSequence returns the messages that write the central address to
// list 0 (CONFIG_START, CONFIG_WRITE_INDEX, CONFIG_END on channel 0).
// With central 000000 it is the CCU-style unpair.
func PairSequence(central [3]byte, internalKeysVisible bool) []Message {
	return []Message{
		{CmdConfig, ConfigStart(0, 0), "CONFIG_START list 0"},
		{CmdConfig, ConfigWriteIndex(0, CentralRegs(central, internalKeysVisible)...), "CONFIG_WRITE_INDEX central address"},
		{CmdConfig, ConfigEnd(0), "CONFIG_END"},
	}
}

// AESActiveSequence returns the messages that set AES_ACTIVE (list 1
// register 8) of channel ch.
func AESActiveSequence(ch byte, on bool) []Message {
	v := byte(AESActiveOff)
	if on {
		v = AESActiveOn
	}
	return []Message{
		{CmdConfig, ConfigStart(ch, 1), fmt.Sprintf("CONFIG_START channel %d list 1", ch)},
		{CmdConfig, ConfigWriteIndex(ch, Reg{RegAESActive, v}), fmt.Sprintf("CONFIG_WRITE_INDEX channel %d AES_ACTIVE", ch)},
		{CmdConfig, ConfigEnd(ch), fmt.Sprintf("CONFIG_END channel %d", ch)},
	}
}

// FactoryResetPayload is the payload of the reset command (cmd 0x11,
// Homegear HomeMaticCentral::reset: “04 00”). hmbridge unpairs with
// PairSequence(000000) instead, like the CCU's “delete device”; the
// reset is documented here for completeness.
var FactoryResetPayload = []byte{0x04, 0x00}

// RxMode describes when a device listens.
type RxMode int

const (
	// Always: mains powered, always listening (switch, dimmer, LED16).
	Always RxMode = iota
	// WakeOnRadio: battery device that listens for wake-up bursts
	// (thermostats; Homegear receiveMode wakeOnRadio).
	WakeOnRadio
	// ConfigOnly: battery device that only listens in config mode
	// (after its config button was pressed) or right after it sent a
	// message (contacts, remotes, weather sensors).
	ConfigOnly
)

func (m RxMode) String() string {
	switch m {
	case Always:
		return "always"
	case WakeOnRadio:
		return "wake-on-radio"
	case ConfigOnly:
		return "config-only"
	}
	return fmt.Sprintf("RxMode(%d)", int(m))
}

// Model describes a model hmbridge can pair.
type Model struct {
	ID   uint16 // model ID in DEVICE_INFO (bytes 1-2 of the payload)
	Name string // eQ-3 model name as used in the configuration
	RX   RxMode
	AES  bool // supports AES (and thus the key exchange)
	// AESDefault lists the channels on which a CCU enables AES when
	// pairing (eQ-3 rftypes: aes_default="true"; Homegear:
	// encryptionEnabledByDefault).
	AESDefault []byte
	// InternalKeysVisible: list 0 register 2 bit 7 exists and defaults
	// to true (Homegear sets it when pairing).
	InternalKeysVisible bool
	// Source names the device description the entry comes from.
	Source string
}

// models: model IDs from eQ-3's device descriptions (rftypes XML
// <type … id="…"> with the 2-byte const_value at index 10.0, as
// converted by Homegear to <typeNumber>), cross-checked with FHEM's
// HMConfig.pm %culHmModel. Only models hmbridge supports are listed.
var models = []Model{
	{ID: 0x0095, Name: "HM-CC-RT-DN", RX: WakeOnRadio, AES: true, Source: "rf_cc_rt_dn.xml; FHEM 0095"},
	{ID: 0x00ad, Name: "HM-TC-IT-WM-W-EU", RX: WakeOnRadio, AES: true, Source: "rf_tc_it_wm-w-eu.xml; FHEM 00AD"},
	{ID: 0x002f, Name: "HM-Sec-SC", RX: ConfigOnly, AES: true, AESDefault: []byte{1}, Source: "rf_sc.xml; FHEM 002F"},
	{ID: 0x00b1, Name: "HM-Sec-SC-2", RX: ConfigOnly, AES: true, AESDefault: []byte{1}, Source: "rf_sc.xml; FHEM 00B1"},
	{ID: 0x00c7, Name: "HM-Sec-SCo", RX: ConfigOnly, AES: true, AESDefault: []byte{1}, Source: "rf_sec_sco.xml; FHEM 00C7"},
	{ID: 0x0030, Name: "HM-Sec-RHS", RX: ConfigOnly, AES: true, AESDefault: []byte{1}, Source: "rf_rhs.xml; FHEM 0030"},
	{ID: 0x00d8, Name: "HM-LC-Sw1-Pl-DN-R1", RX: Always, AES: true, InternalKeysVisible: true, Source: "rf_s_644.xml; FHEM 00D8"},
	{ID: 0x00b3, Name: "HM-LC-Dim1L-Pl-3", RX: Always, AES: true, InternalKeysVisible: true, Source: "rf_dim_1l_644.xml; FHEM 00B3"},
	{ID: 0x006b, Name: "HM-PB-2-WM55", RX: ConfigOnly, AES: true, AESDefault: []byte{1, 2}, Source: "rf_pb-2-wm55*.xml; FHEM 006B"},
	// The successor HM-PB-2-WM55-2 shares the device description
	// (rf_pb-2-wm55_ge_v1_4.xml) and the frames; it is handled as an
	// HM-PB-2-WM55.
	{ID: 0x00c2, Name: "HM-PB-2-WM55", RX: ConfigOnly, AES: true, AESDefault: []byte{1, 2}, Source: "rf_pb-2-wm55_ge_v1_4.xml (HM-PB-2-WM55-2); FHEM 00C2"},
	{ID: 0x00da, Name: "HM-RC-8", RX: ConfigOnly, AES: true, AESDefault: []byte{1, 2, 3, 4, 5, 6, 7, 8}, Source: "rf_rc-4-2.xml; FHEM 00DA"},
	{ID: 0x006d, Name: "HM-OU-LED16", RX: Always, AES: true, Source: "rf_ou_led16*.xml; FHEM 006D"},
	{ID: 0x003d, Name: "HM-WDS10-TH-O", RX: ConfigOnly, Source: "rf_ash550.xml; FHEM 003D"},
	{ID: 0x003e, Name: "HM-WDS30-T-O", RX: ConfigOnly, Source: "rf_s550ia.xml; FHEM 003E"},
}

// ModelForID returns the model with the given DEVICE_INFO model ID.
func ModelForID(id uint16) (Model, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// ModelByName returns the first model with the given name.
func ModelByName(name string) (Model, bool) {
	for _, m := range models {
		if m.Name == name {
			return m, true
		}
	}
	return Model{}, false
}

// Models returns all pairable models, sorted by model ID.
func Models() []Model {
	res := append([]Model(nil), models...)
	sort.Slice(res, func(i, j int) bool { return res[i].ID < res[j].ID })
	return res
}

// DeviceInfo is a decoded DEVICE_INFO (cmd 0x00) payload:
//
//	fw(1) model(2) serial(10) class(1) peerChannels(2) …
//
// (eQ-3 rftypes: firmware at index 9, model at 10.0 size 2; FHEM
// CUL_HM_infoUpdtDevData: fw, mId, serNo, stc, devInfo; Homegear
// handlePairingRequest: serial bytes 3-12, at least 17 bytes).
type DeviceInfo struct {
	Firmware byte   // e.g. 0x14 = 1.4
	ModelID  uint16 // e.g. 0x0095
	Serial   string // 10 characters
	Class    byte   // device class (“subtype”)
	Extra    []byte // peer channel info and the rest
}

// FirmwareString returns the firmware as “major.minor”.
func (di DeviceInfo) FirmwareString() string {
	return fmt.Sprintf("%d.%d", di.Firmware>>4, di.Firmware&0x0f)
}

// ParseDeviceInfo decodes a DEVICE_INFO payload. It needs at least 14
// bytes; the serial must be printable ASCII.
func ParseDeviceInfo(p []byte) (DeviceInfo, error) {
	if len(p) < 14 {
		return DeviceInfo{}, fmt.Errorf("pairing: DEVICE_INFO payload too short (%d bytes, want ≥ 14)", len(p))
	}
	for _, c := range p[3:13] {
		if c < 0x20 || c > 0x7e {
			return DeviceInfo{}, fmt.Errorf("pairing: DEVICE_INFO serial %x is not printable", p[3:13])
		}
	}
	return DeviceInfo{
		Firmware: p[0],
		ModelID:  uint16(p[1])<<8 | uint16(p[2]),
		Serial:   string(p[3:13]),
		Class:    p[13],
		Extra:    append([]byte(nil), p[14:]...),
	}, nil
}

// EncodeDeviceInfo is the inverse of ParseDeviceInfo (for tests and
// device emulation). serial is padded or cut to 10 characters.
func EncodeDeviceInfo(di DeviceInfo) []byte {
	s := []byte(fmt.Sprintf("%-10.10s", di.Serial))
	p := []byte{di.Firmware, byte(di.ModelID >> 8), byte(di.ModelID)}
	p = append(p, s...)
	p = append(p, di.Class)
	return append(p, di.Extra...)
}
