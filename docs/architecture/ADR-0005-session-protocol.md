# ADR-0005: Session-based wire protocol (v2)

- **Status:** Accepted
- **Date:** 2026-06-21
- **Deciders:** @degreane
- **Supersedes:** [ADR-0002](ADR-0002-frame-format.md) (v1 frame format)
- **Subsumes:** the file-envelope from S02-1 (`internal/fileenv` is deleted; its job moves into the SOH)
- **Compatible with:** [ADR-0004](ADR-0004-hmac-signing.md) (HMAC trailer applies to both SOH and DATA frames identically)
- **Closes:** the msg_id-collision foot-gun that fails concurrent-sender tests, the in-memory-until-complete reassembly memory issue, and the disk-write-blocks-UDP-read back-pressure that motivated the async-pool dead-end.

## Context

The v1 protocol (ADR-0002) keyed reassembly by a per-sender `msg_id (uint32)`. That has three problems we cannot grow past:

1. **Sender collisions.** Two senders both start `msg_id=0` and the receiver can't tell their chunks apart.
2. **No upfront sizing.** The receiver doesn't know `total_bytes` until the last chunk arrives. It has to buffer everything in memory.
3. **Atomic completion forces a big synchronous write at the end.** That write blocks the UDP read loop and causes datagrams for *other* in-flight transfers to be lost to kernel buffer overflow.

The session model proposed by the user (and refined here) fixes all three with one mechanism.

## Decision

A v2 wire protocol with **two frame types** sharing a common envelope:

```
common (28 B):  magic | ver=02 | flags | session_id(16)
  - then SOH-specific or DATA-specific fields
  - then sha256(header+payload)
  - then optional hmac-sha256 (when --key-file is set, per ADR-0004)
```

### SOH frame

A *control* frame the sender emits **before any DATA frame** for a session. Carries everything the receiver needs to provision storage and verify completeness.

```
offset  size   field           notes
   0     4     magic = "DDO\0"
   4     1     version = 0x02
   5     1     flags           bit 7 SOH=0x80 (set), bit 5 REDUNDANT=0x20, bit 4 SIGNED=0x10
   6    16     session_id      UUID v4, 128-bit
  22     4     chunk_total     uint32 BE
  26     4     chunk_size      uint32 BE   nominal chunk size (last chunk may be smaller)
  30     8     total_bytes     uint64 BE
  38    32     content_sha256  SHA-256 of the file contents (not the SOH itself)
  70     4     mode            uint32 BE   Unix file mode bits (Windows uses owner-write only)
  74     2     name_len        uint16 BE
  76    N     name             UTF-8 basename, no '/'\\\\NUL/'.'/'..'
  76+N  32    header_sha256   covers bytes [0..76+N)
[76+N+32 32  hmac]            when SIGNED, per ADR-0004; HMAC scope = same as sha256
```

Sender SHOULD ship SOH multiple times if `--redundancy>1` (the SOH is the most expensive frame to lose — a missed SOH means the session never starts even if every DATA frame arrives).

### DATA frame

```
offset  size   field           notes
   0     4     magic = "DDO\0"
   4     1     version = 0x02
   5     1     flags           bit 7 SOH=0 (clear), bit 6 FINAL=0x40 (last chunk), bit 5 REDUNDANT=0x20, bit 4 SIGNED=0x10
   6    16     session_id
  22     4     chunk_index     uint32 BE   0..chunk_total-1
  26     2     payload_len     uint16 BE   1..1400
  28    N      payload         opaque content bytes
  28+N  32     sha256          covers bytes [0..28+N)
[28+N+32 32   hmac]            when SIGNED
```

### Frame size summary

| | Header | + sha | + hmac | Total |
|---|---|---|---|---|
| SOH (no name) | 76 | +32 | +32 | 108 (signed) / 76 (unsigned) — practically 76+N+(32 or 64) |
| DATA (full chunk) | 28 + 1400 | +32 | +32 | 1492 (signed) / 1460 (unsigned) |

