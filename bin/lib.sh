# shellcheck shell=bash
# Shared helpers. Sourced by every bin/ script. Compatible with macOS /bin/bash 3.2.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MESH_CIDR="10.213.0.0/16"
# shellcheck disable=SC2034 # used by callers
MESH_PREFIX=16
NEBULA_ETC="${NEBULA_ETC:-/opt/homebrew/etc/nebula}" # Homebrew's nebula service reads this dir

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
info() { printf '==> %s\n' "$*"; }

load_env() {
  if [ -f "$REPO_ROOT/local.env" ]; then
    # shellcheck disable=SC1091
    . "$REPO_ROOT/local.env"
  fi
  LIGHTHOUSE_PORT="${LIGHTHOUSE_PORT:-443}"
  CA_DIR="${CA_DIR:-$HOME/.fleet-ca}"
  SSH_USER="${SSH_USER:-$(id -un)}"
}

require_vars() {
  local v
  for v in "$@"; do
    [ -n "${!v:-}" ] || die "$v is not set (copy local.env.example to local.env)"
  done
}

# host_field NAME COLUMN   columns: 1 name, 2 ip, 3 kind, 4 groups, 5 duration
host_field() {
  awk -v n="$1" -v c="$2" '
    /^[[:space:]]*(#|$)/ { next }
    $1 == n { print $c; found = 1; exit }
    END { exit !found }' "$REPO_ROOT/hosts.conf" || die "host '$1' is not in hosts.conf"
}

lighthouse_ip() {
  awk '/^[[:space:]]*(#|$)/ { next } $3 == "lighthouse" { print $2; exit }' "$REPO_ROOT/hosts.conf"
}

host_names() {
  awk '/^[[:space:]]*(#|$)/ { next } { print $1 }' "$REPO_ROOT/hosts.conf"
}

# YAML for pki.blocklist from blocklist.conf (first column of each non-comment line).
blocklist_yaml() {
  local fps
  fps="$(awk '/^[[:space:]]*(#|$)/ { next } { print $1 }' "${BLOCKLIST_FILE:-$REPO_ROOT/blocklist.conf}")"
  if [ -z "$fps" ]; then
    printf '  blocklist: []\n'
  else
    printf '  blocklist:\n'
    # Quoted: an all-digit fingerprint would otherwise parse as a YAML integer.
    printf '%s\n' "$fps" | sed 's/.*/    - "&"/'
  fi
}

# render TEMPLATE PKI_DIR  -> stdout. PKI_DIR is where ca.crt/host.crt/host.key live.
render() {
  local tmpl="$1" pki_dir="$2" bl
  bl="$(mktemp)"
  blocklist_yaml >"$bl"
  awk -v f="$bl" '/@BLOCKLIST@/ { while ((getline l < f) > 0) print l; next } { print }' "$tmpl" |
    sed -e "s|@NEBULA_ETC@|$pki_dir|g" \
      -e "s|@LIGHTHOUSE_PORT@|$LIGHTHOUSE_PORT|g" \
      -e "s|@LIGHTHOUSE_IP@|$(lighthouse_ip)|g" \
      -e "s|@LIGHTHOUSE_HOST@|${LIGHTHOUSE_HOST:-}|g" \
      -e "s|@LIGHTHOUSE_LAN_IP@|${LIGHTHOUSE_LAN_IP:-}|g"
  rm -f "$bl"
}

template_for_kind() {
  case "$1" in
    lighthouse) printf '%s\n' "$REPO_ROOT/nebula/lighthouse.yml.tmpl" ;;
    mac) printf '%s\n' "$REPO_ROOT/nebula/node.yml.tmpl" ;;
    *) return 1 ;;
  esac
}

# sshd drop-in: key-only, and only for SSH_USER arriving over the mesh.
sshd_dropin() {
  cat <<EOF
# Managed by fleet/bin/ssh-harden. Key-only SSH, reachable only over the Nebula mesh.
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
AllowUsers ${SSH_USER}@${MESH_CIDR}
EOF
}

cert_fingerprint() {
  nebula-cert print -json -path "$1" | tr ',' '\n' | sed -n 's/.*"fingerprint":"\([0-9a-f]*\)".*/\1/p' | head -1
}
