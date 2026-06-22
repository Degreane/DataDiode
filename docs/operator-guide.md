# DataDiode — The Operator Guide

> Audience: a competent sysadmin who has never used DataDiode before.
> Goal: read this top-to-bottom in one sitting; come out knowing
> exactly what to install, what every flag means, what to set on your
> platform, and what to do when something is wrong.
>
> Companion docs (deeper but narrower):
> - [`tutorial.md`](tutorial.md) — concept-first walk-through with diagrams.
> - [`runbooks.md`](runbooks.md) — on-call playbooks for 7 ranked incidents.
> - [`sizing-guide.md`](sizing-guide.md) — capacity planning math.
> - [`enterprise-roadmap.md`](enterprise-roadmap.md) — what's missing for procurement.
> - [`psk-howto.md`](psk-howto.md) — PSK lifecycle reference.

---

## Table of contents

1. [What you're operating, in two paragraphs](#1-what-youre-operating-in-two-paragraphs)
2. [The mental model](#2-the-mental-model)
3. [Install on your platform](#3-install-on-your-platform)
4. [Your first transfer (5 minutes, loopback)](#4-your-first-transfer-5-minutes-loopback)
5. [The five modes](#5-the-five-modes)
6. [Every flag, explained](#6-every-flag-explained)
7. [Best-practice configurations by scenario](#7-best-practice-configurations-by-scenario)
8. [Per-platform best practices](#8-per-platform-best-practices)
9. [Running as a service](#9-running-as-a-service)
10. [Security best practices](#10-security-best-practices)
11. [Day-2 operations](#11-day-2-operations)
12. [Troubleshooting cheat sheet](#12-troubleshooting-cheat-sheet)
13. [Glossary](#13-glossary)

---

## 1. What you're operating, in two paragraphs

DataDiode is a **one-way file/byte transfer tool**. You run it as two
processes: a **sender** (`diode --mode=tx`) on a low-trust side, and a
**receiver** (`diode --mode=rx`) on a high-trust side. The sender
takes a file (or arbitrary bytes from stdin), chunks it, optionally
encrypts each chunk with a shared key, and sprays the chunks at the
receiver over UDP. The receiver reassembles them and atomically writes
the completed file to a destination directory.

UDP is used **deliberately** — the receiver never sends a single byte
back. That property (no return channel) is the security guarantee: a
compromised receiver cannot pivot back through the diode to attack the
sender. The trade-off is no automatic retransmission — we compensate
with **forward error correction (FEC)**, optional redundant
transmission, and operator-driven `--resend` of completed sessions.

---

## 2. The mental model

```
  LOW SIDE                                                    HIGH SIDE
  (less trusted)                                              (more trusted)

  ┌─────────────────┐                                         ┌─────────────────┐
  │ application     │                                         │ application     │
  │ that produces   │                                         │ that consumes   │
  │ data            │                                         │ data            │
  │ ────┬────       │                                         │  ▲    │         │
  │     ▼  bytes    │                                         │  │ file appears │
  │ diode tx        │                                         │ diode rx        │
  │   - read input  │   one-way UDP, encrypted+authenticated  │   - listen      │
  │   - chunk       │ ─────────────────────────────────────►  │   - reassemble  │
  │   - encrypt     │                                         │   - verify hash │
  │   - send        │                                         │   - rename      │
  │                 │ ◄═══ NEVER any traffic this way ══════  │                 │
  └─────────────────┘                                         └─────────────────┘
```

Two facts to internalize:

1. **There is no acknowledgement, ever.** The sender doesn't know
   whether you got the data. It will exit cleanly even if the receiver
   was off, the firewall blocked the port, or the network ate every
   packet. Always start the receiver **first** for a new transfer.
2. **The receiver opens zero outbound sockets.** Architecturally and
   physically: there is no return path. This is enforced in the code
   (the `Receiver` type literally has no `Send`/`Write` method) and
   should also be enforced at your firewall (egress block on the
   receiver host).

---

## 3. Install on your platform

Three install options, in order of "I'm new" to "I'll build it myself":

1. **Download a prebuilt binary** from `release/` on the `enhanced`
   branch of the GitHub repo. Fastest. No toolchain needed.
2. **Build from source with `make`**. Needs Go 1.22+ and git.
3. **Build from source with raw `go build`**. Needs only Go 1.22+.

### 3.1 Linux (Fedora / RHEL / Rocky / Alma)

```bash
# Option A: download prebuilt
sudo curl -fLo /usr/local/bin/diode \
  https://raw.githubusercontent.com/Degreane/DataDiode/enhanced/release/diode-linux-amd64
sudo chmod +x /usr/local/bin/diode
diode --version

# Option B: build from source
sudo dnf install -y git golang make
git clone -b enhanced https://github.com/Degreane/DataDiode.git
cd DataDiode
make build && sudo make install
```

### 3.2 Linux (Debian / Ubuntu)

```bash
# Option A: prebuilt
sudo curl -fLo /usr/local/bin/diode \
  https://raw.githubusercontent.com/Degreane/DataDiode/enhanced/release/diode-linux-amd64
sudo chmod +x /usr/local/bin/diode

# Option B: build from source (Debian 12+ / Ubuntu 22.04+ ship Go 1.22+)
sudo apt update && sudo apt install -y git golang-go make
git clone -b enhanced https://github.com/Degreane/DataDiode.git
cd DataDiode && make build && sudo make install
```

### 3.3 Linux (Alpine / musl-based)

The prebuilt Linux binary is `CGO_ENABLED=0` static — it runs as-is on
Alpine without installing glibc-compat:

```sh
wget -O /usr/local/bin/diode \
  https://raw.githubusercontent.com/Degreane/DataDiode/enhanced/release/diode-linux-amd64
chmod +x /usr/local/bin/diode
diode --version
```

### 3.4 Linux ARM64 (Raspberry Pi 4/5, AWS Graviton, etc.)

```bash
sudo curl -fLo /usr/local/bin/diode \
  https://raw.githubusercontent.com/Degreane/DataDiode/enhanced/release/diode-linux-arm64
sudo chmod +x /usr/local/bin/diode
```

### 3.5 macOS (Intel)

```bash
curl -fLo /usr/local/bin/diode \
  https://raw.githubusercontent.com/Degreane/DataDiode/enhanced/release/diode-darwin-amd64
chmod +x /usr/local/bin/diode
# First run: macOS will block. Right-click → Open → Allow, or:
xattr -d com.apple.quarantine /usr/local/bin/diode
diode --version
```

### 3.6 macOS (Apple Silicon — M1/M2/M3/M4)

Same as above, with the `darwin-arm64` binary.

### 3.7 Windows 10 / 11 / Server

```powershell
# As Administrator in PowerShell:
New-Item -ItemType Directory -Path 'C:\Program Files\diode' -Force
Invoke-WebRequest -Uri 'https://raw.githubusercontent.com/Degreane/DataDiode/enhanced/release/diode-windows-amd64.exe' `
                  -OutFile 'C:\Program Files\diode\diode.exe'
[Environment]::SetEnvironmentVariable('Path', $env:Path + ';C:\Program Files\diode', 'Machine')
# Open a new PowerShell window:
diode --version
```

For ARM64 Windows (Surface Pro X), use `diode-windows-arm64.exe`.

### 3.8 FreeBSD

```sh
fetch -o /usr/local/bin/diode \
  https://raw.githubusercontent.com/Degreane/DataDiode/enhanced/release/diode-freebsd-amd64
chmod +x /usr/local/bin/diode
```

### 3.9 Verify whatever you installed

```bash
diode --version
# Expect: diode <commit-or-tag> (commit <hash>, built <date>)

sha256sum /usr/local/bin/diode      # Linux / FreeBSD
shasum -a 256 /usr/local/bin/diode  # macOS
Get-FileHash 'C:\Program Files\diode\diode.exe' -Algorithm SHA256  # Windows PowerShell
# Compare against release/SHA256SUMS in the repo.
```

---

## 4. Your first transfer (5 minutes, loopback)

The fastest way to convince yourself it works, on one machine:

```bash
# Terminal 1 — start the receiver
mkdir -p /tmp/diode-in /tmp/diode-spool
diode --mode=rx \
  --listen=127.0.0.1:9999 \
  --files-to=/tmp/diode-in \
  --spool=/tmp/diode-spool

# Terminal 2 — send a file
echo "hello world $(date)" > /tmp/sample.txt
diode --mode=tx \
  --dst=127.0.0.1:9999 \
  --send-file=/tmp/sample.txt

# Check the result
ls -la /tmp/diode-in/
cat /tmp/diode-in/sample.txt
```

You should see your original file in `/tmp/diode-in/`. Stop the
receiver with `Ctrl+C` — its stderr will print a one-line statistics
summary:

```
diode rx: stopped. soh_seen=1 soh_accepted=1 soh_rejected=0 \
soh_for_completed=0 data_frames=1 data_dropped=0 data_dup=0 \
completed=1 hash_mismatch=0 active=0
```

If any of those numbers look wrong (`data_dropped` high,
`hash_mismatch` > 0, `completed=0`), see [§12](#12-troubleshooting-cheat-sheet).

### Same thing with encryption

```bash
# Generate a pre-shared key (32 random bytes, hex-encoded)
diode --mode=psk --file=/tmp/diode.psk
chmod 0400 /tmp/diode.psk
sha256sum /tmp/diode.psk          # record this; both sides MUST match

# Receiver with key enforcement
diode --mode=rx \
  --listen=127.0.0.1:9999 \
  --files-to=/tmp/diode-in \
  --spool=/tmp/diode-spool \
  --key-file=/tmp/diode.psk

# Sender with the same key
diode --mode=tx \
  --dst=127.0.0.1:9999 \
  --send-file=/tmp/sample.txt \
  --key-file=/tmp/diode.psk
```

With `--key-file`, every frame is AEAD-encrypted (AES-256-GCM). The
receiver will silently drop any frame that doesn't decrypt — meaning:
wrong key, no key, tampered frame, or replay.

---

## 5. The five modes

Pick one with `--mode=...`. Every other flag is mode-specific.

| Mode | Purpose | Typical lifetime |
|---|---|---|
| `--mode=tx` | Send a file or byte stream from this host. | seconds-to-minutes per invocation |
| `--mode=rx` | Listen for incoming sessions, write completed files. | long-running daemon |
| `--mode=psk` | Generate a pre-shared key file. | one-shot |
| `--mode=vacuum` | Prune old spool dirs + completed.idx + sender archives. | cron / one-shot |
| `--mode=manifest` | Read the sender's record of sent sessions. | interactive |

### 5.1 `--mode=tx` — send something

Two ways to provide input:

- `--send-file=<path>` — pass a real file; the receiver gets the
  original filename and POSIX permission bits.
- `--in=<path>` (or `--in=-` for stdin) — send arbitrary bytes; the
  receiver gets a stream named `--name=<logical-name>` (default
  `stream.bin`).

The sender exits when it's done blasting frames. There is no
"transfer complete" acknowledgement; UDP doesn't give us one.

### 5.2 `--mode=rx` — receive

Long-running. Listens on `--listen=host:port` for UDP frames. For each
new SOH (start-of-header) frame, opens a per-session directory under
`--spool`. As DATA frames arrive, writes them to the spool dir. When
every chunk is present, verifies the content SHA-256 and **atomically
renames** the assembled file into `--files-to=<dir>` (or copies to
`--out=<path>`).

`Ctrl+C` (SIGINT) or SIGTERM exits cleanly. In-flight sessions are
left on disk — when the next attempt arrives, it will pick up where it
left off (session resume by design).

### 5.3 `--mode=psk` — generate a key

```bash
diode --mode=psk --file=/etc/diode/psk.hex
```

Output is 32 cryptographically random bytes encoded as 64 hex
characters + newline (`--format=hex` is the default). For raw 32-byte
binary use `--format=raw`. Refuses to overwrite an existing file
unless you pass `--force`. See [`psk-howto.md`](psk-howto.md) for
distribution practices.

### 5.4 `--mode=vacuum` — clean up

```bash
sudo diode --mode=vacuum \
  --spool=/var/spool/diode \
  --sender-state=/var/lib/diode/sent \
  --age=1440           # minutes; 1440 = 24 hours
```

`--age=N` is the **age in minutes**. Anything older than `now - N`
gets removed:

- on the **receiver side** (`--spool=...`): abandoned/incomplete
  session dirs *and* entries in `<spool>/completed.idx`.
- on the **sender side** (`--sender-state=...`): archived sessions in
  `<sender-state>/sent/` and rotated manifest entries.

Add `--dry-run` to see what would be removed, `--verbose` to log every
action. Safe to run alongside a live `--mode=rx`.

### 5.5 `--mode=manifest` — inspect what was sent

```bash
diode --mode=manifest                           # human table
diode --mode=manifest --format=json             # JSONL for piping
diode --mode=manifest --since=24h               # last day only
diode --mode=manifest --session-id=<sid>        # one specific session
```

Reads `<sender-state>/manifest.jsonl`. The sender appends one record
per completed send (session id, filename, bytes, sha256, timestamps).
Used as the source of truth for `--resend`.

---

## 6. Every flag, explained

For each flag: what it does, what it defaults to, and when to
change it.

### 6.1 `--mode=tx` flags

#### Network

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--dst=<host>:<port>` | (required) | Where to send frames. Hostname or literal IPv4/IPv6 address. | Always set. Use literal IP if DNS may be flaky during transfer. |
| `--rate=<bytes/sec>` | `0` (unlimited) | Wire-rate cap in **bytes per second**. `0` means "blast as fast as the kernel lets us." | Set when the receiver is the bottleneck or you share the link with other services. Conservative: 80% of link capacity. |
| `--chunk-size=<bytes>` | `1400` | Payload bytes per UDP datagram (before our header + IP/UDP headers). | Lower it if the path MTU is < 1500. Safe rule: `--chunk-size = path_MTU − 72` (keyed) or `path_MTU − 88` (unkeyed). See [§7.4](#74-non-default-path-mtu). |

#### Input

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--send-file=<path>` | (one of `--send-file`/`--in`/`--resend*` is required) | Send a real file; receiver gets the original basename + mode bits. | Default for file transfer. |
| `--in=<path-or-->` | — | Send arbitrary bytes; `-` reads stdin. | Pipelines, programmatic input, no real file. |
| `--name=<string>` | `stream.bin` | Logical filename to advertise when using `--in`. | Always set when piping non-trivial data; helps the receiver organize. |
| `--max-bytes=<N>` | `4 GiB` | Abort if the input exceeds N bytes. Safety belt against a runaway producer. | Bump for large transfers (e.g., `--max-bytes=64GB`). Currently the entire file is buffered in memory — see [§7.3](#73-large-files). |

#### Security

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--key-file=<path>` | (none — unkeyed mode) | Pre-shared key for AES-256-GCM AEAD. Every frame is encrypted + authenticated. | **Always** in production unless the wire is physically isolated. |

#### Reliability

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--fec-group-size=<K>` | `0` (off) | Forward error correction: per K data chunks, send 1 XOR-parity chunk. Tolerates 1 lost chunk per group at +1/K bandwidth. | Set to `8` on any non-ideal link. Set to `4` on lossy links (>1% loss). |
| `--redundancy=<N>` | `1` | Ship each DATA frame N times. Brute-force loss tolerance at N× bandwidth cost. | Only when FEC isn't enough (extreme lossy links). Try `--redundancy=2` before going higher. |
| `--redundancy-order=spread\|consecutive` | `spread` | `spread` round-robins each copy (survives burst loss); `consecutive` sends N copies back-to-back (saves CPU). | Leave at `spread` unless you've measured. |
| `--soh-redundancy=<N>` | `3` | Ship the SOH (start-of-header) frame N times at the beginning. SOH loss = the whole session is dropped. | Higher on a known-lossy first-hop. |
| `--soh-interval=<N>` | `256` | Re-emit SOH every N DATA frames so late-joining receivers can resume. `0` = off. | Set to `0` when you know the receiver is always up. Higher (e.g. `1024`) on long transfers to save bandwidth. |

#### State / resend

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--sender-state=<dir>` | OS-dependent (`~/.diode` on Unix, `%APPDATA%\diode` on Windows) | Where archived sessions and `manifest.jsonl` live. | Production: pin to a stable dir (`/var/lib/diode`) so root and other users see the same archive. |
| `--manifest-rotate-bytes=<N>` | `0` (never) | Roll over `manifest.jsonl` when it reaches N bytes. | Always set in production (e.g., `10000000` for 10 MB) so the manifest doesn't grow without bound. |
| `--session-id=<uuid>` | (random) | Force a specific session id (hex UUID, with or without dashes). | Diagnostics; deterministic tests. Mutually exclusive with `--resend`. |
| `--resend=<sid>` | — | Re-ship a previously archived session by its id (looks it up in manifest). | Receiver missed a transfer; you want to replay it. |
| `--resend-latest=<basename>` | — | Re-ship the most recent session whose filename matches. Easier than copying the sid. | Same scenario as `--resend`, when you don't remember the sid. |

### 6.2 `--mode=rx` flags

#### Network

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--listen=<host>:<port>` | (required) | Bind address. **Always use an explicit IPv4 address**, never bare `:9999`. | Always set explicitly. |
| `--buffer-len=<bytes>` | `4 MiB` | UDP socket read buffer. Capped by the kernel's `net.core.rmem_max`. | Bump to 16 MiB+ for sustained > 100 Mbps. Bump `rmem_max` too (see [§8.1](#81-linux)). |

#### Output

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--files-to=<dir>` | (one of `--files-to`/`--out` required) | Directory where completed files are atomically renamed in. | Default for normal file-shipping. |
| `--out=<path-or-->` | — | Append every completed session to this path (`-` = stdout). | When the receiver is feeding a pipeline (syslog ingest, journald, etc.). |

#### Security

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--key-file=<path>` | (none — accepts plaintext) | When set, **only** AEAD-encrypted frames are accepted. Unkeyed senders are silently dropped. | **Always** in production. Match the sender's PSK byte-for-byte. |

#### Spool layout

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--spool=<dir>` | `/var/spool/diode` (Unix) / `%TEMP%\diode` (Windows) | Per-session staging directory parent. | Pin to a fast disk with adequate free space. See [`sizing-guide.md`](sizing-guide.md) §5. |
| `--spool-mode=sparse\|files` | `sparse` | `sparse` = one `data.partial` + bitmap per session (default, efficient). `files` = one file per chunk (debug-friendly, inode-heavy). | Leave at `sparse` in production. |
| `--max-concurrent=<N>` | `0` (unlimited) | Hard cap on simultaneous in-flight sessions. | Set on shared hosts (e.g. `100`) to bound RAM use. |

#### Replay protection

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--completed-cache=<N>` | `1024` | In-RAM cache of recently completed session ids; resends of completed sessions get dropped rather than re-delivered. | Leave alone unless you have very high session churn (millions/day). |
| `--completed-cache-disk=true\|false` | `true` | Persist completed ids to `<spool>/completed.idx` so the cache survives restart. | Leave on. Turning off re-opens the replay window after every restart. |
| `--completed-cache-disk-cap=<N>` | `100000` | Max records to load from disk at startup. Older records remain on disk for `--mode=vacuum`. | Bump if your replay window is intentionally longer than `100k × --vacuum-age`. |

#### Housekeeping

| Flag | Default | Plain-English meaning | When to change |
|---|---|---|---|
| `--vacuum-interval=<duration>` | `0` (off) | If > 0, prune old spool sessions + `completed.idx` entries every N. | Set to `1h` for any long-running daemon — saves you from writing a cron entry. |
| `--vacuum-age=<duration>` | `24h` | Maximum age before the vacuum prunes. | Lower (e.g., `6h`) on small spools. Higher only when you need a longer replay window. |

### 6.3 `--mode=psk` flags

| Flag | Default | Meaning |
|---|---|---|
| `--file=<path>` | (required) | Where to write the new key. Refuses to overwrite an existing file. |
| `--format=hex\|raw` | `hex` | `hex` = 64 hex chars + newline (recommended). `raw` = 32 binary bytes. |
| `--bytes=<N>` | `32` | Key length in bytes. Minimum 32; AES-256 uses 32. |
| `--force` | `false` | Overwrite the destination file if it already exists. **Use with caution** — this discards the old key. |

### 6.4 `--mode=vacuum` flags

| Flag | Default | Meaning |
|---|---|---|
| `--age=<minutes>` | (required) | Age cutoff in **minutes**. Items older than `now − age` get removed. |
| `--spool=<dir>` | (empty = skip) | Receiver spool dir to clean. |
| `--sender-state=<dir>` | (empty = skip) | Sender archive dir to clean. |
| `--dry-run` | `false` | Print what would be removed, do nothing. **Always run with `--dry-run` first** on a new system. |
| `--verbose` | `false` | Print every action. |

### 6.5 `--mode=manifest` flags

| Flag | Default | Meaning |
|---|---|---|
| `--sender-state=<dir>` | OS-default | Where to read `manifest.jsonl` from. |
| `--format=table\|json\|tsv` | `table` | Output format. `json` = raw JSONL (one record per line, machine-parseable). `tsv` = tab-separated. |
| `--since=<duration>` | (no filter) | Only show records younger than this (e.g. `--since=24h`, `--since=7d`). |
| `--session-id=<sid>` | (no filter) | Only show records matching this session id. |

---

## 7. Best-practice configurations by scenario

Each preset is a known-good starting point. Tune from there.

### 7.1 Logs / SIEM ingest (high session count, small messages)

```bash
# rx (high-side ingest box)
diode --mode=rx \
  --listen=10.20.30.40:9999 \
  --files-to=/var/diode/logs \
  --spool=/var/spool/diode \
  --key-file=/etc/diode/psk.hex \
  --vacuum-interval=15m --vacuum-age=2h \
  --completed-cache=8192 \
  --buffer-len=16777216

# tx (per-log-producer)
diode --mode=tx \
  --dst=10.20.30.40:9999 \
  --key-file=/etc/diode/psk.hex \
  --in=- --name="$(hostname)-syslog-$(date +%Y%m%d-%H%M%S).log" \
  --fec-group-size=8 --soh-redundancy=5 \
  --manifest-rotate-bytes=10000000
```

### 7.2 Batch file transfers (moderate count, large files)

```bash
# rx
diode --mode=rx \
  --listen=10.20.30.40:9999 \
  --files-to=/srv/incoming \
  --spool=/var/spool/diode \
  --key-file=/etc/diode/psk.hex \
  --vacuum-interval=1h --vacuum-age=24h \
  --buffer-len=33554432

# tx (per file, or in a script loop)
diode --mode=tx \
  --dst=10.20.30.40:9999 \
  --key-file=/etc/diode/psk.hex \
  --send-file=/exports/dump-$(date +%Y%m%d).tar.gz \
  --fec-group-size=8 --rate=200000000 \
  --max-bytes=10737418240 \
  --manifest-rotate-bytes=10000000
```

### 7.3 Large files (single file > 4 GiB)

The sender currently loads the input into memory. For a 16 GiB file
you need 16+ GiB RAM on the sender. Workaround: tar and pipe.

```bash
tar -cf - /path/to/big-dir | \
  diode --mode=tx --dst=10.20.30.40:9999 \
        --key-file=/etc/diode/psk.hex \
        --in=- --name=big-dir-$(date +%Y%m%d).tar \
        --max-bytes=53687091200    # 50 GiB cap
```

(Streaming sessions without full-buffer is on the roadmap.)

### 7.4 Non-default path MTU

Use this whenever the path involves a VPN, tunnel, PPPoE, mobile
broadband, or any cloud-egress fabric that may shrink the MTU.

**First, measure the actual MTU:**

```bash
# Linux:    ping -M do -s <payload> <rx-host>
# Windows:  ping -f -l <payload> <rx-host>
# Increase <payload> until the ping fails. Largest success + 28 = path MTU.
ping -M do -s 1372 10.20.30.40       # 1372 + 28 = 1400 MTU? success = yes
ping -M do -s 1452 10.20.30.40       # 1452 + 28 = 1480 MTU? success = yes
```

**Then set `--chunk-size` accordingly:**

| Path MTU | Safe `--chunk-size` (keyed) | Safe `--chunk-size` (unkeyed) |
|---|---|---|
| 1500 | `1400` (default) | `1400` (default) |
| 1492 (PPPoE) | `1392` | `1376` |
| 1450 (some VPNs) | `1350` | `1334` |
| 1400 | `1300` (use this if uncertain) | `1280` |
| 1280 (WireGuard) | `1200` | `1184` |

Formula: `chunk_size_max = path_MTU − 56 − trailer` where trailer is
16 bytes (keyed) or 32 bytes (unkeyed).

### 7.5 Unkeyed transfer (physically isolated wire only)

If the wire is physically point-to-point or otherwise inaccessible:

```bash
# rx
diode --mode=rx --listen=10.20.30.40:9999 --files-to=/srv/incoming \
  --spool=/var/spool/diode --vacuum-interval=1h
# tx
diode --mode=tx --dst=10.20.30.40:9999 --send-file=/tmp/payload.bin
```

If anyone with network access can reach the receiver's port, they can
inject valid-looking frames. **Don't run unkeyed** unless the wire
itself is your perimeter.

### 7.6 OT/SCADA telemetry (resource-constrained sender)

```bash
diode --mode=tx \
  --dst=10.50.60.70:9999 \
  --key-file=/etc/diode/psk.hex \
  --in=- --name="plc-1-$(date +%Y%m%d).log" \
  --chunk-size=512 \           # small datagrams; fits weird embedded MTUs
  --fec-group-size=4 \         # heavier FEC for lossy radio links
  --soh-redundancy=10 \        # SOH loss is fatal; over-insure
  --rate=1000000               # 1 Mbps cap so we don't swamp the radio
```

---

## 8. Per-platform best practices

### 8.1 Linux

**Kernel tuning** (do this **once** on the receiver host):

```bash
sudo tee /etc/sysctl.d/99-diode.conf <<EOF
# Allow our 16+ MiB UDP receive buffer.
net.core.rmem_max = 33554432
net.core.rmem_default = 16777216
# Deeper backlog for high-pps workloads.
net.core.netdev_max_backlog = 5000
EOF
sudo sysctl --system
```

**Dedicated user + dirs**:

```bash
sudo useradd --system --no-create-home --shell /sbin/nologin diode
sudo install -d -m 0750 -o diode -g diode \
  /var/spool/diode /var/lib/diode /srv/incoming
sudo install -m 0400 -o diode -g diode /etc/diode/psk.hex /etc/diode/psk.hex
```

**Firewall** (nftables example, receiver side):

```bash
sudo nft add table inet diode
sudo nft add chain inet diode input '{ type filter hook input priority 0; }'
sudo nft add rule inet diode input udp dport 9999 accept
# Egress block — the diode property at the kernel level:
sudo nft add chain inet diode output '{ type filter hook output priority 0; }'
sudo nft add rule inet diode output meta skuid diode drop
```

**Preflight before declaring the host ready**:

```bash
sudo scripts/preflight.sh --role=rx --psk-file=/etc/diode/psk.hex
```

### 8.2 Windows

Windows UDP performance per single process is materially weaker than
Linux. Plan around it.

**Bigger UDP buffers** (PowerShell as Administrator):

```powershell
# Per-NIC RSC / receive scaling tweaks (Server SKUs; safe-no-op on Client):
Set-NetAdapterRsc -Name 'Ethernet' -IPv4Enabled $true -ErrorAction SilentlyContinue
Set-NetAdapterRss -Name 'Ethernet' -Enabled $true -ErrorAction SilentlyContinue

# Increase Windows UDP receive buffer ceiling. (No exact equivalent of
# rmem_max — Windows tunes per-socket; we pass --buffer-len up to ~16 MiB.)
diode --mode=rx ... --buffer-len=16777216
```

**Dirs + permissions**:

```powershell
# Drop the binary's run privileges if possible. Default install uses
# Local System; for production, create a dedicated service account
# (see §9.2) and grant it read on the PSK + write on spool/files-to.

New-Item -ItemType Directory -Path 'C:\diode\spool', 'C:\diode\inbox' -Force
$acl = Get-Acl 'C:\diode\psk.hex'
$acl.SetAccessRuleProtection($true, $false)
$acl.Access | ForEach-Object { $acl.RemoveAccessRule($_) | Out-Null }
$acl.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule(
  'DOMAIN\diode-svc', 'Read', 'Allow')))
Set-Acl 'C:\diode\psk.hex' $acl
```

**Firewall**:

```powershell
New-NetFirewallRule -DisplayName 'DataDiode rx UDP/9999' `
  -Direction Inbound -Action Allow -Protocol UDP -LocalPort 9999

# Egress block for the diode service account (the diode invariant at OS-level):
New-NetFirewallRule -DisplayName 'DataDiode rx EGRESS BLOCK' `
  -Direction Outbound -Action Block -Service 'diode-rx'
```

**MTU caveats**:

- Default Ethernet MTU on Windows is **1500** (same as Linux).
- VPN clients (AnyConnect, Always-On VPN, WireGuard, OpenVPN) commonly
  drop the effective path MTU to 1400 or lower.
- Mobile broadband adapters often present 1400 MTU.
- Always measure with `ping -f -l <payload> <rx>` before committing
  `--chunk-size`.

**Antivirus / defender**:

- Add `C:\diode\inbox\*` and `C:\diode\spool\*` to **real-time scan
  exclusions**. AV scanning every chunk write demolishes throughput.
- Add `diode.exe` to **process exclusions** if you see CPU spent in
  the AV driver during transfers.

### 8.3 macOS

```bash
# Bigger UDP receive buffer
sudo sysctl -w kern.ipc.maxsockbuf=33554432
sudo sysctl -w net.inet.udp.recvspace=16777216

# Persist:
echo 'kern.ipc.maxsockbuf=33554432' | sudo tee -a /etc/sysctl.conf
echo 'net.inet.udp.recvspace=16777216' | sudo tee -a /etc/sysctl.conf
```

macOS is fine as a sender; not a great rx host for production (no
mature service manager beyond launchd, App Store sandboxing
restrictions on listening sockets at certain ports). Use it for
developer workstations, not production receivers.

### 8.4 FreeBSD

```sh
# Persistent kernel tuning
sudo tee -a /etc/sysctl.conf <<EOF
kern.ipc.maxsockbuf=33554432
net.inet.udp.recvspace=16777216
net.inet.udp.maxdgram=65535
EOF
sudo service sysctl restart
```

FreeBSD's `pf` firewall:

```
pass in quick on em0 proto udp from any to (em0) port 9999
block out quick from diode-uid
```

---

## 9. Running as a service

### 9.1 Linux — systemd

Create `/etc/systemd/system/diode-rx.service`:

```ini
[Unit]
Description=DataDiode receiver
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=diode
Group=diode
ExecStart=/usr/local/bin/diode \
  --mode=rx \
  --listen=10.20.30.40:9999 \
  --files-to=/srv/incoming \
  --spool=/var/spool/diode \
  --key-file=/etc/diode/psk.hex \
  --vacuum-interval=1h --vacuum-age=24h \
  --buffer-len=16777216
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal

# Hardening
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
ReadWritePaths=/srv/incoming /var/spool/diode
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
```

Enable + start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now diode-rx
sudo systemctl status diode-rx
journalctl -u diode-rx -f
```

### 9.2 Windows — service (via `sc.exe` or NSSM)

Quickest (no extra tools), uses Windows' built-in service host:

```powershell
sc.exe create diode-rx binPath= "C:\Program Files\diode\diode.exe --mode=rx --listen=10.20.30.40:9999 --files-to=C:\diode\inbox --spool=C:\diode\spool --key-file=C:\diode\psk.hex --vacuum-interval=1h --vacuum-age=24h --buffer-len=16777216" start= auto displayName= "DataDiode Receiver"
sc.exe failure diode-rx reset= 60 actions= restart/5000
Start-Service diode-rx
Get-Service diode-rx
```

For better lifecycle (graceful stop, log rotation), use NSSM:

```powershell
choco install nssm
nssm install diode-rx 'C:\Program Files\diode\diode.exe'
nssm set diode-rx AppParameters '--mode=rx --listen=10.20.30.40:9999 --files-to=C:\diode\inbox --spool=C:\diode\spool --key-file=C:\diode\psk.hex --vacuum-interval=1h --vacuum-age=24h --buffer-len=16777216'
nssm set diode-rx AppStdout C:\diode\logs\rx.out.log
nssm set diode-rx AppStderr C:\diode\logs\rx.err.log
nssm set diode-rx AppRotateBytes 10485760
nssm start diode-rx
```

### 9.3 macOS — launchd

`/Library/LaunchDaemons/com.example.diode-rx.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>           <string>com.example.diode-rx</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/diode</string>
    <string>--mode=rx</string>
    <string>--listen=10.20.30.40:9999</string>
    <string>--files-to=/srv/incoming</string>
    <string>--spool=/var/spool/diode</string>
    <string>--key-file=/etc/diode/psk.hex</string>
    <string>--vacuum-interval=1h</string>
  </array>
  <key>RunAtLoad</key>       <true/>
  <key>KeepAlive</key>       <true/>
  <key>StandardOutPath</key> <string>/var/log/diode-rx.out</string>
  <key>StandardErrorPath</key> <string>/var/log/diode-rx.err</string>
</dict>
</plist>
```

```bash
sudo launchctl load -w /Library/LaunchDaemons/com.example.diode-rx.plist
```

### 9.4 FreeBSD — rc.d

`/usr/local/etc/rc.d/diode-rx`:

```sh
#!/bin/sh
# PROVIDE: diode_rx
# REQUIRE: NETWORKING
. /etc/rc.subr
name=diode_rx
rcvar=diode_rx_enable
load_rc_config $name
: ${diode_rx_enable:=NO}
command=/usr/local/bin/diode
command_args="--mode=rx --listen=10.20.30.40:9999 --files-to=/srv/incoming --spool=/var/spool/diode --key-file=/etc/diode/psk.hex --vacuum-interval=1h"
run_rc_command "$1"
```

```sh
sudo sysrc diode_rx_enable=YES
sudo service diode_rx start
```

---

## 10. Security best practices

1. **Always use `--key-file` in production.** The threat model treats
   unkeyed mode as a developer convenience. Generate a 32-byte key
   per deployment (`diode --mode=psk --file=…`).
2. **PSK file permissions.** Owner-only readable: `chmod 0400`
   (Unix); on Windows, ACL with only the service account allowed.
3. **PSK distribution out-of-band.** USB, scp from a jump host,
   HashiCorp Vault, SOPS, whatever your shop uses. **Never** send
   the PSK through the diode itself.
4. **Verify the PSK SHA256 on both sides** before declaring deploy
   complete. `sha256sum /etc/diode/psk.hex` should produce the same
   string on both hosts.
5. **PSK rotation = coordinated stop-restart.** No overlap window
   today. See [runbook 4](runbooks.md#4-psk-rotation-across-both-sides).
6. **Firewall the receiver outbound.** Even though the binary opens
   no outbound sockets, defense in depth: explicit egress block on
   the rx host (see §8.1 nftables example).
7. **Keep the receiver host minimal.** Anything else listening on
   the host is a potential pivot if compromised. Pin OS package set
   to the minimum for receiver function.
8. **Bind explicitly.** `--listen=10.20.30.40:9999`, never `:9999`.
   Go binds dual-stack on some kernels; you'll surprise yourself.
9. **Audit the spool dir periodically.** Old in-flight session dirs
   may contain partial sensitive data. Vacuum is the safety net;
   you should not rely on it alone for sensitive payloads.

---

## 11. Day-2 operations

### 11.1 Health check

```bash
# Linux/systemd
systemctl is-active diode-rx && journalctl -u diode-rx --since '5 min ago' | tail
ss -ulnp | grep diode               # is it bound?
ls /var/spool/diode/ | wc -l        # in-flight session count
du -sh /var/spool/diode/            # disk usage
```

### 11.2 Reading the stats line

The rx prints one line to stderr on shutdown (and SIGUSR1 in future
versions). Decoded:

| Field | Meaning |
|---|---|
| `soh_seen` | SOH frames the rx looked at, total. |
| `soh_accepted` | SOH frames that became open sessions. |
| `soh_rejected` | SOH frames dropped (bad signature, bad version, malformed). |
| `soh_for_completed` | SOH for a session id we've already completed (replay-protected drop). |
| `data_frames` | DATA frames processed. |
| `data_dropped` | DATA frames dropped (no matching session, late, full, decoding failed). |
| `data_dup` | DATA frames where the chunk slot was already filled. |
| `completed` | Sessions reassembled successfully. |
| `hash_mismatch` | Sessions reassembled but content SHA failed. **Should always be 0** in production. |
| `active` | Sessions still in-flight at shutdown. |

### 11.3 Reading the sender's manifest

```bash
diode --mode=manifest --since=24h
# session_id                              filename                    bytes      sha256                                completed_at
# 7e15…b3                                  payroll-2026-06-22.tar.gz   24576123   3f1a…                                  2026-06-22T10:42:01Z

# Find a specific transfer
diode --mode=manifest --session-id=7e15...
```

### 11.4 Vacuum schedule

Either run the in-process vacuum loop:

```
diode --mode=rx ... --vacuum-interval=1h --vacuum-age=24h
```

Or cron it:

```cron
# /etc/cron.d/diode
0 * * * * diode /usr/local/bin/diode --mode=vacuum --spool=/var/spool/diode --age=1440
30 * * * * diode /usr/local/bin/diode --mode=vacuum --sender-state=/var/lib/diode --age=10080
```

(`1440` min = 24h, `10080` min = 7 days for the sender archive.)

### 11.5 Resend a missed transfer

```bash
# Find the session
diode --mode=manifest --since=24h | grep payroll

# Resend by id
diode --mode=tx --resend=<sid> --dst=10.20.30.40:9999 --key-file=/etc/diode/psk.hex

# Or resend the most recent transfer of a given filename
diode --mode=tx --resend-latest=payroll-2026-06-22.tar.gz \
  --dst=10.20.30.40:9999 --key-file=/etc/diode/psk.hex
```

---

## 12. Troubleshooting cheat sheet

| Symptom | First check | Likely fix |
|---|---|---|
| Sender exits cleanly, receiver gets nothing | `tcpdump -ni any udp port 9999 -c 5` on rx host | Firewall blocks UDP; or `--listen` bound to wrong IP. |
| `data_dropped` > 0 and rising | `cat /proc/net/snmp \| grep ^Udp:` (RcvbufErrors) | Raise `net.core.rmem_max` and pass bigger `--buffer-len`. |
| `data_dropped` after every transfer, even tiny ones | FEC is on | Normal — those are late-arriving parity chunks. Ignore. |
| `hash_mismatch=1` | (should never happen) | Wire-level corruption past the AEAD or unkeyed-mode header damage. Treat as a bug; collect tcpdump. |
| Sender hangs forever | `--rate` is set too low for input size | Raise `--rate` or remove it. |
| Receiver dies on startup with `address already in use` | `ss -ulnp \| grep <port>` | Old rx still running; kill it or change port. |
| New rx version refuses everything | tcpdump shows traffic, rx stats show `soh_seen=0` | Wire-protocol-version mismatch with the sender (see [runbook 5](runbooks.md#5-receiver-wont-start-after-an-upgrade)). |
| `Permission denied` on spool dir | `ls -ld /var/spool/diode` | `chown diode:diode /var/spool/diode`. |
| Windows transfer slow (< 100 Mbps) | per-process UDP perf on Windows is weak | Bump `--buffer-len`; consider parallel rx processes on different ports. |
| Path MTU is < 1500 | `ping -M do -s 1472 <rx>` fails | Set `--chunk-size` per [§7.4](#74-non-default-path-mtu). |

Run the preflight script before suspecting a binary bug:

```bash
scripts/preflight.sh --role=rx --psk-file=/etc/diode/psk.hex      # Linux/macOS/FreeBSD
bash scripts/preflight.sh --role=rx --psk-file=C:\diode\psk.hex   # Windows (Git Bash / WSL)
```

---

## 13. Glossary

- **AEAD** — Authenticated Encryption with Associated Data. Our wire
  crypto. Specifically AES-256-GCM. Encrypts and authenticates in one
  pass; rejects any tampered frame.
- **Chunk** — one fixed-size piece of payload. Default 1400 B.
- **Datagram** — one UDP packet on the wire.
- **DATA frame** — a wire frame carrying a single chunk of the payload.
- **FEC** — Forward Error Correction. We use XOR parity per group of
  K chunks; tolerates 1 lost chunk per group.
- **MTU** — Maximum Transmission Unit. The biggest IP packet that
  fits on a given network path without fragmentation. Standard
  Ethernet is 1500 bytes; tunnels often reduce it.
- **PSK** — Pre-Shared Key. A 32-byte secret used to derive an AES
  key, distributed out-of-band, identical on tx and rx.
- **Session** — one logical transfer (one file or one byte stream),
  identified by a 128-bit `sid` and bracketed by a SOH at the start
  and (implicitly) the last DATA frame.
- **SOH frame** — Start-Of-Header frame: announces a session
  (filename, size, chunk count, content SHA-256, FEC params).
- **Spool dir** — `--spool=<dir>` on the rx side; per-session
  staging area where chunks land before the assembled file is
  atomically renamed into `--files-to=<dir>`.
- **Vacuum** — periodic pruning of stale spool dirs, completed-cache
  entries, and sender archives. Either built-in (`--vacuum-interval`)
  or via `--mode=vacuum`.
- **Wire-protocol version** — incrementing byte in every frame
  header. The current is v4. A receiver that doesn't recognize the
  version silently drops the frame. Coordinate upgrades.

---

## What's next

- Run through [§4](#4-your-first-transfer-5-minutes-loopback) once.
- Read the [tutorial](tutorial.md) for a deeper conceptual walk.
- Bookmark the [runbooks](runbooks.md) — your incident playbook.
- Print the table in [§6](#6-every-flag-explained) for your wall.

When something is wrong: **start with the preflight script**, then
the troubleshooting table, then the runbooks. The binary almost never
lies — when the stats line and the kernel counters disagree, it's
always the environment.
