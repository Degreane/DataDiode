// Package framing encodes and decodes the v2 on-wire frame format.
//
// See docs/architecture/ADR-0005-session-protocol.md for the locked spec.
// Two frame types share a common preamble (magic|version|flags|session_id):
//
//   - SOH  (Start of Header): control frame, one per session, carries
//     content_sha256, total_bytes, chunk_total, chunk_size, mode, filename.
//   - DATA: chunk frame, payload up to MaxPayloadLen bytes.
//
// Both frame types end with a trailing SHA-256 over (header+payload),
// plus an HMAC-SHA256 trailer when SIGNED (ADR-0004) — opt-in via the
// caller passing a non-nil key to Encode*/Decode*.
package framing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// Wire-format constants.
const (
	Version       uint8 = 0x02
	HashLen             = sha256.Size
	HMACLen             = sha256.Size
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

	// MaxSOHFrameLen with the largest legal filename.
	MaxNameLen     = 255
	MaxSOHFrameLen = SOHHeaderLen + MaxNameLen + HashLen + HMACLen

	// MaxDATAFrameLen at the maximum payload size, signed.
	MaxDATAFrameLen = DATAHeaderLen + MaxPayloadLen + HashLen + HMACLen

	// MaxFrameLen — the larger of the two; used to size receive buffers.
	MaxFrameLen = MaxDATAFrameLen
)

// MagicBytes is the 4-byte sync marker at the start of every frame.
var MagicBytes = [4]byte{'D', 'D', 'O', 0}

// Flag bits.
const (
	FlagSOH       uint8 = 0x80 // frame is an SOH (control) frame
	FlagFinal     uint8 = 0x40 // DATA only: last chunk in the session
	FlagRedundant uint8 = 0x20 // duplicate copy for loss tolerance
	FlagSigned    uint8 = 0x10 // HMAC-SHA256 appended after sha256 (ADR-0004)

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
	ErrShort          = errors.New("framing: buffer shorter than expected")
	ErrTooLong        = errors.New("framing: buffer longer than MaxFrameLen")
	ErrMagic          = errors.New("framing: bad magic")
	ErrVersion        = errors.New("framing: unsupported version")
	ErrReservedFlags  = errors.New("framing: reserved flag bits set")
	ErrPayloadLen     = errors.New("framing: payload_len exceeds maximum")
	ErrLenMismatch    = errors.New("framing: declared length does not match buffer")
	ErrChunkTotal     = errors.New("framing: chunk_total is zero")
	ErrChunkIndex     = errors.New("framing: chunk_index >= chunk_total")
	ErrNameLen        = errors.New("framing: name_len out of range")
	ErrBadName        = errors.New("framing: name contains '/', '\\\\', NUL, or path traversal")
	ErrHash           = errors.New("framing: SHA-256 mismatch")
	ErrSignedExpected = errors.New("framing: receiver has a key but frame is not SIGNED")
	ErrUnexpectedSign = errors.New("framing: receiver has no key but frame is SIGNED")
	ErrHMACMismatch   = errors.New("framing: HMAC mismatch (wrong key or tampered)")
	ErrFrameType      = errors.New("framing: wrong frame type for this Decode call")
)

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
	Flags         uint8 // REDUNDANT only — SOH/SIGNED are set by Encode
	SessionID     SessionID
	ChunkTotal    uint32
	ChunkSize     uint32 // nominal; last chunk may be smaller
	TotalBytes    uint64
	ContentSHA256 [HashLen]byte
	Mode          uint32 // Unix file mode bits
	Name          string // basename only, no separators, no traversal
}

