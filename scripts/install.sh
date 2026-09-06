#!/usr/bin/env bash
# Wildstone bootstrap installer (ADR-W014 A2).
#
#   Control plane:
#     sudo ./install.sh --role keystone [--console-dir <dir>] [--catalog-dir <dir>]
#          [--keystone-arg <flag> ...] [--version vX.Y.Z]
#
#   The Console is installed from the release's own console_dist.tar.gz
#   and needs no Node on the host; --console-dir overrides it with a
#   build of your own.
#   Node:
#     sudo ./install.sh --role node --keystone <host:9440> \
#          --join-token <tok> [--ca-fingerprint <hex>] \
#          [--node-id <id>] [--skip-docker] [--version vX.Y.Z]
#   Either role also takes [--artifact-dir <dir>] to install from local
#   release artifacts instead of downloading (private repos, air-gap),
#   and [--tailscale-authkey <key>] [--tailscale-hostname <name>] to
#   join the host to a tailnet.
#
# Tailscale is HOST-LEVEL on purpose (#42). It needs /dev/net/tun and
# NET_ADMIN; the Wardstone op surface carries neither, and that closed
# surface is the product's central security claim — granting a container
# host network control to get a VPN would trade the thing being
# protected for a convenience. So tailscaled is host setup the operator
# owns, exactly like Docker below.
#
# Consequence worth knowing: the tailnet join is NOT Wildstone-managed.
# It is not in the Statefile, not reconciled, and NOT restored by the
# recovery drill. After a total host loss you re-run this step
# (recovery.md says so too).
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
CATALOG_DIR=""
KEYSTONE_ARGS=""
KEYSTONE_ARGS_SET=0
TOKEN="${WILDSTONE_TOKEN:-${GH_TOKEN:-${GITHUB_TOKEN:-}}}"
ROLE=""
KEYSTONE_ADDR=""
JOIN_TOKEN=""
CA_FINGERPRINT=""
NODE_ID="$(hostname -s 2>/dev/null || echo node)"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
TAILSCALE_HOSTNAME=""
SKIP_DOCKER=0
# INTERACTIVE is decided after flag parsing (ADR-W023 A4): a terminal and
# no --role means a person is installing; anything else is automation or
# an upgrade and must behave exactly as it always has.
INTERACTIVE=0
ANSWERS="/var/lib/wildstone/install-answers"
BIN_DIR="/usr/local/bin"
UNIT_DIR="/etc/systemd/system"

log()  { echo "[wildstone-install] $*"; }
fail() { echo "[wildstone-install] ERROR: $*" >&2; exit 1; }


# ---------------------------------------------------------------- answers
#
# Answers persist as they are given so a failed install RESUMES rather
# than restarting (ADR-W023 A5). Two installs failed partway during
# v0.1.9-alpha testing; a wizard that loses ten answers to a failure is
# worse than the script it replaced.
#
# No credential is ever written here. The admin password is used and
# discarded (ADR-W020 A4 keeps credentials out of state), so this file
# holds only choices — which is also why it is safe to show the operator
# on a resume.
answers_load() {
  [ -f "$ANSWERS" ] || return 0
  # shellcheck disable=SC1090
  . "$ANSWERS"
}

answers_save() { # key value
  install -d -m 0700 "$(dirname "$ANSWERS")"
  touch "$ANSWERS"
  grep -v "^$1=" "$ANSWERS" > "$ANSWERS.tmp" 2>/dev/null || true
  echo "$1=$(printf %q "$2")" >> "$ANSWERS.tmp"
  # The mode goes on the file that SURVIVES. Setting it on $ANSWERS
  # before the rename is theatre: mv replaces that inode with the tmp
  # file, which carries the umask's mode, and the result was 0644.
  chmod 0600 "$ANSWERS.tmp"
  mv "$ANSWERS.tmp" "$ANSWERS"
}

# ask_yes_no PROMPT DEFAULT(y|n) -> 0 for yes, 1 for no
ask_yes_no() {
  local prompt="$1" def="$2" reply hint="[y/N]"
  [ "$def" = y ] && hint="[Y/n]"
  printf '\n%s %s ' "$prompt" "$hint" >&2
  read -r reply </dev/tty || reply=""
  [ -z "$reply" ] && reply="$def"
  case "$reply" in [Yy]*) return 0 ;; *) return 1 ;; esac
}

