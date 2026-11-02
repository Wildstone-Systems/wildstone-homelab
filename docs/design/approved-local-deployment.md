# Exact Owner Plans for local Helm publication

Status: opt-in repository implementation under #257, stacked on #265. Not authorization to enable it on the development VM. CI/review and supported OpenSSH transport evidence remain acceptance gates. #258 owns actual controller observation, runtime account/systemd lifecycle, pod reachability and the owner-operated application proof.

## One product input, ordinary output

Wildstone supplies the household review experience and a narrow tested mapping. Git stores local history, OpenSSH serves a read-only mirror, systemd runs delivery independently, and Flux/Helm reconcile the ordinary generated resources. Wildstone does not run a Git protocol server, execute Helm, apply workloads or proxy Flux reads.

The canonical Statefile gains `HelmDeployment`: the selected reviewed package/version, application timezone and a fingerprint of separately owned placement inventory. The resource has no arbitrary Helm values, filesystem paths, secret values, output YAML or runtime status. Generic resource proposals and out-of-band ingestion cannot change/delete it. The legacy Docker planner and reconcile loop exclude it and its deployment Plans. Existing resources and working Docker/custom paths remain supported.

For the initial integration, one installation-owned Audiobookshelf slot is configured through privileged bootstrap. No API request chooses repositories, chart sources, namespaces, claims, ServiceAccounts or credential references. This is not a generic catalog installer or release-adoption mechanism. Those extensions need their own bounded acceptance.

## Local bootstrap inventory

Keystone leaves this path disabled unless started with `-deployment-config /absolute/local/deployment.json`. The inventory and encrypted Secret file must be private regular files outside every source checkout. The canonical Statefile must also be a dedicated local repository, without remotes, source-code markers or shared Git objects. Product Git operations ignore inherited Git overrides/global configuration and hooks. The promotion and delivery repositories are separate and non-nested, with no external remotes.

Synthetic inventory example, to be created locally by a separately reviewed bootstrap:

```json
{
  "instance": "example-app",
  "profileFile": "/etc/wildstone-example/audiobookshelf-profile.yaml",
  "encryptedSecretFile": "/var/lib/wildstone-example/bootstrap/secret.sops.yaml",
  "repository": "/var/lib/wildstone-example/deployment.git",
  "mirrorRepository": "/var/lib/wildstone-example/delivery.git",
  "bindings": {
    "Name": "example-app",
    "Namespace": "example-app",
    "ServiceAccount": "example-reconciler",
    "ConfigClaim": "example-config",
    "MetadataClaim": "example-metadata",
    "MediaClaim": "example-media",
    "SecretName": "example-jwt"
  }
}
```

Bindings use the existing projector's field names. `SecretRevision` must be absent: the server derives it. The package profile is the validated #256 schema with immutable chart commit and image digest. Bootstrap must supply the supported software profile, existing isolated claims, scoped Flux reconciliation permissions and named Secret. Opening the manager initializes/checks only the private local promotion repository; it installs no software, account, key, listener, service or Kubernetes resource. The separate [delivery bootstrap](local-git-source.md) must use the same repository and mirror paths.

The encrypted file must identify the exact namespace/name and one SOPS-shaped encrypted `jwt-secret` payload with encrypted MAC. Its byte digest versions the **local encrypted configuration reference**, not a Kubernetes Secret resourceVersion. This implementation does not decrypt it, authenticate its SOPS MAC, deliver it, prove its live contents, provision it or rotate it. Bootstrap retains those responsibilities; #258 must verify the actual referenced Secret and app. Plaintext/extra payloads are refused, and neither ciphertext nor key material appears in review, generated Git output or public fixtures.

## Existing public API and Console

1. An active Owner with a fresh Console session calls `POST /api/v1/plans` with `{"deployment":{"instance":"example-app","timezone":"UTC"}}`. The installed-slot discovery endpoint is `GET /api/v1/deployments`; it returns supported choices, never local paths/keys.
2. The server snapshots current accepted input, inventory bytes, profile identity, encrypted configuration version, expected deployment parent and exact generated files. It retains the immutable product candidate and records one standalone high-risk Plan.
3. Existing Plan detail and Owner Review Queue show immutable chart commit/image, placement, versions, product candidate, output digest and exact YAML. The proposing Owner approves via existing `POST /api/v1/plans/{id}/approve` with a fresh session. Another Owner must prepare their own Plan. Agents and machine credentials cannot approve/apply; generic proposals cannot bypass this path.
4. Approval re-derives the projection, refuses stale inputs, and atomically accepts canonical product input plus the exact approval/current-Plan pointer in the existing publication transaction. It does not advance the deployment ref itself.
5. The dispatcher rechecks inputs and current authority, prepares/journals output and enters #264's final SQLite authority fence before bounded expected-old-ref Git promotion. Within that fence it repeats bounded file/version checks. A newer approval, even for unchanged product bytes, invalidates the older approval. Changed placement requires a separate adoption/retention decision.
6. Existing `POST /api/v1/plans/{id}/reject` also withdraws a pending deployment's future promotion authority. Any active Owner with fresh sign-in can revoke it. Revocation serializes with the ref effect; it neither rewinds a completed effect nor rewrites accepted product intent. Resubmission requires a new exact Plan.

The Plans screen adds these controls to its existing review flow and exposes local status. It is not a Flux dashboard or a second API. Direct owner use of standard Flux remains valid; conflicting edits to the managed deployment ref stop automatic publication and require separate review/adoption or relinquishment.

## Restart and truthful status

The manager enumerates approved/applying Plans and unacknowledged journals after startup and periodically. It re-observes actual refs before retrying. Prepared candidates resume only with current exact approval; missing/changed inputs, revoked Owner, superseding approval, household stop, restore quarantine, coordinator loss or conflicting refs prevent new effects. The current coordinator and execution gate also fence acknowledgment writes.

If Git already advanced and acknowledgment was lost, the exact recorded candidate/tree/binding can be acknowledged as historical fact even after Owner/Plan authority changes. This does not grant a new effect or roll back output. An owner ref conflict is preserved. Failed/unknown attempts store only safe diagnostic codes, not raw local errors or paths.

`GET /api/v1/plans/{id}` separates canonical product revision, prepared candidate, observed promoted ref and delivery mirror revision. Mirror availability is a local Git observation, not a network probe. `mirrored` means matching local object delivery; SSH/source-controller availability is not inferred. Flux reconciliation and application readiness are always `unobserved` in this slice. Plan state `applied` means this local promotion completed; Console labels it “locally promoted”. An old operation's current ref may later diverge, so consult its observed status rather than treating Plan history as live health.

## Evidence and deferred work

Synthetic real-Git integration tests cover the public Owner flow, exact review/redaction, generic/agent bypass refusal, stale and cross-instance input, Owner revocation/role change, superseding no-op approval, pending stop/quarantine/coordinator loss, ref conflict, restart before promotion and after ref advance, and independent mirror observation. The existing final-fence concurrency tests remain required. UI checks verify full review is loaded and incomplete output disables approval.

#257 remains open until repository CI/review and the supported-environment SSH retrieval/refusal test pass. A skipped SSH test is not transport evidence. #258 then proves controller revision/readiness on the existing development VM under its own schedule. No current release adoption, deletion/pruning, backup/Hetzner work, public exposure, reboot or path retirement is included.
