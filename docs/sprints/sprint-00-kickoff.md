# Sprint 00 — Kickoff & Research

- **Dates:** 2026-06-21 → 2026-07-04
- **Goal:** Establish the project — research, architecture decisions, and a minimal end-to-end "hello-diode" prototype on loopback.

## Sprint Goal (one sentence)

> Prove the concept: a `diode-tx` binary reads a line on the low side, ships it over one-way UDP, and a `diode-rx` binary prints it on the high side — with no reverse path, on Linux, Windows, and macOS.

## Backlog

| ID | Story | Estimate | Owner | Status |
|---|---|---|---|---|
| S00-1 | Research: data diode concept, prior art, vendors | M | — | 🟡 in progress (see `docs/research/`) |
| S00-2 | Research: language evaluation (Go vs Rust vs Python vs Java) | S | — | 🟡 in progress |
| S00-3 | ADR-0001: language choice | S | — | 🟡 drafted, pending review |
| S00-4 | ADR-0002: on-wire framing format (protobuf vs CBOR vs custom) | S | — | ⚪ todo |
| S00-5 | ADR-0003: FEC strategy (Reed-Solomon vs redundancy vs none for v1) | S | — | ⚪ todo |
| S00-6 | Threat model document (`docs/architecture/threat-model.md`) | M | — | ⚪ todo |
| S00-7 | Skeleton repo layout (`cmd/`, `internal/`, `plugins/`, `test/`) | S | — | ⚪ todo |
| S00-8 | `diode-tx` MVP: stdin → length-prefixed frame → UDP send | M | — | ⚪ todo |
| S00-9 | `diode-rx` MVP: UDP receive → verify hash → stdout | M | — | ⚪ todo |
| S00-10 | E2E test on loopback (Linux) | S | — | ⚪ todo |
| S00-11 | GitHub Actions CI: build & test on linux/windows/macos | S | — | ⚪ todo |
| S00-12 | LICENSE decision (Apache-2.0 vs MIT) | XS | — | ⚪ todo |

Estimates: XS≈½d, S≈1d, M≈2-3d, L≈4-5d.

## Out of Scope (this sprint)

- Plugin host (WASM or subprocess).
- Any real protocol adapter (syslog, file, etc.).
- FEC implementation (just naive redundancy if anything).
- TLS / signing.
- Windows service / systemd unit files.

## Risks

- **Language re-litigation.** If the deep-research report contradicts ADR-0001, we may lose a day re-deciding. *Mitigation:* time-box the re-evaluation to half a day.
- **UDP on Windows quirks.** Behavior with `WSAECONNRESET` on missing receiver can surprise. *Mitigation:* test on Windows early, in S00-10.

## Daily Notes

### 2026-06-21
- Repo initialised. Docs scaffolding in place. Deep-research workflow kicked off in background.
- Drafted ADR-0001 (Go + WASM/subprocess plugins).
- Next: wait for research output, then start S00-7 (skeleton layout) and S00-8/9 (MVP daemons).

## Review (end of sprint)

_To be filled in at sprint close._

## Retro (end of sprint)

_To be filled in at sprint close._
