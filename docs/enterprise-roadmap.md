# DataDiode — Enterprise Deployment & Roadmap

> Audience: product owner + ops lead + engineering lead.
> Purpose: honest assessment of where the product stands for real
> customer deployments, what surprises will bite on day 1, what
> infrastructure supports an iterate-as-needed model, and what the
> path to a procurement-grade "enterprise" release looks like.
>
> Status as of 2026-06-21 (end of Sprint 03):
> - Wire protocol v4. Three closed HIGH residual risks (S-1/T-1 unauth
>   frames, I-1 plaintext on wire, practical replay attack).
> - ~100 unit tests + ~30 E2E + 1 fuzz target; cross-compiled to
>   linux/darwin/windows/freebsd × amd64/arm64.
> - Operator-visible features: `--mode=tx|rx|psk|manifest|vacuum`,
>   `--send-file`, `--files-to`, `--key-file`, `--resend`,
>   `--fec-group-size`, persistent `<spool>/completed.idx`,
>   `--vacuum-interval` rx loop.

---

## Table of contents

1. [Where the product stands today](#1-where-the-product-stands-today)
2. [Day-1 deployment surprises (15 items, ranked)](#2-day-1-deployment-surprises)
3. [Pre-deployment checklist (30 minutes)](#3-pre-deployment-checklist)
4. [The iterate-as-needed safety bundle (Sprint 04 prefix)](#4-the-iterate-as-needed-safety-bundle)
5. [Path to enterprise-grade, by domain](#5-path-to-enterprise-grade-by-domain)
6. [Buyer-profile prioritization](#6-buyer-profile-prioritization)
7. [Recommended sprint ordering](#7-recommended-sprint-ordering)
8. [What's already solid (don't over-worry list)](#8-whats-already-solid)
9. [Risks you're carrying with the iterate model](#9-risks-youre-carrying-with-the-iterate-model)

---

## 1. Where the product stands today

| Property | Status |
|---|---|
| Wire protocol | v4 (AEAD + FEC); two prior wire bumps documented in ADRs |
| Threat model HIGH residuals | All closed |
| Threat model MEDIUM residuals | D-3 (loss) partially closed via XOR FEC; D-4 (slow-consumer drops) open |
| Cross-OS | Linux / macOS / Windows / FreeBSD × amd64 / arm64 |
| Test coverage | 100+ unit, 30+ E2E, 1 fuzz target, ~all on every commit |
| Docs | Tutorial, PSK howto, threat model, 9 ADRs, sprint history, suggestions log |
| Production deployment | **Possible with a knowledgeable operator who reads this doc**. Not yet turnkey. |

The core data path — keyed AEAD session, sparse-spool write, atomic
rename, replay-protected — is reliable and well-tested. What's
missing is the **wrapper** around it: observability, packaging,
service management, compliance artifacts.

---

## 2. Day-1 deployment surprises

Ranked by likelihood-of-hitting-an-unprepared-operator. The product
itself isn't broken — these are environment and packaging gaps.

### 2.1 Will-bite-you on day 1 (high probability, hard to predict)

| # | Surprise | Symptom | Fix today |
|---|---|---|---|
| 1 | **Path MTU < 1500** (PPPoE, VPN, WireGuard, some cloud) | huge `data_dropped` on rx; transfer fails mostly | sender: `--chunk-size=1200` (or whatever fits the path's min-MTU − 100) |
| 2 | **Kernel `net.core.rmem_max`** caps `SO_RCVBUF` below our 4 MiB request | silent kernel drops on burst; rx stats show fewer frames than tx sent | `sysctl -w net.core.rmem_max=16777216` on the rx host; persist in `/etc/sysctl.d/99-diode.conf` |
| 3 | **Customer outbound firewall blocks UDP/9999** | tx exits "successfully" (UDP doesn't ACK); rx gets zero frames | probe with `nc -uw1 <rx-host> 9999 <<< probe` from the tx host before declaring deployment |
| 4 | **Receiver host runs Docker / Podman** (any user of `br_netfilter`) | Docker's `ip filter FORWARD policy drop` silently swallows bridge traffic | per-bridge opt-out: `echo 0 > /sys/class/net/<bridge>/bridge/nf_call_iptables`. Our `scripts/lxc-setup.sh` does this; production deploys need their own systemd-tmpfiles or sysctl entry |
| 5 | **Default `/var/spool/diode` requires root** | rx fails to create spool on first start | run as a dedicated `diode` user with the dir pre-created and `chown`'d |
| 6 | **PSK out-of-band distribution** is the customer's responsibility | customer asks "how do I get the key to both sides?" mid-deployment | decide **before** the install: scp from a jump host, USB key, Vault secret store, etc. See `docs/psk-howto.md`. |
| 7 | **No systemd unit ships in the box** | customer writes their own; forgets `Restart=on-failure` or drops privileges incorrectly | ship a reference unit (open item, see Sprint 05 below); until then, copy the template from the deployment checklist |
| 8 | **Go's dual-stack picks `[::]:9999` from `:9999`** on some kernels | rx binds IPv6 wildcard; customer expected v4-only and is confused | always pass `--listen=<specific-IPv4>:port`, never `:port` |

### 2.2 Will-bite-you in week 1

| # | Surprise | Symptom | Fix |
|---|---|---|---|
| 9  | **Spool dir fills the disk** | rx starts failing to create new spool dirs; eventually disk-full | always set `--vacuum-interval=1h --vacuum-age=24h` (S03-8) or cron `--mode=vacuum` |
| 10 | **`stderr` log grows forever** | systemd or shell redirect → multi-GB log file | hand the customer a logrotate config OR pipe through journald with rate limiting |
| 11 | **`manifest.jsonl` grows forever** on the sender | gradual file bloat in `~/.diode/` | `--manifest-rotate-bytes=10000000` plus cron vacuum on `--sender-state` |
| 12 | **`--max-bytes=4GB` silently caps single transfers** | "why won't this 10 GB file send?" | document loudly; bump per deploy with `--max-bytes` |
| 13 | **Sender reads whole file into RAM** | 16 GB file = 16 GB RAM on the sender box | document; offer a tar-and-split workflow until streaming sessions ship |
| 14 | **Windows mode bits flatten to read-only-only** | Unix `0640` → Windows shows only the read-only bit; surprises permissions audits | already documented; surface in deployment checklist |
| 15 | **Windows AV scans every delivered file** | rx looks slow; files quarantined; SOC alerts on encrypted content | exclude `--files-to` from real-time AV; monitor that dir separately if SOC needs visibility |

### 2.3 Will-bite-you in month 1 (operational maturity)

| # | Surprise | Cost |
|---|---|---|
| 16 | **No live metrics endpoint** — operator can't graph or alert | needs SSH + log tail to check health |
| 17 | **No structured logs** — pipelines must regex-parse the stats line | brittle alerting |
| 18 | **SIGTERM to rx kills in-flight sessions** (no graceful drain) | mid-transfer abandoned; resume happens on the next sender attempt, but operator-confusing in the meantime |
| 19 | **No upgrade story documented** — when v5 ships, mixed-version deploys silently fail | needs a written runbook + the `frames_wrong_version` counter (see §4) |
| 20 | **Single-process rx = single point of failure** | no failover; planned restart = brief downtime |
| 21 | **`<spool>/completed.idx` is operator state now** | `rm -rf /var/spool/diode` between upgrades erases replay protection — document loudly |

---

## 3. Pre-deployment checklist

Run these on the customer host **before** declaring it ready. Estimated
total time: 30 minutes.

```bash
# 1. UDP RCVBUF headroom
sysctl net.core.rmem_max                                    # if < 16777216, raise it
echo 'net.core.rmem_max=16777216' | sudo tee /etc/sysctl.d/99-diode.conf
sudo sysctl --system

# 2. br_netfilter pollution check (if any container runtime is installed)
[ -f /proc/sys/net/bridge/bridge-nf-call-iptables ] && \
  cat /proc/sys/net/bridge/bridge-nf-call-iptables
# if "1", and you'll be bridging traffic to/from the diode receiver:
#   echo 0 > /sys/class/net/<your-bridge>/bridge/nf_call_iptables

# 3. Spool dir provisioned with dedicated user
sudo useradd -r -s /sbin/nologin diode
sudo install -d -m 0750 -o diode -g diode /var/spool/diode /srv/incoming

# 4. PSK delivered out-of-band and sha256-verified on both sides
sha256sum /etc/diode/psk.hex                                 # MUST match across hosts

# 5. Firewall: outbound UDP allowed from tx; inbound UDP allowed on rx
sudo nft list table inet filter | grep 9999                  # or `iptables -L -n`
# expect a rule like "udp dport 9999 accept" for the right interface

# 6. MTU check on the path
ip route get <rx-ip>                                         # eyeball the MTU
ping -M do -s 1372 <rx-ip>                                   # 1372 + 28 IP/UDP = 1400 chunk size

# 7. Vacuum + manifest rotation configured
#    tx side: --manifest-rotate-bytes=10000000
#    rx side: --vacuum-interval=1h --vacuum-age=24h

# 8. Test transfer with a small file BEFORE the real one
diode --mode=psk --file=/tmp/test-psk.hex
diode --mode=tx --dst=rx-host:9999 --in=- --key-file=/tmp/test-psk.hex <<< "hello"
# then verify the delivery on rx, and check the stats line
```

---

## 4. The iterate-as-needed safety bundle

If you're shipping new versions to customers as needed (which is the
right model for a 0.x product), the upgrade loop itself needs a safety
net. **The product is mostly fine for this — but one foot-gun and
four missing-pieces close the gap.** Total effort: ~half a day.

### 4.1 The one real danger: silent wire-version mismatch

When you push a v5 receiver and forget that some sender is still on
v4, the rx's `framing.PeekKind` returns `ok=false` and the frame is
silently binned. Stats line shows `soh_seen=0`; operator can't tell
"the link is dead" from "no one is sending."

After **two** wire breaks already (v2→v3 AEAD, v3→v4 FEC) and more
likely coming (RS FEC, streaming sessions, schema additions), this
will bite a production deploy within 6 months unless we fix it.

**Fix (10 lines):** when `PeekKind` rejects a frame *because of
version mismatch* (not because of garbage), bump a
`frames_wrong_version` counter and log the first occurrence per
minute. Operators see the smoking gun on the rx stats line:
`frames_wrong_version=247 (last seen ver=0x03)`.

### 4.2 The five-item bundle

| | Effort | Value |
|---|---|---|
| **B1.** `frames_wrong_version` counter + rate-limited log line | 30 min | catches the #1 upgrade foot-gun |
| **B2.** `CHANGELOG.md` with explicit `WIRE-BREAKING` / `CLI-BREAKING` / `BACKWARD-COMPATIBLE` tags per release | ongoing, free | operator knows when to coordinate vs. drop-in |
| **B3.** `SECURITY.md` + `VERSIONING.md` declaring "MAJOR = wire break, MINOR = backward-compat feature, PATCH = bug fix" plus a security disclosure address | 1 hour | locks in the policy you're already following implicitly |
| **B4.** `diode --mode=preflight` — bundled version of §3 checklist that runs the checks and fails loudly | 2 hours | operators run it before and after each upgrade; postflight version proves the new binary actually works |
| **B5.** Reproducible `diode --version` output | already done | operator verifies the deployed binary matches the intended commit |

**Ship this bundle as the prefix of Sprint 04** before anything bigger.
After that, the deploy → observe → fix → redeploy loop is genuinely
safe.

---

## 5. Path to enterprise-grade, by domain

Everything below is gap-from-today, not "this is broken." The product
works; what's missing is the wrapper that makes it deployable,
observable, supportable, and procurable in a real org.

### 5.1 Observability & ops

| | Status | Gap | Effort |
|---|---|---|---|
| Stats | shutdown line on stderr | live metrics endpoint (Prometheus / OTLP) so operators can graph + alert without parsing logs | M |
| Logging | unstructured stderr | JSON structured logging, log levels, `--log-format=text|json`, `--log-file` | S |
| Health | none | `--health-addr` on the *low side* (rx is one-way so it can't expose itself; the tx host can run a health probe) | S |
| Live inspect | `cat /var/spool/diode/<sid>/meta.json` | `diode --mode=inspect` renders in-flight sessions as a table | S |

**Domain priority:** universal. Every buyer wants this.

### 5.2 Service management & packaging

| | Status | Gap | Effort |
|---|---|---|---|
| systemd | none | `diode-rx@.service` + `diode-vacuum.timer` + drop-privileges to dedicated `diode` user | S |
| Windows service | NSSM mentioned in tutorial | signed MSI installer, auto-registers as Windows Service | M |
| Config file | 100% CLI flags | `/etc/diode/diode.yaml` so flags don't end up in process listings + config-mgmt-friendly | M |
| Logrotate | not provided | shipped config for stderr + manifest | XS |
| Binaries | unsigned static binaries via `make cross` | signed RPM/DEB/MSI; gpg-signed `SHA256SUMS`; reproducible-builds attestation | M |
| SBOM | none | CycloneDX + SPDX generated per release | S |
| Supply chain | none | SLSA provenance attestation, signed releases (sigstore / cosign) | M |

**Domain priority:** universal.

### 5.3 Security & compliance

| | Status | Gap | Effort |
|---|---|---|---|
| Crypto | AES-256-GCM, HKDF, SHA-256 (Go stdlib) | **FIPS-mode build** using a validated crypto module (`BoringCrypto` Go variant or vendored crypto library) | M-L |
| Key rotation | manual stop-start, no overlap | rx accepts two keys simultaneously during a rollover window; `--key-file=path1,path2` | S |
| Key storage | file at rest | HSM / TPM / Vault-backed key handle; `mlock` to keep PSK off swap | M |
| Audit log | manifest tells you what was sent, but isn't tamper-evident | append-only log with per-record hash chain, optionally signed | M |
| Zeroization | constant-time MAC compare done; PSK left in RAM | zero PSK on shutdown / rotation | XS |
| Replay window | persistent completed-cache (ADR-0007) | good enough for most; bigger orgs want signed-timestamp-in-SOH + rx high-water mark | M |
| Reed-Solomon FEC | XOR (ADR-0009) closes 1-loss-per-group | ADR-0010 RS for multi-loss tolerance | L |
| Formal verification of diode invariant | reflection-based test on `udp.Receiver` | formal statement + third-party audit | L |
| Export control | unaddressed | ECCN classification + BIS notice for downloads (strong crypto export from US) | external |
| License | Apache 2.0 stub | full text, CLA, SPDX header on every source file | XS |

**Domain priority:** highest for defense, financial, regulated SaaS.

### 5.4 Reliability & scale

| | Status | Gap | Effort |
|---|---|---|---|
| Backpressure | kernel UDP buffer + `--rate` cap | when spool disk > N% full, refuse new SOHs gracefully; per-sender quotas | M |
| Multi-key | single global PSK | different PSKs per logical sender (multi-tenant rx) | M |
| In-flight resume across rx restart | spool dir survives but rx doesn't reload pending sessions | on startup, scan spool dir, hydrate pending session state | M |
| HA | single process | active/standby, or two receivers behind a load-balancing UDP socket | L |
| Multicast | none | optional multicast tx → many rx without N× bandwidth | M-L |
| Jumbo frames | hard-coded 1400 | `--mtu` flag; safe-discovery requires return channel so defaults stay conservative | S |
| Per-interface binding | by IP only | `--bind-device=eth0` (Linux-only via `SO_BINDTODEVICE`) | S |

**Domain priority:** in-flight resume + multi-key are highest. HA and
multicast are use-case-dependent.

### 5.5 Protocol features still on the deferred pile

| | Where it lives today | Gap | Effort |
|---|---|---|---|
| Plugin host | ADR-0001 plan; never built (S03-7 → Sprint 04) | path to real protocol adapters: syslog, OPC-UA, Modbus, MQTT, NetFlow… | L |
| Streaming sessions | requires upfront `total_bytes` | ADR for STREAM-flagged sessions with Merkle hash so receiver verifies as bytes arrive | M |
| Multi-file sessions | tar-then-send is the workaround | SOH manifest of N files | M |
| Time-window sessions | not present | SOH carries `expires_at`; rx refuses past expiry | S |

### 5.6 Testing & quality

| | Status | Gap | Effort |
|---|---|---|---|
| Unit | ~100 in tree | good as-is | — |
| E2E | 30 subprocess | good as-is | — |
| Fuzz | 1 target (framing.Decode) | fuzz on `session.IngestSOH/IngestDATA`, key loader, FEC decoder | S |
| Chaos / fault injection | none | deliberate-loss UDP relay for E2E; soak tests | M |
| Performance regression | benchmarks exist but not in CI | bench baselines + alert if regression > X% | S |
| Cross-platform integration | unit-tested per OS in CI; no Windows or macOS LXC-equivalent integration | real keyed transfer between Windows runners | M |

### 5.7 Documentation, runbooks, lifecycle

| | Status | Gap | Effort |
|---|---|---|---|
| User docs | tutorial.md, psk-howto.md, threat-model.md, 9 ADRs | strong; add this roadmap | done |
| Ops runbooks | none | "spool dir is filling up", "rx is dropping every frame", "after PSK rotation X happens" | S |
| Sizing guide | none | "1 GB/s rx needs N GB RAM, M GB/day spool churn" | S |
| Version policy | implicit (v0..v4 with breaks) | written backward-compat policy + deprecation timeline (covered by B3 above) | XS |
| Security disclosure | none | `SECURITY.md` + PGP key + response SLA (covered by B3 above) | XS |
| Upgrade migration docs | none formal | per-version "what changes, what breaks, what to do" (paired with CHANGELOG B2) | S |

---

## 6. Buyer-profile prioritization

Different buyers weight the gaps very differently. Pick a profile —
that decision shapes the next 2-3 sprints far more than any technical
choice.

| Buyer | Hot items (in priority order) |
|---|---|
| **Defense / cleared** | FIPS-mode build, formal verification of the diode property, signed releases, SLSA provenance, certification trail, ITAR/ECCN compliance |
| **Industrial / OT** | Plugin host (OPC-UA, Modbus), runs on resource-constrained hardware, deterministic behavior, no-internet operation, signed binaries |
| **SOC / SIEM pipeline** | Prometheus metrics, structured logs, K8s/helm chart, multi-tenant keys, HA |
| **Financial** | Tamper-evident audit log, replay-detection alerting, FIPS, formal change-control, role-based access |
| **General regulated SaaS** | SBOM, signed releases, security disclosure SLA, SOC 2 alignment, FIPS optional |

The product we have today maps roughly to **"strong upstream OSS we
could harden in-house."** To be procurable off the shelf, the gaps
cluster as:

1. **Observability** (metrics endpoint, structured logs, inspect command) — universal, ~1 sprint
2. **Service packaging** (systemd + MSI + signed RPM/DEB + SBOM + reproducible builds) — universal, ~1.5 sprints
3. **One of**: FIPS+audit-log (defense/financial), or plugin host (OT/SOC), or multi-tenant keys (multi-customer SaaS) — 1–3 sprints each
4. **Operator docs**: runbooks, sizing, version policy — ~1 sprint, can pair with the above

---

## 7. Recommended sprint ordering

This ordering assumes no specific buyer-profile lock-in yet. It
front-loads universal wins and defers buyer-specific work until the
customer signal is clear.

### Sprint 04 — Iterate-safe + observability

- **B1–B5** from §4 (iterate-as-needed bundle, ~half day)
- S03-7 plugin host design + WASM PoC (long-deferred from Sprint 02)
- Metrics endpoint (closes D-4); structured logging
- `diode --mode=inspect`
- LXC demo environmental fix
- Counter rename: `fec_parity_unused`

### Sprint 05 — Packaging + operator docs

- systemd unit + `diode-vacuum.timer`
- Windows MSI installer (signed)
- Signed RPM/DEB
- SBOM (CycloneDX) + reproducible-builds attestation
- Logrotate config + journald integration notes
- Ops runbooks (5 most-likely incidents)
- Sizing guide
- `docs/upgrade-guide.md`

### Sprint 06 — Pick a buyer profile and bias toward it

The customer conversations that happened by this point should make
this choice for you. Possible specializations:

- **Defense path:** FIPS-mode build + audit log + signed releases + formal verification spec
- **OT path:** Plugin host (real OPC-UA / Modbus / syslog adapters) + resource-constrained-host work
- **SOC path:** K8s/helm chart + multi-tenant keys + HA
- **Financial path:** Tamper-evident audit log + replay-detection alerting + change-control docs

### Sprint 07+ — Fill in the other 5.3-5.5 gaps based on real operator feedback

In-flight resume across rx restart, multi-key support, Reed-Solomon
FEC, streaming sessions, etc. — all valuable, but only when an actual
operator has hit the gap.

---

## 8. What's already solid

Resist the temptation to over-engineer these:

- **Wire protocol + AEAD + replay protection.** Exhaustively tested;
  `tcpdump`-verified to be ciphertext on the wire. Three ADRs deep,
  every HIGH residual closed.
- **Cross-OS portability.** Single static binary; works on
  Linux/macOS/Windows/FreeBSD × amd64/arm64.
- **Receiver opens no outbound sockets.** Reflection-test-enforced.
  The diode property is real, not aspirational.
- **Path-traversal safety on filenames.** Parser-level rejection,
  multiple layers of defense in depth.
- **`--mode=psk` + `--key-file`.** Full key lifecycle works:
  generate, distribute, verify, deploy, rotate (manual).
- **24+ E2E tests.** Every critical flow has a black-box subprocess
  test, including the keyed AEAD path, resend, vacuum, persistent
  cache, and FEC reconstruction.
- **Operator visibility.** `meta.json`, `chunks.bitmap`, and the
  shutdown stats line are real diagnostic surfaces that already
  beat what most OSS shippers expose.

---

## 9. Risks you're carrying with the iterate model

Honest list, so you know what you're holding:

- **No HA.** A critical bug in a deploy = brief outage while you push
  the fix or roll back. Acceptable for most diode use cases
  (telemetry, log shipping, batch file transfer); not for
  "real-time medical telemetry."
- **No remote management.** Every deploy needs SSH or equivalent
  access. Normal for OSS infrastructure software, but worth saying.
- **`<spool>/completed.idx` is operator state now.** Don't
  `rm -rf /var/spool/diode` between upgrades — that erases replay
  protection. Documented loudly in `docs/upgrade-guide.md` (when
  written).
- **PSK rotation requires a brief coordinated stop.** Today there's
  no two-keys-overlap window. If you change the PSK, both ends drop
  until they restart in lockstep. Plan maintenance windows
  accordingly until the overlap feature ships.
- **Configuration drift.** 100% CLI flags today. Two operators on
  two sites *will* configure things differently over time. A
  `/etc/diode/diode.yaml` story (Sprint 05) helps but isn't blocking.
- **No live metrics yet.** "Is the diode healthy?" requires SSH and
  log inspection. Bundle B (Sprint 04) closes this.
- **Wire breaks happen.** v2→v3 and v3→v4 in three sprints. Bundle
  item B1 (`frames_wrong_version` counter) is the cheapest mitigation
  until a real backward-compat policy stabilizes.

---

## Bottom line

**Production-deployable today by a knowledgeable operator following
the checklist in §3.** Not turnkey. The 5-item iterate-safety bundle
(§4) is ~half a day's work and makes the iterate-as-needed model
genuinely safe. The path to procurement-grade enterprise is laid out
in §5–7 and depends primarily on which buyer profile you're
targeting.

Default recommendation if no buyer profile is locked in: ship
**Sprint 04 (iterate-safe + observability + plugin host PoC)** and
**Sprint 05 (packaging + operator docs)** in that order. After those
two sprints, you have a defensible "1.0" candidate suitable for the
first real customer engagement.
