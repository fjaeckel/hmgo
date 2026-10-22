// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Command hmsniff passively records the traffic between OpenCCU and its
// radio module (RPI-RF-MOD or HM-MOD-RPI-PCB) and decodes it.
//
// It uses the traffic dump of OpenCCU's raw-uart kernel driver
// (/sys/class/raw-uart/raw-uart/dump_traffic), so OpenCCU keeps running
// normally: hmsniff never opens the radio device and never transmits
// anything. It switches the dump on, reads the kernel log (/dev/kmsg),
// reassembles the module frames in both directions, decodes BidCoS
// messages using OpenCCU's device list (/etc/config/rfd/*.dev), and
// switches the dump off again on exit.
//
// AES key material is never logged, and AES responses are redacted by
// default.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/fjaeckel/hmgo/licenses"
)

var (
	replay   = flag.String("replay", "", "decode a saved kernel log (dmesg, /var/log/messages or a capture) instead of capturing live")
	outPath  = flag.String("out", "", "log file (default /tmp/hmsniff-<time>.log; /tmp is in RAM on OpenCCU)")
	duration = flag.Duration("duration", 0, "stop after this long (e.g. 30m); default: until Ctrl-C")
	rfdDir   = flag.String("rfd", "/etc/config/rfd", "directory with OpenCCU's *.dev device files (for names and types)")
	idsPath  = flag.String("ids", "/etc/config/ids", "OpenCCU's ids file (for the central address)")
	verbose  = flag.Bool("v", false, "also print raw frames on the terminal")
	quiet    = flag.Bool("q", false, "don't print frames on the terminal (log file only)")
	noRedact = flag.Bool("no-redact", false, "don't redact AES responses (key material is redacted regardless)")
	maxSize  = flag.Int64("max-size", 50<<20, "stop writing the log file beyond this many bytes (protects RAM on OpenCCU)")
	dumpPath = flag.String("dump-control", "", "path of the dump_traffic sysfs file (default: autodetect)")
	license  = flag.Bool("license", false, "print the licenses and exit")
)

// hexLineRe matches a raw-uart dump line: "...raw-uart...: RX: fd 00 0e ...".
var hexLineRe = regexp.MustCompile(`raw-uart.*?\b(RX|TX): ((?:[0-9a-fA-F]{2} ?)+)`)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hmsniff [flags]\n\nRecords and decodes OpenCCU's radio module traffic while OpenCCU keeps running.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *license {
		fmt.Print(licenses.Text())
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hmsniff:", err)
		os.Exit(1)
	}
}

