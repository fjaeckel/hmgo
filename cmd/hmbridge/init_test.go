// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
)

type initRun struct {
	out, errOut bytes.Buffer
	code        int
}

func runInitTest(t *testing.T, input string, interactive bool, args ...string) *initRun {
	t.Helper()
	r := &initRun{}
	env := initEnv{
		in:          bufio.NewReader(strings.NewReader(input)),
		out:         &r.out,
		errOut:      &r.errOut,
		rand:        rand.Reader,
		now:         func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) },
		interactive: interactive,
	}
	r.code = runInit(append([]string{"-user", ""}, args...), env)
	return r
}

func TestInitNonInteractive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "etc", "config.json")
	state := filepath.Join(dir, "state")
	pwFile := filepath.Join(dir, "pw")
	os.WriteFile(pwFile, []byte("s3cret pass\n"), 0o600)
	r := runInitTest(t, "", false, "-config", cfgPath, "-state_dir", state,
		"-mqtt_broker", "tcp://ha.lan:1883", "-mqtt_username", "hmbridge", "-mqtt_password_file", pwFile)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut.String())
	}
	keyPath := filepath.Join(state, KeyFileName)
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyHex := strings.TrimSpace(string(keyData))
	if b, err := hex.DecodeString(keyHex); err != nil || len(b) != 16 {
		t.Fatalf("key file %d bytes", len(keyData))
	}
	if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(state); fi.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode %v", fi.Mode())
	}
	cfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// The key is never in the configuration or the output.
	for what, s := range map[string]string{"config": string(cfgData), "stdout": r.out.String(), "stderr": r.errOut.String()} {
		if strings.Contains(strings.ToLower(s), keyHex) {
			t.Errorf("%s contains the key", what)
		}
	}
	if strings.Contains(r.out.String(), "s3cret") {
		t.Error("password printed")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.PermissionWarnings(); len(w) != 0 {
		t.Errorf("permission warnings %q", w)
	}
	cur, prev := cfg.Keys()
	if cur.Index != 1 || hex.EncodeToString(cur.Bytes[:]) != keyHex || prev != nil {
		t.Errorf("key index %d", cur.Index)
	}
	if cfg.Security.KeyFile != keyPath || cfg.PairingAES() != config.PairingAESSystemKey {
		t.Errorf("security %+v, pairing %s", cfg.Security, cfg.PairingAES())
	}
	if cfg.MQTT.BrokerURL() != "tcp://ha.lan:1883" || cfg.MQTT.Username != "hmbridge" || cfg.MQTT.Password != "s3cret pass" {
		t.Errorf("mqtt %+v", cfg.MQTT)
	}
	if cfg.SerialPort != "/dev/serial0" || cfg.Radio != config.RadioAuto || cfg.Reset != "gpio" || !cfg.ResetGPIOAuto() {
		t.Errorf("radio %s %s %s", cfg.SerialPort, cfg.Radio, cfg.Reset)
	}
	if cfg.StateDirPath() != state || len(cfg.Devices) != 0 {
		t.Errorf("state %s devices %d", cfg.StateDirPath(), len(cfg.Devices))
	}
	a := cfg.CentralAddr()
	if v := int(a[0])<<16 | int(a[1])<<8 | int(a[2]); v < minRandomCentral || v > maxRandomCentral {
		t.Errorf("central %06X", v)
	}

	// Never overwrites: neither the configuration nor the key.
	r2 := runInitTest(t, "", false, "-config", cfgPath, "-state_dir", state)
	if r2.code == 0 || !strings.Contains(r2.errOut.String(), "already exists") {
		t.Errorf("second run: exit %d %s", r2.code, r2.errOut.String())
	}
	other := filepath.Join(dir, "other.json")
	r3 := runInitTest(t, "", false, "-config", other, "-state_dir", state)
	if r3.code == 0 || !strings.Contains(r3.errOut.String(), keyPath+" already exists") {
		t.Errorf("existing key: exit %d %s", r3.code, r3.errOut.String())
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("configuration written although the key exists")
	}
	if b, _ := os.ReadFile(keyPath); !bytes.Equal(b, keyData) {
		t.Error("key file changed")
	}
}

