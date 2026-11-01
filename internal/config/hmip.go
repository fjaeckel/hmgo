// SPDX-License-Identifier: BSD-3-Clause

package config

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// ModelSTHO is the Homematic IP outdoor temperature and humidity sensor.
const ModelSTHO = "HmIP-STHO"

// HMIP lists the Homematic IP devices whose reports hmbridge receives
// (see docs/HMIP.md). Nothing is sent to them or to the module's
// Homematic IP side: the module passes on their frames without any
// setup.
type HMIP struct {
	Comment Comment `json:"_comment,omitempty"`
	// Address is the radio module's own Homematic IP address, to which
	// the devices send (OpenCCU's access point: HM_HMIP_ADDRESS in
	// /var/hm_mode). Frames to other addresses are ignored.
	Address string       `json:"address"`
	Devices []HMIPDevice `json:"devices"`

	addr [3]byte
}

// HMIPDevice is a Homematic IP device.
type HMIPDevice struct {
	Comment Comment `json:"_comment,omitempty"`
	Name    string  `json:"name"`
	Model   string  `json:"model"`   // ModelSTHO
	Address string  `json:"address"` // 6 hex digits
	// StaleAfter: see Device.StaleAfter. Default 1h.
	StaleAfter Duration `json:"stale_after,omitempty"`

	addr [3]byte
}

// Addr returns the module's Homematic IP address.
func (h HMIP) Addr() [3]byte { return h.addr }

// Addr returns the device's radio address.
func (d HMIPDevice) Addr() [3]byte { return d.addr }

// AddrHex returns the address as lowercase hex, e.g. "20a4ce".
func (d HMIPDevice) AddrHex() string { return hex.EncodeToString(d.addr[:]) }

// StaleTimeout returns how long the device may be silent before it is
// reported unavailable; 0 means never.
func (d HMIPDevice) StaleTimeout() time.Duration {
	if d.StaleAfter.set {
		return d.StaleAfter.Duration
	}
	return time.Hour
}

// resolveHMIP validates c.HMIP. byName and byAddr hold the BidCoS
// devices: names and addresses must be unique across both (Home
// Assistant topics are named by address).
func (c *Config) resolveHMIP(byName map[string]int, byAddr map[[3]byte]int) error {
	h := c.HMIP
	if h == nil {
		return nil
	}
	if h.Address == "" {
		return fmt.Errorf("hmip: address is required (the module's Homematic IP address, HM_HMIP_ADDRESS in OpenCCU's /var/hm_mode)")
	}
	a, err := parseAddr(h.Address)
	if err != nil {
		return fmt.Errorf("hmip: %v", err)
	}
	h.addr = a
	if len(h.Devices) == 0 {
		return fmt.Errorf("hmip.devices: at least one device is required")
	}
	names := make(map[string]bool)
	addrs := make(map[[3]byte]bool)
	for i := range h.Devices {
		d := &h.Devices[i]
		where := fmt.Sprintf("hmip.devices[%d]", i)
		if strings.TrimSpace(d.Name) == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		where = fmt.Sprintf("hmip.devices[%d] (%q)", i, d.Name)
		if !strings.EqualFold(d.Model, ModelSTHO) {
			return fmt.Errorf("%s: unsupported model %q (supported: %s)", where, d.Model, ModelSTHO)
		}
		d.Model = ModelSTHO
		a, err := parseAddr(d.Address)
		if err != nil {
			return fmt.Errorf("%s: %v", where, err)
		}
		switch {
		case a == [3]byte{}:
			return fmt.Errorf("%s: address must not be 000000", where)
		case a == h.addr:
			return fmt.Errorf("%s: address equals hmip.address", where)
		case a == c.centralAddr:
			return fmt.Errorf("%s: address equals central_address", where)
		}
		if _, dup := byAddr[a]; dup || addrs[a] {
			return fmt.Errorf("%s: duplicate address %s", where, d.Address)
		}
		addrs[a] = true
		d.addr = a
		name := strings.ToLower(d.Name)
		if _, dup := byName[name]; dup || names[name] {
			return fmt.Errorf("%s: duplicate name", where)
		}
		names[name] = true
		if d.StaleAfter.set && d.StaleAfter.Duration != 0 && d.StaleAfter.Duration < time.Minute {
			return fmt.Errorf("%s: stale_after %v is too short (minimum 1m, or \"0s\" for never)", where, d.StaleAfter.Duration)
		}
	}
	return nil
}
