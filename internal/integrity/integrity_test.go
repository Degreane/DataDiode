package integrity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// TestHash_GoldenVectors pins our Hash against the canonical SHA-256
// test vectors. If this fails we've either changed the algorithm or
// broken the wrapper — both should be loud failures.
func TestHash_GoldenVectors(t *testing.T) {
	cases := []struct {
		in   string
		want string // hex, lower case
	}{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"The quick brown fox jumps over the lazy dog",
			"d7a8fbb307d7809469ca9abcb0082e4f8d5651e46d3cdb762d02d0bf37c9e592"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := Hash([]byte(tc.in))
			if got.String() != tc.want {
				t.Fatalf("Hash(%q):\n got  %s\n want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestDigest_BytesAliases(t *testing.T) {
	d := Hash([]byte("x"))
	if &d.Bytes()[0] != &d[0] {
		t.Fatalf("Bytes() should alias underlying array")
	}
}

func TestDigest_StringHex(t *testing.T) {
	d := Hash(nil)
	if _, err := hex.DecodeString(d.String()); err != nil {
		t.Fatalf("String not valid hex: %v", err)
	}
	if strings.ToLower(d.String()) != d.String() {
		t.Fatalf("String should be lower case")
	}
}

func TestEqual_TrueAndFalse(t *testing.T) {
	a := Hash([]byte("hello"))
	b := Hash([]byte("hello"))
	c := Hash([]byte("world"))
	if !Equal(a, b) {
		t.Fatalf("Equal: identical digests should compare equal")
	}
	if Equal(a, c) {
		t.Fatalf("Equal: different digests should compare unequal")
	}
}

func TestVerify_Match(t *testing.T) {
	payload := []byte("payload")
	if err := Verify(payload, Hash(payload)); err != nil {
		t.Fatalf("Verify(matching): %v", err)
	}
}

func TestVerify_Mismatch(t *testing.T) {
	err := Verify([]byte("payload"), Hash([]byte("different")))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("err: got %v, want %v", err, ErrMismatch)
	}
}

// Streaming Hasher should give the same result as one-shot Hash for the
// same total bytes, regardless of how the input is chunked.
func TestHasher_StreamingMatchesOneShot(t *testing.T) {
	full := bytes.Repeat([]byte("abc"), 1024) // 3 KiB
	want := Hash(full)

	chunkSizes := []int{1, 7, 32, 256, 4096}
	for _, n := range chunkSizes {
		h := NewSHA256()
		for i := 0; i < len(full); i += n {
			end := min(i+n, len(full))
			if _, err := h.Write(full[i:end]); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		got := h.Sum()
		if !Equal(got, want) {
			t.Fatalf("chunk=%d: streaming digest %s != one-shot %s", n, got, want)
		}
	}
}

// Sum must reset the hasher so the next message starts clean.
func TestHasher_SumResets(t *testing.T) {
	h := NewSHA256()
	_, _ = h.Write([]byte("first"))
	_ = h.Sum()

	_, _ = h.Write([]byte("second"))
	got := h.Sum()
	want := Hash([]byte("second"))
	if !Equal(got, want) {
		t.Fatalf("Sum did not reset:\n got  %s\n want %s", got, want)
	}
}

// One-shot Hash must match stdlib sha256 exactly. Cross-check guards
// against the wrapper introducing any transformation.
func TestHash_MatchesStdlib(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("a"), bytes.Repeat([]byte{0xAB}, 4096)} {
		want := sha256.Sum256(in)
		got := Hash(in)
		if !bytes.Equal(got[:], want[:]) {
			t.Fatalf("Hash differs from sha256.Sum256 for input of len %d", len(in))
		}
	}
}

// ----- Benchmarks ---------------------------------------------------------

func BenchmarkHash_64KiB(b *testing.B) {
	data := bytes.Repeat([]byte{0xAB}, 64<<10)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		_ = Hash(data)
	}
}

func BenchmarkVerify_64KiB(b *testing.B) {
	data := bytes.Repeat([]byte{0xAB}, 64<<10)
	want := Hash(data)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		_ = Verify(data, want)
	}
}
