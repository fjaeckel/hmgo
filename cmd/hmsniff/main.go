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
	"sync"
	"syscall"
	"time"

	"github.com/fjaeckel/hmgo/licenses"
)

var (
	replay   = flag.String("replay", "", "decode a saved kernel log (dmesg, /var/log/messages or a capture) instead of capturing live")
	outPath  = flag.String("out", "", "log file, overwritten if it exists; also e.g. /dev/stdout (default /tmp/hmsniff-<time>.log, never overwritten; /tmp is in RAM on OpenCCU)")
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

// Overridable in tests.
var (
	stderr   io.Writer = os.Stderr
	kmsgPath           = "/dev/kmsg"
)

// maxLineLen is the longest kernel log line process accepts; longer
// lines are skipped.
const maxLineLen = 256 * 1024

// summaryReserve is the part of -max-size kept free for the final
// summary.
const summaryReserve = 16 * 1024

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
	// A closed terminal or pipe (e.g. hmsniff | head) must not kill
	// hmsniff before it switched the traffic dump off again: writes to
	// it fail with EPIPE instead.
	signal.Ignore(syscall.SIGPIPE)

	d := &Decoder{
		Devices:   loadDevices(*rfdDir),
		Central:   loadCentral(*idsPath),
		RedactAES: !*noRedact,
		stats:     newStats(),
	}

	logf, path, err := createLog(*outPath, time.Now())
	if err != nil {
		return err
	}
	defer logf.Close()
	lw := &limitedWriter{w: logf, max: *maxSize, reserve: min(summaryReserve, *maxSize)}

	fmt.Fprintf(lw, "# hmsniff %s, %d devices from %s, central %s\n", time.Now().Format(time.RFC3339), len(d.Devices), *rfdDir, d.name(d.Central))
	fmt.Fprintf(stderr, "hmsniff: %d devices known, central address %02X%02X%02X, logging to %s\n", len(d.Devices), d.Central[0], d.Central[1], d.Central[2], path)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	restore := func() {}
	var src io.Reader
	if *replay != "" {
		f, err := os.Open(*replay)
		if err != nil {
			return err
		}
		defer f.Close()
		src = f
	} else {
		restore, err = enableDump()
		if err != nil {
			return err
		}
		defer restore() // safety net; restored explicitly below
		kmsg, err := os.Open(kmsgPath)
		if err != nil {
			return fmt.Errorf("opening the kernel log: %w (run as root)", err)
		}
		defer kmsg.Close()
		// Start at the end of the kernel log: only new traffic.
		if _, err := kmsg.Seek(0, io.SeekEnd); err != nil {
			fmt.Fprintln(stderr, "hmsniff: cannot seek to the end of /dev/kmsg; older messages will be included")
		}
		go func() { <-ctx.Done(); kmsg.Close() }()
		src = kmsgReader{kmsg}
		fmt.Fprintln(stderr, "hmsniff: capturing; OpenCCU keeps running. Press Ctrl-C to stop.")
	}

	err = process(ctx, src, d, lw)
	// Switch the dump off before anything else (Ctrl-C, SIGTERM, SIGHUP
	// and -duration all end process).
	restore()
	stop()

	lw.final = true // the summary is always written
	if lw.dropped > 0 {
		fmt.Fprintf(lw, "# log file limit (-max-size %d) reached; %d bytes were not written\n", *maxSize, lw.dropped)
	}
	d.stats.Write(lw, d)
	if !*quiet {
		d.stats.Write(stderr, d)
	}
	if lw.dropped > 0 {
		fmt.Fprintf(stderr, "hmsniff: log file reached its limit of %d bytes; %d bytes were not written\n", *maxSize, lw.dropped)
	}
	fmt.Fprintf(stderr, "hmsniff: log written to %s\n", path)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// createLog opens the log file. The default name in /tmp is created
// exclusively (never following a symlink planted there, never
// overwriting a file) with mode 0600, and gets a suffix if it exists
// already. An explicit path (-out) may name an existing regular file,
// which is truncated and set to mode 0600, or a character device, pipe
// or socket such as /dev/stdout (also through a symlink); a symlink to
// a regular file is refused.
func createLog(path string, now time.Time) (*os.File, string, error) {
	if path != "" {
		f, err := openOut(path)
		return f, path, err
	}
	const flags = os.O_CREATE | os.O_EXCL | os.O_WRONLY | syscall.O_NOFOLLOW
	base := fmt.Sprintf("/tmp/hmsniff-%s", now.Format("20060102-150405"))
	path = base + ".log"
	for i := 1; ; i++ {
		f, err := os.OpenFile(path, flags, 0o600)
		if err == nil || !errors.Is(err, os.ErrExist) || i > 100 {
			return f, path, err
		}
		path = fmt.Sprintf("%s-%d.log", base, i)
	}
}

// openOut opens an explicit -out path, see createLog.
func openOut(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, syscall.ELOOP) {
		// A symlink, e.g. /dev/stdout → /proc/self/fd/1: only to
		// something that is not a regular file (no O_CREATE: never
		// create a symlink's target).
		f, err = os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		if fi, err := f.Stat(); err != nil || fi.Mode().IsRegular() {
			f.Close()
			if err == nil {
				err = fmt.Errorf("%s is a symlink to a regular file; refusing to follow it", path)
			}
			return nil, err
		}
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && fi.Mode().IsRegular() {
		// An existing file: overwrite it, and keep the log private.
		if err = f.Truncate(0); err == nil {
			err = f.Chmod(0o600)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// process reads kernel log lines, reassembles frames per direction and
// writes decoded lines. Lines longer than maxLineLen are skipped.
func process(ctx context.Context, src io.Reader, d *Decoder, out io.Writer) error {
	decoders := map[string]*StreamDecoder{"RX": {}, "TX": {}}
	skipped := 0
	err := readLines(ctx, src, maxLineLen, func(line string) {
		m := hexLineRe.FindStringSubmatch(line)
		if m == nil {
			if strings.Contains(line, "raw-uart") && strings.Contains(line, "traffic dumping") {
				fmt.Fprintf(out, "# %s\n", strings.TrimSpace(line))
			}
			return
		}
		dir := m[1]
		data, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(m[2]), " ", ""))
		if err != nil {
			return
		}
		for _, f := range decoders[dir].Feed(data) {
			d.stats.frame(dir, f)
			desc, redacted := d.Describe(dir, f)
			ts := time.Now().Format("15:04:05.000")
			fmt.Fprintf(out, "%s %s %s\n", ts, dir, desc)
			if !redacted {
				fmt.Fprintf(out, "%s %s   raw %s\n", ts, dir, hex.EncodeToString(f.Raw))
			}
			if !*quiet {
				fmt.Printf("%s %s %s\n", ts, dir, desc)
				if *verbose && !redacted {
					fmt.Printf("%s %s   raw %s\n", ts, dir, hex.EncodeToString(f.Raw))
				}
			}
		}
	}, func() { skipped++ })
	if skipped > 0 {
		fmt.Fprintf(out, "# skipped %d over-long lines (> %d bytes)\n", skipped, maxLineLen)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, os.ErrClosed) {
		return ctx.Err()
	}
	return err
}

// readLines calls line for every line of r (without the line break) and
// skip for every line longer than max bytes, until EOF (nil error), a
// read error or ctx is done.
func readLines(ctx context.Context, r io.Reader, max int, line func(string), skip func()) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var buf []byte
	tooLong := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		chunk, err := br.ReadSlice('\n')
		if !tooLong && len(buf)+len(chunk) > max+1 { // +1: the '\n'
			tooLong, buf = true, buf[:0]
		} else if !tooLong {
			buf = append(buf, chunk...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if len(buf) > 0 || tooLong {
			if tooLong {
				skip()
			} else {
				line(strings.TrimRight(string(buf), "\r\n"))
			}
			buf, tooLong = buf[:0], false
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// kmsgReader turns /dev/kmsg records into lines. Each read returns one
// record; EPIPE means records were overwritten before we read them.
type kmsgReader struct{ f *os.File }

func (k kmsgReader) Read(p []byte) (int, error) {
	for {
		n, err := k.f.Read(p)
		if errors.Is(err, syscall.EPIPE) {
			fmt.Fprintln(stderr, "hmsniff: kernel log overrun, some traffic was lost")
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
	fmt.Fprintf(stderr, "hmsniff: traffic dump enabled (%s)\n", path)
	var once sync.Once
	return func() {
		once.Do(func() {
			if wasEnabled(prev) {
				return // it was on before; leave it on
			}
			if err := os.WriteFile(path, []byte("0"), 0); err != nil {
				fmt.Fprintf(stderr, "hmsniff: could not disable the traffic dump: %v\n  run: echo 0 > %s\n", err, path)
				return
			}
			fmt.Fprintln(stderr, "hmsniff: traffic dump disabled again")
		})
	}, nil
}

// wasEnabled interprets the previous content of dump_traffic (a boolean
// module parameter reads Y/N; accept 1/on as well).
func wasEnabled(prev []byte) bool {
	switch strings.ToLower(strings.TrimSpace(string(prev))) {
	case "1", "on", "y", "yes", "true":
		return true
	}
	return false
}

// limitedWriter stops writing once a write would exceed max - reserve
// bytes; from then on, everything is dropped (no later, shorter lines
// sneak in). With final set (the summary), writes may use the reserve;
// beyond it, they are dropped and summaryTruncated is written once.
type limitedWriter struct {
	w         io.Writer
	max       int64
	reserve   int64 // kept free for the final writes
	n         int64
	dropped   int64
	full      bool
	final     bool
	finalN    int64
	finalFull bool
}

const summaryTruncated = "# summary truncated (log file limit reached)\n"

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.final {
		if l.finalFull || l.finalN+int64(len(p)) > l.reserve-int64(len(summaryTruncated)) {
			l.dropped += int64(len(p))
			if !l.finalFull {
				l.finalFull = true
				n, err := io.WriteString(l.w, summaryTruncated)
				l.n += int64(n)
				return len(p), err
			}
			return len(p), nil
		}
		n, err := l.w.Write(p)
		l.n += int64(n)
		l.finalN += int64(n)
		return len(p), err
	}
	if l.full || l.n+int64(len(p)) > l.max-l.reserve {
		l.full = true
		l.dropped += int64(len(p))
		return len(p), nil
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	return len(p), err
}
