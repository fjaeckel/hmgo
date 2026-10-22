// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Command hmkeycheck finds out which AES key a BidCoS device really uses.
//
// It reads an hmbridge -debug log, extracts the AES
// handshakes it contains (a device's message, the central's challenge and
// the device's 16-byte answer) and checks every candidate key against
// them, offline: no radio, no OpenCCU service and no network are needed.
//
// Candidates are passphrases (key = MD5(passphrase), as the CCU derives
// it), hex keys, and every 32-hex-digit value in a key file such as
// OpenCCU's /etc/config/keys. Key material is never printed: candidates are
// shown by where they came from (e.g. "-passphrase #2", "keys line 3").
package main

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/fjaeckel/hmgo/internal/buildinfo"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/licenses"
)

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint(len(*m)) }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

var (
	passphrases multiFlag
	hexKeys     multiFlag
	keysFile    = flag.String("keys", "", "file to take candidate keys from: every 32-hex-digit value (e.g. /etc/config/keys on OpenCCU)")
	stdinPass   = flag.Bool("stdin", false, "read candidate passphrases from standard input, one per line (keeps them out of the shell history)")
	license     = flag.Bool("license", false, "print the licenses and exit")
	showVersion = flag.Bool("version", false, "print the version and exit")
)

func main() {
	flag.Var(&passphrases, "passphrase", "candidate passphrase (repeatable)")
	flag.Var(&hexKeys, "key", "candidate key, 32 hex digits (repeatable)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: hmkeycheck [flags] <log file>...

Checks candidate AES keys against the AES handshakes in an hmbridge -debug
log, offline. Key material is never printed.

Examples:
  hmkeycheck -keys /etc/config/keys hmbridge-test.log
  hmkeycheck -stdin hmbridge-test.log      (then type passphrases, Ctrl-D)

`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *license {
		fmt.Print(licenses.Text())
		return
	}
	if *showVersion {
		fmt.Println("hmkeycheck", buildinfo.Version())
		return
	}
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	var hs []handshake
	for _, path := range flag.Args() {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hmkeycheck:", err)
			os.Exit(1)
		}
		hs = append(hs, parseLog(f)...)
		f.Close()
	}
	if len(hs) == 0 {
		fmt.Fprintln(os.Stderr, "hmkeycheck: no AES handshakes found in the log (it needs hmbridge's -debug output including a device's answer to a challenge)")
		os.Exit(1)
	}
	fmt.Printf("%d AES handshakes found:\n", len(hs))
	for _, h := range hs {
		fmt.Printf("  device %X, message %X, key index %d\n", h.device, h.msg, h.keyIndex)
	}

	cands, err := candidates()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hmkeycheck:", err)
		os.Exit(1)
	}
	found := false
	for _, c := range cands {
		ok := 0
		for _, h := range hs {
			if h.matches(c.key) {
				ok++
			}
		}
		verdict := "no match"
		if ok > 0 {
			verdict = "MATCH"
			found = true
		}
		fmt.Printf("%-24s %s (%d/%d handshakes)\n", c.label, verdict, ok, len(hs))
	}
	if !found {
		fmt.Println("\nNone of the candidates is the key these devices use.")
		os.Exit(1)
	}
}

type candidate struct {
	label string
	key   [16]byte
}

func candidates() ([]candidate, error) {
	// Always try the eQ-3 default key: a device that was never re-keyed
	// still uses it.
	res := []candidate{{"eQ-3 default key", hmkey.EQ3Default.Bytes}}
	for i, p := range passphrases {
		res = append(res, candidate{fmt.Sprintf("-passphrase #%d", i+1), md5.Sum([]byte(p))})
	}
	for i, k := range hexKeys {
		key, err := hmkey.FromHex(k, 0)
		if err != nil {
			// Generic: hmkey's error may quote a character of the key.
			return nil, fmt.Errorf("-key #%d: must be 32 hex digits (16 bytes)", i+1)
		}
		res = append(res, candidate{fmt.Sprintf("-key #%d", i+1), key.Bytes})
	}
	if *keysFile != "" {
		f, err := os.Open(*keysFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		res = append(res, keysFromFile(f)...)
	}
	if *stdinPass {
		sc := bufio.NewScanner(os.Stdin)
		n := 0
		for sc.Scan() {
			p := strings.TrimRight(sc.Text(), "\r")
			if p == "" {
				continue
			}
			n++
			res = append(res, candidate{fmt.Sprintf("stdin passphrase #%d", n), md5.Sum([]byte(p))})
		}
	}
	return res, nil
}

var hex32Re = regexp.MustCompile(`\b[0-9A-Fa-f]{32}\b`)

// keysFromFile returns every 32-hex-digit value of r as a candidate, also
// trying it as a passphrase (in case the file stores the passphrase).
func keysFromFile(r io.Reader) []candidate {
	var res []candidate
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		for j, m := range hex32Re.FindAllString(sc.Text(), -1) {
			b, _ := hex.DecodeString(m)
			var k [16]byte
			copy(k[:], b)
			label := fmt.Sprintf("keys line %d", line)
			if j > 0 {
				label += fmt.Sprintf(" value %d", j+1)
			}
			res = append(res, candidate{label, k})
		}
		// The value after "=" might be a passphrase rather than a key.
		if _, v, ok := strings.Cut(sc.Text(), "="); ok {
			if v = strings.TrimSpace(v); v != "" && !hex32Re.MatchString(v) {
				res = append(res, candidate{fmt.Sprintf("keys line %d as passphrase", line), md5.Sum([]byte(v))})
			}
		}
	}
	return res
}

// handshake is one AES exchange: the device's message, the central's
// challenge and the device's answer.
type handshake struct {
	device    [3]byte
	msg       []byte
	challenge [6]byte
	keyIndex  byte
	response  [16]byte
}

func (h handshake) matches(key [16]byte) bool {
	s := bidcosaes.NewSigner(hmkey.Key{Bytes: key, Index: h.keyIndex})
	return s.Verify(h.keyIndex, h.challenge, h.msg, h.response)
}

// Lines of hmbridge -debug: "uartgw: recv dst=LLMAC cnt=.. cmd=05 payload=<ts(2) info rssi msg>"
// and "uartgw: send dst=LLMAC cnt=.. cmd=06 payload=<opts(3) msg>".
var lineRe = regexp.MustCompile(`uartgw: (recv|send) dst=LLMAC cnt=\d+ cmd=(05|06) payload=([0-9a-fA-F]+)`)

func parseLog(r io.Reader) []handshake {
	type key struct {
		dev [3]byte
		cnt byte
	}
	msgs := map[key][]byte{}       // last device message (not ACK/AES) per device and counter
	challenges := map[key][]byte{} // last challenge sent to a device per counter
	var res []handshake
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		m := lineRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		p, err := hex.DecodeString(m[3])
		if err != nil {
			continue
		}
		var msg []byte
		switch m[1] {
		case "recv":
			if len(p) < 4+9 {
				continue
			}
			msg = p[4:]
		case "send":
			if len(p) < 3+9 {
				continue
			}
			msg = p[3:]
		}
		cnt, cmd := msg[0], msg[2]
		var src, dst [3]byte
		copy(src[:], msg[3:6])
		copy(dst[:], msg[6:9])
		payload := msg[9:]
		switch {
		case m[1] == "send" && cmd == 0x02 && len(payload) == 8 && payload[0] == 0x04:
			challenges[key{dst, cnt}] = payload
		case m[1] == "recv" && cmd == 0x03 && len(payload) >= 16:
			k := key{src, cnt}
			ch, okc := challenges[k]
			orig, okm := msgs[k]
			if !okc || !okm {
				continue
			}
			var h handshake
			h.device = src
			h.msg = orig
			copy(h.challenge[:], ch[1:7])
			h.keyIndex = ch[7] / 2
			copy(h.response[:], payload[:16])
			res = append(res, h)
			delete(challenges, k)
		case m[1] == "recv" && cmd != 0x02 && cmd != 0x03:
			msgs[key{src, cnt}] = append([]byte(nil), msg...)
		}
	}
	return res
}
