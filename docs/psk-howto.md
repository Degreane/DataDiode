# How to create and manage a DataDiode PSK file

The `--key-file` flag on both `--mode=tx` and `--mode=rx` points to a
**pre-shared key** (PSK) file. This document is the complete reference
for how to create one, what format the file must be in, how to
distribute it safely, and how to verify it.

> **The PSK is the trust anchor.** Anyone who has it can read AND inject
> signed/encrypted frames (depending on which sprint's code you're
> running — `--key-file` enables HMAC in v2, will enable AES-256-GCM
> AEAD in v3). Treat the file like an SSH private key.

> **`--key-file` is always optional.** Running tx and rx **without**
> `--key-file` produces a plain unsigned, unencrypted transfer — the
> v0/v1/v2/v3 transports all support this. Use the keyed mode when you
> need authentication/encryption on the wire; skip it when the network
> path is already trusted (e.g., a physically isolated fiber inside an
> air-gapped facility) or when you're just experimenting on loopback.
> The receiver and sender must agree: **both** keyed with the same key,
> or **both** unkeyed.

---

## 1. The format DataDiode accepts

The key loader (`cmd/diode/key.go`) auto-detects two formats:

### A. Hex-encoded (recommended)

ASCII text. After stripping whitespace:

- Every character must be `0-9` / `a-f` / `A-F`
- Length must be **even**
- Decoded byte count must be **≥ 32** (256 bits)

Typical file looks like one long line of 64 hex characters (32 bytes = 256-bit key):

```
9ed06c0f505b60dd339b9fab143f653f98d38d9773b019d8b83349ab92f23c67
```

Or split across lines (whitespace is trimmed):

```
9ed06c0f505b60dd339b9fab143f653f
98d38d9773b019d8b83349ab92f23c67
```

Both work.

### B. Raw binary

If the file content (after a leading whitespace check) is **not** all-hex
or has odd length, the loader treats the file as raw bytes:

- Length must be **≥ 32 bytes** (256 bits)

Useful when you want to:
- Pipe directly from `/dev/urandom` without hex encoding
- Reuse an existing binary key from another system

### Validation summary

| Bytes after decode | Format detected | Loader accepts? |
|---:|:---:|:---:|
| < 32 | (either) | ❌ "key is N bytes; need at least 32" |
| ≥ 32, all-hex, even length | hex | ✅ |
| ≥ 32, not-all-hex | raw | ✅ |
| ≥ 32, all-hex, odd length | falls through to raw | ✅ |

---

## 2. Creating a PSK — pick one

### Linux / macOS (recommended path — `head -c` + `xxd`)

```bash
head -c 32 /dev/urandom | xxd -p -c 64 > psk.hex
chmod 600 psk.hex
```

- `head -c 32 /dev/urandom` — 32 bytes from the kernel's cryptographically-secure RNG
- `xxd -p -c 64` — hex-encode as one continuous line of 64 chars (no offset prefix, no whitespace)
- `chmod 600` — only the owning user can read/write (the loader warns if this is missed)

Verify what you wrote:

```bash
$ cat psk.hex
9ed06c0f505b60dd339b9fab143f653f98d38d9773b019d8b83349ab92f23c67

$ wc -c psk.hex         # 64 hex chars + 1 newline = 65 bytes
65 psk.hex

$ stat -c %a psk.hex    # mode is 600 (owner read+write only)
600
```

### Linux / macOS (alternative — `openssl`)

```bash
openssl rand -hex 32 > psk.hex
chmod 600 psk.hex
```

Same result, requires OpenSSL.

### Linux / macOS (alternative — raw binary)

```bash
head -c 32 /dev/urandom > psk.bin
chmod 600 psk.bin
# Used the same way: --key-file=psk.bin
```

The loader will detect the binary content and accept it. Slightly less
human-friendly because `cat psk.bin` prints garbage.