// EncodeSOH appends a complete SOH frame to dst. When key is non-nil,
// the frame is SIGNED and an HMAC-SHA256 trailer is appended.
func EncodeSOH(dst []byte, s SOH, key []byte) ([]byte, error) {
	if err := validateName(s.Name); err != nil {
		return nil, err
	}
	if len(s.Name) > MaxNameLen {
		return nil, ErrNameLen
	}
	if s.Flags&(FlagSOH|FlagSigned|FlagFinal) != 0 {
		return nil, ErrReservedFlags
	}
	if s.Flags&flagsReserved != 0 {
		return nil, ErrReservedFlags
	}
	if s.ChunkTotal == 0 {
		return nil, ErrChunkTotal
	}

	flags := s.Flags | FlagSOH
	if key != nil {
		flags |= FlagSigned
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
	dst = append(dst, s.Name...)

	sum := sha256.Sum256(dst[start:])
	dst = append(dst, sum[:]...)

	if key != nil {
		mac := hmac.New(sha256.New, key)
		mac.Write(dst[start : len(dst)-HashLen])
		dst = mac.Sum(dst)
	}
	return dst, nil
}

// DecodeSOH parses a buffer as an SOH frame. Returns ErrFrameType if
// the buffer is actually a DATA frame.
func DecodeSOH(src []byte, key []byte) (SOH, error) {
	var s SOH
	// Cheap preamble checks first so a DATA frame returns ErrFrameType
	// instead of ErrShort (callers rely on that to route frames).
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
	if len(src) < SOHHeaderLen+HashLen {
		return s, ErrShort
	}
	if len(src) > MaxSOHFrameLen {
		return s, ErrTooLong
	}
	signed := flags&FlagSigned != 0
	if flags&flagsReserved != 0 {
		return s, ErrReservedFlags
	}
	if err := checkSign(signed, key); err != nil {
		return s, err
	}

	nameLen := int(binary.BigEndian.Uint16(src[74:76]))
	if nameLen == 0 || nameLen > MaxNameLen {
		return s, ErrNameLen
	}
	wantLen := SOHHeaderLen + nameLen + HashLen
	if signed {
		wantLen += HMACLen
	}
	if len(src) != wantLen {
		return s, ErrLenMismatch
	}

	chunkTotal := binary.BigEndian.Uint32(src[22:26])
	if chunkTotal == 0 {
		return s, ErrChunkTotal
	}

	hashStart := SOHHeaderLen + nameLen
	want := sha256.Sum256(src[:hashStart])
	if !hmac.Equal(want[:], src[hashStart:hashStart+HashLen]) {
		return s, ErrHash
	}
	if signed {
		macStart := hashStart + HashLen
		if err := checkMAC(src[:hashStart], src[macStart:macStart+HMACLen], key); err != nil {
			return s, err
		}
	}

	name := string(src[SOHHeaderLen:hashStart])
	if err := validateName(name); err != nil {
		return s, err
	}

	s = SOH{
		Flags:      flags & ^FlagSOH & ^FlagSigned,
		ChunkTotal: chunkTotal,
		ChunkSize:  binary.BigEndian.Uint32(src[26:30]),
		TotalBytes: binary.BigEndian.Uint64(src[30:38]),
		Mode:       binary.BigEndian.Uint32(src[70:74]),
		Name:       name,
	}
	copy(s.SessionID[:], src[6:22])
	copy(s.ContentSHA256[:], src[38:70])
	return s, nil
}

// ----- DATA frame ----------------------------------------------------------

// DATA is a single chunk in a session.
type DATA struct {
	Flags      uint8 // REDUNDANT and/or FINAL — SOH/SIGNED are set by Encode
	SessionID  SessionID
	ChunkIndex uint32
}

// EncodeDATA appends a complete DATA frame to dst. payload may be empty
// (a FINAL-only signal); usually it's up to MaxPayloadLen bytes.
func EncodeDATA(dst []byte, d DATA, payload []byte, key []byte) ([]byte, error) {
	if len(payload) > MaxPayloadLen {
		return nil, ErrPayloadLen
	}
	if d.Flags&(FlagSOH|FlagSigned) != 0 {
		return nil, ErrReservedFlags
	}
	if d.Flags&flagsReserved != 0 {
		return nil, ErrReservedFlags
	}

	flags := d.Flags // SOH stays clear
	if key != nil {
		flags |= FlagSigned
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
	dst = append(dst, payload...)

	sum := sha256.Sum256(dst[start:])
	dst = append(dst, sum[:]...)

	if key != nil {
		mac := hmac.New(sha256.New, key)
		mac.Write(dst[start : len(dst)-HashLen])
		dst = mac.Sum(dst)
	}
	return dst, nil
}

// DecodeDATA parses a buffer as a DATA frame.
func DecodeDATA(src []byte, key []byte) (DATA, []byte, error) {
	var d DATA
	if len(src) < DATAHeaderLen+HashLen {
		return d, nil, ErrShort
	}
	if len(src) > MaxDATAFrameLen {
		return d, nil, ErrTooLong
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
	signed := flags&FlagSigned != 0
	if flags&flagsReserved != 0 {
		return d, nil, ErrReservedFlags
	}
	if err := checkSign(signed, key); err != nil {
		return d, nil, err
	}

	payloadLen := int(binary.BigEndian.Uint16(src[26:28]))
	if payloadLen > MaxPayloadLen {
		return d, nil, ErrPayloadLen
	}
	wantLen := DATAHeaderLen + payloadLen + HashLen
	if signed {
		wantLen += HMACLen
	}
	if len(src) != wantLen {
		return d, nil, ErrLenMismatch
	}

	hashStart := DATAHeaderLen + payloadLen
	want := sha256.Sum256(src[:hashStart])
	if !hmac.Equal(want[:], src[hashStart:hashStart+HashLen]) {
		return d, nil, ErrHash
	}
	if signed {
		macStart := hashStart + HashLen
		if err := checkMAC(src[:hashStart], src[macStart:macStart+HMACLen], key); err != nil {
			return d, nil, err
		}
	}

	d = DATA{
		Flags:      flags & ^FlagSigned,
		ChunkIndex: binary.BigEndian.Uint32(src[22:26]),
	}
	copy(d.SessionID[:], src[6:22])
	payload := src[DATAHeaderLen:hashStart]
	return d, payload, nil
}

// IsFinal / IsRedundant accessors on DATA flags.
func (d DATA) IsFinal() bool     { return d.Flags&FlagFinal != 0 }
func (d DATA) IsRedundant() bool { return d.Flags&FlagRedundant != 0 }

// IsRedundant accessor on SOH flags.
func (s SOH) IsRedundant() bool { return s.Flags&FlagRedundant != 0 }

// ----- helpers -------------------------------------------------------------

func checkSign(signed bool, key []byte) error {
	if signed && key == nil {
		return ErrUnexpectedSign
	}
	if !signed && key != nil {
		return ErrSignedExpected
	}
	return nil
}

func checkMAC(signedBytes, gotMAC, key []byte) error {
	mac := hmac.New(sha256.New, key)
	mac.Write(signedBytes)
	if !hmac.Equal(mac.Sum(nil), gotMAC) {
		return ErrHMACMismatch
	}
	return nil
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
