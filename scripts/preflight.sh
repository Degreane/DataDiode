#!/usr/bin/env bash
# preflight.sh — interim pre-deployment checker for DataDiode.
#
# Runs the §3 checklist from docs/enterprise-roadmap.md as actual
# probes on the current host. Prints PASS / WARN / FAIL per check.
# Exits 0 if no FAILs; 1 if any FAIL; 2 if invoked wrong.
#
# Stopgap until `diode --mode=preflight` ships as a real subcommand.
# When that lands, this script will be replaced by a wrapper around
# the binary subcommand so the same checks run in CI, on a fresh box,
# and during incident triage.

set -u
# Intentionally no -e: we want to record failures, not abort on first one.

VERSION='preflight.sh stopgap v0.1'

# ---- CLI parsing ----------------------------------------------------------

ROLE=""          # tx | rx | both
RX_HOST=""       # for tx-side reachability probe
RX_PORT="9999"
PSK_FILE=""
SPOOL_DIR=""
CHUNK_SIZE="1400"
QUIET=0
JSON=0

usage() {
  cat >&2 <<EOF
$VERSION

usage: preflight.sh --role=tx|rx|both [options]

required:
  --role=tx|rx|both    which side this host plays

options (tx):
  --rx-host=HOST       the rx host this tx will reach (for UDP probe)
  --rx-port=PORT       default 9999

options (rx):
  --spool-dir=PATH     default /var/spool/diode
  --rx-port=PORT       port we'll bind to; default 9999

options (both):
  --psk-file=PATH      path to PSK file (will check perms, not contents)
  --chunk-size=N       intended --chunk-size; default 1400

  --quiet              suppress PASS lines, only show WARN/FAIL
  --json               machine-readable output instead of human text
  -h|--help            this help

exits: 0 = all checks PASS or WARN, 1 = at least one FAIL, 2 = bad args
EOF
}

for arg in "$@"; do
  case "$arg" in
    --role=*)        ROLE="${arg#*=}" ;;
    --rx-host=*)     RX_HOST="${arg#*=}" ;;
    --rx-port=*)     RX_PORT="${arg#*=}" ;;
    --psk-file=*)    PSK_FILE="${arg#*=}" ;;
    --spool-dir=*)   SPOOL_DIR="${arg#*=}" ;;
    --chunk-size=*)  CHUNK_SIZE="${arg#*=}" ;;
    --quiet)         QUIET=1 ;;
    --json)          JSON=1 ;;
    -h|--help)       usage; exit 0 ;;
    *) echo "unknown flag: $arg" >&2; usage; exit 2 ;;
  esac
done

case "$ROLE" in
  tx|rx|both) : ;;
  *) echo "--role is required (tx|rx|both)" >&2; usage; exit 2 ;;
esac

[[ -z "$SPOOL_DIR" ]] && SPOOL_DIR="/var/spool/diode"

# ---- output helpers -------------------------------------------------------

RESULTS=()        # parallel arrays
RES_STATUS=()
RES_NAME=()
RES_DETAIL=()
N_FAIL=0
N_WARN=0
N_PASS=0

record() {
  local status="$1" name="$2" detail="$3"
  RES_STATUS+=("$status")
  RES_NAME+=("$name")
  RES_DETAIL+=("$detail")
  case "$status" in
    PASS) N_PASS=$((N_PASS+1)) ;;
    WARN) N_WARN=$((N_WARN+1)) ;;
    FAIL) N_FAIL=$((N_FAIL+1)) ;;
  esac
  if [[ $JSON -eq 0 ]]; then
    if [[ $QUIET -eq 1 && "$status" == "PASS" ]]; then return; fi
    local color reset
    case "$status" in
      PASS) color=$'\e[32m' ;;
      WARN) color=$'\e[33m' ;;
      FAIL) color=$'\e[31m' ;;
    esac
    reset=$'\e[0m'
    printf '  %s%-4s%s  %-40s %s\n' "$color" "$status" "$reset" "$name" "$detail"
  fi
}

# ---- common checks --------------------------------------------------------

check_binary_present() {
  if command -v diode >/dev/null 2>&1; then
    local ver
    ver="$(diode --version 2>&1 | head -1)"
    record PASS "diode binary present" "$ver"
  else
    record FAIL "diode binary present" "not on PATH; install or symlink into /usr/local/bin"
  fi
}

