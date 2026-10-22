// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package store

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// SettingsFileName is the file of the device settings cache.
const SettingsFileName = "settings.json"

// SettingsVersion is the format version of settings.json.
const SettingsVersion = 1

// RegList is the cached content of one register list of a device.
type RegList struct {
	Channel byte
	List    byte
	Regs    map[byte]byte
}

// regListJSON is the on-disk form of RegList: the registers as one hex
// string of address/value pairs, sorted by address (compact and
// deterministic, so that an unchanged cache is never rewritten).
type regListJSON struct {
	Channel byte   `json:"channel"`
	List    byte   `json:"list"`
	Regs    string `json:"regs"`
}

// MarshalJSON implements json.Marshaler.
func (l RegList) MarshalJSON() ([]byte, error) {
	addrs := make([]int, 0, len(l.Regs))
	for a := range l.Regs {
		addrs = append(addrs, int(a))
	}
	sort.Ints(addrs)
	b := make([]byte, 0, 2*len(addrs))
	for _, a := range addrs {
		b = append(b, byte(a), l.Regs[byte(a)])
	}
	return json.Marshal(regListJSON{Channel: l.Channel, List: l.List, Regs: hex.EncodeToString(b)})
}

// UnmarshalJSON implements json.Unmarshaler.
func (l *RegList) UnmarshalJSON(data []byte) error {
	var j regListJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	b, err := hex.DecodeString(j.Regs)
	if err != nil || len(b)%2 != 0 {
		return fmt.Errorf("invalid registers %q", j.Regs)
	}
	l.Channel, l.List, l.Regs = j.Channel, j.List, make(map[byte]byte, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		l.Regs[b[i]] = b[i+1]
	}
	return nil
}

// DeviceSettings is the settings cache of one device: the register
// lists last read from it.
type DeviceSettings struct {
	// Model guards against a cache of another model at the same
	// address (the configuration changed): such a cache is not used.
	Model  string    `json:"model"`
	ReadAt time.Time `json:"read_at"`
	Lists  []RegList `json:"lists"`
}

type settingsFile struct {
	Version int                       `json:"version"`
	Comment string                    `json:"_comment,omitempty"`
	Devices map[string]DeviceSettings `json:"devices"` // by lowercase address
}

const settingsComment = "Device settings (configuration registers) last read from the devices, written by hmbridge " +
	"when they change. Deleting this file only makes hmbridge read the settings again when needed."

// ErrNoSettingsFile is returned when there is no state directory.
var ErrNoSettingsFile = errors.New("no settings file (state_dir is \"\")")

// SettingsPath returns the location of settings.json ("" = disabled).
func (s *Store) SettingsPath() string {
	if s.settings == nil {
		return ""
	}
	return s.settings.Path()
}

// LoadSettings reads the settings cache. A missing file yields none; an
// unusable one is moved aside and reported (see File.Load).
func (s *Store) LoadSettings() (map[string]DeviceSettings, error) {
	if s.settings == nil {
		return nil, nil
	}
	var f settingsFile
	if _, err := s.settings.Load(&f); err != nil {
		return nil, err
	}
	return f.Devices, nil
}

// SaveSettings writes the settings cache; unchanged content is not
// rewritten.
func (s *Store) SaveSettings(devs map[string]DeviceSettings) error {
	if s.settings == nil {
		return ErrNoSettingsFile
	}
	if devs == nil {
		devs = map[string]DeviceSettings{}
	}
	for addr, d := range devs {
		sort.Slice(d.Lists, func(i, j int) bool {
			if d.Lists[i].Channel != d.Lists[j].Channel {
				return d.Lists[i].Channel < d.Lists[j].Channel
			}
			return d.Lists[i].List < d.Lists[j].List
		})
		devs[addr] = d
	}
	_, err := s.settings.Save(settingsFile{Version: SettingsVersion, Comment: settingsComment, Devices: devs})
	return err
}
