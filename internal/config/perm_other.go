// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

//go:build !unix

package config

import "io/fs"

func permissionWarnings(string, fs.FileInfo) []string { return nil }

func keyFilePermissionWarnings(string, fs.FileInfo) []string { return nil }