usage() {
  sed -n '2,47p' "$0" | sed 's/^# \{0,1\}//'
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
    --catalog-dir)    CATALOG_DIR="$2"; shift 2 ;;
    --keystone-arg)   KEYSTONE_ARGS="$KEYSTONE_ARGS $2"; KEYSTONE_ARGS_SET=1; shift 2 ;;
    --token)          TOKEN="$2"; shift 2 ;;
    --tailscale-authkey)  TAILSCALE_AUTHKEY="$2"; shift 2 ;;
    --tailscale-hostname) TAILSCALE_HOSTNAME="$2"; shift 2 ;;
    --skip-docker)    SKIP_DOCKER=1; shift ;;
    -h|--help)        usage ;;
    *) fail "unknown flag: $1 (try --help)" ;;
  esac
done

# ---------------------------------------------------------------- mode
# A terminal and no --role means a person is installing (ADR-W023 A4).
# Everything else — CI, an upgrade re-run, a node join — takes the
# flag-driven path unchanged, because both are also this script.
if [ -z "$ROLE" ] && [ -t 0 ]; then
  INTERACTIVE=1
  ROLE=keystone
fi

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


# ---------------------------------------------------------------- steps 1-4
#
# The interactive steps of ADR-W023 A1, in the order the ADR fixes,
# because the order is a constraint: Docker before anything runs,
# Tailscale before an ingress can be told what address to bind.
#
# Every step is idempotent and skippable when already satisfied (A5), so
# a resume asks nothing it has already been told.

# Step 1: the licence. Nothing was ever shown before, which is not a
# defensible default for software that installs a container runtime and
# a VPN client on someone's machine.
step_license() {
  [ "${ACCEPTED_LICENSE:-}" = yes ] && return 0
  local license="$SCRIPT_DIR/LICENSE"
  echo
  echo "Wildstone is free software under the GNU Affero General Public License v3."
  if [ -f "$license" ]; then
    echo "The full text is at $license."
  else
    echo "The full text: https://www.gnu.org/licenses/agpl-3.0.txt"
  fi
  echo "It comes with NO WARRANTY, to the extent permitted by law."
  ask_yes_no "Accept and continue?" n || fail "licence not accepted; nothing was changed"
  answers_save ACCEPTED_LICENSE yes
}

# Step 3: Docker. Today this installs by piping a remote script to a
# shell with no consent and no mention, which is a significant change to
# someone's machine made silently.
step_docker() {
  if command -v docker >/dev/null; then
    log "docker is already installed"
    return 0
  fi
  echo
  echo "Docker is required to run your services and was not found."
  echo "Installing it runs the official script from https://get.docker.com."
  ask_yes_no "Install Docker now?" y || fail "Docker is required; install it yourself and re-run"
  curl -fsSL https://get.docker.com | sh || fail "docker install failed"
  command -v docker >/dev/null || fail "docker still not found after install"
  answers_save DOCKER_OK yes
}

# Step 4: remote access. Reaching the household from outside needs
# Tailscale, and the auth step leaves the terminal for a browser — that
# is a real hop and is designed for rather than discovered (ADR-W023 B3).
step_tailscale() {
  if tailscale status --json 2>/dev/null | grep -q '"BackendState":[[:space:]]*"Running"'; then
    log "tailscale is already joined ($(tailscale ip -4 2>/dev/null | head -1))"
    answers_save WANT_TAILSCALE yes
    expose_tailscale_socket
    return 0
  fi
  if [ "${WANT_TAILSCALE:-}" = no ]; then
    log "skipping tailscale (answered previously)"
    return 0
  fi
  echo
  echo "Tailscale lets you reach your services from anywhere, with a"
  echo "certificate your browser already trusts and nothing to install"
  echo "on your phone or laptop. Without it, services are reachable only"
  echo "from this network."
  if ! ask_yes_no "Set up Tailscale?" y; then
    answers_save WANT_TAILSCALE no
    return 0
  fi
  answers_save WANT_TAILSCALE yes

  if ! command -v tailscale >/dev/null; then
    log "installing tailscale"
    curl -fsSL https://tailscale.com/install.sh | sh || fail "tailscale install failed"
  fi
  systemctl enable --now tailscaled >/dev/null 2>&1 || fail "could not start tailscaled"

  echo
  echo "A sign-in link will be printed below. Open it in any browser —"
  echo "your phone is fine. If you have no Tailscale account yet, the"
  echo "same link will offer to create one (it is free for personal use)."
  echo
  # No --authkey: an interactive operator signs in through the browser,
  # which is also the path someone with no account has to take anyway.
  tailscale up --hostname "${TAILSCALE_HOSTNAME:-$NODE_ID}" || fail "tailscale sign-in did not complete"
  log "tailnet address: $(tailscale ip -4 2>/dev/null | head -1 || echo unknown)"
  expose_tailscale_socket
}

