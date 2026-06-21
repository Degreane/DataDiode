# Suggestions & Best Practices

A living log of recommendations made during DataDiode development. Newest date at the top. Each suggestion belongs to a dated section so it stays traceable.

---

## 2026-06-21

### Language & runtime
- **Use Go** for the core (`diode-tx`, `diode-rx`) — single static binary per OS, strong stdlib, memory-safe receiver, low audit surface. See [ADR-0001](architecture/ADR-0001-language-choice.md).
- Build with `CGO_ENABLED=0` to guarantee static binaries.
- Cross-compile via `GOOS`/`GOARCH` rather than per-OS source paths.

### Plugin model
- Default to **WASM plugins (wazero)** — sandboxed and portable.
- Fallback: **subprocess plugins** (`go-plugin` style, stdio length-prefixed protocol).
- **Reject** in-process dynamic loading (`plugin.Open` / `dlopen`) — not portable to Windows, host-crash risk.

### Transport & framing
- **UDP only** on the wire — TCP requires a return path and defeats the diode.
- Per-message **SHA-256** for integrity from day one; signing later.
- Cap payload at **1400 B** to stay under typical MTU and avoid IP fragmentation.
- Sequence numbers monotonic per sender boot; receiver tolerates gaps.
- Defer FEC to a later sprint; start with naive N× redundancy as a config flag.

### Security posture
- Two stacked enforcement layers in the demo:
  1. **Application:** `diode-rx` opens no outbound sockets, ever (auditable in code).
  2. **Network:** `nft` on the receiver drops all egress (belt-and-braces).
- For confidentiality on the wire: pre-shared keys, **not DTLS** (DTLS handshake is bidirectional).
- Keep the core <3,000 LoC so it can be read end-to-end.
- Every third-party dependency requires an ADR.

### Containers (test environment)
- Use **plain LXC** on Fedora (`lxc-create`/`lxc-start`/`lxc-attach`), not LXD (not in Fedora repos) and not Docker (NAT/bridge model muddies the diode boundary).
- Bridge `diodebr0` with no upstream, no NAT — genuinely isolated L2.
- Pin static IPs in the container config (`lxc.net.0.ipv4.address`) so demo scripts are deterministic.
- All provisioning scripts must be **idempotent** — safe to re-run; detect existing state and skip.

### Process / agile
- 2-week sprints, lightweight ceremonies, each sprint gets one markdown file under `docs/sprints/`.
- Definition of Done includes: green CI on linux/windows/macos, docs updated, no new dep without an ADR.
- ADRs (`docs/architecture/ADR-NNNN-*.md`) for any decision that future contributors might want to challenge.

### Documentation discipline
- All suggestions and best-practice recommendations go in **this file**, not just in chat.
- ADRs capture *decisions* (with alternatives and consequences); this file captures *advice* (which may or may not become a decision later).

### Persistent completed-cache (from S03-5, ADR-0007)
- **Disk-backed replay protection without a wire change.** The cache hydrates from `<spool>/completed.idx` at receiver startup, so replays that capture a completed session and replay-after-restart are caught by the same code path that catches mid-run replays. No version bump, no operator action beyond running with `--completed-cache-disk=true` (default).
- **Append-then-fsync per finalize.** One small write + one fsync per completed session — negligible vs the disk write that just delivered the file. The session is already on disk; durability of the cache entry is symmetric with durability of the file.
- **Ring buffer with `--completed-cache-disk-cap`** (default 100 K) bounds RAM at startup. Disk file is unbounded; **vacuum** is the bounded-disk mechanism. The pruner is in-place with atomic temp+rename, same pattern as the manifest pruner.
- **Tolerate corrupt lines** in completed.idx. A partial write or filesystem corruption shouldn't take down the rx; parser skips bad lines and counts them as pruned during the next vacuum.
- **Default ON when SpoolDir is set.** Operators get replay protection by default; they have to actively opt out with `--completed-cache-disk=false` to revert to the RAM-only behavior. Right side of the safe-default trade-off.
- **One operator gotcha worth surfacing**: `rm -rf <spool>` now also resets the replay-protection set. Document loudly; recommend keeping the spool dir as durable state, not a scratch space.

