# ADR-W012: Secrets path — node-side store, materializer, rotation

- **Status:** accepted
- **Relates to:** ADR-W003 (closed op surface), ADR-W005 A2 (`secret/`
  references), ADR-W008 (level-triggered reconcile), implementation
  scope E8
- **Audience:** decision record AND implementation spec for E8.

## Conditional amendment from ADR-W030

This amendment becomes binding when ADR-W030 is accepted. ADR-W030's
member-scoped application credentials keep this write-only value
path, with one narrow change to A1: one connection credential may exist in the
encrypted Wardstone stores of its explicitly approved primary and fallback
nodes. It exists nowhere else at rest. Keystone stores member, connection,
purpose, eligible nodes, reference/version, and revocation metadata, never the
value. The authenticated setup request may fan the value directly over mTLS to
those nodes without persistence. A package-managed adapter receives it only
through normal container materialization. Chat, models, integration work
queues, events, memory, and audit never receive the value.

Adding an eligible node requires an approved placement Plan and successful
secret provisioning. Removing one immediately fences adapter work and uses the
typed, version-matched erasure operation permitted by the W003 amendment. When
the remote service supports per-client credentials, revoke that node's remote
credential too. Until local erasure and required remote revocation are proven,
the desired-state removal remains incomplete and visible as degraded. Immediate
connection revocation blocks all copies even before cleanup completes.

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
- `PutSecret{name, value}` — for operator-supplied values (a Cloudflare
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

### A5. Rotation is re-creation, driven level-triggered

Rotating `secret/<name>` is a Keystone-side plan:
`EnsureSecret{rotate: true}` plus Stop+Remove of every container whose
env references the secret. The next reconcile pass observes the
dependents absent and re-creates them, materializing the new value —
the level-triggered loop is the delivery mechanism, so rotation needs
no new agent behavior. A mere restart would NOT pick up the new value
(docker env is fixed at create), which is why the plan removes rather
than restarts; risk grades Medium (service interruption, no data).

# Part B — Work packages

- **W1 (agent):** the encrypted store; the three ops; env
  materialization in ensureContainer; protocol minor 7; audit-path
  check that PutSecret values never reach the agent audit log.
- **W2 (keystone):** admin-only API for secret set/generate/list per
  node (dispatch through the gateway, never persist values); rotation
  plan composition (A5); `wild secret` subcommands.
