package session

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/degreane/datadiode/internal/framing"
)

func mkSID(b byte) framing.SessionID {
	var s framing.SessionID
	for i := range s {
		s[i] = b
	}
	return s
}

// sidBytes returns a fresh []byte copy of a SessionID — works around
// "cannot slice unaddressable value" when the SID is a function return.
func sidBytes(s framing.SessionID) []byte {
	b := make([]byte, len(s))
	copy(b, s[:])
	return b
}

func TestAppendCompletedIdx_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completed.idx")
	now := time.Now().UTC().Truncate(time.Microsecond)
	for b := byte(1); b <= 3; b++ {
		if err := appendCompletedIdx(path, mkSID(b), now.Add(time.Duration(b)*time.Second)); err != nil {
			t.Fatalf("append %d: %v", b, err)
		}
	}
	loaded, err := loadCompletedIdx(path, 10)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("len: got %d want 3", len(loaded))
	}
	for i, want := range []byte{1, 2, 3} {
		if loaded[i] != mkSID(want) {
			t.Fatalf("sid[%d] mismatch", i)
		}
	}
}

func TestLoadCompletedIdx_MissingFile(t *testing.T) {
	loaded, err := loadCompletedIdx(filepath.Join(t.TempDir(), "nope.idx"), 10)
	if err != nil || loaded != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", loaded, err)
	}
}

func TestLoadCompletedIdx_CapKeepsMostRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completed.idx")
	now := time.Now().UTC()
	// Write 10 records.
	for b := byte(1); b <= 10; b++ {
		_ = appendCompletedIdx(path, mkSID(b), now.Add(time.Duration(b)*time.Second))
	}
	// Load with cap 3: should keep the last three (sids 8, 9, 10).
	loaded, err := loadCompletedIdx(path, 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("len: got %d want 3", len(loaded))
	}
	want := []framing.SessionID{mkSID(8), mkSID(9), mkSID(10)}
	for i := range want {
		if loaded[i] != want[i] {
			t.Fatalf("loaded[%d]=%x want %x", i, loaded[i], want[i])
		}
	}
}

func TestParseCompletedIdxLine_RejectsBad(t *testing.T) {
	bad := []string{
		"",
		"not-hex\t2026-06-21T20:00:00Z",
		"deadbeef\t2026-06-21T20:00:00Z", // too short
		hex.EncodeToString(bytes.Repeat([]byte{0xAA}, 16)) + "\tnot-a-time",
		hex.EncodeToString(bytes.Repeat([]byte{0xAA}, 16)), // missing field
	}
	for _, b := range bad {
		if _, ok := parseCompletedIdxLine(b); ok {
			t.Fatalf("accepted bad line %q", b)
		}
	}
}

func TestLoadCompletedIdx_SkipsCorruptLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completed.idx")
	// Hand-write a file with two good lines + two corrupt lines.
	good1 := hex.EncodeToString(sidBytes(mkSID(1))) + "\t" + time.Now().UTC().Format(time.RFC3339Nano)
	good2 := hex.EncodeToString(sidBytes(mkSID(2))) + "\t" + time.Now().UTC().Format(time.RFC3339Nano)
	body := good1 + "\nbroken-line\nalso\tbroken\n" + good2 + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := loadCompletedIdx(path, 10)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("got %d entries; want 2 (corrupt lines should be skipped)", len(loaded))
	}
}

func TestPruneCompletedIdx_DropsOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completed.idx")
	now := time.Now().UTC()
	_ = appendCompletedIdx(path, mkSID(1), now.Add(-2*time.Hour))
	_ = appendCompletedIdx(path, mkSID(2), now.Add(-30*time.Minute))
	_ = appendCompletedIdx(path, mkSID(3), now.Add(-1*time.Minute))

	dropped, err := PruneCompletedIdx(path, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped: got %d want 1", dropped)
	}
	loaded, _ := loadCompletedIdx(path, 10)
	if len(loaded) != 2 {
		t.Fatalf("after prune: got %d, want 2", len(loaded))
	}
}

func TestPruneCompletedIdx_MissingFileNoOp(t *testing.T) {
	dropped, err := PruneCompletedIdx(filepath.Join(t.TempDir(), "nope.idx"), time.Now())
	if err != nil || dropped != 0 {
		t.Fatalf("missing file: got (%d, %v) want (0, nil)", dropped, err)
	}
}