DATA-frame **chunk overhead drops from v1's 26+32+envelope to just 28+32** (or 60+32 signed). The per-chunk filename/mode metadata is no longer carried per-frame — it lives in the SOH.

## Receiver state machine

Per session_id, the receiver lives in one of three states:

```
            ┌───────────┐    DATA frame arrives    ┌───────────┐
            │  UNKNOWN  │ ──────────────────────►  │ DROPPED   │  (1 syscall: stat session dir)
            └─────┬─────┘                          └───────────┘
                  │ SOH frame arrives
                  ▼
            ┌───────────┐    DATA frame arrives    ┌───────────┐
            │  ACTIVE   │ ──────────────────────►  │ DEDUPED   │  (bit already set)
            └─────┬─────┘                          └───────────┘
                  │
                  │ all chunks present
                  ▼
            ┌───────────┐
            │ COMPLETED │    atomic-rename to --files-to / write to --out
            └───────────┘
```

When the receiver sees a DATA frame for a session_id it doesn't have, **drop immediately**. No decode beyond the session_id field. This is the "fast early reject" the user asked for.

## Spool layout

Per session, one directory under `--spool` (default `/var/spool/diode` on Linux, `%TEMP%\diode` on Windows):

```
/var/spool/diode/<session_id>/
├── meta.json          ← SOH metadata, JSON, human/automation parseable
├── chunks.bitmap      ← one bit per chunk; bit i set ⇔ chunk i received
└── data.partial       ← (default mode) sparse file of total_bytes; chunks pwrite'd at offset
    OR
└── chunks/            ← (--spool-mode=files) per-chunk files, zero-padded names
    ├── 00000.bin
    ├── 00001.bin
    └── ...
```

### `meta.json` schema (locked v1)

```json
{
  "session_id":     "3f29b1a2-c8e4-4f1d-9b6a-aeb5e7c84021",
  "version":        "v2",
  "filename":       "report.pdf",
  "mode":           "0o644",
  "total_bytes":    1048576,
  "chunk_size":     1400,
  "chunk_total":    750,
  "content_sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "started_at":     "2026-06-21T19:42:11Z",
  "sender_hint":    "10.0.0.42:42013",
  "spool_mode":     "sparse"
}
```

Schema additions are forward-only (new fields = ignored by older readers). Field removals require an ADR.

### `chunks.bitmap` format

Raw little-endian bit array. Bit `i` (i.e., byte `i/8`, bit `i%8` from the LSB) is set ⇔ chunk `i` has been received. File size = `ceil(chunk_total / 8)` bytes. Operator visibility: `xxd chunks.bitmap` or any bit-counting tool.

### `--spool-mode` flag (user requested)

| Value | Layout | Best for | Trade-off |
|---|---|---|---|
| `sparse` (default) | one sparse `data.partial` + bitmap | files of any size, throughput | one big file, less forensic per-chunk visibility |
| `files` | `chunks/<NNNNN>.bin` per chunk | small files, CTF/audit/forensic use | 3 syscalls/chunk, inode pressure on huge files |

Both modes write the same `meta.json`. Both modes are resumable across receiver restarts.

## Completion semantics

When every bit in `chunks.bitmap` is set:

1. **sparse mode:** `fsync(data.partial)`, verify `sha256(data.partial) == meta.content_sha256`, then `rename(data.partial, <files-to>/<filename>)`, then `chmod` to declared mode, then `rmdir <session_id>/`.
2. **files mode:** sequentially read all chunk files into a tmp output file, verify sha, rename, cleanup.

A SHA-256 mismatch at finalization → log loudly, do NOT deliver, leave the spool dir in place for forensic inspection.

## Sender flow

1. Open input. Pre-compute `total_bytes`, `chunk_total = ceil(total_bytes / chunk_size)`, `content_sha256` (one read pass).
2. Generate `session_id` (UUID v4 from `crypto/rand`).
3. Emit SOH frame (× `--soh-redundancy` for loss tolerance — separate from `--redundancy` for DATA).
4. Emit DATA frames in order, optionally with `--redundancy` copies each.
5. Last DATA frame carries `FINAL` flag (advisory; receiver knows completion from bitmap).
6. Exit.

