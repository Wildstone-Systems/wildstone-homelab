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
#   --wallpaper-dir overrides the pictures the themes ship with, the
#   same way --console-dir overrides the Console.
#   --agent-runtime <native|legacy> picks the loop Butler conversations
#   run on. A fresh install defaults to native (the OpenAI Agents SDK,
#   ADR-W041, for Butlers on OpenAI-compatible and Ollama models) when
#   the runtime passes its self-check; an upgrade keeps its setting.
#
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
AGENT_RUNTIME=""
# Set to 1 by install_native_agent when the runtime installed AND passed
# its self-check: the condition for making it the default.
NATIVE_AGENT_OK=0
WALLPAPER_DIR=""
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
  # Set it in THIS shell too. Writing only the file meant a later step
  # reading the variable saw nothing — the summary silently omitted the
  # tailnet line because WANT_TAILSCALE had been saved and never set.
  printf -v "$1" '%s' "$2"
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
  sed -n '2,51p' "$0" | sed 's/^# \{0,1\}//'
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
    --wallpaper-dir)  WALLPAPER_DIR="$2"; shift 2 ;;
    --keystone-arg)   KEYSTONE_ARGS="$KEYSTONE_ARGS $2"; KEYSTONE_ARGS_SET=1; shift 2 ;;
    --agent-runtime)  AGENT_RUNTIME="$2"; shift 2 ;;
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


# Step 5: the first Owner's name (design §2.4, ADR-W023 A3).
#
# Captured here rather than on a Console first-run screen, because
# reaching the Console requires an Owner, which is the thing being
# created.
#
# The name and the password both, and both are USED — which is the fix.
#
# This step used to ask for a password and then set it on LLDAP's
# built-in `admin`, never creating the account it named. An operator who
# typed "mika" was told "create your account 'mika'", got no such
# account, and signed in as `admin` with the password they had chosen for
# somebody who did not exist. It only ever looked right because the
# default was `admin`.
#
# Now the name creates a real directory user, the password is set on
# THAT user, and they can sign in to the Console the moment the installer
# finishes (ADR-W033; ADR-W020 A4 said the credential is never
# Wildstone's and is superseded where the two conflict — see the note in
# that ADR).
#
# The password is held in a variable for this run and never written to
# the answers file: it is used to provision, and then forgotten.
ADMIN_USER=""
ADMIN_EMAIL=""
ADMIN_PASS=""
# Whether this install puts the Console behind the identity proxy.
#
# One decision, read in four places: whether to bind the API where a
# container can reach it, whether to install wildstone-console, whether
# the first Owner may be put in console_users, and what to print at the
# end. They have to agree — a route to a listener nobody exposed, or a
# group nothing declared, each fails somewhere far from the cause.
#
# It follows the tailnet, because the package requires ingress:private
# and that is the only ingress this installer offers.
CONSOLE_BEHIND_PROXY=0
# Where the proxy reaches Keystone's API, as address:port. Empty until
# the tailnet address is known.
#
# NOT KEYSTONE_ADDR. That name is --keystone, the gateway a node joins,
# and it is parsed before this line runs — so resetting it here emptied
# it for every `--role node` run, including the one this installer makes
# to pair its own host, and wardstoned started with `-keystone ` and no
# value.
KEYSTONE_API_ADDR=""
step_admin() {
  # A re-run already knows who the first Owner is: the name and address
  # are in the answers file (the password never is). Asking again made
  # every upgrade re-type an account that already existed — and then
  # discard what was typed, because create_first_owner leaves a declared
  # account alone (#362). The password is asked for later, and only if
  # the account still has to be created (ask_owner_password).
  if [ -n "${ADMIN_USER:-}" ] && [ -n "${ADMIN_EMAIL:-}" ]; then
    log "first owner chosen previously: $ADMIN_USER ($ADMIN_EMAIL); not asking again"
    log "  (to change the account, use the Console's Household screen)"
    return 0
  fi
  echo
  echo "Now an account for you. This is what you will log in with, and"
  echo "what you will use to add everyone else later."
  local u
  while :; do
    printf 'Username: ' >&2
    read -r u </dev/tty || u=""
    [ -z "$u" ] && { echo "a username is required" >&2; continue; }
    # Match resource metadata.name before installing anything: this name
    # becomes the LdapUser resource as well as the directory login.
    if [ "${#u}" -gt 63 ] || [[ ! "$u" =~ ^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$ ]]; then
      echo "use 1-63 lowercase letters, digits, dots, dashes or underscores; start and end with a letter or digit (for example: mika)" >&2
      continue
    fi
    # `admin` is LLDAP's own service account — the identity Authelia
    # binds with to read and modify the directory. Taking that name
    # would set the operator's password on the service credential and
    # break the whole identity stack at the next restart. It used to be
    # the DEFAULT here, which is exactly how the old defect hid: the
    # account the installer really operated on and the one it named
    # happened to be the same.
    # Folded before matching. The first version listed admin|Admin|ADMIN,
    # which is 3 of the 32 ways to spell it — `ADmin` walked straight
    # through a guard whose whole purpose is that no spelling of this
    # name gets through. A guard against a name has to be a guard
    # against the name, not against three of its spellings.
    case "$(printf '%s' "$u" | tr '[:upper:]' '[:lower:]')" in
      admin|radicale)
        echo "'$u' is a service account; please choose another name" >&2
        continue ;;
    esac
    break
  done
  # LLDAP cannot create an account without an address, and Keystone
  # refuses a member declared without one — so an installer that never
  # asked stopped at the very step that makes somebody an Owner.
  local e
  while :; do
    printf 'Email: ' >&2
    read -r e </dev/tty || e=""
    case "$e" in
      *[[:space:]\"\\]*|"") echo "an email address is required" >&2; continue ;;
      ?*@?*.?*) break ;;
      *) echo "that does not look like an email address" >&2; continue ;;
    esac
  done
  ADMIN_EMAIL="$e"
  answers_save ADMIN_EMAIL "$e"
  ADMIN_USER="$u"
  answers_save ADMIN_USER "$u"   # the NAME is a choice; the password is not stored
  ask_owner_password
}

# ask_owner_password asks for the first Owner's password. Asked with the
# account on a first install; on a re-run only when the account still has
# to be created (an earlier run saved the name and stopped before
# creating it), because a password is never stored and never reset on an
# account that exists.
ask_owner_password() {
  local p1 p2
  echo "A password for '$ADMIN_USER'."
  while :; do
    printf 'Password: ' >&2; read -rs p1 </dev/tty; echo >&2
    printf 'Again:    ' >&2; read -rs p2 </dev/tty; echo >&2
    if [ "$p1" != "$p2" ]; then echo "they do not match; try again" >&2; continue; fi
    if [ "${#p1}" -lt 8 ]; then echo "at least 8 characters, please" >&2; continue; fi
    break
  done
  ADMIN_PASS="$p1"
}

