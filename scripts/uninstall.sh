#!/usr/bin/env bash
set -euo pipefail

# Detach the Wildstone control plane from this host while retaining every
# Docker container, volume, network, and bind-mounted support file.
if [[ $# != 1 || $1 != --keep-apps ]]; then
  echo 'Usage: sudo bash scripts/uninstall.sh --keep-apps' >&2
  exit 2
fi
if [[ $(id -u) != 0 ]]; then
  echo 'Run with sudo.' >&2
  exit 1
fi
running=()
if command -v docker >/dev/null; then
  docker info >/dev/null
  owner_filter='label=dev.wildstone.owner=wildstone'
  mapfile -t running < <(docker container ls -q --filter "$owner_filter")
fi
echo "Retaining ${#running[@]} running Wildstone-managed containers:"
if ((${#running[@]})); then
  docker container inspect --format '  {{.Name}}' "${running[@]}"
fi

# The socket mount is still needed by tailnet ingress, so leave its unit
# running. Likewise, leave Docker and Tailscale untouched. Before deleting
# /srv/wildstone later, follow "Removing the managed prefix" in
# docs/uninstall.md: rm -rf would delete through that mount.
for unit in keystone.service wardstoned.service; do
  if [[ -e /etc/systemd/system/$unit ]]; then
    systemctl disable --now "$unit"
  fi
done
rm -f -- /etc/systemd/system/keystone.service /etc/systemd/system/wardstoned.service
systemctl daemon-reload
rm -f -- /usr/local/bin/keystone /usr/local/bin/wardstoned /usr/local/bin/wild
# The native agent runtime is program files, not data: no app mounts it.
rm -rf -- /usr/lib/wildstone/native-agent

# The applications still mount files under /etc/wildstone and /srv/wildstone;
# deleting those paths would break retained containers, including ingress
# and identity. State and shared data are retained for the same reason.
for id in "${running[@]}"; do
  if [[ $(docker container inspect --format '{{.State.Running}}' "$id") != true ]]; then
    echo "Container $id stopped during detach; inspect with docker logs." >&2
    exit 1
  fi
done
echo 'Wildstone control plane detached. Docker apps and their supporting files remain.'
