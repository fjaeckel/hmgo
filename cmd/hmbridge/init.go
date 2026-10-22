// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/store"
)

// KeyFileName is the name of the AES system key file hmbridge init
// creates in the state directory.
const KeyFileName = "system.key"

// Central addresses hmbridge init chooses from. A BidCoS address is 24
// bits. 000000 means “no central” (unpaired devices, unpair messages)
// and FFFFFF is not a valid unicast address. As a precaution the
// ranges 000000-0FFFFF and F00000-FFFFFF are avoided altogether:
// software around the CCU uses low and high addresses for broadcasts,
// virtual and test devices (a convention, not a documented eQ-3 rule).
// Device addresses are assigned by eQ-3 in the factory; a collision
// with one of them is very unlikely, and pairing refuses a device that
// has the central's address.
const (
	minRandomCentral = 0x100000
	maxRandomCentral = 0xEFFFFF
)

// randomCentral returns a random central address in
// [minRandomCentral, maxRandomCentral].
func randomCentral(r io.Reader) ([3]byte, error) {
	var b [4]byte
	n := uint32(maxRandomCentral - minRandomCentral + 1)
	// Rejection sampling: no modulo bias.
	limit := (1 << 32) / uint64(n) * uint64(n)
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return [3]byte{}, err
		}
		v := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
		if v >= limit {
			continue
		}
		a := minRandomCentral + uint32(v%uint64(n))
		return [3]byte{byte(a >> 16), byte(a >> 8), byte(a)}, nil
	}
}

// generatedConfig is the configuration hmbridge init writes (field
// order as in the file).
type generatedConfig struct {
	Comment        []string          `json:"_comment"`
	SerialPort     string            `json:"serial_port"`
	Radio          string            `json:"radio"`
	Reset          string            `json:"reset"`
	CentralAddress string            `json:"central_address"`
	Security       generatedSecurity `json:"security"`
	MQTT           generatedMQTT     `json:"mqtt"`
	StateDir       string            `json:"state_dir"`
	Devices        []struct{}        `json:"devices"`
}

type generatedSecurity struct {
	KeyFile string `json:"key_file"`
	Index   int    `json:"index"`
}