If `total_bytes` is unknown ahead of time (e.g., piping from `tail -f`), `--mode=tx` requires `--in=<file>` for v2; stdin support is deferred to a future ADR with streaming sessions.

## Backward compatibility

- **None at the wire layer.** v1 receivers see `version=0x02` and silently drop via the existing version check. v2 receivers do the same for v1 frames.
- **CLI surface is preserved.** `--mode=tx|rx`, `--send-file`, `--files-to`, `--out`, `--key-file`, `--chunk`, `--redundancy`, `--rate` all keep their names and meanings. The flags `--max-pending`, `--max-bytes`, `--recent-msg-cache`, `--deliver-workers`, `--deliver-queue` go away (no longer meaningful). New flags: `--spool`, `--spool-mode`, `--soh-redundancy`.
- **`internal/fileenv` is deleted** (its job moves into the SOH).
- **`internal/reassembly` is replaced by `internal/session`.**
- **`internal/delivery`** (async pool from the abandoned S02-7 first attempt) is deleted.

## Consequences

### Positive
- **Concurrent senders work correctly** — UUID session_ids never collide in practice.
- **No memory bound issue** — receiver writes chunks straight to disk as they arrive. The 64-MiB-message limit goes away.
- **No async-pool needed** — writes are small (`pwrite` per chunk in sparse mode, one small file write in files mode) and don't block the read loop.
- **Resumable across receiver restart** — full state lives on disk.
- **"What's missing"** is a bitmap read — scriptable, fast, no special tooling.
- **Per-chunk frame is smaller** — no per-chunk filename/mode/chunk_total metadata.
- **Path traversal is impossible** — session_id is generated by the receiver from the sender's UUID (hex-only); the filename is validated identically to the deleted fileenv code.

### Negative
- **SOH loss is fatal to the session.** Without the SOH, the receiver has no spool dir, so every DATA frame for that session is dropped at the door. Mitigation: `--soh-redundancy` default 3.
- **Sender must do a precompute pass** (sha + size). Trivial for files; precludes streaming stdin without buffering.
- **v1 receivers must be upgraded** in lockstep with v1 senders. Acceptable for a sprint-stage product; documented loudly.
- **Spool disk usage** can grow if many sessions are abandoned mid-flight. Mitigation: `--spool-ttl` (default 24h) cleans up sessions whose SOH is older than the TTL with no recent chunk activity.

### Risk: spool path traversal
The receiver assembles `<spool>/<session_id>/<filename>`. session_id is the **server-derived hex** of the wire UUID (never used unsanitised); filename is validated (basename-only, no `/`, no `\`, no `..`, no NUL).

## Test plan

- **Framing v2:** roundtrip SOH and DATA, all validation rule edges, fuzz on Decode.
- **Session sparse mode:** SOH → bitmap → chunks arrive out of order → pwrite to correct offsets → sha verify on completion → atomic rename. Crash mid-session and resume.
- **Session files mode:** same, but per-chunk files. Concatenation order verified.
- **Mode parity:** the same payload via sparse and via files produces byte-identical output.
- **Unknown session_id drop:** DATA frame with no preceding SOH → counter increment, no disk activity.
- **Concurrent senders:** the failing test from the async-pool attempt MUST now pass (5 senders × 200 KiB).
- **E2E:** Windows and Linux receivers both work with `--spool=<dir>` and `--files-to=<dir>`.

## Open questions deferred

- **Streaming stdin sessions** (no upfront sha). Probably a v2.1 with a `STREAMING` flag on the SOH and chunked sha (Merkle-style) so the receiver can verify as it goes.
- **SOH-only signing key** vs whole-stream signing (could allow a different key for control vs data — defer).
- **Multi-file session** (one SOH with manifest of N files). Defer; tar-then-send is the workaround.
