# Sprint 02 — File operations & polish

- **Dates:** 2026-07-05 → 2026-07-18 (notional; actually opened 2026-06-21 to satisfy a user-driven file-transfer requirement)
- **Sprint Goal:** Operators can ship a real file from low to high with name + mode preserved and integrity verified, end-to-end.

## Sprint Goal (one sentence)

> `diode --mode=tx --send-file=foo.tar` on the low side reconstructs the file at `<dir>/foo.tar` on the high side with the same mode bits and a verified SHA-256, with no operator scripting beyond the two CLI invocations.

## Why this jumped the queue

Original Sprint-02 plan from the S01 retro put plugins (WASM) first. A user request landed for file-oriented transfer — "specify a file, receive it as a file" — which is more immediately useful than the plugin host. File transfer is also the highest-leverage *application* of the v0 transport: it stress-tests reassembly, validates the integrity story, and gives operators something they can directly use.

## Backlog

| ID | Story | Est | Status |
|---|---|---|---|
| S02-1  | `internal/fileenv` envelope + `--send-file` (tx) + `--files-to` (rx) | M | ✅ |
| S02-2  | Update tutorial with file-transfer recipe | XS | ✅ |
| S02-3  | Run the live two-LXC demo end-to-end (carry-over from S01) | XS | ✅ — SHA-256 match across 10.99.0.10 → 10.99.0.20, reverse blocked, ICMP blocked |
| ~~S02-4~~ | ~~Push to GitHub~~ — **dropped:** repo is local-only by user direction; CI workflow stays in `.github/` as future-ready scaffolding | — | ❌ dropped |
| S02-5  | Makefile: `build`, `test`, `e2e`, `demo`, `lint`, `clean`, `cross`, etc. | S | ✅ |
| S02-6  | ADR-0004 + impl: HMAC-SHA256 frame signing via `--key-file` (opt-in) | M | ✅ |
| S02-7  | ~~Plugin host design~~ → **session-based protocol (ADR-0005)**: SOH + sparse/files spool, replaces v1 msg_id model, removes async-pool/fileenv/reassembly | L | ✅ |
| S02-8  | ADR-0006 + impl: sender state, resend, vacuum, time-spread redundancy, completed-cache | L | ✅ |
| S02-9  | Sprint review + retro | XS | ✅ |

## On-wire envelope for file transfer (v0)

Layered *inside* the existing diode payload — the transport doesn't know or care about file semantics; this is a pure application-layer header that lives in the bytes the reassembler delivers.

```
offset  size  field
   0     4    magic   = "DDF\0"
   4     1    version = 0x01
   5     1    flags   (reserved, must be 0)
   6     2    name_len  (uint16 BE, 1..255)
   8     4    mode      (uint32 BE, Unix file mode bits; only 0o777 honored on receive)
  12     8    size      (uint64 BE, content byte length)
  20    32    sha256    (over content bytes only, NOT the envelope header)
  52     N    name      (UTF-8 path component; basename only, no '/' or '..')
  52+N   S    content   (size bytes)
```

Total wire size = `52 + name_len + size` bytes. With the transport's 1400-byte chunks, a 1 MiB file becomes ~750 chunks per copy.

### Validation rules (receiver)

- Magic, version, reserved flag bits all match exactly.
- `name_len` ∈ [1, 255].
- `name` must equal `filepath.Base(name)` and contain no `/`, `\\`, or NUL. Anything else → reject (logged, not delivered).
- Declared `size` matches actual content bytes after the name field.
- Computed SHA-256 of content equals the envelope's hash.
- Write atomically: `<dir>/.<name>.partial-<rand>` then `rename` to `<dir>/<name>`. Mode applied with `chmod` after write.

## Non-goals (this sprint)

- Directories / archives (operator can `tar` first).
- Streaming files larger than `--max-message` (currently 64 MiB default; raise per invocation if needed).
- Resume / delta sync.
- Multi-recipient fan-out.

## Definition of Done (sprint)

- E2E test that sends a real binary file (e.g., the diode binary itself) through the diode and asserts byte-identical on the other side, including mode bits.
- `--send-file` is mutually exclusive with `--in`; `--files-to` is mutually exclusive with `--out` (CLI errors at parse time).
- Path-traversal rejection has a test (`name="../etc/passwd"` → rejected, no file created outside `--files-to`).
- Tutorial has a "send a file" recipe section.
- All prior sprint tests still green.

## Review (sprint close — 2026-06-21)

**Demo:** every flow shipped in this sprint runs end-to-end on the
host. The full session-protocol stack — `tx --send-file → SOH+DATA →
spool dir → bitmap → sha verify → atomic rename → manifest entry`
— moves a 3 MiB binary across loopback with zero loss in ~30 ms.
Resend with the same session id deduplicates correctly via the receiver's
bitmap. Resend after completion is recognised by the receiver's
completed-cache. Vacuum prunes old state. All seven cross-compile
targets build cleanly and `go vet` clean.

**Numbers:**
- 8 substantive commits (`ce3b88b` → `2134564`) plus 2 housekeeping commits.
- 3 new ADRs (ADR-0004 HMAC, ADR-0005 session protocol, ADR-0006 sender state).
- 3 internal packages added (`session`, `manifest`, `key-loader inline`).
- 3 internal packages **removed** as part of the v1→v2 transition
  (`fileenv` merged into SOH, `reassembly` replaced by `session`,
  `delivery` discarded as the wrong abstraction).
- Test suite count: 13 framing, 17 session, 9 manifest, 7 cmd/diode flag-parse,
  12 e2e (including 5 signed-mode), ~50 total in-tree + fuzz.