// Manager-level: a hydrated cache rejects a "resend" sid after a
// fresh Manager is constructed on the same SpoolDir.
func TestPersistentCache_SurvivesRestart(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	out := filepath.Join(t.TempDir(), "out")
	// First Manager: complete a session, which appends to completed.idx.
	m, err := New(Options{
		SpoolDir:                 spool,
		FilesTo:                  out,
		SpoolMode:                SpoolModeSparse,
		CompletedCacheSize:       16,
		PersistentCompletedCache: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := []byte("persist-me")
	soh, datas, payloads := plan(t, mkSID(0xAB), "p.txt", 0o644, payload, 100)
	_ = m.IngestSOH(soh)
	if err := m.IngestDATA(datas[0], payloads[0]); err != nil {
		t.Fatalf("IngestDATA: %v", err)
	}
	if m.Stats().Completed != 1 {
		t.Fatalf("expected 1 completion")
	}

	// completed.idx should now exist with one line.
	idxBytes, err := os.ReadFile(filepath.Join(spool, "completed.idx"))
	if err != nil {
		t.Fatalf("read completed.idx: %v", err)
	}
	if !bytes.Contains(idxBytes, []byte(hex.EncodeToString(sidBytes(mkSID(0xAB))))) {
		t.Fatalf("completed.idx does not contain expected sid:\n%s", idxBytes)
	}

	// Simulate a restart: build a brand-new Manager pointing at the
	// same SpoolDir. The completed-cache should hydrate from disk.
	m2, err := New(Options{
		SpoolDir:                 spool,
		FilesTo:                  out,
		SpoolMode:                SpoolModeSparse,
		CompletedCacheSize:       16,
		PersistentCompletedCache: true,
	})
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	// "Resend" the same sid → should be caught by the hydrated cache.
	_ = m2.IngestSOH(soh)
	_ = m2.IngestDATA(datas[0], payloads[0])
	if m2.Stats().SOHsForCompleted != 1 {
		t.Fatalf("hydrated cache did not catch the resend; stats=%+v", m2.Stats())
	}
	if m2.Stats().Completed != 0 {
		t.Fatalf("resend was re-delivered after restart; Completed=%d", m2.Stats().Completed)
	}
}

func TestPersistentCache_DisableWhenFlagOff(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	out := filepath.Join(t.TempDir(), "out")
	m, _ := New(Options{
		SpoolDir:                 spool,
		FilesTo:                  out,
		SpoolMode:                SpoolModeSparse,
		CompletedCacheSize:       16,
		PersistentCompletedCache: false, // off
	})
	payload := []byte("ephemeral")
	soh, datas, payloads := plan(t, mkSID(0xCD), "e.txt", 0o644, payload, 100)
	_ = m.IngestSOH(soh)
	_ = m.IngestDATA(datas[0], payloads[0])
	if _, err := os.Stat(filepath.Join(spool, "completed.idx")); err == nil {
		t.Fatalf("completed.idx should NOT exist when persistence is off")
	}
}

// Sanity: appending to completed.idx with a parent path that needs
// MkdirAll creates the file. (Manager already creates SpoolDir, but
// the helper is also called directly.)
func TestAppendCompletedIdx_CreatesParent(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "newdir", "completed.idx")
	if err := appendCompletedIdx(path, mkSID(1), time.Now().UTC()); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

// Microbenchmark: each finalize incurs at most one append+fsync.
// Smoke-check we can do many in a tight loop without exploding.
func TestAppendCompletedIdx_ThroughputSanity(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	path := filepath.Join(t.TempDir(), "completed.idx")
	now := time.Now().UTC()
	const N = 500
	for i := 0; i < N; i++ {
		sid := mkSID(byte(i & 0xFF))
		// each iteration is distinct by ts
		if err := appendCompletedIdx(path, sid, now.Add(time.Duration(i)*time.Microsecond)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	loaded, _ := loadCompletedIdx(path, N)
	if len(loaded) != N {
		t.Fatalf("loaded %d records, want %d", len(loaded), N)
	}
}

// Helper to assert a string format (used in error messages above);
// kept so the unused fmt import doesn't go stale across edits.
var _ = fmt.Sprintf
