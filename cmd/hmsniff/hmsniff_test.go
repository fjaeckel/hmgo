// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

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
	"time"
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

// The devices hmbridge added later are decoded too, e.g. a captured
// HM-WDS30-T-O broadcast and a button press.
func TestDescribeNewDevices(t *testing.T) {
	wds := [3]byte{0x78, 0x5B, 0x74}
	pb := [3]byte{0x73, 0x78, 0x8E}
	d := &Decoder{
		Devices: map[[3]byte]DeviceInfo{
			wds: {Serial: "TEQ0694924", Type: "HM-WDS30-T-O", Addr: wds},
			pb:  {Serial: "REQ1215000", Type: "HM-PB-2-WM55", Addr: pb},
		},
		Central: central,
	}
	for _, tc := range []struct {
		msg  []byte
		want string
	}{
		{msgBytes(0x31, 0x86, 0x70, wds, [3]byte{}, 0x00, 0x82, 0x64), "| 13.0 °C"},
		{msgBytes(0x10, 0xA4, 0x40, pb, central, 0x42, 0x21), "| button 2 long release (counter 33)"},
	} {
		if got := d.describeMsg("RX", tc.msg, 0x40); !strings.HasSuffix(got, tc.want) {
			t.Errorf("got %q, want suffix %q", got, tc.want)
		}
	}
}

// HmIP frames are decoded with internal/hmip (captured frames from the
// RPI-RF-MOD restart captures; key material is synthetic).
func TestDescribeHmIP(t *testing.T) {
	type in struct{ dir, raw string }
	frames := []in{
		{"TX", "fd0003021412e047"},
		{"RX", "fd00070214060147def51464"},
		{"TX", "fd0007020f084d2fbfc7ef3b"},
		{"RX", "fd0004020f0601bdf1"},
		{"TX", "fd00110218030310008eb4e47af00001220164d078a4"},
		{"RX", "fd0019020f078010008fb4e47af000014d2fbfc9c0374c75220164d00671"},
		{"RX", "fd00170216074512008347def5f000010589067df060023e880414e4"},
	}
	var input strings.Builder
	for _, f := range frames {
		raw, err := hex.DecodeString(f.raw)
		if err != nil {
			t.Fatal(err)
		}
		input.WriteString(kmsgLines(f.dir, encodeFrame(raw[3], raw[4], raw[5:len(raw)-2])))
	}
	secret := bytes.Repeat([]byte{0xab}, 66)
	input.WriteString(kmsgLines("TX", encodeFrame(dstHmIP, 0x10, append([]byte{0x14}, secret...))))
	input.WriteString(kmsgLines("RX", encodeFrame(dstHmIP, 0x10, []byte{0x06, 0x01})))
	input.WriteString(kmsgLines("TX", encodeFrame(0x0c, 0x12, append([]byte{0x0e}, secret...))))
	input.WriteString(kmsgLines("RX", encodeFrame(0x0c, 0x13, append([]byte{0x03, 0x01}, secret[:16]...))))

	d := testDecoder()
	d.RedactAES = false // HmIP key material is redacted regardless
	var out bytes.Buffer
	if err := process(context.Background(), strings.NewReader(input.String()), d, &out); err != nil {
		t.Fatal(err)
	}
	log := out.String()
	for _, want := range []string{
		"TX HMIP   cnt=14 GET_LINK_PARTNERS",
		"RX HMIP   cnt=14 RESPONSE to GET_LINK_PARTNERS OK(01) partners=47DEF5",
		"TX HMIP   cnt=0F SET_SECURITY_COUNTER counter=4D2FBFC7",
		"TX HMIP   cnt=18 SEND burst=03 B4E47A→F00001 APP sec=1 v8 hop=7 | TRIGGER_STATUS seq=01 64d0",
		"RX HMIP   cnt=0F EVENT rssi=none (own transmission) B4E47A→F00001 APP sec=1 v8 hop=7 qos sn=4D2FBFC9 mic=c0374c75 | TRIGGER_STATUS seq=01 64d0",
		"RX HMIP   cnt=16 EVENT rssi=-69 47DEF5→F00001 CT2 sec=1 v8 hop=1 qos sn=0589067D mic=f060023e | 8804",
		"TX HMIP   cnt=10 SET_ENC_NETWORK_KEY <66 bytes redacted>",
		"RX HMIP   cnt=10 RESPONSE to SET_ENC_NETWORK_KEY OK(01)",
		"TX dst0C  cnt=12 KEY cmd=0E <66 bytes redacted>",
		"RX dst0C  cnt=13 KEY cmd=03 <17 bytes redacted>",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q\n%s", want, log)
		}
	}
	if strings.Contains(log, "abab") {
		t.Errorf("key material leaked:\n%s", log)
	}
}

