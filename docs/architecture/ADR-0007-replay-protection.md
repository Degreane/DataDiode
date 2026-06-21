# ADR-0007: Persistent completed-cache for replay protection

- **Status:** Accepted
- **Date:** 2026-06-21
- **Deciders:** @degreane
- **Builds on:** [ADR-0005](ADR-0005-session-protocol.md) (session protocol),
  [ADR-0006](ADR-0006-sender-state-resend-vacuum.md) (in-memory completed-cache + vacuum)
- **Closes (partially):** replay attacks against the keyed (or unkeyed) wire by an
  attacker who captured a complete session's worth of frames

## Context

After ADR-0008 the wire is AEAD-encrypted with a PSK, so an on-path
attacker cannot forge new frames or decrypt captured ones. They can,
however, **replay** captured ciphertext bit-for-bit. The receiver's
existing in-memory `--completed-cache` (default 1024 sids) catches
this for **recent** sessions, but has three weaknesses:

1. **Doesn't survive receiver restart.** A restart loses the cache;
   any prior session can be replayed exactly once before the new
   cache learns the sid.
2. **Bounded by entry count, no age dimension.** Operators can't say
   "remember every sid seen in the last 30 days."
3. **No vacuum integration.** The cache is just RAM; `--mode=vacuum`
   doesn't touch it.

This ADR adds a **disk-backed completed-cache** so the in-memory ring
is hydrated from disk at startup and grows persistently. Combined
with the AEAD on the wire (ADR-0008), this closes the practical
replay-attack surface for any sender→receiver pair that's been
running long enough for the cache to span the attacker's capture
window.

## Decision

Add `<spool>/completed.idx`, a flat append-only file written by the
receiver every time a session finalizes. Format: one record per line,
tab-separated:

```
<session-id-as-hex>\t<completed-at-rfc3339>\n
```

On `--mode=rx` startup:
1. Open `<spool>/completed.idx` if it exists.
2. Read each line (bounded by `--completed-cache-disk-cap`, default
   100,000) into the in-memory cache (the same one ADR-0006 uses).
3. If the in-memory cache fills before all disk entries are loaded,
   load the **most recent** (file is iterated to end; oldest entries
   that overflow are simply not loaded into RAM).
4. Continue normal operation.

On every successful finalize:
- Append one line to `<spool>/completed.idx` (open, write, fsync, close)
- Update the in-memory ring as before

On `--mode=vacuum`:
- Add `--age` pruning of `<spool>/completed.idx`: rewrite the file in
  place keeping only entries with `completed_at >= now - age`.
  Same atomic-temp-and-rename pattern as the manifest pruner.

## CLI surface

| Flag | Default | Where | Purpose |
|---|---|---|---|
| `--completed-cache=<N>` | 1024 | rx | In-memory ring size (existing; unchanged) |
| `--completed-cache-disk` | true | rx | Persist completions to `<spool>/completed.idx`. Set false to disable disk persistence. |
| `--completed-cache-disk-cap=<N>` | 100000 | rx | Max records loaded from disk at startup (entries past this stay on disk; the disk file isn't rotated, vacuum handles it) |

The vacuum subcommand grows no new flags — `--age` and `--spool`
already implicitly cover the completed.idx pruning.

## Wire format change

**None.** This ADR is purely a receiver-side state change. v3 frames
unchanged; sender unchanged; cross-platform unchanged.

## Receiver state machine update

```
IngestSOH(soh):
  if sid in memCache:                   # already completed (in-memory or hydrated from disk)
    soh_for_completed++; drop
  if soh.SessionID in activeSessions:   # duplicate SOH (sender redundancy)
    return                              # silent accept, ADR-0006
  # provision new session …

finalize(session):
  # … existing assembly + sha verify + atomic rename
  memCache.add(session.SID)
  if opts.PersistentCompletedCache:
    append "<sid-hex>\t<completed-at>\n" to <spool>/completed.idx
    fsync
```

The disk append is fire-and-forget: a failed write logs a warning but
doesn't fail the session (the file is already delivered). Worst case
on disk-write failure: the in-memory cache still works for this run,
and a future restart wouldn't recognize the sid.

## Vacuum integration

```
vacuumSenderState pruning (existing): keeps recent manifest entries
vacuumSpool pruning  (existing): removes session dirs by newest-mtime
vacuumCompletedIdx   (new):       in-place rewrite of <spool>/completed.idx,
                                  drop lines whose timestamp is older than cutoff
```

The pruner is added to vacuum_cmd.go and triggered when `--spool` is
supplied (same as the existing spool-session pruner).

## Consequences

### Positive
- **Replay attacks rejected indefinitely** as long as the receiver was
  ever up at the time of the original completion. The disk-backed
  cache survives restart.
- **Bounded growth via vacuum.** The same `--mode=vacuum --age=N
  --spool=...` operator workflow now also prunes completed.idx.
- **No wire-format change** — every existing v3 receiver still
  interoperates with every v3 sender. Operators get the benefit by
  upgrading just their receiver binary.
- **No performance cost on the hot path.** One small fsync per
  completed session; negligible compared to the chunked file write
  that just happened.

### Negative
- **Spool dir is now operator state**, not transient. Previously you
  could `rm -rf /var/spool/diode` to "clean slate"; now that also
  resets the replay-protection set. Document.
- **No protection against the very first replay after a fresh start.**
  If an attacker has a captured session and the receiver is starting
  for the first time on a new spool dir, the replay will succeed.
  Mitigation: don't use a fresh spool dir as the canonical receiver
  install; or use `--completed-cache-disk=false` only when you
  genuinely want a clean slate.
- **completed.idx file grows linearly.** At 64 bytes/line (uuid + tab
  + iso timestamp + newline), 100 K entries ≈ 6.4 MiB. Vacuum keeps
  it bounded.

### Risk: tampering with completed.idx
An attacker who can write to the receiver's `<spool>` can simply
delete completed.idx to enable replays. This is **out of scope** for
the diode threat model (an attacker with that level of access has
the receiver root). Defense: protect the spool dir with normal Unix
permissions, run the receiver as a dedicated user, mount the spool
on a read-mostly partition for the lookup (and write to a small
journal).

## Test plan

- **Unit**:
  - completed.idx writer: one line per call, fsync, parse roundtrip
  - hydrate-from-disk: cache size cap honored (load most recent first)
  - vacuum prunes: in-place rewrite, atomic temp+rename, drop count
- **E2E**:
  - **Replay before restart**: send, capture frames, replay → cache
    rejects (this is ADR-0006 behavior; regression coverage).
  - **Replay after restart**: send, kill rx, restart rx (same spool),
    replay → disk-backed cache rejects.
  - **Replay survives a full restart** but is rejected because the
    disk file persists across rx process lifetimes.
  - **Vacuum prunes the disk cache**: run vacuum --age, replay an
    aged-out sid → now succeeds (vacuum cleared the entry).

## Open questions deferred

- **Receiver-multi-key** (different PSKs per sender): each gets its
  own subkey + its own scope in the cache (`<sid, key-id>` instead of
  just `<sid>`). Defer until multi-tenant rx becomes a thing.
- **Replay window via signed timestamp in SOH**: would require a v4
  wire bump. The disk cache covers the practical attack without that
  change.
