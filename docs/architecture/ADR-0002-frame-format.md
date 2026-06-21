# ADR-0002: On-Wire Frame Format (v0)

- **Status:** Accepted
- **Date:** 2026-06-21
- **Deciders:** @degreane
- **Supersedes:** —
- **Superseded by:** —
- **Related:** [ADR-0001](ADR-0001-language-choice.md) (Go + plugin model), Sprint 01 plan

## Context

DataDiode moves bytes over a one-way UDP transport. UDP gives us packet boundaries and no return channel; everything else (integrity, ordering, message reassembly, version negotiation, replay resistance) must live in our own frame format. Because the receiver has no way to ask the sender to repeat itself, the frame must be:

1. **Self-describing enough** for the receiver to recognise garbage immediately (magic, version).
2. **Self-validating** (per-frame hash) — UDP's checksum is too weak and is optional in some stacks.
3. **MTU-safe** so it never relies on IP fragmentation (fragmentation + loss = total loss of the message).
4. **Cheap to parse** — receiver must keep up with line-rate input on a single core if needed.
5. **Stable** — we can't ship "v0 with a tweak" to deployed receivers, because they can't tell us they need an upgrade. Version field is therefore non-negotiable.

The frame format is the project's single most important contract. It must be locked before the sender, receiver, and any plugin author write a line of code against it.

## Decision

We adopt the v0 frame layout below for Sprint 01 and freeze it for the duration of the sprint. Changes after the sprint require ADR-0002a (or supersession).

### Byte layout

All multi-byte integers are **big-endian** (network byte order).

```
Offset  Size  Field           Notes
------  ----  --------------  --------------------------------------------------
 0       4    magic           Constant 0x44 0x44 0x4F 0x00  ("DDO\0")
 4       1    version         0x01 for v0  (we waste 0x00 to make all-zero garbage detectable)
 5       1    flags           Bitfield, see below
 6       8    seq             uint64 BE, monotonic per sender boot
14       4    msg_id          uint32 BE, identifier of the message this chunk belongs to
18       2    chunk_index     uint16 BE, 0-based index of this chunk within msg_id
20       2    chunk_total     uint16 BE, total number of chunks for msg_id (≥1)
22       4    payload_len     uint32 BE, length of payload in bytes (≤ 1400)
26       N    payload         opaque bytes, length = payload_len
26+N    32    sha256          SHA-256 of bytes [0 .. 26+N), i.e. header + payload
```

Total wire size = `26 + payload_len + 32` bytes. With `payload_len = 1400` that's **1458 bytes** — safely under a 1500-byte Ethernet MTU even after a few bytes of header overhead (LXC veth = 1500; we leave slack for any tunneling layer that may be added later).

### Flags byte

```
bit 7 (0x80)  FINAL       This chunk is the last chunk of msg_id (chunk_index == chunk_total-1)
bit 6 (0x40)  HEARTBEAT   Empty payload; receiver uses to detect liveness without delivering
bit 5 (0x20)  REDUNDANT   This frame is a duplicate sent for redundancy; receiver dedupes
bit 4..0      reserved    Must be 0; receiver rejects frames with reserved bits set
```

### Constants

| Name | Value |
|---|---|
| `MagicBytes` | `[4]byte{'D','D','O',0}` |
| `Version` | `0x01` |
| `HeaderLen` | `26` |
| `HashLen` | `32` |
| `MaxPayloadLen` | `1400` |
| `MaxFrameLen` | `26 + 1400 + 32 = 1458` |
| `MinFrameLen` | `26 + 0 + 32 = 58` (heartbeats are valid with empty payload) |

### Sequencing & chunking semantics

