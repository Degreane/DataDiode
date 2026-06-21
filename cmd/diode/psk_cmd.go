package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
)

// runPSK is the entry point for `diode --mode=psk`. Writes a fresh
// 32-byte CSRNG key to the file at --file in either hex (default) or
// raw binary format. Refuses to overwrite an existing file unless
// --force is given. Prints the file's sha256 to stdout so the operator
// can verify byte-equality on both sides after distribution.
func runPSK(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("diode --mode=psk", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: diode --mode=psk --file=<path> [flags]

Generates a fresh 32-byte pre-shared key from the OS CSRNG and writes
it to --file. The file is created with mode 0600. By default the
content is hex-encoded (suitable for paste, diff, and shell tooling);
--format=raw writes the 32 raw bytes instead.

After writing, the sha256 of the file's contents is printed to stdout
so you can verify byte-equality on both the sender and receiver host
after distributing the file out-of-band.

flags:`)
		fs.PrintDefaults()
	}

	path := fs.String("file", "", "destination path (required)")
	format := fs.String("format", "hex", "\"hex\" (64 hex chars + newline) or \"raw\" (32 binary bytes)")
	bytesN := fs.Int("bytes", 32, "key length in bytes (>= 32)")
	force := fs.Bool("force", false, "overwrite --file if it already exists (default: refuse)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		fs.Usage()
		return errors.New("--file is required")
	}
	if *bytesN < 32 {
		return fmt.Errorf("--bytes must be >= 32 (got %d)", *bytesN)
	}
	if *format != "hex" && *format != "raw" {
		return fmt.Errorf("--format must be \"hex\" or \"raw\" (got %q)", *format)
	}

	keyBytes := make([]byte, *bytesN)
	if _, err := rand.Read(keyBytes); err != nil {
		return fmt.Errorf("CSRNG read: %w", err)
	}

	var fileBytes []byte
	if *format == "hex" {
		fileBytes = []byte(hex.EncodeToString(keyBytes) + "\n")
	} else {
		fileBytes = keyBytes
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !*force {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(*path, flags, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("refusing to overwrite existing %s (use --force to allow)", *path)
		}
		return fmt.Errorf("create %s: %w", *path, err)
	}
	if _, err := f.Write(fileBytes); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", *path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync %s: %w", *path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", *path, err)
	}
	// Belt-and-braces: re-chmod to 0600 in case umask flipped a bit
	// during O_CREATE.
	if err := os.Chmod(*path, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "diode psk: warning: chmod 0600 %s: %v\n", *path, err)
	}

	sum := sha256.Sum256(fileBytes)
	fmt.Fprintf(os.Stderr, "diode psk: wrote %d-byte %s key to %s (mode 0600)\n",
		*bytesN, *format, *path)
	fmt.Fprintf(os.Stderr, "diode psk: file sha256 = %s\n", hex.EncodeToString(sum[:]))
	fmt.Fprintln(os.Stderr, "diode psk: distribute this file out-of-band and verify the sha256 matches on the receiving host.")
	return nil
}
