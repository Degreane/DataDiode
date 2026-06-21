//go:build windows

package e2e

import (
	"errors"
	"os/exec"
)

// sendInterrupt is never called on Windows (the test branches earlier
// to Process.Kill), but the symbol must exist so the package compiles.
func sendInterrupt(*exec.Cmd) error { return errors.New("not supported on windows") }

// isSignalKilled has no Windows analogue (no POSIX signal exit codes).
// Tests use Process.Kill on Windows, so this is unreachable.
func isSignalKilled(error) bool { return false }
