// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bidcosaes

// BidCoS AES key exchange: the central gives a device a new AES key
// (the CCU's “system security key”) with two messages, cmd 0x04, each
// carrying one half of the new key, encrypted with the key the device
// uses so far (the eQ-3 default key for a freshly paired device):
//
//	plain  = 01 idx h0..h7 r0 r1 7E 29 6F A5      (16 bytes)
//	          │  │   │       │     └ magic
//	          │  │   │       └ 2 random bytes
//	          │  │   └ 8 bytes of the new key (first or second half)
//	          │  └ new key index·2 for the first half, ·2+1 for the second
//	          └ 01 = change key
//	payload = AES-128-ECB-Encrypt(old key, plain)
//
// The device answers each message with an AES challenge (with its old
// key index), which the central answers like for any other command;
// then it acknowledges. This is the layout of FHEM 10_CUL_HM.pm
// (“assignHmKey” with a CUL: pack("CCa8nN", 1, idx*2(+1), key half,
// rand(0xffff), 0x7e296fa5)) and of Homegear
// (AesHandshake::generateKeyChangePacket); it was implemented from that
// description and is checked against vectors generated with FHEM's code
// (testdata/fhem_keyexchange.txt). Neither has been checked against a
// real device yet.
//
// Radio modules that do AES themselves (HM-MOD-RPI-PCB, HM-LAN, HM-LGW)
// are given an unencrypted 2-byte template instead, 01 <old index·2>
// and 01 <old index·2+1>, and fill in their current key (FHEM: the
// non-CUL branch of assignHmKey; Homegear: BidCoSPeer key change
// queue, Hm_Mod_Rpi_Pcb::sendPacket), see KeyExchangeTemplates.

import (
	"crypto/aes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/fjaeckel/hmgo/internal/hmkey"
)

// keyExchangeMagic ends every key exchange block.
const keyExchangeMagic = 0x7e296fa5

// KeyExchangePayloads returns the two 16-byte payloads (cmd 0x04) that
// give a device using oldKey the key newKey, in the order they must be
// sent. rnd supplies the 2 random bytes of each block (crypto/rand.Reader
// in production). The payloads contain key material: never log them.
func KeyExchangePayloads(oldKey [16]byte, newKey hmkey.Key, rnd io.Reader) ([2][16]byte, error) {
	var out [2][16]byte
	if newKey.Index == 0 || newKey.Index > 127 {
		return out, fmt.Errorf("bidcosaes: key exchange to key index %d (want 1..127)", newKey.Index)
	}
	block, err := aes.NewCipher(oldKey[:])
	if err != nil {
		panic("bidcosaes: " + err.Error()) // unreachable: key length is fixed
	}
	for half := 0; half < 2; half++ {
		var plain [16]byte
		plain[0] = 0x01
		plain[1] = newKey.Index*2 + byte(half)
		copy(plain[2:10], newKey.Bytes[half*8:half*8+8])
		if _, err := io.ReadFull(rnd, plain[10:12]); err != nil {
			return out, fmt.Errorf("bidcosaes: key exchange random bytes: %w", err)
		}
		binary.BigEndian.PutUint32(plain[12:16], keyExchangeMagic)
		block.Encrypt(out[half][:], plain[:])
	}
	return out, nil
}

// KeyExchangeTemplates returns the 2-byte payloads for radio modules
// that encrypt the key exchange themselves (HM-MOD-RPI-PCB): 01 <old
// index·2> and 01 <old index·2+1>. The module replaces them with its
// current key, encrypted with the device's old key. ASSUMPTION (from
// FHEM and Homegear, not verified with a real module): the module moves
// the device to its current key index.
func KeyExchangeTemplates(oldIndex byte) [2][]byte {
	return [2][]byte{{0x01, oldIndex * 2}, {0x01, oldIndex*2 + 1}}
}

// DecodeKeyExchange decrypts a key exchange payload with oldKey, as a
// device does. It returns the new key index, which half (0 or 1) the
// block carries and the 8 key bytes; ok is false if the block does not
// end with the magic or does not start with 01 (wrong old key).
func DecodeKeyExchange(oldKey [16]byte, payload [16]byte) (newIndex byte, half int, part [8]byte, ok bool) {
	block, err := aes.NewCipher(oldKey[:])
	if err != nil {
		panic("bidcosaes: " + err.Error())
	}
	var plain [16]byte
	block.Decrypt(plain[:], payload[:])
	if plain[0] != 0x01 || binary.BigEndian.Uint32(plain[12:16]) != keyExchangeMagic {
		return 0, 0, part, false
	}
	copy(part[:], plain[2:10])
	return plain[1] / 2, int(plain[1] % 2), part, true
}
