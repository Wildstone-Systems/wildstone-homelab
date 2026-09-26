# Ephemeral CI runner

`ci.yml` and `release.yml` run on a self-hosted runner labelled
`dev-services`. This directory is how that runner is meant to be run
once the repository is public: **one job per runner, then thrown away.**

## Why

A persistent runner keeps its filesystem between jobs. Whatever one job
leaves behind — a cache, a stub download, a modified tool on `PATH`, a
background process — the next job inherits, including the release
build. On a private repository the only jobs are the maintainer's own.
On a public one, a pull request the maintainer approved by mistake is
also a job. The runner must not be somewhere that mistake can persist.

Ephemeral fixes this structurally rather than by policy: the runner is
a `docker run --rm` that registers with `--ephemeral`, takes exactly one
job, deregisters and exits, and the container is deleted with it.
systemd starts a fresh one. There is no state to poison.

This is the third lock, after the two that keep strangers off the
runner in the first place: `ci.yml` skips pull requests from forks, and
the repository's Actions settings require the maintainer's approval
before a fork's workflow runs.

## Where to run it

**On the CI machine, which is already separate from both the household
host and the test homelab.** Keep it that way. The runner mounts the
Docker socket (shellcheck runs through `docker run`), so a job can do
anything Docker on that machine can. The machine is the isolation
boundary, not the container:

- Docker installed, nothing else that matters on it.
- Firewall it so it can reach github.com and container registries and
  **cannot** reach Keystone, the NAS, or the test homelab.
- Snapshot it once configured; rebuild from the snapshot if in doubt.

## Install

```bash
# On the CI VM, as root, from a checkout of this repo:
mkdir -p /etc/wildstone
install -m 0600 deploy/ci-runner/ci-runner.env.example /etc/wildstone/ci-runner.env
$EDITOR /etc/wildstone/ci-runner.env      # set ACCESS_TOKEN
install -m 0644 deploy/ci-runner/github-runner@.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now github-runner@1 github-runner@2
```

Two instances means two jobs run at once. `ci.yml` has five jobs, one
of them a two-way matrix, so two runners roughly halve a run. Add
`github-runner@3` if you want more.

Then in the repository, Settings → Actions → Runners: the new runners
appear as `ci-vm-<host>-1` and `-2` with the `dev-services` label.
**Remove the old persistent runner** from the same page and stop its
service on whichever machine it lives, so no job can land there.

## Verify

```bash
systemctl status 'github-runner@*'
journalctl -fu github-runner@1
```

Push a branch or re-run a workflow. Each job should show a runner name
from this VM, and `docker ps` on the VM should show a container appear
per job and disappear when it finishes.

## What changes for the workflows

Nothing has to. Two things get simpler:

- The "clear a half-written gitleaks download" step in `ci.yml` exists
  only because a persistent runner kept `/tmp` between jobs. It is
  harmless here and can go once the old runner is gone.
- `runner.tool_cache` is empty on every job, so `setup-go` and
  `setup-node` download toolchains each time. Jobs are slower by that
  much and never wrong because of a stale cache.
