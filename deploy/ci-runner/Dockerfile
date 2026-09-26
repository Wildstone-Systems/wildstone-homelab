# The CI runner image: the official GitHub Actions runner, pinned by
# digest, plus the three things the workflows assume a runner has and
# the stock image does not ship:
#
#   build-essential  go test -race needs cgo, and cgo needs a C compiler
#   xz-utils         ci.yml and scripts/build-native-agent.sh extract
#                    .tar.xz archives
#   git              the go job runs `git diff --exit-code` after code
#                    generation (the base installs git too; explicit here
#                    so a base change cannot silently drop it)
#
# Everything else a job needs it downloads itself, pinned by checksum
# (setup-go, setup-node, shellcheck in ci.yml, gh in release.yml).
#
# Build on the CI machine WITH A TAG, and pin the result by image ID:
#   docker build -q -t wildstone-ci-runner:current deploy/ci-runner
# The ID it prints is a content hash: the image cannot change under it,
# and that is what goes in ci-runner.env. The tag is not what the unit
# runs; it exists so the image is never "dangling", which is what
# `docker image prune` deletes by default. Rebuild deliberately when the
# base digest below moves or a package needs updating, then update
# ci-runner.env with the new ID.
FROM ghcr.io/actions/actions-runner@sha256:ee54ad8776606f29434f159196529b7b9c83c0cb9195c1ff5a7817e7e570dcfe

USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential xz-utils git \
 && rm -rf /var/lib/apt/lists/*
USER runner
