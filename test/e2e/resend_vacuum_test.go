package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ----- helpers --------------------------------------------------------------

// txOnceFull runs `diode --mode=tx` with the given flags and returns
// when the subprocess exits. Use for normal sends; for interruptible
// sends see txAsync.
func txOnceFull(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command(diodeBin, append([]string{"--mode=tx"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tx %v: %v", args, err)
	}
}

// manifestEntries parses every line of <state>/manifest.jsonl into a
// list of map[string]any. Empty file → empty slice.
func manifestEntries(t *testing.T, state string) []map[string]any {
	t.Helper()
	path := filepath.Join(state, "manifest.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read manifest: %v", err)
	}
	var out []map[string]any
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad manifest line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// writeFile is a one-liner shortcut.
func writeFile(t *testing.T, path string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, b, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// sha256Hex of file content.
func sha256Hex(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ----- resend ---------------------------------------------------------------

// Happy path: send a file, then --resend=<sid>. With --completed-cache=0
// on the receiver the second send re-delivers; manifest gains a second
// entry with the same sid.
func TestE2E_Resend_ByManifestEntry_ReDelivers(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	state := t.TempDir()
	// Disable completed-cache so resend can re-deliver for this test.
	rx := startRx(t, port, "", "--files-to="+outDir, "--completed-cache=0")

	src := filepath.Join(t.TempDir(), "resend.txt")
	payload := []byte("hello resend\n")
	writeFile(t, src, payload, 0o644)

	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--send-file", src, "--sender-state", state)

	dst := filepath.Join(outDir, "resend.txt")
	_ = readFileWhenStable(t, dst, len(payload), 3*time.Second)

	entries := manifestEntries(t, state)
	if len(entries) != 1 {
		t.Fatalf("manifest: got %d entries, want 1", len(entries))
	}
	sid := entries[0]["session_id"].(string)

	// Resend by sid.
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--resend", sid, "--sender-state", state)

	// Manifest now has 2 entries with the same sid.
	entries = manifestEntries(t, state)
	if len(entries) != 2 {
		t.Fatalf("after resend: %d entries, want 2", len(entries))
	}
	if entries[1]["session_id"].(string) != sid {
		t.Fatalf("resend used a different sid: %s vs %s", entries[1]["session_id"], sid)
	}
	// File content unchanged (delivered twice, same bytes).
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, payload) {
		t.Fatalf("delivered content differs from original")
	}
	_ = rx.stop()
}

// --resend uses the archived snapshot, NOT the live --send-file path.
// Modifying the original file post-send must not affect what resend ships.
func TestE2E_Resend_PullsFromSnapshot(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	state := t.TempDir()
	rx := startRx(t, port, "", "--files-to="+outDir, "--completed-cache=0")

	src := filepath.Join(t.TempDir(), "snap.txt")
	original := []byte("ORIGINAL CONTENT — must be what resend ships\n")
	writeFile(t, src, original, 0o644)
	wantSHA := sha256Hex(t, src)

	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--send-file", src, "--sender-state", state)
	entries := manifestEntries(t, state)
	sid := entries[0]["session_id"].(string)

	// Tamper the live source file: replace with completely different content.
	writeFile(t, src, []byte("REPLACED — must NOT appear on the receiver\n"), 0o644)

	// Resend — should pull from <state>/sent/<UTC>__snap.txt, not src.
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--resend", sid, "--sender-state", state)

	dst := filepath.Join(outDir, "snap.txt")
	// Allow the second delivery time to overwrite.
	time.Sleep(300 * time.Millisecond)
	gotSHA := sha256Hex(t, dst)
	if gotSHA != wantSHA {
		t.Fatalf("resend used live file, not snapshot:\n delivered sha = %s\n original sha  = %s",
			gotSHA, wantSHA)
	}
	_ = rx.stop()
}

// --resend refuses if the snapshot's sha256 differs from the manifest's
// recorded content_sha256 (someone tampered the archive directly).
func TestE2E_Resend_RefusesOnSnapshotMismatch(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	state := t.TempDir()
	rx := startRx(t, port, "", "--files-to="+outDir, "--completed-cache=0")
	defer rx.stop()

	src := filepath.Join(t.TempDir(), "tamper.txt")
	writeFile(t, src, []byte("the truth\n"), 0o644)

	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--send-file", src, "--sender-state", state)
	entries := manifestEntries(t, state)
	sid := entries[0]["session_id"].(string)
	snapshot := entries[0]["snapshot"].(string)

	// Corrupt the snapshot in place.
	writeFile(t, snapshot, []byte("fabrication\n"), 0o644)

	// --resend must fail (and refuse to even open a UDP socket).
	cmd := exec.Command(diodeBin,
		"--mode=tx", "--dst", "127.0.0.1:"+itoa(port),
		"--resend", sid, "--sender-state", state)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected tx to refuse; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "sha256 differs") {
		t.Fatalf("stderr should mention sha256 mismatch; got: %s", stderr.String())
	}
}

// --resend-latest finds the most recent manifest entry by basename.
func TestE2E_ResendLatest_ByFilename(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	state := t.TempDir()
	rx := startRx(t, port, "", "--files-to="+outDir, "--completed-cache=0")
	defer rx.stop()

	src := filepath.Join(t.TempDir(), "latest.bin")
	writeFile(t, src, []byte("v1"), 0o644)
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--send-file", src, "--sender-state", state)
	time.Sleep(50 * time.Millisecond) // ensure distinct completed_at
	writeFile(t, src, []byte("v2"), 0o644)
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--send-file", src, "--sender-state", state)

	entries := manifestEntries(t, state)
	if len(entries) != 2 {
		t.Fatalf("setup: want 2 entries, got %d", len(entries))
	}
	latestSID := entries[1]["session_id"].(string)

	// Resend-latest should pick the second send's sid (the "v2" snapshot).
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--resend-latest", "latest.bin", "--sender-state", state)
	entries = manifestEntries(t, state)
	if len(entries) != 3 {
		t.Fatalf("after resend-latest: want 3 entries, got %d", len(entries))
	}
	if entries[2]["session_id"].(string) != latestSID {
		t.Fatalf("resend-latest used wrong sid: %s want %s", entries[2]["session_id"], latestSID)
	}
}

// Receiver's completed-cache catches resends of already-completed sessions
// (default --completed-cache=1024). End-to-end: send, resend, the second
// SOH is dropped via the cache.
func TestE2E_Resend_BlockedByCompletedCache(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	state := t.TempDir()
	// Default cache (1024) is on by default; no need to set it.
	rx := startRx(t, port, "", "--files-to="+outDir)

	src := filepath.Join(t.TempDir(), "cached.txt")
	writeFile(t, src, []byte("cached payload"), 0o644)
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--send-file", src, "--sender-state", state)
	entries := manifestEntries(t, state)
	sid := entries[0]["session_id"].(string)

	// Capture file size after first delivery (also serves as the
	// "expected unchanged" reference for after the resend).
	dst := filepath.Join(outDir, "cached.txt")
	initial := readFileWhenStable(t, dst, len("cached payload"), 3*time.Second)
	initialMTime := func() time.Time {
		st, _ := os.Stat(dst)
		return st.ModTime()
	}()
	time.Sleep(50 * time.Millisecond) // so any new write would have a later mtime

	// Resend the same sid → cache hit; no second delivery to the file.
	txOnceFull(t, "--dst", "127.0.0.1:"+itoa(port), "--resend", sid, "--sender-state", state)

	// Give the receiver time to NOT-deliver (anti-test: prove nothing happens).
	time.Sleep(400 * time.Millisecond)

	after, _ := os.ReadFile(dst)
	afterMTime := func() time.Time {
		st, _ := os.Stat(dst)
		return st.ModTime()
	}()
	if !bytes.Equal(initial, after) {
		t.Fatalf("file content changed after cached-resend")
	}
	if !afterMTime.Equal(initialMTime) {
		t.Fatalf("file mtime changed (%v → %v); resend was NOT blocked by cache",
			initialMTime, afterMTime)
	}
	_ = rx.stop()
}

// Interrupted scenario: tx is killed mid-flight, leaving a partial
// receiver spool. A second tx using --session-id=<same> at full speed
// fills the holes via bitmap dedup, completing the file.
func TestE2E_Resend_FillsMissingChunksViaSessionID(t *testing.T) {
	port := freeUDPPort(t)
	outDir := t.TempDir()
	spool := t.TempDir() // explicit so we can read the sid from the dir name
	state1 := t.TempDir()
	state2 := t.TempDir()
	rx := startRx(t, port, "", "--files-to="+outDir, "--spool="+spool, "--completed-cache=0")

	// 30 KiB → ~22 chunks at default chunk_size; with --rate=2000 (2 KB/s) the
	// whole send would take ~15s. Killing after 1s leaves a partial spool.
	payload := make([]byte, 30<<10)
	for i := range payload {
		payload[i] = byte((i * 1009) >> 1)
	}
	src := filepath.Join(t.TempDir(), "interrupt.bin")
	writeFile(t, src, payload, 0o644)
	wantSHA := sha256Hex(t, src)

	cmd := exec.Command(diodeBin,
		"--mode=tx",
		"--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src,
		"--sender-state", state1,
		"--rate=2000",
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tx: %v", err)
	}
	// Give it ~1s to ship part of the file then kill it.
	time.Sleep(1 * time.Second)
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	// Read the sid from the spool dir (only one in-flight session).
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatalf("readdir spool: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 spool session, got %d", len(entries))
	}
	sid := entries[0].Name()

	// Confirm the file is NOT yet delivered.
	if _, err := os.Stat(filepath.Join(outDir, "interrupt.bin")); err == nil {
		t.Fatalf("file should not be delivered yet; received too fast")
	}

	// Resume at full speed using --session-id (the manifest doesn't
	// have an entry for the killed run, so --resend wouldn't find it).
	txOnceFull(t,
		"--dst", "127.0.0.1:"+itoa(port),
		"--send-file", src,
		"--session-id", sid,
		"--sender-state", state2,
	)

	dst := filepath.Join(outDir, "interrupt.bin")
	got := readFileWhenStable(t, dst, len(payload), 5*time.Second)
	gotSHA := fmt.Sprintf("%x", sha256.Sum256(got))
	if gotSHA != wantSHA {
		t.Fatalf("after resume: sha mismatch (got %s, want %s)", gotSHA, wantSHA)
	}
	_ = rx.stop()
}

// ----- vacuum ---------------------------------------------------------------

// runVacuumCmd runs `diode --mode=vacuum` synchronously and returns
// its stderr (so tests can grep for action lines).
func runVacuumCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(diodeBin, append([]string{"--mode=vacuum"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("vacuum %v: %v\nstderr: %s", args, err, stderr.String())
	}
	return stderr.String()
}

// stageOldArchive writes a sender-state dir with an archived file + matching
// manifest entry backdated to ageMin minutes ago.
func stageOldArchive(t *testing.T, state string, name string, content []byte, ageMin int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(state, "sent"), 0o755); err != nil {
		t.Fatalf("mkdir sent: %v", err)
	}
	snapshot := filepath.Join(state, "sent", "stale__"+name)
	writeFile(t, snapshot, content, 0o644)
	when := time.Now().UTC().Add(-time.Duration(ageMin) * time.Minute)
	if err := os.Chtimes(snapshot, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// Manifest line.
	rec := map[string]any{
		"started_at":     when.Add(-time.Second).Format(time.RFC3339Nano),
		"completed_at":   when.Format(time.RFC3339Nano),
		"session_id":     "stale-sid-" + name,
		"filename":       name,
		"total_bytes":    len(content),
		"chunk_size":     1400,
		"chunk_total":    1,
		"content_sha256": fmt.Sprintf("%x", sha256.Sum256(content)),
		"destination":    "10.0.0.0:1",
		"mode":           "0o644",
		"redundancy":     1,
		"soh_redundancy": 3,
		"signed":         false,
		"snapshot":       snapshot,
		"tx_status":      "sent",
	}
	b, _ := json.Marshal(rec)
	f, _ := os.OpenFile(filepath.Join(state, "manifest.jsonl"),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	defer f.Close()
	f.Write(b)
	f.Write([]byte("\n"))
}

// stageOldSpoolSession writes a fake spool dir with backdated mtime
// on both the contained files AND the directory itself (otherwise the
// dir's own MkdirAll mtime would be "now" and vacuum's newest-mtime
// rule would treat the session as fresh).
func stageOldSpoolSession(t *testing.T, spool, sid string, ageMin int) {
	t.Helper()
	dir := filepath.Join(spool, sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir spool: %v", err)
	}
	writeFile(t, filepath.Join(dir, "data.partial"), []byte("partial"), 0o644)
	writeFile(t, filepath.Join(dir, "chunks.bitmap"), []byte{0x01}, 0o644)
	writeFile(t, filepath.Join(dir, "meta.json"), []byte("{}"), 0o644)
	when := time.Now().UTC().Add(-time.Duration(ageMin) * time.Minute)
	for _, f := range []string{"data.partial", "chunks.bitmap", "meta.json"} {
		_ = os.Chtimes(filepath.Join(dir, f), when, when)
	}
	// chtimes the dir LAST so the file writes (which bump the parent
	// dir's mtime) don't undo it.
	_ = os.Chtimes(dir, when, when)
}

func TestE2E_Vacuum_DryRun_ChangesNothing(t *testing.T) {
	state := t.TempDir()
	spool := t.TempDir()
	stageOldArchive(t, state, "old.txt", []byte("old"), 9999)
	stageOldSpoolSession(t, spool, "abandoned-old", 9999)

	stderr := runVacuumCmd(t, "--age=60", "--sender-state="+state, "--spool="+spool, "--dry-run", "--verbose")
	if !strings.Contains(stderr, "dry-run=true") {
		t.Fatalf("stderr should announce dry-run; got %s", stderr)
	}
	// Files still there.
	if _, err := os.Stat(filepath.Join(state, "sent", "stale__old.txt")); err != nil {
		t.Fatalf("dry-run removed archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spool, "abandoned-old")); err != nil {
		t.Fatalf("dry-run removed spool dir: %v", err)
	}
	mfst, _ := os.ReadFile(filepath.Join(state, "manifest.jsonl"))
	if len(mfst) == 0 {
		t.Fatalf("dry-run truncated manifest")
	}
}

func TestE2E_Vacuum_RemovesOldArchive_KeepsFresh(t *testing.T) {
	state := t.TempDir()
	stageOldArchive(t, state, "stale.txt", []byte("stale"), 9999)
	// Fresh archive: mtime = now (no backdate)
	freshDir := filepath.Join(state, "sent")
	_ = os.MkdirAll(freshDir, 0o755)
	writeFile(t, filepath.Join(freshDir, "fresh__keep.txt"), []byte("fresh"), 0o644)

	runVacuumCmd(t, "--age=60", "--sender-state="+state)

	if _, err := os.Stat(filepath.Join(freshDir, "stale__stale.txt")); err == nil {
		t.Fatalf("stale archive should have been removed")
	}
	if _, err := os.Stat(filepath.Join(freshDir, "fresh__keep.txt")); err != nil {
		t.Fatalf("fresh archive removed (should keep): %v", err)
	}
}

func TestE2E_Vacuum_PrunesOldManifestEntries(t *testing.T) {
	state := t.TempDir()
	stageOldArchive(t, state, "old1.txt", []byte("a"), 9999)
	stageOldArchive(t, state, "old2.txt", []byte("b"), 9999)
	// One fresh entry.
	freshContent := []byte("c")
	freshDir := filepath.Join(state, "sent")
	writeFile(t, filepath.Join(freshDir, "fresh__keep.txt"), freshContent, 0o644)
	rec := map[string]any{
		"completed_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"started_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"session_id":     "fresh-sid",
		"filename":       "keep.txt",
		"total_bytes":    len(freshContent),
		"chunk_size":     1400,
		"chunk_total":    1,
		"content_sha256": fmt.Sprintf("%x", sha256.Sum256(freshContent)),
		"destination":    "10.0.0.0:1",
		"mode":           "0o644",
		"redundancy":     1,
		"soh_redundancy": 3,
		"signed":         false,
		"snapshot":       filepath.Join(freshDir, "fresh__keep.txt"),
		"tx_status":      "sent",
	}
	b, _ := json.Marshal(rec)
	f, _ := os.OpenFile(filepath.Join(state, "manifest.jsonl"),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	f.Write(b)
	f.Write([]byte("\n"))
	f.Close()

	runVacuumCmd(t, "--age=60", "--sender-state="+state)

	// Manifest should now contain only the fresh entry.
	mfst, _ := os.ReadFile(filepath.Join(state, "manifest.jsonl"))
	lines := bytes.Split(bytes.TrimSpace(mfst), []byte("\n"))
	if len(lines) != 1 || !bytes.Contains(lines[0], []byte("fresh-sid")) {
		t.Fatalf("manifest not pruned correctly:\n%s", mfst)
	}
}

func TestE2E_Vacuum_RemovesAbandonedSpool_KeepsActive(t *testing.T) {
	spool := t.TempDir()
	stageOldSpoolSession(t, spool, "abandoned", 9999)
	// Fresh "in-flight" session: chunks.bitmap just touched.
	stageOldSpoolSession(t, spool, "active", 0)
	// Touch the bitmap to "now" so the newest-mtime rule keeps it.
	now := time.Now().UTC()
	_ = os.Chtimes(filepath.Join(spool, "active", "chunks.bitmap"), now, now)

	runVacuumCmd(t, "--age=60", "--spool="+spool)

	if _, err := os.Stat(filepath.Join(spool, "abandoned")); err == nil {
		t.Fatalf("abandoned spool dir should have been removed")
	}
	if _, err := os.Stat(filepath.Join(spool, "active")); err != nil {
		t.Fatalf("active spool dir removed (should keep): %v", err)
	}
}

func TestE2E_Vacuum_RequiresAge(t *testing.T) {
	cmd := exec.Command(diodeBin, "--mode=vacuum", "--sender-state="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("vacuum without --age should fail")
	}
	if !strings.Contains(stderr.String(), "--age") {
		t.Fatalf("stderr should mention --age: %s", stderr.String())
	}
}

func TestE2E_Vacuum_RequiresTarget(t *testing.T) {
	cmd := exec.Command(diodeBin, "--mode=vacuum", "--age=60")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("vacuum with only --age should fail")
	}
	if !strings.Contains(stderr.String(), "--spool") && !strings.Contains(stderr.String(), "--sender-state") {
		t.Fatalf("stderr should mention --spool/--sender-state: %s", stderr.String())
	}
}
