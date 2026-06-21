# Sprint 01 — MVP: tx → UDP → rx, in two LXC containers

- **Dates:** 2026-07-05 → 2026-07-18
- **Sprint Goal:** A working unidirectional pipe between two LXC system containers on an isolated bridge network, demonstrable end-to-end.

> **Why LXC, not Docker?**
> LXC system containers behave like small VMs: each has its own init, its own network namespace, its own iptables/nftables, and persistent state. That matches the diode threat model much better than Docker — we want two *machines*, with a network in between, and the receiver having genuinely no return path. Docker's bridge + NAT model muddies that picture; LXC's bridged or routed network with per-container firewalls makes the diode boundary obvious and auditable.

> **Demo at sprint review:**
> `lxc launch ubuntu:24.04 diode-low` / `diode-high`, push the binary in, start `diode-tx` in *low* and `diode-rx` in *high*, pipe a file through, diff at the other end. `tcpdump` on the host bridge shows traffic in one direction only. `nft list ruleset` on `diode-high` shows DROP on egress toward `diode-low`.

## Sprint Goal (one sentence)

> Ship a minimal, auditable Go implementation of `diode-tx` and `diode-rx` that moves bytes one-way over UDP between two LXC containers, with per-message integrity, no return channel, and a CI matrix proving it builds on linux/windows/macos.

## Stack (locked in for this sprint)

- **Language:** Go 1.22+
- **Transport:** UDP/IPv4 (IPv6 follow-up)
- **Framing:** length-prefixed binary records (CBOR payload, std `encoding/binary` header) — no protobuf dependency yet
- **Integrity:** SHA-256 per message
- **FEC:** *none* this sprint — naive N× redundant transmission as a config flag (default N=1)
- **Plugins:** *none* this sprint — ingress/egress are stdin/stdout and file paths only
- **Container runtime:** plain LXC (the `lxc-*` userspace shipped on Fedora — `lxc-create`, `lxc-start`, `lxc-attach`, `lxc-stop`, `lxc-destroy`). **Not** LXD.

## On-wire frame (v0)

Locked in [ADR-0002](../architecture/ADR-0002-frame-format.md). Summary:

```
magic(4) | ver(1) | flags(1) | seq(8) | msg_id(4) | chunk_index(2) |
chunk_total(2) | payload_len(4) | payload(≤1400) | sha256(32)
```

- Header is fixed 26 B; total frame ≤ 1458 B (under Ethernet MTU).
- All multi-byte ints big-endian.
- Chunking is explicit (`msg_id` + `chunk_index`/`chunk_total`); FINAL flag marks last chunk.
- SHA-256 covers header + payload (everything before the hash).
- Receiver silently drops anything that fails validation rules (no return channel for error reporting).

## Backlog

| ID | Story | Est | Status |
|---|---|---|---|
| S01-1  | `go mod init github.com/degreane/datadiode`; module layout (`cmd/`, `internal/`) | XS | ⚪ |
| S01-2  | ADR-0002: on-wire frame format (the table above) | S | ⚪ |
| S01-3  | `internal/framing`: encode/decode + unit tests + fuzz target | M | ⚪ |
| S01-4  | `internal/integrity`: SHA-256 wrap/verify | XS | ⚪ |
| S01-5  | `internal/transport/udp`: `Sender` (writer) and `Receiver` (read loop) | M | ⚪ |
| S01-6  | `cmd/diode --mode=tx`: flags (`--dst`, `--chunk`, `--rate`, `--redundancy`, `--in`, `--max-message`), stdin→frames→UDP | M | ✅ |
| S01-7  | `cmd/diode --mode=rx`: flags (`--listen`, `--out`, `--max-pending`, `--max-bytes`, `--delimiter`), UDP→verify→reassemble→stdout/file | M | ✅ |
| S01-8  | E2E test on loopback (Go test that spawns both binaries) | S | ✅ |
| S01-9  | LXC scripts: `lxc-setup.sh`, `lxc-push.sh`, `lxc-harden.sh`, `check-prereqs.sh`, `_common.sh` | M | ✅ (scripts written + syntax-clean; live run pending `dnf install -y lxc`) |
| S01-10 | `scripts/lxc-teardown.sh`: clean removal | XS | ✅ |
| S01-11 | GitHub Actions: lint, native test on linux/macos/windows, cross-build matrix (linux/darwin/windows/freebsd × amd64/arm64), short fuzz, shell syntax | M | ✅ |
| S01-12 | `docs/architecture/threat-model.md` (STRIDE quick pass) | S | ⚪ |
| S01-13 | Demo script `scripts/demo.sh` — pipes a file through the two LXCs and diffs the output | S | ✅ (script written; live run pending `lxc` package) |
| S01-14 | Sprint review + retro | XS | ⚪ |

## Implementation Order (critical path)

```
S01-1 ─► S01-2 ─► S01-3 ─► S01-4 ─► S01-5 ─┬─► S01-6 ─┐
                                            │           ├─► S01-8 ─► S01-9 ─► S01-13
                                            └─► S01-7 ─┘     │
                                                              ▼
                                                           S01-10
                                                              │
                                                              ▼
                                                           S01-14
```

S01-11 (CI) and S01-12 (threat model) run in parallel with the data-plane work.

## Non-Goals (explicit)

