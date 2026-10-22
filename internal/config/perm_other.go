// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

//go:build !unix

package config

import (
	"fmt"
	"io/fs"
	"os"
)

func permissionWarnings(string, fs.FileInfo) []string { return nil }

func keyFilePermissionWarnings(string, fs.FileInfo) []string { return nil }

func openKeyFile(path string) (*os.File, fs.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}
