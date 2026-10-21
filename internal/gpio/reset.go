// Package gpio resets the radio module via the Linux GPIO character
// device (/dev/gpiochip0).
//
// The uAPI v2 (GPIO_V2_GET_LINE_IOCTL, Linux 5.10+) is used when
// available. The deprecated v1 ABI (GPIO_GET_LINEHANDLE_IOCTL), which
// is only present with CONFIG_GPIO_CDEV_V1, serves as a fallback for
// older kernels.
//
// Struct layouts and ioctl numbers mirror include/uapi/linux/gpio.h.
// All 64 bit fields of the v2 structs are naturally 8 byte aligned (the
// header uses __aligned_u64 and explicit padding), so the Go structs
// have the same layout on 32 bit ARM, where Go only aligns uint64 to 4
// bytes. reset_test.go asserts sizes and offsets.
package gpio

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// From include/uapi/linux/gpio.h.
const (
	gpioMaxNameSize       = 32 // GPIO_MAX_NAME_SIZE
	gpioV2LinesMax        = 64 // GPIO_V2_LINES_MAX
	gpioV2LineNumAttrsMax = 10 // GPIO_V2_LINE_NUM_ATTRS_MAX
	gpiohandlesMax        = 64 // GPIOHANDLES_MAX

	gpioV2LineFlagOutput         = 1 << 3     // GPIO_V2_LINE_FLAG_OUTPUT = _BITULL(3)
	gpioV2LineAttrIDOutputValues = 2          // GPIO_V2_LINE_ATTR_ID_OUTPUT_VALUES
	gpiohandleRequestOutput      = 1 << 1     // GPIOHANDLE_REQUEST_OUTPUT
	gpioV2GetLineIoctl           = 0xc250b407 // _IOWR(0xB4, 0x07, struct gpio_v2_line_request), size 592
	gpioV2LineSetValuesIoctl     = 0xc010b40f // _IOWR(0xB4, 0x0F, struct gpio_v2_line_values), size 16
	gpioGetLinehandleIoctl       = 0xc16cb403 // _IOWR(0xB4, 0x03, struct gpiohandle_request), size 364
	gpiohandleSetLineValuesIoctl = 0xc040b409 // _IOWR(0xB4, 0x09, struct gpiohandle_data), size 64
)

// struct gpio_v2_line_attribute (16 bytes). The union holds flags,
// values (both __aligned_u64) or debounce_period_us (__u32); only
// values is used here.
type gpioV2LineAttribute struct {
	ID      uint32
	Padding uint32
	Values  uint64
}

// struct gpio_v2_line_config_attribute (24 bytes).
type gpioV2LineConfigAttribute struct {
	Attr gpioV2LineAttribute
	Mask uint64
}

// struct gpio_v2_line_config (272 bytes).
type gpioV2LineConfig struct {
	Flags    uint64
	NumAttrs uint32
	Padding  [5]uint32
	Attrs    [gpioV2LineNumAttrsMax]gpioV2LineConfigAttribute
}

// struct gpio_v2_line_request (592 bytes).
type gpioV2LineRequest struct {
	Offsets         [gpioV2LinesMax]uint32
	Consumer        [gpioMaxNameSize]byte
	Config          gpioV2LineConfig
	NumLines        uint32
	EventBufferSize uint32
	Padding         [5]uint32
	Fd              int32
}

// struct gpio_v2_line_values (16 bytes).
type gpioV2LineValues struct {
	Bits uint64
	Mask uint64
}

// struct gpiohandle_request (v1, 364 bytes).
type gpiohandleRequest struct {
	Lineoffsets   [gpiohandlesMax]uint32
	Flags         uint32
	DefaultValues [gpiohandlesMax]uint8
	ConsumerLabel [gpioMaxNameSize]byte
	Lines         uint32
	Fd            int32
}

// struct gpiohandle_data (v1, 64 bytes).
type gpiohandleData struct {
	Values [gpiohandlesMax]uint8
}

var consumer = [gpioMaxNameSize]byte{'h', 'm', 'g', 'o'}

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// resetLine is a requested output line (v1 or v2 handle).
type resetLine struct {
	fd uintptr
	v2 bool
}

func (l resetLine) set(value bool) error {
	if l.v2 {
		vals := gpioV2LineValues{Mask: 1}
		if value {
			vals.Bits = 1
		}
		if err := ioctl(l.fd, gpioV2LineSetValuesIoctl, unsafe.Pointer(&vals)); err != nil {
			return fmt.Errorf("GPIO_V2_LINE_SET_VALUES_IOCTL: %w", err)
		}
		return nil
	}
	var data gpiohandleData
	if value {
		data.Values[0] = 1
	}
	if err := ioctl(l.fd, gpiohandleSetLineValuesIoctl, unsafe.Pointer(&data)); err != nil {
		return fmt.Errorf("GPIOHANDLE_SET_LINE_VALUES_IOCTL: %w", err)
	}
	return nil
}

// requestOutput requests line pin of the chip as an output, initially
// high (module running).
func requestOutput(chipfd uintptr, pin int) (resetLine, error) {
	req := gpioV2LineRequest{
		Consumer: consumer,
		NumLines: 1,
	}
	req.Offsets[0] = uint32(pin)
	req.Config.Flags = gpioV2LineFlagOutput
	req.Config.NumAttrs = 1
	req.Config.Attrs[0] = gpioV2LineConfigAttribute{
		Attr: gpioV2LineAttribute{ID: gpioV2LineAttrIDOutputValues, Values: 1},
		Mask: 1, // applies to offsets[0]
	}
	err := ioctl(chipfd, gpioV2GetLineIoctl, unsafe.Pointer(&req))
	if err == nil {
		return resetLine{fd: uintptr(req.Fd), v2: true}, nil
	}
	if !errors.Is(err, syscall.ENOTTY) && !errors.Is(err, syscall.EINVAL) {
		return resetLine{}, fmt.Errorf("GPIO_V2_GET_LINE_IOCTL: %w", err)
	}

	// Kernel without uAPI v2: fall back to the deprecated v1 ABI.
	handlereq := gpiohandleRequest{
		Flags:         gpiohandleRequestOutput,
		ConsumerLabel: consumer,
		Lines:         1,
	}
	handlereq.Lineoffsets[0] = uint32(pin)
	handlereq.DefaultValues[0] = 1
	if err1 := ioctl(chipfd, gpioGetLinehandleIoctl, unsafe.Pointer(&handlereq)); err1 != nil {
		return resetLine{}, fmt.Errorf("GPIO_V2_GET_LINE_IOCTL: %v; GPIO_GET_LINEHANDLE_IOCTL: %w", err, err1)
	}
	return resetLine{fd: uintptr(handlereq.Fd), v2: false}, nil
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

	line, err := requestOutput(f.Fd(), pin)
	if err != nil {
		return err
	}
	// The line handle is intentionally kept open for the lifetime of
	// the process: releasing it could let the reset line float.

	// Turn off device
	if err := line.set(false); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)

	// Flush all data in the input buffer
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uartfd, unix.TCFLSH, uintptr(syscall.TCIFLUSH)); err != 0 {
		return fmt.Errorf("TCFLSH: %v", err)
	}

	// Turn on device
	return line.set(true)
}
