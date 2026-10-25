# ADR-W012: Secrets path — node-side store, materializer, rotation

- **Status:** accepted; retained and scoped by ADR-W033; A1 amended by
  ADR-W045 A5 (sealed off-site secret bundle)
- **Relates to:** ADR-W003 (closed op surface), ADR-W005 A2 (`secret/`
  references), ADR-W008 (level-triggered reconcile), implementation
  scope E8
- **Governed by:** ADR-W033 where they overlap.
- **Audience:** decision record AND implementation spec for E8.

## Scope under ADR-W033

This store is the infrastructure/node secret store, retained by ADR-W033
§D1 and §G. Its write-only rules apply to infrastructure and integration
provisioning credentials. Household and Personal secrets are a separate
domain with their own ownership, use, reveal, and recovery rules (ADR-W033
§D1-D5; [`agent-system-design.md`](../agent-system-design.md) §12): Keystone
stores only authenticated ciphertext for them under a dedicated local key,
authorized Household reveal and Household-submission review use a
protected, non-cacheable UI response, and ordinary API responses, chat and
model tool output keep redaction. Models receive references, never values
(ADR-W033 §D2). No node-secret read operation is added. Personal values have
no reveal path.

## Constraints from ADR-W030

- Member-scoped application credentials for an integration connection (an
  app adapter such as CalDAV, not an ADR-W033 Connection) keep this
  write-only value path, with one narrow change to A1: one credential may
  exist in the encrypted Wardstone stores of its explicitly approved primary
  and fallback nodes. It exists nowhere else at rest. Keystone stores member,
  integration connection, purpose, eligible nodes, reference/version, and
  revocation metadata, never the value. The authenticated setup request may
  fan the value directly over mTLS to those nodes without persistence. A
  package-managed adapter receives it only through normal container
  materialization. Chat, models, integration work queues, events, memory,
  and audit never receive the value.
- Adding an eligible node requires an approved placement Plan and successful
  adapter deployment/readiness and secret provisioning. Removing one
  immediately fences adapter work, stops/removes the adapter container, and
  then uses the typed, version-matched erasure operation permitted by the
  ADR-W003 constraints. That operation refuses while any managed container
  still references the secret. When the remote service supports per-client
  credentials, revoke that node's remote credential too. Until container
  removal, local erasure, and required remote revocation are proven, the
  desired-state removal remains incomplete and visible as degraded.
  Immediate revocation blocks all copies even before cleanup completes.
- Revoking a Person applies that revocation and erasure workflow to every
  eligible node before any personal integration connection can be considered
  removed. A household-shared integration connection that used their
  credential stays fenced until an Owner supplies or reauthorizes a
  replacement (ADR-W033 §F1).

---

# Part A — Decisions (binding)

### A1. Values live on the node, names live in Keystone

