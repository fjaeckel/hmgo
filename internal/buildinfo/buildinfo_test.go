// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFormat(t *testing.T) {
	rev := []debug.BuildSetting{{Key: "vcs.revision", Value: "525b677abcdef0123456789"}}
	for _, tc := range []struct {
		main     string
		settings []debug.BuildSetting
		want     string
	}{
		{"", nil, "devel"},
		{"(devel)", rev, "devel+525b677abcde"},
		{"v1.2.0", rev, "v1.2.0+525b677abcde"},
		// Pseudo-versions already carry the revision: not twice.
		{"v0.0.0-20261022120000-525b677abcde", rev, "v0.0.0-20261022120000-525b677abcde"},
		{"v0.0.0-20261022120000-525b677abcde+dirty", rev, "v0.0.0-20261022120000-525b677abcde+dirty"},
		{"v1.2.0", []debug.BuildSetting{{Key: "vcs.revision", Value: "short"}}, "v1.2.0"},
	} {
		if got := format(tc.main, tc.settings); got != tc.want {
			t.Errorf("format(%q) = %q, want %q", tc.main, got, tc.want)
		}
	}
}
