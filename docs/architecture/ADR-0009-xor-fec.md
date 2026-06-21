# ADR-0009: XOR-based per-group Forward Error Correction (v4 wire)

- **Status:** Accepted
- **Date:** 2026-06-21
- **Deciders:** @degreane
- **Builds on:** [ADR-0005](ADR-0005-session-protocol.md) (session protocol),
  [ADR-0008](ADR-0008-aead-encryption.md) (v3 AEAD)
- **Closes (partially):** threat-model **D-3** — a single lost UDP datagram in
  a multi-chunk session is no longer enough to kill the transfer when
  FEC is configured

## Context

Today's only loss-tolerance knob is `--redundancy=N` on the sender,
which ships every chunk N times. With `--redundancy-order=spread`
(ADR-0006 default) this survives burst loss reasonably well, but it's
**N× bandwidth** for **(N-1)-of-N** tolerance: 100% overhead bought 1
spare copy.

Forward Error Correction (FEC) computes **parity** chunks from data
chunks so that any K-of-N can reconstruct the original K. For an
(K=10, M=1) XOR layout that's 10% overhead to tolerate **any** 1
missing chunk per group — far cheaper than `--redundancy=2` (100%)
which has the same guarantee.

## Why XOR (not Reed-Solomon)

Two reasonable choices for "no external dep":

1. **XOR FEC** — RAID-5-style. One parity chunk per group; tolerates
   exactly 1 chunk loss per group. ~50 lines of Go, no library.
   Sufficient when expected loss rate is ≲ 1/group_size.

2. **Reed-Solomon (k, n)** — tolerates up to (n-k) losses per group
   for any k, n. Requires Galois field arithmetic; implementing
   in-tree is ~600 lines + careful testing, or pulling in
   `klauspost/reedsolomon` (a non-stdlib dependency).

This ADR ships **XOR** as the v1 of FEC. It closes the common case
(rare single-packet loss) at minimal complexity and zero new
dependencies. **A future ADR-0010 will add Reed-Solomon** if
operators report sustained multi-loss-per-group scenarios.

The XOR primitive is symmetric (encode == decode), trivially
correct, and SIMD-friendly via Go's compiler.

## Decision

Add FEC as an **opt-in per-session** feature. Sender groups DATA
chunks into runs of `--fec-group-size=K` (default 0 = off). For each
group of K consecutive data chunks, the sender emits **one parity
chunk** = XOR of all K data chunks (zero-padded to chunk_size). On
receive, if exactly one data chunk in a group is missing AND its
parity arrived, the receiver reconstructs the missing chunk by
XOR-ing the other K-1 data chunks with the parity.

Hard requirements (unchanged):
- **Unkeyed mode** still works without FEC; FEC is opt-in.
- **Keyed mode** still works with FEC; the parity chunk is itself
  AEAD-sealed using the standard DATA-frame path (its `chunk_index`
  is in the parity range, see below).

### Wire format v4 (bump from v3)

Same `magic | ver | flags | session_id` preamble. Version bump:

```
Version uint8 = 0x04
```

**Flags byte (one new bit; rest unchanged):**

```
bit 7 (0x80)  SOH         (unchanged)
bit 6 (0x40)  FINAL       (unchanged)
bit 5 (0x20)  REDUNDANT   (unchanged)
bit 4 (0x10)  ENCRYPTED   (unchanged)
bit 3 (0x08)  PARITY      NEW: this DATA frame is a FEC parity chunk
bit 2..0      reserved    must be 0
```

`flagsReserved` mask narrows from `0x0F` to `0x07`.

**SOH gains a new field:**

```
offset  size   field           notes
 …      …      (existing v3 SOH fields up to and including chunk_size at offset 30)
30..38  8      total_bytes     (existing)
38..70 32      content_sha256  (existing)
70..74  4      mode            (existing)
74      2      name_len        (existing)
76      4      fec_group_size  NEW: uint32 BE; 0 = no FEC (v3-compatible-ish)
80      N      name            (was at offset 76 in v3)
```

SOHHeaderLen grows from 76 to 80. All offsets in name + trailing
sha/AEAD shift by 4.

**DATA chunk_index space when FEC is enabled:**

- `0 .. (chunk_total - 1)` — data chunks (same as v3)
- `chunk_total .. (chunk_total + parity_total - 1)` — parity chunks,
  flagged PARITY.

Where `parity_total = ceil(chunk_total / fec_group_size)` (one
parity per group). The receiver computes this from `chunk_total`
and `fec_group_size` in the SOH — not transmitted separately.

The DATA frame header doesn't change — `chunk_index` is still 4
bytes BE. With v4 + FEC enabled, indices in the parity range carry
PARITY=1 in their flags.

### Decoder algorithm

Per group `g` (0-indexed):
- data indices: `g*K .. min((g+1)*K, chunk_total) - 1`
- parity index: `chunk_total + g`

When the last frame in a group arrives, the receiver checks:
- If all K data chunks are present → no FEC work needed.
- If exactly one data chunk is missing AND the parity is present →
  reconstruct it: `missing = parity XOR (XOR of other K-1 data chunks)`.
- Otherwise → can't recover by XOR; chunk stays missing.

