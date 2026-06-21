package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/degreane/datadiode/internal/framing"
	"github.com/degreane/datadiode/internal/transport/udp"
)

// txConfig is the parsed CLI configuration for `diode tx`.
type txConfig struct {
	dst         string
	chunkBytes  int
	rateBPS     int64
	redundancy  int
	inputPath   string // "-" for stdin
	maxMessage  int64  // hard cap to keep us well under MsgID/ChunkTotal ceilings
	heartbeatHz int
}

func parseTxFlags(args []string) (txConfig, error) {
	fs := flag.NewFlagSet("diode --mode=tx", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: diode --mode=tx --dst host:port [flags]

Reads bytes from --in (default stdin), chunks them into ADR-0002 frames,
and writes each frame as a UDP datagram to --dst. No reverse path is
opened. With --redundancy=N each frame is sent N times.

flags:`)
		fs.PrintDefaults()
	}

	var c txConfig
	fs.StringVar(&c.dst, "dst", "", "destination host:port (required)")
	fs.IntVar(&c.chunkBytes, "chunk", framing.MaxPayloadLen, "payload bytes per frame (1..MaxPayloadLen)")
	fs.Int64Var(&c.rateBPS, "rate", 0, "wire rate cap in bytes/sec (0 = unlimited)")
	fs.IntVar(&c.redundancy, "redundancy", 1, "send each frame N times (>=1)")
	fs.StringVar(&c.inputPath, "in", "-", "input file path (\"-\" = stdin)")
	fs.Int64Var(&c.maxMessage, "max-message", 64<<20, "abort if a single message exceeds N bytes (default 64 MiB)")
	fs.IntVar(&c.heartbeatHz, "heartbeat-hz", 0, "send N heartbeat frames per second when idle (0 = off)")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.dst == "" {
		fs.Usage()
		return c, errors.New("--dst is required")
	}
	if c.chunkBytes < 1 || c.chunkBytes > framing.MaxPayloadLen {
		return c, fmt.Errorf("--chunk must be in [1, %d]", framing.MaxPayloadLen)
	}
	if c.redundancy < 1 {
		return c, errors.New("--redundancy must be >= 1")
	}
	return c, nil
}

// runTx is the entry point for `diode tx`. It is called from main with
// the post-subcommand args. ctx is cancelled on SIGINT/SIGTERM.
func runTx(ctx context.Context, args []string) error {
	cfg, err := parseTxFlags(args)
	if err != nil {
		return err
	}

	in, closeIn, err := openInput(cfg.inputPath)
	if err != nil {
		return err
	}
	defer closeIn()

	sender, err := udp.Dial(cfg.dst, udp.WithRateBytesPerSec(cfg.rateBPS))
	if err != nil {
		return err
	}
	defer sender.Close()

	tx := &txLoop{
		cfg:    cfg,
		send:   sender.Send,
		out:    make([]byte, 0, framing.MaxFrameLen),
		buf:    make([]byte, cfg.chunkBytes),
		reader: bufio.NewReaderSize(in, 64<<10),
	}
	return tx.run(ctx)
}

// openInput returns an io.Reader and a close function for the given path.
// "-" maps to stdin (with a no-op closer).
func openInput(path string) (io.Reader, func(), error) {
	if path == "-" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// txLoop holds the per-run state for the send loop: monotonic counters,
// reusable buffers, and the bound configuration.
type txLoop struct {
	cfg    txConfig
	send   func([]byte) error
	out    []byte // reusable frame buffer (Encode appends, we reset to [:0])
	buf    []byte // read buffer of size cfg.chunkBytes
	reader *bufio.Reader

	seq   uint64
	msgID uint32
}

// run reads the input as one logical message (everything from the source
// until EOF or maxMessage) and ships it. For sprint 01 we treat the whole
// input as a single message; streaming-message semantics are a follow-up
// (open question in ADR-0002).
func (t *txLoop) run(ctx context.Context) error {
	// Read the full message into memory so we know chunk_total up front
	// (ADR-0002 requires it). Capped by --max-message.
	limited := io.LimitReader(t.reader, t.cfg.maxMessage+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	if int64(len(payload)) > t.cfg.maxMessage {
		return fmt.Errorf("input exceeds --max-message (%d bytes); aborting to avoid runaway transmit", t.cfg.maxMessage)
	}
	if len(payload) == 0 {
		// Send a single FINAL heartbeat so the receiver knows we ran cleanly.
		return t.sendHeartbeat()
	}

	chunkSize := t.cfg.chunkBytes
	total := (len(payload) + chunkSize - 1) / chunkSize
	if total > 0xFFFF {
		return fmt.Errorf("message of %d bytes requires %d chunks; max is %d (raise --chunk or split)",
			len(payload), total, 0xFFFF)
	}

	for i := range total {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := i * chunkSize
		end := min(start+chunkSize, len(payload))
		flags := uint8(0)
		if i == total-1 {
			flags |= framing.FlagFinal
		}
		h := framing.Header{
			Flags:      flags,
			Seq:        t.seq,
			MsgID:      t.msgID,
			ChunkIndex: uint16(i),
			ChunkTotal: uint16(total),
		}
		t.seq++
		if err := t.sendFrame(h, payload[start:end]); err != nil {
			return err
		}
	}
	t.msgID++
	return nil
}

// sendFrame encodes the header+payload and writes it to the wire, plus
// redundancy-1 marked copies for loss tolerance.
func (t *txLoop) sendFrame(h framing.Header, payload []byte) error {
	t.out = t.out[:0]
	frame, err := framing.Encode(t.out, h, payload)
	if err != nil {
		return fmt.Errorf("encode frame seq=%d msg=%d chunk=%d: %w", h.Seq, h.MsgID, h.ChunkIndex, err)
	}
	t.out = frame
	if err := t.send(frame); err != nil {
		return err
	}
	if t.cfg.redundancy <= 1 {
		return nil
	}
	// Duplicate copies carry the REDUNDANT flag so the receiver dedupes.
	hDup := h
	hDup.Flags |= framing.FlagRedundant
	for range t.cfg.redundancy - 1 {
		t.out = t.out[:0]
		frame, err := framing.Encode(t.out, hDup, payload)
		if err != nil {
			return err
		}
		t.out = frame
		if err := t.send(frame); err != nil {
			return err
		}
	}
	return nil
}

// sendHeartbeat emits a single empty FINAL+HEARTBEAT frame. Useful so
// the receiver sees end-of-stream even when the input was empty.
func (t *txLoop) sendHeartbeat() error {
	h := framing.Header{
		Flags:      framing.FlagFinal | framing.FlagHeartbeat,
		Seq:        t.seq,
		MsgID:      t.msgID,
		ChunkIndex: 0,
		ChunkTotal: 1,
	}
	t.seq++
	t.msgID++
	return t.sendFrame(h, nil)
}
