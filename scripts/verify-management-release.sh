#!/usr/bin/env bash
# Verification policy is local code, never an identity supplied by the manifest.
set -euo pipefail
if [[ $# != 3 ]]; then
  echo "usage: $0 <expected-tag> <manifest.json> <bundle.sigstore.json>" >&2
  exit 2
fi
tag=$1
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]] || exit 2
cosign verify-blob "$2" --bundle "$3" \
  --certificate-identity "https://github.com/Wildstone-Systems/wildstone-homelab/.github/workflows/release.yml@refs/tags/${tag}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
# Only parse the manifest after cryptographic authentication.
python3 - "$tag" "$2" <<'PY'
import json
from pathlib import Path
import sys
m = json.loads(Path(sys.argv[2]).read_text())
if (m.get('schemaVersion') != 1 or m.get('product') != 'wildstone-management'
        or m.get('release') != sys.argv[1]
        or m.get('source', {}).get('repository') != 'Wildstone-Systems/wildstone-homelab'):
    raise SystemExit('unexpected manifest identity/schema')
PY
