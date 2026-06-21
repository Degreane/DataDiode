package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/degreane/datadiode/internal/framing"
	"github.com/degreane/datadiode/internal/reassembly"
	"github.com/degreane/datadiode/internal/transport/udp"
)

// rxConfig is the parsed CLI configuration for `diode --mode=rx`.
type rxConfig struct {
	listen         string
	outPath        string // "-" for stdout
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
	fs.StringVar(&c.outPath, "out", "-", "output file path (\"-\" = stdout); file is opened for append")
	fs.IntVar(&c.maxPending, "max-pending", 1024, "max incomplete messages held at once")
	fs.IntVar(&c.maxBytes, "max-bytes", 64<<20, "max aggregate bytes held in incomplete messages")
	fs.IntVar(&c.recentSize, "recent-msg-cache", 1024, "size of recently-delivered MsgID cache (for REDUNDANT dedup)")
	fs.IntVar(&c.bufferLen, "buffer-len", udp.DefaultReadBufferLen, "UDP read buffer size in bytes (>= framing.MaxFrameLen)")
	fs.StringVar(&c.delimiter, "delimiter", "", "string appended to each delivered message (e.g. \\n)")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.listen == "" {
		fs.Usage()
		return c, errors.New("--listen is required")
	}
	if c.bufferLen < framing.MaxFrameLen {
		return c, fmt.Errorf("--buffer-len must be >= %d (framing.MaxFrameLen)", framing.MaxFrameLen)
	}
	return c, nil
}

// runRx is the entry point for `diode --mode=rx`. Called by main.
func runRx(ctx context.Context, args []string) error {
	cfg, err := parseRxFlags(args)
	if err != nil {
		return err
	}

	out, closeOut, err := openOutput(cfg.outPath)
	if err != nil {
		return err
	}
	defer closeOut()

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

	asm, err := reassembly.New(deliver, reassembly.Options{
		MaxPending:          cfg.maxPending,
		MaxBytes:            cfg.maxBytes,
		RecentDeliveredSize: cfg.recentSize,
	})
	if err != nil {
		return err
	}

	recv, err := udp.Listen(cfg.listen, udp.WithBufferLen(cfg.bufferLen))
	if err != nil {
		return err
	}
	defer recv.Close()

	fmt.Fprintf(os.Stderr, "diode rx: listening on %s, writing to %s\n", recv.LocalAddr(), describeSink(cfg.outPath))

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
