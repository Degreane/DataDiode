# ADR-0006: Sender state, resend, vacuum, time-spread redundancy, and receiver completed-cache

- **Status:** Accepted
- **Date:** 2026-06-21
- **Deciders:** @degreane
- **Builds on:** [ADR-0005](ADR-0005-session-protocol.md) (session protocol; receiver bitmap + SOH-idempotency are the load-bearing properties this ADR exploits)

## Context

Today (post-ADR-0005) every `--mode=tx` invocation:
- Generates a fresh UUID v4 session_id and never persists it.
- Ships once, exits, leaves no record.
- If a chunk was lost and the receiver's spool is sitting at "743/750 chunks received," there is no operator workflow to fix it. The bitmap on the receiver shows exactly what's missing, but the sender can't re-emit only those chunks (no return path) and can't even re-emit the full session_id because it didn't keep one.

The user proposed the right fix: **persist sender history, and let the operator resend with the same session_id.** The receiver's existing semantics (silent-accept-duplicate-SOH, bitmap-dedup-DATA) make this work with **zero wire-format changes**. A resend with the same session_id against a partial receiver spool naturally fills only the missing bits.

Three additional asks rolled in:
1. A vacuum mode that prunes sent/received state older than N minutes.
2. All defaults (archive on/off, completed-cache size, manifest rotation, vacuum age) configurable via flags.
3. A reporting mode that emits the manifest in a parseable form for humans and pipelines.

## Decision

Add four primitives, all driven from the CLI:

