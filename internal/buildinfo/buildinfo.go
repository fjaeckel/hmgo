// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package buildinfo renders the version of the hmbridge commands.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Version returns the module version of the running binary ("devel" for
// a plain go build), with the VCS revision appended unless the version
// already contains it (as Go's pseudo-versions do).
func Version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	return format(bi.Main.Version, bi.Settings)
}

func format(mainVersion string, settings []debug.BuildSetting) string {
	v := "devel"
	if mainVersion != "" && mainVersion != "(devel)" {
		v = mainVersion
	}
	for _, s := range settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 && !strings.Contains(v, s.Value[:12]) {
			v += "+" + s.Value[:12]
		}
	}
	return v
}