A secret VALUE exists in exactly one place: Wardstone's encrypted store
on the node that uses it. Keystone stores and ships only the reference
(`secret/<name>`) — in the Statefile, in plans, in EnsureContainer env —
plus name-level metadata (which node, when rotated). The runtime DB, the
event stream, the audit log, and the API never carry a value; the E2
redaction layer stays defense-in-depth, not the mechanism. This is the
posture every shipped package already assumed ("only Wardstone
materializes it, E8").

### A2. Store: AES-256-GCM per value, key custody in DataDir

Each secret is a file `<DataDir>/secrets/<name>` holding
nonce‖ciphertext, sealed with AES-256-GCM under a 32-byte key at
`<DataDir>/secret.key` (0600, generated on first use) — the age
architecture (file key, modern AEAD) with zero new dependencies:
crypto/aes + cipher.NewGCM are stdlib, honoring wardstone's minimal
dependency surface (ADR-W003 §A4.5).

**Deliberate deviation from the scope doc:** E8's sketch said "key
custody in `/etc/wildstone`". That path is a managed prefix the ingress
packages bind-mount read-only into their Caddy containers, and it is
writable through WriteManagedFile — both disqualifying for key custody.
The key lives beside the node's TLS material in DataDir, which shares
its threat model exactly. Flagged here so the scope doc's sketch doesn't
read as the decision.

### A3. Op surface (protocol minor 7): write-only, generation-first

Three new arms, closed as ever. There is deliberately NO GetSecret: the
surface cannot read a value back, so neither a compromised Keystone nor
the Butler can exfiltrate one through the protocol.

- `EnsureSecret{name, length_bytes, rotate}` — generate-at-install:
  creates the secret with a random value (default 32 bytes, URL-safe
  base64) if absent; `rotate: true` regenerates even when present.
  Returns `{existed, version}` only. The value never crosses the wire.
- `PutSecret{name, value}` — for Owner-supplied values (a Cloudflare
  tunnel token). The value transits once, over the existing mTLS agent
  plane, and Keystone forwards without persisting. Returns `{version}`.
- `ListSecrets{}` — names, versions, created/rotated timestamps. Never
  values.

Secret names share the resource-name charset (path-segment safety —
they are file names in the store).

### A4. Materialization at container create, and only there

`ensureContainer` resolves any env VALUE matching `secret/<name>`
against the store at docker-create time; the plaintext goes into the
create body and nowhere else. A reference to an absent secret fails the
op with NOT_FOUND naming the reference (never a value). Existing
guarantees keep the value contained: InspectContainer never returns env
(minor 5), and the spec hash covers the REFERENCE, so materialization
does not perturb drift detection. Mount sources and other fields are
NOT materialized — env only, matching every shipped consumer.

#### Destination formats for managed-file materialization

Managed files may reference a node secret in three forms, resolved by
the agent at write time: `secret/<name>` (plaintext), `pbkdf2:secret/<name>`
(Authelia's `$pbkdf2-sha512$` digest) and `mosquitto:secret/<name>`, which
yields only the password field of a Mosquitto password-file entry
(`<user>:` comes from the template). The Mosquitto form is the `$7$`
scheme, PBKDF2-HMAC-SHA512 with 101 iterations (the `mosquitto_passwd`
default), a 12-byte random salt and a 64-byte key, both base64 with
standard padding. Salted output differs per write, so convergence
compares the pre-materialization source hash as for every materialized
file. Keystone, the reconciler and the resource validator all
recognise the same prefix set.

### A5. Rotation is re-creation, driven level-triggered

Rotating `secret/<name>` is a Keystone-side plan:
`EnsureSecret{rotate: true}` plus Stop+Remove of every container whose
env references the secret. The next reconcile pass observes the
dependents absent and re-creates them, materializing the new value —
the level-triggered loop is the delivery mechanism, so rotation needs
no new agent behavior. A mere restart would NOT pick up the new value
(docker env is fixed at create), which is why the plan removes rather
than restarts; risk grades Medium (service interruption, no data).

### A6. Amendment (epic #326 S02): a verified erase

The store gains a fourth, still write-only operation: `EraseSecret{name, max_version, nonce}` (protocol minor 22). It refuses a store holding a newer version than `max_version`, overwrites then unlinks the sealed file and its sidecar, re-checks that both are absent, and returns that fact with evidence bound to the nonce (`proto/secreterase`). There is still no operation that returns a value. Personal integration credentials (`pc.` names) are excluded from the recovery export. See ADR-W030 A5.4.

### A7. Amendment (W050, #180): custody on a cluster

Accepted 2026-10-25 in the [multi-node safety contract](../design/w050-multi-node-safety-contract.md) §4. It applies only to apps running on a Kubernetes cluster (ADR-W050). A Docker node keeps A1–A6 unchanged.

- **The canonical value is a Kubernetes Secret,** with k3s secrets-encryption at rest. One leader-changing cluster replaces one node's store, so A1's "exactly one place" becomes the cluster's Secret.
- **The leader's Wardstone generates it** and writes it straight into the Secret. Keystone still never sees a value and keeps only metadata: name, version, generated-or-provided, and the epoch that created it.
- **Never regenerate.** A secret Keystone records at version V that is missing or empty is an incident, never a reason to generate a new one. This keeps a new leader with an empty local store from replacing passwords.
- **Provided values** go from the authenticated request straight to the leader over mTLS and into the Secret, never persisted on the way (as in the ADR-W030 constraints above).
- **Recovery:** the sealed off-site bundle (ADR-W045 A5) also covers cluster Secrets, and the k3s encryption key is part of the off-host recovery kit. Without both, the secrets are lost, and the Console says so before migration.
- **Unchanged:** there is still no value-returning operation, and household and personal secrets (ADR-W033) stay ciphertext in Keystone, never in Kubernetes.

# Part B — Work packages

- **W1 (agent):** the encrypted store; the three ops; env
  materialization in ensureContainer; protocol minor 7; audit-path
  check that PutSecret values never reach the agent audit log.
- **W2 (keystone):** Owner-only API for secret set/generate/list per
  node (dispatch through the gateway, never persist values); rotating a
  node secret is a consequential operation requiring a fresh Owner session
  (ADR-W033 §A4.1); rotation plan composition (A5); `wild secret`
  subcommands.
