// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package gpio

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

// Reset lines of the radio modules on a Raspberry Pi (BCM GPIO
// numbers), from OpenCCU's device tree overlay rpi-rf-mod.dts
// (pivccu,reset-gpios = <&gpio 18 0>, <&gpio 19 0>): the raw-uart driver
// uses the second one when it finds the RPI-RF-MOD's RTC. Both are
// active low.
const (
	ResetPinHMModRPIPCB = 18
	ResetPinRPIRFMod    = 19
)

// struct gpiochip_info (68 bytes).
type gpiochipInfo struct {
	Name  [gpioMaxNameSize]byte
	Label [gpioMaxNameSize]byte
	Lines uint32
}

// GPIO_GET_CHIPINFO_IOCTL = _IOR(0xB4, 0x01, struct gpiochip_info).
const gpioGetChipinfoIoctl = 0x8044b401

// socLabels are the labels of the GPIO chips that drive the 40-pin
// header of the Raspberry Pi models (Pi 1-3 and Zero, Pi 4, Pi 5).
var socLabels = []string{"pinctrl-bcm2835", "pinctrl-bcm2711", "pinctrl-bcm2712", "pinctrl-rp1"}

// chipGlob lists the GPIO character devices (a variable for tests).
var chipGlob = "/dev/gpiochip*"

// DefaultChip is used when no chip announces itself as the header's.
const DefaultChip = "/dev/gpiochip0"

type chipCandidate struct {
	path  string
	label string
	lines uint32
}

// chooseChip returns the chip of the SoC's header GPIOs that has line
// pin, or DefaultChip. On a Pi 4 that is gpiochip0 (pinctrl-bcm2711);
// on a Pi 5 it is the RP1's chip, whose number depends on the kernel
// version.
func chooseChip(cands []chipCandidate, pin int) string {
	for _, want := range socLabels {
		for _, c := range cands {
			if c.label == want && int(c.lines) > pin {
				return c.path
			}
		}
	}
	return DefaultChip
}

// FindChip returns the GPIO character device that drives BCM GPIO pin
// of the Raspberry Pi's header.
func FindChip(pin int) string {
	paths, _ := filepath.Glob(chipGlob)
	var cands []chipCandidate
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		var info gpiochipInfo
		if ioctl(f.Fd(), gpioGetChipinfoIoctl, unsafe.Pointer(&info)) == nil {
			cands = append(cands, chipCandidate{path: p, label: cString(info.Label[:]), lines: info.Lines})
		}
		f.Close()
	}
	return chooseChip(cands, pin)
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// sysfsRoot is /sys (a variable for tests).
var sysfsRoot = "/sys"

// RPIRFModRTCPresent reports whether the RTC of an RPI-RF-MOD (Epson
// RX8130 at I²C address 0x32) is known to the kernel, i.e. the board
// carries an RPI-RF-MOD and the RTC overlay is loaded (see
// docs/DEPLOY.md). OpenCCU's raw-uart driver picks the reset line the
// same way.
func RPIRFModRTCPresent() bool {
	names, _ := filepath.Glob(filepath.Join(sysfsRoot, "bus/i2c/devices/*-0032/name"))
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(b)) {
		case "rx8130", "rx8130-legacy":
			return true
		}
	}
	return false
}
