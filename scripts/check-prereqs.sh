#!/usr/bin/env bash
# check-prereqs.sh — verify host has everything the LXC demo needs.
# Prints a clear "missing X — install with Y" message and exits non-zero
# on the first miss. Safe to run as a non-root user.

set -uo pipefail

missing=()

want() {
  local cmd="$1" hint="$2"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    printf 'MISSING: %-16s install with: %s\n' "$cmd" "$hint"
    missing+=("$cmd")
  else
    printf 'OK     : %-16s -> %s\n' "$cmd" "$(command -v "$cmd")"
  fi
}

want go          "https://go.dev/dl  or  sudo dnf install golang"
want lxc-create  "sudo dnf install lxc lxc-templates"
want lxc-start   "sudo dnf install lxc"
want lxc-attach  "sudo dnf install lxc"
want lxc-stop    "sudo dnf install lxc"
want lxc-destroy "sudo dnf install lxc"
want lxc-info    "sudo dnf install lxc"
want lxc-wait    "sudo dnf install lxc"
want nft         "sudo dnf install nftables"
want ip          "sudo dnf install iproute"
want sha256sum   "sudo dnf install coreutils"

echo
if (( ${#missing[@]} > 0 )); then
  echo "Missing: ${missing[*]}"
  echo "Hint   : sudo dnf install -y lxc lxc-templates nftables golang"
  exit 1
fi
echo "All prerequisites present."
