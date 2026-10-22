// Package bidcosaes implements BidCoS AES message authentication
// ("AES signing") in software, for radio modules that do not handle it
// themselves.
//
// A BidCoS AES handshake authenticates one message m (the "m-frame"):
//
//  1. The receiver of m answers with a challenge, cmd 0x02 with payload
//     04 c0..c5 kk, where c is a 6 byte random challenge and kk is the
//     key index times two (see [NewChallenge], [ParseChallenge]).
//  2. The sender of m answers with cmd 0x03 and a 16 byte response
//     (see [Signer.Respond]), which the challenger checks
//     (see [Signer.Verify]).
//  3. The challenger acknowledges m; its ACK carries 4 "auth bytes" that
//     prove it could decrypt the response (see [Signer.RespondAuth],
//     [Signer.VerifyAuth]).
//
// The algorithm, as implemented by FHEM's 10_CUL_HM.pm and Homegear's
// AesHandshake.cpp, with m given as BidCoS bytes starting at the message
// counter (cnt flags cmd src(3) dst(3) payload...):
//
//	K'   = key with K'[i] ^= c[i] for i < 6          // temporary key
//	pdd  = r(6) || m[0:10]                            // r random, m zero padded
//	pd   = AES-ECB-Encrypt(K', pdd)                   // auth bytes = pd[0:4]
//	resp = AES-ECB-Encrypt(K', pd XOR m[10:26])       // m[10:] zero padded/truncated to 16
//
// Verification decrypts in the opposite order and compares pdd[6:16] with
// m[0:10], ignoring the "repeated" flag (0x40) of the flags byte.
// Message bytes beyond m[25] are not covered by the response.
package bidcosaes

import (
	"crypto/aes"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"

	"github.com/fjaeckel/hmgo/internal/hmkey"
)

// AESSigner answers and checks BidCoS AES challenges. It is implemented by
// [Signer].
type AESSigner interface {
	// Respond computes the 16-byte payload of the central's cmd 0x03 answer to a device's challenge for msg (the message the central sent, as BidCoS bytes starting at the message counter: cnt flags cmd src(3) dst(3) payload).
	Respond(keyIndex byte, challenge [6]byte, msg []byte) ([16]byte, error)
	// Verify checks a device's 16-byte cmd 0x03 answer to the central's challenge for msg (the device's original message, starting at the message counter).
	Verify(keyIndex byte, challenge [6]byte, msg []byte, response [16]byte) bool
}

var _ AESSigner = (*Signer)(nil)

// ErrUnknownKey is returned (wrapped) when no key with the requested index
// is known.
var ErrUnknownKey = errors.New("unknown AES key index")

// ErrShortMessage is returned (wrapped) when the message is shorter than a
// BidCoS header (cnt flags cmd src dst = 9 bytes).
var ErrShortMessage = errors.New("message too short")

// minMsgLen is the length of a BidCoS message without payload.
const minMsgLen = 9

// repeatedFlag is the BidCoS "repeated" flag, which repeaters may set in
// transit and which is therefore not authenticated.
const repeatedFlag = 0x40

// Signer holds AES keys by index and implements [AESSigner]. It is safe for
// concurrent use. Its String and GoString methods never reveal key material.
type Signer struct {
	mu   sync.RWMutex
	keys map[byte][16]byte
}

// NewSigner returns a Signer for the given keys, looked up by their Index.
// The eQ-3 default key ([hmkey.EQ3Default]) is always available as index 0
// unless a key with index 0 is passed explicitly. If several keys share an
// index, the last one wins.
func NewSigner(keys ...hmkey.Key) *Signer {
	s := &Signer{keys: make(map[byte][16]byte, len(keys)+1)}
	s.keys[hmkey.EQ3Default.Index] = hmkey.EQ3Default.Bytes
	for _, k := range keys {
		s.keys[k.Index] = k.Bytes
	}
	return s
}

// SetKey adds or replaces a key (e.g. after a key change).
func (s *Signer) SetKey(k hmkey.Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k.Index] = k.Bytes
}

// HasKey reports whether a key with the given index is known.
func (s *Signer) HasKey(keyIndex byte) bool {
	_, ok := s.key(keyIndex)
	return ok
}

// String describes the signer without revealing key material.
func (s *Signer) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := make([]int, 0, len(s.keys))
	for i := 0; i < 256; i++ {
		if _, ok := s.keys[byte(i)]; ok {
			idx = append(idx, i)
		}
	}
	return fmt.Sprintf("bidcosaes.Signer(key indexes %v)", idx)
}

// GoString is like String, so that %#v does not reveal key material either.
func (s *Signer) GoString() string { return s.String() }

func (s *Signer) key(keyIndex byte) ([16]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[keyIndex]
	return k, ok
}

// Respond implements [AESSigner].
func (s *Signer) Respond(keyIndex byte, challenge [6]byte, msg []byte) ([16]byte, error) {
	resp, _, err := s.RespondAuth(keyIndex, challenge, msg)
	return resp, err
}

