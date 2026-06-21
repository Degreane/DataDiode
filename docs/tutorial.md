# DataDiode Tutorial — How It Works, End to End

A practical walk-through. Read it once top-to-bottom; the same numbered
sections double as a step-by-step recipe you can run on your own host.

---

## 1. The 30-second mental model

```
   ┌─────────────┐                                ┌─────────────┐
   │  LOW SIDE   │   bytes flow this way ────►   │  HIGH SIDE  │
   │             │                                │             │
   │  diode      │   (one-way UDP, no return)     │  diode      │
   │  --mode=tx  │ ─────────────────────────────► │  --mode=rx  │
   │             │ ◄═════ NEVER any traffic ═════ │             │
   │             │                                │             │
   │  reads      │                                │  writes     │
   │  stdin/file │                                │  stdout/file│
   └─────────────┘                                └─────────────┘
```

- **Always start RX first.** TX into a closed port = silent loss (UDP doesn't error).
- **No return channel.** TCP needs ACKs; we use UDP precisely *because* it doesn't.
- **One binary, two modes.** `diode --mode=tx` and `diode --mode=rx` are the same executable selecting a code path at startup.
- **Receiver opens no outbound sockets.** Enforced by the type system (reflection test) and by host firewall rules.

That's the whole product. Everything below is detail.

---

## 2. Following a single byte through the system

A byte typed into `stdin` on the low side and printed to `stdout` on the high side takes this journey:

```
1. low-side stdin
       │
       ▼
2. diode --mode=tx reads up to --chunk bytes from stdin
       │
       ▼
3. internal/framing.Encode wraps the chunk in a 26-byte header + SHA-256
       │   header: magic="DDO\0" | ver=01 | flags | seq | msg_id |
       │           chunk_index | chunk_total | payload_len
       │   payload: the bytes themselves (≤1400 B)
       │   trailer: SHA-256(header+payload)
       ▼
4. internal/transport/udp.Sender.Send writes the frame as ONE UDP datagram
       │
       ▼   ╔════════════════════════════════════════════════════╗
       │   ║  the wire — could be loopback, an LXC veth pair,   ║
       │   ║  a real ethernet, or a hardware diode appliance    ║
       │   ╚════════════════════════════════════════════════════╝
       ▼
5. internal/transport/udp.Receiver reads the datagram with ReadFromUDP
       │   (peer address is intentionally discarded)
       ▼
6. internal/framing.Decode validates: length, magic, version, flags,
       │  chunk fields, SHA-256. Bad frame → silently dropped.
       ▼
7. internal/reassembly.Reassembler buffers the chunk under (msg_id, chunk_index)
       │   - if it's a HEARTBEAT: counted and discarded
       │   - if REDUNDANT and msg_id was recently delivered: deduped
       │   - if chunk_total chunks now present: concatenate and deliver
       ▼
8. The reassembler calls deliver(payload) → writes payload to --out
       │   (--out=- is stdout; otherwise the file is opened O_APPEND)
       ▼
9. high-side stdout / file
```

When the message is bigger than `--chunk` bytes, steps 3-7 repeat for each chunk, and step 7 only fires `deliver()` when the *last* chunk arrives. Out-of-order arrival is fine (UDP can reorder; the reassembler doesn't care).

---

## 3. Prerequisites

For Section 4 (single-host loopback), all you need is **Go**:

```bash
sudo dnf install -y golang        # Fedora
# or
sudo apt install -y golang        # Debian/Ubuntu
```

For Section 6 (two real LXC containers), you also need **lxc + nftables**:

```bash
sudo dnf install -y lxc lxc-templates nftables
```

Or run the bundled check, which prints the exact install hint for whatever's missing:

```bash
./scripts/check-prereqs.sh
```

---

## 4. Try it on one host (loopback) — the fastest path

This needs nothing except Go and is the right way to see the diode work end-to-end in 30 seconds.

```bash
# from the repo root
go build -o /tmp/diode ./cmd/diode
```

Open **two terminals**.

### Terminal A — start the receiver FIRST

```bash
/tmp/diode --mode=rx --listen=127.0.0.1:9999 --out=- --delimiter=$'\n---\n'
```

You'll see:

```
diode rx: listening on 127.0.0.1:9999, writing to stdout
```

The receiver is now bound and waiting. It does NOT open any outbound socket.

### Terminal B — send something

```bash
echo "hello from the low side" | /tmp/diode --mode=tx --dst=127.0.0.1:9999
```

In **Terminal A** you should immediately see:

```
hello from the low side
---
```

The delimiter (`\n---\n`) is appended after each delivered message so you can tell them apart.

### Send a few more

```bash
echo "second message" | /tmp/diode --mode=tx --dst=127.0.0.1:9999
cat /etc/hostname | /tmp/diode --mode=tx --dst=127.0.0.1:9999
```

Each one appears in Terminal A, separated by `---`.

### Send a big file (multi-chunk)

```bash
cat README.md | /tmp/diode --mode=tx --dst=127.0.0.1:9999 --chunk=400
```

Even though the file gets split into many UDP datagrams, the receiver buffers them and emits the whole file as one delivered message (followed by the `---` delimiter).

### Stop the receiver

In Terminal A, press **Ctrl-C**. The receiver prints its stats line and exits:

```
diode rx: stopped. frames_in=42 frames_dup=0 frames_ignored=0 msgs_delivered=4 msgs_evicted=0 bytes_pending=0
```

`frames_in` = total UDP datagrams accepted. `msgs_delivered` = completed messages. They differ because big messages are many frames.

---

## 5. What each flag does

### `diode --mode=tx`

| Flag | Default | What it means |
|---|---|---|
| `--dst` | (required) | `host:port` of the receiver |
| `--chunk` | 1400 | Payload bytes per UDP datagram. Cap at 1400 to stay under Ethernet MTU and avoid IP fragmentation. |
| `--rate` | 0 (unlimited) | Wire rate cap in **bytes/sec**. Token-bucket, smoothed over 1 s. |
| `--redundancy` | 1 | Send each frame N times. Copies 2..N carry the REDUNDANT flag; the receiver dedupes. Buys loss tolerance at the cost of N× bandwidth. |
| `--in` | `-` (stdin) | Input file path; `-` means stdin |
| `--max-message` | 64 MiB | Hard cap on a single tx invocation's payload. Aborts cleanly if exceeded. |

### `diode --mode=rx`

| Flag | Default | What it means |
|---|---|---|
| `--listen` | (required) | Bind address: `host:port` or `:port` for all interfaces |
| `--out` | `-` (stdout) | Output sink; files are opened **O_APPEND** (each delivery appended, never truncates) |
| `--max-pending` | 1024 | Max incomplete messages held in reassembly. Oldest evicted on overflow. |
| `--max-bytes` | 64 MiB | Max aggregate bytes in incomplete messages. Oldest evicted on overflow. |
| `--recent-msg-cache` | 1024 | Size of recently-delivered MsgID cache (catches REDUNDANT copies arriving after delivery). |
| `--buffer-len` | 1458 | UDP read buffer size. Must be ≥ `framing.MaxFrameLen`. |
| `--delimiter` | (empty) | String appended after each delivered message |

### Global

| Flag | Effect |
|---|---|
| `--mode=tx` / `--mode=rx` | Required; selects the code path |
| `--help` | Top-level usage; or mode-specific (`diode --mode=tx --help`) |
| `--version` | Print version and exit |

---

## 6. Try it across two real LXC containers — the production-like demo

This is what `scripts/demo.sh` automates. Read this section to understand
what's happening; then run the script.

### 6a. Topology

```
              host (Fedora, plain LXC)
              │
              │  diodebr0 — isolated bridge: 10.99.0.0/24
              │            no NAT, no upstream
              │
      ┌───────┴────────┐               ┌────────────────────┐
      │ LXC: diode-low │               │ LXC: diode-high    │
      │ 10.99.0.10     │   UDP/9999    │ 10.99.0.20         │
      │                │ ─────────────►│                    │
      │ diode --mode=tx│               │ diode --mode=rx    │
      │                │               │                    │
      │ nft: allow OUT │               │ nft: DROP all OUT  │
      │   UDP/9999     │               │ except loopback    │
      │   to .20 only  │               │ (no return path)   │
      └────────────────┘               └────────────────────┘
```

**Two stacked enforcement layers:**
- **Application layer:** `diode --mode=rx` opens no outbound sockets.
- **Network layer:** nftables on the receiver drops everything outbound.

### 6b. Run the full demo (one command)

```bash
sudo ./scripts/demo.sh
```

That single command, in order:

1. **`lxc-setup.sh`** — creates the `diodebr0` bridge, downloads a Fedora
   container template, launches `diode-low` (10.99.0.10) and
   `diode-high` (10.99.0.20). Idempotent: re-runs are no-ops.

2. **`lxc-push.sh`** — builds the `diode` binary (CGO disabled = single
   static executable) and atomically pushes it into both containers'
   `/usr/local/bin/diode`. Uses SHA-256 to skip pushes when the
   in-container binary is already current.

3. **`lxc-harden.sh`** — applies the nftables rules above. Inside
   `diode-high` the chain is `policy drop; allow lo only`.

4. **Stages the payload** — copies `docs/sprints/sprint-01-mvp.md` into
   `/tmp/diode-payload.in` inside `diode-low`.

5. **Starts the receiver** — `lxc-attach diode-high` runs
   `diode --mode=rx --listen=0.0.0.0:9999 --out=/tmp/diode-sink.out`
   in the background and waits for the "listening on" banner.

6. **Runs the sender** — `lxc-attach diode-low` pipes the payload
   through `diode --mode=tx --dst=10.99.0.20:9999 --chunk=1400 --redundancy=2`.

7. **Verifies** — copies the sink file out of `diode-high`'s rootfs and
   compares SHA-256 with the original payload.

8. **Prints the receiver's stats line** so you can see frames in / dup
   counts.

Expected final output:

```
== 9. Verify ==
[demo] input  sha256 = a1b2…  (4321 bytes)
[demo] output sha256 = a1b2…  (4321 bytes)
[demo] MATCH — diode roundtrip succeeded across diode-low → diode-high
```

### 6c. What to look for if you're suspicious

In one terminal, while the demo runs, watch the wire:

```bash
sudo tcpdump -i diodebr0 -n udp port 9999
```

You will see packets only `10.99.0.10.* > 10.99.0.20.9999` — never the
reverse. That's the diode property visible at the network layer.

Inside the receiver, list the firewall:

```bash
sudo lxc-attach -n diode-high -- nft list table inet diode
```

You'll see `policy drop` on the output chain, `oifname "lo" accept` and nothing else.

### 6d. Tear down

```bash
sudo ./scripts/lxc-teardown.sh
```

Removes both containers and `diodebr0`. Set `KEEP_BRIDGE=1` to leave the
bridge in place if you share it with other projects.

---

## 7. The on-wire frame format (a paragraph)

If you ever need to debug what's on the wire, this is what one UDP datagram
looks like (locked by [ADR-0002](architecture/ADR-0002-frame-format.md)):

```
offset  size  field
   0     4    magic  = "DDO\0"           ← sanity check, reject garbage in 1 branch
   4     1    version = 0x01
   5     1    flags  (FINAL|HEARTBEAT|REDUNDANT, reserved bits must be 0)
   6     8    seq           uint64 BE, per-sender monotonic
  14     4    msg_id        uint32 BE, per-message ID; shared by all chunks
  18     2    chunk_index   uint16 BE, 0-based
  20     2    chunk_total   uint16 BE, ≥1
  22     4    payload_len   uint32 BE, ≤1400
  26     N    payload       opaque bytes
  26+N  32    sha256        over bytes [0..26+N)
```

Total frame ≤ 1458 B (under Ethernet MTU with slack for tunneling).

Anything that fails validation is silently dropped — there's no return path to
report errors, so logging at line rate would DoS the operator.

---

## 8. Common operations recipes

### 8a. Diode a syslog stream

Receiver side:

```bash
diode --mode=rx --listen=:9999 --out=/var/log/diode-sink.log --delimiter=$'\n'
```

Sender side (use `logger` or your syslog source):

```bash
# pipe rsyslog imfile output / journald / etc into diode
tail -F /var/log/messages | diode --mode=tx --dst=10.99.0.20:9999 --chunk=1200
```

Each line on the left appears on the right, append-only.

### 8b. Send a file (v2 session protocol)

The sender opens the file, computes its SHA-256 and chunk plan,
generates a UUID session_id, ships an SOH preamble carrying the
metadata, then ships every DATA chunk. The receiver creates a spool
directory per session, writes chunks straight to a sparse file (or
per-chunk files with `--spool-mode=files`), and on completion verifies
the SHA-256 and atomic-renames the assembled file into `--files-to`.

**Receiver** — point at a directory; every file delivered lands there:

```bash
mkdir -p /srv/incoming
diode --mode=rx --listen=:9999 --files-to=/srv/incoming --spool=/var/spool/diode
```

**Sender** — one invocation per file:

```bash
diode --mode=tx --dst=10.99.0.20:9999 --send-file=/path/to/report.pdf
diode --mode=tx --dst=10.99.0.20:9999 --send-file=/path/to/another.tar
```

**Observing in-flight transfers** — every active session has a directory under `--spool`:

```bash
$ ls /var/spool/diode/
3f29b1a2-c8e4-4f1d-9b6a-aeb5e7c84021/

$ cat /var/spool/diode/3f29b1a2-.../meta.json
{
  "session_id":     "3f29b1a2-c8e4-4f1d-9b6a-aeb5e7c84021",
  "version":        "v2",
  "filename":       "report.pdf",
  "mode":           "0o644",
  "total_bytes":    1048576,
  "chunk_size":     1400,
  "chunk_total":    750,
  "content_sha256": "...",
  "started_at":     "2026-06-21T19:42:11Z",
  "spool_mode":     "sparse"
}

$ # count missing chunks (bitmap is one bit per chunk, persisted after every chunk)
$ xxd -p chunks.bitmap | tr -d '\n' | python3 -c "import sys; b=int(sys.stdin.read(),16); print(bin(b).count('1'),'received')"
```

When a session completes successfully, the directory is removed automatically. A session whose SHA-256 verification *fails* is **kept in the spool** for forensic inspection.

**Two spool layouts** (pick via `--spool-mode`):

| Mode | Files per session | Best for |
|---|---|---|
| `sparse` (default) | `meta.json` + `chunks.bitmap` + `data.partial` (one sparse file, chunks pwrite'd at offset) | files of any size, throughput |
| `files` | `meta.json` + `chunks.bitmap` + `chunks/00000.bin`, `00001.bin`, ... | small files, CTF / audit / forensic inspection of individual chunks |

Both modes produce **byte-identical output** on completion.

The receiver logs each delivery:

```
diode rx: wrote /srv/incoming/report.pdf (1234567 bytes, mode 644)
diode rx: wrote /srv/incoming/another.tar (98765432 bytes, mode 600)
```

**Security properties:**
- Receiver rejects filenames containing `/`, `\`, NUL, `.`, or `..` — no path traversal can write outside `--files-to`.
- Receiver writes to a spool dir keyed by session_id, then atomic-renames to `--files-to` only after SHA-256 verification.
- SHA-256 of the content is verified before the rename; a corrupted-in-transit file is dropped (spool retained for inspection), not delivered partial.
- Unknown session_id (DATA frame arrived without its SOH) → dropped at the door with one syscall; no per-chunk decoding.

**For lossy networks**, layer `--redundancy=N` (each DATA frame N times) and `--soh-redundancy=N` (each SOH N times — the SOH is the most expensive frame to lose, since without it the receiver drops every DATA for that session):

```bash
diode --mode=tx --dst=10.99.0.20:9999 --send-file=big.tar --redundancy=3 --soh-redundancy=5
```

`--soh-redundancy` defaults to 3, `--redundancy` to 1.

**For very large files**, the default `--max-bytes=4 GiB` cap can be raised:

```bash
diode --mode=tx --dst=10.99.0.20:9999 --send-file=huge.iso --max-bytes=$((16*1024*1024*1024))
```

The receiver writes chunks straight to disk (sparse mode pwrites at the correct offset), so receiver memory does not grow with file size.

### 8c. Authenticate every frame with a pre-shared key

If anyone other than the sender can reach the receiver's UDP port, they
can forge valid-looking frames (SHA-256 is integrity, not authentication).
Enabling HMAC-SHA256 with a pre-shared key closes that gap. The flag is
**`--key-file=<path>`** on both sides.

**Generate a key** (32 random bytes, hex-encoded) — easiest path:

```bash
diode --mode=psk --file=psk.hex
# prints sha256 of the file to stderr; note it for verification on the other host
```

…or, if you prefer OS-native tools:

```bash
head -c 32 /dev/urandom | xxd -p -c 64 > psk.hex
chmod 600 psk.hex
```

Distribute that file to both hosts via a trusted channel (the diode has no
return path, so there's no in-band key exchange — that's the operator's
problem to solve, e.g., with SSH or sneakernet).

**Receiver** (rejects everything that isn't signed with this key):

```bash
diode --mode=rx --listen=:9999 --files-to=/srv/incoming --key-file=/etc/diode/psk.hex
```

**Sender** (signs every frame):

```bash
diode --mode=tx --dst=10.99.0.20:9999 --send-file=report.pdf --key-file=/etc/diode/psk.hex
```

**What's protected:**
- An attacker on the wire **cannot inject frames** the receiver will accept.
- An attacker **cannot tamper** with in-flight frames (HMAC fails).
- An attacker **cannot downgrade** to unsigned (a keyed receiver rejects unsigned frames; a keyless receiver rejects signed ones — see ADR-0004).

**What's NOT protected:**
- **Confidentiality.** The payload is still plaintext on the wire. Pre-encrypt at the application layer until ADR-0005 ships AES-256-GCM.
- **Replay.** A frame captured today and replayed later within the recently-delivered cache window is caught; outside that window it may re-deliver. Closes with ADR-0007.

**Key file format:** the file may contain either a hex-encoded key (`>=64` hex chars, whitespace ignored) or raw binary bytes (`>=32` bytes). Auto-detected.

**Backward compatibility:** if `--key-file` is omitted on either side, behavior is identical to today. Existing deployments don't need to change.

### 8d. Resend a failed transfer

If a transfer was interrupted (network blip, receiver crash, etc.) the
receiver's spool dir for that session is still on disk with whichever
chunks made it. You can complete the transfer by replaying the **same
session id** — the receiver dedupes via its bitmap and only writes the
missing chunks.

**What was sent** (from the manifest):

```bash
diode --mode=manifest
```

```
COMPLETED_AT (UTC)    SESSION_ID                            FILENAME       BYTES  DEST
----------------------------------------------------------------------------------------
2026-06-21T20:15:23Z  3f29b1a2-c8e4-4f1d-9b6a-aeb5e7c84021  report.pdf   1048576  10.0.0.20:9999
2026-06-21T20:11:07Z  8c1d472f-1ba9-4e2c-9c1f-3b8a2d9f7e10  data.bin       50000  10.0.0.20:9999
```

**Resend by session id** (uses the archived snapshot, not the live file):

```bash
diode --mode=tx --dst=10.0.0.20:9999 --resend=3f29b1a2-c8e4-4f1d-9b6a-aeb5e7c84021
```

**Resend the most recent send of a given file by name**:

```bash
diode --mode=tx --dst=10.0.0.20:9999 --resend-latest=report.pdf
```

**Filter the manifest** by recency or by sid:

```bash
diode --mode=manifest --since=24h                    # Go duration, plus 'd' / 'w' shortcuts
diode --mode=manifest --since=7d --format=tsv        # tab-separated for piping
diode --mode=manifest --session-id=3f29b1a2-...      # just one session's history
diode --mode=manifest --format=json | jq '.filename' # structured
```

**Defaults and where things live:**

| Default path | Override flag |
|---|---|
| `~/.diode/sent/<UTC>__<basename>` (Linux/macOS) | `--sender-state=<dir>` |
| `%APPDATA%\diode\sent\<UTC>__<basename>` (Windows) | same |
| `<state>/manifest.jsonl` | (derived from above) |

**Receiver-side recognition of resends:**

The receiver tracks the last `--completed-cache=N` session ids (default
1024). A resend that arrives *after* the original already completed is
silently dropped and counted as `soh_for_completed=N` in the stats
line — no second delivery to `--files-to`. Set `--completed-cache=0`
to revert to always-re-deliver behavior.

### 8e. Vacuum old state

Both the receiver spool and the sender's `sent/` archive grow without
bound. The bundled cleanup is **cron-friendly**:

```bash
# remove sender archive files older than 7 days, prune matching manifest entries
diode --mode=vacuum --age=10080 --sender-state=~/.diode

# remove abandoned receiver spool sessions older than 1 day
sudo diode --mode=vacuum --age=1440 --spool=/var/spool/diode

# show what would be removed, take no action
diode --mode=vacuum --age=10080 --sender-state=~/.diode --dry-run --verbose
```

`--age` is in minutes. A receiver spool session is **only** vacuumed if
its newest file (typically `chunks.bitmap`, refreshed on every chunk)
is older than the threshold — so vacuum never races with an in-flight
transfer.

### 8f. Burst-loss tolerance: time-spread redundancy + interleaved SOH

The defaults (no flags needed) already protect against the two
realistic loss modes:

- **`--redundancy-order=spread`** (default) — when `--redundancy=N`, the
  sender ships **N round-robin passes** of every chunk instead of N
  consecutive copies. Same total bytes; a network burst that drops K
  consecutive datagrams now costs 1 copy each of K different chunks
  instead of all copies of (K/N) chunks.
- **`--soh-interval=256`** (default) — the SOH preamble is also
  re-emitted every 256 data frames. Without this, a tiny early-burst
  could wipe all `--soh-redundancy` copies of the SOH and the receiver
  would silently drop every DATA frame for the rest of the transfer.

On a clean wire (LAN, loopback, point-to-point fiber) you don't need
either — defaults are tuned for "works fine, costs almost nothing."
Bump `--redundancy=2` or `--redundancy=3` on a lossy WAN and the
spread layout will survive bursts that the consecutive layout would
turn into a re-transfer.

### 8g. Raw byte stream (without filename)

When you just want bytes through (the diode doesn't care what they are):

```bash
# receiver — appends every delivered message to this file
diode --mode=rx --listen=:9999 --out=/srv/incoming/payload.bin
```

```bash
# sender — read from stdin
cat big-file.tar | diode --mode=tx --dst=10.99.0.20:9999 --chunk=1400 --redundancy=3
```

This is the right mode for syslog streams, pipelines, or anything where
the consumer doesn't need a filename. `--out` and `--files-to` are
mutually exclusive on the receiver; pick one per receiver process.

### 8c. Rate-limited continuous shipping

```bash
# sender, cap at 5 MiB/s on the wire
diode --mode=tx --dst=10.99.0.20:9999 --rate=$((5*1024*1024)) < /dev/urandom
```

The token-bucket smooths over 1 s. Useful when the receiver's downstream
sink is slower than the LAN.

---

## 9. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `--mode=tx` runs silently, nothing arrives | Receiver not started, or wrong port, or host firewall blocks `--dst` | `ss -lnu \| grep 9999` on the receiver host; check firewall |
| Stats line shows `frames_in=N` but `msgs_delivered=0` | Frames arrived, none completed → either truncation or a partial multi-chunk message | check sender's `--chunk` ≤ 1400; check for packet loss with `--redundancy=2` |
| `frames_ignored=N` non-zero | Some datagrams failed validation (bad magic, version, hash, length). Garbage on the port, or a stale sender from a different version | Check who's sending to that port; pin firewall to expected source |
| `msgs_evicted=N` non-zero | Receiver's reassembly cap was hit | Raise `--max-pending` / `--max-bytes` on the receiver, or chase the upstream loss with `--redundancy` on the sender |
| Receiver exits with `bind: address already in use` | Another process owns the port | `ss -lnup \| grep 9999` |
| LXC scripts say "missing lxc-create" | The `lxc` package isn't installed (only `lxc-libs` is by default on Fedora) | `sudo dnf install -y lxc lxc-templates` |

---

## 10. What's NOT in v0 (so you don't deploy it the wrong way)

The [threat model](architecture/threat-model.md) is the authoritative source.
The short version:

- **Frames are not authenticated.** SHA-256 ≠ MAC. Any attacker who can reach the receiver's port can inject valid-looking frames. **Mitigate by network-layer controls** (point-to-point link, firewall pinning the sender IP) until ADR-0004 lands HMAC/Ed25519.
- **Wire is plaintext.** No encryption in v0. Pre-encrypt at the application layer if the wire isn't physically isolated. ADR-0005 will add PSK + AES-256-GCM.
- **No FEC.** A single lost datagram in a multi-chunk message loses the whole message unless `--redundancy>1`. Linear cost only; ADR-0006 will add Reed-Solomon parity.
- **No plugins.** Protocol adapters (syslog, OPC-UA, file watcher) are not yet pluggable; treat tx/rx as the only API for now.

If those constraints rule out your deployment, wait for Sprint 02+ rather than improvising. The threat model lists each gap and which ADR will close it.

---

## 11. Where to go next in the code

If you want to read the implementation, in this order:

1. `cmd/diode/main.go` — entry point and `--mode` dispatch (50 lines).
2. `cmd/diode/tx.go` — the send loop, flag parsing, redundancy logic.
3. `internal/framing/frame.go` — `Encode` and `Decode` against ADR-0002.
4. `internal/transport/udp/udp.go` — `Sender` and `Receiver` types; note that `Receiver` has no method that writes to the network.
5. `cmd/diode/rx.go` — the receive loop wiring (UDP → framing → reassembly → writer).
6. `internal/reassembly/reassembly.go` — chunk buffering, dedup, eviction.

Tests are in `_test.go` next to each file, plus `test/e2e/` for the
black-box subprocess tests. Run `go test ./... -v` to see them all.
