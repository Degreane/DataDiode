#!/usr/bin/env bash
# demo.sh — end-to-end DataDiode demo across two LXC containers.
#
# Steps:
#   1. (Re)provision bridge + containers (idempotent)
#   2. Build + push the diode binary into both
#   3. Apply nftables egress rules (diode-high has no return path)
#   4. Launch diode --mode=rx in diode-high, writing to /tmp/sink
#   5. Pipe a sample file into diode --mode=tx in diode-low
#   6. Verify the file was reconstructed byte-identically on the high side
#   7. Print the receiver's stats line and exit
#
# Re-runnable: previous state is detected and reused.
# Cleanup: leaves containers running so you can poke at them. Run
# scripts/lxc-teardown.sh to remove everything.

set -euo pipefail

DD_TAG="demo"
# shellcheck source=_common.sh
source "$(dirname "$(readlink -f "$0")")/_common.sh"

# Path to a test payload on the host (override via env). Default = the
# Sprint 01 plan itself, a non-trivial multi-chunk file.
: "${PAYLOAD_FILE:=$DD_REPO_ROOT/docs/sprints/sprint-01-mvp.md}"

# Tunables for the demo run.
: "${CHUNK:=1400}"
: "${REDUNDANCY:=2}"

REMOTE_PAYLOAD="/tmp/diode-payload.in"
REMOTE_SINK="/tmp/diode-sink.out"
LOCAL_VERIFY="$(mktemp /tmp/diode-verify.XXXXXX)"
RX_LOG="$(mktemp /tmp/diode-rx-log.XXXXXX)"

cleanup() {
  rm -f "$LOCAL_VERIFY" "$RX_LOG"
  # Stop any lingering diode-rx inside the container.
  lxc-attach -n "$HIGH_NAME" -- pkill -SIGINT -f 'diode --mode=rx' 2>/dev/null || true
}
trap cleanup EXIT

step() { printf '\n%b== %s ==%b\n' "$_c_blue" "$*" "$_c_reset"; }

main() {
  require_root
  for c in lxc-info lxc-attach lxc-start lxc-wait diff sha256sum mktemp pkill; do require_cmd "$c"; done
  [[ -f "$PAYLOAD_FILE" ]] || die "payload not found: $PAYLOAD_FILE"

  step "1. Ensure bridge + containers exist (lxc-setup.sh)"
  "$DD_REPO_ROOT/scripts/lxc-setup.sh"

  step "2. Build + push binary (lxc-push.sh)"
  "$DD_REPO_ROOT/scripts/lxc-push.sh"

  step "3. Apply nft egress rules (lxc-harden.sh)"
  "$DD_REPO_ROOT/scripts/lxc-harden.sh"

  step "4. Stage payload inside $LOW_NAME"
  local payload_size
  payload_size="$(stat -c '%s' "$PAYLOAD_FILE")"
  log "payload: $PAYLOAD_FILE ($payload_size bytes)"
  install -m 0644 "$PAYLOAD_FILE" "$LXC_DIR/$LOW_NAME/rootfs$REMOTE_PAYLOAD"

  step "5. Reset sink in $HIGH_NAME"
  : > "$LXC_DIR/$HIGH_NAME/rootfs$REMOTE_SINK" || true
  rm -f "$LXC_DIR/$HIGH_NAME/rootfs$REMOTE_SINK"

  step "6. Start diode --mode=rx in $HIGH_NAME (background)"
  # Run detached inside the container; redirect stderr to a host-visible log.
  lxc-attach -n "$HIGH_NAME" --                                              \
    sh -c "nohup $IN_CONTAINER_BIN --mode=rx                                  \
                 --listen=0.0.0.0:$DIODE_PORT                                 \
                 --out=$REMOTE_SINK                                           \
                 >$RX_LOG 2>&1 </dev/null & disown; sleep 0.2"

  # Poll the container's stderr log until the listening banner shows up.
  local deadline=$((SECONDS + 5))
  while (( SECONDS < deadline )); do
    if grep -q 'listening on' "$LXC_DIR/$HIGH_NAME/rootfs$RX_LOG" 2>/dev/null; then break; fi
    sleep 0.1
  done

  step "7. Run diode --mode=tx in $LOW_NAME"
  lxc-attach -n "$LOW_NAME" --                                                \
    sh -c "cat $REMOTE_PAYLOAD                                                \
           | $IN_CONTAINER_BIN --mode=tx                                      \
               --dst=$HIGH_IP_ADDR:$DIODE_PORT                                \
               --chunk=$CHUNK                                                 \
               --redundancy=$REDUNDANCY"

  step "8. Drain + stop receiver"
  # Give in-flight datagrams a moment to land.
  sleep 0.5
  lxc-attach -n "$HIGH_NAME" -- pkill -SIGINT -f 'diode --mode=rx' || true
  sleep 0.3

  step "9. Verify"
  local sink_host="$LXC_DIR/$HIGH_NAME/rootfs$REMOTE_SINK"
  [[ -f "$sink_host" ]] || die "sink file not produced at $sink_host"
  install -m 0644 "$sink_host" "$LOCAL_VERIFY"

  local in_sha out_sha
  in_sha="$(sha256sum "$PAYLOAD_FILE"  | awk '{print $1}')"
  out_sha="$(sha256sum "$LOCAL_VERIFY" | awk '{print $1}')"
  log "input  sha256 = $in_sha  ($(stat -c '%s' "$PAYLOAD_FILE") bytes)"
  log "output sha256 = $out_sha  ($(stat -c '%s' "$LOCAL_VERIFY") bytes)"

  echo
  if [[ "$in_sha" == "$out_sha" ]]; then
    ok "MATCH — diode roundtrip succeeded across $LOW_NAME → $HIGH_NAME"
  else
    err "MISMATCH — bytes diverged across the diode"
    diff <(xxd "$PAYLOAD_FILE") <(xxd "$LOCAL_VERIFY") | head -40 || true
    exit 1
  fi

  echo
  log "receiver stats line:"
  grep -E 'frames_in|stopped' "$LXC_DIR/$HIGH_NAME/rootfs$RX_LOG" || tail -3 "$LXC_DIR/$HIGH_NAME/rootfs$RX_LOG"
}

main "$@"