### Windows (PowerShell — recommended)

```powershell
$bytes = New-Object byte[] 32
[System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
[System.BitConverter]::ToString($bytes).Replace("-", "").ToLower() | Set-Content -NoNewline C:\diode\psk.hex
icacls C:\diode\psk.hex /inheritance:r /grant:r "$env:USERNAME:(R,W)"
```

Breakdown:
- `RandomNumberGenerator` — Windows CNG, the OS CSRNG
- `ToString` + `Replace("-")` + `ToLower()` — hex-encode without separators
- `icacls /inheritance:r /grant:r` — clear inheritance, grant only the current user read+write (the Windows equivalent of `chmod 600`)

Verify:

```powershell
Get-Content C:\diode\psk.hex                    # one line of 64 hex chars
(Get-Item C:\diode\psk.hex).Length              # 64 bytes (no trailing newline)
icacls C:\diode\psk.hex                         # only your user listed
```

### Windows (cmd.exe — quick and dirty)

```cmd
:: Generates a hex key via PowerShell, suitable for one-off scripts.
powershell -Command "$b=New-Object byte[] 32; [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b); [System.BitConverter]::ToString($b).Replace('-','').ToLower() | Out-File -Encoding ascii -NoNewline C:\diode\psk.hex"
```

### Any platform with Go installed

Drop this 4-line program in `genpsk.go`:

```go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

func main() {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(hex.EncodeToString(b))
}
```

Then:

```bash
go run genpsk.go > psk.hex
chmod 600 psk.hex
```

---

## 3. Permissions — the loader's stance

`loadKeyFile()` follows the SSH/PGP convention: **warn loudly, do not refuse**.

| File mode | Behavior |
|---|---|
| `0600` (owner rw, no one else) | accept silently — recommended |
| `0640`, `0644`, etc. (group or world readable) | accept BUT print `diode: warning: key file ... has permissive mode XXX (recommend 0600)` to stderr |
| File missing | hard error: `stat key file: no such file or directory` |
| File empty | hard error: `key file ... is empty` |
| Decoded < 32 bytes | hard error: `... key is N bytes; need at least 32` |

The warn-don't-refuse stance matches how operators stage keys via
configuration management (which often sets 0644 transiently before a
post-hook fixes the mode). If you need strict enforcement, wrap the
binary in a script that checks `stat` first.

---

## 4. Distribution — the operator's problem

The diode has **no return channel** by construction, which means there
is no in-band key exchange. Both sides must have the same PSK before
either can speak. Practical methods, ranked by paranoia:

| Method | Trust assumption | Notes |
|---|---|---|
| Physical USB stick, hand-carried | The carrier | The "sneakernet" approach. Standard for air-gapped deployments. |
| SSH/SCP from a trusted bastion | SSH host keys + your password | Common for internal LANs. `scp -p psk.hex high-side:/etc/diode/psk.hex && ssh high-side chmod 600 /etc/diode/psk.hex`. |
| Configuration management (Ansible/Puppet/Salt) | The CM server + its TLS | Fine if the CM channel is already part of your trust boundary. |
| Encrypted secret manager (Vault, AWS Secrets Manager) | The secret-mgmt service | Fetch on startup; never write to disk. Adapt with a wrapper script that streams to `/dev/shm/psk` and passes that path. |
| **Plaintext email / Slack / chat** | None | **Do not.** |

A safe rotation procedure:

1. Generate the new PSK on the receiver host (`head -c 32 /dev/urandom | xxd -p -c 64 > psk-new.hex; chmod 600 psk-new.hex`).
2. Copy `psk-new.hex` to the sender host out-of-band.
3. Stop the old `diode --mode=rx`; start a new one with `--key-file=psk-new.hex`.
4. On the sender, switch tx jobs to `--key-file=psk-new.hex`.
5. Wipe `psk-old.hex` from both hosts (`shred -u psk-old.hex` on Linux).

