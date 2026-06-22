# DataDiode — Sizing & Capacity Planning

> Audience: line operator + the engineer specifying the host.
> Purpose: pick the right host size, disk size, and config before the
> first transfer — and know when you've outgrown one rx process.
>
> Numbers below are **order-of-magnitude estimates** based on
> architecture, not formal benchmarks. Validate on your hardware before
> committing to a production envelope. We'll replace them with measured
> numbers in Sprint 04.

---

## 1. The thirty-second sizing answer

For 80% of deployments:

| Load profile | Sender host | Receiver host | Spool disk |
|---|---|---|---|
| **Telemetry / logs** (≤ 100 Mbps avg, files ≤ 100 MiB) | 2 vCPU / 2 GiB | 2 vCPU / 2 GiB | 20 GiB SSD on `/var/spool/diode` |
| **Batch file shipping** (avg 200 Mbps bursts, files ≤ 4 GiB) | 4 vCPU / 4 GiB | 4 vCPU / 8 GiB | 100 GiB SSD |
| **Heavy / continuous** (sustained > 500 Mbps, files > 4 GiB) | 8 vCPU / 8 GiB | 8 vCPU / 16 GiB | 500 GiB NVMe |
| **Multi-tenant ingest** (many concurrent senders) | per-tenant | 16 vCPU / 32 GiB | 1 TiB NVMe + monitor |

Everything below is the math behind those numbers — read it when the
defaults don't fit, or when something needs explaining to procurement.

---

## 2. Wire-level math (per chunk and per session)

### Per chunk

| Component | Bytes | Notes |
|---|---|---|
| UDP + IP overhead | 28 | 20B IPv4 + 8B UDP |
| Diode DATA header | ~80 | magic, version, session id (16B), chunk index, flags, fec group, AEAD nonce + tag (28B) |
| Payload | `--chunk-size` (default 1400) | configurable; cap at `path-MTU − 100` |
| **Total on the wire** | `~ 1508` | with `--chunk-size=1400` on a 1500-MTU link |

So per 1 GiB transferred: `1 GiB / 1400 B ≈ 766,000 chunks`. At an
extra ~108 B/chunk of overhead, expect **~8% wire overhead** at the
default chunk size before FEC. FEC at `--fec-group-size=8` adds
another `1/8 = 12.5%`. Total: ~**20% overhead** at default settings.

### Per session (in spool)

| Item | Size | Notes |
|---|---|---|
| Session dir entry | one inode | `/var/spool/diode/<sid>/` |
| `meta.json` | ~1 KiB | filename, size, sha, chunk total, fec params |
| `chunks.bitmap` | `ceil(N_chunks / 8) B` | 1 GiB file → ~95 KiB bitmap |
| `data.partial` | apparent size = total_bytes; **actual on disk** = bytes received so far (sparse) | this is the dominant disk cost |
| **Per-session steady-state cost** | total file size + ~100 KiB | bitmap + meta + inode are negligible vs payload |

When `--spool-mode=files`, replace `data.partial` with N small files
(one per chunk). That's `inode_count = N_chunks`. Avoid this mode on
filesystems with low inode budgets (e.g. small ext4).

### Per session (completed-cache, `<spool>/completed.idx`)

```
record = sid(16B) + completed_at(8B unix nanos) + padding(8B) = 32 B per session
```

So 1 million completed sessions = ~32 MiB on disk. The completed-cache
grows append-only; the vacuum loop prunes records older than
`--vacuum-age`. At default `--vacuum-age=24h`, steady-state size =
`sessions_per_day × 32 B`. A million sessions per day → 32 MiB.

---

## 3. RAM budget

### Receiver

```
base process                ~30 MiB
+ active sessions           ~64 KiB metadata each + 4 KiB OS page cache for bitmap
+ FEC group buffer          group_size × chunk_size  (e.g. 8 × 1400 = 11 KiB per active group)
+ completed-cache in RAM    --completed-cache N × ~32 B  (default 1024 → ~32 KiB)
+ UDP socket buffer         --buffer-len  (default 4 MiB)
```

Worked example: 100 concurrent sessions, default flags:
`30 MiB + 100 × 64 KiB + 100 × 11 KiB + 32 KiB + 4 MiB ≈ 42 MiB`.

A 4 GiB host comfortably handles **~10,000 concurrent sessions**
before RAM is the limit. CPU and disk almost always cap you first.

### Sender

```
base process                ~30 MiB
+ in-memory file buffer     min(file_size, --max-bytes)   ← see §5 caveat
+ AEAD state                negligible
+ manifest writer           ~4 KiB
```

The sender today loads the **entire file** into memory before
transmitting (deferred streaming-session work, see roadmap §5.5). So
RAM ≥ largest single file you'll ever send. Translates to a hard
operational limit of `--max-bytes=4GB` by default; bump with care.

---

## 4. CPU budget

| Stage | Cost (order of magnitude) | Bottleneck? |
|---|---|---|
| AES-256-GCM (AES-NI) | ~5–10 Gbps per core | rarely |
| SHA-256 (SHA-NI / AVX) | ~2–4 GB/s per core | rarely |
| HKDF subkey derive | once per session | never |
| UDP syscall overhead | ~100 ns / sendmmsg or recvmmsg call | possible at very-high pps |
| XOR FEC | bandwidth-bound, ~10+ Gbps per core | never |

A 4-core x86_64 host with AES-NI should saturate a 1 Gbps link with
CPU to spare. To approach 10 Gbps line rate, expect to be CPU-bound
on syscall overhead and packet rate (`> 700 kpps` at 1400-B chunks)
before crypto. At that tier, scale out by running multiple rx
processes on different ports, not by upgrading the host.

---

## 5. Disk budget

### Spool dir size formula (steady-state)

