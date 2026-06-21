package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeKeyFile creates a 0600 key file containing the given hex string.
func writeKeyFile(t *testing.T, hexKey string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(hexKey), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return p
}

const keyA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" // 32 bytes
const keyB = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

// Both sides have the same key — file roundtrips.
func TestE2E_Signed_HappyPath(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	keyFile := writeKeyFile(t, keyA)

	rx := startRx(t, port, "", "--files-to="+outDir, "--key-file="+keyFile)

	src := filepath.Join(t.TempDir(), "secret.txt")
	payload := []byte("authenticated payload\n")
	if err := os.WriteFile(src, payload, 0o640); err != nil {
		t.Fatalf("write src: %v", err)
	}

	cmd := exec.Command(diodeBin,
		"--mode=tx",
		"--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src,
		"--key-file", keyFile,
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tx: %v", err)
	}

	got := readFileWhenStable(t, filepath.Join(outDir, "secret.txt"), len(payload), 3*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch")
	}
	_ = rx.stop()
}

// Sender keyed, receiver unkeyed → receiver drops every frame.
func TestE2E_Signed_TxKeyedRxNot(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	keyFile := writeKeyFile(t, keyA)

	rx := startRx(t, port, "", "--files-to="+outDir) // no --key-file

	src := filepath.Join(t.TempDir(), "wont-arrive.txt")
	_ = os.WriteFile(src, []byte("nope"), 0o600)

	cmd := exec.Command(diodeBin,
		"--mode=tx", "--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src,
		"--key-file", keyFile,
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tx: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(outDir, "wont-arrive.txt")); err == nil {
		t.Fatalf("file MUST NOT have been delivered to an unkeyed receiver")
	}
	_ = rx.stop()
}

// Sender unkeyed, receiver keyed → receiver drops every frame.
func TestE2E_Signed_RxKeyedTxNot(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	keyFile := writeKeyFile(t, keyA)

	rx := startRx(t, port, "", "--files-to="+outDir, "--key-file="+keyFile)

	src := filepath.Join(t.TempDir(), "wont-arrive.txt")
	_ = os.WriteFile(src, []byte("nope"), 0o600)

	cmd := exec.Command(diodeBin,
		"--mode=tx", "--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src,
		// no --key-file
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tx: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(outDir, "wont-arrive.txt")); err == nil {
		t.Fatalf("file MUST NOT have been delivered to a keyed receiver from unsigned sender")
	}
	_ = rx.stop()
}

// Different keys on each side → no delivery (HMAC mismatch).
func TestE2E_Signed_DifferentKeys(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	keyFileA := writeKeyFile(t, keyA)
	keyFileB := writeKeyFile(t, keyB)

	rx := startRx(t, port, "", "--files-to="+outDir, "--key-file="+keyFileB)

	src := filepath.Join(t.TempDir(), "wont-arrive.txt")
	_ = os.WriteFile(src, []byte("attacker"), 0o600)

	cmd := exec.Command(diodeBin,
		"--mode=tx", "--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src,
		"--key-file", keyFileA,
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tx: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(outDir, "wont-arrive.txt")); err == nil {
		t.Fatalf("file MUST NOT have been delivered with mismatched keys")
	}
	_ = rx.stop()
}

// Sanity: bad key-file path on tx makes the command fail without sending.
func TestE2E_Signed_TxBadKeyFile(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	rx := startRx(t, port, "", "--files-to="+outDir)
	defer rx.stop()

	cmd := exec.Command(diodeBin,
		"--mode=tx", "--dst", "127.0.0.1:"+itoa(port),
		"--key-file", "/nonexistent",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdin = bytes.NewReader([]byte("x"))
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected tx to fail with missing key file")
	}
	if !strings.Contains(stderr.String(), "key file") {
		t.Fatalf("stderr should mention key file; got: %s", stderr.String())
	}
}
