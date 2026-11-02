#!/usr/bin/env bash
set -euo pipefail

image="${1:-wildstone-management:ci}"
suffix="${RANDOM}${RANDOM}"
first="wildstone-management-first-$suffix"
second="wildstone-management-second-$suffix"
volume="wildstone-management-runtime-$suffix"
cleanup() {
  docker rm -f "$first" "$second" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() { echo "management image: $*" >&2; exit 1; }

echo "checking image identity and packaged assets"
[ "$(docker image inspect "$image" --format '{{.Config.User}}')" = "65532:65532" ] \
  || fail "image does not declare the verified unprivileged runtime identity"
docker run --rm "$image" -version | grep '^keystone ' >/dev/null
docker run --rm --entrypoint /bin/sh "$image" -ec '
  test -x /usr/local/bin/keystone
  test -s /usr/share/wildstone/console/index.html
  test -s /usr/share/wildstone/packages/wildstone-console/package.yaml
  test -s /usr/share/wildstone/wallpapers/NOTICE.md
  test -x /usr/lib/wildstone/native-agent/bin/node
  test -s /usr/lib/wildstone/native-agent/main.mjs
  test -s /usr/lib/wildstone/native-agent/VERSION
  git --version >/dev/null
  /usr/lib/wildstone/native-agent/bin/node /usr/lib/wildstone/native-agent/main.mjs --health \
    | grep -q ready
'

docker volume create "$volume" >/dev/null
echo "checking first startup, health, Console and synthetic authorization"
docker run -d --name "$first" \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=134217728 \
  -v "$volume:/var/lib/wildstone/keystone" \
  -p 127.0.0.1::9441 "$image" >/dev/null
port="$(docker port "$first" 9441/tcp | sed -n 's/.*://p')"
[ -n "$port" ] || fail "Docker did not publish the API port"
for _ in $(seq 1 240); do
  if curl -fsS "http://127.0.0.1:$port/api/v1/healthz" | grep '"ok":true' >/dev/null; then break; fi
  sleep 0.25
done
curl -fsS "http://127.0.0.1:$port/api/v1/healthz" | grep '"ok":true' >/dev/null || fail "health endpoint did not become ready"
curl -fsS "http://127.0.0.1:$port/" | grep -i '<!doctype html>' >/dev/null || fail "Console was not served by Keystone"
logs="$(docker logs "$first" 2>&1)"
token="$(printf '%s\n' "$logs" | grep -Eo 'wst_[A-Za-z0-9_-]+' | sed -n '1p')"
[ -n "$token" ] || fail "first boot did not create its synthetic bootstrap credential"
curl -fsS -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/v1/whoami" \
  | grep '"role":"admin"' >/dev/null || fail "synthetic bootstrap authorization failed"
docker stop "$first" >/dev/null
docker rm "$first" >/dev/null

echo "checking restart, persistent state and authorization continuity"
docker run -d --name "$second" \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=134217728 \
  -v "$volume:/var/lib/wildstone/keystone" \
  -p 127.0.0.1::9441 "$image" >/dev/null
port="$(docker port "$second" 9441/tcp | sed -n 's/.*://p')"
for _ in $(seq 1 240); do
  if curl -fsS "http://127.0.0.1:$port/api/v1/healthz" >/dev/null; then break; fi
  sleep 0.25
done
curl -fsS -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/v1/whoami" \
  | grep '"role":"admin"' >/dev/null || fail "authorization did not survive image restart"
if docker logs "$second" 2>&1 | grep -Eq 'wst_[A-Za-z0-9_-]+'; then
  fail "restart minted a replacement bootstrap credential"
fi

echo "PASS: management image contents, native runtime, Console/API startup, restart, persistence and authorization continuity"

