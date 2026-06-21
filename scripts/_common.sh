# Shared bash helpers for DataDiode LXC scripts.
# This file is meant to be sourced, not executed.
# shellcheck shell=bash

# ---- repo paths -----------------------------------------------------------

# Absolute path to the repository root, regardless of cwd. Defined here
# so every script gets the same answer even when symlinked.
DD_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
DD_BIN_DIR="$DD_REPO_ROOT/bin"
DD_BINARY="$DD_BIN_DIR/diode"

# ---- topology defaults (override via env) ---------------------------------

: "${BRIDGE:=diodebr0}"
: "${BRIDGE_CIDR:=10.99.0.1/24}"
: "${LOW_NAME:=diode-low}"
: "${HIGH_NAME:=diode-high}"
: "${LOW_IP_ADDR:=10.99.0.10}"
: "${HIGH_IP_ADDR:=10.99.0.20}"
: "${LOW_IP:=${LOW_IP_ADDR}/24}"
: "${HIGH_IP:=${HIGH_IP_ADDR}/24}"
: "${DIODE_PORT:=9999}"
: "${LXC_DIR:=/var/lib/lxc}"
: "${IN_CONTAINER_BIN:=/usr/local/bin/diode}"

# ---- pretty logging -------------------------------------------------------

if [[ -t 2 ]]; then
  _c_blue='\033[1;34m'; _c_yellow='\033[1;33m'; _c_red='\033[1;31m'
  _c_green='\033[1;32m'; _c_reset='\033[0m'
else
  _c_blue=''; _c_yellow=''; _c_red=''; _c_green=''; _c_reset=''
fi

log()  { printf '%b[%s]%b %s\n' "$_c_blue"   "${DD_TAG:-script}" "$_c_reset" "$*"; }
warn() { printf '%b[%s]%b %s\n' "$_c_yellow" "${DD_TAG:-script}" "$_c_reset" "$*" >&2; }
err()  { printf '%b[%s]%b %s\n' "$_c_red"    "${DD_TAG:-script}" "$_c_reset" "$*" >&2; }
ok()   { printf '%b[%s]%b %s\n' "$_c_green"  "${DD_TAG:-script}" "$_c_reset" "$*"; }

die() { err "$*"; exit 1; }

# ---- preconditions --------------------------------------------------------

require_root() {
  [[ $EUID -eq 0 ]] || die "must run as root (try: sudo $0)"
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1 (install it and retry)"
}

container_exists()  { [[ -d "$LXC_DIR/$1" ]]; }
container_running() { lxc-info -n "$1" -s 2>/dev/null | grep -q 'RUNNING'; }

ensure_container_running() {
  local name="$1"
  container_exists  "$name" || die "container $name does not exist; run scripts/lxc-setup.sh first"
  container_running "$name" || die "container $name exists but is not RUNNING; run scripts/lxc-setup.sh"
}