# ---------------------------------------------------------------- tailnet
# Runs for either role and before anything is downloaded: a node that
# reaches its Keystone over the tailnet needs the tailnet up before
# wardstoned starts, and failing here should not leave half an install
# behind.
setup_tailscale() {
  if ! command -v tailscale >/dev/null; then
    log "tailscale not found; installing via tailscale.com/install.sh"
    curl -fsSL https://tailscale.com/install.sh | sh \
      || fail "tailscale install failed"
  fi
  systemctl enable --now tailscaled >/dev/null 2>&1 \
    || fail "could not start tailscaled"

  # Re-running the installer to upgrade must not disturb a working
  # tailnet, and `tailscale up` with an already-spent key would fail.
  if tailscale status --json 2>/dev/null | grep -q '"BackendState":[[:space:]]*"Running"'; then
    log "tailscale is already up; leaving the existing join alone"
    # Still expose the socket: an upgrade of an install that predates
    # this, or any re-run, reaches here and would otherwise never get it.
    expose_tailscale_socket
    return 0
  fi

  log "joining the tailnet as $TAILSCALE_HOSTNAME"
  # The key is passed as an argument and never written to disk here:
  # tailscaled keeps its own state, so nothing needs to persist it, and
  # it is deliberately kept out of every log line below.
  #
  # Tailscale SSH is NOT enabled: that would add a remote-access path
  # the operator did not ask for. Turn it on yourself if you want it.
  tailscale up --authkey "$TAILSCALE_AUTHKEY" --hostname "$TAILSCALE_HOSTNAME" \
    || fail "tailscale up failed (expired, spent, or wrong-tailnet auth key?)"
  log "tailnet address: $(tailscale ip -4 2>/dev/null | head -1 || echo unknown)"
  expose_tailscale_socket
}

# expose_tailscale_socket makes tailscaled's local API reachable from a
# managed container (ADR-W019 A10, F59).
#
# The ingress profile that serves under a tailnet certificate needs it:
# Caddy fetches *.ts.net certificates from tailscaled and needs nothing
# else, no module and no configuration. But Wardstone bind-mounts host
# paths ONLY from its managed prefixes (ADR-W003 A1), and tailscaled puts
# its socket in /run/tailscale, which is not one.
#
# A bind mount rather than tailscaled's own --socket flag: moving the
# socket would satisfy the container and break every `tailscale status`
# the operator types, because the CLI looks in the default place.
#
# A oneshot SERVICE rather than a .mount unit, and this is not a style
# choice (F69). A .mount unit gets systemd's default dependencies, which
# include Before=local-fs.target; ordering it After=tailscaled.service
# then closes a cycle — tailscaled after sysinit.target after
# local-fs.target after the mount after tailscaled — and systemd breaks
# it by DELETING the local-fs.target job. `systemctl enable --now`
# succeeds after boot has finished, so the damage is invisible until the
# next reboot. A service carries no such implicit ordering.
expose_tailscale_socket() {
  install -d -m 0755 /srv/wildstone/tailscale
  # Supersede the .mount unit shipped by v0.1.7/v0.1.8, which introduced
  # the cycle above. Leaving it enabled would keep breaking boot even
  # though nothing uses it any more.
  if [ -f /etc/systemd/system/srv-wildstone-tailscale.mount ]; then
    log "removing the superseded mount unit (F69: it creates a boot ordering cycle)"
    systemctl disable --now srv-wildstone-tailscale.mount >/dev/null 2>&1 || true
    umount /srv/wildstone/tailscale 2>/dev/null || true
    rm -f /etc/systemd/system/srv-wildstone-tailscale.mount
    systemctl daemon-reload
  fi
  if [ -S /srv/wildstone/tailscale/tailscaled.sock ] &&
     systemctl is-active wildstone-tailscale-socket.service >/dev/null 2>&1; then
    log "tailscaled socket already exposed under /srv/wildstone"
    return 0
  fi
  cat > /etc/systemd/system/wildstone-tailscale-socket.service <<'UNIT'
# managed by wildstone-install — do not edit
[Unit]
Description=Expose tailscaled local API inside Wildstone managed prefix
After=tailscaled.service
Requires=tailscaled.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=/bin/mkdir -p /srv/wildstone/tailscale
ExecStart=/bin/mount --bind /run/tailscale /srv/wildstone/tailscale
ExecStop=/bin/umount /srv/wildstone/tailscale

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now wildstone-tailscale-socket.service >/dev/null 2>&1 \
    || fail "could not bind-mount tailscaled's socket into /srv/wildstone"
  [ -S /srv/wildstone/tailscale/tailscaled.sock ] \
    || fail "bind mount reported success but /srv/wildstone/tailscale/tailscaled.sock is not a socket"
  log "tailscaled socket exposed at /srv/wildstone/tailscale/tailscaled.sock"
}


