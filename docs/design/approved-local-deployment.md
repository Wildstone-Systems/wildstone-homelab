# Exact Owner Plans for local Helm publication

Status: #257 implementation and supported-environment transport evidence accepted. #258 adds the opt-in [read-only controller observer](flux-application-observation.md); live authenticated Owner/API and runtime failure/recovery acceptance remain separate. No release adoption or retirement is implied.

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
  },
  "secretDelivery": {
    "generation": 1,
    "outputPath": "secret.sops.yaml",
    "kustomizationNamespace": "example-flux",
    "kustomizationName": "example-app",
    "decryptionSecretName": "example-age-identity",
    "restartPolicy": "recreate-on-ciphertext-revision"
  }
}
```

Bindings use the existing projector's field names. `SecretRevision`,
`SecretFile`, `SecretGeneration` and `SecretRestartPolicy` must be absent: the
server derives them from the reviewed file and `secretDelivery`. Omitting
`secretDelivery` preserves the legacy pre-provisioned-Secret path. Enabling it
requires exactly the fields/values above; callers cannot select paths,
reconcilers, identities or restart behavior. Bootstrap must supply the
supported software profile, existing isolated claims, scoped outer Flux
Kustomization/RBAC and its independently held age identity. Opening the
manager initializes/checks only the private local promotion repository; it
installs no software, account, key, listener, service or Kubernetes resource.
The separate [delivery bootstrap](local-git-source.md) must use the same
repository and mirror paths.

The encrypted file must identify the exact namespace/name and one SOPS-shaped
encrypted `jwt-secret` payload with encrypted MAC. Its byte digest versions the
**local encrypted configuration reference**, not a Kubernetes Secret
resourceVersion. With `secretDelivery` enabled, the manager requires the
pinned SOPS/age shape, fingerprints public recipients, places the exact bytes
at `secret.sops.yaml` in private deployment Git and lists that resource in the
public-safe Kustomization. It also binds a deterministic ciphertext-revision
Pod annotation. It does not decrypt, authenticate the SOPS MAC, read/create
the age identity, prove live contents or itself apply/rotate a Kubernetes
Secret. Flux is the first authoritative decrypt/MAC boundary.

Plan JSON, API/Console responses and ordinary generated-file review contain
only the safe metadata defined by the [#162 custody contract](flux-secret-custody.md):
target, generation, ciphertext revision, recipient fingerprints, relative
output, outer binding and restart effect. Ciphertext, raw recipients, SOPS
metadata, private host/repository paths and keys are withheld. Missing or
changed files fail closed; generation cannot decrease or advance while reusing
the same ciphertext. Live decryption/bootstrap/rotation evidence remains a
separate Child C Owner gate.

## Existing public API and Console

1. An active Owner with a fresh Console session calls `POST /api/v1/plans` with `{"deployment":{"instance":"example-app","timezone":"UTC"}}`. The installed-slot discovery endpoint is `GET /api/v1/deployments`; it returns supported choices, never local paths/keys.
2. The server snapshots current accepted input, inventory bytes, profile identity, encrypted configuration version/generation, expected deployment parent, exact public generated files and private filename/digest. It retains the immutable product candidate and records one standalone high-risk Plan without private file bytes.
3. Existing Plan detail and Owner Review Queue show immutable chart commit/image, placement, versions, product candidate, output digest, safe Secret metadata and exact public YAML. The private ciphertext file is explicitly withheld. The proposing Owner approves via existing `POST /api/v1/plans/{id}/approve` with a fresh session. Another Owner must prepare their own Plan. Agents and machine credentials cannot approve/apply; generic proposals cannot bypass this path.
4. Approval re-derives the projection, refuses stale inputs, and atomically accepts canonical product input plus the exact approval/current-Plan pointer in the existing publication transaction. It does not advance the deployment ref itself.
5. The dispatcher rechecks inputs and current authority, prepares/journals output and enters #264's final SQLite authority fence before bounded expected-old-ref Git promotion. Within that fence it repeats bounded file/version checks. A newer approval, even for unchanged product bytes, invalidates the older approval. Changed placement requires a separate adoption/retention decision.
6. Existing `POST /api/v1/plans/{id}/reject` also withdraws a pending deployment's future promotion authority. Any active Owner with fresh sign-in can revoke it. Revocation serializes with the ref effect; it neither rewinds a completed effect nor rewrites accepted product intent. Resubmission requires a new exact Plan.

The Plans screen adds these controls to its existing review flow and exposes local status. It is not a Flux dashboard or a second API. Direct owner use of standard Flux remains valid; conflicting edits to the managed deployment ref stop automatic publication and require separate review/adoption or relinquishment.

## Restart and truthful status

The manager enumerates approved/applying Plans and unacknowledged journals after startup and periodically. It re-observes actual refs before retrying. Prepared candidates resume only with current exact approval; missing/changed inputs, revoked Owner, superseding approval, household stop, restore quarantine, coordinator loss or conflicting refs prevent new effects. The current coordinator and execution gate also fence acknowledgment writes.

If Git already advanced and acknowledgment was lost, the exact recorded candidate/tree/binding can be acknowledged as historical fact even after Owner/Plan authority changes. This does not grant a new effect or roll back output. An owner ref conflict is preserved. Failed/unknown attempts store only safe diagnostic codes, not raw local errors or paths.

`GET /api/v1/plans/{id}` separates canonical product revision, prepared candidate, observed promoted ref and delivery mirror revision. Mirror availability is a local Git observation, not a network probe. `mirrored` means matching local object delivery; SSH/source-controller availability is not inferred. Flux reconciliation and application readiness remain `unobserved` unless the separate opt-in [controller observer](flux-application-observation.md) is configured. Its exact revision, generation and health checks do not alter local publication status. Plan state `applied` means this local promotion completed; Console labels it “locally promoted”. An old operation's current ref may later diverge, so consult its observed status rather than treating Plan history as live health.

## Evidence and deferred work

Synthetic real-Git integration tests cover the public Owner flow, exact
review/redaction, separate private ciphertext publication, deterministic
restart annotation, generation continuity, missing/changed input refusal,
generic/agent bypass refusal, stale and cross-instance input, Owner
revocation/role change, superseding no-op approval, pending
stop/quarantine/coordinator loss, ref conflict, restart before promotion and
after ref advance, and independent mirror observation. The existing
final-fence concurrency tests remain required. UI checks verify full safe
review is loaded, ciphertext/private paths stay absent and incomplete output
or Secret metadata disables approval.

#257 is accepted with merged repository evidence and executed supported-environment SSH retrieval/refusal tests. #258 must repeat the flow through a real authenticated Owner session and verify the product observer, runtime failure/recovery and data persistence on the existing development VM. No current release adoption, deletion/pruning, backup/Hetzner work, public exposure, reboot or path retirement is included.
