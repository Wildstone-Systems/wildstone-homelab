#!/usr/bin/env bash
# Wildstone bootstrap installer (ADR-W014 A2).
#
#   Control plane:
#     sudo ./install.sh --role keystone [--console-dir <dir>]
#          [--keystone-arg <flag> ...] [--version vX.Y.Z]
#   Node:
#     sudo ./install.sh --role node --keystone <host:9440> \
#          --join-token <tok> [--ca-fingerprint <hex>] \
#          [--node-id <id>] [--skip-docker] [--version vX.Y.Z]
#   Either role also takes [--artifact-dir <dir>] to install from local
#   release artifacts instead of downloading (private repos, air-gap).
#
# Keystone flags: --console-dir and --keystone-arg become KEYSTONE_ARGS in
# /etc/wildstone/keystone.env, which the unit's ExecStart expands. The unit
# file is rewritten on every run, but the env file is NOT: re-running the
# installer to upgrade keeps the flags the last run set, so the documented
# Console setup survives an upgrade. Pass --keystone-arg again to change
# them, or edit the env file (no drop-in needed, and nothing to clobber).
#
# Binaries come from GitHub releases and are sha256-verified against the
# release's checksums.txt before anything is installed (ADR-W014 A1).
# Re-running upgrades the binaries in place and restarts the unit —
# idempotence is the upgrade path at MVP.
#
# Private repos: an anonymous download cannot reach a private repo's
# release assets. Two supported paths, both still checksum-verified:
#   - a GitHub token (--token, or GH_TOKEN/GITHUB_TOKEN/WILDSTONE_TOKEN
#     in the environment), which switches the fetch to the authenticated
#     release-asset API; the token needs only contents:read.
#   - local artifacts: place the release files (*.tar.gz +
#     checksums.txt) next to this script, or point --artifact-dir at
#     them, for air-gapped hosts.
set -euo pipefail

REPO="chickenrat17/wildstone-homelab"
VERSION="${WILDSTONE_VERSION:-latest}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ARTIFACT_DIR=""
CONSOLE_DIR=""
KEYSTONE_ARGS=""
KEYSTONE_ARGS_SET=0
TOKEN="${WILDSTONE_TOKEN:-${GH_TOKEN:-${GITHUB_TOKEN:-}}}"
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
  sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//'
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
    --artifact-dir)   ARTIFACT_DIR="$2"; shift 2 ;;
    --console-dir)    CONSOLE_DIR="$2"; shift 2 ;;
    --keystone-arg)   KEYSTONE_ARGS="$KEYSTONE_ARGS $2"; KEYSTONE_ARGS_SET=1; shift 2 ;;
    --token)          TOKEN="$2"; shift 2 ;;
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
# Auto-detect local artifacts next to the script (private-repo path):
# checksums.txt beside install.sh means a release bundle was copied here.
if [ -z "$ARTIFACT_DIR" ] && [ -f "$SCRIPT_DIR/checksums.txt" ]; then
  ARTIFACT_DIR="$SCRIPT_DIR"
fi

command -v systemctl >/dev/null || fail "systemd is required"
[ -n "$ARTIFACT_DIR" ] || command -v curl >/dev/null || fail "curl is required"
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

# Release metadata, fetched once, for the authenticated asset path.
# Private-repo assets are only reachable through the API's per-asset URL
# with an Authorization header — the browser-download URL 404s.
release_json() {
  # Not `[ -f … ] && return 0`: that list's failure is the function's
  # exit status under `set -e`.
  if [ -f "$WORK/release.json" ]; then
    return 0
  fi
  local path="releases/latest"
  [ "$VERSION" = latest ] || path="releases/tags/$VERSION"
  curl -fsSL -H "Authorization: Bearer $TOKEN" \
       -H "Accept: application/vnd.github+json" \
       -o "$WORK/release.json" "https://api.github.com/repos/$REPO/$path" \
    || fail "could not read release $VERSION (token needs contents:read on $REPO)"
}

# api_fetch <asset-name> <dest>: resolve the asset's API URL out of the
# release metadata and download it. Whitespace is stripped and objects
# split on '}' so the name and its own asset URL land on one line — no
# jq dependency (the installer runs before anything is installed).
api_fetch() {
  local want="$1" dest="$2" url
  release_json
  # `|| true` is load-bearing: with `set -euo pipefail`, a grep that
  # matches nothing fails the pipeline, the assignment inherits that
  # status, and set -e kills the script BEFORE the guard below can say
  # anything — an exit 1 with no output at all, which is what the second
  # real VM install hit against a release missing its asset. Let the
  # guard do the reporting, since reporting is the whole reason it is
  # there.
  url="$(tr -d ' \n\t' < "$WORK/release.json" | tr '}' '\n' \
        | grep -F "\"name\":\"$want\"" \
        | grep -o "https://api\.github\.com/repos/[^\"]*/releases/assets/[0-9][0-9]*" \
        | head -1)" || true
  [ -n "$url" ] || fail "release $VERSION has no asset named $want (wrong --version, wrong architecture, or the release is missing assets)"
  curl -fsSL -H "Authorization: Bearer $TOKEN" \
       -H "Accept: application/octet-stream" -o "$dest" "$url" \
    || fail "authenticated download failed for $want"
}

