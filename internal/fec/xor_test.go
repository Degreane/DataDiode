package fec

import (
	"bytes"
	"errors"
	"testing"
)

const cs = 8 // small chunk size for test clarity

func TestParity_KnownXOR(t *testing.T) {
	a := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	b := []byte{0xFF, 0xFE, 0xFD, 0xFC, 0xFB, 0xFA, 0xF9, 0xF8}
	want := make([]byte, cs)
	for i := range want {
		want[i] = a[i] ^ b[i]
	}
	got, err := Parity([][]byte{a, b}, cs)
	if err != nil {
		t.Fatalf("Parity: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParity_ZeroPadsShortShard(t *testing.T) {
	a := []byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}
	b := []byte{0x55, 0x55} // shorter; treated as 0x55, 0x55, 0, 0, 0, 0, 0, 0
	got, _ := Parity([][]byte{a, b}, cs)
	want := []byte{0xAA ^ 0x55, 0xAA ^ 0x55, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParity_SkipsNilShards(t *testing.T) {
	a := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	// nil shards are no-ops, so Parity([a, nil, a]) == a XOR a == 0.
	got, _ := Parity([][]byte{a, nil, a}, cs)
	zero := make([]byte, cs)
	if !bytes.Equal(got, zero) {
		t.Fatalf("nil-skip wrong: got %x want zeros (a XOR a)", got)
	}
	// And with just one shard + nil, we should get the shard verbatim.
	got2, _ := Parity([][]byte{a, nil}, cs)
	if !bytes.Equal(got2, a) {
		t.Fatalf("nil-skip wrong: got %x want %x", got2, a)
	}
}

func TestParity_RejectsOversizeShard(t *testing.T) {
	big := bytes.Repeat([]byte{1}, cs+1)
	if _, err := Parity([][]byte{big}, cs); !errors.Is(err, ErrShardSizeMismatch) {
		t.Fatalf("err: got %v, want ErrShardSizeMismatch", err)
	}
}

func TestReconstruct_RoundTrip(t *testing.T) {
	// 3 data shards + 1 parity. Drop shard[1]; reconstruct.
	a := []byte{1, 1, 1, 1, 1, 1, 1, 1}
	b := []byte{2, 2, 2, 2, 2, 2, 2, 2}
	c := []byte{3, 3, 3, 3, 3, 3, 3, 3}
	parity, _ := Parity([][]byte{a, b, c}, cs)

	shards := [][]byte{a, nil, c}
	got, idx, err := Reconstruct(shards, parity, cs)
	if err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if idx != 1 {
		t.Fatalf("idx: got %d want 1", idx)
	}
	if !bytes.Equal(got, b) {
		t.Fatalf("reconstructed: got %x want %x", got, b)
	}
}

func TestReconstruct_LastShardShorter(t *testing.T) {
	// Realistic: the last data chunk in a file may be smaller than chunkSize.
	// The encoder zero-pads it before XOR. The reconstructed shard comes
	// out at full chunkSize; the caller truncates to its declared payload_len.
	a := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	b := []byte{0x11, 0x22, 0x33} // last chunk, only 3 bytes
	parity, _ := Parity([][]byte{a, b}, cs)
	shards := [][]byte{nil, b}
	got, idx, err := Reconstruct(shards, parity, cs)
	if err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if idx != 0 {
		t.Fatalf("idx: got %d want 0", idx)
	}
	if !bytes.Equal(got, a) {
		t.Fatalf("got %x want %x", got, a)
	}
}

func TestReconstruct_NoMissingIsError(t *testing.T) {
	a := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	parity, _ := Parity([][]byte{a}, cs)
	_, _, err := Reconstruct([][]byte{a}, parity, cs)
	if !errors.Is(err, ErrFECNotRecoverable) {
		t.Fatalf("err: got %v, want ErrFECNotRecoverable", err)
	}
}

func TestReconstruct_TwoMissingIsError(t *testing.T) {
	parity := make([]byte, cs)
	_, _, err := Reconstruct([][]byte{nil, nil}, parity, cs)
	if !errors.Is(err, ErrFECNotRecoverable) {
		t.Fatalf("err: got %v, want ErrFECNotRecoverable", err)
	}
}

func TestReconstruct_BadParityLen(t *testing.T) {
	shards := [][]byte{nil, []byte{1, 2, 3, 4, 5, 6, 7, 8}}
	_, _, err := Reconstruct(shards, []byte{1, 2, 3}, cs)
	if !errors.Is(err, ErrShardSizeMismatch) {
		t.Fatalf("err: got %v, want ErrShardSizeMismatch", err)
	}
}

func TestParityTotal(t *testing.T) {
	cases := []struct {
		ct, gs, want uint32
	}{
		{0, 0, 0},
		{100, 0, 0},   // disabled
		{100, 10, 10}, // exact divide
		{101, 10, 11}, // remainder → +1
		{1, 10, 1},
		{9, 10, 1},
	}
	for _, c := range cases {
		if got := ParityTotal(c.ct, c.gs); got != c.want {
			t.Fatalf("ParityTotal(%d, %d) = %d, want %d", c.ct, c.gs, got, c.want)
		}
	}
}

func TestGroupOf(t *testing.T) {
	if g := GroupOf(0, 10); g != 0 {
		t.Fatalf("0/10 = %d", g)
	}
	if g := GroupOf(9, 10); g != 0 {
		t.Fatalf("9/10 = %d", g)
	}
	if g := GroupOf(10, 10); g != 1 {
		t.Fatalf("10/10 = %d", g)
	}
	if g := GroupOf(99, 10); g != 9 {
		t.Fatalf("99/10 = %d", g)
	}
}

func TestDataIndicesInGroup(t *testing.T) {
	s, e := DataIndicesInGroup(0, 10, 100)
	if s != 0 || e != 10 {
		t.Fatalf("group 0: %d..%d", s, e)
	}
	s, e = DataIndicesInGroup(9, 10, 100)
	if s != 90 || e != 100 {
		t.Fatalf("group 9: %d..%d", s, e)
	}
	// Last group with remainder: chunk_total=95, K=10, group 9 holds 90..94 (5 chunks)
	s, e = DataIndicesInGroup(9, 10, 95)
	if s != 90 || e != 95 {
		t.Fatalf("partial last group: %d..%d want 90..95", s, e)
	}
}
