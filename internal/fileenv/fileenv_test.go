package fileenv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		fname   string
		mode    uint32
		content []byte
	}{
		{"tiny", "hello.txt", 0o644, []byte("hi")},
		{"empty-content", "empty.bin", 0o600, nil},
		{"binary", "blob.bin", 0o755, []byte{0, 1, 2, 0xff, 0xfe}},
		{"large", "big.bin", 0o644, bytes.Repeat([]byte{0xAB}, 100_000)},
		{"max-name", strings.Repeat("a", MaxNameLen), 0o644, []byte("x")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, err := Encode(nil, tc.fname, tc.mode, tc.content)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			h, content, err := Decode(buf)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if h.Name != tc.fname || h.Mode != tc.mode || h.Size != uint64(len(tc.content)) {
				t.Fatalf("header mismatch: got %+v", h)
			}
			if !bytes.Equal(content, tc.content) {
				t.Fatalf("content mismatch")
			}
		})
	}
}

func TestEncode_RejectsBadNames(t *testing.T) {
	bad := []string{
		"",
		".",
		"..",
		"foo/bar",
		"foo\\bar",
		"foo\x00bar",
		strings.Repeat("a", MaxNameLen+1),
	}
	for _, n := range bad {
		t.Run(n, func(t *testing.T) {
			_, err := Encode(nil, n, 0o644, []byte("x"))
			if err == nil {
				t.Fatalf("Encode accepted bad name %q", n)
			}
		})
	}
}

func TestDecode_RejectsTamperedHash(t *testing.T) {
	buf, _ := Encode(nil, "x.bin", 0o644, []byte("hello"))
	// flip a byte in the content
	buf[len(buf)-1] ^= 0xFF
	_, _, err := Decode(buf)
	if !errors.Is(err, ErrHash) {
		t.Fatalf("err: got %v, want %v", err, ErrHash)
	}
}

func TestDecode_RejectsBadMagic(t *testing.T) {
	buf, _ := Encode(nil, "x", 0o644, []byte("y"))
	buf[0] ^= 0xFF
	_, _, err := Decode(buf)
	if !errors.Is(err, ErrMagic) {
		t.Fatalf("err: got %v, want %v", err, ErrMagic)
	}
}

func TestDecode_RejectsBadVersion(t *testing.T) {
	buf, _ := Encode(nil, "x", 0o644, []byte("y"))
	buf[4] = 0xFF
	_, _, err := Decode(buf)
	if !errors.Is(err, ErrVersion) {
		t.Fatalf("err: got %v, want %v", err, ErrVersion)
	}
}

func TestDecode_RejectsReservedFlags(t *testing.T) {
	buf, _ := Encode(nil, "x", 0o644, []byte("y"))
	buf[5] = 0x01
	_, _, err := Decode(buf)
	if !errors.Is(err, ErrReservedFlags) {
		t.Fatalf("err: got %v, want %v", err, ErrReservedFlags)
	}
}

func TestDecode_RejectsSizeMismatch(t *testing.T) {
	buf, _ := Encode(nil, "x.bin", 0o644, []byte("hello"))
	// lie about content size
	binary.BigEndian.PutUint64(buf[12:20], 9999)
	_, _, err := Decode(buf)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err: got %v, want %v", err, ErrSizeMismatch)
	}
}

func TestDecode_RejectsZeroNameLen(t *testing.T) {
	buf, _ := Encode(nil, "x", 0o644, []byte("y"))
	binary.BigEndian.PutUint16(buf[6:8], 0)
	_, _, err := Decode(buf)
	if !errors.Is(err, ErrNameLen) {
		t.Fatalf("err: got %v, want %v", err, ErrNameLen)
	}
}

// Critical security test: a tampered-in-transit name field must be
// caught before any file write happens.
func TestDecode_RejectsPathTraversalInName(t *testing.T) {
	// Build a legitimate envelope, then surgically replace the name with
	// "../../etc/passwd" (also adjusting name_len). Content/hash stay
	// valid so the only thing rejecting this is the name check.
	content := []byte("malicious")
	original := "ok.txt"
	buf, err := Encode(nil, original, 0o644, content)
	if err != nil {
		t.Fatalf("Encode setup: %v", err)
	}
	evil := "../../etc/passwd"
	// reassemble: header (52) + new_name + content
	newBuf := make([]byte, 0, HeaderLen+len(evil)+len(content))
	newBuf = append(newBuf, buf[:HeaderLen]...)
	binary.BigEndian.PutUint16(newBuf[6:8], uint16(len(evil)))
	newBuf = append(newBuf, evil...)
	newBuf = append(newBuf, content...)

	_, _, err = Decode(newBuf)
	if !errors.Is(err, ErrBadName) {
		t.Fatalf("err: got %v, want %v (path traversal MUST be rejected)", err, ErrBadName)
	}
}

func TestValidateName(t *testing.T) {
	good := []string{"a", "foo.bar", "with spaces.txt", strings.Repeat("a", MaxNameLen), "name-with-_.~+chars"}
	for _, n := range good {
		if err := ValidateName(n); err != nil {
			t.Fatalf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
}
