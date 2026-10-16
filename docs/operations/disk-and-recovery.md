# Disk and recovery guardrails

The host must retain enough free space for Docker, containerd, databases, and
the journal. A local check also detects backup mounts that have fallen back
to an ordinary directory on the VM. An external monitor must watch this host:
an alert emitted only by the failed VM cannot report a total outage.

## Host logging

Install `packaging/host-logging/journald.conf` as
`/etc/systemd/journald.conf.d/90-wildstone-disk.conf`, and
`packaging/host-logging/rsyslog.logrotate` as `/etc/logrotate.d/rsyslog`.
Validate the latter with `logrotate -d /etc/logrotate.conf`, then restart
`systemd-journald`. The journal keeps at most 1 GiB and leaves at least
10 GiB free. Rsyslog rotates daily and compresses seven generations. The
`su root syslog` line is required when `/var/log` is group writable.

For Docker hosts using the default `json-file` log driver, merge
`packaging/host-logging/docker-daemon.json` into `/etc/docker/daemon.json`.
Preserve any other daemon settings. Validate the merged file with
`dockerd --validate --config-file /etc/docker/daemon.json`, then restart
Docker during a maintenance window. The new default affects containers
created after the restart; replace existing containers through the normal
Wildstone reconciliation path to apply it to them.

## Disk and mount check

Install `scripts/disk-guard.sh` as
`/usr/local/libexec/wildstone-disk-guard`, and the matching service and timer
from `packaging/host-logging/` into `/etc/systemd/system/`. Enable the timer.
It fails at 85% root usage or below 15 GiB available. Its mount list is an
optional root-owned file at `/etc/wildstone/disk-guard.mounts`, with one
absolute NFS bind-mount target per line. For example:

```text
/var/lib/docker/volumes/<backup-repository-volume>/_data
/var/lib/docker/volumes/<restore-drill-volume>/_data
```

The check requires an actual NFS mount at each target. A local directory
with the same path fails. An absent default mount-list file selects root-only
checking. An explicitly set `MOUNT_LIST_FILE` must name a readable regular file;
a missing or unreadable configured list is an error. Filesystem-read failures
also fail the check. Threshold overrides accept decimal integers (0–100 for
`MAX_USED_PERCENT`, at most 18 digits for `MIN_FREE_BYTES`).

Run `bash scripts/test-disk-guard.sh` for offline threshold, command-failure,
configuration and mount-fallback checks. These use fake commands and do not
change host state. Monitor the service result and the timer's last
successful run from outside the VM. Also monitor NAS capacity and backup age.

## Backup and restore evidence

Inspect the backup ledger for successful runs at each plan's declared
cadence, and review the monthly restore-drill result. A successful mount
or a readable repository is not proof that a backup can restore. Run a
periodic repository integrity check and restore a sample into a separate
location. Keep a protected off-host copy of the Statefile, runtime database,
secret material, host mount configuration, systemd overrides, and recovery
instructions. The NAS repository protects against VM disk loss; a separate
copy is needed to survive loss of the NAS itself.

Do not clear Docker volumes or prune all images as a generic disk remedy.
The backup engine uses a pinned restic image for one-shot jobs, and named
volumes contain application data. Investigate growth by location and owner
before removing anything.

## Startup dependencies

Storage-dependent jobs must refuse to run when their required NFS mounts
are absent. Keep that check with each affected job or app so a backup mount
failure does not stop unrelated services. Verify this with a controlled
boot test while the NAS path is unavailable, then restore connectivity and
confirm backups resume.
