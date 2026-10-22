// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bidcosaes

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/fjaeckel/hmgo/internal/hmkey"
)

// testdata/fhem_keyexchange.txt was generated like fhem_oracle.txt: a
// Perl script running the byte handling of FHEM's 10_CUL_HM.pm
// “assignHmKey” (CUL branch) verbatim with Crypt::Rijndael, with
// rand(0xffff) replaced by the fixed values in the file. Keys: index 0
// eQ-3 default, 1 MD5("hmbridge-test-passphrase"), 2 MD5("geheim"),
// 5 MD5("second key").
var keyExchangeKeys = map[byte]hmkey.Key{
	0: hmkey.EQ3Default,
	1: hmkey.FromPassphrase("hmbridge-test-passphrase", 1),
	2: hmkey.FromPassphrase("geheim", 2),
	5: hmkey.FromPassphrase("second key", 5),
}

func TestKeyExchangeFHEMVectors(t *testing.T) {
	f, err := os.Open("testdata/fhem_keyexchange.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) != 9 || fl[0] != "X" {
			continue
		}
		oldIdx, _ := strconv.Atoi(fl[1])
		newIdx, _ := strconv.Atoi(fl[2])
		rnd := mustHex(t, fl[3]+fl[4])
		oldKey := keyExchangeKeys[byte(oldIdx)]
		newKey := keyExchangeKeys[byte(newIdx)]
		got, err := KeyExchangePayloads(oldKey.Bytes, newKey, bytes.NewReader(rnd))
		if err != nil {
			t.Fatal(err)
		}
		for half := 0; half < 2; half++ {
			want := strings.ToLower(fl[7+half])
			if hex.EncodeToString(got[half][:]) != want {
				t.Errorf("%v half %d: got %x, want %s", fl[1:5], half, got[half], want)
			}
			idx, h, part, ok := DecodeKeyExchange(oldKey.Bytes, got[half])
			if !ok || idx != byte(newIdx) || h != half || !bytes.Equal(part[:], newKey.Bytes[half*8:half*8+8]) {
				t.Errorf("%v half %d: decoded %d %d %x %v", fl[1:5], half, idx, h, part, ok)
			}
			// Wrong old key: the device cannot decode it.
			if _, _, _, ok := DecodeKeyExchange(keyExchangeKeys[5].Bytes, got[half]); ok && oldIdx != 5 {
				t.Errorf("decoded with the wrong key")
			}
		}
		n++
	}
	if n == 0 {
		t.Fatal("no vectors")
	}
}

func TestKeyExchangeErrors(t *testing.T) {
	if _, err := KeyExchangePayloads(hmkey.EQ3Default.Bytes, hmkey.EQ3Default, bytes.NewReader(make([]byte, 4))); err == nil {
		t.Error("key exchange to index 0 accepted")
	}
	if _, err := KeyExchangePayloads(hmkey.EQ3Default.Bytes, keyExchangeKeys[1], bytes.NewReader(nil)); err == nil {
		t.Error("missing random bytes not reported")
	}
	tpl := KeyExchangeTemplates(1)
	if !bytes.Equal(tpl[0], []byte{1, 2}) || !bytes.Equal(tpl[1], []byte{1, 3}) {
		t.Errorf("templates %x", tpl)
	}
}