- Plugins (Sprint 02).
- FEC / Reed-Solomon (Sprint 02 or 03).
- TLS / signing (Sprint 03 — pre-shared key only, since DTLS is bidirectional).
- Real protocol adapters (syslog/file watcher/OPC) — Sprint 02+.
- Throughput tuning beyond "works at 10 MB/s on loopback".
- Windows service / systemd units.

## Definition of Done (sprint)

- `scripts/lxc-setup.sh && scripts/demo.sh` runs green on a fresh Fedora host with `lxc` installed (`sudo dnf install lxc lxc-templates`).
- `tcpdump -i diodebr0` during the demo shows packets only in the tx→rx direction.
- `nft list ruleset` inside `diode-high` shows DROP on egress to `diode-low`.
- CI matrix is green on linux/windows/macos (LXC steps run only on the linux job).
- Threat model committed.
- Sprint review + retro written.

## Container Topology (preview)

```
            host (Fedora, plain LXC)
            │
            │  diodebr0  (isolated bridge, no upstream, no NAT)
            │   10.99.0.0/24
            │
    ┌───────┴────────┐               ┌────────────────────┐
    │ LXC: diode-low │               │ LXC: diode-high    │
    │ 10.99.0.10     │   UDP/9999    │ 10.99.0.20         │
    │                │ ─────────────►│                    │
    │ diode-tx       │               │ diode-rx           │
    │   --dst        │               │   --listen         │
    │   10.99.0.20   │               │   0.0.0.0:9999     │
    │   :9999        │               │   --out /data/sink │
    │                │               │                    │
    │ nft: allow OUT │               │ nft: DROP all OUT  │
    │   UDP/9999     │               │ (the "no return"   │
    │   to .20       │               │  rule)             │
    └────────────────┘               └────────────────────┘
```

Two enforcement layers stacked:
1. **Application layer:** `diode-rx` opens no outbound sockets, ever. Audited in code.
2. **Network layer:** `nft` on `diode-high` drops every outbound packet. Belt and braces.

## LXC Setup (preview — full script lands in S01-9)

Plain LXC (Fedora). Containers live in `/var/lib/lxc/<name>/`; binaries are pushed by copying into `<rootfs>/usr/local/bin/`.

```bash
# 0. prereqs (Fedora)
sudo dnf install -y lxc lxc-templates libvirt-daemon-config-network nftables

# 1. isolated bridge on the host — no upstream, no NAT
sudo ip link add name diodebr0 type bridge
sudo ip addr add 10.99.0.1/24 dev diodebr0
sudo ip link set diodebr0 up
# (persist via NetworkManager keyfile or systemd-networkd in the real script)

# 2. create two minimal containers from the Fedora template
for name in diode-low diode-high; do
  sudo lxc-create -n "$name" -t download -- \
      --dist fedora --release 40 --arch amd64
done

# 3. wire each container's veth to diodebr0 with a pinned IP
#    (edit /var/lib/lxc/<name>/config)
sudo tee -a /var/lib/lxc/diode-low/config >/dev/null <<'EOF'
lxc.net.0.type = veth
lxc.net.0.link = diodebr0
lxc.net.0.flags = up
lxc.net.0.ipv4.address = 10.99.0.10/24
EOF
sudo tee -a /var/lib/lxc/diode-high/config >/dev/null <<'EOF'
lxc.net.0.type = veth
lxc.net.0.link = diodebr0
lxc.net.0.flags = up
lxc.net.0.ipv4.address = 10.99.0.20/24
EOF

# 4. push binaries by copying into the rootfs (containers stopped)
sudo install -m 0755 ./bin/diode-tx /var/lib/lxc/diode-low/rootfs/usr/local/bin/diode-tx
sudo install -m 0755 ./bin/diode-rx /var/lib/lxc/diode-high/rootfs/usr/local/bin/diode-rx

# 5. start them
sudo lxc-start -n diode-low
sudo lxc-start -n diode-high
sudo lxc-wait  -n diode-high -s RUNNING

# 6. lock down diode-high egress (belt-and-braces, on top of "no rx sockets")
sudo lxc-attach -n diode-high -- nft -f - <<'EOF'
table inet diode {
  chain output {
    type filter hook output priority 0; policy drop;
    oifname "lo" accept
  }
}
EOF

# 7. run
sudo lxc-attach -n diode-high -- /usr/local/bin/diode-rx --listen 0.0.0.0:9999 --out /tmp/sink &
sudo lxc-attach -n diode-low  -- sh -c 'cat /etc/os-release | /usr/local/bin/diode-tx --dst 10.99.0.20:9999'

# teardown
sudo lxc-stop -n diode-low  ; sudo lxc-destroy -n diode-low
sudo lxc-stop -n diode-high ; sudo lxc-destroy -n diode-high
sudo ip link del diodebr0
```

## Risks

- **Plain LXC requires root** for `lxc-create`/`lxc-start` on Fedora out of the box. Unprivileged LXC is possible but adds setup friction — Sprint 01 uses privileged containers; unprivileged is a follow-up.
- **Path MTU on the LXC bridge** — usually 1500; we pin payload to 1400 to be safe.
- **SELinux on Fedora.** May interfere with `lxc-attach` / bind-mounts. *Mitigation:* document `setenforce 0` as a fallback, then add proper policy in a later sprint.
- **nftables is the default on Fedora**, so no iptables fallback needed for the Sprint 01 demo.
- **CBOR dependency** — pulls in `fxamacker/cbor/v2`. Acceptable; one well-known module. Will document in ADR-0002.

## Daily Notes

_(filled in during the sprint)_

## Review / Retro

_(end of sprint)_
