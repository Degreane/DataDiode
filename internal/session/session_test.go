package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/degreane/datadiode/internal/framing"
)

// helper: build an SOH + chunked DATA frames over `payload` with the
// given session_id and chunk_size. Returns the SOH and a slice of
// (DATA, payload-slice) pairs ready for IngestDATA.
func plan(t *testing.T, sid framing.SessionID, name string, mode uint32, payload []byte, chunkSize uint32) (framing.SOH, []framing.DATA, [][]byte) {
	t.Helper()
	total := uint32((uint64(len(payload)) + uint64(chunkSize) - 1) / uint64(chunkSize))
	if len(payload) == 0 {
		total = 1
	}
	soh := framing.SOH{
		SessionID:     sid,
		ChunkTotal:    total,
		ChunkSize:     chunkSize,
		TotalBytes:    uint64(len(payload)),
		ContentSHA256: sha256.Sum256(payload),
		Mode:          mode,
		Name:          name,
	}
	var datas []framing.DATA
	var payloads [][]byte
	for i := uint32(0); i < total; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > uint32(len(payload)) {
			end = uint32(len(payload))
		}
		flags := uint8(0)
		if i == total-1 {
			flags |= framing.FlagFinal
		}
		datas = append(datas, framing.DATA{Flags: flags, SessionID: sid, ChunkIndex: i})
		payloads = append(payloads, payload[start:end])
	}
	return soh, datas, payloads
}

func newMgr(t *testing.T, mode SpoolMode) (*Manager, string, string) {
	t.Helper()
	spool := filepath.Join(t.TempDir(), "spool")
	out := filepath.Join(t.TempDir(), "out")
	m, err := New(Options{SpoolDir: spool, FilesTo: out, SpoolMode: mode})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, spool, out
}

func sid(b byte) framing.SessionID {
	var s framing.SessionID
	for i := range s {
		s[i] = b
	}
	return s
}

func TestSparse_SingleSession_RoundTrip(t *testing.T) {
	m, _, out := newMgr(t, SpoolModeSparse)
	payload := bytes.Repeat([]byte{0xAB}, 4500) // not chunk-aligned
	soh, datas, payloads := plan(t, sid(1), "blob.bin", 0o600, payload, 1400)

	if err := m.IngestSOH(soh); err != nil {
		t.Fatalf("SOH: %v", err)
	}
	for i := range datas {
		if err := m.IngestDATA(datas[i], payloads[i]); err != nil {
			t.Fatalf("DATA %d: %v", i, err)
		}
	}

	got, err := os.ReadFile(filepath.Join(out, "blob.bin"))
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestFiles_SingleSession_RoundTrip(t *testing.T) {
	m, _, out := newMgr(t, SpoolModeFiles)
	payload := []byte("hello, files-mode receiver")
	soh, datas, payloads := plan(t, sid(2), "greeting.txt", 0o644, payload, 10)

	if err := m.IngestSOH(soh); err != nil {
		t.Fatalf("SOH: %v", err)
	}
	for i := range datas {
		if err := m.IngestDATA(datas[i], payloads[i]); err != nil {
			t.Fatalf("DATA %d: %v", i, err)
		}
	}

	got, _ := os.ReadFile(filepath.Join(out, "greeting.txt"))
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %q want %q", got, payload)
	}
}

