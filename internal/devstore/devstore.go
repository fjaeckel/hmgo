// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package devstore keeps the devices paired with hmbridge itself in a
// small JSON file (config devices_file, default
// /var/lib/hmbridge/devices.json).
//
// The file is SD-card friendly: it is only written when a device is
// paired, unpaired or renamed, never during normal operation, and
// always atomically (temporary file, fsync, rename), so that a power
// cut leaves either the old or the new version.
package devstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
)

// Version is the current file format version.
const Version = 1

// Device is one paired device as stored in the file.
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

// File is the on-disk format.
type File struct {
	Version int      `json:"version"`
	Comment string   `json:"_comment,omitempty"`
	Devices []Device `json:"devices"`
}

const comment = "Devices paired with hmbridge. Written by hmbridge on pair, unpair and rename; " +
	"move a device to config.json to manage it there (config.json wins)."

// Load reads the devices file. A missing file yields no devices and no
// error.
func Load(path string) ([]Device, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if f.Version != Version {
		return nil, fmt.Errorf("%s: unsupported version %d (want %d)", path, f.Version, Version)
	}
	return f.Devices, nil
}

// Save writes devs to path atomically (sorted by address, compact
// indentation).
func Save(path string, devs []Device) error {
	sorted := append([]Device(nil), devs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Address < sorted[j].Address })
	b, err := json.MarshalIndent(File{Version: Version, Comment: comment, Devices: sorted}, "", " ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".devices-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// Make the rename durable (best effort).
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
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

// FromConfig returns the stored form of a resolved device.
func FromConfig(c *config.Device, firmware, modelID string, paired time.Time) Device {
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
