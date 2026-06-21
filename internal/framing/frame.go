// Package framing encodes and decodes the v3 on-wire frame format.
//
// See docs/architecture/ADR-0005-session-protocol.md (v2 session
// protocol, unchanged on top) and ADR-0008-aead-encryption.md (v3
// AEAD swap, this file). Two frame types share a common preamble
// (magic | ver=3 | flags | session_id):
//
//   - SOH  (Start of Header): one per session; carries content_sha256,
//     total_bytes, chunk_total, chunk_size, mode, filename.
//   - DATA: a single chunk; payload up to MaxPayloadLen bytes.
//
// Keyed (FlagEncrypted set) vs unkeyed (FlagEncrypted clear) are two
// wire shapes selectable per-frame via the caller passing a non-nil
// key. Unkeyed frames carry a trailing SHA-256 for integrity. Keyed
// frames carry an AES-256-GCM tag (16 B) instead — GCM's tag covers
// both integrity and authentication, so there is no separate sha256
// trailer in the keyed shape. The receiver rejects mismatched
// expectations (keyed receiver seeing an unkeyed frame, or vice
// versa).
package framing

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/degreane/datadiode/internal/integrity"
)

// Wire-format constants.
const (
	Version       uint8 = 0x03 // ADR-0008: v3 introduces AEAD
	HashLen             = sha256.Size
	AEADTagLen          = integrity.AEADTagLen
	MaxPayloadLen       = 1400
	SessionIDLen        = 16

	// Common preamble: magic(4) + ver(1) + flags(1) + session_id(16) = 22 bytes.
	preambleLen = 22

	// SOH header fixed portion: preamble + chunk_total(4) + chunk_size(4) +
	// total_bytes(8) + content_sha256(32) + mode(4) + name_len(2) = 76 bytes
	// before the variable-length name.
	SOHHeaderLen = preambleLen + 4 + 4 + 8 + HashLen + 4 + 2

	// DATA header fixed portion: preamble + chunk_index(4) + payload_len(2) = 28 bytes.
	DATAHeaderLen = preambleLen + 4 + 2

	// Upper bounds across both unkeyed (sha256 trailer) and keyed
	// (AEAD tag) shapes. Used to size receiver buffers.
	MaxNameLen      = 255
	MaxSOHFrameLen  = SOHHeaderLen + MaxNameLen + HashLen // unkeyed is the larger (32 > 16)
	MaxDATAFrameLen = DATAHeaderLen + MaxPayloadLen + HashLen

	// MaxFrameLen — used to size receive buffers.
	MaxFrameLen = MaxDATAFrameLen
)

// MagicBytes is the 4-byte sync marker at the start of every frame.
var MagicBytes = [4]byte{'D', 'D', 'O', 0}

// Flag bits.
const (
	FlagSOH       uint8 = 0x80 // frame is an SOH (control) frame
	FlagFinal     uint8 = 0x40 // DATA only: last chunk in the session
	FlagRedundant uint8 = 0x20 // duplicate copy for loss tolerance
	FlagEncrypted uint8 = 0x10 // payload is AES-256-GCM encrypted (ADR-0008)

	flagsReserved uint8 = 0x0F
)

// SessionID is a 128-bit unique session identifier.
type SessionID [SessionIDLen]byte

// String returns the canonical UUID-style hex form (8-4-4-4-12).
func (s SessionID) String() string {
	const hex = "0123456789abcdef"
	var out [36]byte
	src := s[:]
	j := 0
	for i := 0; i < 16; i++ {
		out[j] = hex[src[i]>>4]
		out[j+1] = hex[src[i]&0x0F]
		j += 2
		switch i {
		case 3, 5, 7, 9:
			out[j] = '-'
			j++
		}
	}
	return string(out[:])
}

// Sentinel errors.
var (
	ErrShort               = errors.New("framing: buffer shorter than expected")
	ErrTooLong             = errors.New("framing: buffer longer than MaxFrameLen")
	ErrMagic               = errors.New("framing: bad magic")
	ErrVersion             = errors.New("framing: unsupported version")
	ErrReservedFlags       = errors.New("framing: reserved flag bits set")
	ErrPayloadLen          = errors.New("framing: payload_len exceeds maximum")
	ErrLenMismatch         = errors.New("framing: declared length does not match buffer")
	ErrChunkTotal          = errors.New("framing: chunk_total is zero")
	ErrChunkIndex          = errors.New("framing: chunk_index >= chunk_total")
	ErrNameLen             = errors.New("framing: name_len out of range")
	ErrBadName             = errors.New("framing: name contains '/', '\\\\', NUL, or path traversal")
	ErrHash                = errors.New("framing: SHA-256 mismatch")
	ErrEncryptedExpected   = errors.New("framing: receiver has a key but frame is not ENCRYPTED")
	ErrUnexpectedEncrypted = errors.New("framing: receiver has no key but frame is ENCRYPTED")
	ErrDecryptFailed       = errors.New("framing: AEAD decrypt failed (wrong key or tampered)")
	ErrFrameType           = errors.New("framing: wrong frame type for this Decode call")
)

