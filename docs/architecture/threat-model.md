# DataDiode Threat Model (v0)

- **Status:** Living document, baseline established in Sprint 01.
- **Scope:** Sprint 01 implementation (single `diode` binary, `--mode=tx|rx`, UDP/IPv4 transport, ADR-0002 framing v0, no plugins, no encryption, no signing).
- **Methodology:** STRIDE walkthrough plus diode-specific threat enumeration.

> This is the **defender's worst-case worksheet**. Items here are
> threats we have either mitigated, accepted, or deferred — they are
> not bugs. Bugs that violate a mitigation listed here are
> security-critical.

## 1. What we are protecting

| Asset | Why it matters |
|---|---|
| **Confidentiality of data on the high side** | The point of a diode is to let the high side *receive* without exposing it. Anything that leaks high-side state back across the boundary defeats the product. |
| **Integrity of data delivered to the high side** | The high side acts on what we deliver; corruption or injection there causes downstream harm. |
| **Availability of the receiver** | If an attacker can crash, hang, or memory-exhaust `diode --mode=rx`, the diode stops being useful even if the boundary still holds. |
| **The diode invariant itself** | "No packet ever flows high → low at the application layer" must remain true under all inputs. This is the load-bearing property of the whole system. |

## 2. Trust boundaries

```
  ┌──────────────────────────┐ ───────► ┌──────────────────────────┐
  │  LOW SIDE                │  one-way │  HIGH SIDE               │
  │  (sender, "untrusted     │   UDP    │  (receiver, "trusted     │
  │   wrt high side")        │          │   wrt low side")         │
  │                          │          │                          │
  │  ┌────────────────────┐  │          │  ┌────────────────────┐  │
  │  │ diode --mode=tx    │──┼──────────┼─►│ diode --mode=rx    │  │
  │  └─────────▲──────────┘  │          │  └─────────┬──────────┘  │
  │            │             │          │            │             │
  │  ┌─────────┴──────────┐  │          │  ┌─────────▼──────────┐  │
  │  │ Ingress source     │  │          │  │ Egress sink        │  │
  │  │ (file/stdin/syslog)│  │          │  │ (file/stdout/…)    │  │
  │  └────────────────────┘  │          │  └────────────────────┘  │
  └──────────────────────────┘          └──────────────────────────┘
       │                  │                  │                  │
       │                  └── network ───────┘                  │
       │                  trust boundary                        │
       │                                                        │
       └── operator                                  operator ──┘
           trust boundary                       trust boundary
```

Three trust boundaries:

1. **Low → High network** — anything on this wire is considered untrusted by the high side. UDP, no return path.
2. **Operator → process** — the operator controls flags, inputs, and host firewall. Compromise here is out of scope (treated as god-mode).
3. **Process → host kernel** — we trust the kernel's UDP stack and `crypto/sha256`. A kernel-level compromise defeats any software diode.

## 3. Actors

| Actor | Capabilities | In scope? |
|---|---|---|
| **Legitimate operator** | Configures, starts/stops, supplies inputs. | Yes (as user, not as threat). |
| **Network attacker (on-path)** | Can read, drop, reorder, replay, or inject UDP packets on the diode wire. | **Yes** — primary threat. |
| **Compromised low-side host** | Full control of `diode --mode=tx` and its inputs. Can send arbitrary frames. | **Yes** — limited threat, the diode is supposed to survive a hostile sender. |
| **Compromised high-side host** | Full control of `diode --mode=rx` and its host. | Out of scope — at this point the high side is already lost; the diode protects the *boundary*, not what's behind it. |
| **Compromised host kernel / NIC firmware** | Can route packets anywhere. | Out of scope (no software diode survives this). |
| **Malicious plugin author** | Code running inside a future WASM/subprocess plugin. | Out of scope **for Sprint 01** (no plugins shipped). Re-evaluate in plugin ADR. |

## 4. Data flow diagram (Sprint 01)

```
  stdin/file  ──►  diode --mode=tx  ──►  UDP datagrams  ──►  diode --mode=rx  ──►  stdout/file
                  ┌──────────────────┐                        ┌──────────────────┐
                  │ chunk            │                        │ udp.Receiver     │
                  │ framing.Encode   │                        │ framing.Decode   │  ← validates
                  │ udp.Sender.Send  │                        │ reassembly       │  ← buffers
                  │   (rate limit)   │                        │   (evicts)       │
                  │   (redundancy)   │                        │ deliver(payload) │  ← writes
                  └──────────────────┘                        └──────────────────┘
```

## 5. STRIDE walkthrough

Each finding lists: **Threat** → **Vector** → **Sprint-01 mitigation** → **Residual risk** → **Deferred to**.

### 5.1 Spoofing

