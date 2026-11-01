# Audiobookshelf offline Helm integration

Status: experimental #256 implementation; proposed ADR-W005 A7 extension under accepted W050. No live catalog entry, install endpoint, Git publication, cluster access or existing-release adoption. #257 and #258 implement those later boundaries.

## Source evaluation

The authorized private proof uses bdclark's `audiobookshelf` chart 0.1.6. The upstream tag resolves to commit `c424d45686c631ce9840306a9d1c804e5bb4f148`; the chart declares appVersion 2.37.2 and MIT licensing. It includes a values schema and chart tests, with no chart dependencies in this revision. This is a community chart, not an official Audiobookshelf support guarantee. Continued release activity and compatible upgrades must be re-evaluated before updates are accepted.

Inspected upstream Chart.yaml, values schema/defaults and Deployment/Service/PVC templates at that commit. The chart supports existing claims, explicit environment references, probes, security contexts and resources. The Deployment image template concatenates repository and tag, so `tag@sha256:digest` supplies a digest-pinned image without a chart fork. The application image manifest digest for tag 2.37.2 was resolved through the GHCR registry API. The fixture records those pins; it contains no private proof identifiers or secrets.

The chart's optional root permissions init container recursively chowns app paths. That is disabled in this integration. Storage must already be usable by UID/GID 1000. The proof's root init-container setting is deliberately not copied. No automatic chmod/chown is added.

## Small staged Package extension

`pkgspec.Package.Helm` records source URL, full commit, relative chart path, chart version and digest-pinned application image. Only the experimental Audiobookshelf mapping is supported. Reuse existing Package metadata, policies, parameter declaration and timezone validation; do not invent a second catalog or a general templating/plugin engine.

`capability.Engine.ProjectHelm` accepts that profile, timezone, explicit namespace/release/reconciler/claim/secret bindings, encrypted-secret configuration digest and accepted product revision. It returns three standard files and a review digest:

- `source.yaml`: Flux GitRepository pinned to upstream commit, in the application namespace.
- `release.yaml`: Flux HelmRelease referencing the chart path, scoped reconciler, pinned image and validated values.
- `kustomization.yaml`: standard Kustomize file listing the source/release. The surrounding Flux Kustomization and its scoped ServiceAccount belong to reviewed bootstrap/publication configuration, not this ordinary app output.

For a Git source, immutable commit selection pins chart bytes; the declared chart version is inventory/review metadata and is verified against upstream Chart.yaml during integration validation. Flux's Git chart build does not enforce that version selector as an HTTP repository would. `reconcileStrategy: Revision` follows the pinned source revision.

All three claims and the named Secret/key `jwt-secret` must already exist in the application namespace. The projector neither creates replacement empty claims nor obtains secret values. The supplied secret revision is a digest of encrypted configuration, not a plaintext secret hash. #257 must verify that revision and reconcile the secret provisioning sequence; supplying a digest here is not proof of current cluster contents.

The caller supplies an accepted revision from the authoritative product snapshot, not arbitrary working-tree HEAD. Review digest binds the entire parsed Package, accepted revision, all bindings and exact file bytes. It is review material, never an authorization token. #257 must verify source authority and the digest before publishing; this API does not establish that the caller owns a named claim or namespace.

The profile lives only in `keystone/capability/testdata/audiobookshelf-helm.yaml`. Ordinary `Expand` refuses it rather than silently declaring an empty successful install. Older Package parsers reject the optional field. No live package or versions.lock changes occur.

## Supported and unsupported facets

| Facet | Current offline support |
|---|---|
| Deployment | Standard source/release files; singleton Recreate with declared interruption |
| Configuration | Validated timezone; fixed private ClusterIP service, health probes and bounded resources; no arbitrary Helm values |
| Storage | Distinct pre-existing config, metadata and writable media claims; UID/GID 1000 prerequisite; no automatic relocation |
| Credentials | Named secret reference plus encrypted revision in review binding; no generated/loaded value |
| Identity/exposure | Application-native authentication; no public ingress or forward-auth rule generated; native-client/OIDC integration deferred |
| Backup/recovery | Contract required below; no backup Job or automated restore implemented by projection |
| Agent API actions/events | Unsupported in this slice; chart installation grants no Agent tools |
| Multi-node/HA | Unsupported/unproven; one replica is not a fencing mechanism |

## Data and lifecycle contract

Config and metadata hold accounts/listening state and must be captured consistently with media/podcast paths and required credential material. The first mapping has one writable media claim, not a complete replacement for every current Docker library/podcast layout. Do not claim migration parity or omit library content from backups without an explicit owner policy. Existing backup/recovery evidence is supporting evidence only for its tested layout/version.

Backups must quiesce writers; restore alongside with application login/library/data checks. Upgrades may change application data schemas, so older image selection alone is not recovery. Claims remain externally owned: the rendered chart creates no PVC, but release uninstall still stops the application and Flux pruning removes its objects. Data deletion, chart changes that create/rename claims, or ownership transfer need separate reviewed retention/adoption steps. The surrounding Flux deletion policy is not configured by this offline function.

## Verification and remaining gates

Tests cover deterministic output, changed input/source/revision/secret-reference binding, unknown/raw inputs, invalid timezone, credentials in source URLs, floating sources/images, mixed resource templates/hooks, distinct storage roles, namespace constraints and refusal through the ordinary install path. Existing package/capability suites and race checks must remain green.

Local validation renders the generated values against the pinned upstream chart using Helm lint/template (including its values schema). Inspect rendered kinds, image, singleton strategy, non-root context, existing claims and absence of init containers. No live API or ownership checks are claimed. Generated GitRepository v1 and HelmRelease v2 also passed JSON Schema validation against the official upstream Flux CRDs retrieved on 2026-11-01. These local checks do not execute CEL admission rules or establish compatibility with every controller version; hosted full-suite checks and the selected live-controller proof remain required. Live integration is #258.

Optional local export for validation uses `WILDSTONE_PROJECTION_TEST_OUT` with `go test ./capability -run TestHelmProjectionExport -count=1` from keystone. This is test-only export; the production function has no filesystem or network side effects.
