#!/usr/bin/env bash
# exit-drill.sh — the E13 exit drill (ADR-W018 A1): total-host-loss
# restore, unattended, against the REAL binaries, the runner's REAL
# Docker daemon, and the pinned restic image. No mocks anywhere.
#
# Phase 1 (first life):  keystone + wardstoned come up, a Volume pair and
#   a BackupPlan are proposed and approved over the public API, reconcile
#   creates the volumes, a canary file is written, `wild backup run`
#   produces a real restic snapshot.
# Phase 2 (host loss):   both processes are killed; the Keystone data dir
#   (runtime DB, CA, keys), the Wardstone data dir (node identity), and
#   the DATA volume are destroyed. Only the recovery contract survives
#   (ADR-W018 A2): the restic repository volume and a copy of the
#   Statefile git repo. Secret values live in the operator's vault — the
#   drill re-enters them.
# Phase 3 (rebirth):     a fresh Keystone starts over the restored
#   Statefile, a fresh Wardstone joins with a new token, the secret is
#   re-entered, reconcile recreates the (empty) data volume, and
#   `wild backup restore` brings the canary back — byte-identical.
#
# Requirements: docker, jq, go, network to pull the restic image.
# Exit code 0 means the alpha promise held: the host died and the data
# survived.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WILDSTONE_DRILL_WORK:-$(mktemp -d /tmp/wildstone-exit-drill.XXXXXX)}"
BIN="$WORK/bin"
API="http://127.0.0.1:9441"
NODE="drill-node"
CANARY="exit-drill-canary-$(date +%s)-$RANDOM"

KEYSTONE_PID=""
WARDSTONE_PID=""

log() { printf '\n=== drill: %s\n' "$*"; }