# Step 5b: where the household lives, in time (ADR-W033; #269).
#
# Asked, not assumed. Servers and VMs run on UTC, and Keystone's default
# is the host's clock — so without this a family in Chicago had its
# agent told it was UTC, and "3pm" put the Fish Fry on the calendar at
# 10am. The host's zone is offered when it is not UTC, since then
# somebody set it on purpose; otherwise the question has no default.
HOUSEHOLD_TZ=""
step_timezone() {
  if [ -n "${HOUSEHOLD_TZ_ANSWERED:-}" ]; then
    HOUSEHOLD_TZ="$HOUSEHOLD_TZ_ANSWERED"
    log "household timezone chosen previously: $HOUSEHOLD_TZ"
    return 0
  fi
  local guess="" tz
  guess="$(timedatectl show -p Timezone --value 2>/dev/null || true)"
  case "$guess" in UTC|Etc/UTC|Etc/Universal|"") guess="" ;; esac
  echo
  echo "Which time zone does your household live in? Your agents read"
  echo "\"3pm\" and \"tomorrow\" in this zone. Examples: America/Chicago,"
  echo "Europe/London, Australia/Sydney."
  while :; do
    if [ -n "$guess" ]; then printf 'Time zone [%s]: ' "$guess" >&2; else printf 'Time zone: ' >&2; fi
    read -r tz </dev/tty || tz=""
    tz="${tz:-$guess}"
    # A zone name is a path under the zoneinfo database, and nothing else:
    # no leading slash, no "..", and a file that exists.
    case "$tz" in ""|/*|*..*|*[!A-Za-z0-9/_+-]*) echo "a zone name like America/Chicago, please" >&2; continue ;; esac
    if [ -f "/usr/share/zoneinfo/$tz" ]; then break; fi
    echo "no time zone called '$tz' on this machine" >&2
  done
  HOUSEHOLD_TZ="$tz"
  answers_save HOUSEHOLD_TZ_ANSWERED "$tz"
}

# Step 6: what to install.
#
# The catalog decides the list, not this script — a package added to the
# catalog should appear here without an installer change. Only packages
# needing no operator secret are offered; ADR-W021 A5's generated form is
# where a secret-carrying package belongs, and that is not built yet.
SERVICES=""
step_services() {
  if [ -n "${SERVICES_ANSWERED:-}" ]; then
    SERVICES="$SERVICES_ANSWERED"
    log "services chosen previously: ${SERVICES:-none}"
    return 0
  fi
  echo
  echo "Which services would you like? You can add more later."
  local chosen=""
  ask_yes_no "  Jellyfin — films and television" y && chosen="$chosen media-jellyfin"
  ask_yes_no "  Photos — your photo library"    y && chosen="$chosen photos-immich"
  SERVICES="${chosen# }"
  answers_save SERVICES_ANSWERED "$SERVICES"
}

# Step 7: the summary, which IS the approval gate (ADR-W023 A2).
#
# Not a formality. This is the human approving a described change while
# present at the machine, which is what ADR-W021 A7's gate requires — so
# the installer may approve the plans it proposes, and the audit trail
# records who approved them.
step_summary() {
  echo
  echo "About to do the following on this machine:"
  echo "  - install the Wildstone control plane and node agent"
  [ "${WANT_TAILSCALE:-no}" = yes ] && echo "  - serve over your tailnet, with a certificate your browser trusts"
  echo "  - create your account '$ADMIN_USER'"
  [ -n "${HOUSEHOLD_TZ:-}" ] && echo "  - keep the household's time in $HOUSEHOLD_TZ"
  # The household homepage needs the proxy, so it is only promised when
  # there will be one.
  local home=""
  [ "${WANT_TAILSCALE:-no}" = yes ] && home=", household homepage (with files, the assistant and calendar)"
  echo "  - install: identity${home}${SERVICES:+, $(echo "$SERVICES" | sed 's/media-jellyfin/jellyfin/g;s/photos-immich/photos/g;s/documents-paperless/documents/g;s/ /, /g')}"
  echo
  echo "Approving here approves the plans this creates; they are recorded"
  echo "in the audit trail against your account."
  ask_yes_no "Proceed?" y || fail "stopped; nothing further was changed"
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
  step_admin
  step_timezone
  step_services
  step_summary
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

install_wallpapers() {
  # The picture each shipped theme arrives with. A release asset for the
  # same reason the catalog and the Console are — data, architecture-
  # independent, and nine megabytes that have no business inside a
  # binary (keystone/api/wallpapers.go records that reasoning, and
  # migration 0061 is the decision it answers).
  #
  # Skipped, not failed, when the release predates the asset: the themes
  # then arrive with palettes only, which is exactly how they shipped
  # before and is a working dashboard, not a fault.
  local dir=/usr/share/wildstone/wallpapers
  release_has wallpapers.tar.gz || {
    log "this release has no wallpapers.tar.gz; the themes will arrive without pictures"
    return 0
  }
  fetch_asset wallpapers.tar.gz
  install -d -m 0755 "$dir"
  find "$dir" -mindepth 1 -delete
  tar --no-same-owner -xzf "$WORK/wallpapers.tar.gz" -C "$dir"
  WALLPAPER_DIR="$dir"
  log "installed the theme wallpapers to $dir"
}

fetch_binary() {
  # fetch_binary <name> [dir]: obtain and verify
  # <name>_linux_<arch>.tar.gz, extract, install to dir (BIN_DIR by
  # default).
  local name="$1" artifact="$1_linux_${ARCH}.tar.gz" dest="${2:-$BIN_DIR}"
  fetch_asset "$artifact"
  tar -xzf "$WORK/$artifact" -C "$WORK" "$name"
  install -d -m 0755 "$dest"
  # A running executable cannot be truncated on Linux (ETXTBSY). Stage
  # the verified replacement beside it and rename it into place, so an
  # upgrade can replace Keystone and Wardstone while their units run.
  install -m 0755 "$WORK/$name" "$dest/.$name.new"
  mv -f "$dest/.$name.new" "$dest/$name"
  log "installed $dest/$name"
}

# install_chat_adapter puts the chat adapter program where the chat
# packages mount it from (ADR-W033 B7.1). Every node gets it, because a
# chat bot runs as a container on whichever node the Owner chose, in a
# public distroless image with this directory bind-mounted read-only —
# so no image of Wildstone's is published anywhere, and a node needs no
# registry credential. /srv/wildstone because Wardstone mounts nothing
# from outside /etc/wildstone and /srv/wildstone. A release from before
# the adapter shipped simply has no chat.
install_uninstall() {
  release_has uninstall.sh || return 0
  fetch_asset uninstall.sh
  install -m 0755 "$WORK/uninstall.sh" "$BIN_DIR/wildstone-uninstall"
  log "installed $BIN_DIR/wildstone-uninstall"
}

install_chat_adapter() {
  release_has "chat-adapter_linux_${ARCH}.tar.gz" || {
    log "this release has no chat-adapter; chat connections will not run on this node"
    return 0
  }
  fetch_binary chat-adapter /srv/wildstone/bin
}

# install_agent_bridge puts the agent runtime bridge (ADR-W039 Phase
# 6/H3, #349) where the agent-hermes package mounts it from, the same
# way install_chat_adapter does: every node gets it, because a packaged
# runtime can run on whichever node the Owner chose, in a public
# distroless image with this directory bind-mounted read-only. A release
# from before the bridge shipped simply has no packaged runtime.
install_agent_bridge() {
  release_has "agent-bridge_linux_${ARCH}.tar.gz" || {
    log "this release has no agent-bridge; packaged agent runtimes will not run on this node"
    return 0
  }
  fetch_binary agent-bridge /srv/wildstone/bin
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

install_native_agent() {
  # The native agent runtime (ADR-W041): the OpenAI Agents SDK sidecar,
  # bundled into one file, beside the Node runtime it runs on. Shipped
  # whole, per architecture, so a control-plane host still never needs
  # Node, npm or a node_modules tree of its own (#34).
  #
  # Skipped, not failed, when the release predates the asset: the
  # Butler's own loop does not need it.
  local dir=/usr/lib/wildstone/native-agent
  local stage=/usr/lib/wildstone/.native-agent.new
  local old=/usr/lib/wildstone/.native-agent.old
  local artifact="native-agent_linux_${ARCH}.tar.gz"
  release_has "$artifact" || {
    log "this release has no $artifact; the native agent runtime is not installed"
    return 0
  }
  fetch_asset "$artifact"
  install -d -m 0755 /usr/lib/wildstone
  # Fixed paths, never a variable that could be empty, for the one rm -rf.
  rm -rf /usr/lib/wildstone/.native-agent.new /usr/lib/wildstone/.native-agent.old
  install -d -m 0755 "$stage"
  # --no-same-owner for the reason install_console gives (F42).
  tar --no-same-owner -xzf "$WORK/$artifact" -C "$stage"
  if [ ! -x "$stage/bin/node" ] || [ ! -f "$stage/main.mjs" ]; then
    fail "$artifact is missing bin/node or main.mjs"
  fi
  # Staged and swapped by rename, like fetch_binary: a sidecar that is
  # mid-job keeps running from the old files it already has open.
  if [ -d "$dir" ]; then
    mv "$dir" "$old"
  fi
  mv "$stage" "$dir"
  rm -rf /usr/lib/wildstone/.native-agent.old
  # A self-check that loads the SDK and calls no model. A host whose C
  # library is too old for this Node fails here, and it is reported
  # rather than fatal: nothing depends on the runtime yet.
  if "$dir/bin/node" "$dir/main.mjs" --health 2>/dev/null | grep -q '"type":"ready"'; then
    NATIVE_AGENT_OK=1
    log "installed the native agent runtime to $dir ($(tr '\n' ' ' < "$dir/VERSION"))"
  else
    log "WARNING: installed the native agent runtime to $dir, but its self-check failed: run '$dir/bin/node $dir/main.mjs --health' to see why"
  fi
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
# keystone_arg_named reports whether a flag is already set for this run,
# either on this invocation's --keystone-arg or in the env file a
# previous run wrote. Both matter: the operator may have named it just
# now, or named it once and expect it to stick.
keystone_arg_named() { # flag
  case " $KEYSTONE_ARGS " in *" $1 "*) return 0 ;; esac
  case " $(read_keystone_args) " in *" $1 "*) return 0 ;; esac
  return 1
}

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

# ---------------------------------------------------------------- step 8
#
# Everything that was done by hand on the alpha VM: pair the node, wait
# for the agent, install the ingress, identity, dashboard and the chosen
# services, and approve each plan (ADR-W023 A2 — the operator approved
# them at step 7, while present at the machine).
#
# Ordering is load-bearing, not aesthetic (B4): identity before any
# service that needs an OIDC client, ingress before Routes mean anything,
# and the agent CONNECTED before any capability is proposed. A capability
# proposed before the agent connects fails, and the failed step stays in
# desired state failing every subsequent tick (F60).

API="http://127.0.0.1:9441"
WILD_TOKEN=""

api() { # method path [json]
  if [ -n "${3:-}" ]; then
    curl -sS -X "$1" -H "Authorization: Bearer $WILD_TOKEN" -H 'Content-Type: application/json' -d "$3" "$API$2"
  else
    curl -sS -X "$1" -H "Authorization: Bearer $WILD_TOKEN" "$API$2"
  fi
}

admin_token() {
  # The offline mint, not the journal: journalctl outlives a reinstall and
  # returns a token per install the host has ever had (F66).
  systemctl stop keystone
  # Two hours, not forever: this token drives the install and nothing
  # else, and a bearer token needs no fresh sign-in to use the Console's
  # most consequential operations (ADR-W033 A4.1). If the install dies
  # before forget_admin_token runs, what it leaves behind expires.
  "$BIN_DIR/keystone" claim-code --data-dir /var/lib/wildstone/keystone --ttl 2h >/dev/null \
    || fail "could not mint an admin token"
  systemctl start keystone
  WILD_TOKEN="$(tr -d '[:space:]' < /var/lib/wildstone/keystone/claim-code)"
  local i
  for i in $(seq 1 30); do
    [ "$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $WILD_TOKEN" "$API/api/v1/principals" 2>/dev/null)" = 200 ] && return 0
    sleep 2
  done
  fail "keystone did not accept the minted token"
}

# forget_admin_token deletes the installer's claim code once the install
# is done with it. Nothing after this needs it: the Owner signs in with
# the account they just created, and recovery is `keystone owner` or a
# new `keystone claim-code` from this host (design §2.6).
forget_admin_token() {
  rm -f /var/lib/wildstone/keystone/claim-code
  WILD_TOKEN=""
}

wait_agent() {
  local i
  for i in $(seq 1 60); do
    # Present is not enough: the node must be CONNECTED. A capability
    # proposed against a known-but-disconnected node fails and leaves a
    # step in desired state that fails every subsequent tick (F60).
    api GET /api/v1/nodes 2>/dev/null | grep -q "\"id\":\"$NODE_ID\",\"connected\":true" && return 0
    sleep 2
  done
  fail "the node agent never connected; see: journalctl -u wardstoned"
}

enable_capability() { # package params-json
  wait_agent
  local r id st i
  r="$(api POST /api/v1/capabilities "{\"package\":\"$1\",\"params\":$2}")"
  # A resumed install proposes what an earlier run already put in
  # desired state. Keystone says so rather than making an empty plan,
  # and that is this step already done, not a failure.
  case "$r" in
    *"no-op against current desired state"*) log "  $1: already installed"; return 0 ;;
  esac
  id="$(echo "$r" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
  [ -n "$id" ] || fail "$1: $(echo "$r" | head -c 200)"
  api POST "/api/v1/plans/$id/approve" >/dev/null
  for i in $(seq 1 90); do
    sleep 3
    st="$(api GET "/api/v1/plans/$id" | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')"
    [ "$st" = applied ] && { log "  $1: installed"; return 0; }
    [ "$st" = failed ] && fail "$1: plan $id failed; see journalctl -u keystone"
  done
  fail "$1: plan $id did not apply in time"
}

# LLDAP's built-in `admin` is a SERVICE account, and its password is
# generated here rather than chosen.
#
# It used to be the operator's own password, which conflated two
# different things: the credential Authelia binds with to read and
# modify the directory, and the credential a person signs in with. They
# have different lifetimes, different blast radius, and only one of them
# belongs to a human. Nobody signs in as `admin` now — the first Owner
# is a real directory user (see create_first_owner) — so this is a
# machine credential and is treated like one: generated, stored as a
# Secret, never printed.
#
# LLDAP applies the config-provided password at startup ("Successfully
# (re)set password for admin" in its own log), so setting the secret and
# restarting is the whole mechanism.
LLDAP_ADMIN_PASS=""
set_admin_password() {
  # 32 hex characters from the kernel. No operator ever types this, and
  # it is kept in a variable only for the rest of this run: creating the
  # first Owner's password needs an admin token, and Secrets are
  # write-only by design, so there is no reading it back afterwards.
  LLDAP_ADMIN_PASS="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  [ "${#LLDAP_ADMIN_PASS}" -eq 32 ] || fail "could not generate the directory's service password"
  api POST "/api/v1/nodes/$NODE_ID/secrets/lldap-admin-password" "{\"value\":\"$LLDAP_ADMIN_PASS\"}" >/dev/null \
    || fail "could not set the directory's service password"
  docker restart lldap >/dev/null 2>&1 || true
  local i
  for i in $(seq 1 30); do
    sleep 2
    if docker exec lldap /bin/sh -c 'exit 0' 2>/dev/null; then break; fi
  done
  log "directory ready"
}

# ALPINE is the same pinned image the identity package's provisioning job
# uses, for the same reason: an empty ENTRYPOINT and a busybox wget built
# with long options. Kept in step with packages/identity/package.yaml.
ALPINE_IMAGE="alpine@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d"

# set_owner_password gives the first Owner the password they chose.
#
# LLDAP stores passwords through an OPAQUE registration, so there is no
# GraphQL mutation for it and no way to do it from SQL or a config file:
# its schema has createUser and updateUser and nothing that takes a
# password. The only thing that can perform the exchange is
# `lldap_set_password`, which ships in the LLDAP image at /app.
#
# Two containers, because neither can do it alone. The token comes from
# a throwaway alpine sharing LLDAP's network namespace — the LLDAP image
# has no HTTP client — and the exchange runs inside LLDAP, where the
# binary is. `--network container:lldap` rather than a network name so
# this does not have to know what Wildstone called the network.
#
# VERIFIED rather than assumed, the way set_admin_password's bind was:
# an install that reports success and leaves the operator unable to log
# in is the worst outcome available, and it is the exact outcome this
# whole step exists to stop.
# json_escape makes a shell value safe to interpolate into a JSON string.
#
# Only the two characters JSON itself reserves. The password is checked
# for length and nothing else, so a backslash or a quote in it would
# otherwise produce a malformed body — and the visible symptom would be
# "cannot sign in with the password just set" on a password that was in
# fact set correctly, because the env var path has no such problem.
json_escape() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# lldap_login returns an admin-or-user token, or empty.
#
# Separated so the caller decides what a failure means, and assigned
# with `|| true` at the call site: under `set -e` a failing command
# substitution aborts the script before the `|| fail` beside it can say
# anything, so the operator sees the install die mid-step with no
# message. That is the worst possible shape for a step whose whole job
# is to be verifiable.
lldap_login() { # username password
  docker run --rm --network "container:lldap" "$ALPINE_IMAGE" \
    wget -qO- --header 'Content-Type: application/json' \
      --post-data "{\"username\":\"$(json_escape "$1")\",\"password\":\"$(json_escape "$2")\"}" \
      http://127.0.0.1:17170/auth/simple/login 2>/dev/null \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p'
}

set_owner_password() {
  [ -n "$LLDAP_ADMIN_PASS" ] || set_admin_password
  local token
  # Retried, because the service password was set moments ago and the
  # directory adopts it when the reconciler re-creates the container
  # with the new Secret rendered in — not the instant the Secret is
  # written. A single attempt here would race that, and would fail on
  # timing rather than on anything being wrong.
  local i
  for i in $(seq 1 30); do
    token="$(lldap_login admin "$LLDAP_ADMIN_PASS" || true)"
    [ -n "$token" ] && break
    sleep 2
  done
  [ -n "$token" ] || fail "could not log in to the directory to set '$ADMIN_USER' a password"

  docker exec -e LLDAP_USER_PASSWORD="$ADMIN_PASS" lldap \
    /app/lldap_set_password --base-url http://127.0.0.1:17170 \
    --token "$token" --username "$ADMIN_USER" >/dev/null 2>&1 \
    || fail "could not set the password for '$ADMIN_USER'"

  # Prove it: log in as them. Without this the install could report
  # success on a password that silently did not take.
  local proof
  proof="$(lldap_login "$ADMIN_USER" "$ADMIN_PASS" || true)"
  [ -n "$proof" ] || fail "'$ADMIN_USER' cannot sign in with the password just set"
  log "password set for '$ADMIN_USER'"
}

# Calendar's LDAP reader is a machine account. The directory creates
# people without passwords, so the installer gives this account a random
# one, stores the same value in the Calendar Secret and Keystone's private
# environment, and proves that the account can log in. Resumed installs
# preserve a declared account with an existing credential file.
CALENDAR_PASS=""
CALENDAR_PROVISIONED=0
CALENDAR_USER="radicale"
CALENDAR_COLLECTION="GROUPS/Y2FsZW5kYXJfdXNlcnM="

lldap_graphql() { # admin-token json-body
  docker run --rm --network "container:lldap" \
    -e "LLDAP_AUTH_TOKEN=$1" -e "LLDAP_BODY=$2" "$ALPINE_IMAGE" \
    /bin/sh -c 'wget -qO- --header "Authorization: Bearer $LLDAP_AUTH_TOKEN" \
      --header "Content-Type: application/json" --post-data "$LLDAP_BODY" \
      http://127.0.0.1:17170/api/graphql' 2>/dev/null
}

seed_calendar_account() {
  local members account_row existing_groups r id st i token groups membership gid response
  members="$(api GET /api/v1/household/members)"
  account_row="$(printf '%s' "$members" | tr '{' '\n' |
    grep "\"username\":\"$CALENDAR_USER\"" | head -1 || true)"
  if [ -n "$account_row" ] && [ "$CALENDAR_PROVISIONED" = 1 ]; then
    log "Calendar service account already provisioned; preserving its credential"
    return 0
  fi
  [ -n "$LLDAP_ADMIN_PASS" ] || set_admin_password
  wait_agent
  if [ -z "$account_row" ]; then
    r="$(api POST /api/v1/household/members \
      "{\"username\":\"$CALENDAR_USER\",\"email\":\"$CALENDAR_USER@wildstone.internal\",\"node\":\"$NODE_ID\",\"groups\":[\"calendar_users\"]}")"
    id="$(echo "$r" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
    [ -n "$id" ] || fail "could not propose the Calendar service account: $(echo "$r" | head -c 200)"
    api POST "/api/v1/plans/$id/approve" >/dev/null || fail "could not approve the Calendar service account"
    for i in $(seq 1 60); do
      sleep 2
      st="$(api GET "/api/v1/plans/$id" | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')"
      [ "$st" = applied ] && break
      [ "$st" = failed ] && fail "the Calendar service account plan failed"
    done
    [ "$st" = applied ] || fail "the Calendar service account did not apply in time"
  elif [[ "$account_row" != *'"calendar_users"'* ]]; then
    # Preserve any existing memberships on an earlier manual account.
    # The member API refuses removals, and it is right to do so.
    existing_groups="$(printf '%s' "$account_row" |
      sed -n 's/.*"groups":\(\[[^]]*\]\).*/\1/p')"
    [ -n "$existing_groups" ] || fail "could not read Calendar account groups"
    if [ "$existing_groups" = '[]' ]; then
      existing_groups='["calendar_users"]'
    else
      existing_groups="${existing_groups%]},\"calendar_users\"]"
    fi
    r="$(api PUT "/api/v1/household/members/$CALENDAR_USER" \
      "{\"groups\":$existing_groups}" || true)"
    case "$r" in
      *"no-op against current desired state"*) ;;
      *)
        id="$(echo "$r" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
        [ -n "$id" ] || fail "could not grant calendar_users to the Calendar service account"
        api POST "/api/v1/plans/$id/approve" >/dev/null || fail "could not approve the Calendar group plan"
        for i in $(seq 1 60); do
          sleep 2
          st="$(api GET "/api/v1/plans/$id" | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')"
          [ "$st" = applied ] && break
          [ "$st" = failed ] && fail "the Calendar group plan failed"
        done
        [ "$st" = applied ] || fail "the Calendar group plan did not apply in time" ;;
    esac
  fi

  token=""
  for i in $(seq 1 30); do
    token="$(lldap_login admin "$LLDAP_ADMIN_PASS" || true)"
    [ -n "$token" ] && break
    sleep 2
  done
  [ -n "$token" ] || fail "could not log in to the directory for Calendar setup"

  # A LdapUser plan declares the account first; the identity provisioning
  # job creates it and grants its app group on a later reconcile tick.
  for i in $(seq 1 60); do
    membership="$(lldap_graphql "$token" \
      '{"query":"{user(userId: \"radicale\") {groups {displayName}}}"}' || true)"
    [[ "$membership" == *'"displayName":"calendar_users"'* ]] && break
    sleep 2
  done
  [[ "$membership" == *'"displayName":"calendar_users"'* ]] \
    || fail "the Calendar account was not provisioned in calendar_users"

  # LLDAP already owns this built-in read-only group. Give it only to the
  # service account; ordinary family accounts need calendar_users alone.
  groups="$(lldap_graphql "$token" '{"query":"{groups {id displayName}}"}' || true)"
  gid="$(printf '%s' "$groups" | tr '{' '\n' |
    grep '"displayName":"lldap_strict_readonly"' |
    sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
  [ -n "$gid" ] || fail "LLDAP has no lldap_strict_readonly group"
  if [[ "$membership" != *'"displayName":"lldap_strict_readonly"'* ]]; then
    response="$(lldap_graphql "$token" \
      "{\"query\":\"mutation {addUserToGroup(userId: \\\"radicale\\\", groupId: $gid) {ok}}\"}" || true)"
    [[ "$response" == *'"ok":true'* ]] || fail "could not grant the Calendar account read-only directory access"
  fi

  docker exec -e "LLDAP_USER_PASSWORD=$CALENDAR_PASS" lldap \
    /app/lldap_set_password --base-url http://127.0.0.1:17170 \
    --token "$token" --username "$CALENDAR_USER" >/dev/null 2>&1 \
    || fail "could not set the Calendar service account password"
  [ -n "$(lldap_login "$CALENDAR_USER" "$CALENDAR_PASS" || true)" ] \
    || fail "the Calendar service account cannot sign in with its new password"
  log "Calendar service account ready"
}