func TestInitInteractive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// Central address, serial port, broker, user name, password.
	input := "0x2A3B4C\n\ntcp://homeassistant.local:1883\nmqttuser\npw with spaces\n"
	r := runInitTest(t, input, true, "-config", cfgPath, "-state_dir", filepath.Join(dir, "state"))
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut.String())
	}
	for _, p := range []string{"Central address [", "Serial port of the radio module [/dev/serial0]", "MQTT broker [tcp://localhost:1883]", `MQTT user name ("-": none) [hmbridge]`, "MQTT password (not shown)"} {
		if !strings.Contains(r.out.String(), p) {
			t.Errorf("no prompt %q in %q", p, r.out.String())
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CentralAddr() != [3]byte{0x2a, 0x3b, 0x4c} || cfg.MQTT.Username != "mqttuser" || cfg.MQTT.Password != "pw with spaces" ||
		cfg.MQTT.BrokerURL() != "tcp://homeassistant.local:1883" || cfg.SerialPort != "/dev/serial0" {
		t.Errorf("config %X %+v %s", cfg.CentralAddr(), cfg.MQTT, cfg.SerialPort)
	}
}

func TestInitMQTTDisabledAndErrors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	r := runInitTest(t, "", false, "-config", cfgPath, "-state_dir", filepath.Join(dir, "s"), "-mqtt_broker", "none", "-serial_port", "/dev/ttyUSB0", "-central_address", "1A2B3C")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.BrokerURL() != "" || cfg.Reset != "gpio" || cfg.CentralAddr() != [3]byte{0x1a, 0x2b, 0x3c} {
		t.Errorf("broker %q reset %s", cfg.MQTT.BrokerURL(), cfg.Reset)
	}
	for _, args := range [][]string{
		{"-central_address", "000000"},
		{"-central_address", "FFFFFF"},
		{"-central_address", "12345"},
		{"-state_dir", "relative"},
		{"extra"},
	} {
		r := runInitTest(t, "", false, append([]string{"-config", filepath.Join(t.TempDir(), "c.json"), "-state_dir", t.TempDir()}, args...)...)
		if r.code == 0 {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestRandomCentral(t *testing.T) {
	seen := map[[3]byte]bool{}
	for i := 0; i < 2000; i++ {
		a, err := randomCentral(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		v := int(a[0])<<16 | int(a[1])<<8 | int(a[2])
		if v < minRandomCentral || v > maxRandomCentral {
			t.Fatalf("%06X out of range", v)
		}
		seen[a] = true
	}
	if len(seen) < 1990 {
		t.Errorf("only %d distinct addresses", len(seen))
	}
	// The bounds are reachable.
	if a, _ := randomCentral(bytes.NewReader([]byte{0, 0, 0, 0})); a != [3]byte{0x10, 0, 0} {
		t.Errorf("min %X", a)
	}
}

// ownerOf returns the owner and group of path (not following a link).
func ownerOf(t *testing.T, path string) (uid, gid int, perm os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), fi.Mode().Perm()
}

// A -key_file in a directory that does not exist yet: the directory is
// created private (0700 when not run as root).
func TestInitKeyFileInNewDirectory(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "keys", "sub", "hmbridge.key")
	r := runInitTest(t, "", false, "-config", filepath.Join(dir, "etc", "config.json"), "-state_dir", filepath.Join(dir, "state"),
		"-key_file", keyPath, "-mqtt_broker", "none")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut.String())
	}
	if _, _, perm := ownerOf(t, filepath.Dir(keyPath)); perm != 0o700 {
		t.Errorf("key directory mode %04o", perm)
	}
	if _, _, perm := ownerOf(t, keyPath); perm != 0o600 {
		t.Errorf("key file mode %04o", perm)
	}
	if _, _, perm := ownerOf(t, filepath.Join(dir, "state")); perm != 0o700 {
		t.Errorf("state dir mode %04o", perm)
	}
}

// Run as root for another user: the owners are set on the open files
// (before the key file gets its name) and directories.
func TestInitOwnersAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("no user nobody")
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	keyPath := filepath.Join(dir, "keys", "hmbridge.key")
	cfgPath := filepath.Join(dir, "etc", "config.json")
	r := runInitTest(t, "", false, "-user", "nobody", "-config", cfgPath, "-state_dir", state, "-key_file", keyPath, "-mqtt_broker", "none")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut.String())
	}
	for _, tc := range []struct {
		path     string
		uid, gid int
		perm     os.FileMode
	}{
		{state, uid, gid, 0o700},
		{filepath.Dir(keyPath), 0, gid, 0o750},
		{keyPath, uid, gid, 0o600},
		{filepath.Dir(cfgPath), 0, 0, 0o755},
		{cfgPath, 0, gid, 0o640},
	} {
		if gu, gg, gp := ownerOf(t, tc.path); gu != tc.uid || gg != tc.gid || gp != tc.perm {
			t.Errorf("%s: %d:%d %04o, want %d:%d %04o", tc.path, gu, gg, gp, tc.uid, tc.gid, tc.perm)
		}
	}
	// The default key file in the state directory.
	dir2 := t.TempDir()
	r = runInitTest(t, "", false, "-user", "nobody", "-config", filepath.Join(dir2, "c.json"), "-state_dir", filepath.Join(dir2, "s"), "-mqtt_broker", "none")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut.String())
	}
	if gu, _, gp := ownerOf(t, filepath.Join(dir2, "s", KeyFileName)); gu != uid || gp != 0o600 {
		t.Errorf("key file %d %04o", gu, gp)
	}
}
