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

Ephemeral fixes this structurally rather than by policy: `run-one-job.sh`
starts a `docker run --rm` of the official runner image with a
**just-in-time (JIT) runner credential**, a single-use registration
minted from the GitHub API for exactly one runner. The runner takes one
job, deregisters and exits, and the container is deleted with it.
systemd starts a fresh one. There is no state to poison.

Three details the script enforces rather than asks for:

- **The repository-administration token never enters a container.**
  It is read by the script on the host and used for one API call, to
  mint the JIT credential. Only that credential reaches `docker run`,
  and it is spent the moment the runner starts.
- **The container gets no Docker socket, no host mounts, and no
  capabilities.** A job that could reach the host's Docker daemon could
  start a privileged container, read the token file, and leave images,
  volumes and detached containers behind for the next job; no amount of
  token handling or cleanup survives that. So the job runs inside one
  unprivileged container (uid 1001, `--cap-drop ALL`,
  `no-new-privileges`) and nothing else. `ci.yml` was changed to need
  no Docker: shellcheck is a checksum-verified release tarball rather
  than an image.
- **The image is an immutable reference**, and the script refuses a
  tag. It is built here from `Dockerfile`: the official runner pinned by
  digest, plus a C compiler (`go test -race` needs cgo), `xz` (the
  workflows extract `.tar.xz`) and `git` (the go job runs `git diff`).
  Nothing else: every other tool a job needs it downloads itself, pinned
  by checksum. `docker build -q` prints the image ID, a content hash,
  and that is what goes in `ci-runner.env`.

The host needs `docker`, `curl` and `python3`.

## Where to run it

**On the CI machine, which is already separate from both the household
host and the test homelab.** Keep it that way. The container is the
first boundary; the machine is the second, for the day a container
escape exists:

- Docker installed, nothing else that matters on it.
- Firewall it so it can reach github.com and container registries and
  **cannot** reach Keystone, the NAS, or the test homelab.
- Snapshot it once configured; rebuild from the snapshot if in doubt.

## Install

```bash
# On the CI VM, as root, from a checkout of this repo:
mkdir -p /etc/wildstone /usr/local/lib/wildstone
docker build -q deploy/ci-runner           # prints the image ID
install -m 0600 deploy/ci-runner/ci-runner.env.example /etc/wildstone/ci-runner.env
$EDITOR /etc/wildstone/ci-runner.env      # set ACCESS_TOKEN and RUNNER_IMAGE (the ID above)
install -m 0755 deploy/ci-runner/run-one-job.sh /usr/local/lib/wildstone/run-one-job.sh
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
- The shellcheck step no longer uses Docker (see above), so nothing in
  CI needs a Docker socket. Keep it that way: a step that needs
  `docker run` is a step that needs the host.
- The image is the stock runner plus a compiler, `xz` and `git`, and
  nothing else a hosted runner happens to have. A workflow that needs a
  tool downloads it pinned by checksum (`release.yml` does this for
  `gh`, `ci.yml` for shellcheck) rather than growing the image. If a
  job needs something that cannot be fetched that way, it goes in the
  `Dockerfile` with a line saying which job needs it.
- `runner.tool_cache` is empty on every job, so `setup-go` and
  `setup-node` download toolchains each time. Jobs are slower by that
  much and never wrong because of a stale cache.