# The first Owner (design §2.4). Installation does not complete without
# one, and that is a failure rather than a warning.
#
# Three things, in an order that matters:
#
#   1. The directory user, so there is an account to sign in with. It
#      goes through the ordinary proposal path — the same plan a Console
#      holder would approve — because a first Owner created by a private
#      back door is one the audit trail cannot explain.
#   2. The Person, recorded and admitted as an Owner. This is what
#      Wildstone is authoritative for (decision 1 of #201); the directory
#      holds the credential and nothing else.
#   3. The check. An install that reported success and left a household
#      with no Owner would hand the operator a Console that admits
#      whoever the directory says — which is the state slice 3 exists to
#      end.
#
# The first Owner carries no permanent privilege afterwards. They are an
# ordinary Owner, and the Last Owner invariant — not a flag — is what
# keeps them irreplaceable until a second one exists (§2.4, §2.5).
# The groups the first Owner is given behind the proxy: the Console's,
# and the family homepage's.
OWNER_GROUPS='"console_users","homepage_users","filedrop_users","assistant_users","calendar_users","lists_users"'

# grant_owner_groups puts an ALREADY-declared first Owner in those
# groups. A resumed install finds the account an earlier run created —
# possibly before homepage-wildstone was installed, so without the group
# that opens the page this install ends on. Not fatal: the account is
# fine, and the Household screen can add the group; saying so beats
# stopping an install that is otherwise done.
grant_owner_groups() {
  wait_agent
  local r id st i members account_row groups group
  members="$(api GET /api/v1/household/members)"
  account_row="$(printf '%s' "$members" | tr '{' '\n' |
    grep "\"username\":\"$ADMIN_USER\"" | head -1 || true)"
  groups="$(printf '%s' "$account_row" |
    sed -n 's/.*"groups":\(\[[^]]*\]\).*/\1/p')"
  [ -n "$groups" ] || fail "could not read groups for '$ADMIN_USER'"
  for group in console_users homepage_users filedrop_users assistant_users calendar_users lists_users; do
    if [[ "$groups" != *"\"$group\""* ]]; then
      if [ "$groups" = '[]' ]; then
        groups="[\"$group\"]"
      else
        groups="${groups%]},\"$group\"]"
      fi
    fi
  done
  if [ "$groups" = "$(printf '%s' "$account_row" |
    sed -n 's/.*"groups":\(\[[^]]*\]\).*/\1/p')" ]; then
    log "'$ADMIN_USER' already has the homepage groups"
    return 0
  fi
  r="$(api PUT "/api/v1/household/members/$ADMIN_USER" "{\"groups\":$groups}" || true)"
  case "$r" in
    *"no-op against current desired state"*) return 0 ;;
  esac
  id="$(echo "$r" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
  if [ -z "$id" ]; then
    log "WARNING: could not add '$ADMIN_USER' to $OWNER_GROUPS: $(echo "$r" | head -c 200)"
    log "  add them on the Console's Household screen"
    return 0
  fi
  api POST "/api/v1/plans/$id/approve" >/dev/null || fail "could not approve the group plan"
  for i in $(seq 1 60); do
    sleep 2
    st="$(api GET "/api/v1/plans/$id" | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')"
    [ "$st" = applied ] && { log "'$ADMIN_USER' added to $OWNER_GROUPS"; return 0; }
    [ "$st" = failed ] && fail "the group plan for '$ADMIN_USER' failed; see journalctl -u keystone"
  done
  fail "the group plan for '$ADMIN_USER' did not apply in time"
}

