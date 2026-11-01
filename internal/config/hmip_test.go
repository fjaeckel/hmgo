// SPDX-License-Identifier: BSD-3-Clause

package config

import (
	"strings"
	"testing"
	"time"
)

func withHMIP(hmip string) string {
	return `{"central_address":"1A2B3C","devices":[{"name":"a","model":"HM-CC-RT-DN","address":"38F59C"}],"hmip":` + hmip + `}`
}

func TestHMIP(t *testing.T) {
	cfg, err := Parse([]byte(withHMIP(`{"address":"B3E83A","devices":[
		{"name":"Balcony","model":"hmip-stho","address":"20A4CE"},
		{"name":"Garden","model":"HmIP-STHO","address":"20A4CF","stale_after":"0s"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.HMIP
	if h.Addr() != [3]byte{0xb3, 0xe8, 0x3a} || len(h.Devices) != 2 {
		t.Fatalf("hmip %+v", h)
	}
	d := h.Devices[0]
	if d.Model != ModelSTHO || d.Addr() != [3]byte{0x20, 0xa4, 0xce} || d.AddrHex() != "20a4ce" || d.StaleTimeout() != time.Hour {
		t.Errorf("device %+v, stale %v", d, d.StaleTimeout())
	}
	if h.Devices[1].StaleTimeout() != 0 {
		t.Errorf("stale_after 0s: %v", h.Devices[1].StaleTimeout())
	}

	if cfg, err := Parse([]byte(minimal)); err != nil || cfg.HMIP != nil {
		t.Errorf("without hmip: %+v, %v", cfg.HMIP, err)
	}
}

func TestHMIPErrors(t *testing.T) {
	for _, c := range []struct{ name, hmip, want string }{
		{"no address", `{"devices":[{"name":"b","model":"HmIP-STHO","address":"20A4CE"}]}`, "hmip: address is required"},
		{"bad address", `{"address":"B3E8","devices":[{"name":"b","model":"HmIP-STHO","address":"20A4CE"}]}`, "hmip: address"},
		{"no devices", `{"address":"B3E83A"}`, "hmip.devices: at least one device"},
		{"model", `{"address":"B3E83A","devices":[{"name":"b","model":"HmIP-SWDO","address":"20A4CE"}]}`, `unsupported model "HmIP-SWDO"`},
		{"no name", `{"address":"B3E83A","devices":[{"model":"HmIP-STHO","address":"20A4CE"}]}`, "name is required"},
		{"name of a BidCoS device", `{"address":"B3E83A","devices":[{"name":"A","model":"HmIP-STHO","address":"20A4CE"}]}`, "duplicate name"},
		{"duplicate name", `{"address":"B3E83A","devices":[{"name":"b","model":"HmIP-STHO","address":"20A4CE"},{"name":"b","model":"HmIP-STHO","address":"20A4CF"}]}`, "duplicate name"},
		{"duplicate address", `{"address":"B3E83A","devices":[{"name":"b","model":"HmIP-STHO","address":"20A4CE"},{"name":"c","model":"HmIP-STHO","address":"20a4ce"}]}`, "duplicate address"},
		// Home Assistant topics are named by address.
		{"address of a BidCoS device", `{"address":"B3E83A","devices":[{"name":"b","model":"HmIP-STHO","address":"38F59C"}]}`, "duplicate address"},
		{"own address", `{"address":"B3E83A","devices":[{"name":"b","model":"HmIP-STHO","address":"B3E83A"}]}`, "equals hmip.address"},
		{"stale_after", `{"address":"B3E83A","devices":[{"name":"b","model":"HmIP-STHO","address":"20A4CE","stale_after":"10s"}]}`, "stale_after"},
	} {
		_, err := Parse([]byte(withHMIP(c.hmip)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}
