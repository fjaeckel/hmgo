// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// An existing, longer, world-readable file is truncated and made
	// private.
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o644)
	if err := writeOut(path, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "{}\n" {
		t.Errorf("content %q", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	// A new file is created with 0600.
	fresh := filepath.Join(dir, "new.json")
	if err := writeOut(fresh, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o600 {
		t.Errorf("new file mode %v", fi.Mode())
	}
	// A symlink to a regular file (e.g. planted in a shared directory)
	// is not followed.
	victim := filepath.Join(dir, "victim")
	os.WriteFile(victim, []byte("keep"), 0o600)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := writeOut(link, []byte("{}")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("symlink target overwritten: %q", b)
	}
	// A dangling symlink is not created through either.
	dangling := filepath.Join(dir, "dangling.json")
	os.Symlink(filepath.Join(dir, "created-by-link"), dangling)
	if err := writeOut(dangling, []byte("{}")); err == nil {
		t.Error("dangling symlink followed")
	}
	if _, err := os.Stat(filepath.Join(dir, "created-by-link")); err == nil {
		t.Error("symlink target created")
	}
	// A symlink to a device (like /dev/stdout) is fine.
	null := filepath.Join(dir, "null")
	if err := os.Symlink("/dev/null", null); err == nil {
		if err := writeOut(null, []byte("{}")); err != nil {
			t.Errorf("symlink to /dev/null: %v", err)
		}
	}
}
