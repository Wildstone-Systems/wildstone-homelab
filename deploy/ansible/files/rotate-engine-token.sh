#!/usr/bin/env bash
# Rotate one scoped Kubernetes ServiceAccount token. Only the root-owned
# bootstrap timer runs this; Wardstone and Keystone read the resulting file,
# never the k3s administrator kubeconfig (W050 safety contract §1).
set -euo pipefail

if [[ ${EUID} -ne 0 || $# -ne 1 ]]; then
  echo 'rotate-engine-token: run as root with one ServiceAccount name' >&2
  exit 2
fi
account=$1
if [[ $account != keystone && ! $account =~ ^wardstone-[a-z0-9]([-a-z0-9.]{0,51}[a-z0-9])?$ ]]; then
  echo 'rotate-engine-token: invalid ServiceAccount name' >&2
  exit 2
fi

dir=/etc/wildstone/k3s
file=$dir/$account.token
install -d -o root -g root -m 0700 "$dir"
chmod 0700 "$dir"

expiry() {
  python3 - "$1" "$account" <<'PY'
import base64
import binascii
import json
import sys

try:
    parts = open(sys.argv[1], "rb").read().strip().split(b".")
    if len(parts) != 3:
        raise ValueError("not a JWT")
    payload = json.loads(base64.urlsafe_b64decode(parts[1] + b"=" * (-len(parts[1]) % 4)))
    if payload.get("sub") != "system:serviceaccount:wildstone-system:" + sys.argv[2]:
        raise ValueError("wrong ServiceAccount")
    value = payload.get("exp")
    if type(value) is not int or value <= 0:
        raise ValueError("no valid expiry")
    print(value)
except (OSError, ValueError, KeyError, TypeError, binascii.Error):
    sys.exit(1)
PY
}

now=$(date +%s)
# The timer runs every five minutes. Keep at least fifteen minutes on a
# token, so one missed run cannot leave the service with an expired token.
if [[ -f $file ]] && exp=$(expiry "$file" 2>/dev/null) && (( exp > now + 900 )); then
  if [[ $(stat -c '%a %u %g' "$file") != '600 0 0' ]]; then
    chown root:root "$file"
    chmod 0600 "$file"
    echo repaired
  else
    echo current
  fi
  exit 0
fi

# Only root's k3s admin context mints tokens. The token is never printed,
# and a failed request cannot replace the last token on disk.
k3s kubectl --request-timeout=10s -n wildstone-system get serviceaccount "$account" -o name >/dev/null
umask 077
tmp=$(mktemp "$dir/.${account}.token.XXXXXX")
trap 'rm -f -- "$tmp"' EXIT
k3s kubectl --request-timeout=10s -n wildstone-system create token "$account" --duration=1h > "$tmp"
new_exp=$(expiry "$tmp") || { echo 'rotate-engine-token: issued token has no expiry' >&2; exit 1; }
if (( new_exp <= now + 900 )); then
  echo 'rotate-engine-token: issued token expires too soon' >&2
  exit 1
fi
chmod 0600 "$tmp"
mv -f -- "$tmp" "$file"
echo rotated