if [ "$INTERACTIVE" = 1 ]; then
  answers_load
  if [ -f "$ANSWERS" ]; then
    log "resuming a previous install; questions already answered are not asked again"
  fi
  step_license
  step_docker
  step_tailscale
elif [ -n "$TAILSCALE_AUTHKEY" ]; then
  [ -n "$TAILSCALE_HOSTNAME" ] || TAILSCALE_HOSTNAME="$NODE_ID"
  setup_tailscale
elif [ -n "$TAILSCALE_HOSTNAME" ]; then
  fail "--tailscale-hostname needs --tailscale-authkey (or TAILSCALE_AUTHKEY)"
elif command -v tailscale >/dev/null 2>&1 &&
     tailscale status --json 2>/dev/null | grep -q '"BackendState":[[:space:]]*"Running"'; then
  # Tailscale is already the operator's, and they passed no key because
  # there is nothing to join. Expose the socket anyway: it is what the
  # tailnet ingress profile needs, and gating it behind an auth key made
  # it unreachable for exactly the hosts most likely to want it — the
  # ones where Tailscale was set up before Wildstone was.
  log "tailscale already joined and no auth key given; exposing its socket only"
  expose_tailscale_socket
fi

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

fetch_checksums() {
  # Obtain checksums.txt into $WORK, once. It is also the release's
  # MANIFEST: an asset that is not listed is not in this release, which
  # is how the Console stays optional for versions built before it was
  # shipped.
  [ -f "$WORK/checksums.txt" ] && return 0
  if [ -n "$ARTIFACT_DIR" ]; then
    [ -f "$ARTIFACT_DIR/checksums.txt" ] || fail "checksums.txt not found in $ARTIFACT_DIR (required to verify local artifacts)"
    cp "$ARTIFACT_DIR/checksums.txt" "$WORK/checksums.txt"
  elif [ -n "$TOKEN" ]; then
    api_fetch checksums.txt "$WORK/checksums.txt"
  else
    curl -fsSL -o "$WORK/checksums.txt" "$BASE/checksums.txt" \
      || fail "could not fetch checksums.txt — if this repo is private, pass --token or set GH_TOKEN (see --help)"
  fi
}

fetch_asset() {
  # fetch_asset <artifact>: obtain one release artifact into $WORK (local
  # ARTIFACT_DIR if set, authenticated API if a token is present, plain
  # download otherwise) and verify it against checksums.txt. Nothing is
  # installed — the caller decides what the bytes are for.
  local artifact="$1"
  fetch_checksums
  if [ -n "$ARTIFACT_DIR" ]; then
    log "using local artifact $ARTIFACT_DIR/$artifact"
    [ -f "$ARTIFACT_DIR/$artifact" ] || fail "local artifact not found: $ARTIFACT_DIR/$artifact"
    cp "$ARTIFACT_DIR/$artifact" "$WORK/$artifact"
  elif [ -n "$TOKEN" ]; then
    log "downloading $artifact ($VERSION, authenticated)"
    api_fetch "$artifact" "$WORK/$artifact"
  else
    log "downloading $artifact ($VERSION)"
    curl -fsSL -o "$WORK/$artifact" "$BASE/$artifact" \
      || fail "download failed for $artifact — if this repo is private, pass --token or set GH_TOKEN (see --help)"
  fi
  ( cd "$WORK" && grep " $artifact\$" checksums.txt | sha256sum -c - >/dev/null ) \
    || fail "checksum verification failed for $artifact"
}

