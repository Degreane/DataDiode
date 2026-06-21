package framing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// Wire-format constants. See docs/architecture/ADR-0002-frame-format.md.
const (
	Version       uint8 = 0x01
	HeaderLen           = 26
	HashLen             = sha256.Size
	MaxPayloadLen       = 1400
	MaxFrameLen         = HeaderLen + MaxPayloadLen + HashLen
	MinFrameLen         = HeaderLen + 0 + HashLen
)

// MagicBytes is the 4-byte sync marker at the start of every frame.
var MagicBytes = [4]byte{'D', 'D', 'O', 0}

// Flag bits in the Header.Flags byte.
const (
	FlagFinal     uint8 = 0x80
	FlagHeartbeat uint8 = 0x40
	FlagRedundant uint8 = 0x20
	FlagSigned    uint8 = 0x10 // ADR-0004: HMAC appended after sha256

	// flagsReserved is the bitmask of bits that MUST be zero in v0.
	// A frame with any of these bits set is rejected by Decode.
	flagsReserved uint8 = 0x0F
)

// HMACLen is the length of the appended HMAC-SHA256 when FlagSigned is set.
const HMACLen = sha256.Size

// Sentinel errors. Callers may errors.Is against these to distinguish
// the validation rule that failed.
var (
	ErrShort          = errors.New("framing: buffer shorter than MinFrameLen")
	ErrTooLong        = errors.New("framing: buffer longer than MaxFrameLen")
	ErrMagic          = errors.New("framing: bad magic")
	ErrVersion        = errors.New("framing: unsupported version")
	ErrReservedFlags  = errors.New("framing: reserved flag bits set")
	ErrPayloadLen     = errors.New("framing: payload_len exceeds maximum")
	ErrLenMismatch    = errors.New("framing: declared length does not match buffer")
	ErrChunkTotal     = errors.New("framing: chunk_total is zero")
	ErrChunkIndex     = errors.New("framing: chunk_index >= chunk_total")
	ErrHash           = errors.New("framing: SHA-256 mismatch")
	ErrSignedExpected = errors.New("framing: receiver has a key but frame is not SIGNED")
	ErrUnexpectedSign = errors.New("framing: receiver has no key but frame is SIGNED")
	ErrHMACMismatch   = errors.New("framing: HMAC mismatch (wrong key or tampered)")
)

// Header is the parsed fixed-size frame header (26 bytes on the wire).
type Header struct {
	Version    uint8
	Flags      uint8
	Seq        uint64
	MsgID      uint32
	ChunkIndex uint16
	ChunkTotal uint16
	PayloadLen uint32
}

// IsFinal reports whether the FINAL flag bit is set.
func (h Header) IsFinal() bool { return h.Flags&FlagFinal != 0 }

// IsHeartbeat reports whether the HEARTBEAT flag bit is set.
func (h Header) IsHeartbeat() bool { return h.Flags&FlagHeartbeat != 0 }

// IsRedundant reports whether the REDUNDANT flag bit is set.
func (h Header) IsRedundant() bool { return h.Flags&FlagRedundant != 0 }

// IsSigned reports whether the SIGNED flag bit is set (frame carries
// an appended HMAC after the SHA-256). See ADR-0004.
func (h Header) IsSigned() bool { return h.Flags&FlagSigned != 0 }

// Encode appends a complete frame (header + payload + SHA-256, plus an
// HMAC-SHA256 trailer when key is non-nil) to dst and returns the
// extended slice. h.Version and h.PayloadLen are set by Encode; callers
// should not pre-fill them. The SIGNED flag is set automatically when
// key is non-nil (callers do not, and MUST NOT, set it themselves).
// dst may be nil; pre-sizing to MaxFrameLen avoids reallocation.
func Encode(dst []byte, h Header, payload []byte, key []byte) ([]byte, error) {
	if len(payload) > MaxPayloadLen {
		return nil, ErrPayloadLen
	}
	// Callers must not pre-set FlagSigned; Encode controls it based on
	// whether a key was supplied. Reject pre-set as a misuse signal.
	if h.Flags&FlagSigned != 0 {
		return nil, ErrReservedFlags
	}
	if h.Flags&flagsReserved != 0 {
		return nil, ErrReservedFlags
	}
	if h.ChunkTotal == 0 {
		return nil, ErrChunkTotal
	}
	if h.ChunkIndex >= h.ChunkTotal {
		return nil, ErrChunkIndex
	}

	h.Version = Version
	h.PayloadLen = uint32(len(payload))
	if key != nil {
		h.Flags |= FlagSigned
	}

	start := len(dst)
	dst = append(dst, make([]byte, HeaderLen)...)
	hdr := dst[start : start+HeaderLen]

	copy(hdr[0:4], MagicBytes[:])
	hdr[4] = h.Version
	hdr[5] = h.Flags
	binary.BigEndian.PutUint64(hdr[6:14], h.Seq)
	binary.BigEndian.PutUint32(hdr[14:18], h.MsgID)
	binary.BigEndian.PutUint16(hdr[18:20], h.ChunkIndex)
	binary.BigEndian.PutUint16(hdr[20:22], h.ChunkTotal)
	binary.BigEndian.PutUint32(hdr[22:26], h.PayloadLen)

	dst = append(dst, payload...)

	sum := sha256.Sum256(dst[start:])
	dst = append(dst, sum[:]...)

	if key != nil {
		mac := hmac.New(sha256.New, key)
		// Sign the same bytes the SHA-256 covers: header+payload.
		// Including the SHA-256 in the MAC would be redundant since the
		// MAC already cryptographically covers everything the SHA does.
		mac.Write(dst[start : start+HeaderLen+int(h.PayloadLen)])
		dst = mac.Sum(dst)
	}

	return dst, nil
}

