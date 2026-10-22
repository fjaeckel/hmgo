// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package openccu reads what OpenCCU (RaspberryMatic) knows about its
// BidCoS devices: rfd's device files, the ids file, /var/hm_mode, the
// rfd XML-RPC API, ReGaHss scripts and the heating groups file.
//
// It never reads key material (/etc/config/keys and the like).
package openccu

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Device is what OpenCCU's rfd knows about a paired device from its
// *.dev file.
type Device struct {
	Serial   string
	Type     string
	Addr     [3]byte
	KeyIndex string // aes_key_index attribute, "" if absent
	// ChannelAES holds per-channel AES flags found in the rest of the
	// file, if any (the format is not documented; this is best effort).
	ChannelAES map[int]bool
	// FirstLine is the <device …> line as read (for debugging dumps).
	FirstLine string
}

func (d Device) String() string { return d.Type + " " + d.Serial }

// KeyIndexInt returns the aes_key_index as an int (-1 if absent or
// invalid).
func (d Device) KeyIndexInt() int {
	n, err := strconv.Atoi(strings.TrimSpace(d.KeyIndex))
	if err != nil || n < 0 || n > 255 {
		return -1
	}
	return n
}

var attrRe = regexp.MustCompile(`([\w.:-]+)\s*=\s*"([^"]*)"`)

func attrs(s string) map[string]string {
	res := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(s, -1) {
		res[m[1]] = m[2]
	}
	return res
}

// LoadDevices reads every *.dev file in dir (e.g. /etc/config/rfd) and
// returns the devices by address. Missing or unparsable files are
// skipped.
func LoadDevices(dir string) map[[3]byte]Device {
	res := make(map[[3]byte]Device)
	files, _ := filepath.Glob(filepath.Join(dir, "*.dev"))
	sort.Strings(files)
	for _, path := range files {
		if d, ok := ReadDeviceFile(path); ok {
			res[d.Addr] = d
		}
	}
	return res
}

// ReadDeviceFile parses one rfd *.dev file.
func ReadDeviceFile(path string) (Device, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Device{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	if !sc.Scan() {
		return Device{}, false
	}
	d, ok := ParseDeviceLine(sc.Text())
	if !ok {
		return d, false
	}
	var rest strings.Builder
	for sc.Scan() {
		rest.WriteString(sc.Text())
		rest.WriteByte('\n')
	}
	d.ChannelAES = ParseChannelAES(rest.String())
	return d, true
}

// ParseDeviceLine parses the first line of a *.dev file, e.g.
// <device serial="NEQ0882326" type="HM-CC-RT-DN" address="0x4DA5A9" aes_key_index="2" …>.
func ParseDeviceLine(line string) (Device, bool) {
	d := Device{FirstLine: strings.TrimSpace(line)}
	a := attrs(line)
	n, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(a["address"]), "0x"), 16, 32)
	if err != nil || n == 0 || n > 0xFFFFFF {
		return d, false
	}
	d.Addr = [3]byte{byte(n >> 16), byte(n >> 8), byte(n)}
	d.Serial = a["serial"]
	d.Type = a["type"]
	d.KeyIndex = a["aes_key_index"]
	return d, true
}

var channelTagRe = regexp.MustCompile(`(?i)<\s*([\w:-]*channel[\w:-]*)\b([^>]*)>`)

// ParseChannelAES looks for channel elements with an index and an
// AES-like attribute (e.g. <channel index="1" aes_active="true"/>) and
// returns the flags by channel number. The rfd format is undocumented,
// so this is deliberately lenient; nil means nothing was found.
func ParseChannelAES(s string) map[int]bool {
	var res map[int]bool
	for _, m := range channelTagRe.FindAllStringSubmatch(s, -1) {
		a := attrs(m[2])
		idx := -1
		for k, v := range a {
			switch strings.ToLower(k) {
			case "index", "number", "no", "channel", "nr":
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n < 256 {
					idx = n
				}
			}
		}
		if idx < 0 {
			continue
		}
		for k, v := range a {
			lk := strings.ToLower(k)
			if !strings.Contains(lk, "aes") || strings.Contains(lk, "key") || strings.Contains(lk, "index") {
				continue
			}
			on, ok := parseBool(v)
			if !ok {
				continue
			}
			if res == nil {
				res = map[int]bool{}
			}
			res[idx] = on
		}
	}
	return res
}

func parseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "on", "yes":
		return true, true
	case "0", "false", "off", "no":
		return false, true
	}
	return false, false
}

// LoadCentral reads "BidCoS-Address = <decimal or 0x hex>" from
// OpenCCU's ids file (/etc/config/ids). ok is false if the file or the
// entry is missing.
func LoadCentral(path string) (addr [3]byte, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return addr, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(k) != "BidCoS-Address" {
			continue
		}
		v = strings.TrimSpace(v)
		base := 10
		if strings.HasPrefix(strings.ToLower(v), "0x") {
			v, base = v[2:], 16
		}
		n, err := strconv.ParseUint(v, base, 32)
		if err == nil && n > 0 && n <= 0xFFFFFF {
			return [3]byte{byte(n >> 16), byte(n >> 8), byte(n)}, true
		}
	}
	return addr, false
}

// ReadHMMode parses /var/hm_mode (shell assignments such as
// HM_HMRF_DEV='RPI-RF-MOD') into a map. A missing file yields nil.
func ReadHMMode(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	res := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.Trim(strings.TrimSpace(v), `'"`)
		res[k] = v
	}
	return res
}