create_first_owner() {
  log "creating the first owner '$ADMIN_USER'"

  # Re-runs are a documented path — the installer resumes from an
  # answers file — and re-declaring an existing member is refused with a
  # 409, because Keystone reads it as a rename rather than an add. So
  # the account step is skipped when they are already declared, and the
  # rest still runs: the Person half below is idempotent, and a run that
  # got half way through is exactly when it needs to.
  local declared created_account=0
  declared="$(api GET /api/v1/household/members || true)"
  case "$declared" in
    *"\"username\":\"$ADMIN_USER\""*)
      log "'$ADMIN_USER' is already declared; leaving the account as it is"
      [ "$CONSOLE_BEHIND_PROXY" = 1 ] && grant_owner_groups ;;
    *)
      # The group is requested only when the package that DECLARES it is
      # installed, and never otherwise.
      #
      # `console_users` comes from wildstone-console, which needs an
      # ingress and so exists only on a tailnet install. Asking for an
      # undeclared group is refused outright (addHouseholdMember checks
      # declaredGroups), and that refusal once aborted every interactive
      # install — so the condition here is load-bearing, not tidiness.
      # Without the group, the first Owner reaches the Console on this
      # host and nowhere else; with it, they sign in through the proxy
      # as themselves.
      #
      # `homepage_users` is the same case: homepage-wildstone declares it,
      # it too needs the proxy, and without it the first Owner lands on
      # the family homepage the install just sent them to and is refused.
      local r id st i groups=""
      if [ "$CONSOLE_BEHIND_PROXY" = 1 ]; then
        groups=",\"groups\":[$OWNER_GROUPS]"
      fi
      # A re-run that saved the name but never created the account
      # has no password yet: ask for it now, the one time it is needed.
      [ -n "$ADMIN_PASS" ] || ask_owner_password
      [ -n "$LLDAP_ADMIN_PASS" ] || set_admin_password
      wait_agent
      r="$(api POST /api/v1/household/members \
        "{\"username\":\"$ADMIN_USER\",\"email\":\"$ADMIN_EMAIL\",\"node\":\"$NODE_ID\"$groups}" || true)"
      id="$(echo "$r" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
      [ -n "$id" ] || fail "could not propose the account for '$ADMIN_USER': $(echo "$r" | head -c 200)"
      api POST "/api/v1/plans/$id/approve" >/dev/null || fail "could not approve the account plan"
      st=""
      for i in $(seq 1 60); do
        sleep 2
        st="$(api GET "/api/v1/plans/$id" | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')"
        [ "$st" = applied ] && break
        [ "$st" = failed ] && fail "the account plan failed; see journalctl -u keystone"
      done
      [ "$st" = applied ] || fail "the account for '$ADMIN_USER' did not apply in time"
      created_account=1 ;;
  esac

  # Existing accounts may have changed their password since setup.
  # Resuming must never replace it with an old installer answer.
  if [ "$created_account" = 1 ]; then
    set_owner_password
  fi

  # The Person, as an Owner, admitted. Role is stated rather than left to
  # default: recordButlerMember defaults to member on purpose, so an
  # omission here would stage the first Owner as a Member and leave the
  # household with none.
  #
  # The responses are READ rather than trusted to a `|| fail`. api()
  # runs curl without -f, so an HTTP 4xx still exits 0 and every
  # `|| fail` beside one of these calls was dead — which meant a failed
  # admit produced a successful install and a recorded-but-unadmitted
  # Owner: precisely the lockout `keystone owner create` exists to
  # repair, created by the step meant to prevent it.
  local rec adm
  rec="$(api POST /api/v1/butler/members "{\"member\":\"$ADMIN_USER\",\"role\":\"owner\"}" || true)"
  case "$rec" in
    *'"error"'*) fail "could not record '$ADMIN_USER' as an owner: $(echo "$rec" | head -c 200)" ;;
  esac
  adm="$(api POST "/api/v1/butler/members/$ADMIN_USER/admit" || true)"
  case "$adm" in
    *'"error"'*) fail "could not admit '$ADMIN_USER': $(echo "$adm" | head -c 200)" ;;
  esac

  # And the check that makes this a requirement rather than an intention.
  # ADMITTED as well as owner: a recorded Owner who was never let in is
  # not an active Owner, and checking only the role would call that
  # household done.
  local row
  row="$(api GET /api/v1/butler/members | tr '{' '\n' | grep "\"member\":\"$ADMIN_USER\"" || true)"
  case "$row" in
    *'"role":"owner"'*) ;;
    *) fail "'$ADMIN_USER' is not an owner after setup; refusing to report success (design §2.4)" ;;
  esac
  case "$row" in
    *'"admitted":true'*) ;;
    *) fail "'$ADMIN_USER' is an owner but was never admitted; refusing to report success (design §2.4)" ;;
  esac
  log "first owner '$ADMIN_USER' recorded and admitted"
}

