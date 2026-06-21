package e2e

import (
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestE2E_SendFile_Roundtrip ships a real binary file (the diode binary
// itself, since TestMain just built it) from --send-file on the tx side
// to --files-to on the rx side, and asserts byte-identical reconstruction
// plus correct mode bits.
func TestE2E_SendFile_Roundtrip(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	rx := startRx(t, port, "", "--files-to="+outDir)

	// Stage a payload with a known mode. The diode binary itself is a
	// non-trivial multi-chunk binary file; perfect stress test.
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "payload.bin")
	original, err := os.ReadFile(diodeBin)
	if err != nil {
		t.Fatalf("read diode binary: %v", err)
	}
	if err := os.WriteFile(src, original, 0o640); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.Chmod(src, 0o640); err != nil {
		t.Fatalf("chmod src: %v", err)
	}

	cmd := exec.Command(diodeBin,
		"--mode=tx",
		"--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src,
		"--chunk=1400",
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tx: %v", err)
	}

	dst := filepath.Join(outDir, "payload.bin")
	got := readFileWhenStable(t, dst, len(original), 10*time.Second)
	if !bytes.Equal(got, original) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(original))
	}
	if sha256.Sum256(got) != sha256.Sum256(original) {
		t.Fatalf("sha256 mismatch")
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode: got %o, want 0640", st.Mode().Perm())
	}

	_ = rx.stop()
}

// TestE2E_SendFile_Redundancy ensures --redundancy doesn't duplicate
// the file (recently-delivered MsgID cache catches the dup at the
// transport layer, before envelope parsing).
func TestE2E_SendFile_Redundancy(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	rx := startRx(t, port, "", "--files-to="+outDir)

	payload := bytes.Repeat([]byte("z"), 50) // small → 1 chunk
	src := filepath.Join(t.TempDir(), "tiny.txt")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	cmd := exec.Command(diodeBin,
		"--mode=tx",
		"--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src,
		"--redundancy=3",
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tx: %v", err)
	}

	dst := filepath.Join(outDir, "tiny.txt")
	got := readFileWhenStable(t, dst, len(payload), 3*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch")
	}
	time.Sleep(200 * time.Millisecond) // any late dups would land by now
	final, _ := os.ReadFile(dst)
	if len(final) != len(payload) {
		t.Fatalf("file grew on dup: got %d bytes, want %d", len(final), len(payload))
	}
	_ = rx.stop()
}

func TestE2E_SendFile_LargeMultiChunk(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	rx := startRx(t, port, "", "--files-to="+outDir)

	// 200 KiB non-repeating pattern → ~150 chunks at default chunk size.
	payload := make([]byte, 200<<10)
	for i := range payload {
		payload[i] = byte((i * 1103515245) >> 8)
	}
	src := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	cmd := exec.Command(diodeBin, "--mode=tx", "--dst", "127.0.0.1:"+itoa(port), "--send-file", src)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tx: %v", err)
	}

	dst := filepath.Join(outDir, "big.bin")
	got := readFileWhenStable(t, dst, len(payload), 10*time.Second)
	if sha256.Sum256(got) != sha256.Sum256(payload) {
		t.Fatalf("sha256 mismatch (got %d bytes, want %d)", len(got), len(payload))
	}
	_ = rx.stop()
}

// itoa avoids importing strconv for the one call site.
func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}