- **`seq`** — per-frame sequence number, monotonically increasing from 0 at sender start. Wraps at `uint64` (effectively never). Receiver uses gaps in `seq` to count losses, but otherwise treats `seq` as advisory — it does **not** require strict ordering.
- **`msg_id`** — per-message identifier, monotonically increasing from 0 at sender start. All chunks of the same message share `msg_id`. After `uint32` wrap (≈4 B messages) the sender re-uses values; receiver must tolerate this (a message older than the chunk-reassembly window is forgotten).
- **`chunk_index` / `chunk_total`** — explicit fragment metadata. The receiver buffers chunks until it has all `chunk_total` of them (or a timeout expires), then delivers the reassembled message. The FINAL flag is redundant with `chunk_index == chunk_total-1` but makes single-chunk messages a single byte to identify.
- **No retransmission, no NACK, no ACK** — by design. Loss is permanent unless the sender was configured with redundancy (in which case `REDUNDANT` frames carry duplicates of earlier `seq` values).

### Hash scope

The SHA-256 covers bytes `[0 .. HeaderLen + payload_len)`, i.e. **everything except the hash itself**. The hash is appended, not interleaved, so the receiver can compute the hash in a single pass once it knows `payload_len`.

We use SHA-256, not a CRC or BLAKE2/BLAKE3, because:
- It is in the Go stdlib (`crypto/sha256`) — zero new dependencies.
- It is fast enough at our throughputs (multi-GB/s on modern CPUs with SHA-NI).
- It gives cryptographic-grade collision resistance; useful if we later add signing (the hash becomes the message to sign).
- CRC32 is too weak against intentional tampering, and a software diode's threat model includes a hostile sender or wire.

### Endianness

Big-endian, always. The format may later cross architectures (ARM gateways, RISC-V industrial nodes); pinning byte order at the spec layer is cheaper than discovering an endianness bug in production.

## Worked example

A 3000-byte message split into three chunks (1400 + 1400 + 200), `msg_id = 7`, `seq` starting at 42:

| seq | msg_id | chunk_index | chunk_total | flags | payload_len |
|----:|-------:|------------:|------------:|:------|------------:|
| 42 | 7 | 0 | 3 | `0x00` | 1400 |
| 43 | 7 | 1 | 3 | `0x00` | 1400 |
| 44 | 7 | 2 | 3 | `0x80` FINAL | 200 |

With `--redundancy=2`, each of the three frames is sent twice; the second copy carries `flags |= REDUNDANT (0x20)` and the same `seq`/`msg_id`/`chunk_index` as the original. The receiver dedupes on `(msg_id, chunk_index)`.

## Validation rules (receiver)

A frame is **rejected silently** (logged at debug, dropped) if any of the following holds:

1. Length < `MinFrameLen` or > `MaxFrameLen`.
2. `magic` ≠ `"DDO\0"`.
3. `version` ≠ `0x01`.
4. Reserved flag bits are set.
5. `payload_len` > `MaxPayloadLen`.
6. `HeaderLen + payload_len + HashLen` ≠ total UDP datagram length.
7. `chunk_total` == 0.
8. `chunk_index` ≥ `chunk_total`.
9. Computed SHA-256 ≠ trailing 32 bytes.

"Silently" because a diode receiver has no return path to report errors and we don't want a single bad packet to spam stderr at line rate. Counters are incremented and exposed via metrics.

## Alternatives Considered

### Protobuf
- **Pro:** schema evolution is a solved problem; tooling everywhere.
- **Con:** new dependency (`google.golang.org/protobuf` + generator), larger wire format for our tiny header (varints add overhead at this scale), and we'd still need our own framing on UDP since protobuf isn't self-delimiting.
- **Verdict:** rejected for v0. Reconsider when we add complex application-layer messages (then protobuf belongs inside `payload`, not around it).

### CBOR for the whole frame
- **Pro:** self-describing, compact, stdlib-ish (one well-known module).
- **Con:** parser allocates; we want a zero-allocation hot path. Header field offsets become non-constant, costing branch predictability.
- **Verdict:** rejected for the frame envelope. Plugins are free to use CBOR *inside* the payload.

### COBS / SLIP-style byte stuffing
- **Pro:** classic for serial transport.
- **Con:** UDP already gives us packet boundaries; stuffing is wasted work and bandwidth.
- **Verdict:** rejected. Revisit if/when we add a serial transport (the diode-over-serial use case is real).