### AEAD encryption (from S03-1, ADR-0008)
- **AEAD ≠ HMAC + encryption layered**. AES-256-GCM is a single primitive that provides confidentiality + integrity + authentication. Composing HMAC on top is duplicate work and wire bytes; composing encryption underneath HMAC needs a separate MAC key. Just use AEAD.
- **HKDF the PSK before use.** Never use the raw `--key-file` bytes as the AEAD key — derive a domain-separated subkey (`HKDF-SHA256(salt="diode-aead-v3-salt", ikm=psk, info="diode-aead-v3 chunk-aead")`). Lets the same PSK be safely reused for future subkeys (signed manifests, replay nonces).
- **Deterministic nonce from session_id || chunk_index** — unique per (key, frame) for free. The receiver doesn't need to track nonces; the framing layer recomputes them. Per-key uniqueness: random 128-bit session_id makes cross-session collisions statistically zero; within-session monotonic chunk_index is unique by construction.
- **Header as AAD, not as plaintext-only.** `framing.PeekKind` must remain cheap (no key required for routing), so the header bytes can't be encrypted — but they MUST be authenticated. AEAD's AAD parameter is exactly the right tool: a header bit-flip breaks the GCM tag, frame is rejected.
- **Unkeyed mode stays first-class.** Hard requirement from the user. The wire format has TWO shapes per frame type (unkeyed = trailing sha256, keyed = AEAD tag); the flag bit `FlagEncrypted` selects. v3 receivers must support both, with mismatched expectations rejected loudly.
- **Verify the wire with `tcpdump` after every encryption change.** A passing E2E test only proves the receiver can decode what the sender produced. A `tcpdump | strings | grep <marker>` proves the marker actually disappeared from the wire bytes — different guarantee, equally important.

### Sender state + resend + vacuum (from S02-8, ADR-0006)
- **Sender owns the session_id.** Receiver consumes whatever sid arrives — it has no opinion. This made the resend feature compose with the existing receiver semantics (bitmap dedup + SOH idempotency) with **zero protocol changes**.
- **Resend always pulls from the archive snapshot, not the live file.** Operators edit files; the snapshot is the only stable representation of "what the receiver was promised." Re-hashing the snapshot before resend catches accidental drift loudly.
- **Receiver's completed-cache is the right place to detect a finished resend.** A FIFO of recently-completed sids; the cache is in-memory only — receiver restart looks like "I never saw this sid" which is the safest default. Persisting it across restart is a future enhancement.
- **JSON Lines for the manifest** — append-only, line-tailable (`tail -f manifest.jsonl | jq .`), trivially streamable into any log pipeline. No schema migrations needed; forward-only field additions.
- **Vacuum as a separate `--mode`, cron-friendly.** Not a daemon, not threads in rx; just `--mode=vacuum --age=N --spool=... --sender-state=...`. Composes with whatever scheduler the operator already has.
- **Vacuum uses newest-mtime of session dir**, not first-mtime. A session that just received a chunk has a fresh `chunks.bitmap`, so vacuum never races with an in-flight transfer.
- **Time-spread redundancy is the default (`--redundancy-order=spread`).** Same total bytes, dramatically better burst-loss survival because N copies of a chunk are now spread across N passes instead of consecutive packets.
- **Interleaved SOH (`--soh-interval=256`) protects against the single biggest reliability foot-gun**: an early burst wiping all SOH copies and silently turning every subsequent DATA frame into an orphan drop. Re-emitting the SOH every 256 chunks costs <1% overhead.