There is no "two keys at once" overlap window in the current code; you
have a brief window of "no transfers possible" during step 3. A future
ADR may add receiver-side multi-key support.

---

## 5. Verification

### Does the loader accept my file?

The fastest check: run the receiver with `--key-file` pointed at it.
It either binds normally and prints

```
diode rx: enforcing PSK auth (32-byte key from /path/to/psk.hex);
unsigned frames will be dropped
```

…or it exits with one of the error messages above.

### Does the same file exist on both sides byte-identically?

Compute sha256 on both hosts and compare:

```bash
# both hosts
sha256sum psk.hex
# 4f3a...  psk.hex      ← must match exactly
```

If they differ, you have **two different keys** and no frames will be
delivered — the receiver will reject every signed frame from the sender.

---

## 6. Common mistakes

| Symptom | Likely cause |
|---|---|
| `diode --mode=rx: ... key file ... no such file or directory` | Wrong path, missing file, or relative path resolved against the wrong cwd. |
| `... key is 0 bytes; need at least 32` | File is empty, often because a redirect (`>`) was followed by Ctrl-C. |
| `... hex-decoded key is 16 bytes; need at least 32` | Used `head -c 16` instead of `head -c 32`, or hex string was half what you intended. |
| Receiver stats show `data_dropped=N`, `completed=0` despite no errors | Sender and receiver have **different** keys. Compare `sha256sum psk.hex` on both. |
| `diode: warning: key file ... has permissive mode 644` | Run `chmod 600 psk.hex`. Not fatal but worth fixing. |
| Operator pastes hex into chat and key gets word-wrapped with spaces | The loader strips whitespace, so this generally still works — but the key has just been disclosed in the chat history. **Rotate.** |
| File made on Windows, used on Linux, fails parsing | CRLF line endings. The loader handles trailing whitespace, but a stray `^M` in the middle of a hex string will fail. Save as LF (`dos2unix psk.hex`). |

---

## 7. What the key actually protects

This is the same answer in any sprint — only the underlying primitive
changes:

- **v2 (today, ADR-0004 HMAC-SHA256):** with `--key-file`, every frame
  carries an HMAC trailer. Receiver rejects unsigned frames or signed
  frames with the wrong key. Closes threat-model **S-1 (frame spoofing)**
  and **T-1 (in-flight tampering)**. **Does not encrypt the payload.**
- **v3 (next, ADR-0008 AES-256-GCM AEAD):** with `--key-file`, every
  frame's payload is encrypted AND authenticated. Same operator UX;
  stronger security. Also closes **I-1 (plaintext on the wire)**.

The same PSK file works for both modes. v3 will internally HKDF-derive
a domain-separated AEAD subkey from the 32-byte PSK; you keep the same
key file across the upgrade.

---

## 8. TL;DR

**With encryption/authentication:**

```bash
# Linux / macOS
head -c 32 /dev/urandom | xxd -p -c 64 > psk.hex
chmod 600 psk.hex
sha256sum psk.hex           # remember this value for verification

# distribute psk.hex to the other host out-of-band
# verify sha256sum on both hosts matches

# tx side
diode --mode=tx --dst=10.0.0.20:9999 --send-file=report.pdf --key-file=psk.hex

# rx side
diode --mode=rx --listen=:9999 --files-to=/srv/incoming --key-file=psk.hex
```

**Without (plain transfer, no key, no encryption, no auth):**

```bash
# tx side — note the absence of --key-file
diode --mode=tx --dst=10.0.0.20:9999 --send-file=report.pdf

# rx side — also no --key-file
diode --mode=rx --listen=:9999 --files-to=/srv/incoming
```

Both must agree: keyed or unkeyed. A keyed sender talking to an unkeyed
receiver gets every frame dropped (and vice versa). The receiver's
stats line tells you when this happens — see the troubleshooting
section in `docs/tutorial.md`.
