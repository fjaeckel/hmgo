// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// maxKeyFileSize bounds what is read from a key file (32 hex characters
// and a newline; some room for whitespace).
const maxKeyFileSize = 256

// readKeyFile reads an AES key file: 32 hex characters, surrounding
// whitespace ignored. It returns the key's hex form (validated by
// hmkey.FromHex later) and warnings about the file's permissions. The
// key itself never appears in an error.
func readKeyFile(path string) (string, []string, error) {
	if !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("%q must be an absolute path", path)
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("%s does not exist (hmbridge init creates it)", path)
		}
		return "", nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	if !fi.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(b) > maxKeyFileSize {
		return "", nil, fmt.Errorf("%s is too large for a key file", path)
	}
	key := string(bytes.TrimSpace(b))
	if len(key) != 32 {
		return "", nil, fmt.Errorf("%s must contain the key as 32 hex characters (16 bytes)", path)
	}
	return key, keyFilePermissionWarnings(path, fi), nil
}
