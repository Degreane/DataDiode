# ADR-0001: Implementation Language

- **Status:** Proposed
- **Date:** 2026-06-21
- **Deciders:** @degreane

## Context

DataDiode is a security-sensitive, long-running network daemon that must:
- Run identically on Linux, Windows, macOS.
- Ship as a single self-contained binary (operators deploy onto isolated hosts).
- Support pluggable protocol adapters added by third parties.
- Minimise audit surface (small codebase, few dependencies).
- Be implementable with low effort by a small team.

## Decision

We will implement the core (sender daemon `diode-tx`, receiver daemon `diode-rx`, framing, transport, integrity) in **Go**.

Plugin extension points will be provided via:
1. **WebAssembly (wazero runtime)** — default, sandboxed, language-agnostic.
2. **Subprocess plugins (stdio JSON-RPC, `go-plugin` style)** — for plugins needing native OS access.

In-process dynamic loading (`plugin.Open` / `dlopen`) is explicitly **rejected** — not portable to Windows, and a plugin crash would take down the host.

## Alternatives Considered

- **Rust** — strongest memory safety, but higher developer cost; revisit if formal certification becomes a goal.
- **Python** — fine for the test harness and as one of several plugin-author languages; rejected for the production data plane (GIL, deploy complexity).
- **Java/Kotlin** — JRE deployment unacceptable for air-gapped targets.
- **C/C++/Zig** — receiver parses untrusted bytes; memory-unsafe languages are an unjustified risk for the marginal performance gain.

## Consequences

- Build: `go build` with `CGO_ENABLED=0` for true static binaries; cross-compile via `GOOS`/`GOARCH`.
- Audit: keep the core under ~3,000 LoC; every third-party dep requires an explicit ADR.
- Plugins: authors can use any language that compiles to WASM, or any language that can speak our subprocess protocol.

## Open Questions

- Final pick between protobuf and CBOR for on-wire framing — deferred to ADR-0002.
- FEC library choice (pure-Go Reed-Solomon) — deferred to ADR-0003.
