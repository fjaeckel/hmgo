// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileNew(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	p := filepath.Join(dir, "system.key")
	if err := WriteFileNew(p, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != FileMode {
		t.Errorf("mode %v", fi.Mode())
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != DirMode {
		t.Errorf("dir mode %v", di.Mode())
	}
	if err := WriteFileNew(p, []byte("second\n")); !errors.Is(err, fs.ErrExist) {
		t.Errorf("overwrite: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "first\n" {
		t.Errorf("content %q", b)
	}
	// A dangling symbolic link counts as existing.
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNew(link, []byte("x")); !errors.Is(err, fs.ErrExist) {
		t.Errorf("symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nowhere")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("followed the link")
	}
	// No temporary files are left behind.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Errorf("entries %v", ents)
	}
}
