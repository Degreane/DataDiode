#!/usr/bin/env bash
# lxc-setup.sh — idempotently provision the DataDiode LXC test environment.
#
# Creates:
#   - host bridge        : diodebr0   (10.99.0.1/24, no NAT, no upstream)
#   - low-side container : diode-low  (10.99.0.10)
#   - high-side container: diode-high (10.99.0.20)
#
# Re-running is safe: anything that already exists is left alone.
# Requires: lxc, lxc-templates, iproute2, nftables. Run as root (or with sudo).

set -euo pipefail

BRIDGE="${BRIDGE:-diodebr0}"
BRIDGE_CIDR="${BRIDGE_CIDR:-10.99.0.1/24}"
LOW_NAME="${LOW_NAME:-diode-low}"
HIGH_NAME="${HIGH_NAME:-diode-high}"
LOW_IP="${LOW_IP:-10.99.0.10/24}"
HIGH_IP="${HIGH_IP:-10.99.0.20/24}"
: "${DIST:=rockylinux}"   # linuxcontainers.org no longer publishes Fedora images
: "${RELEASE:=9}"          # rockylinux 9 ships nft in base
: "${ARCH:=amd64}"

LXC_DIR="${LXC_DIR:-/var/lib/lxc}"

log()  { printf '\033[1;34m[setup]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn ]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fail ]\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
  [[ $EUID -eq 0 ]] || die "must run as root (try: sudo $0)"
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1 (install it and retry)"
}

# ---------- bridge ----------------------------------------------------------

ensure_bridge() {
  if ip link show "$BRIDGE" >/dev/null 2>&1; then
    log "bridge $BRIDGE already exists — skipping create"
  else
    log "creating bridge $BRIDGE"
    ip link add name "$BRIDGE" type bridge
  fi

  if ip -4 addr show dev "$BRIDGE" | grep -q "${BRIDGE_CIDR%/*}"; then
    log "bridge $BRIDGE already has address $BRIDGE_CIDR"
  else
    log "assigning $BRIDGE_CIDR to $BRIDGE"
    ip addr add "$BRIDGE_CIDR" dev "$BRIDGE"
  fi

  ip link set "$BRIDGE" up

  # If br_netfilter is loaded (e.g. because Docker is also installed
  # on this host), bridge-forwarded packets visit the inet/ip FORWARD
  # chain. Docker often sets that chain's policy to DROP, which would
  # silently break our diode traffic. Opt this bridge out so our
  # `bridge` family rules in lxc-harden.sh are the sole authority.
  # The files exist only when br_netfilter is loaded; if it's not,
  # there is nothing to disable and we silently skip.
  for f in nf_call_iptables nf_call_ip6tables nf_call_arptables; do
    if [[ -w "/sys/class/net/$BRIDGE/bridge/$f" ]]; then
      echo 0 > "/sys/class/net/$BRIDGE/bridge/$f"
    fi
  done
  log "$BRIDGE: br_netfilter disabled for this bridge (bridge family rules are authoritative)"
}

# ---------- containers ------------------------------------------------------

container_exists() {
  [[ -d "$LXC_DIR/$1" ]]
}

container_running() {
  lxc-info -n "$1" -s 2>/dev/null | grep -q 'RUNNING'
}

ensure_container() {
  local name="$1" ip_cidr="$2"

  if container_exists "$name"; then
    log "container $name already exists — skipping create"
  else
    log "creating container $name (template: download $DIST/$RELEASE/$ARCH)"
    lxc-create -n "$name" -t download -- \
        --dist "$DIST" --release "$RELEASE" --arch "$ARCH"
  fi

  ensure_network_config "$name" "$ip_cidr"
}

# Append our network stanza to the container config only once.
# The marker line lets us detect prior runs without false positives.
ensure_network_config() {
  local name="$1" ip_cidr="$2"
  local cfg="$LXC_DIR/$name/config"
  local marker="# >>> datadiode: managed network >>>"

  [[ -f "$cfg" ]] || die "expected config not found: $cfg"

  if grep -qF "$marker" "$cfg"; then
    log "$name: network config already managed — skipping"
    return
  fi

  log "$name: writing network config (bridge=$BRIDGE, ip=$ip_cidr)"
  # Strip any pre-existing lxc.net.0.* lines added by the template, to avoid duplicates.
  sed -i '/^lxc\.net\.0\./d' "$cfg"
  cat >>"$cfg" <<EOF
$marker
lxc.net.0.type = veth
lxc.net.0.link = $BRIDGE
lxc.net.0.flags = up
lxc.net.0.ipv4.address = $ip_cidr
lxc.net.0.ipv4.gateway = ${BRIDGE_CIDR%/*}
# <<< datadiode: managed network <<<
EOF
}

start_container() {
  local name="$1"
  if container_running "$name"; then
    log "$name already RUNNING — skipping start"
  else
    log "starting $name"
    lxc-start -n "$name"
    lxc-wait  -n "$name" -s RUNNING --timeout 30
  fi
}

# bring_up_eth0 — explicitly configure eth0 inside the container.
# LXC's lxc.net.0.flags=up brings up the veth from the host POV at
# container creation, but some distro images (rockylinux/9) ship init
# scripts that leave the in-container interface DOWN because they
# don't know about it. We assign the IP and bring it up unconditionally
# here. Safe to re-run.
bring_up_eth0() {
  local name="$1" ip_cidr="$2"
  # ip addr add is idempotent only by "EEXIST" return — ignore that.
  lxc-attach -n "$name" -- ip addr add "$ip_cidr" dev eth0 2>/dev/null || true
  lxc-attach -n "$name" -- ip link set eth0 up
  log "$name: eth0 is up with $ip_cidr"
}

# ---------- main ------------------------------------------------------------

main() {
  require_root
  for c in ip lxc-create lxc-start lxc-info lxc-wait sed grep; do
    require_cmd "$c"
  done

  ensure_bridge
  ensure_container "$LOW_NAME"  "$LOW_IP"
  ensure_container "$HIGH_NAME" "$HIGH_IP"
  start_container  "$LOW_NAME"
  start_container  "$HIGH_NAME"

  bring_up_eth0 "$LOW_NAME"  "$LOW_IP"
  bring_up_eth0 "$HIGH_NAME" "$HIGH_IP"

  log "done."
  log "  bridge : $BRIDGE ($BRIDGE_CIDR)"
  log "  low    : $LOW_NAME  ($LOW_IP)"
  log "  high   : $HIGH_NAME ($HIGH_IP)"
  log "next: push binaries with scripts/lxc-push.sh, then run scripts/demo.sh"
}

main "$@"
