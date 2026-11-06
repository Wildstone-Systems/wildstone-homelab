#!/usr/bin/env bash
# Offline fixture checks: no runtime socket, host path, or live state is used.
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
root="$fixture/root"
bin="$fixture/bin"
mkdir -p "$root" "$bin"

cat > "$bin/docker" <<'STUB'
#!/usr/bin/env bash
if [[ $1 == --host ]]; then
  [[ $2 == unix:///var/run/docker.sock ]] || exit 9
  shift 2
fi
case "${DOCKER_MODE:-ok}:$*" in
  fail:*) exit 7 ;;
  malformed:'image ls --no-trunc --format {{.ID}}') printf '\n' ;;
  *:'image ls --no-trunc --format {{.ID}}') printf '%s\n' sha256:live sha256:restic sha256:scheduled sha256:unused ;;
  *:'ps -aq --no-trunc') printf '%s\n' live-container scheduled-container ;;
  *:'container inspect --format {{.Image}} live-container') printf '%s\n' sha256:live ;;
  *:'container inspect --format {{.Image}} scheduled-container') printf '%s\n' sha256:scheduled ;;
  *:'image inspect --format {{.Id}} restic/restic@sha256:9940e6c7421ab3a21ee9fdc4e91596d04c792b600d31b102696c75cbf3ac4481') printf '%s\n' sha256:restic ;;
  *:'image inspect --format {{.Id}} example.internal/scheduled@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa') printf '%s\n' sha256:scheduled ;;
  *:'image inspect --format {{.Size}} sha256:live') printf '%s\n' 100 ;;
  *:'image inspect --format {{.Size}} sha256:restic') printf '%s\n' 200 ;;
  *:'image inspect --format {{.Size}} sha256:scheduled') printf '%s\n' 250 ;;
  *:'image inspect --format {{.Size}} sha256:unused') printf '%s\n' 300 ;;
  *) exit 8 ;;
esac
STUB

cat > "$bin/ctr" <<'STUB'
#!/usr/bin/env bash
if [[ $1 == --address ]]; then
  [[ $2 == /run/k3s/containerd/containerd.sock && $3 == --namespace && $4 == k8s.io ]] || exit 9
  shift 4
fi
case "${CTR_MODE:-ok}:$*" in
  fail:*) exit 7 ;;
  malformed:'containers info workload') printf '%s\n' '{not-json' ;;
  *:'images list -q') printf '%s\n' example.internal/live@sha256:bbbb example.internal/scheduled@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa example.internal/unused@sha256:cccc ;;
  *:'containers list -q') printf '%s\n' workload ;;
  *:'containers info workload') printf '%s\n' '{"Image":"example.internal/live@sha256:bbbb"}' ;;
  *) exit 8 ;;
esac
STUB
chmod +x "$bin/docker" "$bin/ctr"

run_report() {
  local output=$1
  shift
  env \
    PATH="$bin:/usr/local/bin:/usr/bin:/bin" \
    STORAGE_REPORT_ROOT="$root" \
    REPORT_TIMESTAMP=2026-11-06T00:00:00Z \
    MIN_FREE_BYTES=1 \
    "$@" \
    python3 "$script_dir/storage-growth-report.py" > "$output"
}

assert_report() {
  local output=$1 expression=$2
  python3 - "$output" "$expression" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as report_file:
    report = json.load(report_file)
if not eval(sys.argv[2], {"__builtins__": {"str": str}}, {"r": report}):
    raise SystemExit(f"assertion failed: {sys.argv[2]}\n{json.dumps(report, indent=2)}")
PY
}

reset_root() {
  rm -rf "$root"
  mkdir -p "$root/etc/wildstone" "$root/var/log" "$root/var/run" "$root/run/k3s/containerd"
  printf '%s\n' \
    '# scheduled job inventory' \
    'example.internal/scheduled@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
    > "$root/etc/wildstone/storage-growth.protected-images"
}

output="$fixture/report.json"

# Docker-only, including named-volume, pinned-image, and scheduled-image safety.
reset_root
touch "$root/var/run/docker.sock"
mkdir -p "$root/var/lib/docker/image" "$root/var/lib/docker/overlay2" \
  "$root/var/lib/docker/volumes/application-data/_data" "$root/var/cache/wildstone/build" \
  "$root/var/cache/unlisted"
printf data > "$root/var/lib/docker/volumes/application-data/_data/preserve"
printf old > "$root/var/cache/wildstone/build/disposable"
printf old > "$root/var/cache/unlisted/not-a-candidate"
touch -d '2026-09-01T00:00:00Z' "$root/var/cache/wildstone/build/disposable"
mv "$bin/ctr" "$bin/ctr.disabled"
run_report "$output"
assert_report "$output" 'r["status"] == "complete" and r["runtime"]["docker"]["unreferencedImageCount"] == 1'
assert_report "$output" 'r["runtime"]["containerd"]["presence"] == "not_present"'
assert_report "$output" 'r["runtime"]["containerd"]["imageStorageStatus"] == "not_present"'
assert_report "$output" '[c["id"] for c in r["candidates"]] == ["wildstone-build"]'
assert_report "$output" '"application-data" not in str(r) and "restic/restic" not in str(r) and "example.internal" not in str(r)'
first_build_bytes=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["categories"]["buildCacheBytes"])' "$output")
dd if=/dev/zero of="$root/var/cache/wildstone/build/new-growth" bs=4096 count=2 status=none
run_report "$output"
second_build_bytes=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["categories"]["buildCacheBytes"])' "$output")
(( second_build_bytes > first_build_bytes ))
status=0
run_report "$output" MIN_FREE_BYTES=999999999999999999 || status=$?
[[ $status == 0 ]]
assert_report "$output" 'r["filesystem"]["belowMinimum"] is True and [c["id"] for c in r["candidates"]] == ["wildstone-build"]'
test -f "$root/var/cache/wildstone/build/disposable"
mv "$bin/ctr.disabled" "$bin/ctr"

