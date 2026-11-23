// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package buildinfo renders the version of the hmbridge commands.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// modulePath is this repository's module.
const modulePath = "github.com/fjaeckel/hmgo"

// version, if set at link time (-ldflags "-X
// github.com/fjaeckel/hmgo/internal/buildinfo.version=v1.2.3"), is
// returned by Version as is. Used where the build information cannot
// name the version, e.g. a gokrazy image built from a local checkout
// (contrib/gokrazy).
var version string

// Version returns the module version of the running binary ("devel" for
// a plain go build), with the VCS revision appended unless the version
// already contains it (as Go's pseudo-versions do). When hmbridge is
// built as a dependency of another main module (as gokrazy does), the
// version of that dependency is used.
func Version() string {
	if version != "" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	return format(mainVersion(bi), bi.Settings)
}

// mainVersion returns the version of this module in bi: the main
// module's, or, if the main module is another one, the dependency's.
func mainVersion(bi *debug.BuildInfo) string {
	if bi.Main.Path == modulePath || bi.Main.Path == "" {
		return bi.Main.Version
	}
	for _, d := range bi.Deps {
		if d.Path != modulePath {
			continue
		}
		if d.Replace != nil {
			return d.Replace.Version // "" for a local directory
		}
		return d.Version
	}
	return bi.Main.Version
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