- Cross-target `go vet ./...` clean for linux/darwin/windows/freebsd × amd64/arm64.
- Lines of Go (production + tests): ~6,500.

**Definition of Done — status:**
- ✅ E2E byte-identical test for binary files (the diode binary itself,
  2.95 MiB, sha256 match across files-to).
- ✅ Mutually-exclusive source/sink flags rejected at parse time.
- ✅ Path traversal rejected at framing decode (`framing.validateName`
  + receiver re-base for defense in depth).
- ✅ Tutorial covers send-file, resend, manifest, vacuum, signed mode,
  burst-loss tuning.
- ✅ All prior sprint tests still green; v1 tests retired as part of
  v2 transition (intentional, ADR-0005 documents the break).

**ADRs landed:**
- [ADR-0004](../architecture/ADR-0004-hmac-signing.md) — HMAC frame
  authentication via `--key-file`. Closes threat-model S-1/T-1 (was HIGH).
- [ADR-0005](../architecture/ADR-0005-session-protocol.md) — v2
  session-based protocol with SOH preamble. Supersedes ADR-0002.
- [ADR-0006](../architecture/ADR-0006-sender-state-resend-vacuum.md) —
  Sender state, resend, vacuum, time-spread redundancy, receiver
  completed-cache.

## Retro

**What worked**

- **ADR-then-implement cadence continued to pay off.** Each major
  change (HMAC, session protocol, resend) was specced first, then
  built; the spec was the contract, and tests dropped out of it
  naturally. Particularly valuable when ADR-0006 was layered onto
  ADR-0005 — the "resend works for free because receiver bitmap +
  SOH-idempotency" insight came from re-reading ADR-0005, not from
  starting to write code.

- **Listening to user-driven redirects mid-sprint.** The async-pool
  story was the wrong abstraction; the user's pushback (session-based
  protocol) was the right one. Throwing away ~200 lines was cheap
  versus shipping the wrong design and having to undo it from a
  position of more code. The S02-7 redirect was probably the highest-
  leverage decision of the sprint.

- **Live verification at every step.** Every shipped feature got a
  one-shot live demo (manifest cat, sha-diff, stats line inspection)
  before commit. The bitmap-not-persisted-mid-flight bug was caught
  this way, fixed in 5 minutes, and would have shipped silently
  broken otherwise.

- **CLI flag everything.** The user explicitly asked for it on S02-8
  and it was the right instinct — `--archive`, `--completed-cache`,
  `--manifest-rotate-bytes`, `--age`, `--spool-mode`, `--redundancy-
  order`, `--soh-interval` are all individually tunable. No "magic
  defaults" hidden behind constants.

**What didn't**

- **Initial async-pool work was thrown away (~200 LoC + tests).**
  Caught the wrong direction late. Mitigation in retrospect: when
  user said "would the rx handle them concurrently?" the right
  response was to discuss the protocol-level fix first, not to start
  writing a worker pool. Lesson: **for questions that touch the
  protocol, design first; for questions that touch only the data
  plane, implement first.**

- **The msg_id-collision bug should have been caught earlier.** It
  surfaced only when the E2E concurrent-senders test was written; the
  reassembly unit tests didn't exercise multi-sender. Better: when
  writing protocol-level packages, **write an E2E test that exercises
  the worst-case sharing first**, before unit tests.

- **`sed` mid-refactor mangled three test files.** Twice I used
  `sed -i -E 's/Decode\(([^,)]+)\)/Decode(\1, nil)/g'` on test files
  and it inserted `, nil` into function declarations and slice index
  expressions. The fix is trivial after the fact but cost minutes of
  debugging. Lesson: **for API-changing refactors across many files,
  use `gopls rename`/`go fix` style structural rewrites, not regex.**
  Or accept the slower path of `grep -l` + hand-edit per file.

- **The original Sprint 02 plan listed "plugin host design" as S02-7.**
  Never started; redirected entirely. The plan was right to be a
  *living* doc, but the deviation should have triggered an explicit
  re-plan note rather than just an inline strikethrough.

**One change for next sprint**

- **Write the E2E test first when changing a protocol.** Specifically:
  before implementing the v2 session protocol, the very first test
  authored should have been "5 concurrent senders, 200 KiB each, all
  files must arrive byte-identical." That test would have surfaced
  the msg_id collision in the v1 design *before* anyone wrote v2
  framing — and (in this universe) the v2 protocol design would have
  come earlier.

**Carry-over to Sprint 03**

- **E2E tests for resend + vacuum.** Unit tests exist; black-box
  subprocess tests would catch wire-level regressions.
- **Update the LXC live demo** (`scripts/demo.sh`) to use the v2
  `--send-file` flow instead of the v1 stdin-pipe pattern.
- **ADR-0007: replay protection.** Beyond the completed-cache (which
  is in-memory only), add signed-monotonic high-water-mark protection
  for the keyed path.
- **ADR-0008: encryption.** Closes threat-model I-1 (still HIGH). PSK
  + AES-256-GCM with nonce derived from `seq`. Layers cleanly on top
  of ADR-0004 HMAC.
- **ADR-0009: FEC.** Closes threat-model D-3 (still MEDIUM). Reed-
  Solomon parity chunks; mark with a new flag bit. Bigger lift, but
  best per-byte loss tolerance.
- **Plugin host (originally S02-7).** Real protocol adapters — syslog,
  OPC-UA, file watcher — pluggable via the WASM model from ADR-0001.
- **Receiver `--vacuum-interval`**: an optional in-process vacuum
  loop so a long-running rx process doesn't depend on cron.
