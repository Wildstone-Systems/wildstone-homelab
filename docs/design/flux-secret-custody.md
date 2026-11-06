# Flux deployment-secret custody and recovery

Status: merged repository contract for #162 under amended ADR-W050. Child A
pins the synthetic oracle and versions; Child B implements the opt-in private
ciphertext projection and safe review boundary. Neither child creates an age
identity, decrypts a value, changes a cluster, migrates a release, or
authorizes live secret rotation.

## Scope

This contract covers Kubernetes Secret manifests used as deployment inputs for
Flux-managed applications. It also covers ordinary ConfigMap delivery where
the distinction matters. It does not replace:

- Household and Personal values encrypted in Keystone under ADR-W033;
- write-only Wardstone infrastructure and personal-integration credentials
  under ADR-W012/ADR-W030;
- certificate authority, DNS, Git transport, registry, backup-destination, or
  Kubernetes-at-rest encryption keys; or
- application-native user records and credentials stored in application data.

Those domains keep their existing ownership, reveal, rotation, and recovery
rules. Moving a value between domains requires a separate reviewed migration;
calling every credential a “SOPS secret” is not an acceptable shortcut.

## Selected upstream ownership

| Responsibility | Owner | Wildstone responsibility |
| --- | --- | --- |
| Encrypt/decrypt Kubernetes YAML | SOPS with age | Bind the reviewed encrypted document and report reconciliation without handling plaintext |
| Reconcile and decrypt | Flux kustomize-controller | Configure one exact Kustomization decryption reference through privileged bootstrap |
| Store encrypted deployment history | Installation-owned private Git | Promote only the exact Owner-approved ciphertext tree through the existing local publication journal |
| Store the live application value | Kubernetes Secret | Reference one exact namespace/name/key; retain K3s secrets-encryption and backup requirements |
| Consume the live value | Kubelet and the reviewed chart | Mount or inject only the named key; use a declared restart policy for rotation |
| Hold the age private identity | Owner/break-glass bootstrap | Install and recover it independently of Keystone, deployment Git, and the application namespace |
| Review and publish desired change | Keystone Owner Plan path | Bind ciphertext bytes, non-secret generation metadata, exact output, and expected Git parent without decrypting |
| Execute backup/restore | #163 selected executor | Capture encrypted Git and bootstrap keys as separate recovery inputs and prove restore |

No External Secrets Operator, cloud KMS, Vault, Sealed Secrets, custom
decrypter, or secret-returning Keystone API is selected by this slice. A later
provider may be evaluated only if it replaces a named responsibility and has a
separate custody/recovery decision.

