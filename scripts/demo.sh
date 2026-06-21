#!/usr/bin/env bash
# demo.sh — end-to-end DataDiode v3 demo across two LXC containers.
#
# NOTE: The v3 protocol + keyed AEAD flow is exhaustively covered by
# `make test` (24 E2E tests + integration). This demo script is the
# operator-facing showcase; if it hits an LXC-environmental issue on
# your host (Rocky 9 minimal images sometimes need extra net.ipv6
# tuning), the unit/E2E tests are the authoritative correctness
# proof. Run `make test` first if this script misbehaves.
#
# Steps:
#   1. (Re)provision bridge + containers (idempotent)
#   2. Build + push the diode binary into both
#   3. Apply nftables egress rules (diode-high has no return path)
#   4. Generate a PSK on the host with `diode --mode=psk` and stage it
#      into both containers (matching sha256 verified)
#   5. Stage the payload as a file inside diode-low
#   6. Start diode --mode=rx in diode-high with --files-to + --key-file
#   7. Run diode --mode=tx in diode-low with --send-file + --key-file
#   8. Verify the file was reconstructed byte-identically AND the wire
#      contents (snapshot of one DATA frame) show no plaintext markers
#
# Re-runnable: previous state is detected and reused.
# Cleanup: leaves containers running so you can poke at them. Run
# scripts/lxc-teardown.sh to remove everything.

set -euo pipefail

DD_TAG="demo"
# shellcheck source=_common.sh
source "$(dirname "$(readlink -f "$0")")/_common.sh"

# Path to a test payload on the host (override via env). Default = the
# Sprint 01 plan itself, a non-trivial multi-chunk file with
# distinctive plaintext that lets us prove the wire is encrypted.
: "${PAYLOAD_FILE:=$DD_REPO_ROOT/docs/sprints/sprint-01-mvp.md}"
: "${PLAINTEXT_MARKER:=Sprint Goal}"  # phrase we expect inside PAYLOAD_FILE

# Tunables for the demo run.
: "${CHUNK_SIZE:=1400}"
: "${REDUNDANCY:=2}"

# Where state lives. Defaults are inside container rootfs.
REMOTE_FILES_TO="/srv/diode-incoming"      # where rx writes completed files
REMOTE_SPOOL="/var/spool/diode"            # where rx stages in-flight chunks
REMOTE_PAYLOAD_DIR="/tmp/diode-payload"    # where tx reads from
REMOTE_KEY="/etc/diode/psk.hex"            # PSK inside each container
REMOTE_SENDER_STATE="/var/lib/diode"       # sender's manifest + archive

PAYLOAD_BASENAME="$(basename "$PAYLOAD_FILE")"
RX_LOG="$(mktemp /tmp/diode-rx-log.XXXXXX)"
LOCAL_KEY="$(mktemp /tmp/diode-psk.XXXXXX)"
LOCAL_PCAP="$(mktemp /tmp/diode-wire.XXXXXX.pcap)"

cleanup() {
  rm -f "$RX_LOG" "$LOCAL_KEY" "$LOCAL_PCAP"
  lxc-attach -n "$HIGH_NAME" -- pkill -SIGINT -f 'diode --mode=rx' 2>/dev/null || true
}
trap cleanup EXIT

step() { printf '\n%b== %s ==%b\n' "$_c_blue" "$*" "$_c_reset"; }

# host-side veth name for a container's eth0 (re-used from lxc-harden).
host_veth_for() {
  local name="$1" pid line ifindex
  pid="$(lxc-info -n "$name" -p -H 2>/dev/null || true)"
  [[ -n "$pid" ]] || die "$name: not running"
  line="$(nsenter -t "$pid" -n ip -o link show eth0 2>/dev/null || true)"
  ifindex="$(echo "$line" | sed -n 's/.*@if\([0-9]\+\):.*/\1/p')"
  ip -o link show | awk -F': ' -v i="$ifindex" '$1==i {sub(/@.*/,"",$2); print $2; exit}'
}

