// Package manifest writes and reads the sender-side append-only
// transfer log at <sender-state>/manifest.jsonl.
//
// See docs/architecture/ADR-0006-sender-state-resend-vacuum.md for
// the locked schema. One JSON object per line, UTF-8, no trailing
// whitespace. Append is durable (O_APPEND + fsync); rotation is
// atomic (rename when size >= --manifest-rotate-bytes).
package manifest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Record is one entry in manifest.jsonl. Field names are stable and
// match the ADR-0006 schema verbatim.
type Record struct {
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	SessionID     string    `json:"session_id"`
	Filename      string    `json:"filename"`
	TotalBytes    uint64    `json:"total_bytes"`
	ChunkSize     uint32    `json:"chunk_size"`
	ChunkTotal    uint32    `json:"chunk_total"`
	ContentSHA256 string    `json:"content_sha256"`
	Destination   string    `json:"destination"`
	Mode          string    `json:"mode"`
	Redundancy    int       `json:"redundancy"`
	SOHRedundancy int       `json:"soh_redundancy"`
	Signed        bool      `json:"signed"`
	Snapshot      string    `json:"snapshot,omitempty"`
	TxStatus      string    `json:"tx_status"`
	Note          string    `json:"note,omitempty"`
}

// Writer atomically appends Records to a manifest file with optional
// size-based rotation. Construct with Open; call Close when done.
type Writer struct {
	path        string
	rotateBytes int64
}

// Open returns a Writer for path, creating the parent directory if
// missing. rotateBytes > 0 enables size-based rotation: when the
// manifest's pre-append size meets/exceeds rotateBytes, it's atomically
// renamed to "<path>.<UTC-iso>" and a fresh file is started.
func Open(path string, rotateBytes int64) (*Writer, error) {
	if path == "" {
		return nil, errors.New("manifest: path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("manifest: mkdir parent: %w", err)
	}
	return &Writer{path: path, rotateBytes: rotateBytes}, nil
}

// Append writes one Record as a JSON line. Each call is one syscall
// open (with O_APPEND + O_CREATE), one write, one fsync, one close —
// safer than holding an FD open across calls because the manifest
// might be rotated/vacuumed underneath us.
func (w *Writer) Append(r Record) error {
	if w.rotateBytes > 0 {
		if st, err := os.Stat(w.path); err == nil && st.Size() >= w.rotateBytes {
			rotated := w.path + "." + time.Now().UTC().Format("20060102T150405Z")
			if err := os.Rename(w.path, rotated); err != nil {
				return fmt.Errorf("manifest: rotate: %w", err)
			}
		}
	}
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("manifest: open: %w", err)
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("manifest: marshal: %w", err)
	}
	b = append(b, '\n')
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("manifest: write: %w", err)
	}
	return f.Sync()
}

// Close is a no-op today (Append opens and closes per call) but is
// declared so callers can adopt a "with manifest.Open(...) defer Close()"
// pattern that survives future buffering.
func (w *Writer) Close() error { return nil }

// ReadAll loads every Record from path. Missing file → empty slice,
// no error.
func ReadAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return readAll(f)
}

func readAll(r io.Reader) ([]Record, error) {
	var out []Record
	sc := bufio.NewScanner(r)
	// JSON-Lines records can hold a full chunk_size of metadata; bump
	// the scanner buffer to 1 MiB so an unusually-large `note` doesn't
	// trigger ErrTooLong.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(b, &rec); err != nil {
			return out, fmt.Errorf("manifest: line %d: %w", line, err)
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// FindLatestBySID returns the most recent Record with the given sid,
// or (Record{}, false) if none.
func FindLatestBySID(records []Record, sid string) (Record, bool) {
	var best Record
	found := false
	for _, r := range records {
		if r.SessionID != sid {
			continue
		}
		if !found || r.CompletedAt.After(best.CompletedAt) {
			best = r
			found = true
		}
	}
	return best, found
}

// FindLatestByFilename returns the most recent Record whose Filename
// matches basename, or (Record{}, false) if none.
func FindLatestByFilename(records []Record, basename string) (Record, bool) {
	var best Record
	found := false
	for _, r := range records {
		if r.Filename != basename {
			continue
		}
		if !found || r.CompletedAt.After(best.CompletedAt) {
			best = r
			found = true
		}
	}
	return best, found
}

// FilterSince returns records whose CompletedAt is at or after t.
func FilterSince(records []Record, t time.Time) []Record {
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if !r.CompletedAt.Before(t) {
			out = append(out, r)
		}
	}
	return out
}

// PruneOlderThan rewrites the manifest at path in place, keeping only
// records whose CompletedAt is at or after cutoff. Returns the count
// of records dropped.
//
// Atomic: writes to <path>.tmp then renames over the original.
func PruneOlderThan(path string, cutoff time.Time) (dropped int, err error) {
	records, err := ReadAll(path)
	if err != nil {
		return 0, err
	}
	kept := make([]Record, 0, len(records))
	for _, r := range records {
		if r.CompletedAt.Before(cutoff) {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	if dropped == 0 {
		return 0, nil
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("manifest: prune create tmp: %w", err)
	}
	enc := json.NewEncoder(f)
	for _, r := range kept {
		if err := enc.Encode(r); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return 0, fmt.Errorf("manifest: prune encode: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("manifest: prune sync: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("manifest: prune close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, fmt.Errorf("manifest: prune rename: %w", err)
	}
	return dropped, nil
}

// FormatTable returns a fixed-column human-readable table of records.
// Newest first.
func FormatTable(records []Record) string {
	sorted := append([]Record(nil), records...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CompletedAt.After(sorted[j].CompletedAt)
	})
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-20s  %-36s  %-30s  %12s  %s\n",
		"COMPLETED_AT (UTC)", "SESSION_ID", "FILENAME", "BYTES", "DEST")
	fmt.Fprintln(&sb, strings.Repeat("-", 116))
	for _, r := range sorted {
		name := r.Filename
		if len(name) > 30 {
			name = name[:27] + "..."
		}
		fmt.Fprintf(&sb, "%-20s  %-36s  %-30s  %12d  %s\n",
			r.CompletedAt.UTC().Format("2006-01-02T15:04:05Z"),
			r.SessionID, name, r.TotalBytes, r.Destination)
	}
	return sb.String()
}