release_has() {
  # release_has <artifact>: is this artifact part of the release?
  fetch_checksums
  grep -q " $1\$" "$WORK/checksums.txt"
}

install_packages() {
  # The packages this control plane can install (ADR-W021 A3). Ships as
  # a release asset for the same reason the Console does: it is data, it
  # is architecture-independent, and a host should not need a checkout
  # or a registry to install what the release shipped with.
  #
  # Skipped, not failed, when the release predates the asset —
  # checksums.txt is the manifest that says which one this is, exactly
  # as it does for the Console.
  local dir=/usr/share/wildstone/packages
  release_has packages.tar.gz || {
    log "this release has no packages.tar.gz; keystone will start with no catalog"
    return 0
  }
  fetch_asset packages.tar.gz
  install -d -m 0755 "$dir"
  find "$dir" -mindepth 1 -delete
  tar --no-same-owner -xzf "$WORK/packages.tar.gz" -C "$dir"
  CATALOG_DIR="$dir"
  log "installed the package catalog to $dir"
}

fetch_binary() {
  # fetch_binary <name>: obtain and verify <name>_linux_<arch>.tar.gz,
  # extract, install to BIN_DIR.
  local name="$1" artifact="$1_linux_${ARCH}.tar.gz"
  fetch_asset "$artifact"
  tar -xzf "$WORK/$artifact" -C "$WORK" "$name"
  install -m 0755 "$WORK/$name" "$BIN_DIR/$name"
  log "installed $BIN_DIR/$name"
}

