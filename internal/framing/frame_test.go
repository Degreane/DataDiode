package framing

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// helper: build a known-good frame for a given payload size.
func mustEncode(t *testing.T, h Header, payload []byte) []byte {
	t.Helper()
	buf, err := Encode(nil, h, payload, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return buf
}

func TestRoundTrip_Empty(t *testing.T) {
	h := Header{ChunkIndex: 0, ChunkTotal: 1, Flags: FlagFinal | FlagHeartbeat, Seq: 1, MsgID: 7}
	buf := mustEncode(t, h, nil)

	if len(buf) != MinFrameLen {
		t.Fatalf("empty frame length: got %d, want %d", len(buf), MinFrameLen)
	}

	got, payload, err := Decode(buf, nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(payload) != 0 {
		t.Fatalf("payload: got %d bytes, want 0", len(payload))
	}
	want := h
	want.Version = Version
	want.PayloadLen = 0
	if got != want {
		t.Fatalf("header mismatch:\n got  %+v\n want %+v", got, want)
	}
}

func TestRoundTrip_MaxPayload(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, MaxPayloadLen)
	h := Header{ChunkIndex: 2, ChunkTotal: 3, Flags: 0, Seq: 99, MsgID: 42}
	buf := mustEncode(t, h, payload)

	if len(buf) != MaxFrameLen {
		t.Fatalf("max frame length: got %d, want %d", len(buf), MaxFrameLen)
	}

	gotHdr, gotPayload, err := Decode(buf, nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload mismatch")
	}
	if gotHdr.PayloadLen != MaxPayloadLen {
		t.Fatalf("PayloadLen: got %d, want %d", gotHdr.PayloadLen, MaxPayloadLen)
	}
}

func TestRoundTrip_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		h       Header
		payload []byte
	}{
		{"single-byte", Header{ChunkTotal: 1, Flags: FlagFinal}, []byte{0x55}},
		{"all-flags-allowed", Header{ChunkTotal: 1, Flags: FlagFinal | FlagHeartbeat | FlagRedundant}, []byte("hi")},
		{"chunk-mid", Header{ChunkIndex: 4, ChunkTotal: 10, Seq: 1 << 40, MsgID: 1 << 20}, []byte("middle chunk")},
		{"binary-payload", Header{ChunkTotal: 1, Flags: FlagFinal}, []byte{0, 1, 2, 3, 0xff, 0xfe, 0xfd}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := mustEncode(t, tc.h, tc.payload)
			h, p, err := Decode(buf, nil)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !bytes.Equal(p, tc.payload) {
				t.Fatalf("payload mismatch")
			}
			want := tc.h
			want.Version = Version
			want.PayloadLen = uint32(len(tc.payload))
			if h != want {
				t.Fatalf("header:\n got  %+v\n want %+v", h, want)
			}
		})
	}
}

// IsFinal/IsHeartbeat/IsRedundant accessors.
func TestHeaderAccessors(t *testing.T) {
	h := Header{Flags: FlagFinal | FlagRedundant}
	if !h.IsFinal() || h.IsHeartbeat() || !h.IsRedundant() {
		t.Fatalf("accessors wrong: final=%v heartbeat=%v redundant=%v",
			h.IsFinal(), h.IsHeartbeat(), h.IsRedundant())
	}
}

// ---- Encode error cases ---------------------------------------------------

func TestEncode_PayloadTooLarge(t *testing.T) {
	_, err := Encode(nil, Header{ChunkTotal: 1}, make([]byte, MaxPayloadLen+1), nil)
	if !errors.Is(err, ErrPayloadLen) {
		t.Fatalf("err: got %v, want %v", err, ErrPayloadLen)
	}
}

func TestEncode_ReservedFlags(t *testing.T) {
	_, err := Encode(nil, Header{ChunkTotal: 1, Flags: 0x01}, nil, nil)
	if !errors.Is(err, ErrReservedFlags) {
		t.Fatalf("err: got %v, want %v", err, ErrReservedFlags)
	}
}

func TestEncode_ChunkTotalZero(t *testing.T) {
	_, err := Encode(nil, Header{ChunkTotal: 0}, nil, nil)
	if !errors.Is(err, ErrChunkTotal) {
		t.Fatalf("err: got %v, want %v", err, ErrChunkTotal)
	}
}

