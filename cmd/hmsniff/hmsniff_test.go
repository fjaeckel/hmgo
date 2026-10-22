package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// encodeFrame builds an escaped module frame.
func encodeFrame(dst, cnt byte, payload []byte) []byte {
	n := 2 + len(payload)
	raw := append([]byte{0xFD, byte(n >> 8), byte(n), dst, cnt}, payload...)
	c := crc16(raw, 0xD77F)
	raw = append(raw, byte(c>>8), byte(c))
	out := []byte{0xFD}
	for _, b := range raw[1:] {
		if b == 0xFC || b == 0xFD {
			out = append(out, 0xFC, b&0x7F)
		} else {
			out = append(out, b)
		}
	}
	return out
}

// kmsgLines renders data the way the raw-uart driver dumps it: 32 bytes
// per line, in /dev/kmsg record format.
func kmsgLines(dir string, data []byte) string {
	var sb strings.Builder
	for i := 0; i < len(data); i += 32 {
		end := min(i+32, len(data))
		parts := make([]string, 0, 32)
		for _, b := range data[i:end] {
			parts = append(parts, fmt.Sprintf("%02x", b))
		}
		fmt.Fprintf(&sb, "6,%d,123456,-;raw-uart raw-uart: %s: %s\n", i, dir, strings.Join(parts, " "))
	}
	return sb.String()
}

var (
	central = [3]byte{0x45, 0x80, 0x86}
	rtdn    = [3]byte{0x4D, 0xA5, 0xA9}
)

func testDecoder() *Decoder {
	return &Decoder{
		Devices: map[[3]byte]DeviceInfo{
			rtdn:               {Serial: "NEQ0882326", Type: "HM-CC-RT-DN", Addr: rtdn},
			{0x75, 0x91, 0xAB}: {Serial: "SEQ2094227", Type: "HM-Sec-SCo", Addr: [3]byte{0x75, 0x91, 0xAB}},
		},
		Central:   central,
		RedactAES: true,
		stats:     newStats(),
	}
}

func msgBytes(cnt, flags, cmd byte, src, dst [3]byte, payload ...byte) []byte {
	b := []byte{cnt, flags, cmd}
	b = append(b, src[:]...)
	b = append(b, dst[:]...)
	return append(b, payload...)
}