// Decode parses one frame from src. It verifies, in order: length bounds,
// magic, version, reserved flag bits, payload length, total length
// consistency, chunk fields, the trailing SHA-256, and (when a key is
// supplied) the appended HMAC.
//
// Auth policy (ADR-0004):
//   - key == nil and frame is unsigned: accepted.
//   - key == nil and frame is SIGNED:   rejected (ErrUnexpectedSign).
//   - key != nil and frame is SIGNED:   accepted iff HMAC verifies.
//   - key != nil and frame is unsigned: rejected (ErrSignedExpected).
//
// On success returns the parsed header and a sub-slice of src holding
// the payload (no copy).
func Decode(src []byte, key []byte) (Header, []byte, error) {
	var h Header

	if len(src) < MinFrameLen {
		return h, nil, ErrShort
	}
	if len(src) > MaxFrameLen+HMACLen {
		return h, nil, ErrTooLong
	}

	if [4]byte{src[0], src[1], src[2], src[3]} != MagicBytes {
		return h, nil, ErrMagic
	}
	if src[4] != Version {
		return h, nil, ErrVersion
	}
	flags := src[5]
	signed := flags&FlagSigned != 0
	// Mask the SIGNED bit out before checking reserved bits — SIGNED
	// is *not* reserved any more (it lives in the previously-reserved
	// range), so a frame with only SIGNED set passes this check.
	if (flags & flagsReserved) != 0 {
		return h, nil, ErrReservedFlags
	}

	// Auth policy: enforce key-vs-flag consistency BEFORE other parsing
	// so we don't waste work on a frame we'll reject regardless.
	if signed && key == nil {
		return h, nil, ErrUnexpectedSign
	}
	if !signed && key != nil {
		return h, nil, ErrSignedExpected
	}

	payloadLen := binary.BigEndian.Uint32(src[22:26])
	if payloadLen > MaxPayloadLen {
		return h, nil, ErrPayloadLen
	}

	wantLen := HeaderLen + int(payloadLen) + HashLen
	if signed {
		wantLen += HMACLen
	}
	if wantLen != len(src) {
		return h, nil, ErrLenMismatch
	}

	chunkTotal := binary.BigEndian.Uint16(src[20:22])
	if chunkTotal == 0 {
		return h, nil, ErrChunkTotal
	}
	chunkIndex := binary.BigEndian.Uint16(src[18:20])
	if chunkIndex >= chunkTotal {
		return h, nil, ErrChunkIndex
	}

	hashStart := HeaderLen + int(payloadLen)
	want := sha256.Sum256(src[:hashStart])
	got := src[hashStart : hashStart+HashLen]
	if !bytesEqualConstantTime(want[:], got) {
		return h, nil, ErrHash
	}

	if signed {
		macStart := hashStart + HashLen
		mac := hmac.New(sha256.New, key)
		mac.Write(src[:hashStart]) // sign over header+payload (same scope as sha256)
		expected := mac.Sum(nil)
		if !hmac.Equal(expected, src[macStart:macStart+HMACLen]) {
			return h, nil, ErrHMACMismatch
		}
	}

	h = Header{
		Version:    src[4],
		Flags:      flags,
		Seq:        binary.BigEndian.Uint64(src[6:14]),
		MsgID:      binary.BigEndian.Uint32(src[14:18]),
		ChunkIndex: chunkIndex,
		ChunkTotal: chunkTotal,
		PayloadLen: payloadLen,
	}
	payload := src[HeaderLen:hashStart]
	return h, payload, nil
}

// bytesEqualConstantTime compares two equal-length slices without short-
// circuiting on the first differing byte. SHA-256 is a strong hash so a
// timing attack on the comparison itself is not the dominant risk; using
// constant-time anyway is cheap insurance and keeps the door open for
// switching to a MAC later.
func bytesEqualConstantTime(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
