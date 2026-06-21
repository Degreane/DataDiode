// Package integrity provides application-layer hashing and verification
// for DataDiode payloads.
//
// The on-wire frame already carries a per-frame SHA-256 (see ADR-0002 and
// the framing package). That hash protects against bit-flips and naive
// tampering on the wire. This package exists for the *application* layer
// — for plugins and the message-reassembly stage in diode-rx that want
// to verify a reassembled message end-to-end, independent of the frame
// transport.
//
// Today the only Verifier is SHA-256. The Hasher / Verifier interfaces
// exist so a future ADR-0004 can swap in HMAC-SHA-256 (pre-shared key)
// or Ed25519 signatures without changing call sites.
package integrity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"hash"
	"io"
)

// DigestLen is the byte length of a Digest. Fixed at 32 (SHA-256). If a
// future Verifier uses a different size, it will live behind its own
// type, not this constant.
const DigestLen = sha256.Size

// Digest is a fixed-size cryptographic hash output. The named type
// prevents accidental confusion with arbitrary 32-byte slices and steers
// callers toward Equal / Verify instead of the built-in == on slices.
type Digest [DigestLen]byte

// String returns the lower-case hex encoding of d. Useful for logs and
// metrics; the format is not part of the on-wire contract.
func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// Bytes returns d as a slice aliasing the underlying array. Callers must
// not mutate it. Provided for callers that need to copy into a wire
// buffer or hand the bytes to a stdlib API.
//
// A pointer receiver is used so the returned slice aliases the caller's
// Digest rather than a value-receiver copy.
func (d *Digest) Bytes() []byte { return d[:] }

// Equal reports whether two digests are byte-identical. The comparison
// runs in constant time so callers cannot leak information about a
// secret digest through a timing side channel.
func Equal(a, b Digest) bool {
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// ErrMismatch is returned by Verify when the computed digest does not
// match the expected one.
var ErrMismatch = errors.New("integrity: digest mismatch")

// ----- Hasher ---------------------------------------------------------------

// Hasher is the streaming hash API. It is a narrowed version of hash.Hash
// that returns a typed Digest rather than a []byte. The narrowed surface
// lets us swap implementations (SHA-256, HMAC, etc.) without exposing
// the underlying primitive.
type Hasher interface {
	io.Writer
	// Sum finalizes the running hash and returns the Digest. After Sum
	// the Hasher is reset and ready for a new message.
	Sum() Digest
}

// sha256Hasher adapts hash.Hash (from crypto/sha256) to the Hasher
// interface.
type sha256Hasher struct{ h hash.Hash }

// NewSHA256 returns a fresh streaming Hasher backed by crypto/sha256.
func NewSHA256() Hasher { return &sha256Hasher{h: sha256.New()} }

func (s *sha256Hasher) Write(p []byte) (int, error) { return s.h.Write(p) }

func (s *sha256Hasher) Sum() Digest {
	var d Digest
	s.h.Sum(d[:0])
	s.h.Reset()
	return d
}

// ----- One-shot helpers -----------------------------------------------------

// Hash is the one-shot equivalent of NewSHA256 + Write + Sum, allocation-
// free. Use it when the whole payload is already in memory.
func Hash(b []byte) Digest {
	return Digest(sha256.Sum256(b))
}

// Verify recomputes the digest of payload and compares it against want
// in constant time. Returns ErrMismatch on mismatch, nil on match.
func Verify(payload []byte, want Digest) error {
	got := Hash(payload)
	if !Equal(got, want) {
		return ErrMismatch
	}
	return nil
}

// ----- HKDF-SHA256 + AEAD (ADR-0008) ---------------------------------------

// AEADKeyLen is the byte length of an AES-256-GCM key.
const AEADKeyLen = 32

// AEADNonceLen is the byte length of an AES-256-GCM nonce.
const AEADNonceLen = 12

// AEADTagLen is the byte length of an AES-256-GCM authentication tag.
const AEADTagLen = 16

// ErrAEADDecrypt is returned by AEADOpen when authentication or
// decryption fails (wrong key, tampered ciphertext, or tampered AAD).
var ErrAEADDecrypt = errors.New("integrity: AEAD decryption failed")

// HKDFSHA256 derives okmLen bytes of output keying material from the
// input keying material `ikm` using HKDF-SHA256 with the given salt
// and info strings. Implements RFC 5869 directly (Go stdlib has no
// public HKDF API in this version; a ~20-line implementation is
// simpler than pulling in golang.org/x/crypto).
func HKDFSHA256(salt, ikm, info []byte, okmLen int) ([]byte, error) {
	if okmLen <= 0 || okmLen > 255*sha256.Size {
		return nil, errors.New("integrity: HKDF okmLen out of range")
	}
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size) // RFC 5869 §2.2: zero salt allowed
	}
	// Extract: PRK = HMAC-SHA256(salt, ikm)
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	prk := mac.Sum(nil)

	// Expand: T(0)=empty; T(i)=HMAC-SHA256(PRK, T(i-1)||info||i)
	out := make([]byte, 0, okmLen)
	var prev []byte
	for i := byte(1); len(out) < okmLen; i++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(prev)
		mac.Write(info)
		mac.Write([]byte{i})
		prev = mac.Sum(nil)
		out = append(out, prev...)
	}
	return out[:okmLen], nil
}

// DeriveAEADKey derives the AES-256-GCM subkey from the operator's PSK
// using HKDF-SHA256 with the v3 domain-separation strings locked in
// ADR-0008. The same PSK can be reused for future subkeys by changing
// the salt / info pair.
func DeriveAEADKey(psk []byte) ([]byte, error) {
	const (
		salt = "diode-aead-v3-salt"
		info = "diode-aead-v3 chunk-aead"
	)
	return HKDFSHA256([]byte(salt), psk, []byte(info), AEADKeyLen)
}

// NewAEAD returns an AES-256-GCM cipher.AEAD over the given 32-byte key.
func NewAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != AEADKeyLen {
		return nil, errors.New("integrity: AEAD key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// AEADSeal encrypts plaintext with key+nonce+aad and appends the
// ciphertext+tag to dst. Returns the extended slice.
func AEADSeal(dst, key, nonce, aad, plaintext []byte) ([]byte, error) {
	aead, err := NewAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("integrity: AEAD nonce wrong length")
	}
	return aead.Seal(dst, nonce, plaintext, aad), nil
}

// AEADOpen verifies-and-decrypts ciphertext+tag with key+nonce+aad and
// appends the plaintext to dst. Returns ErrAEADDecrypt on any failure.
func AEADOpen(dst, key, nonce, aad, ciphertext []byte) ([]byte, error) {
	aead, err := NewAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("integrity: AEAD nonce wrong length")
	}
	out, err := aead.Open(dst, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrAEADDecrypt
	}
	return out, nil
}
