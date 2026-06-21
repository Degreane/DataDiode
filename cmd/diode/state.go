package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// defaultSenderStateDir returns the conventional per-user state dir:
//   - Linux/macOS: $HOME/.diode
//   - Windows:     %APPDATA%\diode  (or %USERPROFILE%\AppData\Roaming\diode)
func defaultSenderStateDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "diode")
		}
		if d := os.Getenv("USERPROFILE"); d != "" {
			return filepath.Join(d, "AppData", "Roaming", "diode")
		}
		return `C:\diode`
	}
	if d := os.Getenv("HOME"); d != "" {
		return filepath.Join(d, ".diode")
	}
	return "/var/lib/diode"
}

// archiveStamp formats t as a filesystem-safe UTC ISO timestamp
// suitable for snapshot filenames: "2026-06-21T20-15-23Z".
func archiveStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15-04-05Z")
}

// copyFile copies src to dst (truncating dst), preserving mode bits.
// Returns the number of bytes copied.
func copyFile(src, dst string, mode os.FileMode) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return n, err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return n, err
	}
	return n, out.Close()
}
