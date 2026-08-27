#!/usr/bin/env bash
# Wildstone bootstrap installer (ADR-W014 A2).
#
#   Control plane:
#     sudo ./install.sh --role keystone [--version vX.Y.Z]
#   Node:
#     sudo ./install.sh --role node --keystone <host:9440> \
#          --join-token <tok> [--ca-fingerprint <hex>] \
#          [--node-id <id>] [--skip-docker] [--version vX.Y.Z]
#
# Binaries come from GitHub releases and are sha256-verified against the
# release's checksums.txt before anything is installed (ADR-W014 A1).
# Re-running upgrades the binaries in place and restarts the unit —
# idempotence is the upgrade path at MVP.
set -euo pipefail

REPO="chickenrat17/wildstone-homelab"
VERSION="${WILDSTONE_VERSION:-latest}"
ROLE=""
KEYSTONE_ADDR=""
JOIN_TOKEN=""
CA_FINGERPRINT=""
NODE_ID="$(hostname -s 2>/dev/null || echo node)"
SKIP_DOCKER=0
BIN_DIR="/usr/local/bin"
UNIT_DIR="/etc/systemd/system"

log()  { echo "[wildstone-install] $*"; }
fail() { echo "[wildstone-install] ERROR: $*" >&2; exit 1; }

usage() {
  sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --role)           ROLE="$2"; shift 2 ;;
    --keystone)       KEYSTONE_ADDR="$2"; shift 2 ;;
    --join-token)     JOIN_TOKEN="$2"; shift 2 ;;
    --ca-fingerprint) CA_FINGERPRINT="$2"; shift 2 ;;
    --node-id)        NODE_ID="$2"; shift 2 ;;
    --version)        VERSION="$2"; shift 2 ;;
    --skip-docker)    SKIP_DOCKER=1; shift ;;
    -h|--help)        usage ;;
    *) fail "unknown flag: $1 (try --help)" ;;
  esac
done

# ---------------------------------------------------------------- preflight
[ "$(id -u)" = 0 ] || fail "must run as root (sudo)"
[ "$(uname -s)" = Linux ] || fail "Linux only"
case "$(uname -m)" in
  x86_64)         ARCH=amd64 ;;
  aarch64|arm64)  ARCH=arm64 ;;
  *) fail "unsupported architecture $(uname -m) (amd64/arm64 only)" ;;
esac
command -v systemctl >/dev/null || fail "systemd is required"
command -v curl >/dev/null      || fail "curl is required"
command -v tar >/dev/null       || fail "tar is required"
command -v sha256sum >/dev/null || fail "sha256sum is required"

case "$ROLE" in
  keystone) ;;
  node)
    [ -n "$KEYSTONE_ADDR" ] || fail "--keystone <host:port> is required for --role node"
    [ -n "$JOIN_TOKEN" ]    || fail "--join-token is required for --role node (mint one: wild node mint-join)"
    [ -n "$CA_FINGERPRINT" ] || log "WARNING: no --ca-fingerprint — the join falls back to trust-on-first-use (ADR-W003 W3 recommends pinning)"
    ;;
  *) fail "--role must be keystone or node" ;;
esac

# ---------------------------------------------------------------- fetch
if [ "$VERSION" = latest ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fetch_binary() {
  # fetch_binary <name>: download <name>_linux_<arch>.tar.gz, verify its
  # checksums.txt entry, extract, install to BIN_DIR.
  local name="$1" artifact="$1_linux_${ARCH}.tar.gz"
  log "downloading $artifact ($VERSION)"
  curl -fsSL -o "$WORK/$artifact" "$BASE/$artifact"
  if [ ! -f "$WORK/checksums.txt" ]; then
    curl -fsSL -o "$WORK/checksums.txt" "$BASE/checksums.txt"
  fi
  ( cd "$WORK" && grep " $artifact\$" checksums.txt | sha256sum -c - >/dev/null ) \
    || fail "checksum verification failed for $artifact"
  tar -xzf "$WORK/$artifact" -C "$WORK" "$name"
  install -m 0755 "$WORK/$name" "$BIN_DIR/$name"
  log "installed $BIN_DIR/$name"
}

install_unit() {
  # install_unit <name>: write the unit from stdin, reload, enable+restart.
  local name="$1"
  cat > "$UNIT_DIR/$name.service"
  systemctl daemon-reload
  systemctl enable "$name" >/dev/null 2>&1 || true
  systemctl restart "$name"
  log "$name.service enabled and (re)started"
}

# ---------------------------------------------------------------- roles
if [ "$ROLE" = keystone ]; then
  fetch_binary keystone
  fetch_binary wild
  install -d -m 0700 /var/lib/wildstone/keystone

  install_unit keystone <<'UNIT'
[Unit]
Description=Wildstone Keystone control plane
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/keystone -data-dir /var/lib/wildstone/keystone
Restart=on-failure
RestartSec=3
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT

  log "Keystone is up. Next steps:"
  log "  1. First start printed the one-time admin API token:"
  log "       journalctl -u keystone | grep -A1 'first start'"
  log "  2. The CA fingerprint (pin node joins with it):"
  log "       journalctl -u keystone | grep 'CA fingerprint'"
  log "  3. Pair a node: WILD_TOKEN=<token> wild node mint-join"
  exit 0
fi

# --role node
if [ "$SKIP_DOCKER" = 0 ] && ! command -v docker >/dev/null; then
  log "docker not found; installing via get.docker.com"
  curl -fsSL https://get.docker.com | sh
  systemctl enable --now docker
fi
command -v docker >/dev/null || fail "docker is required for --role node (or pass --skip-docker to manage it yourself, then re-run)"

fetch_binary wardstoned
install -d -m 0755 /etc/wildstone
install -d -m 0700 /etc/wildstone/wardstone
install -d -m 0755 /srv/wildstone

# The env file carries the single-use join token; 0600 and only
# meaningful until first join completes (the agent persists its identity
# in its data dir afterwards).
ENV_FILE=/etc/wildstone/wardstoned.env
umask 077
cat > "$ENV_FILE" <<EOF
NODE_ID=$NODE_ID
KEYSTONE_ADDR=$KEYSTONE_ADDR
JOIN_TOKEN=$JOIN_TOKEN
CA_FINGERPRINT=$CA_FINGERPRINT
EOF

install_unit wardstoned <<'UNIT'
[Unit]
Description=Wildstone Wardstone node agent
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
EnvironmentFile=/etc/wildstone/wardstoned.env
ExecStart=/usr/local/bin/wardstoned -node-id ${NODE_ID} -keystone ${KEYSTONE_ADDR} -join-token ${JOIN_TOKEN} -ca-fingerprint ${CA_FINGERPRINT}
Restart=on-failure
RestartSec=3
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT

log "Node '$NODE_ID' joined toward $KEYSTONE_ADDR. Verify:"
log "  journalctl -u wardstoned -f"
