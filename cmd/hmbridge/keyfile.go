// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/store"
)

// keyFileOf returns security.key_file of a configuration ("" if none).
// Only the current key is considered: a missing security.previous key
// file is never created.
func keyFileOf(b []byte) string {
	var raw struct {
		Security *struct {
			KeyFile string `json:"key_file"`
		} `json:"security"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Security == nil {
		return ""
	}
	return raw.Security.KeyFile
}

// parseWithPlaceholderKey parses the configuration b with
// security.key_file replaced by a temporary file holding a placeholder
// key: everything but the key itself is validated.
func parseWithPlaceholderKey(b []byte) (*config.Config, error) {
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return config.Parse(b) // reports the JSON error with its position
	}
	sec, _ := doc["security"].(map[string]any)
	if sec == nil {
		return config.Parse(b)
	}
	dir, err := os.MkdirTemp("", "hmbridge-key-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	placeholder := filepath.Join(dir, KeyFileName)
	if err := os.WriteFile(placeholder, []byte("00000000000000000000000000000000\n"), 0o600); err != nil {
		return nil, err
	}
	sec["key_file"] = placeholder
	nb, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return config.Parse(nb)
}

// loadConfig loads the configuration at path. With create, a missing
// security.key_file is created with a new random AES system key first
// (for appliance images such as gokrazy, whose configuration is built
// into a read-only image while the key must be generated on the device
// and kept on its writable partition). It refuses when devices are
// configured or paired already: those know the old key, and a new one
// would silently lock them out. With check, nothing is written: the
// configuration is validated with a placeholder key and the returned
// note says that the key will be created.
func loadConfig(path string, create, check bool, rnd io.Reader) (cfg *config.Config, note string, err error) {
	if !create {
		cfg, err := config.Load(path)
		return cfg, "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	keyFile := keyFileOf(b)
	if keyFile == "" {
		cfg, err := config.Load(path)
		return cfg, "", err
	}
	if !filepath.IsAbs(keyFile) {
		return nil, "", fmt.Errorf("%s: security.key_file %q must be an absolute path", path, keyFile)
	}
	if _, err := os.Lstat(keyFile); err == nil {
		cfg, err := config.Load(path)
		return cfg, "", err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}

	pcfg, err := parseWithPlaceholderKey(b)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if n := len(pcfg.Devices); n > 0 {
		return nil, "", fmt.Errorf("%s: security.key_file %s does not exist, but %d devices are configured: they use an existing key, so no new one is created (restore the key file from your backup)", path, keyFile, n)
	}
	if p := pcfg.DevicesFilePath(); p != "" {
		if _, err := os.Lstat(p); err == nil {
			return nil, "", fmt.Errorf("%s: security.key_file %s does not exist, but devices are paired (%s): they use the lost key, so no new one is created (restore the key file from your backup)", path, keyFile, p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", err
		}
	}
	if check {
		return pcfg, fmt.Sprintf("AES system key: %s does not exist yet; it will be created with a new random key at the first start (-create_key_file)", keyFile), nil
	}

	var key [16]byte
	if _, err := io.ReadFull(rnd, key[:]); err != nil {
		return nil, "", fmt.Errorf("generating the system key: %w", err)
	}
	keyHex := []byte(hex.EncodeToString(key[:]) + "\n")
	defer clear(keyHex)
	if err := mkdirOwned(filepath.Dir(keyFile), store.DirMode, -1, -1); err != nil {
		return nil, "", err
	}
	if err := store.WriteFileNew(keyFile, keyHex); err != nil {
		return nil, "", fmt.Errorf("writing the key file: %w", err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		return nil, "", err
	}
	return cfg, fmt.Sprintf("created %s: a new random AES system key, index 1 (not shown; back this file up: without it, every AES device must be factory-reset and paired again)", keyFile), nil
}
