#!/usr/bin/env bash
# lxc-harden.sh — apply nftables rules inside the LXC containers that
# enforce the diode discipline at the network layer (belt-and-braces
# on top of the application-layer guarantee that diode --mode=rx
# opens no outbound sockets).
#
#   diode-low  : allow OUT  UDP/DIODE_PORT → HIGH_IP_ADDR only
#   diode-high : DROP all OUT except loopback (no return path)
#
# Idempotent: the named table is flushed and rebuilt each run, so
# re-running converges to the desired state.

set -euo pipefail

DD_TAG="harden"
# shellcheck source=_common.sh
source "$(dirname "$(readlink -f "$0")")/_common.sh"

apply_low() {
  local name="$LOW_NAME"
  ensure_container_running "$name"
  log "$name: installing egress allow-list (UDP/$DIODE_PORT → $HIGH_IP_ADDR)"
  lxc-attach -n "$name" -- nft -f - <<EOF
table inet diode { }
delete table inet diode
table inet diode {
  chain output {
    type filter hook output priority 0; policy drop;
    oifname "lo" accept
    ip daddr $HIGH_IP_ADDR udp dport $DIODE_PORT accept
    # Allow ICMP echo for liveness checks during development; comment out
    # in production for stricter posture.
    ip daddr $HIGH_IP_ADDR icmp type echo-request accept
  }
}
EOF
  ok "$name: hardened."
}

apply_high() {
  local name="$HIGH_NAME"
  ensure_container_running "$name"
  log "$name: installing DROP-all-egress rule (no return path)"
  lxc-attach -n "$name" -- nft -f - <<EOF
table inet diode { }
delete table inet diode
table inet diode {
  chain output {
    type filter hook output priority 0; policy drop;
    oifname "lo" accept
  }
}
EOF
  ok "$name: hardened."
}

verify() {
  local name="$1"
  log "$name: current ruleset:"
  lxc-attach -n "$name" -- nft list table inet diode || warn "$name: nft list failed"
}

main() {
  require_root
  for c in lxc-info lxc-attach; do require_cmd "$c"; done

  apply_low
  apply_high

  echo
  verify "$LOW_NAME"
  echo
  verify "$HIGH_NAME"
  echo
  ok "done. Receiver cannot route any packet back to the sender."
}

main "$@"