**S-1. Attacker on the wire forges UDP datagrams pretending to be the legitimate sender.**
- *Vector:* UDP has no per-packet identity; source address is trivially spoofable.
- *Mitigation:* SHA-256 per frame (ADR-0002) detects unrelated injection — but an attacker who knows the frame layout can produce valid-looking frames trivially, since SHA-256 alone is not authentication.
- *Residual risk:* **HIGH** — any attacker who can reach the receiver's UDP port can inject frames that pass validation. This is the single biggest gap in v0.
- *Deferred to:* **ADR-0004 (HMAC / Ed25519 signing).** Until then, operators MUST rely on network-layer controls (firewall: only the legitimate sender's IP can reach the receiver's port; ideally a point-to-point link with no other addressable hosts).

**S-2. Attacker spoofs the receiver to the sender.**
- *Vector:* N/A by construction — sender never reads from the network, so no spoofed "ACK" can deceive it.
- *Residual risk:* None.

### 5.2 Tampering

**T-1. On-path attacker modifies a frame in flight.**
- *Vector:* Bit-flip in payload, change `chunk_index`, etc.
- *Mitigation:* SHA-256 covers header + payload (ADR-0002 §"Hash scope"). Receiver computes and compares; mismatch → silent drop. Cross-checked against `crypto/sha256` in tests; golden vectors pin the algorithm.
- *Residual risk:* SHA-256 alone is not a MAC; a sophisticated attacker who can both forge frames AND knows the layout can produce frames with valid hashes. Same gap as S-1 — closed by ADR-0004.

**T-2. Attacker reorders frames.**
- *Vector:* UDP loss/reorder is normal; an attacker can amplify it.
- *Mitigation:* Reassembler buffers per `msg_id` and assembles in `chunk_index` order. Order of arrival is irrelevant.
- *Residual risk:* None for ordering. (Loss → deferred to FEC, see D-3.)

**T-3. Attacker truncates a UDP datagram.**
- *Vector:* On-path attacker delivers a shortened datagram.
- *Mitigation:* `framing.Decode` enforces `HeaderLen + payload_len + HashLen == len(src)`; mismatch → `ErrLenMismatch` → silent drop.
- *Residual risk:* None.

### 5.3 Repudiation

**R-1. Sender denies having sent a payload.**
- *Mitigation:* Out of scope for v0. We are not building a non-repudiation system; we are building a unidirectional transport. Logs at the sender and receiver are advisory.
- *Deferred to:* Future ADR if a customer ever asks for signed audit trails. Likely Ed25519 over `(seq, msg_id, payload_hash)`.

### 5.4 Information disclosure

**I-1. Confidentiality of payload on the wire.**
- *Vector:* UDP is plaintext.
- *Mitigation:* **None in v0.** Operators MUST treat the wire as visible (e.g., isolated point-to-point fiber, or pre-encrypt the payload at the application layer before piping it into `diode --mode=tx`).
- *Residual risk:* **HIGH** for any deployment where the wire is not physically protected.
- *Deferred to:* **ADR-0005 (pre-shared key + AES-256-GCM, nonce derived from seq).** DTLS is rejected because its handshake is bidirectional — defeats the diode.

**I-2. Receiver leaks state back to the sender.**
- *Vector:* In a software diode, any code path that opens an outbound socket on the receiver host violates the invariant.
- *Mitigation:*
  - Application-layer: `udp.Receiver` exposes **no exported method that writes to the network** (ADR-0001 / `internal/transport/udp/udp.go`). Locked in by `TestReceiver_NoWriteMethods` — a reflection-based invariant test that fails if any future contributor adds a `Send`/`Write*` method.
  - Network-layer: `scripts/lxc-harden.sh` installs an nftables rule on the receiver container that drops all egress except loopback.
- *Residual risk:* The single `diode` binary contains the sender code path. A compromised high-side host could `exec /usr/local/bin/diode --mode=tx --dst=<attacker>`. Mitigated by the nftables DROP-all-egress rule and by the threat-model assumption that a compromised receiver is already out of scope. Not justified to split into two binaries (attacker with code execution can write raw UDP in syscalls anyway).
- *Deferred to:* Optional `--no-tx` build tag in a future sprint for operators who want the sender code physically absent.

**I-3. Receiver leaks state via timing or rate side channels.**
- *Vector:* If the receiver's *processing time* or *visible rate* depends on what it received, an on-path observer can infer high-side state. The classic covert-channel concern for diodes.
- *Mitigation:* **None in v0.** The receiver does not transmit, so the only observable is whether it crashes or stops receiving. Resource limits on the reassembler (MaxPending / MaxBytes) cap the amount of state an attacker can force the receiver to hold, indirectly bounding observable behavior.
- *Residual risk:* LOW for v0 (receiver has no visible output back); rises if a future feature adds any operator-visible status surface that varies with content.

