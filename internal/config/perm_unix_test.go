// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPermissionWarnings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"central_address": "1A2B3C", "devices": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[os.FileMode]string{
		0o600: "",
		0o640: "", // group: the process's own group
		0o644: "readable by every user",
		0o666: "writable by every user",
		0o660: "writable by its group",
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		w := strings.Join(cfg.PermissionWarnings(), "; ")
		if want == "" && w != "" || want != "" && !strings.Contains(w, want) {
			t.Errorf("mode %04o: warnings %q, want %q", mode, w, want)
		}
		if want != "" && !strings.Contains(w, "install -m 0640 -o root -g hmbridge") {
			t.Errorf("mode %04o: no fix in %q", mode, w)
		}
	}
	// Ownership and foreign groups (needs no root: checked directly).
	own := map[int]bool{1000: true}
	if w := checkPermissions("c", 0o640, 0, 1000, 1000, own); len(w) != 0 {
		t.Errorf("root:hmbridge 0640: %q", w)
	}
	if w := checkPermissions("c", 0o640, 0, 4, 1000, own); len(w) != 1 || !strings.Contains(w[0], "readable by group 4") {
		t.Errorf("foreign group: %q", w)
	}
	if w := checkPermissions("c", 0o600, 1001, 1000, 1000, own); len(w) != 1 || !strings.Contains(w[0], "owned by another user (uid 1001)") {
		t.Errorf("foreign owner: %q", w)
	}
	if w := checkPermissions("c", 0o600, 1000, 1000, 1000, own); len(w) != 0 {
		t.Errorf("own file: %q", w)
	}
	if cfg, err := Parse([]byte(`{"central_address": "1A2B3C", "devices": []}`)); err != nil || len(cfg.PermissionWarnings()) != 0 {
		t.Errorf("Parse: %v %v", cfg, err)
	}
}