// Key-looking frames are redacted even with -no-redact, which only
// shows AES responses.
func TestNoRedactKeepsKeysRedacted(t *testing.T) {
	d := testDecoder()
	d.RedactAES = false
	key := append([]byte{0x0A}, bytes.Repeat([]byte{0xAA}, 17)...)
	aes := append([]byte{0x05, 0x00, 0x01, 0x00, 0x50},
		msgBytes(0x10, 0x80, 0x03, [3]byte{0x75, 0x91, 0xAB}, central, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)...)
	input := kmsgLines("TX", encodeFrame(dstLLMAC, 0x08, key)) +
		kmsgLines("TX", encodeFrame(dstCommon, 0x09, key)) +
		kmsgLines("TX", encodeFrame(dstOS, 0x0a, key)) +
		kmsgLines("RX", encodeFrame(dstLLMAC, 0x02, aes)) +
		kmsgLines("TX", encodeFrame(0x0c, 0x12, append([]byte{0x0e}, key...)))
	var out bytes.Buffer
	if err := process(context.Background(), strings.NewReader(input), d, &out); err != nil {
		t.Fatal(err)
	}
	log := out.String()
	if strings.Contains(log, "aaaaaaaa") || strings.Count(log, "redacted") != 4 {
		t.Errorf("key material not redacted:\n%s", log)
	}
	if !strings.Contains(log, "0102030405060708") {
		t.Errorf("AES response redacted despite -no-redact:\n%s", log)
	}
}

// Over-long lines are skipped, not fatal.
func TestProcessSkipsLongLines(t *testing.T) {
	d := testDecoder()
	frame := kmsgLines("RX", encodeFrame(dstCommon, 0x00, append([]byte{0x05, 0x01}, "DualCoPro_App"...)))
	input := frame + strings.Repeat("x", maxLineLen+10) + "\n" + frame + "no newline at the end"
	var out bytes.Buffer
	if err := process(context.Background(), strings.NewReader(input), d, &out); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), `"DualCoPro_App"`); n != 2 {
		t.Errorf("decoded %d frames, want 2:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "skipped 1 over-long lines") {
		t.Errorf("skip not reported:\n%s", out.String())
	}
}

func TestReadLines(t *testing.T) {
	var lines []string
	skipped := 0
	err := readLines(context.Background(), strings.NewReader("a\r\n"+strings.Repeat("b", 20)+"\nc\n\nd"), 10,
		func(l string) { lines = append(lines, l) }, func() { skipped++ })
	if err != nil || fmt.Sprint(lines) != "[a c  d]" || skipped != 1 {
		t.Errorf("lines %q, skipped %d, err %v", lines, skipped, err)
	}
}

func TestLimitedWriter(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, max: 30, reserve: 10}
	fmt.Fprint(lw, "0123456789")  // 10
	fmt.Fprint(lw, "0123456789x") // would exceed 20: dropped, limit reached
	fmt.Fprint(lw, "short")       // sticky: dropped too
	lw.final = true
	fmt.Fprint(lw, strings.Repeat("s", 40)) // the summary is always written
	if want := "0123456789" + strings.Repeat("s", 40); buf.String() != want || lw.dropped != 16 {
		t.Errorf("wrote %q (dropped %d)", buf.String(), lw.dropped)
	}
}

func TestCreateLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.log")
	f, _, err := createLog(p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if _, _, err := createLog(p, time.Now()); err == nil {
		t.Error("existing file overwritten")
	}
	link := filepath.Join(dir, "link.log")
	os.Symlink(filepath.Join(dir, "target"), link)
	if _, _, err := createLog(link, time.Now()); err == nil {
		t.Error("symlink followed")
	}
	if _, err := os.Stat(filepath.Join(dir, "target")); err == nil {
		t.Error("symlink target created")
	}
	// The default name gets a suffix.
	now := time.Date(2001, 2, 3, 4, 5, 6, 0, time.Local)
	f1, p1, err := createLog("", now)
	if err != nil {
		t.Skipf("cannot create in /tmp: %v", err)
	}
	defer os.Remove(p1)
	f1.Close()
	f2, p2, err := createLog("", now)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p2)
	f2.Close()
	if p1 == p2 || !strings.HasSuffix(p2, "-1.log") {
		t.Errorf("paths %s, %s", p1, p2)
	}
}

func TestWasEnabled(t *testing.T) {
	for in, want := range map[string]bool{"1\n": true, "on": true, "Y\n": true, "y": true, "0\n": false, "N\n": false, "": false} {
		if got := wasEnabled([]byte(in)); got != want {
			t.Errorf("wasEnabled(%q) = %v", in, got)
		}
	}
}

// summaryWatcher records the dump control state when the summary is
// written to stderr.
type summaryWatcher struct {
	path  string
	state string
}

func (w *summaryWatcher) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "==== summary") {
		b, _ := os.ReadFile(w.path)
		w.state = string(b)
	}
	return len(p), nil
}

// The live capture switches the dump off before writing the summary
// (a broken pipe on stderr must not leave it on), and restoring is
// idempotent.
func TestRunRestoresDumpBeforeSummary(t *testing.T) {
	dir := t.TempDir()
	ctl := filepath.Join(dir, "dump_traffic")
	os.WriteFile(ctl, []byte("N\n"), 0o600)
	kmsg := filepath.Join(dir, "kmsg")
	os.WriteFile(kmsg, nil, 0o600)
	w := &summaryWatcher{path: ctl}
	oldStderr, oldKmsg := stderr, kmsgPath
	oldDump, oldOut, oldQuiet, oldRfd, oldIDs := *dumpPath, *outPath, *quiet, *rfdDir, *idsPath
	t.Cleanup(func() {
		stderr, kmsgPath = oldStderr, oldKmsg
		*dumpPath, *outPath, *quiet, *rfdDir, *idsPath = oldDump, oldOut, oldQuiet, oldRfd, oldIDs
	})
	stderr, kmsgPath = w, kmsg
	*dumpPath, *outPath, *quiet, *rfdDir, *idsPath = ctl, filepath.Join(dir, "out.log"), false, dir, filepath.Join(dir, "ids")
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if w.state != "0" {
		t.Errorf("dump control %q when the summary was written, want 0", w.state)
	}

	// Enabled before: left on; restore runs once.
	os.WriteFile(ctl, []byte("Y\n"), 0o600)
	restore, err := enableDump()
	if err != nil {
		t.Fatal(err)
	}
	restore()
	if b, _ := os.ReadFile(ctl); string(b) != "1" {
		t.Errorf("dump control %q, want it left on", b)
	}
	os.WriteFile(ctl, []byte("N\n"), 0o600)
	restore, _ = enableDump()
	restore()
	os.WriteFile(ctl, []byte("1"), 0o600)
	restore()
	if b, _ := os.ReadFile(ctl); string(b) != "1" {
		t.Errorf("second restore wrote %q", b)
	}
}