### Session protocol design (from S02-7, ADR-0005)
- **SOH preamble is the right primitive.** Sender pre-computes sha256 + total + chunk plan; receiver provisions storage upfront. This is the canonical reliable-transport pattern (BitTorrent, Aspera, NORM, Zmodem) — once we tried to live without it (v1 msg_id model) we paid in concurrent-sender collisions, in-memory buffering, and back-pressure pathology that motivated the dead-end async-pool work.
- **UUID v4 session_id** instead of `uint32 msg_id` — collision probability across any number of senders is essentially zero. Two `crypto/rand` reads + two bit tweaks per session.
- **Spool-per-session directory** is genuinely the right operator UX, not just a nice-to-have. `ls /var/spool/diode/` shows every in-flight transfer; `cat meta.json` shows the plan; `xxd chunks.bitmap` shows progress. Zero special tooling needed.
- **Two spool modes behind a single flag** (`sparse` default, `files` opt-in). Sparse = one `pwrite` per chunk, no inode pressure, no extra disk usage. Files = forensic per-chunk visibility. Trivial to support both since the public sink behavior is identical.
- **Persist bitmap on every chunk**, not just on completion. The user explicitly wanted mid-flight visibility into what's missing; finalize-only persistence defeated that. Cost: one ~256 B write per chunk, well below the bandwidth ceiling.
- **JSON for `meta.json`**, not YAML/INI. Stdlib, universal parser availability, schema-friendly. YAML adds a dep; INI can't represent nesting.
- **Unknown session_id → drop with one stat() syscall**, no decode. `PeekKind` parses just the preamble (22 B) so the router can route before the expensive Decode.
- **SHA-256 mismatch on finalize → keep the spool dir** for forensic inspection. Failing silently into the void is worse than failing loud.
- **SOH carries `chunk_size`** so the receiver's pwrite offset arithmetic is `chunk_index * chunk_size` — no per-chunk size negotiation needed. Last chunk is naturally smaller via the DATA frame's `payload_len`.
- **`--soh-redundancy` defaulted to 3.** Losing one DATA frame in a 750-chunk message costs 1 chunk; losing the SOH costs the entire transfer.