### Bigger header with explicit timestamps
- **Pro:** lets the receiver detect clock skew, replays, ageing.
- **Con:** clocks on air-gapped hosts drift; adding a timestamp invites operators to *rely* on it for ordering, which UDP doesn't guarantee.
- **Verdict:** rejected for v0. If we add replay protection later it'll be via signed nonces in `payload`, not a header timestamp.

### Smaller `chunk_index`/`chunk_total` (uint8)
- **Pro:** 2 bytes saved.
- **Con:** caps messages at 256 × 1400 ≈ 358 KB. Too small for log batches and file transfers.
- **Verdict:** rejected. uint16 gives 65535 × 1400 ≈ 87 MB per message — large enough for v0; larger files become multiple messages at the application layer.

## Consequences

### Positive
- Zero new dependencies for framing — stdlib only.
- Header is fixed-offset → easy to parse with `binary.BigEndian.Uint*` and easy to hand-write test vectors.
- The diode discipline is encoded *in the format*: no field can carry a return-channel signal.
- Format is small enough to spec on one page; that becomes the interop contract for any future re-implementation in a different language.

### Negative
- `chunk_total` must be known when the first chunk is sent. The sender therefore needs the full message in memory (or a known size) before it starts transmitting. For streaming use cases (TCP-stream plugin) we'll either buffer to a configurable cap or define a separate "stream" flag in a later ADR.
- 26 + 32 = 58 bytes of overhead per frame is ~4% on full-MTU payloads, ~50%+ on small payloads. Acceptable for v0; small messages can be batched at the application layer by the syslog/file plugins.
- Big-endian on x86 costs a few cycles per `binary.BigEndian.Uint*`. Negligible.

### Compatibility
- v0 is intentionally not forward-compatible. A v1 will increment `version` and receivers will silently drop v1 frames until upgraded. This is the right default for a security-sensitive format: refuse what you don't understand.

## Implementation Plan

This ADR is the contract S01-3 (`internal/framing`) implements. Specifically:

```go
package framing

const (
    Version       uint8  = 0x01
    HeaderLen            = 26
    HashLen              = 32
    MaxPayloadLen        = 1400
    MaxFrameLen          = HeaderLen + MaxPayloadLen + HashLen
    MinFrameLen          = HeaderLen + 0 + HashLen
)

var MagicBytes = [4]byte{'D', 'D', 'O', 0}

const (
    FlagFinal      uint8 = 0x80
    FlagHeartbeat  uint8 = 0x40
    FlagRedundant  uint8 = 0x20
    flagsReserved  uint8 = 0x1F
)

type Header struct {
    Version     uint8
    Flags       uint8
    Seq         uint64
    MsgID       uint32
    ChunkIndex  uint16
    ChunkTotal  uint16
    PayloadLen  uint32
}

// Encode appends a complete frame (header + payload + hash) to dst and returns the new slice.
func Encode(dst []byte, h Header, payload []byte) ([]byte, error)

// Decode parses one frame from src. It verifies magic, version, flags,
// length consistency, and the trailing SHA-256. Returns the header and a
// sub-slice of src holding the payload (no copy).
func Decode(src []byte) (Header, []byte, error)
```

A fuzz target on `Decode` is part of S01-3's DoD.

## Open Questions (deferred)

- **Signing.** Where does the signature live? Likely a second hash field added in v1, covering `(seq, msg_id, payload_hash)` with an Ed25519 key. ADR-0004.
- **Encryption.** Pre-shared symmetric key with AES-256-GCM; nonce derived from `seq`. ADR-0005.
- **FEC.** Reed-Solomon parity frames marked with a new flag bit. ADR-0006.
- **Streaming mode.** A `STREAM` flag where `chunk_total = 0` means "more coming, total unknown"; receiver flushes on FINAL or timeout. Deferred until we have a TCP-stream plugin to drive the requirement.
