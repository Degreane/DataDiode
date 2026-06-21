# ADR-0004: HMAC frame authentication via pre-shared key

- **Status:** Accepted
- **Date:** 2026-06-21
- **Deciders:** @degreane
- **Closes:** S-1 (frame spoofing), T-1 (in-flight tampering) from [threat-model.md](threat-model.md)
- **Related:** [ADR-0002](ADR-0002-frame-format.md) (frame format)

## Context

ADR-0002 ships frames with a trailing SHA-256, but a hash is not authentication. An attacker on the wire can forge perfectly valid frames — magic, version, structure, hash — and the receiver has no way to tell them apart from legitimate ones. The threat model rates this **HIGH** and gates the gap on "ADR-0004 (HMAC/Ed25519)."

The user requirement: **opt-in**. If no key is configured, the diode keeps behaving exactly as it does today (so existing deployments don't break). When a key is configured on both sides, the receiver MUST refuse anything that doesn't carry a valid signature.

## Decision

Use **HMAC-SHA256 with a pre-shared symmetric key**. The HMAC is appended after the existing SHA-256, and the new `FlagSigned` bit in the frame header marks the frame as signed.

### Frame layout when SIGNED (v1 wire-compatible)

```
offset           size  field                  notes
 0                4    magic = "DDO\0"        unchanged
 4                1    version = 0x01         unchanged — bumps when we add a v2
 5                1    flags                  bit 4 (FlagSigned = 0x10) set
 6..25           20    seq/msg_id/chunks/len  unchanged
26              N      payload                unchanged
26+N            32     sha256                 unchanged (integrity vs corruption)
26+N+32         32     hmac-sha256(key, [0..26+N))  NEW
```

Total wire size (signed) = `26 + N + 32 + 32 = 90 + N` bytes.
Total wire size (unsigned) = `26 + N + 32 = 58 + N` bytes (unchanged).

The HMAC covers exactly the same bytes as the SHA-256 (`header + payload`). The trailing SHA-256 stays for two reasons: (1) decoders can fail fast on corruption *before* doing the keyed verify; (2) the framing wire layout is the same whether keyed or not, only the trailing 32 bytes appear or vanish.

### Flag allocation

Previously reserved bits in the `flags` byte (ADR-0002 §"Flags byte"):

```
bit 7 (0x80)  FINAL       (existing)
bit 6 (0x40)  HEARTBEAT   (existing)
bit 5 (0x20)  REDUNDANT   (existing)
bit 4 (0x10)  SIGNED      ← NEW
bit 3..0      reserved    Must be 0
```

`flagsReserved` mask narrows from `0x1F` to `0x0F`. A v0-only receiver seeing a SIGNED frame will reject it via the reserved-bits check (we don't want unsigned receivers silently accepting signed frames they can't verify).

### Receiver policy

- **Receiver has NO key configured:** continue to reject any frame with bits in `flagsReserved` set. SIGNED frames look like reserved-bit-set frames and are dropped. Identical to today's behavior.
- **Receiver HAS a key configured:** accept ONLY frames with SIGNED set and a valid HMAC. Unsigned frames are dropped (the attacker must not be able to bypass auth by simply not signing).

### Sender policy

- **Sender has NO key configured:** emit unsigned frames (today's behavior).
- **Sender HAS a key configured:** set SIGNED on every frame and append the HMAC. Heartbeats are signed too.

### Key material

- **Source:** `--key-file=<path>` on both `--mode=tx` and `--mode=rx`. The file contains either:
  - Hex-encoded bytes (at least 64 hex chars = 256 bits; longer accepted), or
  - Raw binary bytes (at least 32 bytes).
- **Detection:** if the file content, after whitespace stripping, is all hex characters AND has even length, decode as hex; otherwise treat as raw bytes.
- **Permission check:** the loader warns (does NOT refuse) if the file is mode `g+r` or `o+r`. Following the SSH/PGP convention of nagging without breaking.
- **No `--key=<hex>` flag.** Process arguments leak into `ps`; secrets must not. (For tests/dev convenience there is no exception — write a tmp file, then `chmod 600`.)

### Algorithm choice rationale

| Option | Verdict | Why |
|---|---|---|
| **HMAC-SHA256** | ✅ chosen | Stdlib (`crypto/hmac`), constant-time compare in stdlib, well-understood, symmetric-key simplicity matches the operator model (PSK distributed out of band) |
| AES-GCM (encrypt) | Deferred to ADR-0005 | This ADR is about *authentication*, not confidentiality. Layering encryption is a separate decision; both can ship together later. |
| Ed25519 (asymmetric) | Considered, rejected for v1 | Adds key-management complexity (public key on rx, private on tx, key rotation, etc.) without a use case yet — single-sender point-to-point is the norm. Revisit when multi-sender is in scope. |
| BLAKE3 keyed | Considered, rejected | Faster, but no stdlib support — would pull in a new dep. SHA-256 is ~4 GB/s on modern CPUs (S01-3 bench); the diode is not throughput-limited by hashing. |

### Wire-format compatibility

- This is a **v1 frame format change** in spirit (a flag bit is now meaningful) but does not bump `version`. Rationale: an unsigned receiver correctly rejects a SIGNED frame via the reserved-bit-set check, so the two modes interoperate safely without a version bump.
- A future change that adds NEW fields, not just NEW flag interpretations, will bump `version`.

## Consequences

### Positive
- **Closes S-1 and T-1.** An attacker with the wire but not the key cannot inject frames the receiver will accept.
- **Backward compatible.** Existing deployments without `--key-file` keep working unchanged.
- **Zero new dependencies.** `crypto/hmac` + `crypto/sha256` are stdlib.
- **Auditable.** ~50 lines of code change in `framing` + ~30 in cli. Both sides can be read end-to-end.

### Negative
- **32 extra bytes per signed frame** (~2% overhead on full-MTU). Acceptable.
- **PSK distribution is the operator's problem.** No in-band key exchange (would require a return channel — defeats the diode). Operators must distribute the key via a separate trusted channel (sneakernet, configuration management, etc.).
- **No key rotation in v1.** A single static key. Rotation requires a brief overlap window (receiver accepts two keys simultaneously) — designed but not implemented this sprint.
- **No replay protection beyond the existing recently-delivered cache.** An attacker who records a legitimate signed frame can replay it; the receiver may accept it as a duplicate of a recent message. Closed by ADR-0007 (signed `seq` high-water mark).

### Risk: key-file permissions
We warn, not refuse, on world-readable key files. This matches SSH/PGP conventions and avoids breaking automation that places keys via tooling that briefly stages them at 0644. Operators who want strict enforcement can wrap the binary.

## Test plan

- **Unit:**
  - Roundtrip with a key (encode signed → decode signed → matches).
  - Encode with key, decode without → rejected as reserved-bits-set (existing rule).
  - Encode without key, decode with key → rejected as missing-signature (NEW).
  - Encode with key A, decode with key B → rejected as ErrHMACMismatch (NEW).
  - Tampered payload in a signed frame → rejected.
  - Key loader: hex form, raw form, too-short, world-readable warning.
- **E2E:**
  - Both sides with the same key → SHA-256 of payload matches end-to-end.
  - Sender keyed, receiver unkeyed → no delivery (0 msgs delivered, all `frames_ignored`).
  - Sender unkeyed, receiver keyed → no delivery (same).
  - Sender keyed with key A, receiver keyed with key B → no delivery.

## Open questions deferred

- **Key rotation overlap** — receiver accepts two keys at once for a transition window. Sprint 03.
- **Replay protection via signed monotonic high-water mark** — ADR-0007.
- **Encryption (confidentiality)** — ADR-0005, on top of this.
