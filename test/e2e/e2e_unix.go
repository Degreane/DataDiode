//go:build !windows

package e2e

import (
	"errors"
	"os/exec"
	"syscall"
)

func sendInterrupt(cmd *exec.Cmd) error {
	return cmd.Process.Signal(syscall.SIGINT)
}

func isSignalKilled(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
		return ws.Signaled()
	}
	return false
}
