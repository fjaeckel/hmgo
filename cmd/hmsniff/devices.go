// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import "github.com/fjaeckel/hmgo/internal/openccu"

// DeviceInfo is what OpenCCU's rfd knows about a paired device.
type DeviceInfo = openccu.Device

// loadDevices reads the *.dev files in dir, e.g. /etc/config/rfd on
// OpenCCU. Missing or unreadable files are skipped.
func loadDevices(dir string) map[[3]byte]DeviceInfo { return openccu.LoadDevices(dir) }

// loadCentral reads "BidCoS-Address = <decimal>" from OpenCCU's ids file
// (zero address if missing).
func loadCentral(path string) [3]byte {
	a, _ := openccu.LoadCentral(path)
	return a
}
