#!/usr/bin/env bash
# Exits nonzero when root disk headroom is low or a required mount is absent.
# An off-host monitor should check the timer/service result and its freshness.
set -euo pipefail
export LC_ALL=C

max_used=${MAX_USED_PERCENT:-85}
min_free=${MIN_FREE_BYTES:-16106127360}
mount_list=${MOUNT_LIST_FILE:-/etc/wildstone/disk-guard.mounts}

if [[ ! $max_used =~ ^[0-9]{1,3}$ || ! $min_free =~ ^[0-9]{1,18}$ ]]; then
  echo 'disk-guard: thresholds must be decimal integers (percent: 0-100; bytes: up to 18 digits)' >&2
  exit 2
fi
max_used=$((10#$max_used))
min_free=$((10#$min_free))
if (( max_used > 100 )); then
  echo 'disk-guard: percent threshold must be between 0 and 100' >&2
  exit 2
fi

if ! usage=$(df -P -B1 /); then
  echo 'disk-guard: cannot read root filesystem usage' >&2
  exit 2
fi
read -r _ _ _ free used_percent _ <<< "${usage##*$'\n'}"
used_percent=${used_percent%%%}
if [[ ! $free =~ ^[0-9]+$ || ! $used_percent =~ ^[0-9]+$ ]]; then
  echo 'disk-guard: cannot parse root filesystem usage' >&2
  exit 2
fi

failed=0
if (( used_percent >= max_used || free < min_free )); then
  echo "disk-guard: root disk low: ${used_percent}% used, ${free} bytes available" >&2
  failed=1
fi

# An absent default list means root-only checking. An explicitly configured
# list, broken symlink, directory, or unreadable existing list is an error.
if [[ -e $mount_list || -L $mount_list || ${MOUNT_LIST_FILE+x} ]]; then
  if [[ ! -f $mount_list || ! -r $mount_list ]]; then
    echo "disk-guard: cannot read mount list: $mount_list" >&2
    exit 2
  fi
  while IFS= read -r mount_path || [[ -n $mount_path ]]; do
    [[ -z $mount_path || $mount_path == \#* ]] && continue
    if [[ $mount_path != /* ]]; then
      echo "disk-guard: mount path must be absolute: $mount_path" >&2
      failed=1
      continue
    fi
    fs_type=$(findmnt -M "$mount_path" -n -o FSTYPE 2>/dev/null || true)
    if [[ $fs_type != nfs && $fs_type != nfs4 ]]; then
      echo "disk-guard: required NFS mount absent: $mount_path" >&2
      failed=1
    fi
  done < "$mount_list"
fi

if (( failed )); then
  exit 1
fi
echo "disk-guard: root ${used_percent}% used, ${free} bytes available; required mounts present"
