package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/degreane/datadiode/internal/framing"
	"github.com/degreane/datadiode/internal/manifest"
	"github.com/degreane/datadiode/internal/transport/udp"
)

// txConfig is the parsed CLI configuration for `diode --mode=tx`.
type txConfig struct {
	dst               string
	chunkSize         int
	rateBPS           int64
	redundancy        int
	sohRedundancy     int
	sohInterval       int    // re-emit SOH every N data frames (0 = disabled)
	redundancyOrder   string // "spread" (default) or "consecutive"
	inputPath         string
	sendFile          string
	streamName        string
	keyFile           string
	maxBytes          int64
	senderStateDir    string
	archive           bool
	manifestRotateB   int64
	sessionIDOverride string
	resendSID         string
	resendLatest      string
}

func parseTxFlags(args []string) (txConfig, error) {
	fs := flag.NewFlagSet("diode --mode=tx", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: diode --mode=tx --dst host:port (--send-file path | --in path | --resend sid)

Ships an SOH preamble + DATA chunks over one-way UDP. On success the
file is archived under <sender-state>/sent/ and a record is appended
to <sender-state>/manifest.jsonl, both enabled by default.

flags:`)
		fs.PrintDefaults()
	}

	var c txConfig
	fs.StringVar(&c.dst, "dst", "", "destination host:port (required)")
	fs.IntVar(&c.chunkSize, "chunk-size", framing.MaxPayloadLen, "payload bytes per DATA frame (1..MaxPayloadLen)")
	fs.Int64Var(&c.rateBPS, "rate", 0, "wire rate cap in bytes/sec (0 = unlimited)")
	fs.IntVar(&c.redundancy, "redundancy", 1, "ship each DATA frame N times (>=1)")
	fs.IntVar(&c.sohRedundancy, "soh-redundancy", 3, "ship the SOH frame N times at session start (>=1)")
	fs.IntVar(&c.sohInterval, "soh-interval", 256, "also re-emit SOH every N data frames (0 = off)")
	fs.StringVar(&c.redundancyOrder, "redundancy-order", "spread", "\"spread\" (round-robin passes — survives bursts) or \"consecutive\" (N copies back-to-back)")
	fs.StringVar(&c.sendFile, "send-file", "", "path to a file to send (preserves name + permission bits)")
	fs.StringVar(&c.inputPath, "in", "", "send arbitrary bytes from this path (use \"-\" for stdin)")
	fs.StringVar(&c.streamName, "name", "", "logical filename to advertise when sending via --in (default \"stream.bin\")")
	fs.StringVar(&c.keyFile, "key-file", "", "PSK file path; when set every frame is HMAC-signed (ADR-0004)")
	fs.Int64Var(&c.maxBytes, "max-bytes", 4<<30, "abort if the input exceeds N bytes (default 4 GiB)")
	fs.StringVar(&c.senderStateDir, "sender-state", defaultSenderStateDir(), "directory for sent/ archives and manifest.jsonl")
	archive := fs.Bool("archive", true, "archive the successfully-sent file under <sender-state>/sent/")
	noArchive := fs.Bool("no-archive", false, "shortcut for --archive=false")
	fs.Int64Var(&c.manifestRotateB, "manifest-rotate-bytes", 0, "rotate <sender-state>/manifest.jsonl when it reaches N bytes (0 = never)")
	fs.StringVar(&c.sessionIDOverride, "session-id", "", "force a specific session_id (hex UUID, with or without dashes); mutually exclusive with --resend")
	fs.StringVar(&c.resendSID, "resend", "", "re-ship the previously-archived session with this id (looks it up in manifest.jsonl)")
	fs.StringVar(&c.resendLatest, "resend-latest", "", "re-ship the most recent manifest entry whose filename matches this basename")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
	c.archive = *archive && !*noArchive

	if c.dst == "" {
		fs.Usage()
		return c, errors.New("--dst is required")
	}
	if c.chunkSize < 1 || c.chunkSize > framing.MaxPayloadLen {
		return c, fmt.Errorf("--chunk-size must be in [1, %d]", framing.MaxPayloadLen)
	}
	if c.redundancy < 1 || c.sohRedundancy < 1 {
		return c, errors.New("--redundancy and --soh-redundancy must be >= 1")
	}
	if c.sohInterval < 0 {
		return c, errors.New("--soh-interval must be >= 0")
	}
	if c.redundancyOrder != "spread" && c.redundancyOrder != "consecutive" {
		return c, errors.New("--redundancy-order must be \"spread\" or \"consecutive\"")
	}
	// Mutually exclusive source flags. Exactly one of {send-file, in, resend, resend-latest}.
	srcCount := 0
	for _, s := range []string{c.sendFile, c.inputPath, c.resendSID, c.resendLatest} {
		if s != "" {
			srcCount++
		}
	}
	if srcCount == 0 {
		fs.Usage()
		return c, errors.New("one of --send-file, --in, --resend, or --resend-latest is required")
	}
	if srcCount > 1 {
		return c, errors.New("--send-file, --in, --resend, --resend-latest are mutually exclusive")
	}
	if c.sessionIDOverride != "" && (c.resendSID != "" || c.resendLatest != "") {
		return c, errors.New("--session-id is mutually exclusive with --resend / --resend-latest")
	}
	return c, nil
}

// runTx is the entry point for `diode --mode=tx`.
func runTx(ctx context.Context, args []string) error {
	cfg, err := parseTxFlags(args)
	if err != nil {
		return err
	}

	var key []byte
	if cfg.keyFile != "" {
		key, err = loadKeyFile(cfg.keyFile)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "diode tx: signing every frame with %d-byte PSK from %s\n", len(key), cfg.keyFile)
	}

	manifestPath := filepath.Join(cfg.senderStateDir, "manifest.jsonl")
	archiveDir := filepath.Join(cfg.senderStateDir, "sent")

	// Resolve the input depending on flag combination.
	plan, err := resolveSendPlan(cfg, manifestPath)
	if err != nil {
		return err
	}

	if int64(plan.totalBytes) > cfg.maxBytes {
		return fmt.Errorf("input exceeds --max-bytes (%d > %d)", plan.totalBytes, cfg.maxBytes)
	}

	sender, err := udp.Dial(cfg.dst, udp.WithRateBytesPerSec(cfg.rateBPS))
	if err != nil {
		return err
	}
	defer sender.Close()

	chunkTotal := uint32((plan.totalBytes + uint64(cfg.chunkSize) - 1) / uint64(cfg.chunkSize))
	if chunkTotal == 0 {
		chunkTotal = 1
	}

	startedAt := time.Now().UTC()
	soh := framing.SOH{
		SessionID:     plan.sid,
		ChunkTotal:    chunkTotal,
		ChunkSize:     uint32(cfg.chunkSize),
		TotalBytes:    plan.totalBytes,
		ContentSHA256: plan.sha,
		Mode:          plan.mode,
		Name:          plan.name,
	}

	fmt.Fprintf(os.Stderr, "diode tx: session=%s file=%s bytes=%d chunks=%d chunk-size=%d redundancy=%dx (order=%s) soh-redundancy=%dx soh-interval=%d\n",
		plan.sid.String(), plan.name, plan.totalBytes, chunkTotal, cfg.chunkSize,
		cfg.redundancy, cfg.redundancyOrder, cfg.sohRedundancy, cfg.sohInterval)

	if err := shipSession(ctx, sender, soh, plan.content, cfg, key, chunkTotal); err != nil {
		return err
	}

	completedAt := time.Now().UTC()

	// Archive + manifest.
	var snapshotPath string
	if cfg.archive {
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "diode tx: warning: mkdir archive %s: %v\n", archiveDir, err)
		} else if plan.sourcePath != "" {
			snapshotPath = filepath.Join(archiveDir, archiveStamp(startedAt)+"__"+plan.name)
			if _, err := copyFile(plan.sourcePath, snapshotPath, os.FileMode(plan.mode&0o777)); err != nil {
				fmt.Fprintf(os.Stderr, "diode tx: warning: archive %s: %v\n", snapshotPath, err)
				snapshotPath = ""
			}
		}
		// Note: resend without a snapshot still writes a manifest line; snapshot stays "" in that case.
	}

	if err := writeManifest(manifestPath, cfg.manifestRotateB, manifest.Record{
		StartedAt:     startedAt,
		CompletedAt:   completedAt,
		SessionID:     plan.sid.String(),
		Filename:      plan.name,
		TotalBytes:    plan.totalBytes,
		ChunkSize:     uint32(cfg.chunkSize),
		ChunkTotal:    chunkTotal,
		ContentSHA256: hex.EncodeToString(plan.sha[:]),
		Destination:   cfg.dst,
		Mode:          fmt.Sprintf("0o%o", plan.mode&0o777),
		Redundancy:    cfg.redundancy,
		SOHRedundancy: cfg.sohRedundancy,
		Signed:        key != nil,
		Snapshot:      snapshotPath,
		TxStatus:      "sent",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "diode tx: warning: manifest append: %v\n", err)
	}

	totalFrames := cfg.sohRedundancy + int(chunkTotal)*cfg.redundancy
	if cfg.sohInterval > 0 {
		totalFrames += int(chunkTotal) * cfg.redundancy / cfg.sohInterval
	}
	fmt.Fprintf(os.Stderr, "diode tx: done (%d chunks × %d copies + SOH × %d + interleaved-SOH = ~%d frames)\n",
		chunkTotal, cfg.redundancy, cfg.sohRedundancy, totalFrames)
	return nil
}

// sendPlan is the resolved input for one tx invocation, regardless of
// whether it came from --send-file, --in, --resend, or --resend-latest.
type sendPlan struct {
	sid        framing.SessionID
	name       string
	mode       uint32
	content    []byte
	totalBytes uint64
	sha        [sha256.Size]byte
	sourcePath string // original path or snapshot path, for archive bookkeeping
}

// resolveSendPlan dispatches on the source flag combination.
func resolveSendPlan(cfg txConfig, manifestPath string) (sendPlan, error) {
	switch {
	case cfg.resendSID != "" || cfg.resendLatest != "":
		return planFromResend(cfg, manifestPath)
	case cfg.sendFile != "":
		return planFromFile(cfg)
	default: // --in
		return planFromIn(cfg)
	}
}

func planFromFile(cfg txConfig) (sendPlan, error) {
	st, err := os.Stat(cfg.sendFile)
	if err != nil {
		return sendPlan{}, fmt.Errorf("stat %s: %w", cfg.sendFile, err)
	}
	if st.IsDir() {
		return sendPlan{}, fmt.Errorf("%s is a directory; one-file-per-session only", cfg.sendFile)
	}
	content, err := os.ReadFile(cfg.sendFile)
	if err != nil {
		return sendPlan{}, fmt.Errorf("read %s: %w", cfg.sendFile, err)
	}
	sid, err := pickSessionID(cfg.sessionIDOverride)
	if err != nil {
		return sendPlan{}, err
	}
	return sendPlan{
		sid:        sid,
		name:       filepath.Base(cfg.sendFile),
		mode:       uint32(st.Mode().Perm()),
		content:    content,
		totalBytes: uint64(len(content)),
		sha:        sha256.Sum256(content),
		sourcePath: cfg.sendFile,
	}, nil
}

func planFromIn(cfg txConfig) (sendPlan, error) {
	var content []byte
	var err error
	if cfg.inputPath == "-" {
		content, err = io.ReadAll(os.Stdin)
	} else {
		content, err = os.ReadFile(cfg.inputPath)
	}
	if err != nil {
		return sendPlan{}, fmt.Errorf("read input: %w", err)
	}
	name := cfg.streamName
	if name == "" {
		name = "stream.bin"
	}
	sid, err := pickSessionID(cfg.sessionIDOverride)
	if err != nil {
		return sendPlan{}, err
	}
	srcPath := cfg.inputPath
	if cfg.inputPath == "-" {
		srcPath = "" // stdin can't be archived as a path
	}
	return sendPlan{
		sid:        sid,
		name:       name,
		mode:       0o644,
		content:    content,
		totalBytes: uint64(len(content)),
		sha:        sha256.Sum256(content),
		sourcePath: srcPath,
	}, nil
}

func planFromResend(cfg txConfig, manifestPath string) (sendPlan, error) {
	records, err := manifest.ReadAll(manifestPath)
	if err != nil {
		return sendPlan{}, fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}
	if len(records) == 0 {
		return sendPlan{}, fmt.Errorf("manifest %s is empty (no prior sends to resend)", manifestPath)
	}
	var (
		rec manifest.Record
		ok  bool
	)
	if cfg.resendSID != "" {
		rec, ok = manifest.FindLatestBySID(records, cfg.resendSID)
		if !ok {
			return sendPlan{}, fmt.Errorf("no manifest entry with session_id=%s", cfg.resendSID)
		}
	} else {
		rec, ok = manifest.FindLatestByFilename(records, cfg.resendLatest)
		if !ok {
			return sendPlan{}, fmt.Errorf("no manifest entry with filename=%s", cfg.resendLatest)
		}
	}

	// Source is the snapshot if archived; otherwise fall back to the
	// original path if it still exists.
	src := rec.Snapshot
	if src == "" {
		return sendPlan{}, fmt.Errorf("manifest entry has no snapshot (sent with --no-archive); cannot resend without the source file. session_id=%s", rec.SessionID)
	}
	if _, err := os.Stat(src); err != nil {
		return sendPlan{}, fmt.Errorf("snapshot %s no longer exists: %w", src, err)
	}
	content, err := os.ReadFile(src)
	if err != nil {
		return sendPlan{}, fmt.Errorf("read snapshot %s: %w", src, err)
	}
	gotSHA := sha256.Sum256(content)
	if hex.EncodeToString(gotSHA[:]) != rec.ContentSHA256 {
		return sendPlan{}, fmt.Errorf("snapshot %s sha256 differs from manifest record; refusing to resend (would mismatch receiver)", src)
	}

	sid, err := parseSID(rec.SessionID)
	if err != nil {
		return sendPlan{}, fmt.Errorf("manifest sid %q: %w", rec.SessionID, err)
	}
	mode, _ := parseModeOrDefault(rec.Mode, 0o644)
	return sendPlan{
		sid:        sid,
		name:       rec.Filename,
		mode:       mode,
		content:    content,
		totalBytes: uint64(len(content)),
		sha:        gotSHA,
		sourcePath: src, // re-archive on top is a no-op since snapshot==source path
	}, nil
}

// pickSessionID generates a fresh UUID v4 unless override is non-empty,
// in which case it parses the override (with or without dashes).
func pickSessionID(override string) (framing.SessionID, error) {
	if override == "" {
		var s framing.SessionID
		if _, err := rand.Read(s[:]); err != nil {
			return s, fmt.Errorf("session id: %w", err)
		}
		s[6] = (s[6] & 0x0F) | 0x40
		s[8] = (s[8] & 0x3F) | 0x80
		return s, nil
	}
	return parseSID(override)
}

func parseSID(s string) (framing.SessionID, error) {
	var sid framing.SessionID
	clean := strings.ReplaceAll(s, "-", "")
	if len(clean) != 32 {
		return sid, fmt.Errorf("session id must be 32 hex chars (with or without dashes); got len=%d", len(clean))
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return sid, fmt.Errorf("session id hex decode: %w", err)
	}
	copy(sid[:], b)
	return sid, nil
}

func parseModeOrDefault(s string, def uint32) (uint32, bool) {
	if s == "" {
		return def, false
	}
	cur := s
	if strings.HasPrefix(cur, "0o") || strings.HasPrefix(cur, "0O") {
		cur = cur[2:]
	} else if strings.HasPrefix(cur, "0") && len(cur) > 1 {
		cur = cur[1:]
	}
	var m uint32
	for _, ch := range []byte(cur) {
		if ch < '0' || ch > '7' {
			return def, false
		}
		m = m<<3 | uint32(ch-'0')
	}
	return m, true
}

// shipSession is the inner send loop: SOH × N, then DATA frames in the
// configured redundancy order, with optional interleaved-SOH every N
// chunks.
func shipSession(ctx context.Context, sender *udp.Sender, soh framing.SOH, content []byte, cfg txConfig, key []byte, chunkTotal uint32) error {
	var buf []byte

	emitSOH := func(redundant bool) error {
		s := soh
		if redundant {
			s.Flags |= framing.FlagRedundant
		}
		buf = buf[:0]
		frame, err := framing.EncodeSOH(buf, s, key)
		if err != nil {
			return fmt.Errorf("encode SOH: %w", err)
		}
		buf = frame
		return sender.Send(frame)
	}
	emitDATA := func(idx uint32, redundant bool) error {
		start := int(idx) * cfg.chunkSize
		end := start + cfg.chunkSize
		if end > len(content) {
			end = len(content)
		}
		flags := uint8(0)
		if idx == chunkTotal-1 {
			flags |= framing.FlagFinal
		}
		if redundant {
			flags |= framing.FlagRedundant
		}
		d := framing.DATA{Flags: flags, SessionID: soh.SessionID, ChunkIndex: idx}
		buf = buf[:0]
		frame, err := framing.EncodeDATA(buf, d, content[start:end], key)
		if err != nil {
			return fmt.Errorf("encode DATA chunk=%d: %w", idx, err)
		}
		buf = frame
		return sender.Send(frame)
	}

	// 1. Initial SOH × N.
	for i := 0; i < cfg.sohRedundancy; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emitSOH(i > 0); err != nil {
			return err
		}
	}

	// 2. DATA frames in the configured order, with optional interleaved-SOH.
	emitted := 0
	maybeInterleave := func() error {
		if cfg.sohInterval > 0 && emitted > 0 && emitted%cfg.sohInterval == 0 {
			return emitSOH(true)
		}
		return nil
	}

	if cfg.redundancyOrder == "consecutive" {
		for i := uint32(0); i < chunkTotal; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			for r := 0; r < cfg.redundancy; r++ {
				if err := emitDATA(i, r > 0); err != nil {
					return err
				}
				emitted++
				if err := maybeInterleave(); err != nil {
					return err
				}
			}
		}
	} else { // spread (default): N round-robin passes
		for r := 0; r < cfg.redundancy; r++ {
			for i := uint32(0); i < chunkTotal; i++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := emitDATA(i, r > 0); err != nil {
					return err
				}
				emitted++
				if err := maybeInterleave(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// writeManifest opens the manifest writer, appends one record, and closes.
func writeManifest(path string, rotateBytes int64, rec manifest.Record) error {
	w, err := manifest.Open(path, rotateBytes)
	if err != nil {
		return err
	}
	defer w.Close()
	return w.Append(rec)
}
