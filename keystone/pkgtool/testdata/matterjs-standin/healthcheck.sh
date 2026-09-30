#!/bin/sh
# Upstream's docker/matterjs-server/healthcheck.sh (matterjs-server
# v1.4.0), unchanged except for this comment and using node instead of
# curl, which the stand-in's base image does not ship.
# Resolves LISTEN_ADDRESS (first value if comma-separated) into a healthcheck
# URL, falling back to localhost. IPv6 literals are bracketed for the URL.
set -eu

port="${PORT:-5580}"
listen="${LISTEN_ADDRESS:-}"
addr="${listen%%,*}"
addr="${addr:-localhost}"

case "$addr" in
    *:*) url="http://[$addr]:${port}/health" ;;
    *)   url="http://$addr:${port}/health" ;;
esac

exec node -e 'fetch(process.argv[1], { signal: AbortSignal.timeout(5000) }).then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))' "$url"