// nonceForDATA constructs the deterministic GCM nonce for a DATA frame
// per ADR-0008: nonce[0:8] = session_id[0:8], nonce[8:12] = chunk_index BE.
func nonceForDATA(sid SessionID, chunkIndex uint32) []byte {
	var n [integrity.AEADNonceLen]byte
	copy(n[0:8], sid[0:8])
	binary.BigEndian.PutUint32(n[8:12], chunkIndex)
	return n[:]
}

// nonceForSOH uses the reserved sentinel 0xFFFFFFFF in the last 4 bytes.
func nonceForSOH(sid SessionID) []byte {
	var n [integrity.AEADNonceLen]byte
	copy(n[0:8], sid[0:8])
	binary.BigEndian.PutUint32(n[8:12], 0xFFFFFFFF)
	return n[:]
}

// ----- common preamble -----------------------------------------------------

// PeekKind inspects a buffer's preamble (without full validation) to
// answer "is this an SOH or a DATA frame, and what's its session_id?".
// Used by the receiver's session router to do early reject of frames
// for unknown sessions WITHOUT decoding the rest. Returns false if the
// buffer is too short or doesn't pass the magic+version check.
func PeekKind(src []byte) (isSOH bool, sid SessionID, ok bool) {
	if len(src) < preambleLen {
		return false, sid, false
	}
	if [4]byte{src[0], src[1], src[2], src[3]} != MagicBytes {
		return false, sid, false
	}
	if src[4] != Version {
		return false, sid, false
	}
	isSOH = src[5]&FlagSOH != 0
	copy(sid[:], src[6:22])
	return isSOH, sid, true
}

// ----- SOH frame -----------------------------------------------------------

// SOH carries session metadata. All fields are set by the caller of
// EncodeSOH; Encode sets version + flags itself.
type SOH struct {
	Flags         uint8 // REDUNDANT only — SOH and ENCRYPTED are set by Encode
	SessionID     SessionID
	ChunkTotal    uint32
	ChunkSize     uint32 // nominal; last chunk may be smaller
	TotalBytes    uint64
	ContentSHA256 [HashLen]byte
	Mode          uint32 // Unix file mode bits
	Name          string // basename only, no separators, no traversal
}

// EncodeSOH appends a complete SOH frame to dst. When aeadKey is non-nil
// the filename bytes are AES-256-GCM-encrypted with the header as AAD
// and a 16-byte GCM tag is appended; no separate SHA-256 trailer is
// emitted. When aeadKey is nil the filename is plaintext and a
// trailing SHA-256 covers (header + name).
//
// Callers should not pre-set FlagSOH, FlagEncrypted, or FlagFinal in
// s.Flags — only REDUNDANT may be pre-set.
func EncodeSOH(dst []byte, s SOH, aeadKey []byte) ([]byte, error) {
	if err := validateName(s.Name); err != nil {
		return nil, err
	}
	if len(s.Name) > MaxNameLen {
		return nil, ErrNameLen
	}
	if s.Flags&(FlagSOH|FlagEncrypted|FlagFinal) != 0 {
		return nil, ErrReservedFlags
	}
	if s.Flags&flagsReserved != 0 {
		return nil, ErrReservedFlags
	}
	if s.ChunkTotal == 0 {
		return nil, ErrChunkTotal
	}

	flags := s.Flags | FlagSOH
	if aeadKey != nil {
		flags |= FlagEncrypted
	}

	start := len(dst)
	header := make([]byte, SOHHeaderLen)
	copy(header[0:4], MagicBytes[:])
	header[4] = Version
	header[5] = flags
	copy(header[6:22], s.SessionID[:])
	binary.BigEndian.PutUint32(header[22:26], s.ChunkTotal)
	binary.BigEndian.PutUint32(header[26:30], s.ChunkSize)
	binary.BigEndian.PutUint64(header[30:38], s.TotalBytes)
	copy(header[38:70], s.ContentSHA256[:])
	binary.BigEndian.PutUint32(header[70:74], s.Mode)
	binary.BigEndian.PutUint16(header[74:76], uint16(len(s.Name)))
	dst = append(dst, header...)

	if aeadKey == nil {
		// Unkeyed shape: name in plaintext, trailing sha256.
		dst = append(dst, s.Name...)
		sum := sha256.Sum256(dst[start:])
		dst = append(dst, sum[:]...)
	} else {
		// Keyed shape: AEAD-seal name with header as AAD.
		nonce := nonceForSOH(s.SessionID)
		sealed, err := integrity.AEADSeal(nil, aeadKey, nonce, dst[start:start+SOHHeaderLen], []byte(s.Name))
		if err != nil {
			return nil, err
		}
		dst = append(dst, sealed...)
	}
	return dst, nil
}

