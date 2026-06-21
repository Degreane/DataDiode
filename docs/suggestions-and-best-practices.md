# Suggestions & Best Practices

A living log of recommendations made during DataDiode development. Newest date at the top. Each suggestion belongs to a dated section so it stays traceable.

---

## 2026-06-21

### Language & runtime
- **Use Go** for the core (`diode-tx`, `diode-rx`) — single static binary per OS, strong stdlib, memory-safe receiver, low audit surface. See [ADR-0001](architecture/ADR-0001-language-choice.md).
- Build with `CGO_ENABLED=0` to guarantee static binaries.
- Cross-compile via `GOOS`/`GOARCH` rather than per-OS source paths.

### Plugin model
- Default to **WASM plugins (wazero)** — sandboxed and portable.
- Fallback: **subprocess plugins** (`go-plugin` style, stdio length-prefixed protocol).
- **Reject** in-process dynamic loading (`plugin.Open` / `dlopen`) — not portable to Windows, host-crash risk.

### Transport & framing
- **UDP only** on the wire — TCP requires a return path and defeats the diode.
- Per-message **SHA-256** for integrity from day one; signing later.
- Cap payload at **1400 B** to stay under typical MTU and avoid IP fragmentation.
- Sequence numbers monotonic per sender boot; receiver tolerates gaps.
- Defer FEC to a later sprint; start with naive N× redundancy as a config flag.

### Security posture
- Two stacked enforcement layers in the demo:
  1. **Application:** `diode-rx` opens no outbound sockets, ever (auditable in code).
  2. **Network:** `nft` on the receiver drops all egress (belt-and-braces).
- For confidentiality on the wire: pre-shared keys, **not DTLS** (DTLS handshake is bidirectional).
- Keep the core <3,000 LoC so it can be read end-to-end.
- Every third-party dependency requires an ADR.

### Containers (test environment)
- Use **plain LXC** on Fedora (`lxc-create`/`lxc-start`/`lxc-attach`), not LXD (not in Fedora repos) and not Docker (NAT/bridge model muddies the diode boundary).
- Bridge `diodebr0` with no upstream, no NAT — genuinely isolated L2.
- Pin static IPs in the container config (`lxc.net.0.ipv4.address`) so demo scripts are deterministic.
- All provisioning scripts must be **idempotent** — safe to re-run; detect existing state and skip.

### Process / agile
- 2-week sprints, lightweight ceremonies, each sprint gets one markdown file under `docs/sprints/`.
- Definition of Done includes: green CI on linux/windows/macos, docs updated, no new dep without an ADR.
- ADRs (`docs/architecture/ADR-NNNN-*.md`) for any decision that future contributors might want to challenge.

### Documentation discipline
- All suggestions and best-practice recommendations go in **this file**, not just in chat.
- ADRs capture *decisions* (with alternatives and consequences); this file captures *advice* (which may or may not become a decision later).

### Integrity package design (from S01-4)
- **Named `Digest` type** instead of `[]byte` everywhere — prevents mixing up "32 random bytes" with "this is a hash" and forces callers through `Equal`/`Verify` rather than `bytes.Equal`.
- **`Hasher` / `Verifier` interfaces** behind the concrete `NewSHA256()` — lets ADR-0004 (signing/MAC) swap algorithms without touching call sites.
- **Pointer receiver on `Digest.Bytes()`** so the returned slice aliases the original; value receiver silently returned a slice over a stack copy.
- **Cross-check the wrapper against `crypto/sha256` directly** in tests — proves the wrapper doesn't introduce any transformation.
- **Golden test vectors** (empty, "abc", "The quick brown fox…") pin the algorithm; if it ever changes, the test fails loudly.

### Framing package implementation (from S01-3)
- **Sentinel errors** (one per validation rule) checked with `errors.Is` — lets tests assert *which* rule rejected a frame, not just that it was rejected.
- **Zero-allocation hot path** in `Encode`/`Decode` — caller passes a reusable `dst` slice; we hit ~4 GB/s with 0 B/op. Goal: keep it that way through the lifetime of the project.
- **Fuzz with a property, not just "no panic"** — every successful `Decode` must roundtrip exactly through `Encode`. This catches ambiguous parses where two distinct inputs map to the same header.
- **Golden wire test** pins the exact bytes for a known input. If it fails, ADR-0002 has been violated — revert or supersede the ADR, don't quietly update the test.
- **Constant-time hash comparison** even though SHA-256 is strong; cheap insurance and keeps the door open for switching to a MAC later.
- **Use `b.Loop()`** (Go 1.24+) in benchmarks instead of `for i := 0; i < b.N`; it's the modern idiom and avoids the `b.ResetTimer` dance.

### Frame format (locked by ADR-0002)
- **Magic + version up front** — the receiver can drop garbage in one branch, and a future v1 is explicitly *not* backward-compatible (silently dropped by v0 receivers, which is the right default for a security format).
- **All multi-byte ints big-endian.** Cheap insurance against a future ARM/RISC-V port discovering an endianness bug in production.
- **SHA-256 covers header + payload**, hash is appended (not interleaved), so a receiver can compute it in one pass after reading `payload_len`.
- **Explicit chunking** (`msg_id`, `chunk_index`, `chunk_total`) — never rely on IP fragmentation; a single lost IP fragment loses the whole datagram with no recourse.
- **Receiver drops bad frames silently**, increments counters. No return path means no place to send errors; logs at line rate would DoS the operator.
- **Reserved flag bits must be zero** — receivers reject any frame with them set, so the v0 spec can grow without ambiguity.
- **Cap payload at 1400 B** — leaves headroom under 1500-byte MTU for tunneling layers that might be added later (VLAN, GRE, WireGuard underlay).
