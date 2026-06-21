package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunPSK_HexDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.hex")
	if err := runPSK(context.Background(), []string{"--file=" + path}); err != nil {
		t.Fatalf("runPSK: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	stripped := bytes.TrimSpace(b)
	if len(stripped) != 64 {
		t.Fatalf("expected 64 hex chars, got len=%d (%q)", len(stripped), stripped)
	}
	if _, err := hex.DecodeString(string(stripped)); err != nil {
		t.Fatalf("not valid hex: %v", err)
	}
}

func TestRunPSK_RawFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.bin")
	if err := runPSK(context.Background(), []string{"--file=" + path, "--format=raw"}); err != nil {
		t.Fatalf("runPSK: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b) != 32 {
		t.Fatalf("expected 32 raw bytes, got %d", len(b))
	}
}

func TestRunPSK_LongerKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.hex")
	if err := runPSK(context.Background(), []string{"--file=" + path, "--bytes=64"}); err != nil {
		t.Fatalf("runPSK: %v", err)
	}
	b, _ := os.ReadFile(path)
	if got := len(bytes.TrimSpace(b)); got != 128 {
		t.Fatalf("expected 128 hex chars (64 bytes), got %d", got)
	}
}

func TestRunPSK_RejectsBytesTooSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.hex")
	err := runPSK(context.Background(), []string{"--file=" + path, "--bytes=16"})
	if err == nil || !strings.Contains(err.Error(), ">= 32") {
		t.Fatalf("err: got %v, want >=32 error", err)
	}
}

func TestRunPSK_RejectsBadFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.hex")
	err := runPSK(context.Background(), []string{"--file=" + path, "--format=base64"})
	if err == nil || !strings.Contains(err.Error(), "hex") {
		t.Fatalf("err: got %v, want format error", err)
	}
}

func TestRunPSK_RequiresFile(t *testing.T) {
	err := runPSK(context.Background(), []string{})
	if err == nil || !strings.Contains(err.Error(), "--file") {
		t.Fatalf("err: got %v, want --file required", err)
	}
}

func TestRunPSK_RefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.hex")
	if err := os.WriteFile(path, []byte("previous content\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := runPSK(context.Background(), []string{"--file=" + path})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err: got %v, want refusing-to-overwrite", err)
	}
	// File contents unchanged.
	b, _ := os.ReadFile(path)
	if string(b) != "previous content\n" {
		t.Fatalf("existing file was clobbered")
	}
}

func TestRunPSK_ForceOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.hex")
	_ = os.WriteFile(path, []byte("old\n"), 0o600)
	if err := runPSK(context.Background(), []string{"--file=" + path, "--force"}); err != nil {
		t.Fatalf("runPSK --force: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) == "old\n" {
		t.Fatalf("--force did not overwrite")
	}
	if len(bytes.TrimSpace(b)) != 64 {
		t.Fatalf("overwrite result is wrong length: %q", b)
	}
}

func TestRunPSK_Mode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not represented on Windows")
	}
	path := filepath.Join(t.TempDir(), "psk.hex")
	if err := runPSK(context.Background(), []string{"--file=" + path}); err != nil {
		t.Fatalf("runPSK: %v", err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode: got %o, want 0600", st.Mode().Perm())
	}
}

// Round-trip: the file we just generated must be accepted by loadKeyFile.
func TestRunPSK_LoadKeyFile_AcceptsGeneratedHex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.hex")
	if err := runPSK(context.Background(), []string{"--file=" + path}); err != nil {
		t.Fatalf("runPSK: %v", err)
	}
	key, err := loadKeyFile(path)
	if err != nil {
		t.Fatalf("loadKeyFile: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("loaded key len: got %d, want 32", len(key))
	}
}

func TestRunPSK_LoadKeyFile_AcceptsGeneratedRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.bin")
	if err := runPSK(context.Background(), []string{"--file=" + path, "--format=raw"}); err != nil {
		t.Fatalf("runPSK: %v", err)
	}
	key, err := loadKeyFile(path)
	if err != nil {
		t.Fatalf("loadKeyFile: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("loaded key len: got %d, want 32", len(key))
	}
}
