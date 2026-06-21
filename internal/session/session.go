// Package session implements the v2 receive-side state machine: SOH
// frames spawn a Session, DATA frames write to a sparse file (or
// per-chunk files), completion verifies SHA-256 and atomic-renames
// the result to the configured sink.
//
// See docs/architecture/ADR-0005-session-protocol.md for the locked
// spec. This package is single-goroutine; the Manager.Ingest method
// must be called from one place (the rx UDP read loop).
package session

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/degreane/datadiode/internal/framing"
)

// SpoolMode selects the on-disk staging layout.
type SpoolMode string

const (
	SpoolModeSparse SpoolMode = "sparse" // one data.partial sparse file + chunks.bitmap (default)
	SpoolModeFiles  SpoolMode = "files"  // per-chunk files under chunks/
)

// Options configure a Manager.
type Options struct {
	// SpoolDir is the parent directory under which one subdirectory
	// per session is created. Must exist (or be creatable) before
	// the first SOH arrives.
	SpoolDir string

	// FilesTo is the directory where completed files are atomically
	// renamed. If empty, OnComplete is called instead (raw mode).
	FilesTo string

	// OnComplete is invoked when a session finishes if FilesTo is "".
	// The provided path is the assembled file in the spool dir; the
	// callback owns deciding what to do with it.
	OnComplete func(s *Session, path string) error

	// SpoolMode selects sparse (default) or files.
	SpoolMode SpoolMode

	// MaxConcurrent caps the number of simultaneously-active sessions.
	// SOH frames beyond this are rejected (counter incremented).
	// 0 = unlimited.
	MaxConcurrent int

	// Now is overridable for tests.
	Now func() time.Time
}

// Stats is a snapshot of receiver-side counters.
type Stats struct {
	SOHsSeen     uint64
	SOHsAccepted uint64
	SOHsRejected uint64 // unknown reason — duplicate, max-concurrent hit, etc.
	DataFrames   uint64
	DataDropped  uint64 // unknown session_id (no SOH yet or never received)
	DataDup      uint64 // chunk we already had (bit set)
	Completed    uint64
	HashMismatch uint64 // completion sha256 didn't match SOH's claim
	Active       int
}

// Manager routes incoming frames to per-session state.
type Manager struct {
	opts     Options
	sessions map[framing.SessionID]*Session
	stats    Stats
}

// Session is one in-flight transfer.
type Session struct {
	SID         framing.SessionID
	Meta        Meta
	Dir         string
	mode        SpoolMode
	receivedCnt uint32

	// sparse-mode state
	data   *os.File
	bitmap []byte // in-memory; persisted on completion (and could be on every chunk in a future iteration)

	startedAt time.Time
}

// Meta is the JSON written to <session_dir>/meta.json.
type Meta struct {
	SessionID     string    `json:"session_id"`
	Version       string    `json:"version"`
	Filename      string    `json:"filename"`
	Mode          string    `json:"mode"`
	TotalBytes    uint64    `json:"total_bytes"`
	ChunkSize     uint32    `json:"chunk_size"`
	ChunkTotal    uint32    `json:"chunk_total"`
	ContentSHA256 string    `json:"content_sha256"`
	StartedAt     time.Time `json:"started_at"`
	SenderHint    string    `json:"sender_hint,omitempty"`
	SpoolMode     SpoolMode `json:"spool_mode"`
}

