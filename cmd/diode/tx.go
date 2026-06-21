package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/degreane/datadiode/internal/framing"
	"github.com/degreane/datadiode/internal/transport/udp"
)

// txConfig is the parsed CLI configuration for `diode --mode=tx`.
type txConfig struct {
	dst           string
	chunkSize     int
	rateBPS       int64
	redundancy    int    // copies per DATA frame (>=1)
	sohRedundancy int    // copies of the SOH (>=1)
	inputPath     string // "-" for stdin, or a file path (used when --send-file not set)
	sendFile      string // canonical file-transfer flag (preserves filename + mode)
	streamName    string // logical name shipped with --in flow (if empty, derived)
	keyFile       string // path to PSK file; opt-in HMAC per ADR-0004
	maxBytes      int64  // hard cap on session size
}

func parseTxFlags(args []string) (txConfig, error) {
	fs := flag.NewFlagSet("diode --mode=tx", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: diode --mode=tx --dst host:port (--send-file path | --in path)

Reads a file (--send-file) or arbitrary bytes (--in), computes a sha256
+ chunk plan, ships an SOH frame followed by DATA frames over one-way
UDP. With --key-file every frame is HMAC-signed (ADR-0004).

flags:`)
		fs.PrintDefaults()
	}

	var c txConfig
	fs.StringVar(&c.dst, "dst", "", "destination host:port (required)")
	fs.IntVar(&c.chunkSize, "chunk-size", framing.MaxPayloadLen, "payload bytes per DATA frame (1..MaxPayloadLen)")
	fs.Int64Var(&c.rateBPS, "rate", 0, "wire rate cap in bytes/sec (0 = unlimited)")
	fs.IntVar(&c.redundancy, "redundancy", 1, "send each DATA frame N times (>=1)")
	fs.IntVar(&c.sohRedundancy, "soh-redundancy", 3, "send the SOH frame N times for loss tolerance (>=1)")
	fs.StringVar(&c.sendFile, "send-file", "", "path to a file to send (preserves name + permission bits)")
	fs.StringVar(&c.inputPath, "in", "", "send arbitrary bytes from this path (use \"-\" for stdin)")
	fs.StringVar(&c.streamName, "name", "", "logical filename to advertise when sending via --in (default \"stream.bin\")")
	fs.StringVar(&c.keyFile, "key-file", "", "PSK file path; when set every frame is HMAC-signed (ADR-0004)")
	fs.Int64Var(&c.maxBytes, "max-bytes", 4<<30, "abort if the input exceeds N bytes (default 4 GiB)")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
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
	if c.sendFile == "" && c.inputPath == "" {
		fs.Usage()
		return c, errors.New("one of --send-file or --in is required")
	}
	if c.sendFile != "" && c.inputPath != "" {
		return c, errors.New("--send-file and --in are mutually exclusive")
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

	// Read the full input into memory + compute sha256 + decide chunking.
	// v2 sender requires upfront size; stdin streaming is a deferred feature.
	content, name, mode, err := readInput(cfg)
	if err != nil {
		return err
	}
	if int64(len(content)) > cfg.maxBytes {
		return fmt.Errorf("input exceeds --max-bytes (%d > %d)", len(content), cfg.maxBytes)
	}

	sender, err := udp.Dial(cfg.dst, udp.WithRateBytesPerSec(cfg.rateBPS))
	if err != nil {
		return err
	}
	defer sender.Close()

	// Build session metadata.
	var sid framing.SessionID
	if _, err := rand.Read(sid[:]); err != nil {
		return fmt.Errorf("session id: %w", err)
	}
	// Mark this as RFC 4122 v4 (best-effort; the receiver doesn't care
	// about the bit pattern, only that it's distinct).
	sid[6] = (sid[6] & 0x0F) | 0x40
	sid[8] = (sid[8] & 0x3F) | 0x80

	chunkTotal := uint32((len(content) + cfg.chunkSize - 1) / cfg.chunkSize)
	if chunkTotal == 0 {
		// Empty payload: still send a single 0-byte chunk so the
		// receiver registers the session and produces an empty file.
		chunkTotal = 1
	}

	soh := framing.SOH{
		SessionID:     sid,
		ChunkTotal:    chunkTotal,
		ChunkSize:     uint32(cfg.chunkSize),
		TotalBytes:    uint64(len(content)),
		ContentSHA256: sha256.Sum256(content),
		Mode:          mode,
		Name:          name,
	}

	fmt.Fprintf(os.Stderr, "diode tx: session=%s file=%s bytes=%d chunks=%d chunk-size=%d redundancy=%dx soh-redundancy=%dx\n",
		sid.String(), name, len(content), chunkTotal, cfg.chunkSize, cfg.redundancy, cfg.sohRedundancy)

	// 1. Ship SOH N times.
	var frameBuf []byte
	for i := 0; i < cfg.sohRedundancy; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		s := soh
		if i > 0 {
			s.Flags |= framing.FlagRedundant
		}
		frameBuf = frameBuf[:0]
		frame, err := framing.EncodeSOH(frameBuf, s, key)
		if err != nil {
			return fmt.Errorf("encode SOH: %w", err)
		}
		frameBuf = frame
		if err := sender.Send(frame); err != nil {
			return fmt.Errorf("send SOH: %w", err)
		}
	}

	// 2. Ship DATA frames in order, each N times for redundancy.
	for i := uint32(0); i < chunkTotal; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := int(i) * cfg.chunkSize
		end := start + cfg.chunkSize
		if end > len(content) {
			end = len(content)
		}
		payload := content[start:end]
		flags := uint8(0)
		if i == chunkTotal-1 {
			flags |= framing.FlagFinal
		}
		base := framing.DATA{Flags: flags, SessionID: sid, ChunkIndex: i}

		for r := 0; r < cfg.redundancy; r++ {
			d := base
			if r > 0 {
				d.Flags |= framing.FlagRedundant
			}
			frameBuf = frameBuf[:0]
			frame, err := framing.EncodeDATA(frameBuf, d, payload, key)
			if err != nil {
				return fmt.Errorf("encode DATA chunk=%d: %w", i, err)
			}
			frameBuf = frame
			if err := sender.Send(frame); err != nil {
				return fmt.Errorf("send DATA chunk=%d: %w", i, err)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "diode tx: done (%d chunks × %d copies + %d SOH copies = %d frames)\n",
		chunkTotal, cfg.redundancy, cfg.sohRedundancy,
		int(chunkTotal)*cfg.redundancy+cfg.sohRedundancy)
	return nil
}

// readInput reads the configured source and returns (content, name, mode).
// For --send-file we use the file's basename and permission bits; for
// --in we use --name (or "stream.bin") and a default mode.
func readInput(cfg txConfig) ([]byte, string, uint32, error) {
	if cfg.sendFile != "" {
		st, err := os.Stat(cfg.sendFile)
		if err != nil {
			return nil, "", 0, fmt.Errorf("stat %s: %w", cfg.sendFile, err)
		}
		if st.IsDir() {
			return nil, "", 0, fmt.Errorf("%s is a directory; one-file-per-session only", cfg.sendFile)
		}
		content, err := os.ReadFile(cfg.sendFile)
		if err != nil {
			return nil, "", 0, fmt.Errorf("read %s: %w", cfg.sendFile, err)
		}
		return content, filepath.Base(cfg.sendFile), uint32(st.Mode().Perm()), nil
	}

	// --in path
	var content []byte
	var err error
	if cfg.inputPath == "-" {
		content, err = io.ReadAll(os.Stdin)
	} else {
		content, err = os.ReadFile(cfg.inputPath)
	}
	if err != nil {
		return nil, "", 0, fmt.Errorf("read input: %w", err)
	}
	name := cfg.streamName
	if name == "" {
		name = "stream.bin"
	}
	return content, name, 0o644, nil
}