# The tailnet ingress publishes one fixed TCP port block. Catch a host
# process such as rpc.statd taking one of those ports before Caddy is
# started; Docker's later bind failure otherwise looks like a generic plan
# failure. Existing live Caddy mappings are our own and may be reused.
check_tailnet_ingress_ports() {
  local bind_ip="$1" owned line local_addr process port
  command -v ss >/dev/null || fail "ss (iproute2) is required to check tailnet ingress ports"
  owned="$(docker port caddy-tailnet-ts 2>/dev/null || true)"
  while IFS= read -r line; do
    read -r _ _ _ local_addr _ process <<< "$line"
    port="${local_addr##*:}"
    [[ "$port" =~ ^[0-9]+$ ]] || continue
    if [ "$port" -ne 443 ]; then
      if [ "$port" -lt 7443 ] || [ "$port" -gt 53443 ]; then
        continue
      fi
      [ $(((port - 7443) % 1000)) -eq 0 ] || continue
    fi
    case "$local_addr" in
      "$bind_ip:$port"|"0.0.0.0:$port"|"*:$port"|"[::]:$port") ;;
      *) continue ;;
    esac
    if printf '%s\n' "$owned" | grep -Fxq "$port/tcp -> $bind_ip:$port"; then
      continue
    fi
    [ -n "${process:-}" ] || process="unknown process"
    fail "tailnet ingress needs $bind_ip:$port, but $process is listening there (inspect: ss -ltnp 'sport = :$port')"
  done < <(ss -H -ltnp)
}