func TestSparse_OutOfOrderChunks(t *testing.T) {
	m, _, out := newMgr(t, SpoolModeSparse)
	payload := []byte("the quick brown fox jumps over the lazy dog")
	soh, datas, payloads := plan(t, sid(3), "fox.txt", 0o644, payload, 5)

	if err := m.IngestSOH(soh); err != nil {
		t.Fatalf("SOH: %v", err)
	}
	// Reverse order.
	for i := len(datas) - 1; i >= 0; i-- {
		if err := m.IngestDATA(datas[i], payloads[i]); err != nil {
			t.Fatalf("DATA %d: %v", i, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(out, "fox.txt"))
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestSparse_DuplicateChunkDeduped(t *testing.T) {
	m, _, _ := newMgr(t, SpoolModeSparse)
	// Multi-chunk payload so the first chunk doesn't finish the
	// session — leaves it in-flight so dedup-via-bitmap is exercised.
	payload := []byte("hellohello") // 10 B over chunk_size 5 → 2 chunks
	soh, datas, payloads := plan(t, sid(4), "h.txt", 0o644, payload, 5)
	_ = m.IngestSOH(soh)
	_ = m.IngestDATA(datas[0], payloads[0])
	// REDUNDANT copy of chunk 0.
	_ = m.IngestDATA(datas[0], payloads[0])
	if got := m.Stats().DataDup; got != 1 {
		t.Fatalf("DataDup: got %d want 1", got)
	}
}

func TestSparse_DataForUnknownSessionDropped(t *testing.T) {
	m, _, _ := newMgr(t, SpoolModeSparse)
	d := framing.DATA{SessionID: sid(99), ChunkIndex: 0}
	if err := m.IngestDATA(d, []byte("orphan")); err != nil {
		t.Fatalf("IngestDATA: %v", err)
	}
	if got := m.Stats().DataDropped; got != 1 {
		t.Fatalf("DataDropped: got %d want 1", got)
	}
}

func TestSparse_TamperedContentRejected(t *testing.T) {
	m, spool, out := newMgr(t, SpoolModeSparse)
	payload := []byte("original")
	soh, datas, _ := plan(t, sid(5), "tamper.txt", 0o644, payload, 100)
	_ = m.IngestSOH(soh)
	// Corrupt the chunk we're about to deliver (changes the file
	// content but not the SOH's declared sha → mismatch on finalize).
	tampered := []byte("CORRUPT!")
	err := m.IngestDATA(datas[0], tampered)
	if err == nil {
		t.Fatalf("expected sha mismatch error on finalize, got nil")
	}
	// No output file should exist.
	if _, statErr := os.Stat(filepath.Join(out, "tamper.txt")); statErr == nil {
		t.Fatalf("output file should NOT exist after sha mismatch")
	}
	// Spool dir should be retained for forensics.
	if _, statErr := os.Stat(filepath.Join(spool, sid(5).String())); statErr != nil {
		t.Fatalf("spool dir should be retained on mismatch")
	}
	if m.Stats().HashMismatch != 1 {
		t.Fatalf("HashMismatch counter: %d", m.Stats().HashMismatch)
	}
}

func TestSparse_DuplicateSOHIgnored(t *testing.T) {
	m, _, _ := newMgr(t, SpoolModeSparse)
	soh, _, _ := plan(t, sid(6), "h.txt", 0o644, []byte("hello"), 10)
	_ = m.IngestSOH(soh)
	_ = m.IngestSOH(soh) // re-send
	if m.Stats().SOHsAccepted != 1 {
		t.Fatalf("Duplicate SOH should not double-accept; SOHsAccepted=%d", m.Stats().SOHsAccepted)
	}
	if m.Stats().Active != 1 {
		t.Fatalf("Active sessions: %d, want 1", m.Stats().Active)
	}
}

func TestConcurrentSessions_NoCollision(t *testing.T) {
	m, _, out := newMgr(t, SpoolModeSparse)

	// Interleave chunks from two sessions.
	sid1 := sid(7)
	sid2 := sid(8)
	p1 := bytes.Repeat([]byte("AAAA"), 25)
	p2 := bytes.Repeat([]byte("BBBB"), 25)
	soh1, d1, pp1 := plan(t, sid1, "a.bin", 0o644, p1, 30)
	soh2, d2, pp2 := plan(t, sid2, "b.bin", 0o644, p2, 30)

	_ = m.IngestSOH(soh1)
	_ = m.IngestSOH(soh2)
	// Interleave: 1.0, 2.0, 1.1, 2.1, ...
	for i := 0; i < len(d1) || i < len(d2); i++ {
		if i < len(d1) {
			_ = m.IngestDATA(d1[i], pp1[i])
		}
		if i < len(d2) {
			_ = m.IngestDATA(d2[i], pp2[i])
		}
	}

	got1, _ := os.ReadFile(filepath.Join(out, "a.bin"))
	got2, _ := os.ReadFile(filepath.Join(out, "b.bin"))
	if !bytes.Equal(got1, p1) {
		t.Fatalf("a.bin mismatch")
	}
	if !bytes.Equal(got2, p2) {
		t.Fatalf("b.bin mismatch")
	}
}

func TestMaxConcurrentEnforced(t *testing.T) {
	spool := t.TempDir()
	out := t.TempDir()
	m, _ := New(Options{SpoolDir: spool, FilesTo: out, SpoolMode: SpoolModeSparse, MaxConcurrent: 1})

	soh1, _, _ := plan(t, sid(10), "a.bin", 0o644, []byte("x"), 10)
	soh2, _, _ := plan(t, sid(11), "b.bin", 0o644, []byte("y"), 10)
	_ = m.IngestSOH(soh1)
	_ = m.IngestSOH(soh2)
	if m.Stats().SOHsAccepted != 1 || m.Stats().SOHsRejected != 1 {
		t.Fatalf("MaxConcurrent: accepted=%d rejected=%d", m.Stats().SOHsAccepted, m.Stats().SOHsRejected)
	}
}

func TestMetaJSON_Schema(t *testing.T) {
	m, spool, _ := newMgr(t, SpoolModeSparse)
	soh, _, _ := plan(t, sid(12), "schema.txt", 0o640, []byte("x"), 10)
	_ = m.IngestSOH(soh)
	b, err := os.ReadFile(filepath.Join(spool, sid(12).String(), "meta.json"))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var got Meta
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("parse meta: %v", err)
	}
	if got.SessionID != sid(12).String() {
		t.Fatalf("SessionID: %q", got.SessionID)
	}
	if got.Filename != "schema.txt" {
		t.Fatalf("Filename: %q", got.Filename)
	}
	if got.Mode != "0o640" {
		t.Fatalf("Mode: %q", got.Mode)
	}
	if got.SpoolMode != SpoolModeSparse {
		t.Fatalf("SpoolMode: %q", got.SpoolMode)
	}
	if got.Version != "v2" {
		t.Fatalf("Version: %q", got.Version)
	}
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"0o644", 0o644, true},
		{"0o755", 0o755, true},
		{"0644", 0o644, true},
		{"644", 0o644, true},
		{"", 0, false},
		{"0o9", 0, false},
		{"abc", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseMode(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got (%v, %v) want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestOnCompleteCallback(t *testing.T) {
	spool := t.TempDir()
	var called []string
	m, _ := New(Options{
		SpoolDir:  spool,
		SpoolMode: SpoolModeSparse,
		OnComplete: func(s *Session, path string) error {
			called = append(called, fmt.Sprintf("%s:%s", s.Meta.Filename, path))
			return nil
		},
	})
	payload := []byte("via callback")
	soh, datas, payloads := plan(t, sid(20), "cb.txt", 0o644, payload, 100)
	_ = m.IngestSOH(soh)
	if err := m.IngestDATA(datas[0], payloads[0]); err != nil {
		t.Fatalf("IngestDATA: %v", err)
	}
	if len(called) != 1 {
		t.Fatalf("OnComplete called %d times, want 1", len(called))
	}
}

func TestNew_RejectsBadOptions(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error for empty SpoolDir")
	}
	if _, err := New(Options{SpoolDir: t.TempDir()}); err == nil {
		t.Fatal("expected error when both FilesTo and OnComplete are empty")
	}
	if _, err := New(Options{SpoolDir: t.TempDir(), FilesTo: t.TempDir(), SpoolMode: "weird"}); err == nil {
		t.Fatal("expected error for invalid SpoolMode")
	}
}

func TestSparseAndFilesProduceIdenticalOutput(t *testing.T) {
	payload := bytes.Repeat([]byte("XY"), 1500) // 3000 bytes
	chunk := uint32(400)

	mSparse, _, outSparse := newMgr(t, SpoolModeSparse)
	mFiles, _, outFiles := newMgr(t, SpoolModeFiles)

	for name, m := range map[string]*Manager{"sparse": mSparse, "files": mFiles} {
		soh, datas, payloads := plan(t, sid(byte(name[0])), "p.bin", 0o644, payload, chunk)
		_ = m.IngestSOH(soh)
		for i := range datas {
			_ = m.IngestDATA(datas[i], payloads[i])
		}
	}

	gotSparse, _ := os.ReadFile(filepath.Join(outSparse, "p.bin"))
	gotFiles, _ := os.ReadFile(filepath.Join(outFiles, "p.bin"))
	if !bytes.Equal(gotSparse, payload) {
		t.Fatalf("sparse output mismatch")
	}
	if !bytes.Equal(gotFiles, payload) {
		t.Fatalf("files output mismatch")
	}
	if !bytes.Equal(gotSparse, gotFiles) {
		t.Fatalf("sparse and files modes produced different bytes")
	}
}

// ---- completed-cache --------------------------------------------------

func TestCompletedCache_ResendAfterCompletionDropped(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	out := filepath.Join(t.TempDir(), "out")
	m, err := New(Options{
		SpoolDir: spool, FilesTo: out, SpoolMode: SpoolModeSparse,
		CompletedCacheSize: 16,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := []byte("complete me")
	soh, datas, payloads := plan(t, sid(30), "done.txt", 0o644, payload, 100)

	// Original send completes.
	_ = m.IngestSOH(soh)
	if err := m.IngestDATA(datas[0], payloads[0]); err != nil {
		t.Fatalf("IngestDATA: %v", err)
	}
	if m.Stats().Completed != 1 {
		t.Fatalf("expected 1 completion")
	}

	// "Resend" same sid + same chunk: SOH should be marked-completed
	// drop; DATA should be data_dropped (no session in map).
	_ = m.IngestSOH(soh)
	_ = m.IngestDATA(datas[0], payloads[0])

	if got := m.Stats().SOHsForCompleted; got != 1 {
		t.Fatalf("SOHsForCompleted: got %d want 1", got)
	}
	if got := m.Stats().DataDropped; got != 1 {
		t.Fatalf("DataDropped: got %d want 1", got)
	}
	// No second completion.
	if m.Stats().Completed != 1 {
		t.Fatalf("Completed should remain 1; got %d", m.Stats().Completed)
	}
}

func TestCompletedCache_DisabledByDefault(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	out := filepath.Join(t.TempDir(), "out")
	m, _ := New(Options{SpoolDir: spool, FilesTo: out, SpoolMode: SpoolModeSparse})
	payload := []byte("repeatable")
	soh, datas, payloads := plan(t, sid(31), "repeat.txt", 0o644, payload, 100)

	_ = m.IngestSOH(soh)
	_ = m.IngestDATA(datas[0], payloads[0])
	_ = m.IngestSOH(soh)
	_ = m.IngestDATA(datas[0], payloads[0])

	if m.Stats().Completed != 2 {
		t.Fatalf("with cache disabled, second send should re-deliver; got Completed=%d", m.Stats().Completed)
	}
	if m.Stats().SOHsForCompleted != 0 {
		t.Fatalf("SOHsForCompleted should be 0 when cache disabled")
	}
}

func TestCompletedCache_RingEviction(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	out := filepath.Join(t.TempDir(), "out")
	m, _ := New(Options{
		SpoolDir: spool, FilesTo: out, SpoolMode: SpoolModeSparse,
		CompletedCacheSize: 2,
	})
	// Complete 3 sessions; cache holds last 2.
	for b := byte(40); b < 43; b++ {
		soh, datas, payloads := plan(t, sid(b), fmt.Sprintf("f%d.txt", b), 0o644, []byte("x"), 100)
		_ = m.IngestSOH(soh)
		_ = m.IngestDATA(datas[0], payloads[0])
	}
	// Resend the oldest (sid 40) — should have been evicted from cache
	// → treated as fresh, gets re-delivered.
	soh40, datas40, payloads40 := plan(t, sid(40), "f40.txt", 0o644, []byte("x"), 100)
	_ = m.IngestSOH(soh40)
	_ = m.IngestDATA(datas40[0], payloads40[0])
	if m.Stats().Completed != 4 {
		t.Fatalf("evicted sid should re-deliver; Completed=%d want 4", m.Stats().Completed)
	}
}

// Marker so the unused "time" import stays during incremental development.
var _ = time.Time{}

// readFileHelper is shared across session_test.go and fec_test.go.
func readFileHelper(path string) ([]byte, error) {
	return os.ReadFile(path)
}
