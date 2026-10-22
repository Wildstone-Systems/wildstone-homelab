# ADR-W005: Package format, capability engine, identity wiring (Spike S3)

- **Status:** accepted (locked decision #4 of proposal §10, produced by Spike S3); amended by ADR-W033
- **Relates to:** architecture proposal §5, implementation scope S3, ADR-W004
- **Governed by:** ADR-W033 where they overlap.
- **Audience:** decision record AND implementation spec for Spike S3, written so
  implementation subagents execute Part C without the planning conversation.
  Part A/B binding; contradictions → stop and report.

## Constraints from ADR-W030

- `agentTools` remains a declaration, never an executor or grant. Its
  validated metadata includes purpose, input schema, data/action scope,
  mutating behavior, approval requirements, the tool's authority class
  (`member` or `owner`), and whether it is `opt-in`. An Owner-class tool can be
  granted only to Owner Agents, and an opt-in tool is excluded from broad
  default and bundle grants (ADR-W033 §B3-B5).
- The manifest shown to a model is generated from the active gateway and
  resource-kind registry, narrowed by effective authority; tests pin both
  advertised coverage and exclusions so descriptions cannot drift from
  executable capability.
- `publishes` carries a validated audience policy. Declared household events
  default to household-shared unless the package declares a narrower
  audience; the resolved policy and provenance are stored at intake.
  Migration explicitly maps existing registered package events and trusted
  Keystone-owned automation and household-answer event paths. Unknown or
  invalid event types fail closed, and a caller-supplied internal source name
  never establishes trusted origin.

## Why S3 is the go/no-go gate

S3 exists to answer one question with code: **can a package template the
identity+ingress integration that, in the reference deployment, took
hand-written Authelia config + a fix commit to get right — with no per-app
hand-tuning?** If package-generated OIDC wiring resists templating, the
product thesis needs revisiting *before* MVP. So S3 builds the two packages
(Identity, Photos/Immich) and proves, in CI, that expanding them yields the
exact resource set + Authelia OIDC-client config shape of the scrubbed golden
fixture (`testing/fixtures/immich/authelia-oidc-client.yml`), applied entirely
through S1+S2 machinery. The live "browser logs into Immich via Authelia" run
is the VM-harness / owner path; the CI-runnable equivalent is the gate.

---

# Part A — Decisions (binding)

### A1. Package format v1 (locked decision #4)

A **package** is versioned data (not code) defining one profile of a
capability. YAML:

```yaml
apiVersion: wildstone.dev/v1
kind: Package
metadata:
  name: photos-immich
  version: 1.0.0
  capability: photos          # the user-facing unit
  profile: immich             # the concrete implementation
requires:                     # traits, not app names (ADR proposal §5)
  - { trait: identity, mode: oidc }
  - { trait: ingress, mode: public }
  - { trait: storage, class: bulk }
params:                       # declared inputs; required or defaulted
  - { name: public_domain, required: true }
  - { name: photos_subdomain, default: photos }
policies:
  classifications: [family_public, family_private]
  dataSensitivity: high
templates:                    # parameterized resource.Resource docs (Go text/template
  - |                         # over .Params and .Resolved); expand to the S2/S3 kinds
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: immich-server, owner: capability/photos, node: "{{ .Params.node }}" }
    spec: { image: "ghcr.io/immich-app/immich-server@sha256:...", ... }
lifecycle:                    # hooks run AS CONTAINERS with declared mounts — never host
  preInstall: []              # scripts. Parsed but REJECTED if non-empty (#193): no executor yet; use Jobs/SeedFiles.
  postInstall: []
agentTools:                   # ADR-W001 concierge block. Spike: parsed + validated only.
  - { name: search_photos, risk: domestic, ... }
```

Rules:
- **Packages are data.** Templates are `text/template` over params + resolved
  dependency outputs. No embedded scripts. agentTools are
  **parsed and validated at MVP, executed in a later phase** — S3 must reject
  a malformed agentTools block but must not execute it. Lifecycle hooks have no
  executor: every non-empty `preInstall`/`postInstall` list is rejected (#193)
  until one exists; use Jobs/SeedFiles.
- Packages are the catalog content; they live in `packages/<name>/package.yaml`
  in this repo (the curated registry; signing is D2's concern, not S3's).
- Every expanded resource carries `owner: capability/<capability>` — the
  ownership chain (ADR-W004 A1). Nothing a package produces is orphaned.

### A2. Capability engine

`Engine.Expand(pkg, params) ([]resource.Resource, error)`:
1. Validate params against declarations (required present; defaults filled).
2. Resolve `requires` traits against the enabled catalog: `identity:oidc` is
   satisfied by the Identity package (which provides the `oidc` trait);
   `ingress:public` by the reverse-proxy/Route mechanism; `storage` is a
   declared Volume. Unsatisfied required trait → typed error (this is the
   dependency check the reconciler relies on).
3. Render each template, parse to `resource.Resource`, validate via the S2a
   registry. Dependency outputs (e.g. the Authelia issuer URL, the OIDC
   client secret *reference*) are injected as `.Resolved`.
- **Dedicated-per-capability infra** (MVP posture, proposal §5.1): Photos
  gets its own postgres+redis Containers, not a shared instance — independent
  failure/backup/upgrade domains.
- **Secrets by reference only.** A package never contains a secret value; it
  references `secret/<name>`; only Wardstone resolves it at materialization
  (out of S3 scope — S3 carries the reference through).

### A3. Two new resource kinds for identity wiring

S2 shipped Container/Volume/Route/DnsRecord. S3 adds:
- **OidcClient** — `spec: { clientId, clientName, redirectUris[], scopes[],
  authPolicy, secretRef }`. Applied by the **identity capability's
  controller**: render the client into an Authelia `identity_providers.oidc.
  clients` fragment and WriteManagedFile it to a managed Authelia config path,
  then a **batched** Authelia reload (the trap: Authelia restarts on OIDC
  client changes, proposal + handoff — so multiple OidcClient changes in one
  plan reload Authelia **once**, after all are written). This is the exact
  wiring that was hand-authored in the reference repo; templating it is the
  thesis test.
- **LdapGroup** — `spec: { name, members? }`. Per the handoff trap ("per-app
  identity attribution will be messy — packages declare best-achievable"),
  S3 models LdapGroup as **declared + best-effort**: it expands and is
  recorded/validated, and its apply is a documented no-op-with-log seam
  (real LLDAP provisioning is a later provider). Do NOT build an LLDAP API
  client in the spike.

### A4. What S3 proves (the gate assertion)

Expanding `identity` + `photos-immich` with placeholder params, writing the
result to a Statefile, and reconciling against a fake Docker + fake managed
filesystem yields: Immich's containers (server/ml/redis/postgres) + upload
Volume + public Route, AND an Authelia OIDC-client config fragment whose
shape matches `testing/fixtures/immich/authelia-oidc-client.yml` (scrubbed
placeholders) — with **no hand-editing between package and applied state**.
The Authelia reload fires exactly once for the batch. That equivalence is
the go/no-go signal.

---

# Part B — Codebase contract

### B1. Layout (under `keystone/` + `packages/`)

| Path | Contents |
|---|---|
| `keystone/resource` | extend the closed registry with OidcClient + LdapGroup kinds (specs + validation). |
| `keystone/reconcile` | OidcClient applier (Authelia fragment render + WriteManagedFile + batched reload seam); LdapGroup declared/best-effort applier; observe/health-gate for both. |
| `keystone/capability` | the Engine: Expand(pkg, params), trait resolution, template rendering, `.Resolved` injection. |
| `keystone/pkgspec` (or `keystone/capability/pkg`) | Package struct + YAML load + validation (params, lifecycle-hooks-rejected, agentTools-parsed-not-run). |
| `packages/identity/package.yaml`, `packages/photos-immich/package.yaml` | the two hand-written packages. |

Reuse S2's reconcile/apply path and the S1/E1 real-gateway+fake-docker test
style. The Authelia fragment path is a managed prefix (e.g.
`<managed>/authelia/oidc-clients.yaml`).

### B2. Build/verify contract (unchanged from ADR-W004 B2)

```sh
export GOTOOLCHAIN=go1.25.0
gofmt -l ./proto ./wardstone ./keystone ./cli ./testing/e2e   # empty
for m in proto wardstone keystone cli testing/e2e; do (cd "$m" && go vet ./... && go build ./... && go test ./...); done
(cd keystone && go test -race -count=1 ./...)
```

No proto changes (identity wiring rides existing ops: WriteManagedFile +
container ops). No new deps beyond yaml. gitleaks stays green — packages use
placeholder domains/secrets-by-reference only, never real values.

### B3. Traps (carry ADR-W004 B3) + S3-specific

- Batched reload: render+write ALL OidcClient fragments for a plan, THEN one
  reload — not one reload per client (Authelia restart storm).
- Packages must scrub: `example.com`, `secret/immich-oidc` references,
  digest-pinned images (real digests are fine; they're public), no real
  domains/IPs/secrets — CI runs gitleaks.
- Image digests in packages must satisfy the ADR-W003 digest regex.

---

# Part C — Work packages (ordered)

Rules of engagement as in prior ADRs (one package per change, tests per B2,
report decisions/deviations, stop-and-report on proto/dep changes).

### S3a — OidcClient + LdapGroup kinds and appliers

- `keystone/resource`: add the two kinds to the registry with specs +
  validation (OidcClient needs clientId + at least one redirectUri;
  LdapGroup needs a name).
- `keystone/reconcile`: OidcClient applier — render an Authelia clients
  fragment (deterministic YAML matching the fixture shape) and WriteManagedFile
  to the managed Authelia path; a `reloadAutheliaFn` seam; batching so N
  OidcClient steps in one plan reload once (apply all writes, then reload
  once at end of plan if any OidcClient changed). Observe via ReadManagedFile
  byte-compare; health-gate = fragment present. LdapGroup applier =
  declared/best-effort (log + record), observe = declared-state.
- Tests (real gateway + fake docker + managed FS): apply two OidcClients in
  one plan → both fragments written, reload seam called exactly once;
  re-reconcile → no-op, no reload; LdapGroup declared without error.

### S3b — Package format + capability engine

- `keystone/pkgspec`: Package struct, YAML load, validation (params required/
  defaulted; non-empty lifecycle hooks rejected (#193); agentTools parsed and
  schema-checked but NOT executed; reject malformed).
- `keystone/capability`: `Engine.Expand(pkg, params) ([]resource.Resource,
  error)` — param validation, trait resolution against a provided catalog,
  `text/template` rendering, parse+validate each resource, inject `.Resolved`
  (issuer URL, secret refs) from resolved deps. Unsatisfied required trait →
  typed error.
- Tests: a small inline package expands correctly; missing required param →
  error; unsatisfied `requires` trait → error; non-empty lifecycle hooks
  → validation error; agentTools malformed → validation error but never executed; owner chain stamped on every output.

### S3c — The two packages + the go/no-go end-to-end test

- `packages/identity/package.yaml`: LLDAP (container+volume), Authelia
  (container+volume+route), provides trait `identity:oidc`; its expansion
  includes the OidcClient-registration mechanism (the identity capability
  owns applying OidcClients).
- `packages/photos-immich/package.yaml`: Immich server/ml + dedicated
  postgres+redis containers, upload volume, public Route, and an OidcClient
  resource requesting registration (redirect URIs to `photos.example.com`,
  scopes openid/profile/email/groups), secret-by-reference.
- The gate test (`keystone/capability` or a new `keystone/vertical` test):
  Expand(identity)+Expand(photos-immich) with placeholder params → write to a
  temp Statefile → reconcile through the real gateway + fake docker + managed
  FS → assert: Immich containers+volume+route exist; the Authelia OIDC-client
  fragment written for `immich` matches the scrubbed golden fixture shape
  (`testing/fixtures/immich/authelia-oidc-client.yml`); Authelia reloaded
  exactly once; zero hand-edits between package and applied state. Log the
  result as the **go/no-go verdict**.
- README note: the live VM demo (real Immich behind Authelia, browser OIDC
  login) is the owner-run testing/e2e path; this test is its CI-runnable
  equivalent.

### S3 exit / gate

If S3c's equivalence holds, the thesis is validated and MVP proceeds. If the
package-generated identity wiring needed per-app hand-tuning to match the
fixture, that is the go/no-go failure the spike exists to catch — report it
prominently rather than papering over it. Produces locked decision #4.
