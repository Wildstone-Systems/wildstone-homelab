#!/usr/bin/env bash
# Total-loss recovery from S3 alone (ADR-W018 A2, ADR-W045 A5/A6).
# The host data, Statefile, runtime database, node identity, application
# volume and every local backup repository are destroyed. Only a recovery
# kit and an external S3-compatible server survive.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WILDSTONE_DRILL_WORK:-$(mktemp -d /tmp/wildstone-offsite-exit-drill.XXXXXX)}"
BIN="$WORK/bin"
API="http://127.0.0.1:9441"
NODE="drill-node"
CANARY="offsite-exit-drill-$(date +%s)-$RANDOM"
S3_ACCESS="drill-access"
S3_SECRET="drill-secret-$RANDOM-$RANDOM"
S3_BUCKET="wildstone-drill"
S3_CONTAINER="wildstone-offsite-drill-s3"
VERSITY_IMAGE="${VERSITY_IMAGE:-versity/versitygw@sha256:30292fc2eeacc67a36993b01f7a7a5e3361a19cced0e80c1d71cfa2a4b0a2499}"
KEYSTONE_PID=""
WARDSTONE_PID=""

log() { printf '\n=== offsite drill: %s\n' "$*"; }
fail() {
  echo "OFFSITE DRILL FAILED: $*" >&2
  tail -80 "$WORK"/keystone*.log >&2 2>/dev/null || true
  tail -50 "$WORK"/wardstone*.log >&2 2>/dev/null || true
  docker logs "$S3_CONTAINER" >&2 2>/dev/null || true
  exit 1
}
cleanup() {
  [ -z "$KEYSTONE_PID" ] || kill "$KEYSTONE_PID" 2>/dev/null || true
  [ -z "$WARDSTONE_PID" ] || kill "$WARDSTONE_PID" 2>/dev/null || true
  docker ps -aq --filter "label=dev.wildstone.owner" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker rm -f "$S3_CONTAINER" >/dev/null 2>&1 || true
  docker volume rm -f drill-data drill-restore >/dev/null 2>&1 || true
  sudo rm -rf /srv/wildstone/recovery /srv/wildstone/recovery.previous >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_for() {
  local desc="$1" timeout="$2"; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || fail "timed out waiting for $desc"
    sleep 2
  done
}
curl_api() {
  local token="$1" method="$2" path="$3" body="${4:-}"
  if [ -n "$body" ]; then
    curl -sS -f -X "$method" -H "Authorization: Bearer $token" -H "Content-Type: application/json" -d "$body" "$API$path"
  else
    curl -sS -f -X "$method" -H "Authorization: Bearer $token" "$API$path"
  fi
}
admin_token() { grep -o 'wst_[0-9a-f]*' "$1" | head -1; }
start_keystone() {
  "$BIN/keystone" -data-dir "$1" -join-tokens "$2" -ingest-interval 2s -reconcile-interval 1s -backup-interval 1h >"$3" 2>&1 &
  KEYSTONE_PID=$!
  wait_for "Keystone API" 30 curl -sS -f "$API/api/v1/healthz"
}
start_wardstone() {
  # Wardstone owns privileged host and Docker operations in production.
  sudo -n "$BIN/wardstoned" -node-id "$NODE" -keystone 127.0.0.1:9440 -data-dir "$1" -join-token "$2" >"$3" 2>&1 &
  WARDSTONE_PID=$!
}
s3_ready() { [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7070/)" != "000" ]; }

log "building real binaries"
mkdir -p "$BIN" "$WORK/s3" "$WORK/iam"
(cd "$REPO_ROOT/keystone" && go build -o "$BIN/keystone" ./cmd/keystone)
(cd "$REPO_ROOT/wardstone" && go build -o "$BIN/wardstoned" ./cmd/wardstoned)
(cd "$REPO_ROOT/cli" && go build -o "$BIN/wild" ./cmd/wild)

log "starting the external S3 survivor"
docker run -d --name "$S3_CONTAINER" -p 7070:7070 \
  -v "$WORK/s3:/data/s3" -v "$WORK/iam:/data/iam" \
  -e "ROOT_ACCESS_KEY=$S3_ACCESS" -e "ROOT_SECRET_KEY=$S3_SECRET" \
  -e VGW_PORT=:7070 -e VGW_IAM_DIR=/data/iam \
  -e VGW_BACKEND=posix -e VGW_BACKEND_ARGS=/data/s3 \
  "$VERSITY_IMAGE" >/dev/null
wait_for "S3 gateway" 60 s3_ready
BRIDGE_GATEWAY="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')"
S3_ENDPOINT="http://$BRIDGE_GATEWAY:7070"

log "phase 1: protect application data and the whole control plane off-site"
K1="$WORK/keystone1"; W1="$WORK/wardstone1"
mkdir -p "$K1" "$W1"
sudo install -d -o "$(id -u)" -g "$(id -g)" /srv/wildstone
start_keystone "$K1" drill-join-1 "$WORK/keystone1.log"
ADMIN="$(admin_token "$WORK/keystone1.log")"
[ -n "$ADMIN" ] || fail "no bootstrap admin token"
start_wardstone "$W1" drill-join-1 "$WORK/wardstone1.log"
wait_for "agent join" 30 grep -q "agent connected" "$WORK/keystone1.log"

"$BIN/wild" recovery keygen "$WORK/recovery.key"
PUBLIC_KEY="$(tr -d '\r\n' < "$WORK/recovery.key.pub")"
for item in "offsite-access:$S3_ACCESS" "offsite-secret:$S3_SECRET" "restic-drill:drill-password" "restic-wildstone:wildstone-password"; do
  name="${item%%:*}"; value="${item#*:}"
  printf '%s' "$value" | WILD_TOKEN="$ADMIN" "$BIN/wild" secret set "$name" -node "$NODE"
done

PROPOSAL="$(jq -n --arg endpoint "$S3_ENDPOINT" --arg node "$NODE" --arg pub "$PUBLIC_KEY" \
  --arg volume "$(cat <<EOF
apiVersion: wildstone.dev/v1
kind: Volume
metadata: { name: drill-data, owner: capability/drill, node: $NODE }
spec: { name: drill-data }
EOF
)" --arg target "$(cat <<EOF
apiVersion: wildstone.dev/v1
kind: BackupTarget
metadata: { name: offsite, owner: system, node: $NODE }
spec:
  type: s3
  endpoint: $S3_ENDPOINT
  bucket: $S3_BUCKET
  pathStyle: true
  insecureHTTP: true
  accessKeySecret: secret/offsite-access
  secretKeySecret: secret/offsite-secret
EOF
)" --arg app "$(cat <<EOF
apiVersion: wildstone.dev/v1
kind: BackupPlan
metadata: { name: drill, owner: capability/drill, node: $NODE }
spec:
  volumes: [drill-data]
  class: critical
  offsite:
    target: offsite
    route: offsite-only
    passwordSecret: secret/restic-drill
    schedule: daily
EOF
)" --arg recovery "$(cat <<EOF
apiVersion: wildstone.dev/v1
kind: BackupPlan
metadata: { name: wildstone, owner: system, node: $NODE }
spec:
  paths: [/srv/wildstone/recovery]
  class: critical
  recoveryPublicKey: $PUBLIC_KEY
  offsite:
    target: offsite
    route: offsite-only
    passwordSecret: secret/restic-wildstone
    schedule: daily
EOF
)" '{summary:"off-site total-loss drill",put:[$volume,$target,$app,$recovery]}')"
PLAN_ID="$(curl_api "$ADMIN" POST /api/v1/plans "$PROPOSAL" | jq -r .id)"
if [ -z "$PLAN_ID" ] || [ "$PLAN_ID" = null ]; then
  fail "proposal did not return an id"
fi
WILD_TOKEN="$ADMIN" "$BIN/wild" plan approve "$PLAN_ID"
wait_for "data volume" 60 docker volume inspect drill-data

docker run --rm -v drill-data:/data --entrypoint /bin/sh busybox:stable -c "printf '%s' '$CANARY' > /data/canary.txt"
WILD_TOKEN="$ADMIN" "$BIN/wild" backup run drill
WILD_TOKEN="$ADMIN" "$BIN/wild" backup run wildstone

export WILD_RECOVERY_READ_KEY="$S3_SECRET"
export WILD_RECOVERY_REPOSITORY_PASSWORDS='{"wildstone":"wildstone-password","drill":"drill-password"}'
"$BIN/wild" recovery kit -out "$WORK/recovery-kit.json" -endpoint "$S3_ENDPOINT" -bucket "$S3_BUCKET" -path-style -access-key-id "$S3_ACCESS" -private-key "$WORK/recovery.key"
unset WILD_RECOVERY_READ_KEY WILD_RECOVERY_REPOSITORY_PASSWORDS
rm -f "$WORK/recovery.key" "$WORK/recovery.key.pub"

log "phase 2: destroy every local recovery input"
kill "$KEYSTONE_PID" "$WARDSTONE_PID"; wait "$KEYSTONE_PID" "$WARDSTONE_PID" 2>/dev/null || true
KEYSTONE_PID=""; WARDSTONE_PID=""
sed -i 's/wst_[0-9a-f]*/[destroyed-bootstrap-token]/g' "$WORK/keystone1.log"
docker ps -aq --filter "label=dev.wildstone.owner" | xargs -r docker rm -f >/dev/null 2>&1 || true
docker volume rm drill-data >/dev/null
rm -rf "$K1"
sudo rm -rf "$W1"
sudo rm -rf /srv/wildstone/recovery /srv/wildstone/recovery.previous
if [ -e "$K1/statefile" ] || [ -e "$K1/keystone.db" ]; then
  fail "control-plane files survived the loss"
fi
ADMIN=""

log "phase 3: recover the control plane from the kit and S3"
K2="$WORK/keystone2"; W2="$WORK/wardstone2"
"$BIN/keystone" recover -kit "$WORK/recovery-kit.json" -data-dir "$K2"
"$BIN/keystone" claim-code -data-dir "$K2" -name recovery-drill -out "$WORK/claim-code"
ADMIN="$(tr -d '\r\n' < "$WORK/claim-code")"
rm -f "$WORK/claim-code"
mkdir -p "$W2"
start_keystone "$K2" drill-join-2 "$WORK/keystone2.log"
start_wardstone "$W2" drill-join-2 "$WORK/wardstone2.log"
wait_for "fresh node join" 30 grep -q "agent connected" "$WORK/keystone2.log"

NODE_BUNDLE="$(jq -r --arg node "$NODE" '.nodeBundles[$node]' "$K2/recovery-import/manifest.json")"
WILD_TOKEN="$ADMIN" "$BIN/wild" recovery import-node "$NODE" -bundle "$K2/recovery-import/$NODE_BUNDLE" -kit "$WORK/recovery-kit.json"

log "approving the High-risk target drift on the rebuilt host"
DRIFT_ID=""
for _ in $(seq 1 30); do
  DRIFT_ID="$(curl_api "$ADMIN" GET /api/v1/plans | jq -r '.[] | select(.state=="proposed" and .principal=="system/drift") | .id' | head -1)"
  [ -z "$DRIFT_ID" ] || break
  sleep 2
done
[ -n "$DRIFT_ID" ] || fail "no off-site drift-repair plan appeared"
WILD_TOKEN="$ADMIN" "$BIN/wild" plan approve "$DRIFT_ID"
wait_for "recreated data volume" 60 docker volume inspect drill-data

log "restoring the application from S3 only"
WILD_TOKEN="$ADMIN" "$BIN/wild" backup restore drill -from offsite
wait_for "restore volume" 120 docker volume inspect drill-restore
RESTORED="$(docker run --rm -v drill-restore:/r --entrypoint /bin/sh busybox:stable -c 'cat "$(find /r -name canary.txt | head -1)"')"
[ "$RESTORED" = "$CANARY" ] || fail "canary mismatch: wrote '$CANARY', restored '$RESTORED'"

WILD_TOKEN="$ADMIN" "$BIN/wild" restore reconcile --release
log "PASS: Statefile, quarantined runtime DB, sealed node secrets and '$CANARY' all returned from S3"
