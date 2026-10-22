# installer/

**The installer lives at [`scripts/install.sh`](../scripts/install.sh).**
This directory is a placeholder, kept for the layout in decision D2; it
holds no code, and nothing here is shipped. Look here for the bootstrap
script and you will find nothing — start with `scripts/install.sh` and
[`docs/install.md`](../docs/install.md) instead.

Per decision D2 (you-first), the bootstrap script is fetched from GitHub
releases; no hosted install endpoint until a second user exists.
`scripts/install.sh` is published as a release asset alongside the
binaries, covered by the release's `checksums.txt`. It installs Docker CE
if absent, installs `wardstoned` (systemd) on a node, and pulls up
Keystone on the control plane; onboarding continues in the Console, and
the Restore path branches from the same flow (see
[`docs/recovery.md`](../docs/recovery.md)).

This directory becomes real if and when the installer outgrows one shell
script — a preflight binary, or a hosted endpoint. Until then, one
script beats a directory that promises one.
