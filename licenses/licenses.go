// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: MIT

// Package licenses embeds the license texts so that binaries can show
// them (BSD-3-Clause requires the notice to accompany binary
// distributions).
package licenses

import _ "embed"

//go:embed MIT.txt
var mit string

//go:embed BSD-3-Clause-hmgo.txt
var bsd string

// Text returns the license notice for binaries.
func Text() string {
	return "hmbridge is distributed under two licenses. Code written for hmbridge:\n\n" +
		mit +
		"\n----------------------------------------------------------------------\n\n" +
		"Parts derived from hmgo (https://github.com/stapelberg/hmgo):\n\n" +
		bsd
}
