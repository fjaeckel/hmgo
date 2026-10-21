// Package gpio configures and toggles GPIO pins using /sys/class/gpio.
package gpio

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	GPIOHANDLE_REQUEST_OUTPUT        = 0x2
	GPIO_GET_LINEHANDLE_IOCTL        = 0xc16cb403
	GPIOHANDLE_SET_LINE_VALUES_IOCTL = 0xc040b409
)

type gpiohandlerequest struct {
	Lineoffsets   [64]uint32
	Flags         uint32
	DefaultValues [64]uint8
	ConsumerLabel [32]byte
	Lines         uint32
	Fd            uintptr
}

type gpiohandledata struct {
	Values [64]uint8
}

// ResetUARTGW resets the UARTGW whose reset pin is connected to GPIO
// line pin of /dev/gpiochip0 (18 on the HM-MOD-RPI-PCB) by holding the
// pin low for 150ms, flushing the pending UART data, then setting the
// pin high again.
func ResetUARTGW(uartfd uintptr, pin int) error {
	f, err := os.Open("/dev/gpiochip0")
	if err != nil {
		return err
	}
	defer f.Close()

	handlereq := gpiohandlerequest{
		Lineoffsets:   [64]uint32{uint32(pin)},
		Flags:         GPIOHANDLE_REQUEST_OUTPUT,
		DefaultValues: [64]uint8{1},
		ConsumerLabel: [32]byte{'h', 'm', 'g', 'o'},
		Lines:         1,
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(f.Fd()), GPIO_GET_LINEHANDLE_IOCTL, uintptr(unsafe.Pointer(&handlereq))); errno != 0 {
		return fmt.Errorf("GPIO_GET_LINEHANDLE_IOCTL: %v", errno)
	}
	// The line handle is intentionally kept open for the lifetime of
	// the process: releasing it could let the reset line float.

	// Turn off device
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(handlereq.Fd), GPIOHANDLE_SET_LINE_VALUES_IOCTL, uintptr(unsafe.Pointer(&gpiohandledata{
		Values: [64]uint8{0},
	}))); errno != 0 {
		return fmt.Errorf("GPIOHANDLE_SET_LINE_VALUES_IOCTL: %v", errno)
	}
	time.Sleep(150 * time.Millisecond)

	// Flush all data in the input buffer
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uartfd, unix.TCFLSH, uintptr(syscall.TCIFLUSH)); err != 0 {
		return fmt.Errorf("TCFLSH: %v", err)
	}

	// Turn on device
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(handlereq.Fd), GPIOHANDLE_SET_LINE_VALUES_IOCTL, uintptr(unsafe.Pointer(&gpiohandledata{
		Values: [64]uint8{1},
	}))); errno != 0 {
		return fmt.Errorf("GPIOHANDLE_SET_LINE_VALUES_IOCTL: %v", errno)
	}

	return nil
}
