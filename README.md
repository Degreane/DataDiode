# DataDiode

A portable, modular, pluggable **software data diode** — a unidirectional network gateway that enforces one-way data flow from a *low-side* (source) network to a *high-side* (destination) network, with no possibility of return traffic at the application layer.

> Local repository (not pushed to a remote).

---

## What is a Data Diode?

A **data diode** (also called a *unidirectional security gateway* or *unidirectional network*) is a network device or software construct that allows data to travel in **one direction only**. It is the network equivalent of an electronic diode: traffic flows from A → B, and physically (or logically) **cannot** flow from B → A.

### Why use one?

Data diodes are used to bridge networks of different trust levels while preserving the security boundary:

- **OT / ICS / SCADA** — get telemetry out of a power plant, refinery, or factory floor without exposing the control network to the internet.
- **Classified / cross-domain** — push data from a low-classification enclave into a higher-classification enclave (or vice versa, depending on policy) without leakage in the reverse direction.
- **Log / SIEM aggregation** — ship audit logs out of a sensitive zone with zero risk of an attacker pivoting back through the log pipe.
- **Financial / regulated networks** — segregate trading, settlement, or PCI zones.
- **Air-gap replacement** — preserve most of the security guarantees of an air gap while still enabling automated data flow.

### How it differs from a firewall

A firewall is a *policy* device: it allows or denies traffic based on rules, but the underlying medium is bidirectional. A misconfiguration, a zero-day, or a compromised rule set can permit reverse traffic. A **true data diode** is enforced by the *medium itself* — most commonly by physically removing the receive (RX) path on one side of an optical link — so reverse traffic is not just disallowed, it is **physically impossible**.

A *software* data diode (this project) cannot match a hardware diode's physical guarantee, but it can provide a strong logical guarantee suitable for many real-world threat models — especially when combined with OS-level firewall rules, separate NICs, and minimal attack surface on the receiver.

### Hardware vs Software

| | Hardware diode | Software diode |
|---|---|---|
| Reverse channel | Physically impossible (cut fiber, optical tap) | Logically prevented (no listener / no return socket) |
| Cost | $10k–$100k+ per pair | Free / commodity hardware |
| Certifications | Common Criteria EAL4+, NSA-approved variants | None inherent |
| Protocol support | Vendor-specific proxies | Pluggable per deployment |
| Throughput | Multi-Gbps line rate | Bound by CPU / NIC / disk |
| Examples | Owl Cyber Defense, Waterfall Security, Fox-IT, Advenica, BAE | *(this project)*, `udp-broadcast-relay`, custom UDP forwarders |

The classic trick for software diodes is to use **UDP** (connectionless, no ACKs needed) plus **forward error correction** or **redundant transmission** to compensate for the lack of retransmission — because retransmission would require a reverse channel, which would defeat the diode.

---

## Project Goals

1. **Portable** — runs on Linux, Windows, macOS, and ideally BSD with the same binary semantics.
2. **Pluggable** — protocol adapters (syslog, file, OPC-UA, MQTT, TCP-stream, custom) load as plugins without recompiling the core.
3. **Modular** — clean separation between *transport*, *encoding*, *integrity*, and *protocol adapter* layers.
4. **Minimal effort** — small codebase, few external dependencies, easy to audit.
5. **Auditable** — a security-sensitive tool must be small enough to read end-to-end.
6. **Operable** — sensible defaults, structured logging, metrics, health checks.

## Architecture (sketch)

