// Package fileenv encodes and decodes the v0 file-transfer envelope.
//
// The envelope is an application-layer header carried inside the
// diode's normal frame payload. The transport (ADR-0002) is unaware
// of file semantics; this package exists so `--mode=tx --send-file`
// and `--mode=rx --files-to` can speak a common format.
//
// Wire layout (see docs/sprints/sprint-02-fileops.md):
//
//	offset  size  field
//	   0     4    magic   = "DDF\0"
//	   4     1    version = 0x01
//	   5     1    flags   (reserved, must be 0)
//	   6     2    name_len  (uint16 BE, 1..255)
//	   8     4    mode      (uint32 BE, Unix file mode bits)
//	  12     8    size      (uint64 BE, content byte length)
//	  20    32    sha256    (over content bytes only)
//	  52     N    name      (basename, no '/' or '..')
//	  52+N   S    content
package fileenv

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	Version    uint8 = 0x01
	HeaderLen        = 52
	HashLen          = sha256.Size
	MaxNameLen       = 255
)

var MagicBytes = [4]byte{'D', 'D', 'F', 0}

// Sentinel errors. Decode returns one of these on validation failure;
// callers can errors.Is to react.
var (
	ErrShort         = errors.New("fileenv: buffer shorter than HeaderLen")
	ErrMagic         = errors.New("fileenv: bad magic")
	ErrVersion       = errors.New("fileenv: unsupported version")
	ErrReservedFlags = errors.New("fileenv: reserved flag bits set")
	ErrNameLen       = errors.New("fileenv: name_len out of range")
	ErrSizeMismatch  = errors.New("fileenv: declared size does not match buffer")
	ErrHash          = errors.New("fileenv: sha256 mismatch")
	ErrBadName       = errors.New("fileenv: name contains '/', '\\\\', NUL, or path traversal")
)

// Header is the parsed fixed-size envelope header.
type Header struct {
	Version uint8
	Flags   uint8
	Mode    uint32 // Unix file mode bits (0o777 mask honored on receive)
	Size    uint64 // content length
	Name    string // basename only
	Hash    [HashLen]byte
}

// Encode appends the envelope (header + name + content) to dst and
// returns the extended slice. It validates inputs and refuses to
// build an unsafe envelope.
func Encode(dst []byte, name string, mode uint32, content []byte) ([]byte, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if len(name) > MaxNameLen {
		return nil, fmt.Errorf("%w: %d > %d", ErrNameLen, len(name), MaxNameLen)
	}

	hash := sha256.Sum256(content)

	out := dst
	header := make([]byte, HeaderLen)
	copy(header[0:4], MagicBytes[:])
	header[4] = Version
	header[5] = 0
	binary.BigEndian.PutUint16(header[6:8], uint16(len(name)))
	binary.BigEndian.PutUint32(header[8:12], mode)
	binary.BigEndian.PutUint64(header[12:20], uint64(len(content)))
	copy(header[20:52], hash[:])

	out = append(out, header...)
	out = append(out, name...)
	out = append(out, content...)
	return out, nil
}

// Decode parses an envelope from src. It validates the header, name,
// declared content size, and SHA-256. On success it returns the
// parsed header and a sub-slice of src holding the content (no copy).
func Decode(src []byte) (Header, []byte, error) {
	var h Header

	if len(src) < HeaderLen {
		return h, nil, ErrShort
	}
	if [4]byte{src[0], src[1], src[2], src[3]} != MagicBytes {
		return h, nil, ErrMagic
	}
	if src[4] != Version {
		return h, nil, ErrVersion
	}
	if src[5] != 0 {
		return h, nil, ErrReservedFlags
	}

	nameLen := binary.BigEndian.Uint16(src[6:8])
	if nameLen == 0 || nameLen > MaxNameLen {
		return h, nil, ErrNameLen
	}
	mode := binary.BigEndian.Uint32(src[8:12])
	size := binary.BigEndian.Uint64(src[12:20])

	if uint64(len(src)) != uint64(HeaderLen)+uint64(nameLen)+size {
		return h, nil, ErrSizeMismatch
	}

	nameStart := HeaderLen
	nameEnd := nameStart + int(nameLen)
	name := string(src[nameStart:nameEnd])
	if err := ValidateName(name); err != nil {
		return h, nil, err
	}

	content := src[nameEnd:]
	got := sha256.Sum256(content)
	var want [HashLen]byte
	copy(want[:], src[20:52])
	if got != want {
		return h, nil, ErrHash
	}

	h = Header{
		Version: src[4],
		Flags:   src[5],
		Mode:    mode,
		Size:    size,
		Name:    name,
		Hash:    want,
	}
	return h, content, nil
}

// ValidateName rejects names that would be unsafe to write under an
// arbitrary destination directory. The diode receiver is often run as
// root; a malicious "../../etc/cron.d/anything" must NEVER be honored.
//
// Rules: non-empty, no '/', no '\\', no NUL, no leading '.' that
// resolves to a parent (".." exactly, or any path-component form).
// We restrict to basenames only — directories are out of scope this
// sprint.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrBadName)
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("%w: %d > %d", ErrNameLen, len(name), MaxNameLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '/' || c == '\\' || c == 0 {
			return fmt.Errorf("%w: contains forbidden byte 0x%02x", ErrBadName, c)
		}
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return nil
}