post_install() {
  log "pairing this host as its own node"
  local mint join fp
  mint="$(WILD_TOKEN="$WILD_TOKEN" "$BIN_DIR/wild" node mint-join 2>/dev/null)" || fail "could not mint a join token"
  join="$(echo "$mint" | grep -oE 'join-token [a-f0-9]{64}' | head -1 | awk '{print $2}')"
  fp="$(echo "$mint" | grep -oE 'ca-fingerprint [a-f0-9]{64}' | head -1 | awk '{print $2}')"
  [ -n "$join" ] || fail "could not read the join token from mint-join output"

  # Re-exec for the node role: the same script, the same code path a
  # separate machine takes, so a single host is not a special case.
  #
  # Built as an array rather than ${var:+...} inside the command line.
  # Those expansions are unquoted by construction, so every optional
  # argument is a word-splitting hazard for the sake of brevity.
  local -a args=(--role node --keystone "127.0.0.1:9440" --join-token "$join"
                 --node-id "$NODE_ID" --version "$VERSION")
  [ -n "$fp" ] && args+=(--ca-fingerprint "$fp")
  [ -n "$ARTIFACT_DIR" ] && args+=(--artifact-dir "$ARTIFACT_DIR")
  [ -n "$TOKEN" ] && args+=(--token "$TOKEN")
  bash "$0" "${args[@]}" >/dev/null || fail "pairing this host as a node failed"
  wait_agent
  log "node agent connected"

  if [ "${WANT_TAILSCALE:-no}" = yes ]; then
    local tsname tsaddr tssuffix
    # The FIRST DNSName in the document is Self's — Self precedes Peer —
    # and there are as many DNSName entries as there are machines on the
    # tailnet, so a greedy match would happily return somebody else's
    # host. Derived, then VALIDATED against the tailnet suffix, because
    # being confidently wrong here means issuing for the wrong name.
    tsname="$(tailscale status --json 2>/dev/null | grep -m1 '"DNSName"' | sed 's/.*: *"//; s/\.\?",\?$//')"
    tssuffix="$(tailscale status --json 2>/dev/null | grep -m1 '"MagicDNSSuffix"' | sed 's/.*: *"//; s/",\?$//')"
    tsaddr="$(tailscale ip -4 2>/dev/null | head -1)"
    if [ -z "$tsname" ] || [ -z "$tsaddr" ]; then
      fail "could not read this host's tailnet name or address"
    fi
    case "$tsname" in
      *".$tssuffix") ;;
      *) fail "tailnet name '$tsname' is not under this tailnet's suffix '$tssuffix' — refusing to guess" ;;
    esac
    check_tailnet_ingress_ports "$tsaddr"
    log "installing ingress (tailnet, trusted certificate)"
    enable_capability ingress-tailnet-ts \
      "{\"node\":\"$NODE_ID\",\"tailnet_name\":\"$tsname\",\"listen_addr\":\"$tsaddr\"}"
    # The page itself, not the bare host: Keystone serves the Console at
    # / and the household's homepage under /home/.
    DASHBOARD_URL="https://$tsname/home/"
    # There is a proxy now, so the Console can sit behind it.
    #
    # The proxy reaches Keystone at this host's tailnet address, not at
    # 127.0.0.1: the upstream is dialled from inside a container, where
    # loopback is the container's own. The address is the one the
    # operator already uses to reach this node and tailscale keeps it
    # across restarts, which a Docker bridge address is not guaranteed
    # to do.
    CONSOLE_BEHIND_PROXY=1
    KEYSTONE_API_ADDR="$tsaddr:9441"
  else
    log "no tailnet: skipping ingress, services will be reachable on this host only"
    DASHBOARD_URL=""
  fi

  local dom="wildstone.internal" svc
  log "installing identity"
  enable_capability identity "{\"public_domain\":\"$dom\",\"node\":\"$NODE_ID\"}"
  # Directory admin credentials are needed only when provisioning a new account.
  # An already-provisioned resume must not rotate them.
  # The Console's own package: the console_users group, and the route
  # that puts Authelia in front of it. Nothing else declares either, so
  # without this the Console has no group to grant it and no address
  # but this host's — which is why forward-auth, written and tested,
  # had never once engaged on a real deployment.
  #
  # BEFORE create_first_owner, because that step asks for the group and
  # a group must be declared before anybody can be put in it.
  #
  # Only with an ingress. The package requires ingress:private, and on a
  # host without a tailnet there is no proxy to sit in front, so this is
  # skipped rather than failed: a Console reached on localhost is the
  # supported posture for such a household, not a broken one.
  if [ "$CONSOLE_BEHIND_PROXY" = 1 ]; then
    log "installing the console route"
    enable_capability wildstone-console \
      "{\"admin_domain\":\"$dom\",\"node\":\"$NODE_ID\",\"keystone_addr\":\"$KEYSTONE_API_ADDR\"}"
    # The household's front door is Wildstone's own homepage, not the
    # gethomepage profile of the same capability: that one only
    # presents, and the family's own page — what needs a decision, what
    # the house did, their files and their assistant — is served by
    # Keystone (packages/homepage-wildstone). gethomepage stays in the
    # catalog for a household that wants it.
    #
    # Here, with the Console, for the same two reasons: it needs the
    # proxy in front of Keystone, and it declares homepage_users, which
    # create_first_owner asks for. Without a tailnet there is no proxy,
    # and no dashboard is installed — the old one was unreachable there
    # too.
    log "installing the household homepage"
    enable_capability homepage-wildstone \
      "{\"public_domain\":\"$dom\",\"node\":\"$NODE_ID\",\"keystone_addr\":\"$KEYSTONE_API_ADDR\"}"
    # Its two widgets, installed with it rather than left for somebody to
    # discover. Both are served by Keystone and need nothing on day one:
    # files keeps uploads under /srv/wildstone/media, created on first
    # use, and the assistant has no model until one is given to a Butler
    # in the Console — which its own card says, to whoever can do it.
    # Arriving in that state is the point: the dashboard is where the
    # household finds out what is left to set up.
    log "installing the homepage's files and assistant"
    enable_capability filedrop-wildstone \
      "{\"public_domain\":\"$dom\",\"node\":\"$NODE_ID\",\"keystone_addr\":\"$KEYSTONE_API_ADDR\"}"
    enable_capability assistant-wildstone \
      "{\"public_domain\":\"$dom\",\"node\":\"$NODE_ID\",\"keystone_addr\":\"$KEYSTONE_API_ADDR\"}"
    # The household's shared lists, behind the dashboard's Lists card and
    # the assistant's lists tools. Nothing runs: Keystone keeps them.
    log "installing the household lists"
    enable_capability lists-wildstone "{\"node\":\"$NODE_ID\"}"
    # Calendar is part of the homepage. Keep the generated credential on
    # reruns: changing the Secret without rerunning its credential-file
    # Job would leave Radicale using the old password. The generation
    # parameter makes the Job run when this installer first adopts an
    # existing Calendar installation or a credential is replaced.
    CALENDAR_PROVISIONED=0
    if [ -f /etc/wildstone/calendar.env ]; then
      CALENDAR_PROVISIONED=1
      CALENDAR_PASS="$(sed -n 's/^CALENDAR_PASSWORD=//p' /etc/wildstone/calendar.env)"
    else
      CALENDAR_PASS="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    fi
    [[ "$CALENDAR_PASS" =~ ^[0-9a-f]{48}$ ]] || fail "invalid Calendar credential in /etc/wildstone/calendar.env"
    local secret_response calendar_generation
    calendar_generation="$(printf '%s' "$CALENDAR_PASS" | sha256sum | cut -c1-16)"
    secret_response="$(api POST "/api/v1/nodes/$NODE_ID/secrets/calendar-service-password" \
      "{\"value\":\"$CALENDAR_PASS\"}")"
    [[ "$secret_response" != *'"error"'* ]] || fail "could not store the Calendar credential"
    log "installing the household calendar"
    enable_capability calendar-radicale \
      "{\"public_domain\":\"$dom\",\"node\":\"$NODE_ID\",\"service_account_generation\":\"$calendar_generation\"}"
    seed_calendar_account

    # Keystone's portal and agent use the same shared collection. Keep
    # its password in a root-only EnvironmentFile, never a process flag.
    local calendar_url="https://$tsname:51443/$CALENDAR_COLLECTION/"
    if [ ! -f /etc/wildstone/calendar.env ]; then
      local calendar_env_tmp
      calendar_env_tmp="$(mktemp /etc/wildstone/calendar.env.XXXXXX)"
      printf 'CALENDAR_PASSWORD=%s\n' "$CALENDAR_PASS" > "$calendar_env_tmp"
      chmod 0600 "$calendar_env_tmp"
      mv "$calendar_env_tmp" /etc/wildstone/calendar.env
    fi
    KEYSTONE_ARGS="$(merge_keystone_args "$(read_keystone_args)" \
      " -calendar-url $calendar_url -calendar-user $CALENDAR_USER")"
    printf 'KEYSTONE_ARGS=-data-dir /var/lib/wildstone/keystone%s\n' "$KEYSTONE_ARGS" > "$KEYSTONE_ENV"
    systemctl restart keystone || fail "could not enable Calendar in Keystone"
    wait_agent
    local i calendar_ready=0
    for i in $(seq 1 30); do
      if printf 'user = "%s:%s"\n' "$CALENDAR_USER" "$CALENDAR_PASS" |
          curl -fsS --config - --connect-timeout 3 --max-time 5 -o /dev/null \
            -X PROPFIND -H 'Depth: 0' "$calendar_url" 2>/dev/null; then
        log "household calendar reachable with the service account"
        calendar_ready=1
        break
      fi
      sleep 2
    done
    [ "$calendar_ready" = 1 ] || fail "Calendar could not authenticate; see: docker logs radicale"
  fi
  create_first_owner
  for svc in $SERVICES; do
    log "installing $svc"
    enable_capability "$svc" "{\"public_domain\":\"$dom\",\"node\":\"$NODE_ID\"}"
  done
}

