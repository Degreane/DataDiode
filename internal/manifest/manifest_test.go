package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mkRec(sid, name string, ts time.Time) Record {
	return Record{
		StartedAt:     ts.Add(-time.Second),
		CompletedAt:   ts,
		SessionID:     sid,
		Filename:      name,
		TotalBytes:    100,
		ChunkSize:     1400,
		ChunkTotal:    1,
		ContentSHA256: "abc",
		Destination:   "10.0.0.1:9999",
		Mode:          "0o644",
		Redundancy:    1,
		SOHRedundancy: 3,
		TxStatus:      "sent",
	}
}

func TestAppendAndReadAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.jsonl")
	w, err := Open(path, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	recs := []Record{
		mkRec("sid-a", "a.bin", now.Add(-2*time.Hour)),
		mkRec("sid-b", "b.bin", now.Add(-1*time.Hour)),
		mkRec("sid-c", "c.bin", now),
	}
	for _, r := range recs {
		if err := w.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	for i := range recs {
		if got[i].SessionID != recs[i].SessionID {
			t.Fatalf("rec %d sid: got %q want %q", i, got[i].SessionID, recs[i].SessionID)
		}
		if !got[i].CompletedAt.Equal(recs[i].CompletedAt) {
			t.Fatalf("rec %d completed_at differs", i)
		}
	}
}

func TestReadAll_MissingFileIsEmpty(t *testing.T) {
	got, err := ReadAll(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || got != nil {
		t.Fatalf("missing file: got (%v, %v), want (nil, nil)", got, err)
	}
}

func TestFindLatestBySID(t *testing.T) {
	now := time.Now().UTC()
	recs := []Record{
		mkRec("a", "f", now.Add(-3*time.Hour)),
		mkRec("a", "f", now.Add(-1*time.Hour)), // newest for "a"
		mkRec("b", "g", now.Add(-2*time.Hour)),
	}
	got, ok := FindLatestBySID(recs, "a")
	if !ok || !got.CompletedAt.Equal(now.Add(-1*time.Hour)) {
		t.Fatalf("latest sid 'a': got %v ok=%v", got.CompletedAt, ok)
	}
	if _, ok := FindLatestBySID(recs, "missing"); ok {
		t.Fatalf("missing sid should return ok=false")
	}
}

func TestFindLatestByFilename(t *testing.T) {
	now := time.Now().UTC()
	recs := []Record{
		mkRec("a", "x.bin", now.Add(-2*time.Hour)),
		mkRec("b", "x.bin", now.Add(-30*time.Minute)),
		mkRec("c", "y.bin", now.Add(-1*time.Hour)),
	}
	got, ok := FindLatestByFilename(recs, "x.bin")
	if !ok || got.SessionID != "b" {
		t.Fatalf("latest x.bin: got sid=%q want 'b'", got.SessionID)
	}
}

func TestFilterSince(t *testing.T) {
	now := time.Now().UTC()
	recs := []Record{
		mkRec("a", "f", now.Add(-2*time.Hour)),
		mkRec("b", "f", now.Add(-30*time.Minute)),
		mkRec("c", "f", now.Add(-5*time.Minute)),
	}
	got := FilterSince(recs, now.Add(-1*time.Hour))
	if len(got) != 2 {
		t.Fatalf("FilterSince: got %d want 2", len(got))
	}
}

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.jsonl")
	w, _ := Open(path, 200) // tiny rotate threshold

	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		if err := w.Append(mkRec("sid-x", "f", now.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	// Expect at least one rotated file in the same directory.
	entries, _ := os.ReadDir(filepath.Dir(path))
	rotated := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "manifest.jsonl.") {
			rotated++
		}
	}
	if rotated == 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected at least one rotated file, got dir: %v", names)
	}
}

func TestPruneOlderThan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.jsonl")
	w, _ := Open(path, 0)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_ = w.Append(mkRec("old", "a", now.Add(-2*time.Hour)))
	_ = w.Append(mkRec("mid", "b", now.Add(-30*time.Minute)))
	_ = w.Append(mkRec("new", "c", now.Add(-1*time.Minute)))

	dropped, err := PruneOlderThan(path, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped: got %d want 1", dropped)
	}
	got, _ := ReadAll(path)
	if len(got) != 2 {
		t.Fatalf("after prune: got %d records want 2", len(got))
	}
}

func TestFormatTable_Empty(t *testing.T) {
	s := FormatTable(nil)
	if !strings.Contains(s, "COMPLETED_AT") {
		t.Fatalf("table header missing: %s", s)
	}
}

func TestFormatTable_TruncatesLongFilenames(t *testing.T) {
	now := time.Now().UTC()
	long := strings.Repeat("x", 50)
	s := FormatTable([]Record{mkRec("a", long, now)})
	if !strings.Contains(s, "xxx...") {
		t.Fatalf("expected truncated name, got:\n%s", s)
	}
}
