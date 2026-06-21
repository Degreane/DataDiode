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
| S02-9  | Sprint review + retro | XS | ⚪ |

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