```
  ┌─────────────────┐   one-way UDP / serial / optical   ┌─────────────────┐
  │   LOW SIDE      │ ─────────────────────────────────► │   HIGH SIDE     │
  │   (sender)      │                                    │   (receiver)    │
  │                 │                                    │                 │
  │ ┌─────────────┐ │                                    │ ┌─────────────┐ │
  │ │ Ingress     │ │                                    │ │ Egress      │ │
  │ │ Plugin      │ │                                    │ │ Plugin      │ │
  │ │ (syslog,    │ │                                    │ │ (file,      │ │
  │ │  file,      │ │                                    │ │  syslog,    │ │
  │ │  TCP, …)    │ │                                    │ │  kafka, …)  │ │
  │ └──────┬──────┘ │                                    │ └──────▲──────┘ │
  │        │        │                                    │        │        │
  │ ┌──────▼──────┐ │                                    │ ┌──────┴──────┐ │
  │ │ Framing +   │ │                                    │ │ Reassembly  │ │
  │ │ FEC + Hash  │ │                                    │ │ + Verify    │ │
  │ └──────┬──────┘ │                                    │ └──────▲──────┘ │
  │        │        │                                    │        │        │
  │ ┌──────▼──────┐ │                                    │ ┌──────┴──────┐ │
  │ │ UDP Tx      │─┼───────────────────────────────────►│─│ UDP Rx      │ │
  │ └─────────────┘ │                                    │ └─────────────┘ │
  └─────────────────┘                                    └─────────────────┘
        NO RETURN PATH — receiver never sends a single byte back
```

## Language Choice

See [`docs/research/00-language-evaluation.md`](docs/research/00-language-evaluation.md) — full evaluation pending the deep-research report. Working recommendation: **Go**.

Rationale (short form):
- Single static binary, trivial cross-compilation (`GOOS=windows GOARCH=amd64 go build`) → portability.
- Strong stdlib for `net`, `crypto`, `encoding`, `os` → minimal external dependencies.
- Goroutines + channels map naturally onto the *ingress → frame → transmit* pipeline.
- Plugin story: Go has `plugin` package (Linux/macOS only), but the more portable answer is **subprocess plugins over stdio / Unix sockets** or **WASM via wazero**, both of which we'll evaluate.
- Small enough to be audited; large enough ecosystem to avoid reinventing TLS, CBOR, etc.

Rust and Zig are credible alternatives if memory-safety-without-GC is a hard requirement. Python is rejected for production data-plane work (GIL, deploy complexity) but is a fine choice for the test harness.

## Repository Layout (planned)

```
datadiode/
├── README.md
├── LICENSE
├── docs/
│   ├── research/        ← background research, language eval, prior art
│   ├── architecture/    ← ADRs, protocol spec, threat model
│   └── sprints/         ← agile sprint plans & retros
├── cmd/
│   ├── diode-tx/        ← low-side sender daemon
│   └── diode-rx/        ← high-side receiver daemon
├── internal/
│   ├── transport/       ← UDP, serial, file-drop transports
│   ├── framing/         ← chunking, sequencing, FEC
│   ├── integrity/       ← hashing, signing
│   └── plugin/          ← plugin host (subprocess / WASM)
├── plugins/
│   ├── syslog/
│   ├── file/
│   └── tcp-stream/
└── test/
    └── e2e/
```

## Quick start (Fedora + plain LXC)

```bash
# 0. Verify prereqs (Go + lxc + lxc-templates + nftables)
./scripts/check-prereqs.sh
# If anything is missing:
sudo dnf install -y lxc lxc-templates nftables golang

# 1. End-to-end demo across two LXC containers
sudo ./scripts/demo.sh
# Output ends with "MATCH — diode roundtrip succeeded".

# 2. Tear down when done
sudo ./scripts/lxc-teardown.sh
```

The demo orchestrates `lxc-setup.sh` → `lxc-push.sh` → `lxc-harden.sh`, then launches
`diode --mode=rx` in `diode-high`, pipes a file through `diode --mode=tx` in
`diode-low`, and SHA-256-diffs the input against the reconstructed output.

For loopback-only testing on the host (no LXC needed):

```bash
go test ./...                              # unit + reassembly tests
go test ./test/e2e/ -v                     # spawns the real binary on 127.0.0.1
go build -o /tmp/diode ./cmd/diode && /tmp/diode --help
```

## Status

🚧 **Sprint 1 — MVP.** See [`docs/sprints/sprint-01-mvp.md`](docs/sprints/sprint-01-mvp.md).
Sprint 0 (research + ADRs + project scaffolding) closed at commit `f69047b`.

## License

TBD (likely Apache-2.0 or MIT). See [`LICENSE`](LICENSE) once chosen.
