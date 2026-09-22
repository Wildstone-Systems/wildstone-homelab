# Detach Wildstone while keeping apps running

Run `sudo wildstone-uninstall --keep-apps` on each Wildstone host. The
installer places this command in `/usr/local/bin` when the release includes
it. From a source checkout, run `sudo bash scripts/uninstall.sh --keep-apps`.

This stops and disables Keystone and Wardstone, then removes their service
units and command-line binaries. It leaves every Docker container, volume,
network, and image alone. It also keeps the Tailscale socket mount, Docker,
and Tailscale running. Immich, Jellyfin, the ingress proxy, and identity
services therefore keep their existing runtime and data.

The shared directories `/etc/wildstone`, `/srv/wildstone`,
`/var/lib/wildstone`, and `/usr/share/wildstone` remain. Retained containers
can bind-mount files under those paths, so deleting them during detach would
break the apps. The command does not convert the apps into an independently
managed Compose stack; Docker restart policies continue to govern their
restarts, and Wildstone will no longer reconcile or upgrade them.

To remove the retained apps later, first inventory their containers and
volumes, then remove them with an explicit data-deletion procedure. Do not
use broad Docker prune commands on a host with other workloads.
