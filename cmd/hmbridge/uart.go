// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/gpio"
	"github.com/fjaeckel/hmgo/internal/serial"
	"github.com/fjaeckel/hmgo/internal/uartgw"
)

// probeKindTimeout is how long openUART waits for an answer to the
// IDENTIFY sent to choose the reset line (see resetPin).
const probeKindTimeout = time.Second

// openUART opens and configures the serial port and resets the module
// as configured. It returns a description of the reset (for hmbridge
// probe).
func openUART(cfg *config.Config, logf func(string, ...any)) (*os.File, string, error) {
	logf("opening serial port %s", cfg.SerialPort)
	uart, err := os.OpenFile(cfg.SerialPort, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, "", err
	}
	// Use SyscallConn instead of Fd: Fd would switch the file to
	// blocking mode, after which Close could not interrupt a pending read.
	rc, err := uart.SyscallConn()
	if err != nil {
		uart.Close()
		return nil, "", err
	}
	var (
		ctlErr error
		isTTY  bool
	)
	if err := rc.Control(func(fd uintptr) {
		if isTTY = serial.IsTerminal(fd); isTTY {
			if err := serial.Configure(fd); err != nil {
				ctlErr = fmt.Errorf("configuring %s: %w", cfg.SerialPort, err)
			}
		} else {
			logf("%s is not a tty (e.g. OpenCCU's raw-uart), skipping line configuration", cfg.SerialPort)
		}
	}); err != nil {
		ctlErr = err
	}
	if ctlErr != nil {
		uart.Close()
		return nil, "", ctlErr
	}

	var desc string
	switch cfg.Reset {
	case "gpio":
		pin, why := resetPin(cfg, uart, logf)
		var chip string
		if err := rc.Control(func(fd uintptr) {
			chip, ctlErr = gpio.Reset(fd, pin)
		}); err != nil {
			ctlErr = err
		}
		if ctlErr != nil {
			ctlErr = fmt.Errorf("resetting the radio module via GPIO %d: %w (set \"reset\": \"none\" if the module's reset line is not connected)", pin, ctlErr)
			break
		}
		desc = fmt.Sprintf("GPIO %d on %s (%s)", pin, chip, why)
		logf("reset the radio module via %s", desc)
	case "raw-uart":
		logf("resetting the radio module via the raw-uart driver")
		if err := rc.Control(func(fd uintptr) { ctlErr = serial.ResetRawUART(fd) }); err != nil {
			ctlErr = err
		}
		if ctlErr != nil {
			hint := ""
			if errors.Is(ctlErr, syscall.EBUSY) {
				hint = " (another program uses the radio module; on OpenCCU stop multimacd first, see docs/DEPLOY.md)"
			}
			ctlErr = fmt.Errorf("resetting the radio module via %s: %w%s", cfg.SerialPort, ctlErr, hint)
			break
		}
		desc = "raw-uart driver"
	default:
		desc = "none (\"reset\": \"none\")"
	}
	if ctlErr != nil {
		uart.Close()
		return nil, "", ctlErr
	}
	return uart, desc, nil
}

// resetPin returns the GPIO of the module's reset line and why it was
// chosen. With "radio": "auto" and no reset_gpio, the RPI-RF-MOD (GPIO
// 19) is recognized by its RTC (if the RTC overlay is loaded) or by its
// answer to IDENTIFY; otherwise the HM-MOD-RPI-PCB's line (GPIO 18) is
// used. A wrong guess only means the module is not reset: detection
// after the reset copes with a running module.
func resetPin(cfg *config.Config, uart *os.File, logf func(string, ...any)) (int, string) {
	if !cfg.ResetGPIOAuto() {
		switch {
		case cfg.ResetGPIO == gpio.ResetPinRPIRFMod && cfg.Radio == config.RadioRPIRFMod:
			return cfg.ResetGPIO, "RPI-RF-MOD reset line"
		case cfg.ResetGPIO == gpio.ResetPinHMModRPIPCB && cfg.Radio == config.RadioHMModRPIPCB:
			return cfg.ResetGPIO, "HM-MOD-RPI-PCB reset line"
		}
		return cfg.ResetGPIO, "reset_gpio"
	}
	if gpio.RPIRFModRTCPresent() {
		return gpio.ResetPinRPIRFMod, "RPI-RF-MOD reset line: its RTC is present"
	}
	kind, err := uartgw.ProbeKind(uart, probeKindTimeout)
	switch {
	case err != nil:
		logf("asking the radio module which kind it is: %v", err)
	case kind == uartgw.KindRPIRFMod:
		return gpio.ResetPinRPIRFMod, "RPI-RF-MOD reset line: the module answered IDENTIFY"
	case kind == uartgw.KindHMModRPIPCB:
		return gpio.ResetPinHMModRPIPCB, "HM-MOD-RPI-PCB reset line: the module announced itself"
	}
	return gpio.ResetPinHMModRPIPCB, "HM-MOD-RPI-PCB reset line: no RPI-RF-MOD found"
}