1. **Sender state directory** (`~/.diode/` Linux/macOS, `%APPDATA%\diode\` Windows; override with `--sender-state=<dir>`).
2. **Archive** of successfully-sent files to `<state>/sent/<UTC-iso>__<basename>` (default on; `--no-archive` to skip).
3. **Manifest** at `<state>/manifest.jsonl` — append-only JSON Lines, one record per successful send. Optionally rotated when it exceeds `--manifest-rotate-bytes`.
4. **Vacuum** as a separate `--mode=vacuum` that prunes files older than `--age=<minutes>` from any combination of `--spool` (receiver) and `--sender-state` (sender).

And on the receiver:

5. **Recently-completed sids cache** (`--completed-cache=<N>`, default 1024, 0 = disabled). When set, a SOH for a sid in the cache is silently ignored (no spool dir is re-created); DATA frames for such a sid are dropped. This is what makes resend-after-completion behave sanely.

## CLI surface

### Tx-side flags (additions)

| Flag | Default | Purpose |
|---|---|---|
| `--sender-state=<dir>` | `~/.diode` (Linux/macOS), `%APPDATA%\diode` (Windows) | Parent directory for `sent/` and `manifest.jsonl`. |
| `--archive` / `--no-archive` | archive **on** | Copy the successfully-sent file to `<state>/sent/<UTC>__<basename>`. Without an archive, `--resend` falls back to using the original `--send-file` path if it still exists. |
| `--manifest-rotate-bytes=<N>` | 0 (no rotation) | When the manifest size reaches N, rename to `manifest.jsonl.<UTC>` and start fresh on next append. |
| `--session-id=<hex-uuid>` | (auto) | Override the auto-generated session_id. Useful for scripts / debugging. Mutually exclusive with `--resend`. |
| `--resend=<session_id>` | — | Look up sid in `manifest.jsonl`, re-ship from the archived snapshot (or the original path if `--no-archive` was used and the file still exists). Reuses the same chunk_size and content_sha256 by construction. Mutually exclusive with `--send-file` / `--in` / `--session-id`. |
| `--resend-latest=<basename>` | — | Convenience: find the most recent manifest entry whose filename matches and behave like `--resend=<that sid>`. |

### Tx-side flags — redundancy ordering

| Flag | Default | Purpose |
|---|---|---|
| `--redundancy-order=consecutive\|spread` | `spread` | `consecutive` ships N copies of each chunk back-to-back (today's behavior); `spread` ships all chunks once, then all chunks again, N passes total. Same total bytes; `spread` survives burst loss much better because a burst that wipes K consecutive datagrams costs 1 copy of K different chunks instead of N copies of (K/N) chunks. |
| `--soh-interval=<N>` | 256 | When > 0, the sender re-emits the SOH every N DATA frames (in addition to the initial `--soh-redundancy` copies). Protects against the catastrophic "first 100µs burst wipes all SOH copies and the receiver silently drops every DATA that follows" failure mode. Set to 0 to disable. |

The combination of `--redundancy-order=spread`, `--soh-redundancy=3`, and `--soh-interval=256` (all defaults) makes a 750-chunk transfer survive any single ≤2-chunk-window burst with default `--redundancy=1`, and any single ≤(2×N)-chunk burst with `--redundancy=N` — at zero extra wire bytes vs the consecutive-copies layout.

### Rx-side flag (addition)

| Flag | Default | Purpose |
|---|---|---|
| `--completed-cache=<N>` | 1024 | FIFO cache of recently-completed session ids. SOH for a cached sid is silently accepted-and-ignored; DATA frames for one are dropped. Set to 0 to revert to the "always re-deliver on resend after completion" behavior. |

### New mode: `--mode=manifest`

Prints the manifest in a parseable format. Read-only.

```
diode --mode=manifest                                    # human table
diode --mode=manifest --format=json                      # raw JSONL
diode --mode=manifest --format=tsv                       # tab-separated (id, ts, filename, bytes, dst, status)
diode --mode=manifest --since=24h                        # filter by completed_at
diode --mode=manifest --sender-state=/etc/diode          # alternate state dir
```

### New mode: `--mode=vacuum`

Removes spool sessions, archived snapshots, and old manifest entries older than `--age` minutes. One-shot; designed to be run from cron.

```
diode --mode=vacuum --age=1440 --spool=/var/spool/diode
diode --mode=vacuum --age=10080 --sender-state=~/.diode  # 7 days
diode --mode=vacuum --age=1440 --spool=/var/spool/diode --sender-state=~/.diode --dry-run
```

Flags:

| Flag | Default | Purpose |
|---|---|---|
| `--age=<minutes>` | (required) | Age threshold; files / directories whose modification time is older than `now - age` are candidates. |
| `--spool=<dir>` | — | Vacuum the receiver spool: any `<sid>/` directory whose newest member is older than the threshold is removed. |
| `--sender-state=<dir>` | — | Vacuum the sender state: `sent/<UTC>__*` files older than threshold are removed; manifest entries with `completed_at` older than threshold are rewritten out (in-place atomic). |
| `--dry-run` | false | Print actions, perform nothing. |
| `--verbose` | false | Print every action taken. |

At least one of `--spool` or `--sender-state` must be supplied. Vacuum **never** touches a session whose mtime indicates recent chunk activity — that's how it avoids racing with an in-flight transfer.

## Manifest record format (JSON Lines)

One record per line, UTF-8, no trailing whitespace. Schema additions are forward-only.

```json
{
  "started_at":     "2026-06-21T20:15:23.123456Z",
  "completed_at":   "2026-06-21T20:15:25.789012Z",
  "session_id":     "3f29b1a2-c8e4-4f1d-9b6a-aeb5e7c84021",
  "filename":       "report.pdf",
  "total_bytes":    1048576,
  "chunk_size":     1400,
  "chunk_total":    750,
  "content_sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "destination":    "10.0.0.20:9999",
  "mode":           "0o644",
  "redundancy":     1,
  "soh_redundancy": 3,
  "signed":         true,
  "snapshot":       "/home/op/.diode/sent/2026-06-21T20-15-23Z__report.pdf",
  "tx_status":      "sent",
  "note":           ""
}
```

`snapshot` is empty when `--no-archive` was used.
`tx_status` is `"sent"` on a clean completion of the tx loop. (Without a return channel, the sender cannot know whether the receiver actually completed the session — that's a receiver-side fact. For end-to-end status the operator looks at the receiver's stats line or the spool dir.)

## Resend semantics — exact behavior

When you run `--resend=<sid>`:

1. Sender reads `manifest.jsonl`, finds the **most recent** record with that `session_id`.
2. Sender opens the `snapshot` path (or the original `filename` path if `snapshot` is empty and the file still exists).
3. Sender recomputes the file's sha256. If it differs from the manifest's `content_sha256`, the resend is **refused with a clear error** (refusing to ship bytes that don't match what the receiver was promised — would just produce a `hash_mismatch` on completion).
4. Sender ships a fresh SOH (with the **same sid** and **same content_sha256**) followed by every DATA chunk.
5. Receiver's behavior:
   - If it has the session in its spool: SOH is a no-op; DATA frames already received are deduped; missing ones fill in; finalize as soon as bitmap fills.
   - If the session is in the receiver's completed-cache: SOH dropped, DATA dropped. (Resend was unnecessary; file already delivered.)
   - If neither: fresh session, full re-receive, finalize as if first time.
6. Resend appends a new manifest line with the **same session_id** and a fresh `started_at` / `completed_at` (so operators can see "this sid was resent at T1 and T2").

## Receiver completed-cache — exact behavior

In-memory ring buffer of the last N session_ids that successfully completed. On startup the cache is empty (receivers do not persist this — restart looks like "I never saw that sid before" which is the safest assumption). On `IngestSOH`:
- If sid in cache: silent accept, do not provision a spool dir, do not increment `soh_accepted`.
- Else: existing logic.

On `IngestDATA`:
- If sid not in `sessions` AND sid in completed-cache: drop with `data_dropped++`, no log.
- Else: existing logic.

A successful `finalize` adds the sid to the cache, evicting the oldest if the cache is full. `--completed-cache=0` disables the cache (current re-deliver-on-resend-after-completion behavior).

## Manifest rotation — exact behavior

Default: never rotate. With `--manifest-rotate-bytes=N`:
- Before each append, the sender stats the manifest. If size ≥ N:
  1. `rename(manifest.jsonl, manifest.jsonl.<UTC>)`
  2. Create a fresh empty manifest.
  3. Append the new line to the fresh manifest.
- Rotation is atomic (rename is atomic on a single filesystem).
- `--mode=manifest` reads ONLY the active file. Rotated files are operator-owned; gzip/rotate/archive them with standard tools.

## Vacuum — exact behavior

For each candidate directory or file:
1. Determine its "freshness" timestamp:
   - **Receiver spool `<sid>/`**: max(mtime) of `chunks.bitmap`, `data.partial`, and any `chunks/*.bin`. A session that just received a chunk has a fresh `chunks.bitmap`, so it's safe from vacuum.
   - **Sender archive `sent/<UTC>__name`**: file's own mtime.
   - **Manifest entries**: parsed `completed_at`.
2. If `now - freshness > --age`, mark for deletion.
3. With `--dry-run`: print the candidate list, exit 0.
4. Without `--dry-run`: `os.RemoveAll` on spool dirs, `os.Remove` on archive files; rewrite the manifest in place (atomic temp + rename) without the expired entries.

Vacuum is conservative: it never deletes a session whose freshness equals the threshold exactly (must be strictly older), and it logs each action when `--verbose`.

## Consequences

### Positive
- **Partial-spool recovery becomes a first-class operator workflow.** "1% of the time a transfer is incomplete? Re-run with `--resend=<sid>` from cron after the original tx exits."
- **The receiver doesn't need to change much** — `--completed-cache` is the only addition, and the dedup-via-bitmap path that makes resend work is already there.
- **Vacuum solves the unbounded-spool growth problem** without requiring a daemon — cron-friendly.
- **JSONL manifest is human and machine-readable** with no special tooling (`jq`, `tail -f`, awk all work).
- **Archive is opt-out, not opt-in** — operators get the safety net by default and pay the disk cost knowingly via the prominent flag.

### Negative
- **Archive doubles local disk usage on the sender** unless vacuumed or disabled. Documented loudly.
- **Manifest is per-user state** in the default `~/.diode/`. Running the tx as different users produces different manifests. Service accounts should set `--sender-state=/var/lib/diode` to share.
- **Completed-cache doesn't survive receiver restart**, so a resend immediately after a receiver restart looks fresh. Acceptable; persisting the cache is a future enhancement.

### Risk: resend with a wrong file
Operator passes `--session-id=<sid> --send-file=<wrong-file>`. The receiver's chunks would mix bytes from the original with bytes from the wrong file → SHA fails on finalize → receiver retains the spool dir with both messes intermingled, and the operator sees the mismatch in the stats line. This is the same failure mode as today's "I tampered the SHA" path; documented in the threat model.

Mitigation: when the operator uses `--resend=<sid>` (recommended path) we re-hash the snapshot and refuse to ship if the SHA doesn't match the manifest's recorded value. Only the manual `--session-id` path lets you shoot yourself in the foot, and it requires an explicit override.

## Test plan

- Unit:
  - manifest writer: append, JSONL invariants, atomic-rename on rotation
  - manifest reader: parse, filter by sid, filter by `since`
  - vacuum: dry-run vs real, spool dir age computation, manifest in-place rewrite
  - completed-cache: ring eviction, lookup, SOH/DATA dropping
- E2E:
  - **Resend fills only missing chunks**: start rx, send a 100-chunk file but kill tx after 50 chunks (use a low `--rate` so we can interrupt mid-flight); restart tx with `--resend=<sid>`; expect rx stats `data_dup ≈ 50`, completed=1, file hash matches.
  - **Resend after completion with cache enabled**: send a file, wait for completion, run `--resend=<sid>` again; expect rx logs the second SOH as dropped (cached), no second delivery.
  - **Resend with `--no-archive` and original deleted**: clear error, manifest entry retained.
  - **Resend with wrong file via `--session-id`**: hash_mismatch on receiver side.
  - **Vacuum dry-run** doesn't change disk; real vacuum removes the expected entries.

## Open questions deferred

- **Persistent receiver completed-cache** (survive restart). Adds disk I/O on every completion. Defer until a customer asks.
- **Sender-side delivery confirmation** via an out-of-band side channel (e.g., a separate "ack diode" in the opposite direction). Not really a diode story; defer indefinitely.
- **Partial-resend** ("only chunks N..M"). Would require the sender to know which chunks the receiver is missing, which requires a return channel — defeats the diode. Resend-everything is the right diode-compatible approach.