**I-4. Operator misconfiguration leaks data.**
- *Vector:* `--listen=0.0.0.0:9999` exposes the receiver to every network it has an interface on. `--out=/path` could overwrite something sensitive.
- *Mitigation:* Documented in `scripts/demo.sh` and the LXC nftables harden script — receiver binds to its single isolated interface in the demo. `--out` files are opened with `O_APPEND` (no truncation).
- *Residual risk:* Standard operator risk. Documentation mitigation only.

### 5.5 Denial of service

**D-1. Memory exhaustion via unbounded reassembly.**
- *Vector:* Send N partials with distinct `msg_id`, never complete any. Without limits, the reassembler buffers forever.
- *Mitigation:* `reassembly.Options.MaxPending` (count cap) and `MaxBytes` (aggregate byte cap). FIFO eviction of oldest partial when either is exceeded. Defaults: 1024 messages, 64 MiB. Unit tests `TestEvictionOnPendingCap` and `TestEvictionOnByteCap` lock in the behavior.
- *Residual risk:* An attacker can force eviction of legitimate in-flight messages by flooding partials. Acceptable: the alternative (no cap) is worse.

**D-2. CPU exhaustion via malformed-frame flood.**
- *Vector:* Send a million tiny garbage datagrams; receiver spends cycles in `framing.Decode`.
- *Mitigation:* Decoder is allocation-free (~4 GB/s, 0 B/op per S01-3 benchmarks). Bad-frame counters (`stats.FramesIgnored`) but **no per-frame log line** — logging at line rate would DoS the operator's console.
- *Residual risk:* Bounded by the receiver host's UDP receive buffer and CPU; not a software-fixable issue.

**D-3. Packet loss → message never completes.**
- *Vector:* One lost UDP datagram in a multi-chunk message means the message never delivers. Attacker can target specific chunks.
- *Mitigation:* Sender supports `--redundancy=N` (each frame sent N times, REDUNDANT-flagged copies deduped by receiver). Single-chunk messages survive almost any loss; large messages benefit linearly in N.
- *Residual risk:* Linear-only loss tolerance. FEC (Reed-Solomon parity) would give super-linear tolerance for the same overhead.
- *Deferred to:* **ADR-0006 (FEC).**

**D-4. Output-side back-pressure stalls the receiver.**
- *Vector:* `--out=<file>` on a slow disk, or stdout into a slow consumer, blocks the receiver. While blocked, the UDP receive buffer fills, then the kernel drops new datagrams silently.
- *Mitigation:* Stats line on shutdown reports `frames_in` vs `msgs_delivered` so operators see drops after the fact.
- *Residual risk:* No real-time alarm. Acceptable for v0.
- *Deferred to:* Optional `--metrics-addr=host:port` HTTP endpoint exposing the same counters live (Prometheus-compatible). On the **low side only** — must not be enabled on the receiver, see I-2.

**D-5. Sender flooded by operator inputs.**
- *Vector:* `cat /dev/urandom | diode --mode=tx` ships forever.
- *Mitigation:* `--max-message` hard cap (default 64 MiB) on a single tx invocation. `--rate` token-bucket throttle on bytes/sec.
- *Residual risk:* Operator can override caps. Standard.

### 5.6 Elevation of privilege

**E-1. Memory-safety bug in `framing.Decode` lets an attacker control the receiver process.**
- *Vector:* Receiver parses untrusted bytes from the wire. A buffer overflow or use-after-free here is catastrophic.
- *Mitigation:*
  - Implementation language is Go (GC, bounds-checked slices). Single largest reason ADR-0001 picked Go over C/C++/Zig.
  - Decoder is fuzzed (`FuzzDecode`, 30 s in CI, 10 s + 26M execs locally on every change). Property is *roundtrip*, not just "no panic" — catches ambiguous parses.
  - Zero-allocation hot path verified by benchmark (`0 B/op` in S01-3) — no hidden allocator paths to exercise.
- *Residual risk:* LOW. The Go runtime itself is in our TCB, but treating Go as memory-safe is industry standard.

**E-2. Compromised plugin escapes its sandbox.**
- *Vector:* N/A in Sprint 01 (no plugins). Future risk surface.
- *Deferred to:* Plugin ADR. Default plan: WASM via `wazero` (sandboxed, no syscalls). Subprocess plugins run in their own process; if compromised, they're confined to whatever OS user we run them as.

**E-3. Operator misuses sudo on a setup script.**
- *Vector:* `scripts/lxc-setup.sh` runs as root; a malicious patch could do anything.
- *Mitigation:* Scripts are short, in-repo, and reviewed in commit history. `check-prereqs.sh` runs as a non-root user.
- *Residual risk:* Same as any rooted shell script. Operator review is the control.

## 6. Diode-specific threats not cleanly STRIDE

### 6.1 Covert channel via packet rate / timing

