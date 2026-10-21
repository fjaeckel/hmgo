package gpio

import (
	"testing"
	"unsafe"
)

// iowr mirrors the generic _IOWR(type, nr, size) macro from
// include/uapi/asm-generic/ioctl.h (used by arm and arm64):
// dir (_IOC_READ|_IOC_WRITE = 3) << 30 | size << 16 | type << 8 | nr.
func iowr(typ, nr, size uintptr) uintptr {
	const (
		iocNRShift   = 0
		iocTypeShift = 8
		iocSizeShift = 16
		iocDirShift  = 30
		iocWrite     = 1
		iocRead      = 2
	)
	if size >= 1<<14 {
		panic("ioctl size too large")
	}
	return (iocRead|iocWrite)<<iocDirShift | size<<iocSizeShift | typ<<iocTypeShift | nr<<iocNRShift
}

// Sizes as computed from include/uapi/linux/gpio.h.
func TestStructSizes(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		// __u32 id; __u32 padding; union { __aligned_u64 ... }
		{"gpio_v2_line_attribute", unsafe.Sizeof(gpioV2LineAttribute{}), 16},
		// attr (16) + __aligned_u64 mask
		{"gpio_v2_line_config_attribute", unsafe.Sizeof(gpioV2LineConfigAttribute{}), 24},
		// __aligned_u64 flags; __u32 num_attrs; __u32 padding[5]; attrs[10]
		{"gpio_v2_line_config", unsafe.Sizeof(gpioV2LineConfig{}), 8 + 4 + 5*4 + 10*24},
		// offsets[64] (256) + consumer[32] + config (272) + num_lines +
		// event_buffer_size + padding[5] + fd
		{"gpio_v2_line_request", unsafe.Sizeof(gpioV2LineRequest{}), 256 + 32 + 272 + 4 + 4 + 20 + 4},
		// __aligned_u64 bits; __aligned_u64 mask
		{"gpio_v2_line_values", unsafe.Sizeof(gpioV2LineValues{}), 16},
		// lineoffsets[64] + flags + default_values[64] + consumer_label[32] + lines + int fd
		{"gpiohandle_request", unsafe.Sizeof(gpiohandleRequest{}), 256 + 4 + 64 + 32 + 4 + 4},
		{"gpiohandle_data", unsafe.Sizeof(gpiohandleData{}), 64},
	} {
		if tt.got != tt.want {
			t.Errorf("sizeof(struct %s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
	if got := unsafe.Sizeof(gpioV2LineRequest{}); got != 592 {
		t.Errorf("sizeof(struct gpio_v2_line_request) = %d, want 592", got)
	}
	if got := unsafe.Sizeof(gpiohandleRequest{}); got != 364 {
		t.Errorf("sizeof(struct gpiohandle_request) = %d, want 364", got)
	}
}

// Field offsets: the C structs use __aligned_u64, so every 64 bit field
// must be at a multiple of 8 even on 32 bit platforms (where Go aligns
// uint64 to 4 bytes only). Run with GOARCH=386 to exercise that.
func TestStructOffsets(t *testing.T) {
	var req gpioV2LineRequest
	var cfg gpioV2LineConfig
	var attr gpioV2LineAttribute
	var ca gpioV2LineConfigAttribute
	var h gpiohandleRequest
	for _, tt := range []struct {
		name      string
		got, want uintptr
	}{
		{"attribute.values", unsafe.Offsetof(attr.Values), 8},
		{"config_attribute.mask", unsafe.Offsetof(ca.Mask), 16},
		{"config.num_attrs", unsafe.Offsetof(cfg.NumAttrs), 8},
		{"config.attrs", unsafe.Offsetof(cfg.Attrs), 32},
		{"request.consumer", unsafe.Offsetof(req.Consumer), 256},
		{"request.config", unsafe.Offsetof(req.Config), 288},
		{"request.num_lines", unsafe.Offsetof(req.NumLines), 560},
		{"request.event_buffer_size", unsafe.Offsetof(req.EventBufferSize), 564},
		{"request.fd", unsafe.Offsetof(req.Fd), 588},
		{"handle.flags", unsafe.Offsetof(h.Flags), 256},
		{"handle.default_values", unsafe.Offsetof(h.DefaultValues), 260},
		{"handle.consumer_label", unsafe.Offsetof(h.ConsumerLabel), 324},
		{"handle.lines", unsafe.Offsetof(h.Lines), 356},
		{"handle.fd", unsafe.Offsetof(h.Fd), 360},
	} {
		if tt.got != tt.want {
			t.Errorf("offsetof(%s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestIoctlNumbers(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		// #define GPIO_V2_GET_LINE_IOCTL _IOWR(0xB4, 0x07, struct gpio_v2_line_request)
		{"GPIO_V2_GET_LINE_IOCTL", gpioV2GetLineIoctl, iowr(0xB4, 0x07, unsafe.Sizeof(gpioV2LineRequest{}))},
		// #define GPIO_V2_LINE_SET_VALUES_IOCTL _IOWR(0xB4, 0x0F, struct gpio_v2_line_values)
		{"GPIO_V2_LINE_SET_VALUES_IOCTL", gpioV2LineSetValuesIoctl, iowr(0xB4, 0x0F, unsafe.Sizeof(gpioV2LineValues{}))},
		// #define GPIO_GET_LINEHANDLE_IOCTL _IOWR(0xB4, 0x03, struct gpiohandle_request)
		{"GPIO_GET_LINEHANDLE_IOCTL", gpioGetLinehandleIoctl, iowr(0xB4, 0x03, unsafe.Sizeof(gpiohandleRequest{}))},
		// #define GPIOHANDLE_SET_LINE_VALUES_IOCTL _IOWR(0xB4, 0x09, struct gpiohandle_data)
		{"GPIOHANDLE_SET_LINE_VALUES_IOCTL", gpiohandleSetLineValuesIoctl, iowr(0xB4, 0x09, unsafe.Sizeof(gpiohandleData{}))},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %#x, want %#x", tt.name, tt.got, tt.want)
		}
	}
	// Spelled out, independent of the Go struct sizes.
	if got, want := iowr(0xB4, 0x07, 592), uintptr(0xc250b407); got != want {
		t.Errorf("iowr(0xB4, 0x07, 592) = %#x, want %#x", got, want)
	}
	if got, want := iowr(0xB4, 0x0F, 16), uintptr(0xc010b40f); got != want {
		t.Errorf("iowr(0xB4, 0x0F, 16) = %#x, want %#x", got, want)
	}
}

func TestFlagValues(t *testing.T) {
	// enum gpio_v2_line_flag: GPIO_V2_LINE_FLAG_OUTPUT = _BITULL(3)
	if gpioV2LineFlagOutput != 8 {
		t.Errorf("GPIO_V2_LINE_FLAG_OUTPUT = %d, want 8", gpioV2LineFlagOutput)
	}
	// enum gpio_v2_line_attr_id: GPIO_V2_LINE_ATTR_ID_OUTPUT_VALUES = 2
	if gpioV2LineAttrIDOutputValues != 2 {
		t.Errorf("GPIO_V2_LINE_ATTR_ID_OUTPUT_VALUES = %d", gpioV2LineAttrIDOutputValues)
	}
	// #define GPIOHANDLE_REQUEST_OUTPUT (1UL << 1)
	if gpiohandleRequestOutput != 2 {
		t.Errorf("GPIOHANDLE_REQUEST_OUTPUT = %d", gpiohandleRequestOutput)
	}
}
