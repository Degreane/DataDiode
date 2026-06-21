#!/usr/bin/env bash
# lxc-push.sh — build the diode binary on the host and push it into
# both LXC containers atomically.
#
# Idempotent: safe to re-run after editing code; the in-container
# binary is replaced via mv (atomic), so a running diode process is
# not disturbed mid-execution (it keeps its mmap'd inode until it
# exits, then the next start uses the new file).
#
# Requires: scripts/lxc-setup.sh has been run successfully.

set -euo pipefail

DD_TAG="push"
# shellcheck source=_common.sh
source "$(dirname "$(readlink -f "$0")")/_common.sh"

build_binary() {
  require_cmd go
  mkdir -p "$DD_BIN_DIR"
  log "building $DD_BINARY (CGO_ENABLED=0)"
  ( cd "$DD_REPO_ROOT" && CGO_ENABLED=0 go build -o "$DD_BINARY" ./cmd/diode )
  [[ -x "$DD_BINARY" ]] || die "build produced no executable at $DD_BINARY"
  log "built: $(stat -c '%s' "$DD_BINARY") bytes, $(file -b "$DD_BINARY")"
}

push_to() {
  local name="$1"
  ensure_container_running "$name"

  local rootfs="$LXC_DIR/$name/rootfs"
  local dst_dir="$rootfs/usr/local/bin"
  local dst="$rootfs$IN_CONTAINER_BIN"
  local tmp="${dst}.new"

  [[ -d "$dst_dir" ]] || install -d -m 0755 "$dst_dir"

  # Hash-compare to skip work if the binary is already current.
  if [[ -f "$dst" ]]; then
    local cur new
    cur="$(sha256sum "$dst"      | awk '{print $1}')"
    new="$(sha256sum "$DD_BINARY" | awk '{print $1}')"
    if [[ "$cur" == "$new" ]]; then
      log "$name: binary already current (sha256 ${cur:0:12}…) — skipping"
      return
    fi
  fi

  log "$name: installing into ${IN_CONTAINER_BIN}"
  install -m 0755 "$DD_BINARY" "$tmp"
  mv -f "$tmp" "$dst"     # atomic rename within the same filesystem
  ok  "$name: pushed."
}

main() {
  require_root
  for c in lxc-info lxc-wait install mv sha256sum stat file go; do require_cmd "$c"; done

  build_binary
  push_to "$LOW_NAME"
  push_to "$HIGH_NAME"

  ok "done."
  log "verify inside a container:  sudo lxc-attach -n $LOW_NAME -- $IN_CONTAINER_BIN --version"
}

main "$@"