// DecodeSOH parses a buffer as an SOH frame. Returns ErrFrameType if
// the buffer is actually a DATA frame.
//
// Auth policy (ADR-0008):
//   - key == nil and frame is unencrypted: accepted (sha256 verified).
//   - key == nil and frame is ENCRYPTED:   rejected (ErrUnexpectedEncrypted).
//   - key != nil and frame is ENCRYPTED:   accepted iff AEAD verifies.
//   - key != nil and frame is unencrypted: rejected (ErrEncryptedExpected).
func DecodeSOH(src []byte, aeadKey []byte) (SOH, error) {
	var s SOH
	if len(src) < preambleLen {
		return s, ErrShort
	}
	if [4]byte{src[0], src[1], src[2], src[3]} != MagicBytes {
		return s, ErrMagic
	}
	if src[4] != Version {
		return s, ErrVersion
	}
	flags := src[5]
	if flags&FlagSOH == 0 {
		return s, ErrFrameType
	}
	if len(src) < SOHHeaderLen {
		return s, ErrShort
	}
	if len(src) > MaxSOHFrameLen {
		return s, ErrTooLong
	}
	encrypted := flags&FlagEncrypted != 0
	if flags&flagsReserved != 0 {
		return s, ErrReservedFlags
	}
	if err := checkAuthFlag(encrypted, aeadKey); err != nil {
		return s, err
	}

	nameLen := int(binary.BigEndian.Uint16(src[74:76]))
	if nameLen == 0 || nameLen > MaxNameLen {
		return s, ErrNameLen
	}
	wantLen := SOHHeaderLen + nameLen
	if encrypted {
		wantLen += AEADTagLen
	} else {
		wantLen += HashLen
	}
	if len(src) != wantLen {
		return s, ErrLenMismatch
	}

	chunkTotal := binary.BigEndian.Uint32(src[22:26])
	if chunkTotal == 0 {
		return s, ErrChunkTotal
	}

	var name string
	copy(s.SessionID[:], src[6:22])
	if !encrypted {
		hashStart := SOHHeaderLen + nameLen
		want := sha256.Sum256(src[:hashStart])
		if !constantTimeEqual(want[:], src[hashStart:hashStart+HashLen]) {
			return s, ErrHash
		}
		name = string(src[SOHHeaderLen:hashStart])
	} else {
		nonce := nonceForSOH(s.SessionID)
		ct := src[SOHHeaderLen:]
		pt, err := integrity.AEADOpen(nil, aeadKey, nonce, src[:SOHHeaderLen], ct)
		if err != nil {
			return s, ErrDecryptFailed
		}
		name = string(pt)
	}
	if err := validateName(name); err != nil {
		return s, err
	}

	s.Flags = flags & ^FlagSOH & ^FlagEncrypted
	s.ChunkTotal = chunkTotal
	s.ChunkSize = binary.BigEndian.Uint32(src[26:30])
	s.TotalBytes = binary.BigEndian.Uint64(src[30:38])
	s.Mode = binary.BigEndian.Uint32(src[70:74])
	s.Name = name
	copy(s.ContentSHA256[:], src[38:70])
	return s, nil
}

// ----- DATA frame ----------------------------------------------------------

// DATA is a single chunk in a session.
type DATA struct {
	Flags      uint8 // REDUNDANT and/or FINAL — SOH and ENCRYPTED are set by Encode
	SessionID  SessionID
	ChunkIndex uint32
}

