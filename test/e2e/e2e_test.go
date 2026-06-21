// Package e2e holds black-box end-to-end tests that spawn the real
// `diode` binary as subprocesses on loopback. These are slower than
// unit tests but exercise the actual UDP stack, signal handling, and
// CLI surface — catching any drift between the in-process Sender/
// Receiver path and what an operator actually invokes.
package e2e

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// diodeBin is the path to the `diode` binary built in TestMain. Shared
// across all tests in the package so we pay the build cost once.
var diodeBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "diode-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: mkdir temp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	diodeBin = filepath.Join(tmp, "diode")
	build := exec.Command("go", "build", "-o", diodeBin, "../../cmd/diode")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: build diode:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// freeUDPPort asks the kernel for a free UDP port, closes the socket,
// and returns the port. There's a tiny race window before the test
// rebinds it, but on a single dev/CI host with no other listeners on
// 127.0.0.1 the window is acceptable. If this ever flakes we'll switch
// to passing the listener fd to the child.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("alloc port: %v", err)
	}
	port := l.LocalAddr().(*net.UDPAddr).Port
	_ = l.Close()
	return port
}

// rxProc is a started receiver subprocess. Call stop() to send SIGINT
// and wait for shutdown; out is the path the receiver was told to
// write to.
type rxProc struct {
	cmd  *exec.Cmd
	out  string
	stop func() error
}

// containsFlag reports whether args includes a --name=... or --name argument.
func containsFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// itoa is a tiny helper kept here for tests that build URLs / args.
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

// startRx launches `diode --mode=rx` on the given port, writing to
// outPath. It blocks until the child prints its "listening on" banner
// so the caller is guaranteed the socket is bound before sending.
//
// If outPath is empty, no --out flag is passed; this is how callers
// using --files-to (passed via extraArgs) opt out of the raw-stream sink.
func startRx(t *testing.T, port int, outPath string, extraArgs ...string) *rxProc {
	t.Helper()
	args := []string{"--mode=rx", fmt.Sprintf("--listen=127.0.0.1:%d", port)}
	if outPath != "" {
		args = append(args, "--out="+outPath)
	}
	// Always supply a writable spool dir per test to avoid permission
	// issues with the default /var/spool/diode. Tests may override by
	// passing --spool= in extraArgs themselves.
	if !containsFlag(extraArgs, "--spool") {
		args = append(args, "--spool="+t.TempDir())
	}
	args = append(args, extraArgs...)
	cmd := exec.Command(diodeBin, args...)
	cmd.Stdout = os.Stderr // chatter to stderr in tests; doesn't affect assertions

	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rx: %v", err)
	}

	// Drain stderr asynchronously so the child can't block on a full pipe.
	bannerSeen := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stderr)
		seen := false
		for sc.Scan() {
			line := sc.Text()
			if !seen && strings.Contains(line, "listening on") {
				seen = true
				close(bannerSeen)
			}
			fmt.Fprintln(os.Stderr, "rx>", line)
		}
	}()

	select {
	case <-bannerSeen:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("rx did not announce listening within 5s")
	}

	r := &rxProc{cmd: cmd, out: outPath}
	r.stop = func() error {
		// On Windows there is no portable equivalent of SIGINT for a
		// non-console child; Process.Kill is the supported shutdown
		// path. The stats line printed on clean shutdown is for
		// operators; tests don't depend on it.
		if runtime.GOOS == "windows" {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return nil
		}
		if err := sendInterrupt(cmd); err != nil {
			return fmt.Errorf("interrupt: %w", err)
		}
		// Wait up to 5s for clean shutdown, then SIGKILL.
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil && !isSignalKilled(err) {
				return err
			}
			return nil
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			return errors.New("rx did not exit within 5s of SIGINT")
		}
	}
	t.Cleanup(func() { _ = r.stop() })
	return r
}

// runTxOnce sends payload through a fresh `diode --mode=tx` invocation
// via --in=- (stdin) and blocks until it exits. For file-with-name flows
// use --send-file via extraArgs and pass nil payload.
func runTxOnce(t *testing.T, port int, payload []byte, extraArgs ...string) {
	t.Helper()
	args := []string{"--mode=tx", fmt.Sprintf("--dst=127.0.0.1:%d", port)}
	// If caller did not pass --send-file, default to --in=- so the
	// payload is piped through stdin (v2 requires an explicit source).
	if !containsFlag(extraArgs, "--send-file") && !containsFlag(extraArgs, "--in") {
		args = append(args, "--in=-")
	}
	args = append(args, extraArgs...)
	cmd := exec.Command(diodeBin, args...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tx (%v): %v", args, err)
	}
}

