package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/degreane/datadiode/internal/fileenv"
	"github.com/degreane/datadiode/internal/framing"
	"github.com/degreane/datadiode/internal/reassembly"
	"github.com/degreane/datadiode/internal/transport/udp"
)

// rxConfig is the parsed CLI configuration for `diode --mode=rx`.
type rxConfig struct {
	listen         string
	outPath        string // "-" for stdout
	filesTo        string // directory; if non-empty, payloads are parsed as DDF envelopes
	keyFile        string // path to PSK; when set ONLY signed frames are accepted (ADR-0004)
	maxPending     int
	maxBytes       int
	recentSize     int
	bufferLen      int
	delimiter      string // appended to each delivered message (e.g. "\n"); empty by default
	printStatsOnly bool   // testing: never bind a socket
}

func parseRxFlags(args []string) (rxConfig, error) {
	fs := flag.NewFlagSet("diode --mode=rx", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: diode --mode=rx --listen [host:]port [flags]

Listens for ADR-0002 frames on UDP, verifies each frame's SHA-256,
reassembles chunks into messages, and writes each completed message
to --out (default stdout). The receiver opens no outbound sockets.

flags:`)
		fs.PrintDefaults()
	}

	var c rxConfig
	fs.StringVar(&c.listen, "listen", "", "bind address \"host:port\" or \":port\" (required)")
	fs.StringVar(&c.outPath, "out", "-", "output file path (\"-\" = stdout); raw payload, appended")
	fs.StringVar(&c.filesTo, "files-to", "", "directory to write incoming files (parses each message as a DDF envelope); mutually exclusive with --out")
	fs.StringVar(&c.keyFile, "key-file", "", "path to a pre-shared key file (>=32 bytes raw or >=64 hex chars); when set ONLY signed frames are accepted (ADR-0004)")
	fs.IntVar(&c.maxPending, "max-pending", 1024, "max incomplete messages held at once")
	fs.IntVar(&c.maxBytes, "max-bytes", 64<<20, "max aggregate bytes held in incomplete messages")
	fs.IntVar(&c.recentSize, "recent-msg-cache", 1024, "size of recently-delivered MsgID cache (for REDUNDANT dedup)")
	fs.IntVar(&c.bufferLen, "buffer-len", udp.DefaultReadBufferLen, "UDP read buffer size in bytes (>= framing.MaxFrameLen)")
	fs.StringVar(&c.delimiter, "delimiter", "", "string appended to each delivered message in --out mode (e.g. \\n)")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.listen == "" {
		fs.Usage()
		return c, errors.New("--listen is required")
	}
	minBuf := framing.MaxFrameLen + framing.HMACLen
	if c.bufferLen < minBuf {
		return c, fmt.Errorf("--buffer-len must be >= %d (MaxFrameLen + HMACLen)", minBuf)
	}
	// --files-to and --out are mutually exclusive: each message goes to
	// either a named file or the raw stream sink, not both.
	if c.filesTo != "" && c.outPath != "-" {
		return c, errors.New("--files-to and --out are mutually exclusive")
	}
	return c, nil
}

// runRx is the entry point for `diode --mode=rx`. Called by main.
func runRx(ctx context.Context, args []string) error {
	cfg, err := parseRxFlags(args)
	if err != nil {
		return err
	}

	var key []byte
	if cfg.keyFile != "" {
		key, err = loadKeyFile(cfg.keyFile)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "diode rx: enforcing PSK auth (%d-byte key from %s); unsigned frames will be dropped\n", len(key), cfg.keyFile)
	}

	deliver, closeDeliver, sinkLabel, err := newDeliver(cfg)
	if err != nil {
		return err
	}
	defer closeDeliver()

	asm, err := reassembly.New(deliver, reassembly.Options{
		MaxPending:          cfg.maxPending,
		MaxBytes:            cfg.maxBytes,
		RecentDeliveredSize: cfg.recentSize,
		Key:                 key,
	})
	if err != nil {
		return err
	}

	recv, err := udp.Listen(cfg.listen, udp.WithBufferLen(cfg.bufferLen))
	if err != nil {
		return err
	}
	defer recv.Close()

	fmt.Fprintf(os.Stderr, "diode rx: listening on %s, writing to %s\n", recv.LocalAddr(), sinkLabel)

	err = recv.Run(ctx, asm.Ingest)
	s := asm.Stats()
	fmt.Fprintf(os.Stderr, "diode rx: stopped. frames_in=%d frames_dup=%d frames_ignored=%d msgs_delivered=%d msgs_evicted=%d bytes_pending=%d\n",
		s.FramesIn, s.FramesDup, s.FramesIgnored, s.MsgsDelivered, s.MsgsEvicted, s.BytesPending)
	// ctx.Cancel returns context.Canceled — that's clean shutdown.
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// openOutput returns an io.Writer and a close function for the given
// path. "-" maps to stdout (with a no-op closer). Files are opened in
// append mode (messages are appended as they arrive).
func openOutput(path string) (io.Writer, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

func describeSink(path string) string {
	if path == "-" {
		return "stdout"
	}
	return path
}

// newDeliver builds the DeliverFunc for the chosen sink mode.
// Returns the function, a close (for the underlying file handle), and a
// human-readable label for the startup banner.
func newDeliver(cfg rxConfig) (reassembly.DeliverFunc, func(), string, error) {
	// --files-to mode: each delivered message is parsed as a DDF envelope
	// and written to <dir>/<basename> atomically.
	if cfg.filesTo != "" {
		if err := os.MkdirAll(cfg.filesTo, 0o755); err != nil {
			return nil, nil, "", fmt.Errorf("mkdir %s: %w", cfg.filesTo, err)
		}
		dir := cfg.filesTo
		deliver := func(payload []byte) error {
			h, content, err := fileenv.Decode(payload)
			if err != nil {
				// Application-layer parse failure: log once, drop. Same
				// discipline as bad frames at the transport layer —
				// silent at line rate, counter elsewhere.
				fmt.Fprintf(os.Stderr, "diode rx: dropping non-DDF or invalid envelope (%v)\n", err)
				return nil
			}
			return writeFileAtomic(dir, h.Name, h.Mode, content)
		}
		return deliver, func() {}, "files-to=" + dir, nil
	}

	// --out mode: raw stream sink (unchanged from Sprint 01).
	out, closeOut, err := openOutput(cfg.outPath)
	if err != nil {
		return nil, nil, "", err
	}
	delim := []byte(cfg.delimiter)
	deliver := func(payload []byte) error {
		if _, err := out.Write(payload); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
		if len(delim) > 0 {
			if _, err := out.Write(delim); err != nil {
				return fmt.Errorf("write delimiter: %w", err)
			}
		}
		return nil
	}
	return deliver, closeOut, describeSink(cfg.outPath), nil
}

// writeFileAtomic writes content to <dir>/<name> via a same-directory
// temp file + rename, then chmods to mode. On error the partial file
// is removed. Path-traversal in name has already been rejected by
// fileenv.Decode, but we re-baseline with filepath.Base as defense in
// depth — a Decode() bug would otherwise let a future caller write
// outside dir.
func writeFileAtomic(dir, name string, mode uint32, content []byte) error {
	safeName := filepath.Base(name)
	if safeName == "." || safeName == ".." || safeName == "" {
		return fmt.Errorf("refusing to write file with unsafe name %q", name)
	}

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("rand: %w", err)
	}
	tmp := filepath.Join(dir, "."+safeName+".partial-"+hex.EncodeToString(rnd[:]))
	final := filepath.Join(dir, safeName)

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	// Apply mode (permission bits only) before rename so the final
	// inode never appears with a different mode than intended.
	if err := os.Chmod(tmp, os.FileMode(mode)&os.ModePerm); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	fmt.Fprintf(os.Stderr, "diode rx: wrote %s (%d bytes, mode %o)\n", final, len(content), mode&0o777)
	return nil
}
