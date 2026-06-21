# ADR-0008: AES-256-GCM AEAD on the wire (v3 frame format)

- **Status:** Accepted
- **Date:** 2026-06-21
- **Deciders:** @degreane
- **Supersedes:** ADR-0004 (HMAC-only frame authentication)
- **Builds on:** ADR-0005 (v2 session protocol)
- **Closes:** threat-model **I-1 (plaintext on the wire)** — last HIGH residual risk

## Context

Today's keyed mode (ADR-0004) authenticates frames via HMAC-SHA256 but
leaves the payload in plaintext on the wire. An on-path observer
captures everything sent. Closing **I-1** has been the last HIGH item
in the threat model since Sprint 01.

The user asked whether the existing HMAC infrastructure could be
reused for encryption. Short answer: **the HMAC primitive cannot be
reused as an encryption key** (a textbook cryptographic anti-pattern),
but the entire surrounding investment — `--key-file`, the PSK
distribution model, the auto-detect hex/raw loader, the reject-mismatched-
key semantics — is preserved. We swap **the underlying primitive**
from HMAC-SHA256 to AES-256-GCM AEAD and get confidentiality "for
free" on top of the authentication HMAC already provides, while
shrinking the wire by 48 bytes per frame.

## Hard constraint: unkeyed mode stays a first-class path

The default operator experience — no `--key-file` on either side —
must continue to work exactly as today. v3 supports both keyed
(encrypted+authenticated) and unkeyed (plain) frames. The flag bit
that signals "this frame is encrypted" defaults to clear; receivers
that don't have a key reject any frame with it set; receivers that
do have a key reject any frame with it clear. This is the same
matrix as ADR-0004's keyed/unsigned interaction, only the primitive
behind the flag changes.

## Decision

**Replace HMAC-SHA256 with AES-256-GCM AEAD as the single keyed
primitive. v3 wire-format bump.**

### Cipher: AES-256-GCM

Why:
- **Stdlib** (`crypto/cipher.NewGCM`) — no new dependencies.
- **Authenticated Encryption**: confidentiality + integrity +
  authentication in one primitive. Provably-secure (IND-CCA2, INT-CTXT).
- **Fast on AES-NI**: 5–10 GB/s on modern x86/ARM. Faster than our
  current SHA-256 throughput.
