#!/usr/bin/env bash
# Package the native agent runtime (ADR-W041) for one architecture:
#
#   scripts/build-native-agent.sh <amd64|arm64> <out-dir>
#
# Produces <out-dir>/native-agent_linux_<arch>.tar.gz, containing:
#
#   bin/node                  the Node runtime, from nodejs.org, pinned below
#   main.mjs                  the sidecar, bundled with its dependencies
#   THIRD_PARTY_LICENSES.txt  licenses of what main.mjs bundles
#   LICENSE.node              Node's license
#   VERSION                   what is inside, one line each
#
# A control-plane host installs this and needs no Node, npm or
# node_modules of its own (#34 kept Node off those hosts for the Console;
# this keeps it off for the agent too).
#
# Requires the bundle to be built first:
#   (cd runtimes/native-agent && npm ci && npm run bundle)
#
# NODE_CACHE (optional) is a directory to keep downloaded Node tarballs in.
set -euo pipefail

# Bumping Node is a reviewed change: take the new version's lines from
# https://nodejs.org/dist/v<version>/SHASUMS256.txt. A download that does
# not match these hashes is refused.
NODE_VERSION="v24.21.0"
NODE_SHA256_x64="fd8e59d5a511510f6a298afb548f18c7d2b1be404d8b4a27d94fbe49f56cb2d6"
NODE_SHA256_arm64="6ad1325edbdb5649c379b75a237147a666c95d4f9ae8d340fef2d1575d289ad2"

fail() { echo "build-native-agent: $*" >&2; exit 1; }

[ $# -eq 2 ] || fail "usage: $0 <amd64|arm64> <out-dir>"
arch="$1"
out="$2"
case "$arch" in
  amd64) node_arch=x64;   want_sha="$NODE_SHA256_x64" ;;
  arm64) node_arch=arm64; want_sha="$NODE_SHA256_arm64" ;;
  *) fail "unsupported architecture: $arch" ;;
esac

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bundle="$repo/runtimes/native-agent/dist/bundle"
[ -f "$bundle/main.mjs" ] || fail "no bundle at $bundle/main.mjs; run: (cd runtimes/native-agent && npm ci && npm run bundle)"
[ -f "$bundle/THIRD_PARTY_LICENSES.txt" ] || fail "bundle has no THIRD_PARTY_LICENSES.txt"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cache="${NODE_CACHE:-$work/cache}"
mkdir -p "$cache" "$out"

node_name="node-${NODE_VERSION}-linux-${node_arch}"
node_tar="$cache/${node_name}.tar.xz"
if [ ! -f "$node_tar" ] || ! echo "$want_sha  $node_tar" | sha256sum -c - >/dev/null 2>&1; then
  curl -fsSL -o "$node_tar.part" "https://nodejs.org/dist/${NODE_VERSION}/${node_name}.tar.xz" \
    || fail "could not download ${node_name}.tar.xz"
  mv -f "$node_tar.part" "$node_tar"
fi
echo "$want_sha  $node_tar" | sha256sum -c - >/dev/null \
  || fail "checksum mismatch for ${node_name}.tar.xz (expected $want_sha)"

stage="$work/stage"
mkdir -p "$stage/bin"
# Only the runtime and its license: npm, corepack and the headers are
# for developing, and nothing on an installed host develops.
tar -xJf "$node_tar" -C "$work" "${node_name}/bin/node" "${node_name}/LICENSE"
install -m 0755 "$work/${node_name}/bin/node" "$stage/bin/node"
install -m 0644 "$work/${node_name}/LICENSE" "$stage/LICENSE.node"
install -m 0644 "$bundle/main.mjs" "$stage/main.mjs"
install -m 0644 "$bundle/THIRD_PARTY_LICENSES.txt" "$stage/THIRD_PARTY_LICENSES.txt"

sdk_version="$(sed -n 's/.*"@openai\/agents-core": *"\([^"]*\)".*/\1/p' "$repo/runtimes/native-agent/package.json")"
{
  echo "node ${NODE_VERSION}"
  echo "openai-agents-sdk ${sdk_version:-unknown}"
  echo "arch linux/${arch}"
} > "$stage/VERSION"

artifact="$out/native-agent_linux_${arch}.tar.gz"
# Owner and mtime normalised so the archive does not carry whoever built it.
tar --owner=0 --group=0 --numeric-owner -czf "$artifact" -C "$stage" .
echo "built $artifact"
