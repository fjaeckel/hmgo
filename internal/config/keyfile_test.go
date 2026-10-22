// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fjaeckel/hmgo/internal/hmkey"
)

const keyFileHex = "00112233445566778899aabbccddeeff"

func writeKeyFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hmbridge.key")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask
		t.Fatal(err)
	}
	return p
}

func TestKeyFile(t *testing.T) {
	p := writeKeyFile(t, keyFileHex+"\n", 0o600)
	cfg, err := Parse([]byte(fmt.Sprintf(`{"central_address":"1A2B3C","security":{"key_file":%q,"index":1}}`, p)))
	if err != nil {
		t.Fatal(err)
	}
	cur, prev := cfg.Keys()
	want, _ := hmkey.FromHex(keyFileHex, 1)
	if *cur != want || prev != nil {
		t.Errorf("keys %v %v", cur.Index, prev)
	}
	if w := cfg.PermissionWarnings(); len(w) != 0 {
		t.Errorf("warnings %q", w)
	}
	if cfg.PairingAES() != PairingAESSystemKey {
		t.Errorf("pairing AES %q", cfg.PairingAES())
	}

	// Previous key from a file, too.
	p2 := writeKeyFile(t, "  ffeeddccbbaa99887766554433221100 \n", 0o600)
	cfg, err = Parse([]byte(fmt.Sprintf(`{"central_address":"1A2B3C","security":{"key_file":%q,"index":2,"previous":{"key_file":%q}}}`, p, p2)))
	if err != nil {
		t.Fatal(err)
	}
	if _, prev := cfg.Keys(); prev == nil || prev.Index != 1 || prev.Bytes[0] != 0xff {
		t.Errorf("previous %v", prev)
	}
}

func TestKeyFileErrorsDoNotLeakKey(t *testing.T) {
	for _, content := range []string{keyFileHex + "00", "zz" + keyFileHex[2:], keyFileHex[:30], strings.Repeat("a", 300)} {
		p := writeKeyFile(t, content, 0o600)
		_, err := Parse([]byte(fmt.Sprintf(`{"central_address":"1A2B3C","security":{"key_file":%q}}`, p)))
		if err == nil {
			t.Errorf("%q accepted", content)
			continue
		}
		if strings.Contains(err.Error(), content) || strings.Contains(err.Error(), keyFileHex[2:]) {
			t.Errorf("error leaks the key: %v", err)
		}
	}
	dir := t.TempDir()
	if _, err := Parse([]byte(fmt.Sprintf(`{"central_address":"1A2B3C","security":{"key_file":%q}}`, dir))); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: %v", err)
	}
}

func TestKeyFilePermissionWarnings(t *testing.T) {
	p := writeKeyFile(t, keyFileHex, 0o640)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"central_address":"1A2B3C","security":{"key_file":%q}}`, p)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.PermissionWarnings()
	if len(w) != 1 || !strings.Contains(w[0], p) || !strings.Contains(w[0], "mode 0640") {
		t.Errorf("warnings %q", w)
	}

	for _, tt := range []struct {
		perm      os.FileMode
		uid, euid int
		n         int
	}{
		{0o600, 1000, 1000, 0},
		{0o400, 0, 1000, 0},
		{0o600, 1000, 0, 0}, // root checking hmbridge's file
		{0o604, 1000, 1000, 1},
		{0o620, 1000, 1000, 1},
		{0o600, 1001, 1000, 1},
	} {
		if got := checkKeyFilePermissions("k", tt.perm, tt.uid, tt.euid); len(got) != tt.n {
			t.Errorf("%04o uid %d euid %d: %q", tt.perm, tt.uid, tt.euid, got)
		}
	}
}

func TestResetGPIODefaults(t *testing.T) {
	for _, tt := range []struct {
		extra string
		pin   int
		auto  bool
	}{
		{``, 18, true},
		{`,"radio":"hm-mod-rpi-pcb"`, 18, false},
		{`,"radio":"rpi-rf-mod"`, 19, false},
		{`,"reset_gpio":23`, 23, false},
		{`,"radio":"rpi-rf-mod","reset_gpio":18`, 18, false},
	} {
		c, err := Parse([]byte(`{"central_address":"1A2B3C"` + tt.extra + `}`))
		if err != nil {
			t.Fatalf("%s: %v", tt.extra, err)
		}
		if c.ResetGPIO != tt.pin || c.ResetGPIOAuto() != tt.auto {
			t.Errorf("%s: pin %d auto %v, want %d %v", tt.extra, c.ResetGPIO, c.ResetGPIOAuto(), tt.pin, tt.auto)
		}
	}
}
