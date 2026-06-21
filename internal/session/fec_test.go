package session

import (
	"bytes"
	"crypto/sha256"
	"path/filepath"
	"testing"

	"github.com/degreane/datadiode/internal/fec"
	"github.com/degreane/datadiode/internal/framing"
)

// TestFEC_ReconstructsMissingDataChunk: send 5 data chunks + 1 parity
// for a single group, skip chunk[2], the receiver should reconstruct
// it via XOR and complete the session.
func TestFEC_ReconstructsMissingDataChunk(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	out := filepath.Join(t.TempDir(), "out")
	m, err := New(Options{
		SpoolDir: spool, FilesTo: out, SpoolMode: SpoolModeSparse,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const chunkSize = 16
	const groupSize uint32 = 5
	// 5 distinct chunks; bytes carefully chosen so reconstruction can be verified.
	chunks := [][]byte{
		bytes.Repeat([]byte{0x01}, chunkSize),
		bytes.Repeat([]byte{0x02}, chunkSize),
		bytes.Repeat([]byte{0x04}, chunkSize),
		bytes.Repeat([]byte{0x08}, chunkSize),
		bytes.Repeat([]byte{0x10}, chunkSize),
	}
	full := bytes.Join(chunks, nil)
	parity, err := fec.Parity(chunks, chunkSize)
	if err != nil {
		t.Fatalf("Parity: %v", err)
	}

	sid := sid(0xF1)
	soh := framing.SOH{
		SessionID:     sid,
		ChunkTotal:    5,
		ChunkSize:     chunkSize,
		TotalBytes:    uint64(len(full)),
		ContentSHA256: sha256.Sum256(full),
		Mode:          0o644,
		Name:          "fec.bin",
		FECGroupSize:  groupSize,
	}
	if err := m.IngestSOH(soh); err != nil {
		t.Fatalf("IngestSOH: %v", err)
	}

	// Deliver chunks 0, 1, 3, 4 (skipping chunk 2).
	for _, idx := range []uint32{0, 1, 3, 4} {
		d := framing.DATA{SessionID: sid, ChunkIndex: idx}
		if err := m.IngestDATA(d, chunks[idx]); err != nil {
			t.Fatalf("IngestDATA %d: %v", idx, err)
		}
	}
	// Deliver the parity chunk at index chunk_total (=5).
	dParity := framing.DATA{
		Flags:      framing.FlagParity,
		SessionID:  sid,
		ChunkIndex: 5, // chunk_total + 0
	}
	if err := m.IngestDATA(dParity, parity); err != nil {
		t.Fatalf("IngestDATA parity: %v", err)
	}

	// Session should now be complete; the file delivered.
	if m.Stats().Completed != 1 {
		t.Fatalf("expected 1 completion; stats=%+v", m.Stats())
	}
	got, err := readDelivered(t, out, "fec.bin")
	if err != nil {
		t.Fatalf("read delivered: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("delivered bytes mismatch:\n got  %x\n want %x", got, full)
	}
}

// TestFEC_NoLoss_NoReconstructionNeeded: with all data chunks
// delivered before the parity, the parity arrival is a no-op and
// the session still completes.
func TestFEC_NoLoss_ParityIsNoOp(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	out := filepath.Join(t.TempDir(), "out")
	m, _ := New(Options{SpoolDir: spool, FilesTo: out, SpoolMode: SpoolModeSparse})

	const chunkSize = 16
	chunks := [][]byte{
		bytes.Repeat([]byte{0xAA}, chunkSize),
		bytes.Repeat([]byte{0xBB}, chunkSize),
	}
	full := bytes.Join(chunks, nil)
	parity, _ := fec.Parity(chunks, chunkSize)
	sid := sid(0xF2)
	soh := framing.SOH{
		SessionID: sid, ChunkTotal: 2, ChunkSize: chunkSize,
		TotalBytes: uint64(len(full)), ContentSHA256: sha256.Sum256(full),
		Mode: 0o644, Name: "noloss.bin", FECGroupSize: 2,
	}
	_ = m.IngestSOH(soh)
	for i := uint32(0); i < 2; i++ {
		_ = m.IngestDATA(framing.DATA{SessionID: sid, ChunkIndex: i}, chunks[i])
	}
	// Parity AFTER completion: must be a benign drop, not a re-deliver.
	parityFrame := framing.DATA{
		Flags:      framing.FlagParity,
		SessionID:  sid,
		ChunkIndex: 2,
	}
	_ = m.IngestDATA(parityFrame, parity)

	if m.Stats().Completed != 1 {
		t.Fatalf("expected 1 completion")
	}
	got, _ := readDelivered(t, out, "noloss.bin")
	if !bytes.Equal(got, full) {
		t.Fatalf("delivered bytes mismatch")
	}
}

// TestFEC_TwoMissingNotRecoverable: when two chunks in one group are
// missing, XOR FEC cannot recover and the session stays incomplete.
func TestFEC_TwoMissingNotRecoverable(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	out := filepath.Join(t.TempDir(), "out")
	m, _ := New(Options{SpoolDir: spool, FilesTo: out, SpoolMode: SpoolModeSparse})

	const chunkSize = 16
	const groupSize uint32 = 4
	chunks := [][]byte{
		bytes.Repeat([]byte{0x01}, chunkSize),
		bytes.Repeat([]byte{0x02}, chunkSize),
		bytes.Repeat([]byte{0x04}, chunkSize),
		bytes.Repeat([]byte{0x08}, chunkSize),
	}
	full := bytes.Join(chunks, nil)
	parity, _ := fec.Parity(chunks, chunkSize)
	sid := sid(0xF3)
	soh := framing.SOH{
		SessionID: sid, ChunkTotal: 4, ChunkSize: chunkSize,
		TotalBytes: uint64(len(full)), ContentSHA256: sha256.Sum256(full),
		Mode: 0o644, Name: "twoMiss.bin", FECGroupSize: groupSize,
	}
	_ = m.IngestSOH(soh)
	// Drop chunks 1 AND 2.
	_ = m.IngestDATA(framing.DATA{SessionID: sid, ChunkIndex: 0}, chunks[0])
	_ = m.IngestDATA(framing.DATA{SessionID: sid, ChunkIndex: 3}, chunks[3])
	_ = m.IngestDATA(framing.DATA{Flags: framing.FlagParity, SessionID: sid, ChunkIndex: 4}, parity)

	if m.Stats().Completed != 0 {
		t.Fatalf("two-missing should not complete; stats=%+v", m.Stats())
	}
}

// Tiny helper to read delivered file (avoids importing os in every test).
func readDelivered(t *testing.T, outDir, name string) ([]byte, error) {
	t.Helper()
	return readFileHelper(filepath.Join(outDir, name))
}
