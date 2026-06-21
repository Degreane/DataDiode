package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/degreane/datadiode/internal/framing"
)

// newLoopForTest builds a txLoop wired to a capturing send function so
// tests can inspect the exact frames the loop would have shipped.
func newLoopForTest(cfg txConfig, input io.Reader) (*txLoop, *[][]byte) {
	var sent [][]byte
	send := func(b []byte) error {
		cp := make([]byte, len(b))
		copy(cp, b)
		sent = append(sent, cp)
		return nil
	}
	return &txLoop{
		cfg:    cfg,
		send:   send,
		out:    make([]byte, 0, framing.MaxFrameLen),
		buf:    make([]byte, cfg.chunkBytes),
		reader: bufio.NewReader(input),
	}, &sent
}

func TestParseTxFlags_Defaults(t *testing.T) {
	c, err := parseTxFlags([]string{"--dst=10.99.0.20:9999"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.dst != "10.99.0.20:9999" {
		t.Fatalf("dst: %q", c.dst)
	}
	if c.chunkBytes != framing.MaxPayloadLen {
		t.Fatalf("chunkBytes: got %d, want %d", c.chunkBytes, framing.MaxPayloadLen)
	}
	if c.redundancy != 1 {
		t.Fatalf("redundancy default: %d", c.redundancy)
	}
	if c.inputPath != "-" {
		t.Fatalf("inputPath default: %q", c.inputPath)
	}
}

func TestParseTxFlags_RequiresDst(t *testing.T) {
	_, err := parseTxFlags([]string{})
	if err == nil || !strings.Contains(err.Error(), "--dst") {
		t.Fatalf("err: got %v, want --dst error", err)
	}
}

func TestParseTxFlags_RejectsBadChunk(t *testing.T) {
	for _, bad := range []string{"--chunk=0", "--chunk=99999"} {
		t.Run(bad, func(t *testing.T) {
			_, err := parseTxFlags([]string{"--dst=:9", bad})
			if err == nil {
				t.Fatalf("expected error for %s", bad)
			}
		})
	}
}

func TestParseTxFlags_RejectsBadRedundancy(t *testing.T) {
	_, err := parseTxFlags([]string{"--dst=:9", "--redundancy=0"})
	if err == nil {
		t.Fatalf("expected error for --redundancy=0")
	}
}

func TestTxLoop_EmptyInputEmitsHeartbeat(t *testing.T) {
	cfg := txConfig{dst: ":9", chunkBytes: 100, redundancy: 1, maxMessage: 1024}
	loop, sent := newLoopForTest(cfg, bytes.NewReader(nil))
	if err := loop.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("expected 1 heartbeat frame, got %d", len(*sent))
	}
	h, payload, err := framing.Decode((*sent)[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !h.IsFinal() || !h.IsHeartbeat() {
		t.Fatalf("flags: got %08b, want FINAL+HEARTBEAT", h.Flags)
	}
	if len(payload) != 0 {
		t.Fatalf("payload: got %d bytes, want 0", len(payload))
	}
}

func TestTxLoop_SingleChunkMessage(t *testing.T) {
	payload := []byte("hello, diode")
	cfg := txConfig{dst: ":9", chunkBytes: 100, redundancy: 1, maxMessage: 1024}
	loop, sent := newLoopForTest(cfg, bytes.NewReader(payload))
	if err := loop.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(*sent))
	}
	h, got, err := framing.Decode((*sent)[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !h.IsFinal() {
		t.Fatalf("single-chunk frame must be FINAL")
	}
	if h.ChunkTotal != 1 || h.ChunkIndex != 0 {
		t.Fatalf("chunk: got %d/%d, want 0/1", h.ChunkIndex, h.ChunkTotal)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestTxLoop_MultiChunkMessage(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 250)
	cfg := txConfig{dst: ":9", chunkBytes: 100, redundancy: 1, maxMessage: 1 << 20}
	loop, sent := newLoopForTest(cfg, bytes.NewReader(payload))
	if err := loop.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*sent) != 3 {
		t.Fatalf("frame count: got %d, want 3", len(*sent))
	}

	var reassembled []byte
	for i, raw := range *sent {
		h, p, err := framing.Decode(raw)
		if err != nil {
			t.Fatalf("decode frame %d: %v", i, err)
		}
		if int(h.ChunkIndex) != i {
			t.Fatalf("frame %d ChunkIndex: got %d", i, h.ChunkIndex)
		}
		if h.ChunkTotal != 3 {
			t.Fatalf("frame %d ChunkTotal: got %d, want 3", i, h.ChunkTotal)
		}
		if h.IsFinal() != (i == 2) {
			t.Fatalf("frame %d FINAL: got %v, want %v", i, h.IsFinal(), i == 2)
		}
		reassembled = append(reassembled, p...)
	}
	if !bytes.Equal(reassembled, payload) {
		t.Fatalf("reassembled payload mismatch")
	}
}

func TestTxLoop_Redundancy(t *testing.T) {
	payload := []byte("dup-me")
	cfg := txConfig{dst: ":9", chunkBytes: 100, redundancy: 3, maxMessage: 1024}
	loop, sent := newLoopForTest(cfg, bytes.NewReader(payload))
	if err := loop.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*sent) != 3 {
		t.Fatalf("frame count: got %d, want 3", len(*sent))
	}
	original, _, err := framing.Decode((*sent)[0])
	if err != nil {
		t.Fatalf("decode original: %v", err)
	}
	if original.IsRedundant() {
		t.Fatalf("first frame must NOT carry REDUNDANT")
	}
	for i := 1; i < 3; i++ {
		h, _, err := framing.Decode((*sent)[i])
		if err != nil {
			t.Fatalf("decode copy %d: %v", i, err)
		}
		if !h.IsRedundant() {
			t.Fatalf("copy %d must carry REDUNDANT", i)
		}
		if h.Seq != original.Seq || h.MsgID != original.MsgID || h.ChunkIndex != original.ChunkIndex {
			t.Fatalf("copy %d identity differs from original", i)
		}
	}
}

func TestTxLoop_OversizedMessageAborts(t *testing.T) {
	cfg := txConfig{dst: ":9", chunkBytes: 100, redundancy: 1, maxMessage: 50}
	loop, sent := newLoopForTest(cfg, bytes.NewReader(bytes.Repeat([]byte{'x'}, 200)))
	err := loop.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "max-message") {
		t.Fatalf("err: got %v, want max-message error", err)
	}
	if len(*sent) != 0 {
		t.Fatalf("must not send anything on overflow; sent %d frames", len(*sent))
	}
}

func TestTxLoop_CtxCancelStops(t *testing.T) {
	cfg := txConfig{dst: ":9", chunkBytes: 1, redundancy: 1, maxMessage: 1 << 20}
	loop, sent := newLoopForTest(cfg, bytes.NewReader(bytes.Repeat([]byte{'x'}, 10_000)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := loop.run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err: got %v, want context.Canceled", err)
	}
	if len(*sent) > 1 {
		t.Fatalf("sent too much after cancel: %d frames", len(*sent))
	}
}
