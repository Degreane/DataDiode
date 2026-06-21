#!/usr/bin/env bash
# lxc-teardown.sh — fully remove the DataDiode LXC test environment.
#
# Idempotent: missing pieces are skipped without error.
# Destructive: deletes the containers' rootfs and the host bridge.
#
# Bridge deletion is opt-out via KEEP_BRIDGE=1 in case other things
# share diodebr0.

set -euo pipefail

DD_TAG="teardown"
# shellcheck source=_common.sh
source "$(dirname "$(readlink -f "$0")")/_common.sh"

stop_and_destroy() {
  local name="$1"
  if ! container_exists "$name"; then
    log "container $name does not exist — skipping"
    return
  fi
  if container_running "$name"; then
    log "stopping $name"
    lxc-stop -n "$name" || warn "$name: lxc-stop returned non-zero (continuing)"
  fi
  log "destroying $name"
  lxc-destroy -n "$name" || die "lxc-destroy $name failed"
  ok "$name: removed."
}

remove_bridge() {
  if [[ "${KEEP_BRIDGE:-0}" == "1" ]]; then
    log "KEEP_BRIDGE=1 — leaving $BRIDGE in place"
    return
  fi
  if ! ip link show "$BRIDGE" >/dev/null 2>&1; then
    log "bridge $BRIDGE does not exist — skipping"
    return
  fi
  log "removing bridge $BRIDGE"
  ip link set "$BRIDGE" down || true
  ip link del "$BRIDGE"
  ok "bridge: removed."
}

main() {
  require_root
  for c in ip lxc-info lxc-stop lxc-destroy; do require_cmd "$c"; done

  stop_and_destroy "$LOW_NAME"
  stop_and_destroy "$HIGH_NAME"
  remove_bridge

  ok "done. To recreate: scripts/lxc-setup.sh && scripts/lxc-push.sh && scripts/lxc-harden.sh"
}

main "$@"
