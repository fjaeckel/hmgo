// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package licenses embeds the license text so that binaries can show it
// (BSD-3-Clause requires the notice to accompany binary distributions).
package licenses

import _ "embed"

//go:embed LICENSE.txt
var text string

// Text returns the license notice for binaries.
func Text() string {
	return "hmbridge is based on hmgo (https://github.com/stapelberg/hmgo).\n\n" + text
}
