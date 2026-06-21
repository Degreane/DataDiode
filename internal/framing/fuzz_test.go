package framing

import (
	"bytes"
	"testing"
)

// FuzzDecode feeds arbitrary bytes into Decode and asserts the parser
// never panics. Any input that successfully decodes must also roundtrip:
// re-encoding the decoded (header, payload) must equal the original
// buffer exactly. This guards against ambiguous parses where two distinct
// inputs decode to the same header.
func FuzzDecode(f *testing.F) {
	// Seed corpus: a couple of valid frames and some obvious garbage.
	seeds := [][]byte{
		mustFuzzEncode(Header{ChunkTotal: 1, Flags: FlagFinal}, []byte("hello")),
		mustFuzzEncode(Header{ChunkTotal: 1}, nil),
		mustFuzzEncode(Header{ChunkIndex: 1, ChunkTotal: 2, Flags: FlagRedundant},
			bytes.Repeat([]byte{0xCC}, 200)),
		make([]byte, 0),                           // empty
		make([]byte, MinFrameLen),                 // zeroed min-size
		make([]byte, MaxFrameLen),                 // zeroed max-size
		bytes.Repeat([]byte{0xFF}, MinFrameLen+1), // all-ones
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		h, payload, err := Decode(data)
		if err != nil {
			return // expected for most random inputs
		}
		// A successful Decode is a strong claim. Verify roundtrip.
		// Strip the version/payload_len fields that Encode re-derives.
		reH := h
		reH.Version = 0
		reH.PayloadLen = 0
		got, err := Encode(nil, reH, payload)
		if err != nil {
			t.Fatalf("re-encode of valid-decoded frame failed: %v\nheader=%+v payload=%x", err, h, payload)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("roundtrip mismatch:\n in  %x\n out %x", data, got)
		}
	})
}

func mustFuzzEncode(h Header, payload []byte) []byte {
	buf, err := Encode(nil, h, payload)
	if err != nil {
		panic(err)
	}
	return buf
}