install_console() {
  # The Console ships as its own release asset so a control-plane host
  # never needs Node (#34): building it pulled 568 apt packages to
  # produce 160 KB of static files, on a machine whose job is running
  # containers.
  #
  # Skipped, not failed, when the release predates the asset — an older
  # --version is a legitimate thing to install, and checksums.txt is the
  # manifest that says which one this is. An explicit --console-dir
  # always wins: someone pointing at their own build means it.
  local dir=/usr/share/wildstone/console
  release_has console_dist.tar.gz || {
    log "this release has no console_dist.tar.gz; keystone will serve the API only"
    return 0
  }
  fetch_asset console_dist.tar.gz
  install -d -m 0755 "$dir"
  # Emptied rather than overwritten: Vite names its bundles by content
  # hash, so extracting over an older Console leaves every previous
  # build's assets behind forever. find -delete on the directory we just
  # created, never rm -rf on a variable.
  find "$dir" -mindepth 1 -delete
  # --no-same-owner: tar restores ownership from the archive when run
  # as root, so without it the Console lands owned by whatever uid
  # built it in CI (1001), giving that uid write access to what the
  # admin plane serves the moment such a user exists (F42).
  tar --no-same-owner -xzf "$WORK/console_dist.tar.gz" -C "$dir"
  CONSOLE_DIR="$dir"
  log "installed the Console to $dir"
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

# ------------------------------------------------- keystone flag merging
# read_keystone_args prints the flags currently in the env file, minus the
# -data-dir the writer always prepends (it is re-added on every write, so
# carrying it through would duplicate it).
read_keystone_args() {
  local line
  line="$(grep -m1 '^KEYSTONE_ARGS=' "$KEYSTONE_ENV" 2>/dev/null || true)"
  line="${line#KEYSTONE_ARGS=}"
  line="${line#-data-dir /var/lib/wildstone/keystone}"
  printf '%s' "$line"
}

# merge_keystone_args prints $1 (existing flags) with every flag named in
# $2 (this run's flags) removed, then $2 appended — so a re-run overrides
# the flags it sets and preserves the ones it does not mention.
#
# A flag's value is the following token ONLY when that token is not itself
# a flag, which keeps a boolean flag from swallowing the next flag's name.
merge_keystone_args() {
  local existing="$1" new="$2" names=" " tok out="" drop=0
  # Unquoted $existing / $new is deliberate — word splitting is the whole
  # point — but pathname expansion rides along with it, and a stored value
  # like "-globtest /srv/*" then became three arguments on the next
  # installer run (F29). `set -f` keeps the splitting and drops the
  # globbing; it is restored before returning, including on the early
  # paths, because the rest of the script expects globbing on.
  set -f
  for tok in $new; do
    case "$tok" in -*) names="$names$tok " ;; esac
  done
  for tok in $existing; do
    case "$tok" in
      -*)
        case "$names" in
          *" $tok "*) drop=1; continue ;;
          *)          drop=0 ;;
        esac
        ;;
      *)
        # An `if`, not `[ ... ] && continue`: under `set -e` that list's
        # failure when drop=0 is the branch's exit status and kills the
        # script. Same trap as F19.
        if [ "$drop" = 1 ]; then continue; fi
        ;;
    esac
    out="$out $tok"
  done
  set +f
  printf '%s%s' "$out" "$new"
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
  # Only when the operator did not name one: an explicit --console-dir is
  # a deliberate choice and this must not overwrite it.
  [ -n "$CONSOLE_DIR" ] || install_console
  [ -n "$CATALOG_DIR" ] || install_packages
  if [ -n "$CONSOLE_DIR" ]; then
    [ -d "$CONSOLE_DIR" ] || fail "--console-dir $CONSOLE_DIR does not exist (build the Console elsewhere and copy dist/ here)"
    KEYSTONE_ARGS="$KEYSTONE_ARGS -console-dir $CONSOLE_DIR"
    KEYSTONE_ARGS_SET=1
  fi
  if [ -n "$CATALOG_DIR" ]; then
    [ -d "$CATALOG_DIR" ] || fail "--catalog-dir $CATALOG_DIR does not exist"
    KEYSTONE_ARGS="$KEYSTONE_ARGS -catalog-dir $CATALOG_DIR"
    KEYSTONE_ARGS_SET=1
  fi
  if [ "$KEYSTONE_ARGS_SET" = 0 ] && [ -f "$KEYSTONE_ENV" ]; then
    log "keeping the keystone flags already in $KEYSTONE_ENV"
  else
    # Merge, do not replace (F22). Writing this file all-or-nothing meant
    # one --keystone-arg silently dropped every flag already set: add an
    # unrelated flag after the documented Console setup and the Console
    # stopped being served, with the service still reporting healthy and
    # the script still printing "re-running this script keeps them".
    if [ -f "$KEYSTONE_ENV" ]; then
      KEYSTONE_ARGS="$(merge_keystone_args "$(read_keystone_args)" "$KEYSTONE_ARGS")"
    fi
    umask 022
    printf 'KEYSTONE_ARGS=-data-dir /var/lib/wildstone/keystone%s\n' "$KEYSTONE_ARGS" > "$KEYSTONE_ENV"
    log "keystone flags: $KEYSTONE_ENV"
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
install -d -m 0700 /var/lib/wildstone/wardstone
install -d -m 0755 /srv/wildstone

# The node identity and secret store used to live at
# /etc/wildstone/wardstone — inside the managed prefix the ingress
# packages bind-mount into Caddy. Move an existing one rather than
# leaving it readable there, and rather than silently re-joining the node
# with a fresh identity.
if [ -d /etc/wildstone/wardstone ] && [ -n "$(ls -A /etc/wildstone/wardstone 2>/dev/null)" ]; then
  if [ -n "$(ls -A /var/lib/wildstone/wardstone 2>/dev/null)" ]; then
    fail "both /etc/wildstone/wardstone and /var/lib/wildstone/wardstone hold node data; move or remove one by hand — refusing to guess which identity is current"
  fi
  log "moving node identity out of the managed prefix: /etc/wildstone/wardstone -> /var/lib/wildstone/wardstone"
  ( cd /etc/wildstone/wardstone && tar cf - . ) | ( cd /var/lib/wildstone/wardstone && tar xf - ) \
    || fail "could not migrate the node data directory"
  rm -rf /etc/wildstone/wardstone
  log "migrated; the old location is removed"
fi

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