// New returns a Manager. SpoolDir is created if missing.
func New(opts Options) (*Manager, error) {
	if opts.SpoolDir == "" {
		return nil, errors.New("session: SpoolDir required")
	}
	if opts.SpoolMode == "" {
		opts.SpoolMode = SpoolModeSparse
	}
	if opts.SpoolMode != SpoolModeSparse && opts.SpoolMode != SpoolModeFiles {
		return nil, fmt.Errorf("session: invalid SpoolMode %q", opts.SpoolMode)
	}
	if opts.FilesTo == "" && opts.OnComplete == nil {
		return nil, errors.New("session: either FilesTo or OnComplete must be set")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.MkdirAll(opts.SpoolDir, 0o755); err != nil {
		return nil, fmt.Errorf("session: mkdir spool: %w", err)
	}
	if opts.FilesTo != "" {
		if err := os.MkdirAll(opts.FilesTo, 0o755); err != nil {
			return nil, fmt.Errorf("session: mkdir files-to: %w", err)
		}
	}
	return &Manager{
		opts:     opts,
		sessions: make(map[framing.SessionID]*Session),
	}, nil
}

// IngestSOH provisions a new session from an SOH frame. Idempotent on
// duplicate SOHs (sender redundancy): the second SOH for an existing
// session is recognised by SID and silently ignored.
func (m *Manager) IngestSOH(soh framing.SOH) error {
	m.stats.SOHsSeen++

	if _, ok := m.sessions[soh.SessionID]; ok {
		// Duplicate SOH (typical when --soh-redundancy>1). Quietly accept.
		return nil
	}
	if m.opts.MaxConcurrent > 0 && len(m.sessions) >= m.opts.MaxConcurrent {
		m.stats.SOHsRejected++
		return nil
	}

	dir := filepath.Join(m.opts.SpoolDir, soh.SessionID.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.stats.SOHsRejected++
		return fmt.Errorf("session: mkdir %s: %w", dir, err)
	}

	s := &Session{
		SID: soh.SessionID,
		Meta: Meta{
			SessionID:     soh.SessionID.String(),
			Version:       "v2",
			Filename:      soh.Name,
			Mode:          fmt.Sprintf("0o%o", soh.Mode&0o777),
			TotalBytes:    soh.TotalBytes,
			ChunkSize:     soh.ChunkSize,
			ChunkTotal:    soh.ChunkTotal,
			ContentSHA256: fmt.Sprintf("%x", soh.ContentSHA256[:]),
			StartedAt:     m.opts.Now().UTC(),
			SpoolMode:     m.opts.SpoolMode,
		},
		Dir:       dir,
		mode:      m.opts.SpoolMode,
		bitmap:    make([]byte, (soh.ChunkTotal+7)/8),
		startedAt: m.opts.Now(),
	}

	if err := writeMeta(dir, s.Meta); err != nil {
		m.stats.SOHsRejected++
		_ = os.RemoveAll(dir)
		return fmt.Errorf("session: write meta: %w", err)
	}

	if m.opts.SpoolMode == SpoolModeSparse {
		f, err := os.OpenFile(filepath.Join(dir, "data.partial"),
			os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			m.stats.SOHsRejected++
			_ = os.RemoveAll(dir)
			return fmt.Errorf("session: create data.partial: %w", err)
		}
		if soh.TotalBytes > 0 {
			if err := f.Truncate(int64(soh.TotalBytes)); err != nil {
				_ = f.Close()
				m.stats.SOHsRejected++
				_ = os.RemoveAll(dir)
				return fmt.Errorf("session: truncate to %d: %w", soh.TotalBytes, err)
			}
		}
		s.data = f
	} else { // SpoolModeFiles
		if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
			m.stats.SOHsRejected++
			_ = os.RemoveAll(dir)
			return fmt.Errorf("session: mkdir chunks: %w", err)
		}
	}

	m.sessions[soh.SessionID] = s
	m.stats.SOHsAccepted++
	m.stats.Active = len(m.sessions)
	return nil
}

// IngestDATA writes one chunk's payload into the session's spool.
// Frames for unknown sessions are dropped (counter incremented).
// When the last chunk arrives, finalize is called.
func (m *Manager) IngestDATA(d framing.DATA, payload []byte) error {
	m.stats.DataFrames++

	s, ok := m.sessions[d.SessionID]
	if !ok {
		m.stats.DataDropped++
		return nil
	}
	if d.ChunkIndex >= s.Meta.ChunkTotal {
		m.stats.DataDropped++
		return nil
	}
	if bitGet(s.bitmap, d.ChunkIndex) {
		m.stats.DataDup++
		return nil
	}

	offset := int64(d.ChunkIndex) * int64(s.Meta.ChunkSize)
	switch s.mode {
	case SpoolModeSparse:
		if _, err := s.data.WriteAt(payload, offset); err != nil {
			return fmt.Errorf("session: pwrite chunk %d: %w", d.ChunkIndex, err)
		}
	case SpoolModeFiles:
		name := fmt.Sprintf("%05d.bin", d.ChunkIndex)
		path := filepath.Join(s.Dir, "chunks", name)
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			return fmt.Errorf("session: write chunk %d: %w", d.ChunkIndex, err)
		}
	}

	bitSet(s.bitmap, d.ChunkIndex)
	s.receivedCnt++

	// Persist the bitmap after every chunk so operators / dashboards
	// can `xxd chunks.bitmap` mid-flight and see exactly what's
	// missing. One small write per chunk (~256 B for a 2K-chunk
	// session); cheap relative to the chunk write itself.
	if err := os.WriteFile(filepath.Join(s.Dir, "chunks.bitmap"), s.bitmap, 0o644); err != nil {
		return fmt.Errorf("session: persist bitmap: %w", err)
	}

	if s.receivedCnt == s.Meta.ChunkTotal {
		return m.finalize(s)
	}
	return nil
}

