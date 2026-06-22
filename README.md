# DataDiode

A portable, modular, pluggable **software data diode** — a unidirectional network gateway that enforces one-way data flow from a *low-side* (source) network to a *high-side* (destination) network, with no possibility of return traffic at the application layer.

> Hosted at https://github.com/degreane/DataDiode.

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

## Prebuilt binaries

Cross-compiled, stripped, static (`CGO_ENABLED=0`) builds for seven
platforms are checked into [`release/`](release/) on this branch.
About 3 MB per binary; one self-contained file per target.

| Platform | File | Notes |
|---|---|---|
| Linux amd64   | `release/diode-linux-amd64`       | glibc-free static binary |
| Linux arm64   | `release/diode-linux-arm64`       | Raspberry Pi 4/5, AWS Graviton, etc. |
| macOS amd64   | `release/diode-darwin-amd64`      | Intel Macs |
| macOS arm64   | `release/diode-darwin-arm64`      | Apple Silicon |
| Windows amd64 | `release/diode-windows-amd64.exe` | |
| Windows arm64 | `release/diode-windows-arm64.exe` | Surface Pro X, Windows-on-ARM |
| FreeBSD amd64 | `release/diode-freebsd-amd64`     | |

### Download just the binary you need (no clone)

```bash
# Linux amd64 example — swap the filename for your target.
curl -fLO https://raw.githubusercontent.com/Degreane/DataDiode/enhanced/release/diode-linux-amd64
chmod +x diode-linux-amd64
./diode-linux-amd64 --version
```

### Verify the checksum

```bash
curl -fLO https://raw.githubusercontent.com/Degreane/DataDiode/enhanced/release/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
```

### Rebuild them yourself

Quick path with `make`:

```bash
make cross DIST_DIR=release          # outputs the same 7 files into release/
( cd release && sha256sum diode-* > SHA256SUMS )
```

The build is reproducible-ish: identical Go toolchain + identical
commit + identical `-trimpath -ldflags "-s -w …"` flags should give
you matching SHA-256s (modulo build-date embedded in `--version`).

#### Cross-compile tutorial (from scratch)

If you don't have `make`, or you want to understand what the Makefile
is doing, the build is just `go build` with two env vars.

**1. Install Go 1.22+ on the host doing the build** (Linux example):

```bash
# Fedora / RHEL
sudo dnf install -y golang
# Debian / Ubuntu
sudo apt install -y golang-go
# macOS
brew install go
# Verify (need >= 1.22)
go version
```

Go ships with cross-compilers for every supported target already —
you do **not** need a Windows machine to build a Windows binary, or
an ARM box to build an arm64 binary. One toolchain, all targets.

**2. Clone the repo:**

```bash
git clone -b enhanced https://github.com/Degreane/DataDiode.git
cd DataDiode
```

**3. Build for one target at a time** with `GOOS` + `GOARCH`:

```bash
mkdir -p release

# Native build (whatever host you're on)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o release/diode ./cmd/diode

# Linux amd64
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o release/diode-linux-amd64 ./cmd/diode

# Linux arm64 (Raspberry Pi 4/5, AWS Graviton, etc.)
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o release/diode-linux-arm64 ./cmd/diode

# macOS Intel
GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o release/diode-darwin-amd64 ./cmd/diode

# macOS Apple Silicon
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o release/diode-darwin-arm64 ./cmd/diode

# Windows amd64
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o release/diode-windows-amd64.exe ./cmd/diode

# Windows arm64 (Surface Pro X, Windows-on-ARM)
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o release/diode-windows-arm64.exe ./cmd/diode

# FreeBSD amd64
GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o release/diode-freebsd-amd64 ./cmd/diode
```

**4. Or loop over the whole matrix in one shell snippet:**

```bash
for target in linux/amd64 linux/arm64 \
              darwin/amd64 darwin/arm64 \
              windows/amd64 windows/arm64 \
              freebsd/amd64; do
  os=${target%/*}; arch=${target#*/}
  ext=""; [ "$os" = "windows" ] && ext=".exe"
  out="release/diode-${os}-${arch}${ext}"
  echo "→ $out"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/diode
done
sha256sum release/diode-* > release/SHA256SUMS
```

