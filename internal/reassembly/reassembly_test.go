package reassembly

import (
	"bytes"
	"errors"
	"testing"

	"github.com/degreane/datadiode/internal/framing"
)

// encode is a tiny test helper that wraps framing.Encode with sane defaults.
func encode(t *testing.T, h framing.Header, payload []byte) []byte {
	t.Helper()
	buf, err := framing.Encode(nil, h, payload)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return buf
}

func newR(t *testing.T) (*Reassembler, *[][]byte) {
	t.Helper()
	var delivered [][]byte
	r, err := New(func(p []byte) error {
		cp := make([]byte, len(p))
		copy(cp, p)
		delivered = append(delivered, cp)
		return nil
	}, DefaultOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, &delivered
}

func TestSingleChunkDelivers(t *testing.T) {
	r, delivered := newR(t)
	payload := []byte("hello")
	frame := encode(t, framing.Header{ChunkTotal: 1, Flags: framing.FlagFinal}, payload)
	if err := r.Ingest(frame); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(*delivered) != 1 || !bytes.Equal((*delivered)[0], payload) {
		t.Fatalf("delivered: %v", *delivered)
	}
	if got := r.Stats().MsgsDelivered; got != 1 {
		t.Fatalf("MsgsDelivered: got %d, want 1", got)
	}
}

func TestMultiChunkInOrder(t *testing.T) {
	r, delivered := newR(t)
	payload := bytes.Repeat([]byte("ABCD"), 25) // 100 bytes
	const chunk = 25
	for i := range 4 {
		flags := uint8(0)
		if i == 3 {
			flags |= framing.FlagFinal
		}
		f := encode(t, framing.Header{
			MsgID: 7, ChunkIndex: uint16(i), ChunkTotal: 4, Flags: flags,
		}, payload[i*chunk:(i+1)*chunk])
		if err := r.Ingest(f); err != nil {
			t.Fatalf("Ingest %d: %v", i, err)
		}
	}
	if len(*delivered) != 1 {
		t.Fatalf("delivered count: %d", len(*delivered))
	}
	if !bytes.Equal((*delivered)[0], payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestMultiChunkOutOfOrder(t *testing.T) {
	r, delivered := newR(t)
	payload := []byte("0123456789")
	chunks := [][]byte{payload[0:3], payload[3:7], payload[7:10]}
	headers := []framing.Header{
		{MsgID: 1, ChunkIndex: 0, ChunkTotal: 3},
		{MsgID: 1, ChunkIndex: 1, ChunkTotal: 3},
		{MsgID: 1, ChunkIndex: 2, ChunkTotal: 3, Flags: framing.FlagFinal},
	}
	// Feed in reverse order.
	for i := 2; i >= 0; i-- {
		if err := r.Ingest(encode(t, headers[i], chunks[i])); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
	}
	if len(*delivered) != 1 || !bytes.Equal((*delivered)[0], payload) {
		t.Fatalf("delivered: %v", *delivered)
	}
}

func TestRedundantChunkOfCompletedMessageDeduped(t *testing.T) {
	// The "recently delivered" cache (default size 1024) prevents a
	// REDUNDANT copy arriving after delivery from re-emitting the
	// message. Without the cache, single-chunk messages with
	// --redundancy>1 would deliver multiple times.
	r, delivered := newR(t)
	payload := []byte("x")
	h := framing.Header{ChunkTotal: 1, Flags: framing.FlagFinal}
	if err := r.Ingest(encode(t, h, payload)); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	hDup := h
	hDup.Flags |= framing.FlagRedundant
	if err := r.Ingest(encode(t, hDup, payload)); err != nil {
		t.Fatalf("dup Ingest: %v", err)
	}
	if len(*delivered) != 1 {
		t.Fatalf("delivered: got %d, want 1 (recently-delivered cache should have caught the dup)", len(*delivered))
	}
	if got := r.Stats().FramesDup; got != 1 {
		t.Fatalf("FramesDup: got %d, want 1", got)
	}
}

// A non-REDUNDANT frame with the same MsgID is treated as a fresh
// message (the diode has no way to know that a MsgID has "really" been
// reused after wrap; only REDUNDANT-marked dupes are ignored against
// the recent cache). With ChunkTotal=1 it would deliver again.
func TestNonRedundantReuseRedelivers(t *testing.T) {
	r, delivered := newR(t)
	h := framing.Header{ChunkTotal: 1, Flags: framing.FlagFinal}
	_ = r.Ingest(encode(t, h, []byte("a")))
	_ = r.Ingest(encode(t, h, []byte("b")))
	if len(*delivered) != 2 {
		t.Fatalf("delivered: got %d, want 2", len(*delivered))
	}
}

func TestRedundantChunkOnInFlightDeduped(t *testing.T) {
	r, delivered := newR(t)
	payload := []byte("abcdef")
	// 2-chunk message. Receive chunk 0, then a REDUNDANT copy of chunk 0,
	// then chunk 1. Should deliver once, with FramesDup incremented.
	h0 := framing.Header{MsgID: 9, ChunkIndex: 0, ChunkTotal: 2}
	h1 := framing.Header{MsgID: 9, ChunkIndex: 1, ChunkTotal: 2, Flags: framing.FlagFinal}

	_ = r.Ingest(encode(t, h0, payload[0:3]))
	dup := h0
	dup.Flags |= framing.FlagRedundant
	_ = r.Ingest(encode(t, dup, payload[0:3]))
	_ = r.Ingest(encode(t, h1, payload[3:6]))

	if len(*delivered) != 1 || !bytes.Equal((*delivered)[0], payload) {
		t.Fatalf("delivered: %v", *delivered)
	}
	if got := r.Stats().FramesDup; got != 1 {
		t.Fatalf("FramesDup: got %d, want 1", got)
	}
}

func TestHeartbeatIgnored(t *testing.T) {
	r, delivered := newR(t)
	f := encode(t, framing.Header{
		ChunkTotal: 1, Flags: framing.FlagFinal | framing.FlagHeartbeat,
	}, nil)
	if err := r.Ingest(f); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(*delivered) != 0 {
		t.Fatalf("heartbeat should not deliver; got %v", *delivered)
	}
	if got := r.Stats().FramesIgnored; got != 1 {
		t.Fatalf("FramesIgnored: got %d, want 1", got)
	}
}

func TestMalformedFrameDropped(t *testing.T) {
	r, delivered := newR(t)
	if err := r.Ingest([]byte{0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(*delivered) != 0 {
		t.Fatalf("malformed frame must not deliver")
	}
	if got := r.Stats().FramesIgnored; got != 1 {
		t.Fatalf("FramesIgnored: got %d, want 1", got)
	}
}

func TestEvictionOnPendingCap(t *testing.T) {
	var delivered [][]byte
	r, err := New(func(p []byte) error {
		delivered = append(delivered, p)
		return nil
	}, Options{MaxPending: 2, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Three different MsgIDs, each missing a chunk → all stay pending.
	for id := uint32(1); id <= 3; id++ {
		h := framing.Header{MsgID: id, ChunkIndex: 0, ChunkTotal: 2}
		if err := r.Ingest(encode(t, h, []byte("x"))); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
	}
	if got := r.Stats().MsgsEvicted; got != 1 {
		t.Fatalf("MsgsEvicted: got %d, want 1", got)
	}
	if len(delivered) != 0 {
		t.Fatalf("no message should have been delivered yet")
	}
}

func TestEvictionOnByteCap(t *testing.T) {
	r, _ := New(func(p []byte) error { return nil },
		Options{MaxPending: 100, MaxBytes: 50})

	// 3 × 30-byte chunks → 90 bytes, only 50 allowed.
	chunk := bytes.Repeat([]byte("z"), 30)
	for id := uint32(1); id <= 3; id++ {
		h := framing.Header{MsgID: id, ChunkIndex: 0, ChunkTotal: 2}
		if err := r.Ingest(encode(t, h, chunk)); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
	}
	if got := r.Stats().MsgsEvicted; got == 0 {
		t.Fatalf("expected at least one eviction; stats: %+v", r.Stats())
	}
}

func TestDeliverErrorPropagates(t *testing.T) {
	stopErr := errors.New("downstream closed")
	r, err := New(func(p []byte) error { return stopErr }, DefaultOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f := encode(t, framing.Header{ChunkTotal: 1, Flags: framing.FlagFinal}, []byte("x"))
	if err := r.Ingest(f); !errors.Is(err, stopErr) {
		t.Fatalf("err: got %v, want %v", err, stopErr)
	}
}

func TestChunkTotalMismatchRestartsPartial(t *testing.T) {
	r, delivered := newR(t)

	// First frame for msg=1 says total=3, chunk 0
	_ = r.Ingest(encode(t, framing.Header{MsgID: 1, ChunkIndex: 0, ChunkTotal: 3}, []byte("a")))
	// A later frame for msg=1 says total=1 — looks like a reused MsgID.
	// Reassembler should drop the prior partial and start fresh; this
	// single-chunk message then completes and is delivered.
	_ = r.Ingest(encode(t, framing.Header{MsgID: 1, ChunkIndex: 0, ChunkTotal: 1, Flags: framing.FlagFinal}, []byte("b")))

	if len(*delivered) != 1 || !bytes.Equal((*delivered)[0], []byte("b")) {
		t.Fatalf("delivered: %v", *delivered)
	}
}

func TestNew_RejectsBadOptions(t *testing.T) {
	if _, err := New(func([]byte) error { return nil }, Options{}); err == nil {
		t.Fatalf("expected error for zero options")
	}
	if _, err := New(nil, DefaultOptions()); err == nil {
		t.Fatalf("expected error for nil deliver")
	}
}
