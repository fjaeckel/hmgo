// Package hmkey describes HomeMatic BidCoS AES keys.
//
// A BidCoS device authenticates messages with a 128-bit AES key that is
// identified by an index. A device that was never assigned a custom key
// uses the eQ-3 default key with index 0. When a CCU (or OpenCCU) sets a
// “system security key”, the key bytes are the MD5 hash of the passphrase
// the user entered, and the index starts at 1 and increments on every
// subsequent key change.
package hmkey

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
)

// Key is an AES key together with its BidCoS key index.
type Key struct {
	Bytes [16]byte
	Index byte
}

// EQ3Default is the publicly known key all BidCoS devices ship with.
var EQ3Default = Key{
	Bytes: [16]byte{
		0xA4, 0xE3, 0x75, 0xC6, 0xB0, 0x9F, 0xD1, 0x85,
		0xF2, 0x7C, 0x4E, 0x96, 0xFC, 0x27, 0x3A, 0xE4,
	},
	Index: 0,
}

// FromHex parses a key given as 32 hexadecimal characters.
func FromHex(s string, index byte) (Key, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return Key{}, fmt.Errorf("invalid hex key: %v", err)
	}
	if len(b) != 16 {
		return Key{}, fmt.Errorf("invalid key length: got %d bytes, want 16", len(b))
	}
	k := Key{Index: index}
	copy(k.Bytes[:], b)
	return k, nil
}

// FromPassphrase derives a key the same way a CCU does: MD5(passphrase).
func FromPassphrase(passphrase string, index byte) Key {
	return Key{Bytes: md5.Sum([]byte(passphrase)), Index: index}
}

// String hides the key material so that keys can be logged safely.
func (k Key) String() string {
	return fmt.Sprintf("hmkey(index %d)", k.Index)
}
