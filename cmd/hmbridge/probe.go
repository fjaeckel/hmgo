// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/uartgw"
)

// probeCentral is the central address set in the module during a probe
// without a configuration. The module keeps it in RAM only and nothing
// is transmitted.
const probeCentral = "000001"

// runProbe implements “hmbridge probe”: open the serial port, reset the
// radio module, identify it, print what it reports, and exit. Settings
// come from the configuration file if it exists (or -config is given)
// and from the flags, which take precedence.
func runProbe(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hmbridge probe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfgPath := flags.String("config", "/etc/hmbridge/config.json", "configuration to take serial_port, radio, reset and reset_gpio from, if it exists")
	port := flags.String("serial_port", "", "serial port of the radio module (default from the configuration, else "+config.DefaultSerialPort+")")
	radio := flags.String("radio", "", "radio module: auto, hm-mod-rpi-pcb or rpi-rf-mod (default from the configuration, else auto)")
	reset := flags.String("reset", "", "reset method: auto, gpio, raw-uart or none (default from the configuration, else auto)")
	resetGPIO := flags.Int("reset_gpio", 0, "BCM GPIO of the reset line (default: the module's, 18 or 19)")
	debug := flags.Bool("debug", false, "log every frame exchanged with the radio module")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "usage: hmbridge probe [flags]\n\nOpens the serial port, resets the radio module, identifies it and prints its kind, firmware and serial number. Stop hmbridge first (systemctl stop hmbridge).\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	*debugFrames = *debug

	settings := map[string]any{"central_address": probeCentral}
	if cfg, err := config.Load(*cfgPath); err == nil {
		fmt.Fprintf(stdout, "configuration:  %s\n", *cfgPath)
		settings["serial_port"] = cfg.SerialPort
		settings["radio"] = cfg.Radio
		settings["reset"] = cfg.Reset
		if !cfg.ResetGPIOAuto() {
			settings["reset_gpio"] = cfg.ResetGPIO
		}
		settings["central_address"] = fmt.Sprintf("%X", cfg.CentralAddr())
	} else if explicit["config"] || !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "hmbridge probe: %v\n", err)
		return 1
	}
	for name, v := range map[string]string{"serial_port": *port, "radio": *radio, "reset": *reset} {
		if v != "" {
			settings[name] = v
		}
	}
	if *resetGPIO != 0 {
		settings["reset_gpio"] = *resetGPIO
	}
	b, _ := json.Marshal(settings)
	cfg, err := config.Parse(b)
	if err != nil {
		fmt.Fprintf(stderr, "hmbridge probe: %v\n", err)
		return 2
	}

	logf := func(format string, a ...any) { log.Printf(format, a...) }
	port0 := cfg.SerialPort
	if target, err := filepath.EvalSymlinks(port0); err == nil && target != port0 {
		port0 += " -> " + target
	}
	fmt.Fprintf(stdout, "serial port:    %s\n", port0)
	uart, resetDesc, err := openUART(cfg, logf)
	if err != nil {
		fmt.Fprintf(stderr, "hmbridge probe: %v\n%s", err, probeHint(err))
		return 1
	}
	fmt.Fprintf(stdout, "reset:          %s\n", resetDesc)
	if err := probeModule(stdout, cfg, uart); err != nil {
		fmt.Fprintf(stderr, "hmbridge probe: %v\n%s", err, probeHint(err))
		return 1
	}
	return 0
}

// probeModule initializes the radio module on uart, prints what it
// reports and closes it again. uart is closed on return.
func probeModule(w io.Writer, cfg *config.Config, uart io.ReadWriteCloser) error {
	conn, err := openModule(cfg, uart, nil)
	if err != nil {
		uart.Close()
		return err
	}
	defer conn.Close()
	fmt.Fprintf(w, "module:         %s\n", conn.Kind())
	if a, ok := conn.(interface{ Application() string }); ok && a.Application() != "" {
		fmt.Fprintf(w, "application:    %s\n", a.Application())
	}
	fmt.Fprintf(w, "firmware:       %s\n", conn.FirmwareVersion())
	fmt.Fprintf(w, "serial number:  %s\n", conn.SerialNumber())
	if s, ok := conn.(interface{ SGTIN() string }); ok && s.SGTIN() != "" {
		fmt.Fprintf(w, "SGTIN:          %s\n", s.SGTIN())
	}
	if conn.Kind() == uartgw.KindHMModRPIPCB && strings.HasPrefix(conn.FirmwareVersion(), "2.") {
		fmt.Fprintf(w, "note:           firmware 2.x is eQ-3's dual firmware; hmbridge expects 1.x on the HM-MOD-RPI-PCB (see docs/DEPLOY.md, Module firmware)\n")
	}
	fmt.Fprintf(w, "OK: the radio module works. Next: hmbridge init (see docs/DEPLOY.md).\n")
	return nil
}

// probeHint explains the usual causes of a failed probe.
func probeHint(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "hint: permission denied: run as root (sudo), or as a user in the groups dialout (serial port) and gpio (reset line)\n"
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOENT):
		return "hint: no such device: is enable_uart=1 set in /boot/firmware/config.txt and the Pi rebooted? (see docs/DEPLOY.md, Fresh installation)\n"
	case errors.Is(err, syscall.EBUSY):
		return "hint: the device is busy: stop hmbridge (systemctl stop hmbridge) and anything else using the serial port (a serial console: remove console=serial0,115200 from cmdline.txt)\n"
	}
	return "hint: check that the module sits on the right pins, that Bluetooth is moved off the UART (dtoverlay=disable-bt or miniuart-bt), that no serial console runs on it (cmdline.txt), and run with -debug to see the frames\n"
}
