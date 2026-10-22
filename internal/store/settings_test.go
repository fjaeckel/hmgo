// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSettingsFile(t *testing.T) {
	dir := t.TempDir()
	s := Open(Options{Dir: dir})
	if got, err := s.LoadSettings(); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	devs := map[string]DeviceSettings{"38f59c": {Model: "HM-CC-RT-DN", ReadAt: at, Lists: []RegList{
		{Channel: 0, List: 7, Regs: map[byte]byte{2: 0x22, 1: 0x2a}},
		{Channel: 0, List: 0, Regs: map[byte]byte{15: 1}},
	}}}
	if err := s.SaveSettings(devs); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, SettingsFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Compact, lists sorted, registers as sorted address/value pairs.
	if !strings.Contains(string(b), `"lists":[{"channel":0,"list":0,"regs":"0f01"},{"channel":0,"list":7,"regs":"012a0222"}]`) {
		t.Errorf("file: %s", b)
	}
	fi, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)
	if err := s.SaveSettings(devs); err != nil {
		t.Fatal(err)
	}
	if fi2, _ := os.Stat(path); !fi2.ModTime().Equal(fi.ModTime()) {
		t.Error("unchanged settings rewritten")
	}
	got, err := Open(Options{Dir: dir}).LoadSettings()
	if err != nil || !reflect.DeepEqual(got, devs) {
		t.Errorf("reload: %+v %v", got, err)
	}

	// Damaged registers: the file is moved aside, startup continues.
	os.WriteFile(path, []byte(`{"version":1,"devices":{"38f59c":{"lists":[{"regs":"0"}]}}}`), 0o600)
	if _, err := Open(Options{Dir: dir}).LoadSettings(); err == nil {
		t.Error("damaged file loaded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("damaged file not moved aside")
	}

	// No state directory: no file.
	none := Open(Options{})
	if none.SettingsPath() != "" || none.SaveSettings(devs) != ErrNoSettingsFile {
		t.Error("settings file without a state directory")
	}
	if got, err := none.LoadSettings(); got != nil || err != nil {
		t.Errorf("%v %v", got, err)
	}
}