# Containerd-only.
reset_root
touch "$root/run/k3s/containerd/containerd.sock"
mkdir -p "$root/var/lib/rancher/k3s/agent/containerd/io.containerd.content.v1.content" \
  "$root/var/lib/rancher/k3s/agent/containerd/io.containerd.snapshotter.v1.overlayfs"
mv "$bin/docker" "$bin/docker.disabled"
run_report "$output"
assert_report "$output" 'r["status"] == "complete" and r["runtime"]["containerd"]["unreferencedImageCount"] == 1'
assert_report "$output" 'r["runtime"]["docker"]["presence"] == "not_present"'
mv "$bin/docker.disabled" "$bin/docker"

# Coexistence hosts collect both runtimes without double-counting category totals.
reset_root
touch "$root/var/run/docker.sock" "$root/run/k3s/containerd/containerd.sock"
mkdir -p "$root/var/lib/docker/image" \
  "$root/var/lib/rancher/k3s/agent/containerd/io.containerd.content.v1.content"
run_report "$output"
assert_report "$output" 'r["status"] == "complete" and r["categories"]["totalsAreNonAdditive"] is True'
assert_report "$output" 'r["runtime"]["docker"]["presence"] == "present" and r["runtime"]["containerd"]["presence"] == "present"'

# An absent runtime is distinct from a present storage tree with no usable binary.
reset_root
rm -f "$root/var/run/docker.sock" "$root/run/k3s/containerd/containerd.sock"
mv "$bin/docker" "$bin/docker.disabled"
mv "$bin/ctr" "$bin/ctr.disabled"
run_report "$output"
assert_report "$output" 'r["status"] == "complete" and r["runtime"]["docker"]["presence"] == "not_present" and r["runtime"]["containerd"]["presence"] == "not_present"'
assert_report "$output" 'r["runtime"]["docker"]["imageStorageStatus"] == "not_present" and r["categories"]["cacheStatus"]["apt-archives"] == "not_present"'
mkdir -p "$root/var/lib/docker/image"
status=0
run_report "$output" || status=$?
[[ $status == 1 ]]
assert_report "$output" 'r["status"] == "partial" and "docker_binary_missing_with_runtime" in r["errors"]'
mv "$bin/docker.disabled" "$bin/docker"
mv "$bin/ctr.disabled" "$bin/ctr"

# Failed and malformed runtime reference output fails closed and reports no unreferenced count.
reset_root
touch "$root/var/run/docker.sock"
mkdir -p "$root/var/lib/docker/image"
status=0
run_report "$output" DOCKER_MODE=fail || status=$?
[[ $status == 1 ]]
assert_report "$output" 'r["status"] == "partial" and r["runtime"]["docker"]["unreferencedImageCount"] is None'
status=0
run_report "$output" DOCKER_MODE=malformed || status=$?
[[ $status == 1 ]]
assert_report "$output" 'r["status"] == "partial" and "docker_reference_unknown" in r["errors"] and r["runtime"]["docker"]["unreferencedImageCount"] is None'
reset_root
touch "$root/run/k3s/containerd/containerd.sock"
mkdir -p "$root/var/lib/rancher/k3s/agent/containerd/io.containerd.content.v1.content"
status=0
run_report "$output" CTR_MODE=malformed || status=$?
[[ $status == 1 ]]
assert_report "$output" 'r["status"] == "partial" and "containerd_container_reference_malformed" in r["errors"]'

# Invalid configuration is rejected without echoing the supplied value.
status=0
run_report "$output" RETENTION_DAYS=not-a-number || status=$?
[[ $status == 2 ]]
assert_report "$output" 'r["status"] == "error" and "RETENTION_DAYS" in r["error"] and "not-a-number" not in r["error"]'

unit_dir="$script_dir/../packaging/host-logging"
timer_target=$(sed -n 's/^Unit=//p' "$unit_dir/storage-growth-report.timer")
test -f "$unit_dir/$timer_target"
grep -qx 'ExecStart=/usr/local/libexec/wildstone-storage-growth-report' \
  "$unit_dir/storage-growth-report.service"
if grep -Eiq '^Exec(Start|StartPre|StartPost)=.*(delete|prune|remove|rm )' \
  "$unit_dir/storage-growth-report.service"; then
  echo 'storage growth service contains a mutation command' >&2
  exit 1
fi

echo 'storage-growth-report: 7 offline scenarios passed'
