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

## Removing the managed prefix

Deleting `/srv/wildstone` with a plain `rm -rf` follows bind mounts and
deletes into `/run/tailscale` (destroying tailscaled's live local API) and
into any NAS share mounted beneath it. Tear down the mounts first:

0. Inventory host-path application data first. Several packages keep
   household data in ordinary directories under the prefix, for example
   `/srv/wildstone/media`, `/srv/wildstone/knowledge`, and
   `/srv/wildstone/sync`. Back up or move anything you want to keep; step 5
   deletes it irreversibly.
1. Stop and disable the socket mount:
   `sudo systemctl disable --now wildstone-tailscale-socket.service`
   (its `ExecStop` unmounts `/srv/wildstone/tailscale`). On hosts still
   running v0.1.7 or v0.1.8 this unit does not exist; use
   `sudo systemctl disable --now srv-wildstone-tailscale.mount` instead.
2. Delete the unit and reload systemd:
   `sudo rm -f /etc/systemd/system/wildstone-tailscale-socket.service
   /etc/systemd/system/srv-wildstone-tailscale.mount &&
   sudo systemctl daemon-reload`.
3. Unmount any NFS shares under `/srv/wildstone` and remove their
   `/etc/fstab` lines.
4. Confirm nothing is still mounted beneath the prefix:
   `findmnt -rn -o TARGET | grep -E '^/srv/wildstone(/|$)'` must print
   nothing. (`findmnt -R /srv/wildstone` is not sufficient: the prefix is an
   ordinary directory, so it reports no submounts.)
5. Only then run `sudo rm -rf --one-file-system /srv/wildstone`.

Do this only after the retained apps that use the prefix are gone.
