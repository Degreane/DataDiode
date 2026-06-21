#!/usr/bin/env bash
# lxc-harden.sh — apply nftables rules ON THE HOST that enforce the
# diode discipline at the network layer.
#
# Rationale: the minimal Rocky/Alpine LXC images don't ship nft, and
# the diode bridge has no internet for dnf to install it. Enforcing
# on the host is also a stronger guarantee — a future compromised
# container cannot rewrite host rules.
#
# Layout:
#
#   table inet datadiode {
#     chain forward {
#       type filter hook forward priority 0; policy accept;
#       # FROM diode-high (high side): drop EVERYTHING. No return path.
#       iifname <high-veth> drop
#       # FROM diode-low to diode-high: allow only UDP/DIODE_PORT.
#       iifname <low-veth> oifname <high-veth> udp dport DIODE_PORT accept
#       iifname <low-veth> oifname <high-veth> drop
#     }
#   }
#
# Idempotent: the named table is flushed and rebuilt each run.

set -euo pipefail

DD_TAG="harden"
# shellcheck source=_common.sh
source "$(dirname "$(readlink -f "$0")")/_common.sh"

# host_veth_for <container> — print the host-side veth name peer of
# the container's eth0. Parses the "eth0@if13" notation from `ip link`
# inside the container's netns: the digits after "@if" are the peer
# ifindex on the host. (/sys is host-mounted and won't show the
# container's iflink, so we go through netlink instead.)
host_veth_for() {
  local name="$1"
  local pid line ifindex
  pid="$(lxc-info -n "$name" -p -H 2>/dev/null || true)"
  [[ -n "$pid" ]] || die "$name: not running"
  line="$(nsenter -t "$pid" -n ip -o link show eth0 2>/dev/null || true)"
  [[ -n "$line" ]] || die "$name: eth0 not present in netns"
  ifindex="$(echo "$line" | sed -n 's/.*@if\([0-9]\+\):.*/\1/p')"
  [[ -n "$ifindex" ]] || die "$name: could not parse peer ifindex from: $line"
  ip -o link show | awk -F': ' -v i="$ifindex" '$1==i {sub(/@.*/,"",$2); print $2; exit}'
}

apply_host_rules() {
  local low_veth="$1" high_veth="$2"
  log "applying host nft (bridge family): drop egress from $high_veth; allow UDP/$DIODE_PORT from $low_veth → $high_veth"
  # NOTE: we use the bridge family rather than inet, because the
  # diode containers share an L2 segment (diodebr0) and the inet
  # forward hook normally does NOT see bridge-forwarded traffic
  # unless br_netfilter is loaded. The bridge family hooks at L2,
  # so it sees every packet the bridge would forward.
  nft -f - <<EOF
table bridge datadiode { }
delete table bridge datadiode
table bridge datadiode {
  chain forward {
    type filter hook forward priority 0; policy accept;
    # 1. drop EVERYTHING coming FROM the high-side veth — the receiver
    #    container cannot egress to anywhere, including replies.
    iifname "$high_veth" drop
    # 2. from low → high, allow only UDP/DIODE_PORT (L4 matching in
    #    bridge family requires ether type ip first).
    iifname "$low_veth" oifname "$high_veth" ether type ip ip protocol udp udp dport $DIODE_PORT accept
    # 3. drop any other low → high traffic (ICMP, other UDP/TCP, etc.).
    iifname "$low_veth" oifname "$high_veth" drop
  }
}
EOF
}

main() {
  require_root
  for c in lxc-info nsenter ip nft awk; do require_cmd "$c"; done

  ensure_container_running "$LOW_NAME"
  ensure_container_running "$HIGH_NAME"

  local low_veth high_veth
  low_veth="$(host_veth_for "$LOW_NAME")"
  high_veth="$(host_veth_for "$HIGH_NAME")"
  [[ -n "$low_veth"  ]] || die "could not resolve host veth for $LOW_NAME"
  [[ -n "$high_veth" ]] || die "could not resolve host veth for $HIGH_NAME"
  log "$LOW_NAME  veth on host = $low_veth"
  log "$HIGH_NAME veth on host = $high_veth"

  apply_host_rules "$low_veth" "$high_veth"

  echo
  log "current host ruleset (bridge datadiode):"
  nft list table bridge datadiode || warn "nft list failed"
  echo
  ok "done. Receiver's veth ($high_veth) cannot egress; sender can only reach UDP/$DIODE_PORT on the high side."
}

main "$@"
