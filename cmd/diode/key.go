package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// MinKeyBytes is the minimum acceptable raw key length (256 bits).
// HMAC-SHA256 has no formal minimum key length, but anything shorter
// than the output size is gratuitously weak.
const MinKeyBytes = 32

// loadKeyFile reads path, strips whitespace, autodetects hex-vs-raw,
// and returns the raw key bytes. Returns ErrEmptyKey if path is "".
//
// A nag-but-don't-fail warning is printed to stderr if the file is
// group- or world-readable; this matches SSH/PGP convention so
// automation that briefly stages keys with default permissions still
// works.
func loadKeyFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("loadKeyFile: empty path")
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat key file: %w", err)
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "diode: warning: key file %s has permissive mode %o (recommend 0600)\n", path, mode)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	stripped := bytes.TrimSpace(raw)
	if len(stripped) == 0 {
		return nil, fmt.Errorf("key file %s is empty", path)
	}

	// Autodetect: if the stripped contents are all hex chars AND even
	// length, treat as hex; otherwise treat as raw bytes.
	if isAllHex(stripped) && len(stripped)%2 == 0 {
		decoded := make([]byte, hex.DecodedLen(len(stripped)))
		n, err := hex.Decode(decoded, stripped)
		if err != nil {
			return nil, fmt.Errorf("hex decode key: %w", err)
		}
		if n < MinKeyBytes {
			return nil, fmt.Errorf("hex-decoded key is %d bytes; need at least %d", n, MinKeyBytes)
		}
		return decoded[:n], nil
	}

	// Raw bytes path (also handles binary key files).
	if len(raw) < MinKeyBytes {
		return nil, fmt.Errorf("raw key is %d bytes; need at least %d", len(raw), MinKeyBytes)
	}
	// For raw mode we keep the file content verbatim (don't strip
	// whitespace — binary keys may legitimately include whitespace
	// bytes). We just verified above that the file wasn't a hex string.
	return raw, nil
}

func isAllHex(b []byte) bool {
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