func TestEncode_ChunkIndexOutOfRange(t *testing.T) {
	_, err := Encode(nil, Header{ChunkIndex: 5, ChunkTotal: 5}, nil, nil)
	if !errors.Is(err, ErrChunkIndex) {
		t.Fatalf("err: got %v, want %v", err, ErrChunkIndex)
	}
}

// ---- Decode error cases ---------------------------------------------------

func TestDecode_TooShort(t *testing.T) {
	_, _, err := Decode(make([]byte, MinFrameLen-1), nil)
	if !errors.Is(err, ErrShort) {
		t.Fatalf("err: got %v, want %v", err, ErrShort)
	}
}

func TestDecode_TooLong(t *testing.T) {
	_, _, err := Decode(make([]byte, MaxFrameLen+HMACLen+1), nil)
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("err: got %v, want %v", err, ErrTooLong)
	}
}

func TestDecode_BadMagic(t *testing.T) {
	buf := mustEncode(t, Header{ChunkTotal: 1}, nil)
	buf[0] ^= 0xFF
	_, _, err := Decode(buf, nil)
	if !errors.Is(err, ErrMagic) {
		t.Fatalf("err: got %v, want %v", err, ErrMagic)
	}
}

func TestDecode_BadVersion(t *testing.T) {
	buf := mustEncode(t, Header{ChunkTotal: 1}, nil)
	buf[4] = 0xFF
	_, _, err := Decode(buf, nil)
	if !errors.Is(err, ErrVersion) {
		t.Fatalf("err: got %v, want %v", err, ErrVersion)
	}
}

func TestDecode_ReservedFlagsBits(t *testing.T) {
	buf := mustEncode(t, Header{ChunkTotal: 1}, nil)
	buf[5] |= 0x01 // set a reserved bit
	_, _, err := Decode(buf, nil)
	if !errors.Is(err, ErrReservedFlags) {
		t.Fatalf("err: got %v, want %v", err, ErrReservedFlags)
	}
}

func TestDecode_PayloadLenTooBig(t *testing.T) {
	buf := mustEncode(t, Header{ChunkTotal: 1}, []byte("x"))
	// Overwrite payload_len to MaxPayloadLen+1 (still inside MaxFrameLen check).
	binary.BigEndian.PutUint32(buf[22:26], MaxPayloadLen+1)
	_, _, err := Decode(buf, nil)
	if !errors.Is(err, ErrPayloadLen) {
		t.Fatalf("err: got %v, want %v", err, ErrPayloadLen)
	}
}

func TestDecode_LenMismatch(t *testing.T) {
	buf := mustEncode(t, Header{ChunkTotal: 1}, []byte("hello"))
	// Lie about payload_len.
	binary.BigEndian.PutUint32(buf[22:26], 4)
	_, _, err := Decode(buf, nil)
	if !errors.Is(err, ErrLenMismatch) {
		t.Fatalf("err: got %v, want %v", err, ErrLenMismatch)
	}
}

func TestDecode_ChunkTotalZero(t *testing.T) {
	buf := mustEncode(t, Header{ChunkTotal: 1}, nil)
	binary.BigEndian.PutUint16(buf[20:22], 0)
	// Recompute hash so we hit the chunk_total check, not the hash check.
	rehash(buf)
	_, _, err := Decode(buf, nil)
	if !errors.Is(err, ErrChunkTotal) {
		t.Fatalf("err: got %v, want %v", err, ErrChunkTotal)
	}
}

func TestDecode_ChunkIndexOutOfRange(t *testing.T) {
	buf := mustEncode(t, Header{ChunkIndex: 0, ChunkTotal: 1}, nil)
	binary.BigEndian.PutUint16(buf[18:20], 5) // index >= total
	rehash(buf)
	_, _, err := Decode(buf, nil)
	if !errors.Is(err, ErrChunkIndex) {
		t.Fatalf("err: got %v, want %v", err, ErrChunkIndex)
	}
}