// EncodeDATA appends a complete DATA frame to dst. When aeadKey is
// non-nil the payload is encrypted with the header as AAD and a 16-byte
// GCM tag is appended (no separate sha256). When aeadKey is nil the
// payload is plaintext followed by a sha256 trailer over (header+payload).
func EncodeDATA(dst []byte, d DATA, payload []byte, aeadKey []byte) ([]byte, error) {
	if len(payload) > MaxPayloadLen {
		return nil, ErrPayloadLen
	}
	if d.Flags&(FlagSOH|FlagEncrypted) != 0 {
		return nil, ErrReservedFlags
	}
	if d.Flags&flagsReserved != 0 {
		return nil, ErrReservedFlags
	}

	flags := d.Flags
	if aeadKey != nil {
		flags |= FlagEncrypted
	}

	start := len(dst)
	header := make([]byte, DATAHeaderLen)
	copy(header[0:4], MagicBytes[:])
	header[4] = Version
	header[5] = flags
	copy(header[6:22], d.SessionID[:])
	binary.BigEndian.PutUint32(header[22:26], d.ChunkIndex)
	binary.BigEndian.PutUint16(header[26:28], uint16(len(payload)))
	dst = append(dst, header...)

	if aeadKey == nil {
		dst = append(dst, payload...)
		sum := sha256.Sum256(dst[start:])
		dst = append(dst, sum[:]...)
	} else {
		nonce := nonceForDATA(d.SessionID, d.ChunkIndex)
		sealed, err := integrity.AEADSeal(nil, aeadKey, nonce, dst[start:start+DATAHeaderLen], payload)
		if err != nil {
			return nil, err
		}
		dst = append(dst, sealed...)
	}
	return dst, nil
}

// DecodeDATA parses a buffer as a DATA frame.
//
// On success returns the parsed header and a payload slice. The
// returned slice aliases src for the unkeyed path and is a freshly-
// allocated decrypt buffer for the keyed path.
func DecodeDATA(src []byte, aeadKey []byte) (DATA, []byte, error) {
	var d DATA
	if len(src) < preambleLen {
		return d, nil, ErrShort
	}
	if [4]byte{src[0], src[1], src[2], src[3]} != MagicBytes {
		return d, nil, ErrMagic
	}
	if src[4] != Version {
		return d, nil, ErrVersion
	}
	flags := src[5]
	if flags&FlagSOH != 0 {
		return d, nil, ErrFrameType
	}
	if len(src) < DATAHeaderLen {
		return d, nil, ErrShort
	}
	if len(src) > MaxDATAFrameLen+AEADTagLen { // generous: AEAD adds 16, sha adds 32
		return d, nil, ErrTooLong
	}
	encrypted := flags&FlagEncrypted != 0
	if flags&flagsReserved != 0 {
		return d, nil, ErrReservedFlags
	}
	if err := checkAuthFlag(encrypted, aeadKey); err != nil {
		return d, nil, err
	}

	payloadLen := int(binary.BigEndian.Uint16(src[26:28]))
	if payloadLen > MaxPayloadLen {
		return d, nil, ErrPayloadLen
	}
	wantLen := DATAHeaderLen + payloadLen
	if encrypted {
		wantLen += AEADTagLen
	} else {
		wantLen += HashLen
	}
	if len(src) != wantLen {
		return d, nil, ErrLenMismatch
	}

	d.Flags = flags & ^FlagEncrypted
	d.ChunkIndex = binary.BigEndian.Uint32(src[22:26])
	copy(d.SessionID[:], src[6:22])

	var payload []byte
	if !encrypted {
		hashStart := DATAHeaderLen + payloadLen
		want := sha256.Sum256(src[:hashStart])
		if !constantTimeEqual(want[:], src[hashStart:hashStart+HashLen]) {
			return d, nil, ErrHash
		}
		payload = src[DATAHeaderLen:hashStart]
	} else {
		nonce := nonceForDATA(d.SessionID, d.ChunkIndex)
		ct := src[DATAHeaderLen:]
		pt, err := integrity.AEADOpen(nil, aeadKey, nonce, src[:DATAHeaderLen], ct)
		if err != nil {
			return d, nil, ErrDecryptFailed
		}
		payload = pt
	}
	return d, payload, nil
}

// IsFinal / IsRedundant accessors on DATA flags.
func (d DATA) IsFinal() bool     { return d.Flags&FlagFinal != 0 }
func (d DATA) IsRedundant() bool { return d.Flags&FlagRedundant != 0 }

// IsRedundant accessor on SOH flags.
func (s SOH) IsRedundant() bool { return s.Flags&FlagRedundant != 0 }

// ----- helpers -------------------------------------------------------------

func checkAuthFlag(encrypted bool, key []byte) error {
	if encrypted && key == nil {
		return ErrUnexpectedEncrypted
	}
	if !encrypted && key != nil {
		return ErrEncryptedExpected
	}
	return nil
}

func constantTimeEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func validateName(name string) error {
	if name == "" || name == "." || name == ".." {
		return ErrBadName
	}
	if len(name) > MaxNameLen {
		return ErrNameLen
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '/' || c == '\\' || c == 0 {
			return ErrBadName
		}
	}
	return nil
}