if [ "$ROLE" = keystone ]; then
  # On a single-host upgrade, Keystone's new reconciler can act as soon
  # as it starts. Upgrade the existing agent first so ported containers
  # are recreated by a Wardstone that publishes those ports correctly.
  if [ "$INTERACTIVE" = 1 ] && systemctl is-active --quiet wardstoned; then
    fetch_binary wardstoned
    systemctl restart wardstoned || fail "could not restart wardstoned after upgrade"
    log "existing node agent upgraded before Keystone"
  fi
  fetch_binary keystone
  fetch_binary wild
  install_uninstall
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
  [ -n "$WALLPAPER_DIR" ] || install_wallpapers
  install_native_agent
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
  if [ -n "$WALLPAPER_DIR" ]; then
    [ -d "$WALLPAPER_DIR" ] || fail "--wallpaper-dir $WALLPAPER_DIR does not exist"
    KEYSTONE_ARGS="$KEYSTONE_ARGS -wallpaper-dir $WALLPAPER_DIR"
    KEYSTONE_ARGS_SET=1
  fi
  # Reachable by the proxy, on a host that will have one.
  #
  # -api-listen defaults to 127.0.0.1:9441, and a container dialling
  # that reaches its OWN loopback. So the route wildstone-console
  # renders could never connect, and the Console could never be served
  # through Authelia — the listener has to be somewhere the proxy can
  # dial before the route pointing at it means anything.
  #
  # This widens the API to the LAN and the tailnet, which ADR-W032
  # names as the risk of the forwarded-header design, so the compensating
  # control is the thing that makes it safe rather than an afterthought:
  # Keystone serves the Console's own pages ONLY to the proxy or to
  # loopback (ConsoleUIReachableFrom), and consoleForwardAuth honours
  # Remote-User from the proxy's address alone, so a client reaching
  # this port directly can set any header it likes and be nobody. What
  # it can still do is present a bearer token, which is the same
  # authority `wild` has always carried over the same network.
  #
  # Tested on the tailnet actually being up rather than on the answer to
  # a question: the non-interactive path joins a tailnet without ever
  # setting WANT_TAILSCALE, and a flag that disagreed with reality here
  # would bind the wrong interface on exactly the hosts that need it.
  #
  # Never over an operator who named one. An explicit -api-listen is a
  # deliberate choice about where a control plane answers, the same kind
  # of choice --console-dir is treated as a few lines above, and
  # docs/audit/runtime-access.md documents a second Keystone on
  # `-api-listen 127.0.0.1:19441` that this would quietly hijack. Go's
  # flag package takes the LAST occurrence and reports nothing, so the
  # override would not even announce itself — the audit instance would
  # simply come up on the wrong interface.
  # The agent runtime (ADR-W041). A FRESH install — no env file yet —
  # starts on the native runtime when it installed and passed its
  # self-check; that is the product's default for a new household. An
  # UPGRADE is never switched: without --agent-runtime it keeps whatever
  # the env file already says (nothing, for an install that predates
  # the runtime, which is Keystone's legacy default), like every other
  # flag here. --agent-runtime legacy opts a fresh install out.
  if [ -z "$AGENT_RUNTIME" ] && [ ! -f "$KEYSTONE_ENV" ]; then
    if [ "$NATIVE_AGENT_OK" = 1 ]; then
      AGENT_RUNTIME=native
      log "Butler conversations will run on the native agent runtime (pass --agent-runtime legacy to keep Keystone's own loop)"
    else
      log "the native agent runtime is not available on this host; Butler conversations will run on Keystone's own loop"
    fi
  fi
  if [ -n "$AGENT_RUNTIME" ]; then
    case "$AGENT_RUNTIME" in
      legacy|native) ;;
      *) fail "--agent-runtime must be legacy or native, not '$AGENT_RUNTIME'" ;;
    esac
    KEYSTONE_ARGS="$KEYSTONE_ARGS -agent-runtime $AGENT_RUNTIME"
    KEYSTONE_ARGS_SET=1
  fi
  if [ -n "${HOUSEHOLD_TZ:-}" ] && ! keystone_arg_named -household-timezone; then
    KEYSTONE_ARGS="$KEYSTONE_ARGS -household-timezone $HOUSEHOLD_TZ"
    KEYSTONE_ARGS_SET=1
  fi
  if tailscale ip -4 >/dev/null 2>&1 && ! keystone_arg_named -api-listen; then
    KEYSTONE_ARGS="$KEYSTONE_ARGS -api-listen 0.0.0.0:9441"
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
EnvironmentFile=-/etc/wildstone/calendar.env
ExecStart=/usr/local/bin/keystone $KEYSTONE_ARGS
Restart=on-failure
RestartSec=3
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT

  if [ "$INTERACTIVE" = 1 ]; then
    admin_token
    post_install
    forget_admin_token
    echo
    echo "Done."
    if [ -n "${DASHBOARD_URL:-}" ]; then
      echo "  Your household's homepage: $DASHBOARD_URL"
      echo "  Sign in as '$ADMIN_USER' with the password you chose."
      if [ "$CONSOLE_BEHIND_PROXY" = 1 ]; then
        # Where the Console is, and the one way back in if the proxy
        # is the thing that breaks. Both are worth a line here because
        # neither is guessable: the Console is no longer served to
        # anything but the proxy and this host, so an operator who
        # tries its old address gets a refusal with no explanation.
        echo
        echo "  The Console is behind that same sign-in: open it from the"
        echo "  Console button at the top of the dashboard. It asks you to"
        echo "  sign in again every 12 hours, and to confirm your password"
        echo "  before anything drastic, like removing a person."
        echo "  It is no longer served to anything but the proxy and this"
        echo "  machine, so reach it there rather than by address."
        echo "  If the proxy ever fails, run 'keystone owner list' on this"
        echo "  host: the recovery commands do not go through it."
      fi
    else
      echo "  Services are running but have no ingress; re-run and choose"
      echo "  Tailscale to reach them from a browser."
    fi
    echo
    echo "  Add family members on the Console's Household screen."
    exit 0
  fi

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
install_uninstall
install -d -m 0755 /etc/wildstone
install -d -m 0700 /var/lib/wildstone/wardstone
install -d -m 0755 /srv/wildstone
install_chat_adapter
install_agent_bridge

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