// readFileWhenStable reads path repeatedly until its size has been
// stable for `stable` consecutive checks, or `timeout` elapses. Used
// instead of a fixed sleep so tests don't race the receiver's writes.
func readFileWhenStable(t *testing.T, path string, expectedSize int, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(b) >= expectedSize {
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("file %s did not reach %d bytes within %v; got %d bytes: %q",
		path, expectedSize, timeout, len(b), b)
	return nil
}

// sendInterrupt and isSignalKilled are defined per-OS in
// e2e_unix.go / e2e_windows.go because syscall.WaitStatus has a
// different shape on Windows (uint32) than on Unix (struct).

// ---------- tests ---------------------------------------------------------

func TestE2E_SmallMessage(t *testing.T) {
	port := freeUDPPort(t)
	out := filepath.Join(t.TempDir(), "out.bin")
	rx := startRx(t, port, out)

	payload := []byte("hello, diode\n")
	runTxOnce(t, port, payload)

	got := readFileWhenStable(t, out, len(payload), 3*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch:\n got %q\n want %q", got, payload)
	}
	if err := rx.stop(); err != nil {
		t.Fatalf("rx stop: %v", err)
	}
}

func TestE2E_LargeMessage_ManyChunks(t *testing.T) {
	port := freeUDPPort(t)
	out := filepath.Join(t.TempDir(), "out.bin")
	rx := startRx(t, port, out)

	// 50 KiB payload with a non-repeating pattern so partial matches are loud.
	payload := make([]byte, 50<<10)
	for i := range payload {
		payload[i] = byte(i ^ (i >> 8))
	}
	// chunk=1400 → ~37 chunks → exercises multi-frame reassembly.
	runTxOnce(t, port, payload, "--chunk-size=1400")

	got := readFileWhenStable(t, out, len(payload), 5*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch (len got=%d want=%d)", len(got), len(payload))
	}
	_ = rx.stop()
}

func TestE2E_RedundancyDeliversOnce(t *testing.T) {
	port := freeUDPPort(t)
	out := filepath.Join(t.TempDir(), "out.bin")
	rx := startRx(t, port, out)

	payload := []byte("dedupe me please")
	runTxOnce(t, port, payload, "--redundancy=3")

	got := readFileWhenStable(t, out, len(payload), 3*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload: got %q, want %q", got, payload)
	}
	// Give the loopback any in-flight redundant copies a moment to arrive.
	time.Sleep(200 * time.Millisecond)
	final, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(final) != len(payload) {
		t.Fatalf("expected exactly one delivery (%d bytes); got %d bytes: %q",
			len(payload), len(final), final)
	}
	_ = rx.stop()
}

func TestE2E_MultipleMessagesAppend(t *testing.T) {
	port := freeUDPPort(t)
	out := filepath.Join(t.TempDir(), "out.bin")
	// v2: each session writes its payload to --out via append. No
	// per-message delimiter (the protocol moved to discrete sessions;
	// callers can include a separator in their payload if needed).
	rx := startRx(t, port, out)

	msgs := [][]byte{
		[]byte("one|"),
		[]byte("two|"),
		bytes.Repeat([]byte("three"), 500), // multi-chunk in the middle
		[]byte("|four|"),
	}
	for _, m := range msgs {
		runTxOnce(t, port, m, "--chunk-size=200")
	}

	var want bytes.Buffer
	for _, m := range msgs {
		want.Write(m)
	}
	got := readFileWhenStable(t, out, want.Len(), 5*time.Second)
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("output mismatch:\n got len=%d\n want len=%d", len(got), want.Len())
	}
	_ = rx.stop()
}

func TestE2E_EmptyInputDoesNotDeliver(t *testing.T) {
	port := freeUDPPort(t)
	out := filepath.Join(t.TempDir(), "out.bin")
	rx := startRx(t, port, out)

	runTxOnce(t, port, nil) // tx sends only a HEARTBEAT frame
	time.Sleep(200 * time.Millisecond)

	b, err := os.ReadFile(out)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("expected no bytes delivered for empty input; got %q", b)
	}
	_ = rx.stop()
}

// Sanity: the daemon must reject --mode without a value.
func TestE2E_BadFlags(t *testing.T) {
	cmd := exec.Command(diodeBin, "--mode=nope")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected non-zero exit for --mode=nope")
	}
	if !strings.Contains(stderr.String(), "tx, rx, manifest, vacuum, or psk") {
		t.Fatalf("stderr should mention valid modes; got: %s", stderr.String())
	}
}

// Defensive: ensure the binary was actually built (helpful diagnostic
// if TestMain ran but produced nothing).
func TestE2E_BinaryExists(t *testing.T) {
	if _, err := os.Stat(diodeBin); err != nil {
		t.Fatalf("diode binary missing at %s: %v", diodeBin, err)
	}
}
