# Management release contract v1

Implements the publication portion of #315 under amended ADR-W050. No
self-upgrade endpoint, application upgrade, deployment adoption or live upgrade
acceptance is introduced. The public workflow is `.github/workflows/release.yml`.

## Release unit and manifest

Keystone, Console, the native runtime, catalog and assets travel in one management
image. Wardstone remains independently updated. Replacing the bundled catalog
must never authorize changes to installed applications.

`management-release.json` is UTF-8 JSON, schemaVersion 1. Its exact bytes are
signed; consumers must not normalize or reserialize before signature verification.
`scripts/management-release.py` is the deterministic producer. The release tag
is canonical v-prefixed SemVer (no build suffix). All prereleases, including
alpha/beta, are preview; only versions without a prerelease are stable.

| Field | Meaning |
| --- | --- |
| schemaVersion/product | Fixed `1` / `wildstone-management`; unknown schemas are refused |
| release/channel | Exact tag; stable or preview derived from SemVer |
| source.repository/commit | Fixed public repository and full source commit |
| image.repository/digest/platforms | Fixed GHCR repository, immutable multi-platform index SHA-256 and supported linux architectures |
| chart.path/version/files | Chart source at source.commit, chart version and SHA-256 per chart file; appVersion is not the running release identity |
| database.migrationFiles | Exact migration filenames and hashes, not a claim of backward compatibility |
| compatibility.upgradeFrom | Explicit tested predecessor allowlist; empty initially |
| compatibility.wardstoneCompatibility | `unverified` initially; installation unavailable |
| compatibility.recovery | Backup required; image rollback unsupported initially |
| upgradeEnabled | Always false in this producer; metadata awareness only |
| releaseNotes | GitHub release page; mutable explanatory content, never executable authority |

The chart is obtained from the exact public source commit, not a mutable branch.
Every chart file is bound by its hash; future consumers must reject missing,
additional or changed chart files. The signed index digest binds architecture
manifests transitively. Require a supported host platform and digest-pinned pulls.
The image label/source revision is build evidence, not permission to install.

## Compatibility rules

The initial release establishes identity and distribution only. No predecessor is
implicitly safe: a newer version, successful build, chart version or higher SQL
migration number is insufficient evidence. Empty upgradeFrom means no supported
in-place upgrade, not “all versions.” The current producer deliberately rejects
attempts to enable installation by editing only the policy file.

A later reviewed schema/producer amendment must define target/source schema and
storage-layout constraints, Wardstone protocol compatibility and tested predecessor
records. Initially support the previous supported release only. Refuse skipped,
unknown, development and downgrade paths; explain required intermediate versions.
Preview requires explicit opt-in and cannot replace stable eligibility implicitly.
No arbitrary version range or caller-supplied compatibility override is supported.

Database migrations are forward-only. Rollback compatibility must be independently
proven; replacing an image does not undo a migration. For a data-changing upgrade,
require a verified complete consistency-group backup and independent recovery,
including later-write-loss policy. #166/#163 own relevant deployment/recovery gates.

## Signing choice and trust

Use Cosign v3 through the pinned official cosign-installer action. Keyless signing
uses GitHub Actions OIDC and a short-lived certificate, with a public transparency
log entry. `management-release.sigstore.json` carries the signature, certificate
and verification evidence. There is no new persistent signing secret. Existing
Wardstone playbook signing is unchanged and has a different trust purpose.

The local verification policy requires BOTH:

- Issuer `https://token.actions.githubusercontent.com`.
- Exact certificate identity
  `https://github.com/Wildstone-Systems/wildstone-homelab/.github/workflows/release.yml@refs/tags/<expected-tag>`.

`scripts/verify-management-release.sh` performs this check before parsing identity
fields. Never read allowed issuer/workflow/trust roots from the downloaded manifest.
Never accept a signature merely because any GitHub workflow signed it. Never use
insecure transparency-log/certificate bypass flags.

Bootstrap: obtain the verifier and its Sigstore trust material through the trusted
installation/distribution path. Cosign manages Sigstore trust metadata; preserve
its verified local cache for later verification. Network-disabled/offline behavior
must be tested before the future checker claims offline support. Missing/expired
trust material is unknown/unavailable, never “current” or installable.

Ephemeral signing keys rotate per invocation. Sigstore root updates use its trust
metadata mechanisms, not an embedded key from the release being checked. Changing
repository/workflow identity needs a reviewed verifier/trust-policy update delivered
through an already trusted path; do not broaden matching automatically. Compromised
GitHub publishing authority can sign malicious releases: protect tag creation,
workflow changes and owner accounts. Signatures prove publication identity, not
software quality or release freshness.

Future checker requirements: bounded downloads (manifest 1 MiB, bundle 4 MiB),
strict known schema, duplicate-key refusal, hash/type validation, safe notes links,
stale timestamps, and remembered highest accepted release to detect regression.
A valid old signature does not prove there is no newer release. This helper is a
publication verification tool, not the future untrusted-network client/installer.
Security advisory/revocation and long-offline root refresh need a separate contract
before automatic installation; no unsupported revocation guarantees are made here.

## Publication sequence

1. Review code and policy, then create a new version tag. Do not reuse published tags.
2. Existing archive builds/checksums run as before. Prepare or resume a draft release;
   refuse an already published release instead of clobbering it.
3. Build amd64 and arm64 management images, run existing image smoke checks on each
   (arm64 via QEMU on the hosted runner), push those tested images, and assemble
   the multi-platform index using their immutable digests. No rebuild between test
   and push. Public GHCR package visibility must be checked by the release operator.
4. Generate the manifest from the checkout, policy, chart/migration hashes and index
   digest. Sign it using workflow OIDC; immediately verify exact tag identity.
5. Upload all archives, manifest and signature bundle to the draft, then publish.
   Preview releases are marked prerelease. The workflow does not automatically
   change GitHub's latest pointer; promote a stable release after release validation.

If publication fails, the draft is not update availability. Orphaned GHCR images
are not releases. Draft asset replacement is allowed on retry, but published asset
replacement is refused. A published release with missing metadata is unsupported.
Enable GitHub immutable releases through repository settings as an additional
protection; this PR does not change repository settings. Consumers must authenticate
and bind immutable identities regardless of that setting.

Release operator checks: public anonymous image access, asset downloads and exact
signature verification from a clean environment. Actual hosted OIDC signing, registry
push, multi-architecture image exercise and draft publication are release-only
checks; local unit tests do not establish them. No tag/release is created by this PR.

## References

- https://docs.sigstore.dev/quickstart/quickstart-ci/
- https://docs.sigstore.dev/cosign/signing/signing_with_blobs/
- https://docs.sigstore.dev/cosign/verifying/verify/
- https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases
