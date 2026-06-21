package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/degreane/datadiode/internal/framing"
	"github.com/degreane/datadiode/internal/session"
	"github.com/degreane/datadiode/internal/transport/udp"
)

// rxConfig is the parsed CLI configuration for `diode --mode=rx`.
type rxConfig struct {
	listen    string
	filesTo   string // directory for completed files (default sink for --send-file flows)
	outPath   string // when set, the assembled payload is written to this path (or "-" for stdout)
	keyFile   string // PSK; when set, only signed frames are accepted (ADR-0004)
	spoolDir  string // parent dir for per-session staging
	spoolMode string // "sparse" (default) or "files"
	maxConc   int    // max concurrent sessions (0 = unlimited)
	bufferLen int    // UDP read buffer size
}

func defaultSpoolDir() string {
	if runtime.GOOS == "windows" {
		if t := os.Getenv("TEMP"); t != "" {
			return t + `\diode`
		}
		return `C:\Windows\Temp\diode`
	}
	return "/var/spool/diode"
}

func parseRxFlags(args []string) (rxConfig, error) {
	fs := flag.NewFlagSet("diode --mode=rx", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: diode --mode=rx --listen [host:]port (--files-to dir | --out path)

Listens for v2 frames on UDP. An SOH frame opens a per-session spool
directory under --spool; subsequent DATA frames are written there
(sparse pwrite by default; per-chunk files with --spool-mode=files).
When all chunks have been received, the sha256 is verified and the
assembled file is atomic-renamed into --files-to (or written to --out).

flags:`)
		fs.PrintDefaults()
	}

	var c rxConfig
	fs.StringVar(&c.listen, "listen", "", "bind address \"host:port\" or \":port\" (required)")
	fs.StringVar(&c.filesTo, "files-to", "", "directory where completed files are atomically renamed")
	fs.StringVar(&c.outPath, "out", "", "write the assembled payload to this path (\"-\" = stdout); mutually exclusive with --files-to")
	fs.StringVar(&c.keyFile, "key-file", "", "PSK file path; when set ONLY signed frames are accepted (ADR-0004)")
	fs.StringVar(&c.spoolDir, "spool", defaultSpoolDir(), "parent directory for per-session staging")
	fs.StringVar(&c.spoolMode, "spool-mode", "sparse", "per-session layout: \"sparse\" (one data.partial + bitmap) or \"files\" (per-chunk files)")
	fs.IntVar(&c.maxConc, "max-concurrent", 0, "max simultaneous sessions (0 = unlimited)")
	fs.IntVar(&c.bufferLen, "buffer-len", udp.DefaultReadBufferLen, "UDP read buffer size in bytes (>= MaxFrameLen)")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.listen == "" {
		fs.Usage()
		return c, errors.New("--listen is required")
	}
	if c.filesTo == "" && c.outPath == "" {
		fs.Usage()
		return c, errors.New("one of --files-to or --out is required")
	}
	if c.filesTo != "" && c.outPath != "" {
		return c, errors.New("--files-to and --out are mutually exclusive")
	}
	if c.bufferLen < framing.MaxFrameLen {
		return c, fmt.Errorf("--buffer-len must be >= %d (MaxFrameLen)", framing.MaxFrameLen)
	}
	return c, nil
}

// runRx is the entry point for `diode --mode=rx`.
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
		fmt.Fprintf(os.Stderr, "diode rx: enforcing PSK auth (%d-byte key); unsigned frames will be dropped\n", len(key))
	}

	// Build the session manager. For --out mode we supply an
	// OnComplete callback that copies the assembled file to the
	// configured sink; for --files-to we let the manager rename.
	opts := session.Options{
		SpoolDir:      cfg.spoolDir,
		FilesTo:       cfg.filesTo,
		SpoolMode:     session.SpoolMode(cfg.spoolMode),
		MaxConcurrent: cfg.maxConc,
	}
	if cfg.outPath != "" {
		opts.OnComplete = func(s *session.Session, path string) error {
			return streamToOut(cfg.outPath, path)
		}
	}
	mgr, err := session.New(opts)
	if err != nil {
		return err
	}
	defer mgr.Close()

	recv, err := udp.Listen(cfg.listen, udp.WithBufferLen(cfg.bufferLen))
	if err != nil {
		return err
	}
	defer recv.Close()

	sinkLabel := "files-to=" + cfg.filesTo
	if cfg.outPath != "" {
		sinkLabel = "out=" + describeSink(cfg.outPath)
	}
	fmt.Fprintf(os.Stderr, "diode rx: listening on %s, spool=%s, mode=%s, %s\n",
		recv.LocalAddr(), cfg.spoolDir, cfg.spoolMode, sinkLabel)

	handle := func(frame []byte) error {
		isSOH, _, ok := framing.PeekKind(frame)
		if !ok {
			// Bad magic/version/short — silently drop. Operators see
			// no counter for this today; could add one.
			return nil
		}
		if isSOH {
			soh, err := framing.DecodeSOH(frame, key)
			if err != nil {
				return nil // silently drop bad/unauthenticated SOH
			}
			return mgr.IngestSOH(soh)
		}
		d, payload, err := framing.DecodeDATA(frame, key)
		if err != nil {
			return nil // silently drop bad/unauthenticated DATA
		}
		return mgr.IngestDATA(d, payload)
	}

	runErr := recv.Run(ctx, handle)
	s := mgr.Stats()
	fmt.Fprintf(os.Stderr,
		"diode rx: stopped. soh_seen=%d soh_accepted=%d soh_rejected=%d data_frames=%d data_dropped=%d data_dup=%d completed=%d hash_mismatch=%d active=%d\n",
		s.SOHsSeen, s.SOHsAccepted, s.SOHsRejected,
		s.DataFrames, s.DataDropped, s.DataDup,
		s.Completed, s.HashMismatch, s.Active)
	if runErr == nil || errors.Is(runErr, context.Canceled) {
		return nil
	}
	return runErr
}

// streamToOut copies an assembled file to the configured --out sink.
// "-" maps to stdout; any other path is opened in append mode.
func streamToOut(outPath, assembled string) error {
	src, err := os.Open(assembled)
	if err != nil {
		return fmt.Errorf("open assembled: %w", err)
	}
	defer src.Close()

	var sink io.Writer
	if outPath == "-" {
		sink = os.Stdout
	} else {
		f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("open out %s: %w", outPath, err)
		}
		defer f.Close()
		sink = f
	}
	if _, err := io.Copy(sink, src); err != nil {
		return fmt.Errorf("copy to sink: %w", err)
	}
	// We owned the spool file via OnComplete contract; remove it now.
	return os.Remove(assembled)
}

func describeSink(path string) string {
	if path == "-" {
		return "stdout"
	}
	return path
}