func TestDecode_HashMismatch(t *testing.T) {
	buf := mustEncode(t, Header{ChunkTotal: 1}, []byte("tamper me"))
	// Flip a byte in the payload without recomputing the hash.
	buf[HeaderLen] ^= 0xFF
	_, _, err := Decode(buf, nil)
	if !errors.Is(err, ErrHash) {
		t.Fatalf("err: got %v, want %v", err, ErrHash)
	}
}

// rehash recomputes the trailing SHA-256 in place. Used by tests that
// intentionally tamper with header fields and need to bypass the hash
// check to exercise a different validation rule.
func rehash(buf []byte) {
	if len(buf) < MinFrameLen {
		return
	}
	hashStart := len(buf) - HashLen
	sum := sha256.Sum256(buf[:hashStart])
	copy(buf[hashStart:], sum[:])
}

// ---- Wire-format invariants ----------------------------------------------

// TestWireGolden pins the exact bytes for a known input so accidental
// format drift is caught loudly. If this test fails, ADR-0002 has been
// violated; either revert the change or supersede the ADR.
func TestWireGolden(t *testing.T) {
	h := Header{
		Flags:      FlagFinal,
		Seq:        0x0102030405060708,
		MsgID:      0x0A0B0C0D,
		ChunkIndex: 0,
		ChunkTotal: 1,
	}
	buf, err := Encode(nil, h, []byte("HI"), nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// 26 header + 2 payload + 32 hash = 60 bytes.
	if len(buf) != 60 {
		t.Fatalf("length: got %d, want 60", len(buf))
	}
	wantHeader := []byte{
		'D', 'D', 'O', 0x00, // magic
		0x01,                                           // version
		0x80,                                           // flags = FINAL
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // seq
		0x0A, 0x0B, 0x0C, 0x0D, // msg_id
		0x00, 0x00, // chunk_index
		0x00, 0x01, // chunk_total
		0x00, 0x00, 0x00, 0x02, // payload_len
	}
	if !bytes.Equal(buf[:HeaderLen], wantHeader) {
		t.Fatalf("header bytes:\n got  %x\n want %x", buf[:HeaderLen], wantHeader)
	}
	if !bytes.Equal(buf[HeaderLen:HeaderLen+2], []byte("HI")) {
		t.Fatalf("payload bytes: got %x", buf[HeaderLen:HeaderLen+2])
	}
	// Hash is deterministic and can be regenerated by the test; assert
	// it's exactly what crypto/sha256 produces over the header+payload.
	wantHash := sha256.Sum256(buf[:HeaderLen+2])
	if !bytes.Equal(buf[HeaderLen+2:], wantHash[:]) {
		t.Fatalf("hash mismatch")
	}
}

// ---- Misc ----------------------------------------------------------------

func TestSentinelErrorsHaveStablePrefix(t *testing.T) {
	for _, e := range []error{
		ErrShort, ErrTooLong, ErrMagic, ErrVersion, ErrReservedFlags,
		ErrPayloadLen, ErrLenMismatch, ErrChunkTotal, ErrChunkIndex, ErrHash,
	} {
		if !strings.HasPrefix(e.Error(), "framing: ") {
			t.Errorf("error %q missing 'framing: ' prefix", e)
		}
	}
}

// ---- Benchmarks ----------------------------------------------------------

func BenchmarkEncode(b *testing.B) {
	payload := bytes.Repeat([]byte{0xAB}, MaxPayloadLen)
	h := Header{ChunkTotal: 1, Flags: FlagFinal, Seq: 1, MsgID: 1}
	dst := make([]byte, 0, MaxFrameLen)
	b.SetBytes(int64(MaxFrameLen))
	for b.Loop() {
		dst = dst[:0]
		_, _ = Encode(dst, h, payload, nil)
	}
}

func BenchmarkDecode(b *testing.B) {
	payload := bytes.Repeat([]byte{0xAB}, MaxPayloadLen)
	h := Header{ChunkTotal: 1, Flags: FlagFinal, Seq: 1, MsgID: 1}
	buf, _ := Encode(nil, h, payload, nil)
	b.SetBytes(int64(len(buf)))
	for b.Loop() {
		_, _, _ = Decode(buf, nil)
	}
}
