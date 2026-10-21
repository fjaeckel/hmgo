//go:build linux
// +build linux

// Package serial configures serial ports.
package serial

import (
	"syscall"
	"unsafe"
)

// Configure configures fd as a 115200 baud 8N1 serial port.
func Configure(fd uintptr) error {
	var termios syscall.Termios
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios))); err != 0 {
		return err
	}

	termios.Iflag = 0
	termios.Oflag = 0
	termios.Lflag = 0
	termios.Ispeed = syscall.B115200
	termios.Ospeed = syscall.B115200
	// CLOCAL: ignore modem control lines (no DCD on the module UART).
	termios.Cflag = syscall.B115200 | syscall.CS8 | syscall.CREAD | syscall.CLOCAL

	// Block on a zero read (instead of returning EOF)
	termios.Cc[syscall.VMIN] = 1
	termios.Cc[syscall.VTIME] = 0

	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&termios))); err != 0 {
		return err
	}

	return nil
}

// IsTerminal reports whether fd refers to a terminal (tty). The
// raw-uart character device that OpenCCU provides for the radio module
// is not a tty and needs no line configuration.
func IsTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)))
	return err == 0
}
