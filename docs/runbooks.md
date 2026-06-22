# DataDiode — Operator Runbooks

> Audience: a line operator on shift. You don't have to read the
> source. Each runbook is a single incident: **what you'll see**, **how
> to confirm it**, **immediate mitigation**, **root cause**, **permanent
> fix**.
>
> Every runbook ends with a one-line **escalate-if** trigger — when to
> hand it to engineering instead of continuing to poke.

Today's most-likely incidents, ranked roughly by frequency:

1. [Spool directory filling the disk](#1-spool-directory-filling-the-disk)
2. [Receiver is dropping every frame](#2-receiver-is-dropping-every-frame)
3. [`data_dropped` counter climbing](#3-data_dropped-counter-climbing)
4. [PSK rotation across both sides](#4-psk-rotation-across-both-sides)
5. [Receiver won't start after an upgrade](#5-receiver-wont-start-after-an-upgrade)
6. [In-flight session abandoned by sender](#6-in-flight-session-abandoned-by-sender)
7. [Operator-initiated rx restart with traffic in flight](#7-operator-initiated-rx-restart-with-traffic-in-flight)

Companion docs:
- [`tutorial.md`](tutorial.md) — how it normally works.
- [`enterprise-roadmap.md`](enterprise-roadmap.md) — day-1 deployment surprises + preflight checklist.
- [`scripts/preflight.sh`](../scripts/preflight.sh) — interim preflight tool until `diode --mode=preflight` ships.
- [`sizing-guide.md`](sizing-guide.md) — capacity planning numbers.

---

## 1. Spool directory filling the disk

### What you'll see
- Disk-full alert on the rx host.
- `df -h /var/spool/diode` (or wherever `--spool` points) shows usage climbing.
- Receiver `stderr` shows errors like `create spool dir`, `pwrite: no space left on device`, `mkdir`. New sessions stop completing.

### How to confirm it's this incident
```bash
du -sh /var/spool/diode
ls /var/spool/diode/ | wc -l            # number of pending sessions
find /var/spool/diode/ -mtime +1 -type d | wc -l   # abandoned (>24h old)
```
If the count of `mtime +1` directories is high (hundreds → thousands), the vacuum loop isn't running.

### Immediate mitigation (60 seconds, safe)
```bash
sudo diode --mode=vacuum --spool=/var/spool/diode --age=24h
# or aggressive cleanup if disk is critical:
sudo diode --mode=vacuum --spool=/var/spool/diode --age=1h
```
`--mode=vacuum` is **safe to run alongside a live rx** — it only prunes session dirs older than the age cutoff, never touches in-flight ones.

### Root cause
- **No vacuum loop**: the rx was started without `--vacuum-interval=...` and nobody added a cron entry.
- **OR** the spool dir is sized below the rx's realistic backlog at peak ingest.
- **OR** receiver is wedged on a single huge session that won't complete (look at the largest dir's age + bitmap fill ratio).

### Permanent fix
Restart the rx with vacuum enabled:
```bash
diode --mode=rx \
  --listen=<ip>:<port> --files-to=<dst> --key-file=<psk> \
  --spool=/var/spool/diode \
  --vacuum-interval=1h --vacuum-age=24h
```
If the spool is genuinely undersized for the workload, see [`sizing-guide.md`](sizing-guide.md) §"Spool dir budget" and either grow the volume or shrink `--vacuum-age`.

### Escalate if
- After vacuum, the spool refills within the same shift → your `--vacuum-age` is wider than the gap between cleanups, **or** you have a producer dumping sessions that never complete (sender-side problem, not rx).

---

## 2. Receiver is dropping every frame

### What you'll see
- Sender exits cleanly and reports the SHA256 of what it sent.
- Receiver `stderr` stats line: `soh_seen=0 soh_accepted=0 data_frames=0`.
- No new files in `--files-to`. Spool dir empty or unchanged.

### How to confirm
On the rx host:
```bash
# Is the rx actually listening on the right address + port?
ss -ulnp | grep diode

# Are frames hitting the wire at the rx?
sudo tcpdump -ni any udp port 9999 -c 5
```
- If `tcpdump` shows nothing → **network problem** (firewall, route, MTU).
- If `tcpdump` shows frames but rx stats show 0 → **rx is rejecting them** (key mismatch, version mismatch, frame corruption).

### Immediate mitigation
| Failure mode | Quick check | Quick fix |
|---|---|---|
| Firewall blocking inbound | `sudo nft list table inet filter \| grep <port>` or `iptables -L -n` | open the port temporarily; coordinate permanent rule with netops |
| Bound to wrong IP | rx stderr shows the bind address | restart with explicit `--listen=<correct-ip>:<port>` (never bare `:port` — Go picks IPv6 wildcard on some kernels) |
| Wrong PSK | rx started with `--key-file=…` whose SHA differs from sender's | `sha256sum /etc/diode/psk.hex` on both sides; if they differ, see [Runbook 4](#4-psk-rotation-across-both-sides) |
| Wire-version mismatch (older sender, newer rx or vice-versa) | rx stats `soh_seen=0` despite tcpdump showing traffic | check `diode --version` on both ends; the older one must be upgraded |

### Root cause
The receiver silently drops bad frames by design — UDP has no return path, so we cannot tell the sender "your key is wrong." Stats counters are your only signal. The four causes above cover ~95% of "every frame is dropped" cases.

### Permanent fix
- Add a host-firewall rule for the diode port, persistent across reboot.
- Always pass `--listen=<specific-ipv4>:<port>`, never `:port`.
- Store the PSK SHA256 in your config-management system and check it pre-deploy.
- Pin the wire-protocol-version mismatch on the upgrade runbook so it isn't silent next time.

### Escalate if
- `tcpdump` shows frames on the rx host, the PSK SHAs match, the binary versions match, and the stats line still reports `soh_seen=0`. That's a framing bug — collect the pcap + the binary's `--version` and hand to engineering.

---

## 3. `data_dropped` counter climbing

### What you'll see
- rx stats line at shutdown (or `--stats-interval` if you enabled live stats):
  `data_frames=120000 data_dropped=37000 data_dup=12 completed=43`
- Customer reports "the transfers complete eventually but feel slow" or "I see a lot of FEC reconstructions in the stats."

### How to confirm
```bash
# Watch the rmem high-water mark
ss -uln | awk '/UNCONN/ {print $2, $3, $4, $5}'
# Watch UDP drop counters
ss -s ; cat /proc/net/snmp | grep ^Udp:
```
If `InErrors` or `RcvbufErrors` is climbing in `/proc/net/snmp`, the kernel is dropping before our code sees the frames.

### Immediate mitigation
```bash
# Bump kernel UDP rcvbuf ceiling temporarily.
sudo sysctl -w net.core.rmem_max=33554432

# Restart rx asking for a bigger read buffer.
diode --mode=rx --buffer-len=33554432 ...
```

### Root cause (one of)
- **Kernel `net.core.rmem_max` < 4 MiB** caps the rx socket buffer regardless of `--buffer-len`. Default Linux is often 208 KiB.
- **MTU on the path < `--chunk-size + 100B`**, causing IP fragmentation that is then dropped by some hop or by the receiver if `df` bit is set.
- **Slow consumer (disk)** can't keep up with the wire arrival rate; spool dir on a slow disk amplifies this. Watch `iostat -x 1`.
- **Senders running concurrently** swamping a single rx with no rate limit.

### Permanent fix
Persist the kernel tunable on the rx host:
```bash
echo 'net.core.rmem_max=33554432' | sudo tee /etc/sysctl.d/99-diode.conf
sudo sysctl --system
```
Drop `--chunk-size` on the senders to fit the path's actual minimum MTU (check with `ping -M do -s <size>`). And put the spool on a disk that can sustain at least 2× the expected ingress rate.

`data_dropped=N` after a *successful* transfer is **normal** when FEC is on: those are the parity chunks arriving just-too-late because the data chunks already completed the session. The relevant counter to alarm on is `data_dropped` *without* a matching `completed`.

### Escalate if
- `/proc/net/snmp` `RcvbufErrors` is climbing despite `rmem_max=64MiB` and bumped `--buffer-len`. That's a CPU-bound receiver and needs scale-out (multiple rx processes, ports, or hosts).

---

## 4. PSK rotation across both sides

### What you'll see
Routine ticket: rotate the PSK by date X. Or breach response: rotate now.

### Procedure (coordinated stop-restart — there is no overlap window today)

**On the future-key side (off-line key generation):**
```bash
sudo diode --mode=psk --file=/etc/diode/psk.hex.new
sudo chmod 0400 /etc/diode/psk.hex.new
sha256sum /etc/diode/psk.hex.new            # write down for verify
```

**Transport the new PSK out-of-band** (USB, scp from a jump host with the operator on the call, Vault, etc.). NOT through the diode itself.

**On both sides, in this order:**
```bash
# 1. Stop sender first so it doesn't keep firing under the old key.
sudo systemctl stop diode-tx   # or kill -INT <pid>

# 2. Verify the new key is in place and matches on both ends.
sha256sum /etc/diode/psk.hex.new            # MUST match the value written above

# 3. Swap files atomically.
sudo mv /etc/diode/psk.hex.new /etc/diode/psk.hex

# 4. Restart rx, then tx.
sudo systemctl restart diode-rx
sudo systemctl restart diode-tx
```

### How to confirm rotation worked
```bash
# Sender: send one small test file through.
echo "rotation test $(date)" | diode --mode=tx --dst=<rx>:<port> \
  --in=- --key-file=/etc/diode/psk.hex

# Receiver: check the file landed and rx stats incremented.
ls -lt /srv/incoming | head
```
If the test file lands and rx stats show `soh_accepted` incremented, rotation is good.

### What happens if you skip the coordinated stop
- During the gap, the side with the new key rejects everything from the side with the old key (silently).
- Sender-side: queued sessions in flight get dropped on the floor. They will need to be resent via `--resend` from `~/.diode/sent/`.

### Escalate if
- After rotation, `--resend` of a session from before-rotation still fails. The cached SOH was signed with the old key; the archive needs the old key alongside. Engineering can re-sign if needed, or you accept the loss.

---

## 5. Receiver won't start after an upgrade

### What you'll see
- `systemctl status diode-rx` shows the unit failing in a loop, or the binary exits within seconds of starting.
- Common stderr lines:
  - `error opening completed.idx: …`
  - `mkdir /var/spool/diode: permission denied`
  - `listen udp …: address already in use`
  - `unrecognized flag: …`

### How to confirm + fix per symptom

| stderr line | Cause | Fix |
|---|---|---|
| `unrecognized flag: --foo` | New version removed or renamed a flag (CLI break — see CHANGELOG) | Re-read the CHANGELOG entry tagged `CLI-BREAKING`; update the unit / config to the new flag. |
| `error opening completed.idx` | The persistent replay-cache file is corrupt OR the spool dir was deleted between versions | **Don't `rm -rf /var/spool/diode`**. Move it: `sudo mv /var/spool/diode /var/spool/diode.bak && sudo install -d -m 0750 -o diode -g diode /var/spool/diode`. Old completed-sids are lost — replay protection resets, which is acceptable as a one-time event. Investigate the corruption (disk error?) with the moved-aside copy. |
| `mkdir … permission denied` | Spool dir owner doesn't match the user the service drops to | `sudo chown -R diode:diode /var/spool/diode` |
| `listen udp …: address already in use` | Old rx process didn't fully exit, or a different service grabbed the port | `sudo ss -ulnp \| grep <port>`; kill the holder or restart it. |
| Process restarts in a loop with no obvious error | systemd's `Restart=on-failure` cycling faster than logging catches up | `journalctl -u diode-rx -n 200 --no-pager` to see the full failure. |

### Wire-protocol mismatch — special case
If the new rx is one wire version ahead of the sender (e.g. rx upgraded to v5, senders still on v4), the rx will simply not accept any frames. Stats line shows `soh_seen=0`. There's no error log today; this is fixed in Sprint 04 with the `frames_wrong_version` counter. Until then:
```bash
sudo tcpdump -ni any udp port <port> -c 1 -X | head -10
# byte 0x01 of the payload is the version byte. v4 = 0x04, v5 = 0x05, etc.
```

### Escalate if
- The completed.idx corruption recurs after replacing the file. Disk-health check overdue, or a kernel bug.
- The upgrade introduced an undocumented CLI break (CHANGELOG didn't tag it). File a bug.

---

## 6. In-flight session abandoned by sender

### What you'll see
- A session dir under `/var/spool/diode/<sid>/` that hasn't grown in N minutes.
- `meta.json` shows the expected `total_bytes`; `chunks.bitmap` shows only some bits set.
- No new frames for that `sid` arriving in `tcpdump`.

### Diagnosis
Inspect the session:
```bash
SID=<the-session-id-you-see-in-ls>
cat /var/spool/diode/$SID/meta.json | jq
ls -la /var/spool/diode/$SID/
# how many chunks landed vs total?
python3 -c "import sys; b=open('/var/spool/diode/$SID/chunks.bitmap','rb').read();
n=sum(bin(x).count('1') for x in b); print(f'{n} bits set')"
```

### Mitigation options

**(A) Sender knows about the abandonment** — ask the sender to:
```bash
diode --mode=tx --resend=<sid> --dst=<rx>:<port> --key-file=…
```
This pulls the session from `~/.diode/sent/<sid>/` on the sender host and resends only the chunks the receiver is missing. (Actually it resends all, but rx dedups via the bitmap.)

**(B) Sender is gone / source data lost** — the session will never complete; reclaim disk:
```bash
sudo rm -rf /var/spool/diode/$SID
```
Or wait for the vacuum loop to do it (after `--vacuum-age`).

### When to leave it alone
If the sender is on a flaky link and the session is < `--vacuum-age` old, **leave it**. The session-resume design (ADR-0005) means new frames for the same `sid` will pick up where they left off.

### Escalate if
- `meta.json` is malformed or missing. That's a bug — the rx normally writes it before accepting any DATA frames.

---

## 7. Operator-initiated rx restart with traffic in flight

### What you'll see (current limitation)
- You stop the rx with `systemctl stop diode-rx` (SIGTERM).
- In-flight sessions: their session dirs are preserved in `/var/spool/diode/` but the rx's in-memory state is gone.
- When the rx restarts, those sessions are *not* automatically rehydrated. New frames arriving for those `sid`s today are treated as new SOHs being missing → `data_dropped` increments.

### Mitigation procedure

**1. Drain expected traffic if you can.** Coordinate with the sender to pause before you restart.

**2. Restart cleanly:**
```bash
sudo systemctl restart diode-rx
```

**3. Tell the senders to resend any in-flight session:**
```bash
diode --mode=tx --resend=<sid> --dst=<rx>:<port> --key-file=…
```
Senders know their `sid`s from `~/.diode/sent/manifest.jsonl`:
```bash
tail -5 ~/.diode/sent/manifest.jsonl | jq .
```

### Permanent fix (Sprint 05+)
"In-flight resume across rx restart" is on the roadmap (`enterprise-roadmap.md` §5.4). When it ships, this runbook collapses to "restart, wait, done."

### Escalate if
- Sender doesn't have the session in `~/.diode/sent/` (vacuum ran on the sender side too aggressively). Then the session is lost; the source data must be re-fed from the application that produced it.

---

## What's *not* in this doc (and where to find it)

- **Hardware sizing, throughput expectations, disk math** → [`sizing-guide.md`](sizing-guide.md).
- **Pre-deployment checklist** → [`enterprise-roadmap.md`](enterprise-roadmap.md) §3, plus the [`scripts/preflight.sh`](../scripts/preflight.sh) stopgap.
- **Wire format internals, ADR rationale** → `docs/architecture/`.
- **GUI / dashboard** → not in scope today; the operator surface is CLI + stderr + the stats line.

When in doubt: keep `--mode=vacuum` running, keep `diode --version` and `sha256sum psk.hex` outputs in your incident notes, and never `rm -rf /var/spool/diode` between upgrades.
