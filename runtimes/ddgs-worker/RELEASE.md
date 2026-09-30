# DDGS managed package: incomplete release evidence

Issue #493 / ADR-W047. The package is catalog support, not an installed service
or a tool grant. It requires an explicit `worker_image` repository digest;
the example in YAML is a shape-only lint fixture and is not pullable evidence.
No default image is supplied until an actual build is published and verified.

## Build and publication

The existing `.github/workflows/release.yml` bundles `packages/` into the
checksummed `packages.tar.gz`. It has no container publisher, registry login or
packages-write permission. CI's self-hosted runner is intentionally socket-free.
Do not add a Docker socket or new workflow secret to obtain image evidence.

An authorized publisher with registry access, a buildx multi-platform builder
and push rights can run `scripts/build-ddgs-worker.sh` with three arguments:
a verified `python:3.12.14-slim-bookworm@sha256:<digest>` base, an explicit
registry/repository:tag, and a new output directory outside the source tree. Existing evidence directories
are refused so a failed build cannot be confused with an old successful pin.
The helper builds linux/amd64 and linux/arm64 from hash-locked requirements,
records source revision/base/digest and emits provenance/SBOM metadata.
It performs an explicit registry push, never installation or deployment.
No floating base is accepted by this release helper. The Dockerfile's tag-only
default is a developer convenience, not the managed release procedure.
Pinned inputs and recorded provenance are reproducibility controls; byte-for-byte
reproduction is not claimed until two independent builds have been compared.

Before treating #493 as complete, retain `build-metadata.json` and `image.json`,
verify the published digest is pullable on both architectures, run the fake
upstream worker suite and hardened-container health/shutdown smoke fixtures,
and add the verified image as the reviewed package default with a version bump.
Use the normal `wild pkg` lint/build/sign/verify tooling for distribution.
Signing fixtures use ephemeral generated keys, not a household signing key.
No live upstream query is part of these fixtures; S36 owns live conformance.

## Current evidence and remaining gate

On 2026-09-30 Docker client/server 28.4.0 were available. Building the S18
Dockerfile failed on Docker Hub's Python manifest HEAD with `Forbidden`, also
on an escalated retry. No worker image was produced. No registry publication,
container smoke test, reproducibility comparison or live conformance passed.
The supported release workflow cannot supply that missing build. A publisher
with permitted registry access and an agreed artifact destination is required.

## Lifecycle and readiness

An Owner explicitly installs `research-ddgs` through the ordinary capability
Plan, on Keystone's node. Reconfiguration and removal use the same approval and
reconcile lifecycle. Fixed capability/container names prevent per-Agent replicas.
The package is internal-only, with one loopback published port, bounded resources,
read-only root, dropped capabilities and no mounts, credentials or public Route.
It supplies `service/ddgs`; Keystone derives the endpoint from rendered desired
state, refuses other nodes/references and rereads configuration on every resolve.

Before a Plan changes/removes that container, all provider tests for its reference
are invalidated, including in-flight receipts. Probe completion is serialized
with approval writes. Failed applies remain unavailable. This does not select
providers, grant tools, automatically test upstreams or deploy on upgrade.
The production `ResearchTest` callback remains nil: endpoint configuration alone
is not proof of reconciled image/dependency identity. Wiring a test requires
verified current-revision reconciliation, not merely a reachable old listener.
This integration is incomplete while release evidence is absent; removal still
invalidates any persisted successful test immediately on approved desired change.

Trusted homelab-network access is the accepted #493 boundary. Sibling containers
and local processes may call the worker directly, bypassing Keystone permissions,
budgets and audit. Host loopback publication does not provide caller isolation.
Do not expose the worker to the public Internet. This limitation does not alter
Keystone's grant, provenance, privacy or transport obligations.