The receiver does not transmit, so a high-side observer can't be signalled directly by the receiver. But a compromised high-side process that has *some* legitimate egress path (e.g., another network) could modulate its CPU load by the *content* of received frames, and a separate observer could read that out-of-band. This is fundamentally out of scope for the diode (which protects the boundary, not the high-side host).

### 6.2 IP fragmentation amplification

If a sender's `--chunk` is set higher than the path MTU, IP fragmentation kicks in. A single lost fragment loses the whole datagram silently. **Mitigation:** ADR-0002 caps `MaxPayloadLen` at 1400 B so total frame ≤ 1458 B, comfortably under a 1500 B Ethernet MTU. Operators who set `--chunk` larger know what they're doing.

### 6.3 Replay attack

An attacker captures a legitimate datagram and replays it later.
- *Today:* The receiver's recently-delivered MsgID cache (default 1024) catches near-term replays. Replays older than 1024 distinct messages re-deliver as if fresh.
- *Mitigation seam:* `seq` and `msg_id` are monotonic per sender boot. A future signed/MAC'd frame would include them in the signed scope, making out-of-order or replayed frames detectable.
- *Deferred to:* ADR-0004 (signing) — once frames are MAC'd, the receiver can keep a high-water-mark `seq` and reject anything older.

### 6.4 Endianness / parser ambiguity

ADR-0002 pins big-endian and fixed offsets. `FuzzDecode` enforces a roundtrip property: every successfully-decoded buffer must re-encode to identical bytes. That kills off ambiguous parses where two distinct inputs map to the same header.

## 7. Residual risk summary (where v0 is genuinely weak)

| ID | Risk | Severity | Fix landing |
|---|---|---|---|
| ~~**S-1 / T-1**~~ | ~~Unauthenticated frames; on-path attacker can inject anything~~ | **CLOSED** | ADR-0004 shipped via `--key-file`. Operators MUST configure on both sides to benefit. |
| **I-1** | Plaintext on the wire | **HIGH** for non-physically-isolated wires | ADR-0005 (AES-256-GCM with PSK) |
| **D-3** | One-lost-packet kills a multi-chunk message | MEDIUM | ADR-0006 (FEC) |
| **D-4** | Slow consumer causes silent drops in kernel UDP buffer | LOW–MEDIUM | Optional metrics endpoint |
| **I-2 corollary** | Sender code present in receiver binary | LOW (already mitigated by nft) | Optional `--no-tx` build tag |

**Operators MUST compensate for the HIGH items in v0 with network-layer controls** (point-to-point fiber or VLAN, strict host firewall pinning the sender's IP) until the ADRs above land.

## 8. Mitigation index → code

| Mitigation | Implementation |
|---|---|
| Frame magic + version | `internal/framing/frame.go` const block, `Decode` enforcement |
| Per-frame SHA-256 | `internal/framing/frame.go` `Encode`/`Decode` |
| Constant-time hash compare | `internal/framing/frame.go` `bytesEqualConstantTime`, `internal/integrity` `Equal` |
| Receiver exposes no Write methods | `internal/transport/udp/udp.go` `Receiver` type, `udp_test.go` `TestReceiver_NoWriteMethods` |
| Peer-address discarded on receive | `internal/transport/udp/udp.go` `Recv`/`Run` (drop `*net.UDPAddr` from `ReadFromUDP`) |
| Reassembly memory caps | `internal/reassembly/reassembly.go` `Options.MaxPending` / `MaxBytes`, `evictIfOverBudget` |
| Recently-delivered dedup | `internal/reassembly/reassembly.go` `recent` cache |
| Decoder fuzz | `internal/framing/fuzz_test.go` `FuzzDecode`, CI 30 s |
| Cross-OS compile sanity | `.github/workflows/ci.yml` `build-matrix` + `go vet` per target |
| nft DROP-all-egress on receiver | `scripts/lxc-harden.sh` |
| Reject `chunk` > MaxPayloadLen | `cmd/diode/tx.go` `parseTxFlags` |
| `--max-message` cap | `cmd/diode/tx.go` `txConfig.maxMessage`, `txLoop.run` |
| Output append-only | `cmd/diode/rx.go` `openOutput` (`O_APPEND`) |

## 9. Open questions / re-evaluate next sprint

- Should the receiver expose a `--peer-allowlist=<CIDR>` so it can reject frames from unexpected sources at userspace (a defense-in-depth on top of host firewall)? Cheap to add; reduces blast radius if firewall is misconfigured.
- Is there value in shipping a separate `diode-rx` binary that physically excludes the sender code? Lift to a build tag rather than two binaries; revisit when a customer asks.
- What is the policy for the recently-delivered cache size in long-running receivers handling >1024 distinct `msg_id` per second? Probably need a time-based prune in addition to the FIFO cap.

## 10. Change log

- **2026-06-21 (v0):** Initial draft, Sprint 01 scope.