main() {
  require_root
  for c in lxc-info lxc-attach lxc-start lxc-wait nsenter sha256sum mktemp pkill grep awk tcpdump; do
    require_cmd "$c"
  done
  [[ -f "$PAYLOAD_FILE" ]] || die "payload not found: $PAYLOAD_FILE"

  step "1. Ensure bridge + containers exist (lxc-setup.sh)"
  "$DD_REPO_ROOT/scripts/lxc-setup.sh"

  step "2. Build + push binary (lxc-push.sh)"
  "$DD_REPO_ROOT/scripts/lxc-push.sh"

  step "3. Apply nft egress rules (lxc-harden.sh)"
  "$DD_REPO_ROOT/scripts/lxc-harden.sh"

  step "4. Generate a PSK on the host and distribute to both containers"
  rm -f "$LOCAL_KEY"
  "$DD_BINARY" --mode=psk --file="$LOCAL_KEY" --force
  local key_sha
  key_sha="$(sha256sum "$LOCAL_KEY" | awk '{print $1}')"
  log "host PSK sha256 = $key_sha"
  for name in "$LOW_NAME" "$HIGH_NAME"; do
    install -d -m 0755 "$LXC_DIR/$name/rootfs$(dirname "$REMOTE_KEY")"
    install -m 0600 "$LOCAL_KEY" "$LXC_DIR/$name/rootfs$REMOTE_KEY"
    local in_sha
    in_sha="$(lxc-attach -n "$name" -- sha256sum "$REMOTE_KEY" | awk '{print $1}')"
    [[ "$in_sha" == "$key_sha" ]] || die "$name: PSK sha mismatch after push ($in_sha vs $key_sha)"
    log "$name: PSK staged at $REMOTE_KEY (sha matches)"
  done

  step "5. Stage payload as a file inside $LOW_NAME"
  local payload_size
  payload_size="$(stat -c '%s' "$PAYLOAD_FILE")"
  log "payload: $PAYLOAD_FILE ($payload_size bytes, basename=$PAYLOAD_BASENAME)"
  install -d -m 0755 "$LXC_DIR/$LOW_NAME/rootfs$REMOTE_PAYLOAD_DIR"
  install -m 0644 "$PAYLOAD_FILE" "$LXC_DIR/$LOW_NAME/rootfs$REMOTE_PAYLOAD_DIR/$PAYLOAD_BASENAME"

  step "6. Prepare receiver sink + spool in $HIGH_NAME"
  install -d -m 0755 "$LXC_DIR/$HIGH_NAME/rootfs$REMOTE_FILES_TO" "$LXC_DIR/$HIGH_NAME/rootfs$REMOTE_SPOOL"
  rm -f "$LXC_DIR/$HIGH_NAME/rootfs$REMOTE_FILES_TO/$PAYLOAD_BASENAME"
  # Prepare sender state dir on the low side too.
  install -d -m 0755 "$LXC_DIR/$LOW_NAME/rootfs$REMOTE_SENDER_STATE"

  step "7. Start tcpdump on the bridge (capture 8 frames for wire-encryption proof)"
  log "capturing on $BRIDGE (UDP/$DIODE_PORT, ≤8 packets, 10s cap)"
  # Capture in background; bounded by both packet count (-c 8) and a
  # 10-second wall-clock timeout so the demo can't hang if the bridge
  # never sees the expected number of frames.
  timeout 10 tcpdump -i "$BRIDGE" -c 8 -w "$LOCAL_PCAP" -n udp port "$DIODE_PORT" >/dev/null 2>&1 &
  local TCP=$!
  sleep 0.3

  step "8. Start diode --mode=rx in $HIGH_NAME (--files-to + --key-file)"
  # Bind to the high-side IPv4 explicitly to avoid Go's dual-stack
  # binding picking IPv6 on some container kernels.
  lxc-attach -n "$HIGH_NAME" -- \
    sh -c "nohup $IN_CONTAINER_BIN --mode=rx \
                 --listen=$HIGH_IP_ADDR:$DIODE_PORT \
                 --files-to=$REMOTE_FILES_TO \
                 --spool=$REMOTE_SPOOL \
                 --key-file=$REMOTE_KEY \
                 >$RX_LOG 2>&1 </dev/null & disown; sleep 0.2"
  local deadline=$((SECONDS + 5))
  while (( SECONDS < deadline )); do
    grep -q 'listening on' "$LXC_DIR/$HIGH_NAME/rootfs$RX_LOG" 2>/dev/null && break
    sleep 0.1
  done
  # The banner prints right after Listen() returns; give the goroutine
  # that owns the read loop a beat to start consuming before tx fires.
  sleep 0.5

  step "9. Run diode --mode=tx in $LOW_NAME (--send-file + --key-file)"
  lxc-attach -n "$LOW_NAME" -- \
    "$IN_CONTAINER_BIN" --mode=tx \
        --dst="$HIGH_IP_ADDR:$DIODE_PORT" \
        --send-file="$REMOTE_PAYLOAD_DIR/$PAYLOAD_BASENAME" \
        --key-file="$REMOTE_KEY" \
        --chunk-size="$CHUNK_SIZE" \
        --redundancy="$REDUNDANCY" \
        --sender-state="$REMOTE_SENDER_STATE"

  step "10. Drain + stop receiver"
  sleep 0.5
  lxc-attach -n "$HIGH_NAME" -- pkill -SIGINT -f 'diode --mode=rx' || true
  sleep 0.3
  wait "$TCP" 2>/dev/null || true

  step "11. Verify byte-identical delivery"
  local sink_host="$LXC_DIR/$HIGH_NAME/rootfs$REMOTE_FILES_TO/$PAYLOAD_BASENAME"
  [[ -f "$sink_host" ]] || die "sink file not produced at $sink_host"
  local in_sha out_sha
  in_sha="$(sha256sum "$PAYLOAD_FILE" | awk '{print $1}')"
  out_sha="$(sha256sum "$sink_host"   | awk '{print $1}')"
  log "input  sha256 = $in_sha  ($(stat -c '%s' "$PAYLOAD_FILE") bytes)"
  log "output sha256 = $out_sha  ($(stat -c '%s' "$sink_host") bytes)"
  if [[ "$in_sha" == "$out_sha" ]]; then
    ok "MATCH — diode roundtrip succeeded across $LOW_NAME → $HIGH_NAME (AEAD-encrypted)"
  else
    err "MISMATCH — bytes diverged across the diode"
    exit 1
  fi

  step "12. Verify the wire was encrypted (plaintext marker absent)"
  if [[ -n "${PLAINTEXT_MARKER:-}" ]]; then
    if strings "$LOCAL_PCAP" | grep -qF "$PLAINTEXT_MARKER"; then
      err "WIRE LEAK — plaintext marker \"$PLAINTEXT_MARKER\" found in pcap"
      exit 1
    fi
    ok "WIRE ENCRYPTED — marker \"$PLAINTEXT_MARKER\" not present in $(stat -c '%s' "$LOCAL_PCAP")-byte pcap"
  fi

  echo
  log "receiver stats line:"
  grep -E 'soh_seen|stopped' "$LXC_DIR/$HIGH_NAME/rootfs$RX_LOG" || tail -3 "$LXC_DIR/$HIGH_NAME/rootfs$RX_LOG"

  echo
  log "sender manifest (last entry):"
  tail -1 "$LXC_DIR/$LOW_NAME/rootfs$REMOTE_SENDER_STATE/manifest.jsonl" 2>/dev/null | "${DD_BINARY%/*}/../bin/diode" --mode=manifest --sender-state="$LXC_DIR/$LOW_NAME/rootfs$REMOTE_SENDER_STATE" 2>/dev/null | tail -3 || \
    cat "$LXC_DIR/$LOW_NAME/rootfs$REMOTE_SENDER_STATE/manifest.jsonl" 2>/dev/null | tail -1
}

main "$@"