// finalize verifies the assembled content and moves it to FilesTo (or
// invokes OnComplete) atomically.
func (m *Manager) finalize(s *Session) error {
	// Persist the bitmap so a future "resume" implementation has full state.
	if err := os.WriteFile(filepath.Join(s.Dir, "chunks.bitmap"), s.bitmap, 0o644); err != nil {
		return fmt.Errorf("session: write bitmap: %w", err)
	}

	var assembled string
	switch s.mode {
	case SpoolModeSparse:
		if err := s.data.Sync(); err != nil {
			return fmt.Errorf("session: fsync: %w", err)
		}
		if err := s.data.Close(); err != nil {
			return fmt.Errorf("session: close data.partial: %w", err)
		}
		assembled = filepath.Join(s.Dir, "data.partial")
	case SpoolModeFiles:
		// Concatenate chunks/00000.bin .. chunks/NNNNN.bin into data.partial.
		out, err := os.OpenFile(filepath.Join(s.Dir, "data.partial"),
			os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("session: create combined: %w", err)
		}
		for i := uint32(0); i < s.Meta.ChunkTotal; i++ {
			name := fmt.Sprintf("%05d.bin", i)
			b, err := os.ReadFile(filepath.Join(s.Dir, "chunks", name))
			if err != nil {
				_ = out.Close()
				return fmt.Errorf("session: read chunk %d: %w", i, err)
			}
			if _, err := out.Write(b); err != nil {
				_ = out.Close()
				return fmt.Errorf("session: write combined chunk %d: %w", i, err)
			}
		}
		if err := out.Sync(); err != nil {
			return fmt.Errorf("session: fsync combined: %w", err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("session: close combined: %w", err)
		}
		assembled = filepath.Join(s.Dir, "data.partial")
	}

	// Verify sha256 against the SOH-declared value.
	gotSum, err := fileSHA256(assembled)
	if err != nil {
		return fmt.Errorf("session: rehash for verify: %w", err)
	}
	wantHex := s.Meta.ContentSHA256
	gotHex := fmt.Sprintf("%x", gotSum)
	if gotHex != wantHex {
		m.stats.HashMismatch++
		// Leave the spool dir in place for forensic inspection.
		return fmt.Errorf("session: sha256 mismatch (got %s, want %s); spool retained at %s",
			gotHex, wantHex, s.Dir)
	}

	if m.opts.FilesTo != "" {
		dst := filepath.Join(m.opts.FilesTo, s.Meta.Filename)
		if err := os.Rename(assembled, dst); err != nil {
			return fmt.Errorf("session: rename to %s: %w", dst, err)
		}
		// Mode is per-OS: on Unix this honors all permission bits; on
		// Windows it only toggles the read-only attribute.
		if mode, ok := parseMode(s.Meta.Mode); ok {
			_ = os.Chmod(dst, os.FileMode(mode)&os.ModePerm)
		}
	} else if m.opts.OnComplete != nil {
		if err := m.opts.OnComplete(s, assembled); err != nil {
			return fmt.Errorf("session: OnComplete: %w", err)
		}
	}

	// Drop session from the map and tidy up the spool dir.
	delete(m.sessions, s.SID)
	if err := os.RemoveAll(s.Dir); err != nil {
		return fmt.Errorf("session: cleanup %s: %w", s.Dir, err)
	}
	m.stats.Completed++
	m.stats.Active = len(m.sessions)
	return nil
}

// Stats returns a snapshot of counters.
func (m *Manager) Stats() Stats {
	s := m.stats
	s.Active = len(m.sessions)
	return s
}

// Close releases all in-flight session resources (open files) without
// finalizing them. The spool dirs are left in place for inspection or
// resume.
func (m *Manager) Close() error {
	sids := make([]framing.SessionID, 0, len(m.sessions))
	for sid := range m.sessions {
		sids = append(sids, sid)
	}
	sort.Slice(sids, func(i, j int) bool { return sids[i].String() < sids[j].String() })
	for _, sid := range sids {
		s := m.sessions[sid]
		if s.data != nil {
			_ = s.data.Close()
		}
		// Persist bitmap so a future resume implementation can pick up.
		_ = os.WriteFile(filepath.Join(s.Dir, "chunks.bitmap"), s.bitmap, 0o644)
	}
	return nil
}

// ----- helpers -------------------------------------------------------------

func bitGet(b []byte, i uint32) bool { return b[i/8]&(1<<(i%8)) != 0 }
func bitSet(b []byte, i uint32)      { b[i/8] |= 1 << (i % 8) }

func writeMeta(dir string, m Meta) error {
	tmp := filepath.Join(dir, "meta.json.tmp")
	final := filepath.Join(dir, "meta.json")
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// parseMode parses "0o644" or "0644" to uint32. Returns (0, false) on
// failure; callers should treat that as "no mode override."
func parseMode(s string) (uint32, bool) {
	var m uint32
	if len(s) >= 3 && s[0] == '0' && (s[1] == 'o' || s[1] == 'O') {
		s = s[2:]
	} else if len(s) >= 1 && s[0] == '0' {
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '7' {
			return 0, false
		}
		m = m<<3 | uint32(c-'0')
	}
	return m, true
}