func TestStreamDecoderEscapingAndSplits(t *testing.T) {
	payload := []byte{0x05, 0xFD, 0xFC, 0x00, 0x7D, 0x7C} // bytes that need escaping
	enc := encodeFrame(dstLLMAC, 0x42, payload)
	var d StreamDecoder
	var frames []Frame
	// garbage, then the frame byte by byte
	frames = append(frames, d.Feed([]byte{0x12, 0x34})...)
	for _, b := range enc {
		frames = append(frames, d.Feed([]byte{b})...)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	f := frames[0]
	if !f.CRCOK || f.Dst != dstLLMAC || f.Cnt != 0x42 || !bytes.Equal(f.Payload, payload) {
		t.Errorf("frame = %+v", f)
	}
	if d.Skipped != 2 {
		t.Errorf("skipped = %d, want 2", d.Skipped)
	}
	// A truncated frame followed by a new one: resync.
	part := encodeFrame(dstLLMAC, 1, []byte{1, 2, 3, 4})
	frames = d.Feed(append(part[:4], enc...))
	if len(frames) != 1 || d.BadFrame != 1 {
		t.Errorf("after truncation: %d frames, %d bad", len(frames), d.BadFrame)
	}
	// Corrupted CRC is reported, not dropped.
	bad := encodeFrame(dstOS, 2, []byte{0x00, 'x'})
	bad[len(bad)-1] ^= 0x01
	frames = d.Feed(bad)
	if len(frames) != 1 || frames[0].CRCOK {
		t.Errorf("bad CRC frame: %+v", frames)
	}
}

func TestProcessKmsgRecvAndSend(t *testing.T) {
	d := testDecoder()
	// RT-DN INFO_LEVEL received (LLMAC RECV: cmd ts(2) info rssi msg).
	recv := append([]byte{0x05, 0x12, 0x34, 0x00, 0x41},
		msgBytes(0x05, 0xA6, 0x10, rtdn, central, 0x0A, 0xB0, 0xE2, 0x08, 0x25, 0x00)...)
	// central sends set temperature (LLMAC SEND: cmd opts(3) msg).
	send := append([]byte{0x06, 0x80, 0x00, 0x80},
		msgBytes(0x09, 0xB0, 0x11, central, rtdn, 0x86, 0x04, 43)...)
	// AES response from a contact: must be redacted.
	aes := append([]byte{0x05, 0x00, 0x01, 0x00, 0x50},
		msgBytes(0x10, 0x80, 0x03, [3]byte{0x75, 0x91, 0xAB}, central, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)...)
	// key-sized host→module frame on an unknown command: redacted.
	key := append([]byte{0x0A}, bytes.Repeat([]byte{0xAA}, 17)...)

	input := "unrelated line\n" +
		kmsgLines("RX", encodeFrame(dstLLMAC, 0x01, recv)) +
		kmsgLines("TX", encodeFrame(dstLLMAC, 0x07, send)) +
		kmsgLines("RX", encodeFrame(dstLLMAC, 0x02, aes)) +
		kmsgLines("TX", encodeFrame(dstLLMAC, 0x08, key)) +
		"[  12.345678] raw-uart raw-uart: RX: " + spaced(encodeFrame(dstCommon, 0x00, append([]byte{0x05, 0x01}, "DualCoPro_App"...))) + "\n"

	var out bytes.Buffer
	if err := process(context.Background(), strings.NewReader(input), d, &out); err != nil {
		t.Fatal(err)
	}
	log := out.String()
	for _, want := range []string{
		"RX LLMAC  cnt=01 RECV ts=1234 info=00 rssi=-65 #05 WKMEUP|BCAST|BIDI|RPTEN INFO 4DA5A9(HM-CC-RT-DN NEQ0882326) → 458086(central)",
		"SetTemperature:22 ActualTemperature:22.6",
		"TX LLMAC  cnt=07 SEND opts=800080 #09 BURST|BIDI|RPTEN SET 458086(central) → 4DA5A9(HM-CC-RT-DN NEQ0882326) 86042b",
		"AES_REPLY 7591AB(HM-Sec-SCo SEQ2094227) → 458086(central) <16 bytes redacted>",
		"<17 bytes redacted (possible key material)>",
		`COMMON cnt=00 cmd=05 01`,
		`"DualCoPro_App"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q\n%s", want, log)
		}
	}
	// Redacted frames must not appear as raw hex either.
	if strings.Contains(log, "0102030405060708") || strings.Contains(log, "aaaaaaaaaaaaaaaa") {
		t.Errorf("redacted bytes leaked into the log:\n%s", log)
	}
	var sum bytes.Buffer
	d.stats.Write(&sum, d)
	if !strings.Contains(sum.String(), "4DA5A9(HM-CC-RT-DN NEQ0882326)") || !strings.Contains(sum.String(), "rssi  -65") {
		t.Errorf("summary:\n%s", sum.String())
	}
}

func spaced(b []byte) string {
	s := hex.EncodeToString(b)
	var parts []string
	for i := 0; i < len(s); i += 2 {
		parts = append(parts, s[i:i+2])
	}
	return strings.Join(parts, " ")
}

func TestLoadDevicesAndCentral(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "NEQ0882326.dev"), []byte(`<device serial="NEQ0882326" type="HM-CC-RT-DN" address="0x4DA5A9" aes_key_index="2" firmware_version="1.5">
	<config/>
</device>`), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.dev"), []byte("garbage"), 0o644)
	devs := loadDevices(dir)
	if di, ok := devs[rtdn]; !ok || di.Type != "HM-CC-RT-DN" || di.KeyIndex != "2" || len(devs) != 1 {
		t.Errorf("devices = %+v", devs)
	}
	ids := filepath.Join(dir, "ids")
	os.WriteFile(ids, []byte("BidCoS-Address = 4554886\n\n"), 0o644)
	if got := loadCentral(ids); got != central {
		t.Errorf("central = %X, want %X", got, central)
	}
}