cleanup() {
    log "cleanup"
    if [ -n "$KEYSTONE_PID" ]; then kill "$KEYSTONE_PID" 2>/dev/null || true; fi
    if [ -n "$WARDSTONE_PID" ]; then kill "$WARDSTONE_PID" 2>/dev/null || true; fi
    sleep 1
    docker ps -aq --filter "label=dev.wildstone.owner" | xargs -r docker rm -f >/dev/null 2>&1 || true
    docker volume rm -f drill-data drill-repo drill-restore >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() {
    echo "DRILL FAILED: $*" >&2
    # >&2 BEFORE 2>/dev/null: the reverse order pointed stdout at the
    # already-nulled stderr and swallowed every log tail (run 3).
    echo "--- keystone log (first life) ---" >&2; tail -50 "$WORK/keystone1.log" >&2 2>/dev/null || true
    echo "--- keystone log (rebirth) ---" >&2;    tail -50 "$WORK/keystone2.log" >&2 2>/dev/null || true
    echo "--- wardstone logs ---" >&2;            tail -30 "$WORK"/wardstone*.log >&2 2>/dev/null || true
    exit 1
}

# wait_for <description> <timeout-seconds> <command...>
wait_for() {
    local desc="$1" timeout="$2"; shift 2
    local deadline=$((SECONDS + timeout))
    until "$@" >/dev/null 2>&1; do
        [ "$SECONDS" -ge "$deadline" ] && fail "timed out waiting for $desc"
        sleep 2
    done
}

curl_api() { # curl_api <token> <method> <path> [json-body]
    local token="$1" method="$2" path="$3" body="${4:-}"
    if [ -n "$body" ]; then
        curl -sS -f -X "$method" -H "Authorization: Bearer $token" \
            -H "Content-Type: application/json" -d "$body" "$API$path"
    else
        curl -sS -f -X "$method" -H "Authorization: Bearer $token" "$API$path"
    fi
}

admin_token() { # admin_token <keystone-log>: the once-printed bootstrap token
    grep -o 'wst_[0-9a-f]*' "$1" | head -1
}

start_keystone() { # start_keystone <data-dir> <join-token> <log>
    "$BIN/keystone" -data-dir "$1" -join-tokens "$2" \
        -ingest-interval 2s -reconcile-interval 1s -backup-interval 1h \
        >"$3" 2>&1 &
    KEYSTONE_PID=$!
    wait_for "keystone API" 30 curl -sS -f "$API/api/v1/healthz"
}

start_wardstone() { # start_wardstone <data-dir> <join-token> <log>
    # Wardstone is the host-privileged component: this drill exercises its
    # real /etc/wildstone and Docker operations, so run it with the same
    # authority the installed system service has.
    sudo -n "$BIN/wardstoned" -node-id "$NODE" -keystone 127.0.0.1:9440 \
        -data-dir "$1" -join-token "$2" >"$3" 2>&1 &
    WARDSTONE_PID=$!
}

latest_run_status() { # latest_run_status <token>: newest ledger row's status
    # GET /api/v1/backups wraps the ledger as {"runs": [...]} — run 3
    # timed out because the poll read the array at the top level.
    curl_api "$1" GET /api/v1/backups | jq -r '.runs[0].status // empty'
}

log "building keystone, wardstoned, wild"
mkdir -p "$BIN"
(cd "$REPO_ROOT/keystone" && go build -o "$BIN/keystone" ./cmd/keystone)
(cd "$REPO_ROOT/wardstone" && go build -o "$BIN/wardstoned" ./cmd/wardstoned)
(cd "$REPO_ROOT/cli" && go build -o "$BIN/wild" ./cmd/wild)

# The backup runner pulls the restic image itself before EnsureContainer
# (the agent's ensureContainer deliberately never pulls). The drill must
# exercise that path, so: NO pre-pull, and best-effort remove any cached
# copy of the pinned image (extracted from source so the drill can't
# drift) so the run starts from a genuinely image-less node — the exact
# state the first real VM install failed in.
# `|| true` so the guard below is reachable: under `set -euo pipefail` a
# grep that matches nothing would otherwise kill the script silently.
RESTIC_IMAGE="$(grep -o 'restic/restic@sha256:[0-9a-f]\{64\}' "$REPO_ROOT/keystone/cmd/keystone/main.go" | head -1)" || true
[ -n "$RESTIC_IMAGE" ] || fail "could not extract the pinned restic image from keystone/cmd/keystone/main.go"
docker image rm -f "$RESTIC_IMAGE" >/dev/null 2>&1 || true

# Helper containers (canary write, restore verification) use a separate
# image so they don't implicitly cache restic for the backup runner.
HELPER_IMAGE="busybox:stable"
log "pulling helper image $HELPER_IMAGE"
docker pull -q "$HELPER_IMAGE" >/dev/null

# ---------------------------------------------------------------- phase 1
log "phase 1: first life"
K1="$WORK/keystone1"; W1="$WORK/wardstone1"
mkdir -p "$K1" "$W1"
start_keystone "$K1" drill-join-1 "$WORK/keystone1.log"
ADMIN1="$(admin_token "$WORK/keystone1.log")"
[ -n "$ADMIN1" ] || fail "no bootstrap admin token in keystone output"
start_wardstone "$W1" drill-join-1 "$WORK/wardstone1.log"
wait_for "agent join" 30 grep -q "agent connected" "$WORK/keystone1.log"

log "setting the restic repository password (the operator's-vault secret)"
printf 'drill-repo-password' | WILD_TOKEN="$ADMIN1" "$BIN/wild" secret set restic-drill -node "$NODE"

log "proposing volumes + backup plan over the public API"
PROPOSAL="$(jq -n --arg v "$(cat <<EOF
apiVersion: wildstone.dev/v1
kind: Volume
metadata:
  name: drill-data
  owner: capability/drill
  node: $NODE
spec:
  name: drill-data
EOF
)" --arg r "$(cat <<EOF
apiVersion: wildstone.dev/v1
kind: Volume
metadata:
  name: drill-repo
  owner: capability/drill
  node: $NODE
spec:
  name: drill-repo
EOF
)" --arg b "$(cat <<EOF
apiVersion: wildstone.dev/v1
kind: BackupPlan
metadata:
  name: drill
  owner: capability/drill
  node: $NODE
spec:
  volumes: [drill-data]
  repository: drill-repo
  passwordSecret: secret/restic-drill
  schedule: daily
EOF
)" '{summary: "exit drill: volumes + backup plan", put: [$v, $r, $b]}')"
PLAN_ID="$(curl_api "$ADMIN1" POST /api/v1/plans "$PROPOSAL" | jq -r .id)" || true
if [ -z "$PLAN_ID" ] || [ "$PLAN_ID" = null ]; then fail "proposal did not return a plan id"; fi
WILD_TOKEN="$ADMIN1" "$BIN/wild" plan approve "$PLAN_ID"
wait_for "reconcile to create drill-data" 60 docker volume inspect drill-data
wait_for "reconcile to create drill-repo" 60 docker volume inspect drill-repo

log "writing the canary"
docker run --rm -v drill-data:/data --entrypoint /bin/sh "$HELPER_IMAGE" \
    -c "printf '%s' '$CANARY' > /data/canary.txt"

log "running the backup"
WILD_TOKEN="$ADMIN1" "$BIN/wild" backup run drill
wait_for "backup ledger success" 120 sh -c \
    "[ \"\$(curl -sS -H 'Authorization: Bearer $ADMIN1' $API/api/v1/backups | jq -r '.runs[0].status')\" = succeeded ]"

# ---------------------------------------------------------------- phase 2
log "phase 2: total host loss"
# The recovery contract (ADR-W018 A2): the Statefile replica and the
# restic repository survive; NOTHING else does.
cp -r "$K1/statefile" "$WORK/statefile-survivor"
kill "$KEYSTONE_PID" "$WARDSTONE_PID"; wait "$KEYSTONE_PID" "$WARDSTONE_PID" 2>/dev/null || true
KEYSTONE_PID=""; WARDSTONE_PID=""
docker ps -aq --filter "label=dev.wildstone.owner" | xargs -r docker rm -f >/dev/null 2>&1 || true
docker volume rm drill-data >/dev/null
rm -rf "$K1"
sudo rm -rf "$W1"
log "host is gone: data dirs deleted, data volume destroyed"

# ---------------------------------------------------------------- phase 3
log "phase 3: rebirth from the survivors"
K2="$WORK/keystone2"; W2="$WORK/wardstone2"
mkdir -p "$K2" "$W2"
cp -r "$WORK/statefile-survivor" "$K2/statefile"
start_keystone "$K2" drill-join-2 "$WORK/keystone2.log"
ADMIN2="$(admin_token "$WORK/keystone2.log")"
[ -n "$ADMIN2" ] || fail "rebirth keystone minted no bootstrap token (runtime DB was supposed to be gone)"
start_wardstone "$W2" drill-join-2 "$WORK/wardstone2.log"
wait_for "agent re-join" 30 grep -q "agent connected" "$WORK/keystone2.log"

log "re-entering the secret from the operator's vault"
printf 'drill-repo-password' | WILD_TOKEN="$ADMIN2" "$BIN/wild" secret set restic-drill -node "$NODE"

wait_for "reconcile to recreate drill-data from the statefile" 60 docker volume inspect drill-data

log "restoring alongside from the surviving repository"
WILD_TOKEN="$ADMIN2" "$BIN/wild" backup restore drill
wait_for "restore ledger success" 120 sh -c \
    "[ \"\$(curl -sS -H 'Authorization: Bearer $ADMIN2' $API/api/v1/backups | jq -r '.runs[0].status')\" = succeeded ]"

log "verifying the canary came back byte-identical"
RESTORED="$(docker run --rm -v drill-restore:/r --entrypoint /bin/sh "$HELPER_IMAGE" \
    -c 'cat "$(find /r -name canary.txt | head -1)"')"
[ "$RESTORED" = "$CANARY" ] || fail "canary mismatch: wrote '$CANARY', restored '$RESTORED'"

log "PASS: host lost, data survived (canary '$CANARY' restored intact)"
