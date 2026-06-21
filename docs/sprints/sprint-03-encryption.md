# Sprint 03 — Encryption on the wire (and the deferred Sprint 02 carry-over)

- **Dates:** 2026-07-05 → 2026-07-18 (notional)
- **Sprint Goal:** Close the last HIGH-severity gap in the threat model
  (I-1, plaintext on the wire) by replacing HMAC with AES-256-GCM AEAD,
  while keeping the unkeyed-pass-through mode fully functional.

## Sprint Goal (one sentence)

> When `--key-file` is set on both sides, the payload is encrypted on
> the wire with AES-256-GCM. When `--key-file` is absent on both sides,
> the diode behaves exactly as today (unsigned, unencrypted, plain
> bytes on the wire). Anyone with the PSK can read; nobody else can.

## Backlog

| ID | Story | Est | Status |
|---|---|---|---|
| S03-PSK | PSK creation/distribution/verification howto (`docs/psk-howto.md`) | XS | ✅ |
| S03-1   | ADR-0008 + impl: AES-256-GCM AEAD (v3 wire), HKDF subkey, **unkeyed mode unchanged** | L | ✅ |
| S03-2   | `--mode=psk` built-in PSK generator (cross-platform alternative to `openssl rand`) | S | ✅ |
| S03-3   | E2E tests for resend + vacuum (carry-over from S02) | M | ⚪ |
| S03-3   | Update `scripts/demo.sh` to use the v2/v3 `--send-file` flow | S | ⚪ |
| S03-4   | ADR-0007: replay protection (signed monotonic high-water mark) | M | ⚪ |
| S03-5   | ADR-0009: Reed-Solomon FEC | L | ⚪ |
| S03-6   | Plugin host design + WASM PoC (long-deferred from S02) | L | ⚪ |
| S03-7   | Optional `--vacuum-interval` in rx (in-process cleanup loop) | S | ⚪ |
| S03-8   | Sprint review + retro | XS | ⚪ |

## Non-negotiable: unkeyed mode stays a first-class path

The default operator experience MUST work without any key material:

```bash
diode --mode=tx --dst=10.0.0.20:9999 --send-file=report.pdf       # no --key-file
diode --mode=rx --listen=:9999 --files-to=/srv/incoming           # no --key-file
```

Rationale: not every deployment needs encryption. Air-gapped fiber,
local-loopback testing, demos, and CTF setups all have legitimate
reasons to skip the key step. We optimise for "secure when keyed,
trivial when not." This requirement is locked at the wire level
(unsigned frames are first-class in v3, just as in v0/v1/v2) and at
the CLI level (no flag becomes required when adding encryption).

## Definition of Done (sprint)

- `--key-file` present on both sides → AES-256-GCM-encrypted on the wire,
  `tcpdump -X` shows random bytes for the payload.
- `--key-file` absent on both sides → plain bytes on the wire, exactly
  as v2 today.
- Mismatched (one keyed, one not) → receiver drops every frame, stats
  line shows the imbalance.
- Wrong-key → receiver drops with `data_decrypt_failed` counter.
- All existing unkeyed E2E tests still pass with no flag changes.
- New E2E tests cover all three matrix cells (both-keyed, both-unkeyed,
  mismatched).
- Threat model updated: **I-1 marked CLOSED**.
- Tutorial updated: keyed flow now described as "encrypts + authenticates."
- Cross-compile clean for all 7 targets.
