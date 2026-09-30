#!/usr/bin/env bash
# Explicit publisher operation, never run by installation or reconciliation.
set -euo pipefail
if [[ $# != 3 ]]; then
  echo 'usage: build-ddgs-worker.sh python:3.12.14-slim-bookworm@sha256:<digest> registry/repository:tag output-directory' >&2
  exit 2
fi
base=$1
image=$2
out=$3
[[ $base =~ ^python:3\.12\.14-slim-bookworm@sha256:[a-f0-9]{64}$ ]] || { echo 'a verified pinned Python base is required' >&2; exit 2; }
[[ $image =~ ^[a-z0-9][a-z0-9./:_-]+:[a-zA-Z0-9][a-zA-Z0-9._-]*$ && $image == */* && $image != *@* ]] || { echo 'an explicit registry repository and tag are required' >&2; exit 2; }
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# Only committed source is eligible for provenance; do not silently publish
# local edits or secrets as part of a build context.
[[ -z $(git -C "$root" status --porcelain --untracked-files=all -- runtimes/ddgs-worker) ]] || { echo 'worker source must be committed and clean' >&2; exit 2; }
mkdir "$out" # refuse existing evidence directories, including stale successful pins
out=$(cd "$out" && pwd)
source_revision=$(git -C "$root" rev-parse HEAD)
source_epoch=$(git -C "$root" show -s --format=%ct HEAD)
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg "PYTHON_BASE=$base" --build-arg "SOURCE_DATE_EPOCH=$source_epoch" \
  --label "org.opencontainers.image.revision=$source_revision" \
  --provenance=mode=min --sbom=true --metadata-file "$out/build-metadata.json" \
  --tag "$image" --push "$root/runtimes/ddgs-worker"
python3 - "$out" "$image" "$base" "$source_revision" <<'PY'
import json, pathlib, re, sys
out, image, base, revision = sys.argv[1:]
metadata = json.loads((pathlib.Path(out) / 'build-metadata.json').read_text())
digest = metadata.get('containerimage.digest', '')
if not re.fullmatch(r'sha256:[a-f0-9]{64}', digest):
    raise SystemExit('builder did not report a deployment digest')
repository = image.rsplit(':', 1)[0]
(pathlib.Path(out) / 'image.json').write_text(json.dumps({
    'image': repository + '@' + digest, 'base': base,
    'sourceRevision': revision, 'platforms': ['linux/amd64', 'linux/arm64'],
    'smokeTest': 'not run', 'liveConformance': 'not run'
}, indent=2) + '\n')
PY
