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