**5. What each flag is for:**

| Flag | Why |
|---|---|
| `CGO_ENABLED=0`       | Pure-Go build — no glibc / no `libSystem` dependency. The Linux binary runs on Alpine, musl, and even inside `FROM scratch` containers. |
| `GOOS=<os>`           | Target operating system. Values used here: `linux`, `darwin`, `windows`, `freebsd`. |
| `GOARCH=<arch>`       | Target CPU architecture: `amd64` (x86-64) or `arm64`. |
| `-trimpath`           | Strips local filesystem paths out of the binary — gets you closer to reproducible builds and removes a tiny info leak. |
| `-ldflags="-s -w"`    | `-s` strips the symbol table, `-w` strips DWARF debug info. ~30% smaller binaries. |
| `-ldflags="-X main.version=…"` | (Makefile does this) Embeds `git describe` and commit hash so `diode --version` shows what was built. |

**6. Smoke-test the binary that matches your host:**

```bash
./release/diode-linux-amd64 --version
./release/diode-linux-amd64 --mode=tx --help | head
```

For non-native targets you'll need either the actual hardware, an
emulator (`qemu-user-static` for Linux on ARM; Wine for Windows
binaries on Linux), or just `scp` it to the target machine and run
it there.

**7. (Optional) verify your build matches the checked-in `release/`:**

```bash
( cd release && sha256sum -c SHA256SUMS )
```

`-s -w -trimpath` + the same Go toolchain version + the same commit
should give you bit-identical binaries. If they differ, check
`go version` first — minor toolchain updates change the embedded
build ID.

---

## Getting the enhanced branch

Active development lives on the `enhanced` branch (the `main` branch
on this remote is older history). Pick whichever workflow matches what
you have locally.

### Fresh clone

```bash
git clone -b enhanced git@github.com:Degreane/DataDiode.git
# or, over HTTPS:
git clone -b enhanced https://github.com/Degreane/DataDiode.git
cd DataDiode
```

### Existing clone — switch to it

```bash
git fetch origin enhanced
git checkout enhanced          # creates a local tracking branch on first run
git pull --ff-only              # subsequent updates
```

### Just peek at what's on enhanced without switching

```bash
git fetch origin
git log --oneline origin/main..origin/enhanced       # commits enhanced has that main doesn't
git diff origin/main...origin/enhanced -- README.md  # or any path
```

> Heads up: `enhanced` and `main` have diverged. Don't merge `main`
> into `enhanced` (or vice versa) without reading the commit log first
> — the wire protocol on `enhanced` is at v4, while `main` predates
> session-based framing.

---

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

Three sprints closed. Wire protocol at v4 (AEAD + FEC). All HIGH
residual risks from the threat model closed.

- Sprint 0 (research + ADRs + project scaffolding) — closed.
- Sprint 1 (MVP) — closed. See [`docs/sprints/sprint-01-mvp.md`](docs/sprints/sprint-01-mvp.md).
- Sprint 2 (file ops + manifest + vacuum) — closed.
- Sprint 3 (encryption + replay protection + FEC) — closed.

**Thinking about a real customer deployment?**

| What you need | Where to look |
|---|---|
| **Total beginner — install + first transfer + every flag** | [`docs/operator-guide.md`](docs/operator-guide.md) |
| Plan the deployment + understand surprises | [`docs/enterprise-roadmap.md`](docs/enterprise-roadmap.md) |
| Verify the host BEFORE installing | `scripts/preflight.sh --role=rx` (or `tx`/`both`) |
| Pick host + disk + RAM sizes | [`docs/sizing-guide.md`](docs/sizing-guide.md) |
| On-call playbook for incidents | [`docs/runbooks.md`](docs/runbooks.md) |
| Concept-first walk-through with diagrams | [`docs/tutorial.md`](docs/tutorial.md) |

## License

TBD (likely Apache-2.0 or MIT). See [`LICENSE`](LICENSE) once chosen.
