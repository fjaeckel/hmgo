// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// writeKeyConfig writes a configuration with security.key_file in
// state/ and returns the paths of the configuration and the key file.
func writeKeyConfig(t *testing.T, devices string) (cfgPath, keyFile, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	stateDir = filepath.Join(dir, "perm", "hmbridge")
	keyFile = filepath.Join(stateDir, KeyFileName)
	cfgPath = filepath.Join(dir, "config.json")
	conf := fmt.Sprintf(`{
	  "central_address": "1A2B3C",
	  "security": {"key_file": %q, "index": 1},
	  "mqtt": {"broker": ""},
	  "state_dir": %q,
	  "devices": [%s]
	}`, keyFile, stateDir, devices)
	if err := os.WriteFile(cfgPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, keyFile, stateDir
}

var keyHexRe = regexp.MustCompile(`^[0-9a-f]{32}\n$`)

func TestLoadConfigCreatesKeyFile(t *testing.T) {
	cfgPath, keyFile, _ := writeKeyConfig(t, "")
	rnd := bytes.NewReader(bytes.Repeat([]byte{0xab}, 16))
	cfg, note, err := loadConfig(cfgPath, true, false, rnd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "created "+keyFile) {
		t.Errorf("note = %q", note)
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !keyHexRe.Match(b) || string(b) != strings.Repeat("ab", 16)+"\n" {
		t.Errorf("key file = %q", b)
	}
	fi, _ := os.Stat(keyFile)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(keyFile))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("key directory mode = %v, want 0700", di.Mode().Perm())
	}
	if cur, _ := cfg.Keys(); cur == nil || cur.Index != 1 {
		t.Errorf("key = %v", cur)
	}

	// The next start keeps the key.
	_, note, err = loadConfig(cfgPath, true, false, bytes.NewReader(bytes.Repeat([]byte{0xcd}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Errorf("second start: note = %q, want none", note)
	}
	if b2, _ := os.ReadFile(keyFile); !bytes.Equal(b, b2) {
		t.Errorf("key file changed on the second start")
	}
}

func TestLoadConfigCheckWritesNothing(t *testing.T) {
	cfgPath, keyFile, stateDir := writeKeyConfig(t, "")
	cfg, note, err := loadConfig(cfgPath, true, true, bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || !strings.Contains(note, "will be created") {
		t.Errorf("cfg = %v, note = %q", cfg, note)
	}
	if _, err := os.Lstat(keyFile); err == nil {
		t.Errorf("-check_config created %s", keyFile)
	}
	if _, err := os.Lstat(stateDir); err == nil {
		t.Errorf("-check_config created %s", stateDir)
	}
}

func TestLoadConfigWithoutCreate(t *testing.T) {
	cfgPath, keyFile, _ := writeKeyConfig(t, "")
	if _, _, err := loadConfig(cfgPath, false, false, bytes.NewReader(make([]byte, 16))); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v, want key file does not exist", err)
	}
	if _, err := os.Lstat(keyFile); err == nil {
		t.Errorf("created %s without -create_key_file", keyFile)
	}
}

func TestLoadConfigRefusesNewKeyForPairedDevices(t *testing.T) {
	cfgPath, keyFile, stateDir := writeKeyConfig(t, "")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "devices.json"), []byte(`{"version":1,"devices":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadConfig(cfgPath, true, false, bytes.NewReader(make([]byte, 16)))
	if err == nil || !strings.Contains(err.Error(), "devices are paired") {
		t.Errorf("err = %v, want refusal (devices are paired)", err)
	}
	if _, err := os.Lstat(keyFile); err == nil {
		t.Errorf("created %s although devices are paired", keyFile)
	}
}

func TestLoadConfigRefusesNewKeyForConfiguredDevices(t *testing.T) {
	cfgPath, keyFile, _ := writeKeyConfig(t, `{"name": "Bad", "model": "HM-CC-RT-DN", "address": "38F59C"}`)
	_, _, err := loadConfig(cfgPath, true, false, bytes.NewReader(make([]byte, 16)))
	if err == nil || !strings.Contains(err.Error(), "1 devices are configured") {
		t.Errorf("err = %v, want refusal (devices configured)", err)
	}
	if _, err := os.Lstat(keyFile); err == nil {
		t.Errorf("created %s although devices are configured", keyFile)
	}
}

func TestLoadConfigInvalidConfigWithMissingKey(t *testing.T) {
	cfgPath, keyFile, _ := writeKeyConfig(t, "")
	b, _ := os.ReadFile(cfgPath)
	os.WriteFile(cfgPath, bytes.Replace(b, []byte(`"1A2B3C"`), []byte(`"XYZ"`), 1), 0o600)
	if _, _, err := loadConfig(cfgPath, true, false, bytes.NewReader(make([]byte, 16))); err == nil || !strings.Contains(err.Error(), "central_address") {
		t.Errorf("err = %v, want central_address error", err)
	}
	if _, err := os.Lstat(keyFile); err == nil {
		t.Errorf("created %s for an invalid configuration", keyFile)
	}
}