type generatedMQTT struct {
	Broker   string `json:"broker"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// initEnv is what runInit talks to (replaced in tests).
type initEnv struct {
	in     *bufio.Reader
	out    io.Writer
	errOut io.Writer
	// tty is the terminal to read the password from without echo; nil
	// if stdin is no terminal.
	tty  *os.File
	rand io.Reader
	now  func() time.Time
	// interactive: prompt for the settings not given as flags.
	interactive bool
}

// runInit implements “hmbridge init”: it writes a new configuration
// and a new AES system key file, and never overwrites existing files.
func runInit(args []string, env initEnv) int {
	flags := flag.NewFlagSet("hmbridge init", flag.ContinueOnError)
	flags.SetOutput(env.errOut)
	cfgPath := flags.String("config", "/etc/hmbridge/config.json", "configuration file to create")
	stateDir := flags.String("state_dir", config.DefaultStateDir, "state directory (absolute)")
	keyFile := flags.String("key_file", "", "AES system key file to create (default <state_dir>/"+KeyFileName+")")
	central := flags.String("central_address", "", "central address (6 hex digits; default: random)")
	port := flags.String("serial_port", config.DefaultSerialPort, "serial port of the radio module")
	broker := flags.String("mqtt_broker", "", "MQTT broker URL, e.g. tcp://homeassistant.local:1883 (default "+config.DefaultBroker+"; \"none\" disables MQTT)")
	username := flags.String("mqtt_username", "", "MQTT user name")
	passwordFile := flags.String("mqtt_password_file", "", "read the MQTT password from this file (\"-\": the first line of stdin)")
	owner := flags.String("user", "hmbridge", "system user hmbridge runs as: owns the state directory and the key file, and is the configuration file's group (only when run as root; \"\" skips)")
	nonInteractive := flags.Bool("non_interactive", false, "do not prompt; use the flags and defaults")
	flags.Usage = func() {
		fmt.Fprintf(env.errOut, "usage: hmbridge init [flags]\n\nCreates a new configuration (no devices) and a new random AES system key for a fresh installation without a CCU. The key is stored in a separate file (mode 0600) and never printed. Existing files are never overwritten.\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(env.errOut, "hmbridge init: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	interactive := env.interactive && !*nonInteractive
	fail := func(format string, a ...any) int {
		fmt.Fprintf(env.errOut, "hmbridge init: "+format+"\n", a...)
		return 1
	}

	if !filepath.IsAbs(*stateDir) {
		return fail("-state_dir must be an absolute path")
	}
	*stateDir = filepath.Clean(*stateDir)
	if *keyFile == "" {
		*keyFile = filepath.Join(*stateDir, KeyFileName)
	}
	if !filepath.IsAbs(*keyFile) {
		return fail("-key_file must be an absolute path")
	}
	// Refuse before asking anything.
	for _, p := range []string{*cfgPath, *keyFile} {
		if _, err := os.Lstat(p); err == nil {
			return fail("%s already exists; not overwriting it (move it away first if you really want a new one; a new system key means re-pairing every AES device)", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fail("%v", err)
		}
	}

	ask := func(prompt, def string) (string, error) {
		if def != "" {
			fmt.Fprintf(env.out, "%s [%s]: ", prompt, def)
		} else {
			fmt.Fprintf(env.out, "%s: ", prompt)
		}
		line, err := env.in.ReadString('\n')
		if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
			return "", fmt.Errorf("reading the answer: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return def, nil
		}
		return line, nil
	}

	// Central address.
	if *central == "" {
		a, err := randomCentral(env.rand)
		if err != nil {
			return fail("generating a central address: %v", err)
		}
		*central = fmt.Sprintf("%06X", a[:])
		if interactive {
			fmt.Fprintf(env.out, "The central address is the radio address hmbridge uses. A new random one is fine for a fresh installation;\nenter your CCU's address instead only if devices are still paired to it.\n")
			v, err := ask("Central address", *central)
			if err != nil {
				return fail("%v", err)
			}
			*central = v
		}
	}
	addr, err := parseCentral(*central)
	if err != nil {
		return fail("central address %q: %v", *central, err)
	}

	if interactive && !explicit["serial_port"] {
		if *port, err = ask("Serial port of the radio module", *port); err != nil {
			return fail("%v", err)
		}
	}

	// MQTT.
	if interactive && !explicit["mqtt_broker"] {
		fmt.Fprintf(env.out, "MQTT broker of Home Assistant, e.g. tcp://homeassistant.local:1883 (\"none\" disables MQTT).\n")
		if *broker, err = ask("MQTT broker", config.DefaultBroker); err != nil {
			return fail("%v", err)
		}
	}
	if *broker == "" {
		*broker = config.DefaultBroker
	}
	mqttOff := *broker == "none"
	if mqttOff {
		*broker = ""
	}
	if !mqttOff && interactive && !explicit["mqtt_username"] {
		if *username, err = ask("MQTT user name (\"-\": none)", "hmbridge"); err != nil {
			return fail("%v", err)
		}
		if *username == "-" {
			*username = ""
		}
	}
	var password string
	switch {
	case *passwordFile != "":
		if password, err = readPasswordFile(*passwordFile, env.in); err != nil {
			return fail("-mqtt_password_file: %v", err)
		}
	case !mqttOff && interactive && *username != "":
		fmt.Fprintf(env.out, "MQTT password (not shown): ")
		if password, err = readPassword(env); err != nil {
			return fail("reading the password: %v", err)
		}
		fmt.Fprintln(env.out)
	}
	if strings.ContainsAny(password, "\r\n") {
		return fail("the MQTT password must be a single line")
	}

	// The key: 128 random bits, index 1 (the first key a CCU sets).
	var key [16]byte
	if _, err := io.ReadFull(env.rand, key[:]); err != nil {
		return fail("generating the system key: %v", err)
	}
	keyHex := []byte(hex.EncodeToString(key[:]) + "\n")
	defer clear(keyHex)

	gen := generatedConfig{
		Comment: []string{
			fmt.Sprintf("Generated by hmbridge init on %s. See internal/config/example.json for all options.", env.now().Format("2006-01-02")),
			"The AES system key is in security.key_file (mode 0600), never in this file. Back that file up: without it, every AES device must be factory-reset and paired again.",
			"Devices are paired from Home Assistant (button \"Pair new device\" on the hmbridge device) and kept in state_dir/devices.json.",
		},
		SerialPort:     *port,
		Radio:          config.RadioAuto,
		Reset:          "gpio",
		CentralAddress: fmt.Sprintf("%06X", addr[:]),
		Security:       generatedSecurity{KeyFile: *keyFile, Index: 1},
		MQTT:           generatedMQTT{Broker: *broker, Username: *username, Password: password},
		StateDir:       *stateDir,
		Devices:        []struct{}{},
	}
	if !config.OnboardUART(*port) {
		gen.Reset = "auto"
	}
	cfgJSON, err := json.MarshalIndent(gen, "", "  ")
	if err != nil {
		return fail("%v", err)
	}
	cfgJSON = append(cfgJSON, '\n')

	// Who will run hmbridge.
	uid, gid := -1, -1
	if *owner != "" && os.Geteuid() == 0 {
		if u, err := user.Lookup(*owner); err != nil {
			fmt.Fprintf(env.errOut, "warning: user %q not found (create it first, see docs/DEPLOY.md); the files stay owned by root and hmbridge running as %q will not be able to read them\n", *owner, *owner)
		} else {
			uid, _ = strconv.Atoi(u.Uid)
			gid, _ = strconv.Atoi(u.Gid)
		}
	}

	// Write the key, check the whole configuration, then write it.
	if err := os.MkdirAll(*stateDir, store.DirMode); err != nil {
		return fail("%v", err)
	}
	if uid >= 0 {
		if err := os.Chown(*stateDir, uid, gid); err != nil {
			return fail("%v", err)
		}
	}
	if err := store.WriteFileNew(*keyFile, keyHex); err != nil {
		return fail("writing the key file: %v", err)
	}
	removeKey := func() {
		if err := os.Remove(*keyFile); err != nil {
			fmt.Fprintf(env.errOut, "hmbridge init: removing %s again: %v\n", *keyFile, err)
		}
	}
	if uid >= 0 {
		if err := os.Chown(*keyFile, uid, gid); err != nil {
			removeKey()
			return fail("%v", err)
		}
	}
	if _, err := config.Parse(cfgJSON); err != nil {
		removeKey()
		return fail("the generated configuration is invalid: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o755); err != nil {
		removeKey()
		return fail("%v", err)
	}
	if err := store.WriteFileNew(*cfgPath, cfgJSON); err != nil {
		removeKey()
		return fail("writing the configuration: %v", err)
	}
	// Readable by hmbridge's group (it holds the MQTT password).
	if uid >= 0 {
		if err := os.Chown(*cfgPath, 0, gid); err != nil {
			fmt.Fprintf(env.errOut, "warning: %v\n", err)
		} else if err := os.Chmod(*cfgPath, 0o640); err != nil {
			fmt.Fprintf(env.errOut, "warning: %v\n", err)
		}
	}

	fmt.Fprintf(env.out, "created %s (central address %06X, %s)\n", *cfgPath, addr[:], describeMQTT(gen.MQTT))
	fmt.Fprintf(env.out, "created %s: a new random AES system key, index 1 (not shown; back this file up)\n", *keyFile)
	if uid < 0 && *owner != "" {
		fmt.Fprintf(env.out, "note: run as root to hand %s and %s to user %q, or fix the owners yourself (see docs/DEPLOY.md)\n", *stateDir, *keyFile, *owner)
	}
	fmt.Fprintf(env.out, "next: sudo -u %s hmbridge -check_config -config %s, then systemctl enable --now hmbridge\n", ownerOr(*owner), *cfgPath)
	return 0
}

func ownerOr(u string) string {
	if u == "" {
		return "hmbridge"
	}
	return u
}

func describeMQTT(m generatedMQTT) string {
	if m.Broker == "" {
		return "MQTT disabled"
	}
	s := "MQTT " + m.Broker
	if m.Username != "" {
		s += " as " + m.Username
	}
	return s
}

func parseCentral(s string) ([3]byte, error) {
	t := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	b, err := hex.DecodeString(t)
	if err != nil || len(b) != 3 {
		return [3]byte{}, errors.New("want 6 hex digits")
	}
	a := [3]byte(b)
	if a == [3]byte{} || a == [3]byte{0xff, 0xff, 0xff} {
		return a, errors.New("000000 and FFFFFF are reserved")
	}
	return a, nil
}

// readPasswordFile reads the first line of path ("-": of in).
func readPasswordFile(path string, in *bufio.Reader) (string, error) {
	var r *bufio.Reader
	if path == "-" {
		r = in
	} else {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r = bufio.NewReader(io.LimitReader(f, 4096))
	}
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readPassword reads a line from env.in, with the terminal's echo
// switched off if env.tty is set (otherwise, e.g. in tests, a plain
// line).
func readPassword(env initEnv) (string, error) {
	if env.tty == nil {
		line, err := env.in.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fd := int(env.tty.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	noEcho := *old
	noEcho.Lflag &^= unix.ECHO
	noEcho.Lflag |= unix.ICANON | unix.ECHONL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err != nil {
		return "", err
	}
	// Restore the echo also on Ctrl-C.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			unix.IoctlSetTermios(fd, unix.TCSETS, old)
			os.Exit(130)
		case <-done:
		}
	}()
	defer func() {
		close(done)
		signal.Stop(sig)
		unix.IoctlSetTermios(fd, unix.TCSETS, old)
	}()
	line, err := env.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// defaultInitEnv talks to the process's stdin/stdout.
func defaultInitEnv() initEnv {
	env := initEnv{
		in:     bufio.NewReader(os.Stdin),
		out:    os.Stdout,
		errOut: os.Stderr,
		rand:   rand.Reader,
		now:    time.Now,
	}
	if isTerminal(os.Stdin) {
		env.interactive = true
		env.tty = os.Stdin
	}
	return env
}
