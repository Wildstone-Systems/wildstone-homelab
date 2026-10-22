#!/usr/bin/env bash
# Offline behavioral checks: no real mounts, host changes, or disk filling.
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir "$fixture/bin"
cat > "$fixture/bin/df" <<'STUB'
#!/usr/bin/env bash
if [[ ${DF_MODE:-} == malformed ]]; then
  echo 'unparseable output'
else
  echo 'Filesystem 1-blocks Used Available Capacity Mounted on'
  echo "fixture 100000000000 1 ${DF_FREE:-20000000000} ${DF_USED:-10}% /"
fi
exit "${DF_STATUS:-0}"
STUB
cat > "$fixture/bin/findmnt" <<'STUB'
#!/usr/bin/env bash
[[ $1 == -M && $2 == /fixture/backup ]] || exit 2
[[ ${MOUNT_TYPE:-nfs4} != absent ]] || exit 1
echo "${MOUNT_TYPE:-nfs4}"
STUB
chmod +x "$fixture/bin/df" "$fixture/bin/findmnt"
export PATH="$fixture/bin:$PATH"
export MOUNT_LIST_FILE="$fixture/mounts"
: > "$MOUNT_LIST_FILE"

expect() {
  local want=$1 description=$2 status=0
  shift 2
  "$@" > "$fixture/output" 2>&1 || status=$?
  if [[ $status != "$want" ]]; then
    echo "FAIL: $description (got $status, want $want)" >&2
    cat "$fixture/output" >&2
    exit 1
  fi
}

expect 0 'healthy root' bash "$script_dir/disk-guard.sh"
expect 1 'percentage at threshold' env DF_USED=85 bash "$script_dir/disk-guard.sh"
expect 1 'low available bytes' env DF_FREE=16106127359 bash "$script_dir/disk-guard.sh"
expect 0 'available bytes at threshold' env DF_FREE=16106127360 bash "$script_dir/disk-guard.sh"
expect 2 'df failure even with parseable output' env DF_STATUS=1 bash "$script_dir/disk-guard.sh"
expect 2 'malformed df output' env DF_MODE=malformed bash "$script_dir/disk-guard.sh"
expect 0 'leading zero thresholds are decimal' env MAX_USED_PERCENT=085 MIN_FREE_BYTES=016106127360 bash "$script_dir/disk-guard.sh"
expect 2 'invalid threshold' env MAX_USED_PERCENT=oops bash "$script_dir/disk-guard.sh"
expect 2 'percentage over 100' env MAX_USED_PERCENT=101 bash "$script_dir/disk-guard.sh"
expect 2 'oversized bytes threshold' env MIN_FREE_BYTES=9999999999999999999 bash "$script_dir/disk-guard.sh"
expect 2 'missing configured mount list' env MOUNT_LIST_FILE="$fixture/missing" bash "$script_dir/disk-guard.sh"
expect 2 'directory as mount list' env MOUNT_LIST_FILE="$fixture" bash "$script_dir/disk-guard.sh"
ln -s "$fixture/missing" "$fixture/broken"
expect 2 'broken mount list symlink' env MOUNT_LIST_FILE="$fixture/broken" bash "$script_dir/disk-guard.sh"

printf '%s\n' '# required backup mount' '/fixture/backup' > "$MOUNT_LIST_FILE"
expect 0 'NFSv4 mount' bash "$script_dir/disk-guard.sh"
expect 0 'NFS mount' env MOUNT_TYPE=nfs bash "$script_dir/disk-guard.sh"
expect 1 'local directory fallback' env MOUNT_TYPE=ext4 bash "$script_dir/disk-guard.sh"
expect 1 'missing mount' env MOUNT_TYPE=absent bash "$script_dir/disk-guard.sh"
expect 1 'unrecognized filesystem' env MOUNT_TYPE=nfs-invalid bash "$script_dir/disk-guard.sh"
printf '%s' 'relative/path' > "$MOUNT_LIST_FILE"
expect 1 'relative path without final newline' bash "$script_dir/disk-guard.sh"
printf '%s' '/fixture/backup' > "$MOUNT_LIST_FILE"
expect 0 'mount path without final newline' bash "$script_dir/disk-guard.sh"

unit_dir="$script_dir/../packaging/host-logging"
target=$(sed -n 's/^Unit=//p' "$unit_dir/disk-guard.timer")
expect 0 'timer target is a shipped service' test -f "$unit_dir/$target"
echo 'disk-guard: 21 offline checks passed'