// RespondAuth is like Respond but also returns the 4 auth bytes that the
// device must append to its ACK of msg to prove that it accepted the
// response (FHEM: AESreqAck; Homegear: checkAFrame).
func (s *Signer) RespondAuth(keyIndex byte, challenge [6]byte, msg []byte) (resp [16]byte, auth [4]byte, err error) {
	key, ok := s.key(keyIndex)
	if !ok {
		return resp, auth, fmt.Errorf("bidcosaes: respond: %w %d", ErrUnknownKey, keyIndex)
	}
	if len(msg) < minMsgLen {
		return resp, auth, fmt.Errorf("bidcosaes: respond: %w (%d bytes)", ErrShortMessage, len(msg))
	}
	var r [6]byte
	rand.Read(r[:]) // never returns an error since Go 1.24
	resp, auth = respond(key, challenge, r, msg)
	return resp, auth, nil
}

// Verify implements [AESSigner].
func (s *Signer) Verify(keyIndex byte, challenge [6]byte, msg []byte, response [16]byte) bool {
	_, ok := s.VerifyAuth(keyIndex, challenge, msg, response)
	return ok
}

// VerifyAuth is like Verify but also returns the 4 auth bytes the central
// appends to its ACK of msg if the response is valid (FHEM: aesAuthBytes;
// Homegear: getAFrame). auth is only meaningful if ok is true.
func (s *Signer) VerifyAuth(keyIndex byte, challenge [6]byte, msg []byte, response [16]byte) (auth [4]byte, ok bool) {
	key, found := s.key(keyIndex)
	if !found || len(msg) < minMsgLen {
		return auth, false
	}
	return verify(key, challenge, msg, response)
}

// tempKey returns the handshake key: the AES key with its first 6 bytes
// XORed with the challenge (FHEM: $keys{$kNo} ^ pack("H12", $challenge)).
func tempKey(key [16]byte, challenge [6]byte) [16]byte {
	for i, c := range challenge {
		key[i] ^= c
	}
	return key
}

// header returns m[0:10], zero padded (FHEM: pack("H20", substr(cmd, 4, 20))).
func header(msg []byte) (h [10]byte) {
	copy(h[:], msg)
	return h
}

// ivXOR XORs m[10:] into b, i.e. m[10:26] zero padded or truncated to 16
// bytes (FHEM: $response ^ pack("H*", substr(cmd, 24)), then substr 0, 16).
func ivXOR(b *[16]byte, msg []byte) {
	if len(msg) <= 10 {
		return
	}
	for i, v := range msg[10:] {
		if i >= len(b) {
			break
		}
		b[i] ^= v
	}
}

// respond is the deterministic core of RespondAuth: r is the 6 byte random
// prefix (FHEM: pack("Nn", seconds, microseconds)).
func respond(key [16]byte, challenge, r [6]byte, msg []byte) (resp [16]byte, auth [4]byte) {
	k := tempKey(key, challenge)
	block, err := aes.NewCipher(k[:])
	if err != nil {
		panic("bidcosaes: " + err.Error()) // unreachable: key length is fixed
	}
	var pdd, pd [16]byte
	copy(pdd[0:6], r[:])
	h := header(msg)
	copy(pdd[6:], h[:])
	block.Encrypt(pd[:], pdd[:])
	copy(auth[:], pd[0:4])
	ivXOR(&pd, msg)
	block.Encrypt(resp[:], pd[:])
	return resp, auth
}

// verify is the core of VerifyAuth (FHEM: "Check AES response from device").
func verify(key [16]byte, challenge [6]byte, msg []byte, response [16]byte) (auth [4]byte, ok bool) {
	k := tempKey(key, challenge)
	block, err := aes.NewCipher(k[:])
	if err != nil {
		panic("bidcosaes: " + err.Error()) // unreachable: key length is fixed
	}
	var pd, pdd [16]byte
	block.Decrypt(pd[:], response[:])
	ivXOR(&pd, msg)
	copy(auth[:], pd[0:4])
	block.Decrypt(pdd[:], pd[:])

	want := header(msg)
	want[1] &^= repeatedFlag
	got := pdd[6:16]
	got[1] &^= repeatedFlag
	return auth, subtle.ConstantTimeCompare(got, want[:]) == 1
}

// NewChallenge creates a challenge for a device's message: the payload of
// the central's cmd 0x02 frame, 04 c0..c5 kk with a random challenge c and
// kk = keyIndex*2 (FHEM 10_CUL_HM.pm: sprintf("%02X", $kNo*2)), and the
// challenge itself for the later [Signer.Verify].
func NewChallenge(keyIndex byte) (payload []byte, challenge [6]byte) {
	rand.Read(challenge[:]) // never returns an error since Go 1.24
	payload = make([]byte, 0, 8)
	payload = append(payload, 0x04)
	payload = append(payload, challenge[:]...)
	payload = append(payload, keyIndex*2)
	return payload, challenge
}

// ParseChallenge parses the payload of a device's cmd 0x02 challenge frame
// (04 c0..c5 kk). keyIndex is kk/2 (FHEM: hex($aesKeyNbr) / 2; Homegear:
// payload.back() / 2). ok is false if the payload is not an AES challenge
// or kk is odd.
func ParseChallenge(payload []byte) (keyIndex byte, challenge [6]byte, ok bool) {
	if len(payload) != 8 || payload[0] != 0x04 || payload[7]%2 != 0 {
		return 0, challenge, false
	}
	copy(challenge[:], payload[1:7])
	return payload[7] / 2, challenge, true
}