### HMAC signing design (from S02-6, ADR-0004)
- **Opt-in via `--key-file=<path>` on both sides.** No `--key=<hex>` flag — process arguments leak into `ps`. The file should be `chmod 600`; we warn (don't refuse) on permissive modes, following SSH/PGP convention.
- **Auto-detect hex vs raw** in the key file. If every byte after whitespace-strip is a hex char AND the length is even, decode as hex; otherwise treat as raw. Lets operators paste a hex string OR write a binary file.
- **HMAC is appended after the SHA-256, not in place of it.** Wire layout: `header + payload + sha256 + hmac` when SIGNED. Pro: same code path can detect corruption (sha256 fails) before doing the keyed verify; layout-wise the unsigned and signed paths share offset arithmetic up to the hash.
- **The SIGNED flag bit lives in a previously-reserved bit (0x10).** A v0 receiver without a key correctly rejects a SIGNED frame via the existing reserved-bit-set check — no version bump needed for two-way safety.
- **Receiver policy is bidirectional refusal.** A keyed receiver MUST reject unsigned frames (otherwise an attacker bypasses auth by not signing). A keyless receiver MUST reject signed frames (otherwise an attacker downgrades by signing with their own key). Both are enforced inside `framing.Decode` via `ErrUnexpectedSign` / `ErrSignedExpected`.
- **`crypto/hmac` provides `hmac.Equal` for constant-time compare.** Use it — never `bytes.Equal` on MAC outputs.
- **Encode controls FlagSigned.** Callers must not pre-set the flag; Encode sets it iff a non-nil key was passed. Pre-setting is rejected as misuse — prevents callers from accidentally setting SIGNED without actually signing.
- **Bump the UDP read buffer minimum** by `HMACLen` (32 bytes). A signed full-MTU frame is 1490 bytes, not 1458. Forgetting this would silently truncate every signed frame.
- **HMAC scope = same as SHA-256** (header + payload). Including the SHA-256 in the MAC scope would be redundant — the MAC already cryptographically covers everything the SHA does.

### Makefile design (from S02-5)
- **Self-documenting via `## target: desc` comments + awk in `make help`.** Single source of truth — the comment IS the help text. No drift between docs and reality.
- **`.DEFAULT_GOAL := help`** — bare `make` prints the menu, not an error or a build.
- **`.SHELLFLAGS := -eu -o pipefail -c`** for safer recipes — but watch for `set -e` biting `$(command)` substitutions where the command may legitimately fail (e.g., `sha256sum missing-file`). Trail with `|| true` in those spots.
- **Inject version/commit/date via `-ldflags -X`** in one place (the Makefile), not in source. Requires the source-side identifiers to be `var`, not `const` — Go's `-X` only sets variables.
- **`make ci` mirrors the GitHub Actions lint+test+fuzz pipeline** so contributors can run the same checks locally before a push (or for a local-only repo, before each session ends).
- **`make cross` matches the CI build matrix verbatim** — `linux/darwin/windows/freebsd × amd64/arm64`. If CI changes, change here too; the list lives at the top of the Makefile as `CROSS_TARGETS`.
- **Avoid recipes that need root unless they wrap a sudo call themselves.** `make demo` does `sudo ./scripts/demo.sh` — operators always know when sudo is happening.

### LXC live-demo lessons (from S02-3)
- **No Fedora image on linuxcontainers.org** as of 2026-06. Use `rockylinux 9` (RPM-family, has systemd, similar feel) or `alpine 3.22` (smaller). Defaults moved to rockylinux/9.
- **lxc-create download template doesn't accept `--no-validate`.** Drop it; the template validates by default.
- **Distro init may leave eth0 DOWN** even when `lxc.net.0.flags = up` is set in the container config — NetworkManager/systemd-networkd inside the container doesn't know about the host-assigned IP. After `lxc-start`, explicitly `lxc-attach -- ip addr add ... && ip link set eth0 up`. Idempotent (EEXIST on second run is fine).
- **Enforce diode rules on the HOST, not inside the container.** Minimal Rocky/Alpine LXC images don't ship `nft`/`iptables`, and the isolated bridge has no internet for `dnf install`. The host already has `nft` and is also out of reach of a compromised container — stronger guarantee anyway.
- **Use the `bridge` family, not `inet`, for L2 bridge filtering.** The `inet` forward hook normally doesn't see bridge-forwarded packets. The `bridge` family hooks at L2 directly and sees every packet the bridge would forward. L4 matching in bridge family requires `ether type ip` before `udp dport`.
- **Watch for `br_netfilter` cross-contamination.** If Docker (or any user) loaded `br_netfilter`, ALL bridge-forwarded packets visit the `ip filter FORWARD` chain. Docker's chain has `policy drop`, silently breaking unrelated bridge traffic. Per-bridge opt-out: `echo 0 > /sys/class/net/<bridge>/bridge/nf_call_iptables`.
- **Find the host-side veth name via `ip link show eth0` inside the netns**, not `/sys/class/net/eth0/iflink`. `/sys` is host-mounted in plain LXC; netlink (via `ip`) is namespace-aware. The peer ifindex shows up as the digits after `@if` in the link name.
- **Always include a stats line printed AFTER the verify step in demo scripts**, so when it fails the operator can still see frames_in/dup/delivered counters. Current script's `exit 1` on MISMATCH bypasses the stats print; refactor in a later sprint to always print before exit.

### File transfer (from S02-1)
- **Application-layer envelope, not transport-layer change.** File semantics (name, mode, hash) live in a `DDF` envelope *inside* the existing diode payload. The transport stays oblivious. This is also the model for future protocols (syslog, OPC, MQTT) — each gets its own envelope.
- **Reject path traversal at the parser, not at the writer.** `fileenv.Decode` rejects names containing `/`, `\`, NUL, `.`, `..` *before* returning to the caller. The writer also re-baselines with `filepath.Base` (defense in depth) — a Decode bug would otherwise let the writer escape `--files-to`.
- **Atomic write via same-dir temp + rename.** The file at its final path either doesn't exist or is complete. Mode is applied with `chmod` on the tmp file *before* rename, so the final inode never appears with a wrong mode briefly.
- **Mutually-exclusive sink flags.** `--out` (raw stream) and `--files-to` (named files) cannot both be set — the receiver picks one mode at startup. Same on tx: `--in` and `--send-file` are mutually exclusive.
- **Bump default `SO_RCVBUF` to 4 MiB.** Discovered while sending a 4 MB binary: kernel default rcvbuf (~256 KiB) overflowed at full loopback speed → 8% UDP loss → message never completed. 4 MiB holds ~3000 max-MTU datagrams, enough for any realistic receiver decode rate. Best-effort: kernel `net.core.rmem_max` may cap it; we don't error.
- **Reuse `startRx` helper across E2E tests** by allowing an empty `outPath` to suppress `--out` — keeps the helper general without copy-pasting subprocess plumbing per test.

### Threat-modeling discipline (from S01-12)
- **Write the threat model after the code, but before the next sprint** — too early and you're guessing what'll get built; too late and you've shipped the threats. End of sprint is the right beat.
- **STRIDE alone is not enough** for a diode. Add a "diode-specific" section for covert channels, replay, IP fragmentation, parser ambiguity — the threats that don't cleanly fit S/T/R/I/D/E but matter for *this* product.
- **Every mitigation has a code pointer.** A threat model that lists "we mitigate X" without saying *where* is just optimism. Section 8 of `threat-model.md` is a flat index from mitigation name → file path / test name.
- **Be explicit about HIGH residual risk in v0** (S-1 unauthenticated, I-1 plaintext). An operator must not deploy the diode in the wrong threat environment because the doc was diplomatic about its gaps.
- **Reflection-based invariant tests** (`TestReceiver_NoWriteMethods`, `TestWireGolden`) are the strongest way to encode "this property must remain true." Cite them directly in the mitigation index so a future contributor can't quietly delete the test.

### CI design (from S01-11)
- **Lint job is the gate** — gofmt, go vet, *and* `go mod tidy` no-op check run first. `needs: lint` on the matrix jobs means a stray missing-tidy doesn't burn a half-hour matrix run.
- **Native tests on every supported OS**, not just linux. Windows UDP semantics differ; running the suite on `windows-latest` catches regressions that loopback-on-Linux can't.
- **`fail-fast: false`** on matrices — surface every failure in one run, not the first one and a curtain.
- **Concurrency group cancels in-flight runs on the same ref** — saves minutes when you push twice in a row.
- **Cross-vet, not just cross-build.** `go build ./...` skips test files, hiding test-only compile errors on other targets (we caught Windows missing `syscall.WaitStatus.Signaled()` this way). Always `GOOS=X go vet ./...` for cross-target sanity.
- **Per-OS build-tagged helpers** (`_unix.go` / `_windows.go`) for anything that uses POSIX-only types like `syscall.WaitStatus`. `runtime.GOOS == "windows"` at call sites is not enough — the compiler still needs the symbols to exist.
- **Short fuzz in CI (30s)** is enough to catch most regressions in `framing.Decode` without slowing down the build. Long campaigns belong in a nightly job.
- **Upload built artifacts** even from cross-compile jobs — operators can grab a pre-built `diode-windows-amd64.exe` from a green commit without setting up Go locally.
- **`bash -n` job for every script** — catches typos in shell scripts the same way `go vet` catches typos in Go.

### LXC scripts (from S01-9/10/13)
- **Hash-compare before replacing the binary.** `lxc-push.sh` SHA-256s the in-container file vs the freshly-built one and skips the install when they match. Avoids unnecessary `mv`s (and unnecessary inode churn) on re-runs after no-op rebuilds.
- **Atomic install via tmp + mv.** Write the new binary to `diode.new`, then `mv` it on top of `diode`. Inside the same filesystem this is atomic, and any currently-running process keeps its mmap'd inode until exit. No "ETXTBSY" or "Text file busy" errors.
- **`_common.sh` for shared bash state.** Defaults (BRIDGE, IPs, port, names), logging colors, and `require_root/cmd` helpers live in one sourced file. Override anything via env vars without editing scripts.
- **Idempotent nftables via named table + flush + rebuild.** `nft -f -` with `table inet diode {}; delete table inet diode; table inet diode { ... }` always converges to the desired state, leaves other rules untouched.
- **`check-prereqs.sh` runs WITHOUT root and prints exact install hints.** Operators want "install with: sudo dnf install lxc", not "missing dependency, good luck."
- **Block on the receiver's "listening on" banner inside the container** (poll its stderr log file via the host-visible rootfs path). Same discipline as the Go E2E tests — no racy sleeps.
- **`KEEP_BRIDGE=1` opt-out** on teardown. Multiple projects might share a bridge; default destroys it but it's one env var away from non-destructive.

### E2E tests (from S01-8)
- **Spawn the real binary, not the in-process types.** Unit tests already cover the in-process Sender/Receiver path; E2E catches drift in the CLI surface, signal handling, real UDP stack, and the actual kernel buffer behavior. Both are needed.
- **Build once in `TestMain`** to a tempdir; share the binary across all tests in the package. Per-test builds would inflate runtime several-fold.
- **Block on the receiver's "listening on" banner** before sending. A naive sleep (e.g. 100ms) is racy on slow CI; parsing stderr is deterministic and faster on healthy hosts.
- **Poll for output stability**, don't fixed-sleep. `readFileWhenStable` checks size every 10ms up to a timeout — fast on healthy hosts, only slow when something is genuinely broken.
- **Use ephemeral UDP ports** (`net.ListenUDP` with port 0, close, reuse). Race window is tiny on a single test host and avoids hardcoded port conflicts in CI.
- **Treat `signal.Signaled` exit as success** for SIGINT'd subprocesses. `exec.ExitError` with a signal is not a test failure.
- **Stats line in subprocess stderr is the assertion surface.** Tests read `frames_in / dup / delivered` — proves the diode's internal counters match expectations without bolting on a metrics endpoint just for tests.

### Reassembly design (from S01-7)
- **Recently-delivered MsgID cache** is a real requirement, not over-engineering. Without it, `--redundancy>1` on a single-chunk message delivers the message N times: the original completes and frees the partial; the REDUNDANT copy arrives and looks like a fresh msg_id with `ChunkTotal=1`, so it completes too. Cache size 1024 (FIFO eviction) suffices.
- **Only consult the recent cache for frames flagged REDUNDANT.** A non-REDUNDANT frame with a reused MsgID is treated as a legitimate new message (post-wrap reuse). Conservative: better to re-deliver than to silently drop after a 4B-message wrap.
- **Two complementary backpressure knobs** on pending state: `MaxPending` (message count) and `MaxBytes` (aggregate bytes). One isn't enough — many tiny partials can blow message count without much memory; one large partial can blow memory without crossing message count.
- **Single-goroutine ingest** — the udp.Receiver.Run loop is single-threaded, so the Reassembler needs no mutex. Document this explicitly in the package doc so a future contributor doesn't add concurrent calls.
- **Print stats line on shutdown** (frames in/dup/ignored, msgs delivered/evicted, bytes pending). Operators need to see "you lost 7 frames" without a metrics endpoint.

### CLI design (from S01-6)
- **Single binary, `--mode=tx|rx` flag** rather than two binaries or subcommands. Distribution is one file; both LXC containers get the same artifact. Security trade-off (sender code linked into receiver binary) is marginal — attacker with code execution on the receiver can write raw UDP in a few syscalls anyway.
- **Pre-parser for `--mode`** before handing remaining args to a mode-specific `flag.FlagSet`. The stdlib `flag` package errors on unknown flags, so a top-level FlagSet that doesn't know mode-specific flags would reject them.
- **`--help` falls through to the mode parser** when it appears after `--mode`, so `diode --mode=tx --help` shows tx flags. Only intercept at the top level when no mode is set yet.
- **`flag.ErrHelp` exits with code 0**, not 1. Getting help is not an error.
- **Inject `send` as a function in the loop struct** so unit tests capture frames without binding to a real UDP socket. Real-network testing belongs in E2E (S01-8), not in unit tests.
- **`--max-message` hard cap** so a runaway stdin can't keep the sender transmitting forever; protects an operator who pipes the wrong file in. Default 64 MiB; abort with a clear error if exceeded.
- **Empty input still emits a single FINAL+HEARTBEAT frame** so the receiver sees end-of-stream even when nothing was sent — saves operators from "did it run or did it hang?" ambiguity.

### UDP transport design (from S01-5)
- **Two separate types, `Sender` and `Receiver`** — not one bidirectional `Transport`. The split *is* the diode discipline: the Receiver type literally has no method that writes to the network.
- **Reflection-based invariant test** — `TestReceiver_NoWriteMethods` enumerates the exported methods of `*Receiver` and fails if any future contributor adds a `Send`/`Write*` method. Compile-time pressure beats code-review vigilance.
- **Discard peer addresses on Recv** — `net.UDPConn.ReadFromUDP` returns the source `*net.UDPAddr`; we throw it away. The diode does not need to know who sent a frame, and exposing that knowledge invites a future "just reply with an ACK".
- **Cancel a blocking Read by closing the conn** — standard Go idiom; cleaner than `SetReadDeadline` loops because it avoids polling.
- **Token-bucket rate limiter inline**, no dependency. Burst capped at 1 second of throughput. Lock is released across `time.Sleep` so concurrent senders aren't blocked on a single waiter.
- **Buffer size validated at constructor** — reject `WithBufferLen(n)` if `n < framing.MaxFrameLen`; a too-small read buffer would silently truncate datagrams.
- **Refuse to use builtin names as variables** (`cap`, `len`, `new`, `make`, etc.). Shadowing compiles and works but trips up readers; rename to `burst`/`limit`/etc.

### Integrity package design (from S01-4)
- **Named `Digest` type** instead of `[]byte` everywhere — prevents mixing up "32 random bytes" with "this is a hash" and forces callers through `Equal`/`Verify` rather than `bytes.Equal`.
- **`Hasher` / `Verifier` interfaces** behind the concrete `NewSHA256()` — lets ADR-0004 (signing/MAC) swap algorithms without touching call sites.
- **Pointer receiver on `Digest.Bytes()`** so the returned slice aliases the original; value receiver silently returned a slice over a stack copy.
- **Cross-check the wrapper against `crypto/sha256` directly** in tests — proves the wrapper doesn't introduce any transformation.
- **Golden test vectors** (empty, "abc", "The quick brown fox…") pin the algorithm; if it ever changes, the test fails loudly.

### Framing package implementation (from S01-3)
- **Sentinel errors** (one per validation rule) checked with `errors.Is` — lets tests assert *which* rule rejected a frame, not just that it was rejected.
- **Zero-allocation hot path** in `Encode`/`Decode` — caller passes a reusable `dst` slice; we hit ~4 GB/s with 0 B/op. Goal: keep it that way through the lifetime of the project.
- **Fuzz with a property, not just "no panic"** — every successful `Decode` must roundtrip exactly through `Encode`. This catches ambiguous parses where two distinct inputs map to the same header.
- **Golden wire test** pins the exact bytes for a known input. If it fails, ADR-0002 has been violated — revert or supersede the ADR, don't quietly update the test.
- **Constant-time hash comparison** even though SHA-256 is strong; cheap insurance and keeps the door open for switching to a MAC later.
- **Use `b.Loop()`** (Go 1.24+) in benchmarks instead of `for i := 0; i < b.N`; it's the modern idiom and avoids the `b.ResetTimer` dance.

### Frame format (locked by ADR-0002)
- **Magic + version up front** — the receiver can drop garbage in one branch, and a future v1 is explicitly *not* backward-compatible (silently dropped by v0 receivers, which is the right default for a security format).
- **All multi-byte ints big-endian.** Cheap insurance against a future ARM/RISC-V port discovering an endianness bug in production.
- **SHA-256 covers header + payload**, hash is appended (not interleaved), so a receiver can compute it in one pass after reading `payload_len`.
- **Explicit chunking** (`msg_id`, `chunk_index`, `chunk_total`) — never rely on IP fragmentation; a single lost IP fragment loses the whole datagram with no recourse.
- **Receiver drops bad frames silently**, increments counters. No return path means no place to send errors; logs at line rate would DoS the operator.
- **Reserved flag bits must be zero** — receivers reject any frame with them set, so the v0 spec can grow without ambiguity.
- **Cap payload at 1400 B** — leaves headroom under 1500-byte MTU for tunneling layers that might be added later (VLAN, GRE, WireGuard underlay).
