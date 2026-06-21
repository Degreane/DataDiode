package session

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/degreane/datadiode/internal/framing"
)

// completed.idx file format (ADR-0007):
//
//   <sid-hex-32-chars>\t<completed-at-rfc3339nano>\n
//
// One record per line. Append-only on the hot path; rewritten in
// place by --mode=vacuum.

// completedIdxRecord parses one line. Returns ok=false on malformed
// lines so a partially-corrupted file doesn't take down the rx.
type completedIdxRecord struct {
	sid framing.SessionID
	ts  time.Time
}

func parseCompletedIdxLine(line string) (completedIdxRecord, bool) {
	var r completedIdxRecord
	parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
	if len(parts) != 2 {
		return r, false
	}
	if len(parts[0]) != 32 {
		return r, false
	}
	b, err := hex.DecodeString(parts[0])
	if err != nil || len(b) != 16 {
		return r, false
	}
	copy(r.sid[:], b)
	t, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return r, false
	}
	r.ts = t
	return r, true
}

// loadCompletedIdx reads up to `cap` of the most recent records from
// path. Missing file → empty slice + nil error (a fresh receiver is
// expected; not an error condition).
//
// "Most recent" = lines closer to the end of the file. The file is
// scanned once; if it has more than `cap` records, the oldest
// (file-head) ones are dropped from the returned slice but remain on
// disk — vacuum (--mode=vacuum) is the bounded-disk mechanism.
func loadCompletedIdx(path string, capN int) ([]framing.SessionID, error) {
	if capN <= 0 {
		return nil, errors.New("session: completed.idx cap must be > 0")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	// Use a ring buffer of size capN so we keep the LAST capN records.
	ring := make([]framing.SessionID, 0, capN)
	overwrite := 0
	full := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4<<10), 1<<20)
	for sc.Scan() {
		rec, ok := parseCompletedIdxLine(sc.Text())
		if !ok {
			continue // tolerate corrupt lines
		}
		if !full {
			ring = append(ring, rec.sid)
			if len(ring) == capN {
				full = true
			}
		} else {
			ring[overwrite] = rec.sid
			overwrite = (overwrite + 1) % capN
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !full {
		return ring, nil
	}
	// Re-order so the slice is oldest-first (matches what the caller
	// expects when hydrating a FIFO ring).
	out := make([]framing.SessionID, 0, capN)
	out = append(out, ring[overwrite:]...)
	out = append(out, ring[:overwrite]...)
	return out, nil
}

// appendCompletedIdx writes one record to path. Creates the file
// (mode 0644) if it doesn't exist; parent dir must already exist.
// Each call opens, writes, fsyncs, and closes — durable per-record
// at the cost of one syscall per session-finalize, which is
// negligible relative to the disk write that just completed the file.
func appendCompletedIdx(path string, sid framing.SessionID, ts time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line := fmt.Sprintf("%s\t%s\n", hex.EncodeToString(sid[:]), ts.UTC().Format(time.RFC3339Nano))
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	return f.Sync()
}

// PruneCompletedIdx rewrites path in place, keeping only records whose
// timestamp is at or after cutoff. Returns the count of records dropped.
// Atomic: writes to <path>.tmp then renames over the original. Missing
// file → 0 dropped, no error.
//
// Exposed (capital P) for the vacuum subcommand.
func PruneCompletedIdx(path string, cutoff time.Time) (dropped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()

	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		rec, ok := parseCompletedIdxLine(line)
		if !ok {
			// Drop corrupt lines as part of the prune (housekeeping
			// side-effect — vacuum may as well clean up garbage).
			dropped++
			continue
		}
		if rec.ts.Before(cutoff) {
			dropped++
			continue
		}
		if _, err := w.WriteString(line + "\n"); err != nil {
			return 0, err
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if err := w.Flush(); err != nil {
		return 0, err
	}
	if err := out.Sync(); err != nil {
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	closed = true
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	return dropped, nil
}