The implementation follows the current Flux SOPS contract: a Flux
Kustomization uses `spec.decryption.provider: sops` and an optional
`spec.decryption.secretRef`; an age identity is provided in a Secret data key
whose name ends in `.agekey`. See the upstream
[Flux SOPS guide](https://fluxcd.io/flux/guides/mozilla-sops/) and
[Kustomization decryption API](https://fluxcd.io/flux/components/kustomize/kustomizations/#decryption).
Exact controller, SOPS, and age versions must be pinned and recorded by each
implementation proof rather than inferred from these links.

### Child A implementation-version pins

The repository-only fixture and the next private-projection child target these
exact upstream releases:

| Component | Exact version | Role in this child |
| --- | --- | --- |
| Flux distribution | [`v2.10.0`](https://github.com/fluxcd/flux2/releases/tag/v2.10.0) | Distribution whose controller set defines the reviewed API surface |
| Flux kustomize-controller | [`v1.10.1`](https://github.com/fluxcd/kustomize-controller/releases/tag/v1.10.1) | `spec.decryption.provider: sops` reconciler |
| SOPS CLI | [`v3.13.3`](https://github.com/getsops/sops/releases/tag/v3.13.3) | Local operator encryption/rewrap tool; Keystone does not execute it |
| age CLI | [`v1.3.2`](https://github.com/FiloSottile/age/releases/tag/v1.3.2) | Local operator identity/recipient tool; Keystone does not execute it |

Tags are version selection, not artifact-integrity evidence. Child C must also
record the exact installed controller image digest and the verified binary or
package identities used by its isolated proof. A newer release requires a
reviewed pin change and the same fixture/test suite; `latest`, ranges and
floating controller images are not supported inputs.

### Exact planned privileged inventory and safe review

Child B may add exactly one `secretDelivery` object to the existing private
bootstrap inventory:

```json
{
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

`generation` is positive and monotonic. `outputPath` is the fixed relative
private-tree filename `secret.sops.yaml`, never a caller-selected or absolute
path. The Kustomization and decryption-Secret fields bind the privileged outer
Flux object that ordinary publication cannot edit. The only supported restart
policy deterministically annotates the Pod template with the exact ciphertext
revision and retains the singleton `Recreate` behavior.

The Owner review may expose only these new safe fields:

- validation state `encrypted-reference-validated`;
- exact Secret namespace, name, key and type;
- declared Secret generation and ciphertext revision;
- SHA-256 fingerprints of normalized public age recipients, not recipient
  strings or private identities;
- relative encrypted output path;
- bound outer Kustomization namespace/name and decryption Secret name;
- restart policy and the expected singleton bounded-outage effect; and
- private-file count, without private host paths or contents.

It must not expose the encrypted payload, SOPS MAC or metadata, raw age
recipient, local input path, private repository path, private file bytes, or
age identity. The synthetic oracle lives in
`keystone/deployment/testdata/secret-custody/`; its encrypted strings and
recipient are deliberately non-decryptable placeholders and establish only
structural validation and redaction, not SOPS authenticity.

## Three deliberately separate inputs

### 1. Encrypted application document

Each managed Secret is an ordinary Kubernetes Secret YAML document encrypted
by SOPS. It lives in the installation's private generated deployment tree,
never in the public source repository. Its namespace, name, allowed key set,
Secret type, SOPS recipient metadata, encrypted payloads, and authenticated
SOPS MAC are part of the approved bytes.

The first Audiobookshelf integration remains limited to the exact existing
Secret name and one `jwt-secret` key. General arbitrary Secret YAML, extra
keys, cross-namespace output, `dataFrom`, arbitrary Helm `valuesFrom`, and
secret generators remain refused.

The encrypted document is added to the private path's `kustomization.yaml` so
kustomize-controller, not Helm and not Keystone, decrypts and applies it. The
document must never be applied directly with `kubectl`, rendered into a
ConfigMap, copied into chart values, or logged by publication/observation.

### 2. Flux decryption identity

The age private identity is a privileged bootstrap Secret in the same
namespace as the Flux Kustomization that references it. The bootstrap chooses
and binds its exact name; ordinary product input, generated application YAML,
agents, and API requests cannot choose or change it.

The identity Secret:

- contains only reviewed SOPS key material with data-key names ending in
  `.agekey`;
- is absent from public Git, private deployment Git, Helm values, Statefile,
  Keystone's runtime database, application namespaces, Pods, logs, and model
  context;
- is readable only by the Flux decryption path and cluster administrators;
- is not mounted into Keystone, the application, Helm controller, or the
  scoped apply ServiceAccount; and
- is installed, rotated, restored, or removed only through an explicit
  Owner/break-glass bootstrap action.

Ordinary application publication cannot edit the outer Flux Kustomization,
its ServiceAccount/RBAC, or this bootstrap Secret. That separation prevents a
reviewed application change from selecting another decryption key or widening
its apply authority.

### 3. Non-secret configuration

ConfigMaps contain only values safe to store as plaintext in the private
deployment repository and to expose through normal Kubernetes metadata/API
access. A setting is not safe merely because it is not a password: bearer
tokens, private endpoints where disclosure is material, client keys, recovery
material, and credential-bearing URLs belong in a Secret.

ConfigMap bytes receive their own exact content digest and review. Secret and
ConfigMap generation/version continuity are reported separately; one cannot
silently replace the other.

## Review and publication boundary

The current #257 path reads an encrypted file, performs a strict structural
check, and binds its SHA-256 digest, but deliberately does not publish or
decrypt it. #162 extends that path without exposing ciphertext through the
public API:

1. Privileged local inventory fixes the encrypted file path, target
   namespace/name/key, restart policy, outer Flux Kustomization identity, and
   decryption bootstrap-Secret name.
2. Keystone reads a mode-0600 regular file outside source checkouts, applies a
   closed schema, and computes a ciphertext revision over the exact bytes.
3. Review shows the safe target identity, explicit secret generation,
   ciphertext revision, recipient fingerprints, expected output path, restart
   effect, and complete non-secret YAML. It does not show ciphertext, SOPS MAC,
   private paths, private recipients where they identify a deployment, or any
   plaintext.
4. The projection digest binds both public-safe review files and a separate
   private encrypted-file set. API/Console response serializers may return
   only names and digests for the private set.
5. Approval re-reads and re-hashes the local encrypted file inside the existing
   final authority fence. Any byte, target, recipient, generation, inventory,
   or expected-parent change requires a new Plan.
6. Publication writes the exact approved encrypted bytes into the private
   candidate tree before the existing compare-and-swap promotion. No partial
   tree is served.
7. Flux decrypts, authenticates the SOPS MAC, applies through the scoped
   ServiceAccount, and reports failure without fallback or regeneration.

Keystone's structural validation is not SOPS MAC authentication. Repository
implementation must say `encrypted-reference-validated`, not `secret-verified`.
Flux reconciliation is the first authoritative decrypt/MAC result unless a
separate local operator preflight is later selected. Such a preflight must
discard plaintext, emit only bounded success/identity metadata, and remain
outside model-visible execution.

## Version and identity model

Four identifiers remain distinct:

| Identifier | Meaning | Not evidence of |
| --- | --- | --- |
| Secret generation | Monotonic, non-secret intended value generation in privileged inventory/document metadata | Exact bytes or successful apply |
| Ciphertext revision | SHA-256 of the exact SOPS document bytes | Plaintext equality; SOPS can rewrap unchanged plaintext |
| Deployment revision | Exact promoted Git commit/tree | Successful decryption or application readiness |
| Kubernetes resourceVersion | Observation of one live object update | Canonical generation or backup identity |

The Owner Plan binds all available identifiers. Observation reports them with
the Flux source revision, Kustomization generation, Secret existence/type/key
shape, workload generation, and application readiness. It never reads or
returns Secret data.

A missing live Secret may be recreated only from the same approved encrypted
document and recoverable age identity. That is reconciliation of an existing
generation, not generation of a replacement value. If the encrypted document,
age identity, required key, or expected generation is absent or invalid, the
state is blocked. No controller, chart, init container, leader, or startup path
may synthesize a new value for a recorded Secret.

## Rotation

Application-value rotation is an exact new Owner Plan:

1. Create a replacement SOPS document locally for the next declared Secret
   generation, using the bound age recipient set and the same exact target.
2. Validate its closed shape; review its new ciphertext revision, generation,
   output diff, and restart/outage effect without exposing ciphertext or
   plaintext.
3. Promote by expected-parent compare-and-swap and observe Flux decryption and
   Secret update.
4. Recreate/roll the workload according to the integration's declared policy.
   Environment-variable consumers require Pod replacement; a Secret update
   alone does not change an existing process environment. The first singleton
   profile uses `Recreate` and accepts a bounded outage.
5. Verify application usability with the new credential and refusal of the old
   credential where the application supports that distinction.
6. Retain the previous encrypted revision and application-consistent rollback
   material for the reviewed rollback window. A Git rollback is not a safe
   value rollback if the application has already persisted dependent state or
   new writes; #168 owns that migration question.

The initial projector should use a deterministic, non-secret Pod-template
annotation bound to the ciphertext revision so an approved rotation replaces
the Pod. It must not use a plaintext hash, Kubernetes resourceVersion, current
time, or random value.

## Age identity rotation

Age-key rotation is a privileged bootstrap operation, not an application Plan:

1. Generate the new identity outside Keystone and record its public recipient.
2. Back up and test recovery of the new private identity separately from the
   encrypted deployment repository.
3. Install a bootstrap Secret containing both old and new identities.
4. Rewrap every managed SOPS document to the reviewed recipient set and verify
   Flux can decrypt it. Rewrapping changes the ciphertext revision even when
   the application value is unchanged and must not imply value rotation.
5. Prove recovery using the new identity before removing the old recipient
   from every document.
6. Remove the old private identity only after inventory proves no managed
   document needs it and the rollback/recovery window has closed.

At every step, missing identity material blocks reconciliation. It never
triggers a fresh application value, a new age identity, or plaintext fallback.

## Backup and recovery

The recovery set has separate inputs that must not be stored as one
self-decrypting bundle:

- private deployment Git and promoted-delivery history, containing only SOPS
  ciphertext for this domain;
- the age private identity and bootstrap binding, kept in the independently
  protected key-recovery path;
- K3s secrets-encryption material required for restoring existing cluster
  state where applicable;
- application-consistent data/PVC backups; and
- exact chart/image/source identities and scoped Flux bootstrap/RBAC.

Recovery under #163 restores the read-only Git endpoint and Flux independently
of Keystone, installs the scoped decryption identity through privileged
bootstrap, and reconciles into a fresh target. Acceptance proves the same
declared Secret generation and application behavior without printing or
hashing plaintext into public evidence. Missing encrypted Git or missing keys
must fail before an empty/default application credential is initialized.

Deleting/replacing the age identity, encrypted document, live Secret,
Kustomization, or application release is destructive and separately gated.
Ordinary source rollback, Kustomization deletion policy, Helm uninstall, and
namespace cleanup must state their Secret/PVC consequences explicitly.

## Scoped authority and observation

- The public source repository contains contracts and synthetic shapes only.
- Private deployment Git contains reviewed ciphertext, never private keys.
- The source endpoint remains read-only and independent of Keystone.
- The outer Flux Kustomization and decryption Secret are privileged bootstrap
  objects outside ordinary generated application inventories.
- The Kustomization apply ServiceAccount is limited by RBAC to the selected
  namespace and supported kinds; it cannot modify cluster RBAC, admission,
  bootstrap keys, or other namespaces. Fixed inventory, closed projection, and
  exact review—not a claim that Kubernetes create RBAC filters arbitrary names—
  enforce the one supported application Secret name.
- Helm controller authority is limited to the reviewed release inventory. The
  chart references the existing Secret and does not generate/delete it.
- The workload ServiceAccount is tokenless. Kubelet supplies only the reviewed
  Secret key through the chart's declared destination.
- Observation reads metadata/status and verifies binding shape. It never reads
  Secret `data`/`stringData`, ciphertext, or the decryption Secret.
- Agents can propose product changes but cannot select paths/keys, approve,
  publish, install bootstrap material, decrypt, rotate, or reveal values.

## Bounded implementation children

### Child A — contract and fixtures

- Land this ownership/custody contract and synthetic redaction fixtures.
- Pin supported Flux, SOPS, and age versions for the implementation child.
- Specify the exact privileged inventory additions and safe review fields.
- No decryption, live key, cluster change, or private deployment data.

### Child B — private encrypted projection

Repository implementation status: implemented behind explicit
`secretDelivery` bootstrap inventory; live enablement remains unapproved.

- Split public-safe review files from private encrypted publication files.
- Include the exact SOPS Secret in the private candidate tree and Kustomize
  resources; bind both sets into one deterministic projection digest.
- Add the deterministic ciphertext-revision Pod annotation.
- Refuse plaintext, extra keys, arbitrary paths/targets, private-file API
  serialization, logs, and changed inputs at approval/publication fences.
- Do not add decryption or Kubernetes credentials to Keystone.

### Child C — scoped Flux bootstrap and isolated proof

- Select a fresh synthetic target and inspect the real Flux/Kustomization RBAC,
  parent prune behavior, decryption Secret ownership, storage, collisions, and
  recovery custody.
- Render and review the outer Flux Kustomization decryption binding, scoped
  apply authority, encrypted document, workload, rotation, rollback, and
  cleanup before an Owner gate.
- Prove ciphertext publication, Flux MAC/decryption, missing-key and tampered-
  ciphertext fail-closed behavior, deterministic Pod replacement, old/new
  credential behavior, redacted observation, and continued reconciliation
  while Keystone is unavailable.
- Retain evidence; fault, rollback, key removal, uninstall, and cleanup remain
  separately gated.

### Child D — recovery integration

- Under #163, capture encrypted Git and bootstrap identities as separate
  recovery inputs and restore to a fresh target.
- Prove missing/wrong key refusal, no regeneration, version continuity,
  independent Flux bootstrap, application usability, and revocation behavior.
- Do not claim household migration, adoption, or HA; #168 and #180 remain
  separate.

## Acceptance and stop conditions

Repository acceptance requires deterministic fixtures and tests proving that
plaintext/ciphertext/private paths cannot enter review/API/logs, the exact
private bytes and generation are approval-bound, changed inputs fail closed,
and only the supported Secret target/restart policy can be projected.

Live acceptance requires a separately approved isolated target and exact
inventory. Stop before mutation if key custody/recovery, source independence,
apply RBAC, parent pruning, target collision, artifact pinning, or Secret/PVC
retention cannot be established. Stop after any unexpected controller
inventory or plaintext exposure; preserve evidence and do not automatically
retry, rotate, delete, or clean up.

No result under #162 alone establishes off-host recovery, existing-release
adoption, household migration, new-write-preserving rollback, or HA.
