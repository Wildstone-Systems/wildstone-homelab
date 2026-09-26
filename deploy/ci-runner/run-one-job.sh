#!/usr/bin/env bash
# Run exactly one GitHub Actions job in a throwaway container, then exit.
#
# Called by github-runner@.service with the instance number as $1 and
# /etc/wildstone/ci-runner.env already in the environment. Everything
# that needs the repository-administration token happens HERE, on the
# host, as root: the token is never passed to the container.
#
# What the container gets instead is a just-in-time (JIT) runner config:
# a single-use credential minted from the GitHub API for one runner
# registration, consumed the moment the runner starts. The runner it
# produces is ephemeral by definition, deregisters itself when its one
# job ends, and the container is deleted with it.
#
# The container gets NO Docker socket. A job that can reach the host's
# daemon can start a privileged container, read this file, and leave
# images, volumes and detached containers behind for the next job: no
# token handling or cleanup survives that. So the job runs inside one
# unprivileged container and nothing else; ci.yml was changed to need no
# Docker (shellcheck is a verified release tarball, not an image).
set -euo pipefail

instance="${1:?instance number required}"
: "${RUNNER_IMAGE:?RUNNER_IMAGE is required}"
: "${GITHUB_REPO:?GITHUB_REPO is required (owner/name)}"
: "${ACCESS_TOKEN:?ACCESS_TOKEN is required}"
LABELS="${LABELS:-self-hosted,dev-services}"
RUNNER_NAME_PREFIX="${RUNNER_NAME_PREFIX:-ci-vm}"
RUNNER_GROUP_ID="${RUNNER_GROUP_ID:-1}"

# The image must be pinned by digest. It receives the Docker socket, so a
# mutable tag would hand the CI machine to whoever controls the tag.
case "$RUNNER_IMAGE" in
  *@sha256:*) ;;
  *) echo "RUNNER_IMAGE must be pinned by digest (image@sha256:...), got: $RUNNER_IMAGE" >&2; exit 1 ;;
esac

name="gh-runner-${instance}"
runner_name="${RUNNER_NAME_PREFIX}-${instance}-$(date +%s)"

docker rm -f "$name" >/dev/null 2>&1 || true
docker pull "$RUNNER_IMAGE"

# Mint the single-use JIT config. This is the only call that uses the
# token, and the token stays in this process's environment on the host.
labels_json=$(printf '%s' "$LABELS" | python3 -c 'import sys,json; print(json.dumps([l.strip() for l in sys.stdin.read().split(",") if l.strip()]))')
body=$(python3 -c 'import sys,json; print(json.dumps({"name": sys.argv[1], "runner_group_id": int(sys.argv[2]), "labels": json.loads(sys.argv[3])}))' \
  "$runner_name" "$RUNNER_GROUP_ID" "$labels_json")
jit=$(curl -fsS -X POST \
  -H "Authorization: Bearer ${ACCESS_TOKEN}" \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "https://api.github.com/repos/${GITHUB_REPO}/actions/runners/generate-jitconfig" \
  -d "$body" | python3 -c 'import sys,json; print(json.load(sys.stdin)["encoded_jit_config"])')

# --rm: the container is deleted when the runner exits after its one job,
# and its work directory with it. The runner runs as uid 1001 ("runner")
# with every capability dropped and no way to gain one; the job gets a
# checkout, a toolchain it downloads itself, and nothing of this host.
# --entrypoint is explicit so this does not depend on what the image
# declares: the JIT flag goes to run.sh and nothing else.
exec docker run --rm --name "$name" \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --entrypoint /home/runner/run.sh \
  "$RUNNER_IMAGE" \
  --jitconfig "$jit"
