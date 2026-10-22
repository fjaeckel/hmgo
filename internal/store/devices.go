// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package store

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
)

// DevicesVersion is the format version of devices.json.
const DevicesVersion = 1

// Device is one device paired with hmbridge, as stored in devices.json.
type Device struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Address string `json:"address"` // 6 uppercase hex digits
	Serial  string `json:"serial,omitempty"`
	// Firmware and ModelID as reported in DEVICE_INFO (informational).
	Firmware string `json:"firmware,omitempty"`
	ModelID  string `json:"model_id,omitempty"` // 4 hex digits
	// AESChannels are the channels with AES_ACTIVE (empty: no AES).
	AESChannels []int `json:"aes_channels,omitempty"`
	// KeyIndex is the AES key index the device uses (0 = eQ-3 default
	// key, the configured security.index after a key exchange).
	KeyIndex int       `json:"key_index"`
	Paired   time.Time `json:"paired"`
}

// devicesFile is the on-disk format of devices.json.
type devicesFile struct {
	Version int      `json:"version"`
	Comment string   `json:"_comment,omitempty"`
	Devices []Device `json:"devices"`
}

const devicesComment = "Devices paired with hmbridge. Written by hmbridge on pair, unpair and rename; " +
	"move a device to config.json to manage it there (config.json wins)."

// ErrPairingDisabled is returned when there is no devices file.
var ErrPairingDisabled = errors.New("no devices file (state_dir is \"\")")

// PairingEnabled reports whether paired devices can be stored.
func (s *Store) PairingEnabled() bool { return s.devices != nil }

// DevicesBlocked returns why devices.json must not be written (nil if
// it may be): see File.Blocked.
func (s *Store) DevicesBlocked() error {
	if s.devices == nil {
		return nil
	}
	return s.devices.Blocked()
}

// DevicesPath returns the location of devices.json ("" = disabled).
func (s *Store) DevicesPath() string {
	if s.devices == nil {
		return ""
	}
	return s.devices.Path()
}

// LoadDevices reads the paired devices. A missing file yields none; an
// unusable one is moved aside and reported (see File.Load) — the
// caller logs the error and continues without paired devices.
func (s *Store) LoadDevices() ([]Device, error) {
	if s.devices == nil {
		return nil, nil
	}
	var f devicesFile
	if _, err := s.devices.Load(&f); err != nil {
		return nil, err
	}
	return f.Devices, nil
}

// SaveDevices writes the paired devices (sorted by address)
// synchronously; unchanged content is not rewritten.
func (s *Store) SaveDevices(devs []Device) error {
	if s.devices == nil {
		return ErrPairingDisabled
	}
	sorted := append([]Device{}, devs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Address < sorted[j].Address })
	_, err := s.devices.Save(devicesFile{Version: DevicesVersion, Comment: devicesComment, Devices: sorted})
	return err
}

// Config returns the configuration form of d (to be resolved with
// config.Config.ResolveStoreDevice).
func (d Device) Config() config.Device {
	ki := d.KeyIndex
	return config.Device{
		Name:             d.Name,
		Model:            d.Model,
		Address:          d.Address,
		Serial:           d.Serial,
		AES:              len(d.AESChannels) > 0,
		AESChannels:      append([]int(nil), d.AESChannels...),
		KeyIndexOverride: &ki,
	}
}

// DeviceFromConfig returns the stored form of a resolved device.
func DeviceFromConfig(c *config.Device, firmware, modelID string, paired time.Time) Device {
	var chans []int
	for _, ch := range c.AESChannelList() {
		chans = append(chans, int(ch))
	}
	return Device{
		Name:        c.Name,
		Model:       c.Model,
		Address:     strings.ToUpper(c.AddrHex()),
		Serial:      c.Serial,
		Firmware:    firmware,
		ModelID:     modelID,
		AESChannels: chans,
		KeyIndex:    int(c.KeyIndex()),
		Paired:      paired,
	}
}
