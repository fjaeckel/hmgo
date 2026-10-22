// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package devstore

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
)

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "devices.json")
	if devs, err := Load(path); err != nil || devs != nil {
		t.Fatalf("missing file: %v %v", devs, err)
	}
	paired := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	devs := []Device{
		{Name: "HM-Sec-SCo NEQ0000002", Model: "HM-Sec-SCo", Address: "4C5D6F", Serial: "NEQ0000002", AESChannels: []int{1}, KeyIndex: 2, Paired: paired},
		{Name: "Plug", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "112233", KeyIndex: 0, Paired: paired, Firmware: "2.8", ModelID: "00D8"},
	}
	if err := Save(path, devs); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{devs[1], devs[0]} // sorted by address
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	// No temporary files are left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory has %d entries", len(entries))
	}

	os.WriteFile(path, []byte(`{"version": 7, "devices": []}`), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("unknown version accepted")
	}
	os.WriteFile(path, []byte(`{`), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("broken JSON accepted")
	}
}

func TestConfigRoundTrip(t *testing.T) {
	cfg, err := config.Parse([]byte(`{"central_address":"1A2B3C","security":{"passphrase":"x","index":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	in := Device{Name: "Taster", Model: "HM-PB-2-WM55", Address: "73788E", Serial: "NEQ1234567", AESChannels: []int{1, 2}, KeyIndex: 2}
	c, err := cfg.ResolveStoreDevice(in.Config())
	if err != nil {
		t.Fatal(err)
	}
	if !c.AESEnabled() || c.KeyIndex() != 2 || !c.FromStore() {
		t.Errorf("resolved %+v", c)
	}
	out := FromConfig(c, "1.4", "006B", time.Time{})
	if out.Address != "73788E" || !reflect.DeepEqual(out.AESChannels, []int{1, 2}) || out.KeyIndex != 2 || out.Firmware != "1.4" {
		t.Errorf("FromConfig = %+v", out)
	}
	// Without AES channels: no AES.
	in.AESChannels, in.KeyIndex = nil, 0
	c, err = cfg.ResolveStoreDevice(in.Config())
	if err != nil || c.AESEnabled() || c.KeyIndex() != 0 {
		t.Errorf("no AES: %+v %v", c, err)
	}
}