The receiver writes reconstructed chunks via the same `pwrite`-to-
offset path used for arriving data chunks; the bitmap is updated.
Session completes when all data bits are set (parity bits don't
need to be set for completion — they're advisory).

### Padding

The last group may have fewer than K data chunks. The XOR still
works as long as the encoder zero-pads the missing slots. Equivalently,
the parity is XOR of the partial group's actual chunks.

The last chunk in the whole file may be smaller than chunk_size.
For XOR, we pad with zeros up to chunk_size before XORing. Decoder
truncates the recovered chunk to its declared `payload_len` (which
we know for data chunks because their `payload_len` field is sent —
but for a RECONSTRUCTED chunk, we don't know `payload_len` directly).

**Solution:** the SOH carries `total_bytes`. For chunk `i` < chunk_total-1,
`payload_len = chunk_size`. For the last data chunk:
`payload_len = total_bytes - (chunk_total-1) * chunk_size`. So we can
always derive the correct `payload_len` from the SOH metadata.

## CLI surface

| Flag | Default | Where | Purpose |
|---|---|---|---|
| `--fec-group-size=<K>` | 0 (off) | tx | When > 0, group every K data chunks and ship one XOR parity per group. K=1 is silly (parity == data); K must be ≥ 2 if > 0. |
| `--fec=auto\|on\|off` | `auto` | rx | Receiver enables FEC decode logic when SOH advertises `fec_group_size > 0`. `auto` (default) honors SOH. `off` ignores parity chunks (treats them as data_dropped). `on` requires every SOH to set fec_group_size > 0; SOHs without it are rejected. |

Sender example:
```bash
diode --mode=tx --dst=… --send-file=foo.iso --fec-group-size=10
# ships data + 10% parity overhead; receiver auto-reconstructs any
# single-chunk loss per group of 10
```

## Receiver state additions

`internal/session.Session` grows:
- `fecGroupSize uint32` — copied from SOH
- `parityRecv map[uint32][]byte` — parity chunks keyed by group index (only when FEC enabled)

On every data chunk arrival, after writing + setting bit, the
receiver checks: for the chunk's group, do we now have (K-1)
data + 1 parity? If so AND there's exactly one data chunk still
missing → reconstruct + write + set bit.

On every parity chunk arrival, same check (we might be the last
piece needed to reconstruct a missing data chunk).

## Consequences

### Positive
- **Closes D-3 for single-chunk-per-group loss** — common WAN scenario.
- **Tiny code surface** — XOR + group accounting, ~150 lines total.
- **No new dependencies.**
- **Backward compatible at the CLI**: `--fec-group-size=0` (default)
  is identical to v3 behavior.
- **Parity chunks ride the same encryption path** — keyed-FEC sessions
  encrypt parities too. No special crypto handling needed.

### Negative
- **Wire-format v4 break.** v3 receivers reject v4 frames via the
  version check. Operators upgrade in lockstep.
- **Tolerates only 1 loss per group.** Two losses in the same group
  → that group is unrecoverable. Mitigate with smaller K (more
  groups, less data per group) or layer with `--redundancy=N` on
  the sender.
- **Parity computation cost.** O(K * chunk_size) per group at the
  sender. For K=10 chunk_size=1400 = 14 KB XOR per group → negligible.
- **Sender needs the file in memory** (we already do this in v3 for
  SHA precompute and chunking; FEC doesn't change that).
- **Receiver needs to hold parity chunks in RAM** (one per group)
  until the group is complete. For 1 GB file at K=10 chunk_size=1400
  → 715K data chunks / 10 = 71500 parities × 1400 B = ~100 MB peak.
  Tunable via `--fec-group-size`.

### Risk: padding-induced ambiguity
The last group might have fewer than K data chunks. The encoder
zero-pads. If the decoder doesn't know the precise group sizes, it
could reconstruct a wrong byte (zeros instead of real data) for the
last chunk. **Mitigation:** the encoder always zero-pads to a full
group, but the decoder knows from SOH metadata exactly which data
indices exist (`chunk_total`) and derives the last group's actual
size as `chunk_total - (K * (parity_total - 1))`.

## Test plan

- Unit (internal/fec):
  - XOR encode + decode roundtrip with no loss
  - reconstruct 1 missing chunk per group
  - 0 missing chunks → no reconstruction, no error
  - 2 missing chunks per group → ErrFECNotRecoverable, group stays incomplete
  - last group with k < K data chunks (padding correctness)
- Unit (internal/session):
  - Session opens with fec_group_size > 0, parity chunks routed correctly
  - Reconstruction triggers correctly on the "(K-1)th data + parity" arrival
- E2E:
  - Send with --fec-group-size=10, drop a known data chunk in-flight,
    receiver reconstructs and delivers the byte-identical file
  - Send with --fec-group-size=0, behavior matches v3
  - Wrong --fec=on with no SOH FEC → rejected

## Open questions deferred

- **Reed-Solomon RS(k, n) in ADR-0010** for multi-loss-per-group
  tolerance. Either implement in-tree (~600 LoC of Galois field
  arithmetic) or accept the klauspost/reedsolomon dep.
- **Adaptive FEC** (sender adjusts K based on observed loss rate) —
  no return channel makes this hard; operator-driven for now.