fetch_binary() {
  # fetch_binary <name>: obtain <name>_linux_<arch>.tar.gz (local
  # ARTIFACT_DIR if set, GitHub release download otherwise), verify its
  # checksums.txt entry, extract, install to BIN_DIR.
  local name="$1" artifact="$1_linux_${ARCH}.tar.gz"
  if [ -n "$ARTIFACT_DIR" ]; then
    log "using local artifact $ARTIFACT_DIR/$artifact"
    [ -f "$ARTIFACT_DIR/$artifact" ] || fail "local artifact not found: $ARTIFACT_DIR/$artifact"
    cp "$ARTIFACT_DIR/$artifact" "$WORK/$artifact"
    if [ ! -f "$WORK/checksums.txt" ]; then
      [ -f "$ARTIFACT_DIR/checksums.txt" ] || fail "checksums.txt not found in $ARTIFACT_DIR (required to verify local artifacts)"
      cp "$ARTIFACT_DIR/checksums.txt" "$WORK/checksums.txt"
    fi
  elif [ -n "$TOKEN" ]; then
    log "downloading $artifact ($VERSION, authenticated)"
    api_fetch "$artifact" "$WORK/$artifact"
    if [ ! -f "$WORK/checksums.txt" ]; then
      api_fetch checksums.txt "$WORK/checksums.txt"
    fi
  else
    log "downloading $artifact ($VERSION)"
    curl -fsSL -o "$WORK/$artifact" "$BASE/$artifact" \
      || fail "download failed for $artifact — if this repo is private, pass --token or set GH_TOKEN (see --help)"
    if [ ! -f "$WORK/checksums.txt" ]; then
      curl -fsSL -o "$WORK/checksums.txt" "$BASE/checksums.txt"
    fi
  fi
  ( cd "$WORK" && grep " $artifact\$" checksums.txt | sha256sum -c - >/dev/null ) \
    || fail "checksum verification failed for $artifact"
  tar -xzf "$WORK/$artifact" -C "$WORK" "$name"
  install -m 0755 "$WORK/$name" "$BIN_DIR/$name"
  log "installed $BIN_DIR/$name"
}

install_unit() {
  # install_unit <name>: write the unit from stdin, reload, enable+restart,
  # then gate on the service actually staying up. Restart=on-failure means
  # "systemctl restart succeeded" can still be a crash loop — the first
  # real install printed success over a dead keystone. Watch is-active for
  # a few seconds and fail with the journal tail instead.
  local name="$1" i
  cat > "$UNIT_DIR/$name.service"
  systemctl daemon-reload
  systemctl enable "$name" >/dev/null 2>&1 || true
  systemctl restart "$name"
  for i in 1 2 3 4 5; do
    sleep 1
    if ! systemctl is-active --quiet "$name"; then
      echo "[wildstone-install] ERROR: $name.service is not running (check $i/5) — last 20 journal lines:" >&2
      journalctl -u "$name" -n 20 --no-pager >&2 || true
      exit 1
    fi
  done
  log "$name.service enabled, (re)started, and healthy after 5s"
}

# ---------------------------------------------------------------- roles
if [ "$ROLE" = keystone ]; then
  fetch_binary keystone
  fetch_binary wild
  install -d -m 0700 /var/lib/wildstone/keystone
  install -d -m 0755 /etc/wildstone

  # Keystone's flags live in an env file the unit expands, so the
  # installer can set them and an upgrade does not silently drop them.
  # ExecStart used to be hardcoded, which meant the documented
  # "-console-dir <path>" had to be hand-edited into a unit this script
  # cat > 's over on the next run.
  KEYSTONE_ENV=/etc/wildstone/keystone.env
  if [ -n "$CONSOLE_DIR" ]; then
    [ -d "$CONSOLE_DIR" ] || fail "--console-dir $CONSOLE_DIR does not exist (build the Console elsewhere and copy dist/ here)"
    KEYSTONE_ARGS="$KEYSTONE_ARGS -console-dir $CONSOLE_DIR"
    KEYSTONE_ARGS_SET=1
  fi
  if [ "$KEYSTONE_ARGS_SET" = 0 ] && [ -f "$KEYSTONE_ENV" ]; then
    log "keeping the keystone flags already in $KEYSTONE_ENV"
  else
    umask 022
    printf 'KEYSTONE_ARGS=-data-dir /var/lib/wildstone/keystone%s\n' "$KEYSTONE_ARGS" > "$KEYSTONE_ENV"
  fi

  install_unit keystone <<'UNIT'
[Unit]
Description=Wildstone Keystone control plane
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/wildstone/keystone.env
ExecStart=/usr/local/bin/keystone $KEYSTONE_ARGS
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
  log "  Keystone's flags: $KEYSTONE_ENV (re-running this script keeps them)"
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
