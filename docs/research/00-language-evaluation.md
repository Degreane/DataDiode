# Language Evaluation for a Portable, Pluggable Software Data Diode

> Status: **draft** — to be cross-checked against the deep-research report once it lands.

## Evaluation Criteria

A data diode is a long-running, security-sensitive network daemon with two physically separated processes (sender and receiver) that share no return channel. The language we pick must score well on:

| Criterion | Why it matters |
|---|---|
| **Cross-platform** | Linux is primary; Windows and macOS are required; ideally BSD too. Must build the same source for all targets, ideally without per-OS code. |
| **Static binaries** | Operators need to drop a single file onto an isolated host with no package manager or internet. |
| **Low dependency footprint** | Smaller TCB = smaller audit surface. Every transitive dependency is a supply-chain risk. |
| **Concurrency** | Pipeline of ingress → encode → fragment → transmit must run without blocking; receiver does reassembly + dispatch in parallel. |
| **Memory safety** | The receiver parses untrusted bytes from an attacker-controlled network. Use-after-free or buffer overflow here is catastrophic. |
| **Plugin / extension model** | Operators must add new protocol adapters without forking. |
| **Stdlib networking & crypto** | Strong batteries-included `net`, TLS, hashing, CBOR/protobuf reduce dependency risk. |
| **Developer effort** | The user explicitly asked for *"so little effort"*. |

## Candidates

### Go
- **Cross-platform:** excellent. `GOOS=windows GOARCH=amd64 go build` produces a static `.exe`. Same for `darwin`, `linux`, `freebsd`.
- **Static binaries:** default. Single file, no libc dependency (with `CGO_ENABLED=0`).
- **Deps:** strong stdlib (`net`, `crypto/*`, `encoding/*`). Most diode functionality needs zero third-party packages.
- **Concurrency:** goroutines + channels are an almost perfect fit for the framing/FEC pipeline.
- **Memory safety:** GC'd, no UAF, slice bounds checked.
- **Plugins:** `plugin` package is Linux/macOS only and brittle across versions → **avoid**. Instead use **subprocess plugins over stdio** (the HashiCorp `go-plugin` pattern) or **WASM via `wazero`** (pure-Go, cross-platform, sandboxed) — both work everywhere Go works.
- **Effort:** very low. Idiomatic Go is short, and `go test` ships with the toolchain.
- **Risks:** GC pauses (irrelevant at the data rates a software diode actually handles); plugin story requires a deliberate choice.

### Rust
- **Cross-platform:** excellent via `cargo` + cross.
- **Static binaries:** yes (musl target on Linux; native on Windows/macOS).
- **Deps:** stdlib is leaner than Go's — most projects pull in `tokio`, `serde`, `rustls`, etc. Larger crate graph = larger audit surface.
- **Concurrency:** `tokio` is excellent but is itself a non-trivial dependency.
- **Memory safety:** best-in-class — compiler-enforced, no GC. Most attractive property for a security tool.
- **Plugins:** same options as Go (subprocess, WASM via `wasmtime`/`wasmer`). Dynamic loading via `libloading` exists but has the same fragility as Go's `plugin`.
- **Effort:** higher than Go. Lifetimes, async colors, slower compiles. Real cost for a small team.
- **Risks:** team velocity, crate-graph audit burden.

### Python
- **Cross-platform:** runtime is portable; *deployment* is not (interpreter version, pip, native deps).
- **Static binaries:** not natively — `PyInstaller` / `Nuitka` work but are not pleasant for production.
- **Deps:** large ecosystem, but every pip install is supply chain.
- **Concurrency:** GIL limits CPU-bound parallelism; `asyncio` is fine for I/O.
- **Memory safety:** safe, but C-extension surface is not.
- **Plugins:** trivial (entry points, dynamic import) — Python's *best* dimension here.
- **Effort:** lowest to prototype, highest to ship as a hardened daemon.
- **Verdict:** great for the **test harness** and for **plugin authors who want a scripting option**, poor for the production data plane.

### Java / JVM (Kotlin)
- **Cross-platform:** yes, but requires a JRE on target (or GraalVM native-image, which is heavy).
- **Static binaries:** only via GraalVM, with caveats.
- **Deps:** large ecosystem, large baseline footprint.
- **Concurrency:** excellent (virtual threads in modern JDK).
- **Memory safety:** safe.
- **Plugins:** OSGi / `ServiceLoader` are mature.
- **Effort:** medium, but the JRE deployment story is a non-starter for "drop a binary on an air-gapped host."
- **Verdict:** rejected.

### Zig / C / C++
- Considered and rejected for a v1: memory-safety risk on the receiver side is not worth the marginal performance gain over Go at the throughputs a software diode actually serves.

## Working Recommendation

**Go**, with **WASM (wazero) plugins** as the primary extension mechanism and **subprocess plugins (`go-plugin`)** as a fallback for plugins that need native syscalls.

### Why Go wins for *this* project
1. **Effort vs safety trade-off is best.** Memory-safe, GC'd, no lifetime tax, builds in seconds.
2. **Single static binary on every target OS** — directly satisfies the portability requirement.
3. **Stdlib is enough** for UDP transport, hashing, TLS, CBOR-ish encoding (protobuf or msgpack via one well-known dep).
4. **Goroutines model the pipeline naturally** — one goroutine per ingress, one per chunk encoder, one for the UDP writer; channels carry frames.
5. **Auditable.** A complete data diode core in Go is realistically <3,000 lines.

### What we'd revisit Rust for
- If a formal Common Criteria evaluation becomes a goal, the absence of GC and the stronger type system are meaningful.
- If we needed to embed in a kernel module or driver. We don't.

## Plugin Architecture Sketch

Two complementary mechanisms, both cross-platform:

1. **WASM plugins (`wazero`)** — default. Sandboxed, portable, language-agnostic (authors can write in Rust, Go, AssemblyScript). Host exposes a narrow ABI: `on_message(bytes) → bytes[]`.
2. **Subprocess plugins (`go-plugin` style)** — for plugins that legitimately need OS access (e.g., a Windows Event Log reader). The host spawns the plugin, communicates over stdio with a length-prefixed protocol, and kills it on shutdown.

We explicitly **reject** in-process dynamic loading (`plugin.Open` / `dlopen`) because:
- It does not work on Windows.
- A buggy plugin crashes the host — bad for a security daemon.
- It bypasses the sandbox we want for untrusted extension code.

## Next Steps

- [ ] Cross-check this recommendation against the deep-research report.
- [ ] Write ADR-0001 capturing the language decision.
- [ ] Prototype: UDP transport + length-prefixed framing + SHA-256 integrity in Go, ~200 LoC, end-to-end on loopback.
