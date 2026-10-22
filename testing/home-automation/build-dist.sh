#!/usr/bin/env bash
# Build release-shaped artifacts from the checked-out branch, for
# installing an unreleased build on a test machine with
#   sudo bash ./install.sh --artifact-dir <this dist directory> ...
# (testing/home-automation/README.md, step 0; #453).
#
#   testing/home-automation/build-dist.sh [amd64|arm64]
#
# It needs only Docker: Go and Node run in the official golang and node
# images, as your own user, so nothing is installed on this machine and
# nothing it writes is owned by root. Output goes to dist/ at the top of
# the repository (git ignores it). The default architecture is this
# machine's.
#
# What it builds mirrors .github/workflows/release.yml for one
# architecture: keystone, wardstoned and wild, the Console, the package
# catalog, the wallpapers, install.sh/uninstall.sh and checksums.txt.
# The chat adapter, agent bridge and native agent runtime are left out;
# install.sh skips what checksums.txt does not list.
set -euo pipefail

GO_IMAGE="golang:1.26"
NODE_IMAGE="node:22"

repo="$(cd "$(dirname "$0")/../.." && pwd)"
arch="${1:-}"
if [ -z "$arch" ]; then
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "unsupported machine $(uname -m): pass amd64 or arm64" >&2; exit 2 ;;
  esac
fi
case "$arch" in amd64|arm64) ;; *) echo "architecture must be amd64 or arm64" >&2; exit 2 ;; esac

command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
sha="$(git -C "$repo" rev-parse --short HEAD)"
version="v0.0.0-test-$sha"
dist="$repo/dist"
cache="$repo/dist/.cache"
mkdir -p "$dist" "$cache/go" "$cache/npm"
find "$dist" -maxdepth 1 -type f -delete

as_me=(--user "$(id -u):$(id -g)" -e HOME=/tmp -v "$repo:/src")

echo "[build-dist] console (npm ci && npm run build)"
docker run --rm "${as_me[@]}" -v "$cache/npm:/npm" -e npm_config_cache=/npm \
  -e PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 -w /src/console "$NODE_IMAGE" \
  sh -c 'npm ci --no-audit --no-fund && npm run build'

echo "[build-dist] keystone, wardstoned, wild for linux/$arch ($version)"
docker run --rm "${as_me[@]}" -v "$cache/go:/go-cache" \
  -e GOPATH=/go-cache/path -e GOCACHE=/go-cache/build -e GOFLAGS=-modcacherw \
  -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH="$arch" -e VERSION="$version" \
  -w /src "$GO_IMAGE" sh -c '
    set -e
    build() { # name module package
      (cd "$2" && go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "/src/dist/$1" "$3")
    }
    build keystone   keystone  ./cmd/keystone
    build wardstoned wardstone ./cmd/wardstoned
    build wild       cli       ./cmd/wild
  '

cd "$dist"
for name in keystone wardstoned wild; do
  tar -czf "${name}_linux_${arch}.tar.gz" "$name"
  rm "$name"
done
tar -czf console_dist.tar.gz -C "$repo/console/dist" .
tar -czf packages.tar.gz -C "$repo/packages" .
tar -czf wallpapers.tar.gz -C "$repo/wallpapers" .
cp "$repo/scripts/install.sh" "$repo/scripts/uninstall.sh" .
sha256sum -- *.tar.gz install.sh uninstall.sh > checksums.txt

echo "[build-dist] done: $dist ($version, linux/$arch)"
ls -l "$dist"
