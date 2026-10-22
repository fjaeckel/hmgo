// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package gpio

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestChipinfoIoctl(t *testing.T) {
	if got := unsafe.Sizeof(gpiochipInfo{}); got != 68 {
		t.Errorf("sizeof(gpiochip_info) = %d, want 68", got)
	}
	// #define GPIO_GET_CHIPINFO_IOCTL _IOR(0xB4, 0x01, struct gpiochip_info)
	// _IOR: dir _IOC_READ (2) << 30.
	want := uintptr(2)<<30 | unsafe.Sizeof(gpiochipInfo{})<<16 | 0xB4<<8 | 0x01
	if gpioGetChipinfoIoctl != want {
		t.Errorf("GPIO_GET_CHIPINFO_IOCTL = %#x, want %#x", gpioGetChipinfoIoctl, want)
	}
}

func TestChooseChip(t *testing.T) {
	pi4 := []chipCandidate{
		{"/dev/gpiochip0", "pinctrl-bcm2711", 58},
		{"/dev/gpiochip1", "raspberrypi-exp-gpio", 8},
	}
	// Pi 5 on a 6.1 kernel: the header is on gpiochip4.
	pi5 := []chipCandidate{
		{"/dev/gpiochip0", "gpio-brcmstb@107d508500", 32},
		{"/dev/gpiochip1", "gpio-brcmstb@107d508520", 4},
		{"/dev/gpiochip4", "pinctrl-rp1", 54},
	}
	for _, tt := range []struct {
		name  string
		cands []chipCandidate
		pin   int
		want  string
	}{
		{"pi4 rpi-rf-mod", pi4, 19, "/dev/gpiochip0"},
		{"pi5", pi5, 19, "/dev/gpiochip4"},
		{"unknown board", []chipCandidate{{"/dev/gpiochip3", "foo", 64}}, 18, DefaultChip},
		{"none", nil, 18, DefaultChip},
		{"pin out of range", []chipCandidate{{"/dev/gpiochip2", "pinctrl-bcm2835", 10}}, 18, DefaultChip},
	} {
		if got := chooseChip(tt.cands, tt.pin); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestRPIRFModRTCPresent(t *testing.T) {
	old := sysfsRoot
	t.Cleanup(func() { sysfsRoot = old })
	sysfsRoot = t.TempDir()
	if RPIRFModRTCPresent() {
		t.Error("RTC found in an empty sysfs")
	}
	write := func(dev, name string) {
		d := filepath.Join(sysfsRoot, "bus/i2c/devices", dev)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "name"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("1-0068", "ds3231")
	write("1-0032", "pcf8563")
	if RPIRFModRTCPresent() {
		t.Error("other RTCs taken for the RPI-RF-MOD's")
	}
	write("1-0032", "rx8130")
	if !RPIRFModRTCPresent() {
		t.Error("rx8130 not found")
	}
}
