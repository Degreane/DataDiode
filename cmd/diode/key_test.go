package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKey(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(contents), mode); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadKey_Hex(t *testing.T) {
	hex64 := strings.Repeat("ab", 32) // 32 raw bytes → 64 hex chars
	p := writeKey(t, hex64+"\n", 0o600)
	got, err := loadKeyFile(p)
	if err != nil {
		t.Fatalf("loadKeyFile: %v", err)
	}
	if len(got) != 32 || got[0] != 0xab {
		t.Fatalf("decoded bytes wrong: len=%d first=%x", len(got), got[0])
	}
}

func TestLoadKey_Raw(t *testing.T) {
	raw := bytes.Repeat([]byte{0x55}, 48) // 48 raw bytes, not all-hex
	raw[0] = 0xFF                         // ensure non-hex char early
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadKeyFile(p)
	if err != nil {
		t.Fatalf("loadKeyFile: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("raw bytes mismatch")
	}
}

func TestLoadKey_TooShortHex(t *testing.T) {
	p := writeKey(t, strings.Repeat("ab", 16)+"\n", 0o600) // 16 bytes < MinKeyBytes
	if _, err := loadKeyFile(p); err == nil {
		t.Fatalf("expected error for short hex key")
	}
}

func TestLoadKey_TooShortRaw(t *testing.T) {
	p := writeKey(t, "GG"+strings.Repeat("x", 10), 0o600) // not-hex, only 12 bytes
	if _, err := loadKeyFile(p); err == nil {
		t.Fatalf("expected error for short raw key")
	}
}

func TestLoadKey_EmptyFile(t *testing.T) {
	p := writeKey(t, "", 0o600)
	if _, err := loadKeyFile(p); err == nil {
		t.Fatalf("expected error for empty key file")
	}
}

func TestLoadKey_Missing(t *testing.T) {
	if _, err := loadKeyFile("/nonexistent/path"); err == nil {
		t.Fatalf("expected error for missing key file")
	}
}

func TestLoadKey_PermissiveWarns(t *testing.T) {
	// Just ensure no error; the warning goes to stderr and is not asserted here.
	p := writeKey(t, strings.Repeat("c", 64), 0o644)
	if _, err := loadKeyFile(p); err != nil {
		t.Fatalf("loadKeyFile (warn path): %v", err)
	}
}