check_psk_file() {
  [[ -z "$PSK_FILE" ]] && { record WARN "PSK file" "not specified — only OK for unkeyed-mode deployments"; return; }
  if [[ ! -e "$PSK_FILE" ]]; then
    record FAIL "PSK file exists" "missing: $PSK_FILE"
    return
  fi
  # Permission check (Unix only)
  if [[ "$(uname -s)" != "Linux" && "$(uname -s)" != "Darwin" && "$(uname -s)" != "FreeBSD" ]]; then
    record WARN "PSK file perms" "skipped on non-Unix"
  else
    local mode
    mode="$(stat -c '%a' "$PSK_FILE" 2>/dev/null || stat -f '%Lp' "$PSK_FILE" 2>/dev/null)"
    if [[ "$mode" =~ ^[0-4]?[0-4]?0$ ]] || [[ "$mode" == "400" || "$mode" == "440" || "$mode" == "600" || "$mode" == "640" ]]; then
      record PASS "PSK file perms" "mode $mode (owner-readable only)"
    else
      record WARN "PSK file perms" "mode $mode — recommend 0400 or 0600"
    fi
  fi
  local sha
  sha="$(sha256sum "$PSK_FILE" 2>/dev/null | awk '{print $1}')"
  record PASS "PSK file sha256" "${sha:0:16}…  (compare with the other side)"
}

# ---- tx-side checks -------------------------------------------------------

check_tx_dns() {
  [[ -z "$RX_HOST" ]] && { record WARN "rx hostname resolution" "skipped — no --rx-host"; return; }
  if getent hosts "$RX_HOST" >/dev/null 2>&1; then
    local ip
    ip="$(getent hosts "$RX_HOST" | awk '{print $1; exit}')"
    record PASS "rx hostname resolves" "$RX_HOST → $ip"
  else
    record FAIL "rx hostname resolves" "$RX_HOST does not resolve"
  fi
}

check_tx_udp_reachable() {
  [[ -z "$RX_HOST" ]] && { record WARN "udp reachability" "skipped — no --rx-host"; return; }
  # UDP doesn't ACK, so we use a "no error" probe + path-MTU heuristic.
  if command -v nc >/dev/null 2>&1; then
    if echo "diode-preflight" | nc -uw1 "$RX_HOST" "$RX_PORT" 2>/dev/null; then
      record PASS "udp probe to rx" "$RX_HOST:$RX_PORT — sent without local error (NOT proof of delivery, just absence of refusal)"
    else
      record WARN "udp probe to rx" "nc reported an error — check local firewall + DNS"
    fi
  else
    record WARN "udp probe to rx" "skipped — nc not installed"
  fi
}

check_tx_mtu() {
  [[ -z "$RX_HOST" ]] && { record WARN "path MTU check" "skipped — no --rx-host"; return; }
  local probe=$(( CHUNK_SIZE - 28 ))  # chunk includes our header inside the IP payload; this is conservative
  if ping -c 1 -W 1 -M do -s "$probe" "$RX_HOST" >/dev/null 2>&1; then
    record PASS "path MTU >= chunk-size" "ping -M do -s $probe succeeded → 1500-MTU path OK for chunk-size=$CHUNK_SIZE"
  else
    record FAIL "path MTU >= chunk-size" "ping -M do -s $probe failed → MTU on path is < $((probe+28)). Lower --chunk-size or check tunnels."
  fi
}

# ---- rx-side checks -------------------------------------------------------

check_rx_rmem_max() {
  if [[ "$(uname -s)" != "Linux" ]]; then
    record WARN "kernel rmem_max" "skipped on non-Linux"
    return
  fi
  local v
  v="$(sysctl -n net.core.rmem_max 2>/dev/null)"
  if [[ -z "$v" ]]; then
    record WARN "kernel rmem_max" "sysctl unavailable"
  elif (( v >= 16777216 )); then
    record PASS "kernel rmem_max" "$v (>= 16 MiB)"
  elif (( v >= 4194304 )); then
    record WARN "kernel rmem_max" "$v — adequate but recommend 16 MiB+ for sustained throughput"
  else
    record FAIL "kernel rmem_max" "$v — too small; will silently drop frames under load. Run: sysctl -w net.core.rmem_max=16777216"
  fi
}

check_rx_netdev_backlog() {
  if [[ "$(uname -s)" != "Linux" ]]; then return; fi
  local v
  v="$(sysctl -n net.core.netdev_max_backlog 2>/dev/null)"
  if [[ -z "$v" ]]; then return; fi
  if (( v >= 5000 )); then
    record PASS "kernel netdev_max_backlog" "$v (>= 5000)"
  else
    record WARN "kernel netdev_max_backlog" "$v — bump to >= 5000 for high pps workloads"
  fi
}

check_rx_br_netfilter() {
  [[ "$(uname -s)" != "Linux" ]] && return
  local f=/proc/sys/net/bridge/bridge-nf-call-iptables
  if [[ ! -f "$f" ]]; then
    record PASS "br_netfilter not active" "module not loaded — bridge traffic flows unmolested"
    return
  fi
  if [[ "$(cat "$f")" == "0" ]]; then
    record PASS "br_netfilter disabled" "globally off"
  else
    if ip -o link show type bridge 2>/dev/null | grep -q .; then
      record FAIL "br_netfilter on with bridges present" "Docker/Podman/libvirt typically enables this and silently drops bridge traffic. Per-bridge opt-out: echo 0 > /sys/class/net/<br>/bridge/nf_call_iptables"
    else
      record WARN "br_netfilter on (no bridges)" "no bridges currently, but if you add one it will be affected"
    fi
  fi
}