```
spool_bytes ≈ peak_concurrent_sessions × avg_session_size
            + sessions_received_in_(--vacuum-age) × avg_session_size  // if --files-to is on the same volume
            + completed.idx steady-state size
```

Worked example, batch profile: 50 concurrent × 1 GiB avg + 24h × 1k
sessions/h × 200 MiB avg + 24k × 32 B completed-cache
`= 50 GiB + 4.6 TiB + 768 KiB ≈ 4.7 TiB`.

**Lesson**: if `--files-to` is on the same volume as `--spool`, the
guarantee is "spool dir can hold 1× `--vacuum-age` of throughput." If
it isn't, the spool dir only needs to hold concurrent in-flight
sessions. Put `--files-to` on a different (bigger) volume in production.

### Disk write rate

Receiver disk must sustain at least the wire rate (it writes every
DATA chunk once; FEC chunks are read back from disk if needed).

```
required_disk_write_bandwidth ≈ wire_bandwidth
required_disk_read_bandwidth  ≈ 0 in the no-loss case
                              ≈ (group_size − 1) × chunk_size × parity_rate
                                in the reconstruction case
```

For 1 Gbps sustained ingest, a SATA SSD (~500 MB/s sequential) is
comfortable. NVMe is overkill at 1 Gbps; mandatory at 10 Gbps.

### Inode budget (only with `--spool-mode=files`)

```
inodes_used = sum over active sessions of (N_chunks + 3)
```

A 1 GiB session with default chunk size = ~766k inodes. Two
concurrent ones = ~1.5M inodes. Default ext4 inode density (~16 KiB
per inode) is fine on a TiB-scale filesystem but **fail** on a
small (say 10 GiB) volume. Use `--spool-mode=sparse` (the default)
unless you have a specific reason.

---

## 6. Network budget

| Item | Number | Notes |
|---|---|---|
| Bytes-per-chunk on wire | 1508 (default) | including IP + UDP headers |
| Packets-per-second at 1 Gbps | ~80,000 pps | divide by 1508 × 8 |
| Packets-per-second at 10 Gbps | ~800,000 pps | rx host needs `RPS` / `RFS` + NIC multi-queue tuning |
| `net.core.rmem_max` recommendation | ≥ 16 MiB | default Linux 208 KiB is **catastrophic** for diode workloads |
| `net.core.netdev_max_backlog` | ≥ 5000 | default 1000 drops packets at >100k pps |
| FEC overhead | `1 / fec_group_size` | default 8 = 12.5% extra packets |
| Redundancy (`--redundancy=N`) overhead | `× N` | use sparingly; only when FEC isn't enough |

For sustained throughput planning, multiply your application data
rate by 1.2 for **wire bandwidth needed** (default FEC + headers). At
`--redundancy=2`, multiply by another 2 = 2.4×.

---

## 7. Scale-out triggers

Run a second rx process (or host) when:

- `data_dropped` is climbing and `/proc/net/snmp` `RcvbufErrors`
  remains > 0 despite `rmem_max=64MiB`.
- A single core is pinned at 100% on the rx host while the link still
  has headroom — CPU is the limit, not bandwidth.
- A single `--files-to` directory is being written by hundreds of
  concurrent sessions and the disk's queue depth is saturated.
- You have multiple logical tenants and want their PSK rotations to be
  independent.

Scale-out today is "operator runs two rx processes on different
ports, senders are pointed at the appropriate port." There's no
load-balancing fabric yet (see roadmap §5.4 "HA").

---

## 8. Defaults reference

Built-in defaults that affect sizing decisions:

| Flag | Default | Sizing impact |
|---|---|---|
| `--chunk-size` | 1400 B | + ~8% wire overhead vs payload |
| `--fec-group-size` | 0 (off) | when enabled, +`1/N` wire overhead |
| `--soh-interval` | 0 (sent once at start) | with `>0`, periodic SOH retransmission for late-joiner resume |
| `--max-bytes` (tx) | 4 GiB | hard cap on a single session; bump to your largest expected file |
| `--buffer-len` (rx) | 4 MiB | UDP socket read buffer; kernel-capped by `rmem_max` |
| `--max-concurrent` (rx) | 0 (unlimited) | set to bound RAM use on a shared host |
| `--completed-cache` (rx) | 1024 | RAM-only hot cache; default fine |
| `--completed-cache-disk-cap` (rx) | 100,000 | how many records to load from `completed.idx` at start |
| `--vacuum-interval` (rx) | 0 (cron-only) | set to `1h` for hands-off long-running rx |
| `--vacuum-age` (rx & vacuum mode) | 24h | spool + completed.idx older than this is pruned |

---

## 9. When you must benchmark on your own hardware

Estimates above are good enough for first-deployment sizing. Measure
your own hardware when:

- Hardware is unusual: ARM64 server, FPGA NIC, encrypted root with software dm-crypt, or a hypervisor with virtio-net contention.
- Workload concurrency > 1000 sessions/s sustained.
- Wire rate target > 1 Gbps.
- You have a regulatory commitment to a specific throughput SLO.

Quick bench loop (loopback, single host):
```bash
# Terminal 1
diode --mode=rx --listen=127.0.0.1:9999 --files-to=/tmp/rx \
  --spool=/tmp/spool

# Terminal 2 — push N files of varying sizes and time them
for sz in 1M 10M 100M 1G; do
  dd if=/dev/urandom of=/tmp/in.$sz bs=$sz count=1 2>/dev/null
  time diode --mode=tx --dst=127.0.0.1:9999 \
    --send-file=/tmp/in.$sz --filename=$sz.bin
done
```

Loopback throughput is a ceiling; real wire will be lower. Multiply
loopback bps by 0.7 for a realistic estimate over a clean 1 Gbps link.