func run() error {
	d := &Decoder{
		Devices:   loadDevices(*rfdDir),
		Central:   loadCentral(*idsPath),
		RedactAES: !*noRedact,
		stats:     newStats(),
	}

	path := *outPath
	if path == "" {
		path = fmt.Sprintf("/tmp/hmsniff-%s.log", time.Now().Format("20060102-150405"))
	}
	logf, err := os.Create(path)
	if err != nil {
		return err
	}
	defer logf.Close()
	lw := &limitedWriter{w: logf, max: *maxSize}

	fmt.Fprintf(lw, "# hmsniff %s, %d devices from %s, central %s\n", time.Now().Format(time.RFC3339), len(d.Devices), *rfdDir, d.name(d.Central))
	fmt.Fprintf(os.Stderr, "hmsniff: %d devices known, central address %02X%02X%02X, logging to %s\n", len(d.Devices), d.Central[0], d.Central[1], d.Central[2], path)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	var src io.Reader
	if *replay != "" {
		f, err := os.Open(*replay)
		if err != nil {
			return err
		}
		defer f.Close()
		src = f
	} else {
		restore, err := enableDump()
		if err != nil {
			return err
		}
		defer restore()
		kmsg, err := os.Open("/dev/kmsg")
		if err != nil {
			return fmt.Errorf("opening the kernel log: %w (run as root)", err)
		}
		// Start at the end of the kernel log: only new traffic.
		if _, err := kmsg.Seek(0, io.SeekEnd); err != nil {
			fmt.Fprintln(os.Stderr, "hmsniff: cannot seek to the end of /dev/kmsg; older messages will be included")
		}
		go func() { <-ctx.Done(); kmsg.Close() }()
		src = kmsgReader{kmsg}
		fmt.Fprintln(os.Stderr, "hmsniff: capturing; OpenCCU keeps running. Press Ctrl-C to stop.")
	}

	err = process(ctx, src, d, lw)
	d.stats.Write(lw, d)
	if !*quiet {
		d.stats.Write(os.Stderr, d)
	}
	if lw.dropped > 0 {
		fmt.Fprintf(os.Stderr, "hmsniff: log file reached %d bytes; %d bytes were not written\n", *maxSize, lw.dropped)
	}
	fmt.Fprintf(os.Stderr, "hmsniff: log written to %s\n", path)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// process reads kernel log lines, reassembles frames per direction and
// writes decoded lines.
func process(ctx context.Context, src io.Reader, d *Decoder, out io.Writer) error {
	decoders := map[string]*StreamDecoder{"RX": {}, "TX": {}}
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 256*1024), 256*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Text()
		m := hexLineRe.FindStringSubmatch(line)
		if m == nil {
			if strings.Contains(line, "raw-uart") && strings.Contains(line, "traffic dumping") {
				fmt.Fprintf(out, "# %s\n", strings.TrimSpace(line))
			}
			continue
		}
		dir := m[1]
		data, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(m[2]), " ", ""))
		if err != nil {
			continue
		}
		for _, f := range decoders[dir].Feed(data) {
			d.stats.frame(dir, f)
			desc := d.Describe(dir, f)
			ts := time.Now().Format("15:04:05.000")
			fmt.Fprintf(out, "%s %s %s\n", ts, dir, desc)
			if !strings.Contains(desc, "redacted") {
				fmt.Fprintf(out, "%s %s   raw %s\n", ts, dir, hex.EncodeToString(f.Raw))
			}
			if !*quiet {
				fmt.Printf("%s %s %s\n", ts, dir, desc)
				if *verbose && !strings.Contains(desc, "redacted") {
					fmt.Printf("%s %s   raw %s\n", ts, dir, hex.EncodeToString(f.Raw))
				}
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	err := sc.Err()
	if errors.Is(err, os.ErrClosed) {
		return ctx.Err()
	}
	return err
}

// kmsgReader turns /dev/kmsg records into lines. Each read returns one
// record; EPIPE means records were overwritten before we read them.
type kmsgReader struct{ f *os.File }

func (k kmsgReader) Read(p []byte) (int, error) {
	for {
		n, err := k.f.Read(p)
		if errors.Is(err, syscall.EPIPE) {
			fmt.Fprintln(os.Stderr, "hmsniff: kernel log overrun, some traffic was lost")
			continue
		}
		return n, err
	}
}

// enableDump switches on the raw-uart traffic dump and returns a function
// that restores the previous setting.
func enableDump() (func(), error) {
	path := *dumpPath
	if path == "" {
		matches, _ := filepath.Glob("/sys/class/raw-uart/*/dump_traffic")
		if len(matches) == 0 {
			return nil, errors.New("no /sys/class/raw-uart/*/dump_traffic found; is this OpenCCU with the raw-uart driver (RPI-RF-MOD / HM-MOD-RPI-PCB)?")
		}
		path = matches[0]
	}
	prev, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w (run as root)", path, err)
	}
	if err := os.WriteFile(path, []byte("1"), 0); err != nil {
		return nil, fmt.Errorf("enabling the traffic dump in %s: %w (run as root)", path, err)
	}
	fmt.Fprintf(os.Stderr, "hmsniff: traffic dump enabled (%s)\n", path)
	return func() {
		if strings.TrimSpace(string(prev)) == "on" {
			return // it was on before; leave it on
		}
		if err := os.WriteFile(path, []byte("0"), 0); err != nil {
			fmt.Fprintf(os.Stderr, "hmsniff: could not disable the traffic dump: %v\n  run: echo 0 > %s\n", err, path)
			return
		}
		fmt.Fprintln(os.Stderr, "hmsniff: traffic dump disabled again")
	}, nil
}

// limitedWriter stops writing after max bytes.
type limitedWriter struct {
	w       io.Writer
	max     int64
	n       int64
	dropped int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n+int64(len(p)) > l.max {
		l.dropped += int64(len(p))
		return len(p), nil
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	return len(p), err
}
