// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/gpio"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

// ptyModule connects an emulated radio module to a pseudo terminal and
// returns the path of its slave side: hmbridge opens it like the Pi's
// UART (a tty, termios, non-blocking reads with deadlines).
func ptyModule(t *testing.T, opts fakemod.Options) (string, *fakemod.Module) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo terminals: %v", err)
	}
	var unlock int32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatal(errno)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	// Raw mode on the master side too until hmbridge configures the
	// slave: no echo of what the module writes.
	tio, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	tio.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	tio.Iflag = 0
	tio.Oflag = 0
	if err := unix.IoctlSetTermios(int(master.Fd()), unix.TCSETS, tio); err != nil {
		t.Fatal(err)
	}
	mod, rw := fakemod.New(opts)
	go io.Copy(rw, master)
	go io.Copy(master, rw)
	t.Cleanup(func() {
		master.Close()
		rw.Close()
		mod.Close()
	})
	return fmt.Sprintf("/dev/pts/%d", n), mod
}

func probeConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`{"central_address":"000001","require_synced_clock":false` + extra + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The serial port path without OpenCCU: a tty, configured with
// termios; the module announces itself after a reset.
func TestProbeOverPTY(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts fakemod.Options
		want []string
	}{
		{"rpi-rf-mod after reset", fakemod.Options{DualCoPro: true}, []string{"module:         RPI-RF-MOD", "application:    DualCoPro_App", "firmware:       4.4.22", "serial number:  FAKE000001", "SGTIN:          3014F711A0001F5A4993DA3A", "OK"}},
		{"rpi-rf-mod running", fakemod.Options{DualCoPro: true, StartInApp: true}, []string{"module:         RPI-RF-MOD", "OK"}},
		{"hm-mod-rpi-pcb after reset", fakemod.Options{}, []string{"module:         HM-MOD-RPI-PCB", "firmware:       1.4.1", "OK"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			port, _ := ptyModule(t, tt.opts)
			cfg := probeConfig(t, fmt.Sprintf(`,"serial_port":%q,"reset":"none"`, port))
			uart, desc, err := openUART(cfg, t.Logf)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(desc, "none") {
				t.Errorf("reset %q", desc)
			}
			var out bytes.Buffer
			if err := probeModule(&out, cfg, uart); err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
		})
	}
}

// With "radio": "auto" and no reset_gpio, the reset line is chosen by
// asking the module (IDENTIFY) on the real tty before the reset.
func TestResetPinAuto(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts fakemod.Options
		pin  int
	}{
		{"rpi-rf-mod", fakemod.Options{DualCoPro: true, StartInApp: true}, gpio.ResetPinRPIRFMod},
		{"hm-mod-rpi-pcb", fakemod.Options{StartInApp: true}, gpio.ResetPinHMModRPIPCB},
		{"silent", fakemod.Options{Mute: true}, gpio.ResetPinHMModRPIPCB},
	} {
		t.Run(tt.name, func(t *testing.T) {
			port, _ := ptyModule(t, tt.opts)
			cfg := probeConfig(t, fmt.Sprintf(`,"serial_port":%q,"reset":"none"`, port))
			if !cfg.ResetGPIOAuto() {
				t.Fatal("not auto")
			}
			uart, _, err := openUART(cfg, t.Logf)
			if err != nil {
				t.Fatal(err)
			}
			defer uart.Close()
			pin, why := resetPin(cfg, uart, t.Logf)
			if pin != tt.pin {
				t.Errorf("pin %d (%s), want %d", pin, why, tt.pin)
			}
		})
	}
	// Explicit settings are not probed.
	for _, tt := range []struct {
		extra string
		pin   int
	}{
		{`,"radio":"rpi-rf-mod"`, 19},
		{`,"radio":"hm-mod-rpi-pcb"`, 18},
		{`,"reset_gpio":23`, 23},
	} {
		cfg := probeConfig(t, tt.extra)
		if pin, _ := resetPin(cfg, nil, t.Logf); pin != tt.pin {
			t.Errorf("%s: pin %d, want %d", tt.extra, pin, tt.pin)
		}
	}
}

func TestProbeCommandErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runProbe([]string{"-config", "/nonexistent/config.json"}, &out, &errOut); code != 1 {
		t.Errorf("explicit missing config: exit %d", code)
	}
	errOut.Reset()
	if code := runProbe([]string{"-config", "/nonexistent/c.json", "-radio", "cul"}, &out, &errOut); code == 0 {
		t.Errorf("exit %d", code)
	}
	errOut.Reset()
	code := runProbe([]string{"-serial_port", "/nonexistent/tty", "-reset", "none"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "enable_uart=1") {
		t.Errorf("exit %d: %s", code, errOut.String())
	}
}

// The line settings without the raw-uart driver: 115200 baud 8N1, no
// flow control (the modules have no RTS/CTS lines; OpenCCU's pl011
// raw-uart driver does not enable CTSEN/RTSEN either), raw mode.
func TestSerialLineSettings(t *testing.T) {
	port, _ := ptyModule(t, fakemod.Options{Mute: true})
	cfg := probeConfig(t, fmt.Sprintf(`,"serial_port":%q,"reset":"none"`, port))
	uart, _, err := openUART(cfg, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer uart.Close()
	rc, _ := uart.SyscallConn()
	var tio *unix.Termios
	rc.Control(func(fd uintptr) { tio, err = unix.IoctlGetTermios(int(fd), unix.TCGETS) })
	if err != nil {
		t.Fatal(err)
	}
	if tio.Cflag&unix.CBAUD != unix.B115200 || tio.Cflag&unix.CSIZE != unix.CS8 ||
		tio.Cflag&(unix.PARENB|unix.CSTOPB|unix.CRTSCTS) != 0 || tio.Cflag&(unix.CREAD|unix.CLOCAL) != unix.CREAD|unix.CLOCAL {
		t.Errorf("cflag %#o", tio.Cflag)
	}
	if tio.Iflag&(unix.IXON|unix.IXOFF|unix.ICRNL|unix.ISTRIP) != 0 || tio.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) != 0 || tio.Oflag&unix.OPOST != 0 {
		t.Errorf("not raw: iflag %#o lflag %#o oflag %#o", tio.Iflag, tio.Lflag, tio.Oflag)
	}
	if tio.Cc[unix.VMIN] != 1 || tio.Cc[unix.VTIME] != 0 {
		t.Errorf("VMIN %d VTIME %d", tio.Cc[unix.VMIN], tio.Cc[unix.VTIME])
	}
}
