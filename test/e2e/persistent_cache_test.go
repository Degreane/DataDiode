package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// startRxWithSpool is like startRx but pins --spool and returns the
// spool dir so the caller can use the same one across a "restart".
func startRxWithSpool(t *testing.T, port int, outPath, spool string, extraArgs ...string) *rxProc {
	t.Helper()
	args := []string{"--mode=rx",
		"--listen=127.0.0.1:" + itoa(port),
		"--spool=" + spool,
	}
	if outPath != "" {
		args = append(args, "--out="+outPath)
	}
	args = append(args, extraArgs...)
	// startRx (in e2e_test.go) auto-adds --spool unless one is already
	// present; we passed --spool explicitly so it stays.
	return startRx(t, port, "", append([]string{"--spool=" + spool}, args[1:]...)...)
}

// TestE2E_PersistentCache_SurvivesRestart sends a file (it completes),
// stops the receiver, starts a fresh receiver on the same --spool dir,
// then sends the SAME session_id again. The hydrated completed-cache
// must catch the replay and prevent a second delivery to --files-to.
func TestE2E_PersistentCache_SurvivesRestart(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	spool := t.TempDir()
	state := t.TempDir()

	// ---- first receiver: complete one session ----
	rx1 := startRx(t, port, "",
		"--files-to="+outDir,
		"--spool="+spool,
		"--completed-cache-disk=true",
	)

	src := filepath.Join(t.TempDir(), "persist.txt")
	if err := os.WriteFile(src, []byte("once is enough"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src, "--sender-state", state)

	dst := filepath.Join(outDir, "persist.txt")
	_ = readFileWhenStable(t, dst, len("once is enough"), 3*time.Second)
	firstStat, _ := os.Stat(dst)

	// Stop the receiver. completed.idx persists in $spool.
	if err := rx1.stop(); err != nil {
		t.Fatalf("rx1 stop: %v", err)
	}

	// Confirm completed.idx exists and is non-empty.
	idx := filepath.Join(spool, "completed.idx")
	st, err := os.Stat(idx)
	if err != nil || st.Size() == 0 {
		t.Fatalf("completed.idx missing/empty: %v, size=%d", err, st.Size())
	}

	// ---- new receiver on the SAME spool ----
	port2 := freeUDPPort(t)
	rx2 := startRx(t, port2, "",
		"--files-to="+outDir,
		"--spool="+spool,
		"--completed-cache-disk=true",
	)
	defer rx2.stop()

	// Try to "replay": resend the same sid against rx2. We use
	// --resend (which pulls from the sender's manifest+snapshot, same
	// sid). The hydrated cache should reject.
	entries := manifestEntries(t, state)
	if len(entries) != 1 {
		t.Fatalf("manifest entries: got %d want 1", len(entries))
	}
	sid := entries[0]["session_id"].(string)

	cmd := exec.Command(diodeBin,
		"--mode=tx", "--dst", "127.0.0.1:"+itoa(port2),
		"--resend", sid, "--sender-state", state)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("resend tx: %v", err)
	}

	// Give the receiver time to NOT-deliver.
	time.Sleep(400 * time.Millisecond)

	// Output file mtime must NOT change — second delivery was blocked.
	secondStat, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat after replay: %v", err)
	}
	if !secondStat.ModTime().Equal(firstStat.ModTime()) {
		t.Fatalf("file was re-delivered (mtime changed %v → %v); persistent cache did NOT catch the restart-replay",
			firstStat.ModTime(), secondStat.ModTime())
	}
}

// TestE2E_PersistentCache_DiskOff_ReDelivers is the negative control:
// with --completed-cache-disk=false, the same restart scenario WILL
// re-deliver because the cache is RAM-only and dies with the rx.
func TestE2E_PersistentCache_DiskOff_ReDelivers(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	spool := t.TempDir()
	state := t.TempDir()

	rx1 := startRx(t, port, "",
		"--files-to="+outDir,
		"--spool="+spool,
		"--completed-cache-disk=false",
	)
	src := filepath.Join(t.TempDir(), "reborn.txt")
	_ = os.WriteFile(src, []byte("re-deliverable"), 0o644)
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src, "--sender-state", state)

	dst := filepath.Join(outDir, "reborn.txt")
	_ = readFileWhenStable(t, dst, len("re-deliverable"), 3*time.Second)
	firstMtime := func() time.Time {
		st, _ := os.Stat(dst)
		return st.ModTime()
	}()
	_ = rx1.stop()
	if _, err := os.Stat(filepath.Join(spool, "completed.idx")); err == nil {
		t.Fatalf("completed.idx should NOT exist when --completed-cache-disk=false")
	}

	port2 := freeUDPPort(t)
	rx2 := startRx(t, port2, "",
		"--files-to="+outDir,
		"--spool="+spool,
		"--completed-cache-disk=false",
	)
	defer rx2.stop()

	entries := manifestEntries(t, state)
	sid := entries[0]["session_id"].(string)

	// Resend → should re-deliver because no cache survives restart.
	time.Sleep(50 * time.Millisecond)
	cmd := exec.Command(diodeBin,
		"--mode=tx", "--dst", "127.0.0.1:"+itoa(port2),
		"--resend", sid, "--sender-state", state)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("resend tx: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	secondMtime := func() time.Time {
		st, _ := os.Stat(dst)
		return st.ModTime()
	}()
	if secondMtime.Equal(firstMtime) {
		t.Fatalf("file was NOT re-delivered with --completed-cache-disk=false; mtime unchanged at %v", firstMtime)
	}
}

// TestE2E_PersistentCache_VacuumPrunes verifies that --mode=vacuum
// drops entries from completed.idx older than --age.
func TestE2E_PersistentCache_VacuumPrunes(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	spool := t.TempDir()
	state := t.TempDir()

	rx := startRx(t, port, "",
		"--files-to="+outDir,
		"--spool="+spool,
		"--completed-cache-disk=true",
	)
	src := filepath.Join(t.TempDir(), "prune.txt")
	_ = os.WriteFile(src, []byte("prune-me"), 0o644)
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src, "--sender-state", state)
	_ = readFileWhenStable(t, filepath.Join(outDir, "prune.txt"), len("prune-me"), 3*time.Second)
	_ = rx.stop()

	idx := filepath.Join(spool, "completed.idx")
	before, _ := os.ReadFile(idx)
	if len(before) == 0 {
		t.Fatalf("completed.idx should have content")
	}

	// Vacuum with --age=1 (one minute) shouldn't drop our just-written
	// entry (it's seconds old).
	runVacuumCmd(t, "--age=1", "--spool="+spool)
	mid, _ := os.ReadFile(idx)
	if string(mid) != string(before) {
		t.Fatalf("--age=1 should not drop a fresh entry")
	}

	// Backdate the file's mtime AND rewrite content with a backdated
	// timestamp so the prune actually applies.
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	// Replace the timestamp in the existing line.
	parts := []byte(string(before))
	// Hand-write: same sid, backdated ts.
	for i, b := range parts {
		if b == '\t' {
			parts = append(parts[:i+1], append([]byte(old), '\n')...)
			break
		}
	}
	if err := os.WriteFile(idx, parts, 0o644); err != nil {
		t.Fatalf("rewrite idx: %v", err)
	}

	runVacuumCmd(t, "--age=60", "--spool="+spool)
	after, _ := os.ReadFile(idx)
	if len(after) != 0 {
		t.Fatalf("after vacuum: completed.idx should be empty, got %d bytes: %q", len(after), after)
	}
}

// Marker so the unused declaration above doesn't get optimized out.
var _ = startRxWithSpool