check_rx_spool_dir() {
  if [[ -d "$SPOOL_DIR" ]]; then
    local owner mode
    owner="$(stat -c '%U' "$SPOOL_DIR" 2>/dev/null || stat -f '%Su' "$SPOOL_DIR" 2>/dev/null)"
    mode="$(stat -c '%a' "$SPOOL_DIR" 2>/dev/null || stat -f '%Lp' "$SPOOL_DIR" 2>/dev/null)"
    if [[ "$owner" == "root" ]]; then
      record WARN "spool dir ownership" "$SPOOL_DIR is root-owned. Recommend a dedicated 'diode' user (less blast radius)."
    else
      record PASS "spool dir ownership" "$SPOOL_DIR owned by $owner (mode $mode)"
    fi
    # Free space
    local free_gib
    free_gib="$(df -BG --output=avail "$SPOOL_DIR" 2>/dev/null | tail -1 | tr -d ' G')"
    if [[ -n "$free_gib" ]]; then
      if (( free_gib >= 50 )); then
        record PASS "spool dir free space" "${free_gib} GiB free"
      elif (( free_gib >= 10 )); then
        record WARN "spool dir free space" "${free_gib} GiB free — adequate for light workloads, set --vacuum-interval"
      else
        record FAIL "spool dir free space" "${free_gib} GiB free — too small; see docs/sizing-guide.md §5"
      fi
    fi
  else
    record WARN "spool dir exists" "$SPOOL_DIR does not exist — will be created on first start. Pre-create with: install -d -m 0750 -o diode -g diode $SPOOL_DIR"
  fi
}

check_rx_port_free() {
  if command -v ss >/dev/null 2>&1; then
    if ss -uln 2>/dev/null | awk '{print $5}' | grep -E "(^|:)$RX_PORT\$" >/dev/null; then
      record FAIL "udp port :$RX_PORT free" "something is already listening on udp/$RX_PORT — see ss -ulnp | grep :$RX_PORT"
    else
      record PASS "udp port :$RX_PORT free" "no existing listener"
    fi
  else
    record WARN "udp port :$RX_PORT free" "ss not installed; cannot verify"
  fi
}

check_clock_sync() {
  # Both sides — completed.idx records use wall-clock time; large skew
  # confuses vacuum decisions.
  if command -v timedatectl >/dev/null 2>&1; then
    local sync
    sync="$(timedatectl show --property=NTPSynchronized --value 2>/dev/null)"
    if [[ "$sync" == "yes" ]]; then
      record PASS "system clock NTP-synced" "(timedatectl reports yes)"
    else
      record WARN "system clock NTP-synced" "not synchronized — vacuum age decisions may be skewed"
    fi
  fi
}

# ---- runner ---------------------------------------------------------------

[[ $JSON -eq 0 ]] && printf '%s — role=%s\n\n' "$VERSION" "$ROLE"

check_binary_present
check_psk_file
check_clock_sync

if [[ "$ROLE" == "tx" || "$ROLE" == "both" ]]; then
  [[ $JSON -eq 0 ]] && echo "--- tx-side checks ---"
  check_tx_dns
  check_tx_udp_reachable
  check_tx_mtu
fi

if [[ "$ROLE" == "rx" || "$ROLE" == "both" ]]; then
  [[ $JSON -eq 0 ]] && echo "--- rx-side checks ---"
  check_rx_rmem_max
  check_rx_netdev_backlog
  check_rx_br_netfilter
  check_rx_spool_dir
  check_rx_port_free
fi

# ---- summary --------------------------------------------------------------

if [[ $JSON -eq 1 ]]; then
  printf '{"summary":{"pass":%d,"warn":%d,"fail":%d},"checks":[' "$N_PASS" "$N_WARN" "$N_FAIL"
  for i in "${!RES_STATUS[@]}"; do
    [[ $i -gt 0 ]] && printf ','
    printf '{"status":"%s","name":"%s","detail":"%s"}' \
      "${RES_STATUS[$i]}" \
      "${RES_NAME[$i]//\"/\\\"}" \
      "${RES_DETAIL[$i]//\"/\\\"}"
  done
  printf ']}\n'
else
  printf '\nsummary: %d pass, %d warn, %d fail\n' "$N_PASS" "$N_WARN" "$N_FAIL"
fi

if [[ $N_FAIL -gt 0 ]]; then
  exit 1
fi
exit 0
