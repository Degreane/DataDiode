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