- **Smaller wire**: 16-byte GCM tag replaces 32-byte HMAC tag AND the
  separate 32-byte SHA-256 (GCM's tag covers integrity). **Net −48 B
  per frame.**

ChaCha20-Poly1305 deferred (better on hardware without AES-NI, but
that's not a current target). Trivial future add via a `--cipher`
flag.

### Key derivation: HKDF-SHA256

The `--key-file` PSK (≥256 bits) is the master key. At session start
the sender and receiver run:

```
aead_key = HKDF-SHA256(
    salt   = "diode-aead-v3-salt",        // 18 bytes, ASCII
    ikm    = <psk-file-bytes>,
    info   = "diode-aead-v3 chunk-aead"   // 26 bytes, ASCII
).Expand(32 bytes)
```

Why HKDF instead of using the PSK directly:
- **Domain separation**: the same `--key-file` can be reused safely
  for future subkeys (e.g., a signed-manifest key in a later ADR).
- **PSK shape flexibility**: HKDF accepts any-length IKM ≥1 byte and
  outputs a fixed-length key.
- **Standard hygiene**: TLS 1.3, WireGuard, age all use HKDF-derived
  subkeys.

### Nonce: deterministic, 12 bytes

`nonce[0:8] = session_id[0:8]` + `nonce[8:12] = big-endian uint32` where the
last 4 bytes are:

| Frame type | Last 4 bytes | Rationale |
|---|---|---|
| SOH | `0xFFFFFFFF` | Reserved sentinel |
| DATA chunk_index = i | `i` | uint32, 0..2³²−2 (chunk_total caps at 2³²−1) |

**Per-key uniqueness invariant:**
- Across sessions: collisions require two random 128-bit session_ids
  to share their first 8 bytes — statistically zero in any realistic
  deployment.
- Within a session: SOH nonce is a reserved value (`0xFFFFFFFF`); DATA
  nonces are unique by `chunk_index`.
- Across redundant copies of the same chunk: nonce IS reused, but
  plaintext is identical, so ciphertext is identical — same
  information leakage as `--redundancy` already has, no new attack
  surface.

### AAD: the header

GCM's "associated data" (authenticated but not encrypted) is the
**entire frame header**: `magic | ver | flags | session_id |
chunk_index | payload_len` (for DATA) or `magic | ver | flags |
session_id | chunk_total | chunk_size | total_bytes | content_sha256 |
mode | name_len | name` (for SOH).

Why header-as-AAD:
- The receiver's `framing.PeekKind` already does a cheap
  preamble-only decode for session routing. It must remain cheap
  (no key needed). The AAD covers exactly those bytes — they are
  authenticated end-to-end without being encrypted.
- A man-in-the-middle who flips a header bit (e.g., changes
  `chunk_index` to redirect a chunk's offset) breaks the GCM tag →
  receiver rejects. No header tampering is possible.

### Wire format (v3)

**Common preamble** unchanged from v2:
```
magic(4) | ver=03 | flags(1) | session_id(16)
```

**Flag bits:**
```
bit 7 (0x80)  SOH         frame is an SOH preamble
bit 6 (0x40)  FINAL       DATA only: last chunk in the session
bit 5 (0x20)  REDUNDANT   duplicate copy for loss tolerance
bit 4 (0x10)  ENCRYPTED   payload is AEAD-encrypted; trailer is the 16-B GCM tag
bit 3..0      reserved    must be 0
```

The bit position is the same as v2's `FlagSigned` — renamed for
clarity. v2 receivers reject v3 frames at the version check; the
shared bit position is for code-locality, not interop.

**DATA frame, unkeyed (no FlagEncrypted):**
```
magic | ver=03 | flags(SOH=0) | session_id(16) | chunk_index(4) |
payload_len(2) | payload(N) | sha256(32)
```
Same as v2 unsigned. Total: `28 + N + 32` bytes.

**DATA frame, keyed (FlagEncrypted set):**
```
magic | ver=03 | flags(SOH=0, ENCRYPTED=1) | session_id(16) |
chunk_index(4) | payload_len(2) | ciphertext(N) | gcm_tag(16)
```
**No trailing SHA-256** — GCM's tag covers integrity. Total: `28 + N + 16` bytes.

**SOH frame, unkeyed**: same as v2 unsigned (header + sha256).

**SOH frame, keyed**: header + ciphertext(name) + gcm_tag. Filename is
encrypted; content_sha256 in the header is AAD-authenticated.

### Frame-size comparison

| Mode | Per-DATA overhead | At payload=1400 |
|---|---|---|
| v2 unsigned | +32 (sha256) | 1460 B |
| v2 signed (HMAC) | +32 (sha) +32 (HMAC) | 1492 B |
| v3 unsigned | +32 (sha256) | 1460 B (identical to v2) |
| **v3 encrypted** | **+16 (GCM tag)** | **1444 B (16 B less than v2 signed)** |

### Receiver policy (auth matrix, unchanged in shape)

| Frame's FlagEncrypted | Receiver has `--key-file`? | Outcome |
|---|---|---|
| 0 | No | accept (plain unsigned path, v3 unsigned == v2 unsigned in shape) |
| 0 | Yes | **reject** with `ErrEncryptedExpected` — operator opted into auth and gets it everywhere |
| 1 | No | **reject** with `ErrUnexpectedEncrypted` — unkeyed receiver cannot decrypt |
| 1 | Yes, valid HMAC | accept, payload decrypted |
| 1 | Yes, wrong key | **reject** with `ErrDecryptFailed` (GCM tag verification fails) |

Mirror exactly to ADR-0004's policy with the primitive swapped.

### Sender policy

- **No `--key-file`** → emit unsigned, unencrypted frames (FlagEncrypted=0,
  sha256 trailer). Default behavior unchanged.
- **`--key-file` set** → emit encrypted frames (FlagEncrypted=1, GCM
  trailer). Same flag, same operator UX as ADR-0004.

## Hard requirement: unkeyed mode

Restated for clarity:

```bash
diode --mode=tx --dst=10.0.0.20:9999 --send-file=foo.bin       # works
diode --mode=rx --listen=:9999 --files-to=/srv/incoming         # works
```

is **and remains** a valid, supported, regression-tested configuration.
v3 receivers accept v3 unsigned frames (no FlagEncrypted, trailing
sha256). The only break is across **wire versions** — a v2 receiver
sees v3's `version=0x03` and rejects via the existing version check.
Operators upgrade tx and rx together, same as v1→v2.

## Consequences

### Positive
- **Closes I-1.** Wire payload is ciphertext; on-path observer learns nothing.
- **Smaller wire.** 16-byte tag replaces 32-byte HMAC + 32-byte sha256.
- **Stronger authentication.** GCM's tag is unforgeable without the key;
  HMAC was unforgeable without the key but didn't encrypt.
- **Standard, audited primitive.** No bespoke crypto.
- **Same CLI.** `--key-file` semantics preserved; operators don't relearn.

### Negative
- **GCM nonce reuse is catastrophic if it ever happens.** Our nonce
  construction is collision-free by design (random 128-bit session_id
  + monotonic chunk_index), but it's worth saying out loud and
  defending in code review.
- **GCM has a key-volume limit** (~2⁵⁰ blocks ≈ 18 PB per key). At
  1 Gbps that's ~5 years. Document as a key-rotation guideline.
- **No PFS.** A leaked PSK decrypts past traffic captured on the wire.
  The diode has no return channel → no DH handshake possible → PFS
  doesn't compose with the model. Operators who care should rotate
  PSKs on a schedule (and ADR may layer on per-session ephemeral keys
  via pre-shared-key-store + index, deferred).

### Risk: silent unkeyed downgrade
A keyed sender ship a frame; a keyed receiver expects encryption. What
if an attacker on the wire flips the `FlagEncrypted` bit to 0? The
frame becomes "unsigned" wire-shape, but the receiver expects encryption
and rejects it (`ErrEncryptedExpected`). No silent downgrade.

The symmetric case: receiver expects unsigned; attacker sets
`FlagEncrypted`. Receiver has no key → `ErrUnexpectedEncrypted` →
rejected. Also no silent downgrade.

## Test plan

- Unit:
  - HKDF derivation: known-answer test vector
  - AEAD round-trip (encrypt-then-decrypt with matching key/nonce)
  - Wrong-key decrypt → `ErrDecryptFailed`
  - Tampered ciphertext byte → `ErrDecryptFailed`
  - Tampered AAD (header) byte → `ErrDecryptFailed`
  - `EncodeDATA(key=nil)` and `EncodeDATA(key=...)` round-trip via the
    matching `DecodeDATA(key=nil)` / `DecodeDATA(key=...)`
  - `Decode(key=nil)` on an encrypted frame → `ErrUnexpectedEncrypted`
  - `Decode(key=...)` on an unencrypted frame → `ErrEncryptedExpected`
- E2E:
  - **Unkeyed end-to-end** (regression for the hard requirement): both
    sides without `--key-file`, file delivered byte-identical.
  - **Keyed end-to-end**: both sides with `--key-file=<same>`, file
    delivered byte-identical; `tcpdump -X` shows random payload bytes.
  - **Mismatched**: one side keyed and other side not → 0 deliveries.
  - **Wrong-key**: both keyed with different keys → 0 deliveries,
    `data_decrypt_failed > 0` in receiver stats.

## Open questions deferred

- **ChaCha20-Poly1305 as an alternative cipher** behind `--cipher=chacha20-poly1305`.
  Trivial future add.
- **Per-session ephemeral key**: include a 16-B nonce-extension in the
  SOH that's HKDF-mixed into the AEAD subkey. Provides a form of
  forward security (a leaked PSK only decrypts past traffic if the
  attacker also captured the SOH for each session). Future ADR.
- **Replay protection** (ADR-0007): even with AEAD, an attacker can
  replay a captured ciphertext. The receiver's `completed-cache` blocks
  same-sid replays after completion; full replay protection wants
  signed monotonic high-water marks.
