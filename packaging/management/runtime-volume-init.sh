#!/bin/sh
set -eu

root="${1:?runtime volume root is required}"
target="$root/keystone"

fail() {
  echo "wildstone runtime volume: $*" >&2
  exit 1
}

[ -d "$root" ] || fail "volume root is not a directory"
[ ! -L "$target" ] || fail "keystone path must not be a symbolic link"

unexpected="$(find "$root" -mindepth 1 -maxdepth 1 ! -name keystone -print -quit)"
[ -z "$unexpected" ] || fail "volume root is not empty and needs an explicit migration"

if [ -e "$target" ]; then
  [ -d "$target" ] || fail "keystone path is not a directory"
  ownership="$(stat -c '%u:%g:%a' "$target")"
  [ "$ownership" = "65532:65532:700" ] \
    || fail "keystone directory must be owned by 65532:65532 with mode 0700"
  exit 0
fi

mkdir -m 0700 "$target" \
  || fail "cannot create the private keystone directory; verify fsGroup support"
# A setgid volume root can propagate that bit to a new child despite mkdir's
# requested mode. The child is ours, so reduce it to the exact private mode.
chmod g-s,u=rwx,go= "$target" \
  || fail "cannot secure the private keystone directory"

ownership="$(stat -c '%u:%g:%a' "$target")"
[ "$ownership" = "65532:65532:700" ] \
  || fail "created keystone directory has unexpected ownership or permissions"
