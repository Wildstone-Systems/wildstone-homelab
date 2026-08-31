# Wildstone concepts

The mental model, in the order the pieces depend on each other. Each
section cites the ADR that decided it.

## Desired state lives in the Statefile

The **Statefile** is a git-backed store of typed resources
(ADR-W004): `Volume`, `LdapGroup`, `OidcClient`, `Container`, `Route`,
`DnsRecord`, `BackupPlan`. Specs only — observed status never touches
it. Container images must be digest-pinned (`repo@sha256:...`); the
agent refuses anything else.

Every resource but `DnsRecord` names the node it is applied at, in
`metadata.node`, and that field is required: the reconciler groups
desired state by node and dispatches each group at one agent, so a
resource with no node is never dispatched anywhere. `DnsRecord` is the
exception because it is applied through an external provider rather than
an agent, and it must *not* carry a node.

For most resources the node is obvious — the machine running the
container. Two are less so: `OidcClient` and `LdapGroup` are applied at
the node running the **identity provider**, not the node running the app
that consumes them, because what they produce is configuration Authelia
and LLDAP read. App packages expose this as an optional `identity_node`
param that defaults to the package's own `node`, which is correct for a
single-node homelab; set it when identity runs on a different machine.

A `Container`'s published ports use Docker's own syntax and may name the
host address to bind:

```yaml
ports:
  - "8443:80"                      # every interface — Docker's default
  - "100.64.0.1:8443:80"           # this address only, e.g. the tailnet one
  - "[fd7a:115c:a1e0::1]:8443:80"  # IPv6 goes in brackets
```

The address must be a literal IP, not a hostname: a listener whose real
reach depends on what `/etc/hosts` resolves to at container-create time
is not a listener you can reason about. Both ingress profiles require
you to name the address for their private surface rather than defaulting
it, because a surface documented as private that silently binds a public
interface is the worst of both.

## Plans are the only write path

Every change to the Statefile is a **plan**: proposed (by a human, a
package install, or the Butler), reviewed, and **approved by a human**
before anything happens (ADR-W004 A3, ADR-W007). A plan lists its
steps with a risk grade (`Low`/`Medium`/`High`) and blast radius.
Approval commits the change; the reconciler then converges nodes to
it. There is no unmediated write anywhere — not for the UI, not for
the CLI, not for the AI.

## Routing is generated, never hand-written

A `Route` is desired state like anything else. The reconciler renders it
to a Caddy config fragment at `/etc/wildstone/routes/<name>.caddy` and
maintains the `Caddyfile` beside it that imports them all; both ingress
profiles mount that directory **read-only**, so those generated files
are Caddy's entire configuration.

Two consequences worth knowing:

Both ingress profiles run Caddy with `--watch`, so a Route added later
takes effect on its own: the reconciler writes the fragment and Caddy
re-reads it within about a second. No restart, and nothing for you to do.

- **Editing a Caddyfile by hand does not stick.** That covers the base
  `Caddyfile` and the routes placeholder as well as the fragments: they
  are not resources, so they get no plan and no approval step, but the
  reconciler re-asserts them on every tick and logs a `prereq.repaired`
  event when it has to. The base file carries the default-deny block, so
  "nobody noticed it was removed" is not an acceptable failure mode.
- **Editing a route fragment by hand does not stick either.** The reconciler
  compares the live bytes against a fresh render every tick and repairs
  the difference. Change the `Route`, not the file.
- **Apps are reached by container name.** Every managed container is
  attached to a user-defined Docker network (`wildstone`), which is what
  makes `container/<name>:<port>` upstreams resolve — Docker's default
  bridge has no DNS. The agent refuses to attach a container to a
  network it did not create.

A route declaring `auth: authelia_oidc` gets a real `forward_auth`
directive; the other values in the closed set (`app_oidc`, `app_ldap`,
`portal`, `none`) mean the proxy passes through by definition, because
the app authenticates itself or the surface is honestly public.

## The reconciler converges, and repairs drift

The reconcile service (ADR-W008) diffs desired against observed on
every tick (and immediately on approval/ingest — event-driven pokes).
Approved plans authorize their Medium/High steps; unattended drift
repair applies **Low-risk steps only**. Resources it did not create are
foreign: the drift classifier never deletes them (this is what makes
side-by-side migration safe — see [migration.md](migration.md)).

## Wardstone is a closed op surface

Nodes run **wardstoned**, which accepts a fixed set of signed
operations over mTLS (ADR-W003): ensure/stop/remove container, ensure
volume, write managed file (under a managed prefix only), health,
stats, logs, probes, and the write-only secret ops. There is no Exec.
A compromised Keystone still cannot run arbitrary commands on a node.

## Principals, roles, one API

Everything — Console, `wild`, Butler — consumes the ONE public
HTTP+JSON API (ADR-W007). Callers are **principals** with closed
roles: `admin` (everything), `operator` (read/propose/approve),
`butler` (read/propose — structurally cannot approve). Tokens are
opaque `wst_` bearers, stored hashed. Every request runs
authn → RBAC → rate limit → handler → audit, and every response body
passes a redacting encoder that strips secret-shaped values.

Redaction is deliberately blunt — over-redacting is the safe direction —
so a handful of shapes that look secret but are not carry explicit
shelters: `secret/<name>` references, `sha256:` digest pins, a Statefile
commit under a key that names one, and `Kind/name` resource references.
That last one is why the shelter list matters rather than being an
implementation detail: without it an ordinary `Volume/immich-postgres-data`
is a 27-character run of base64's alphabet, and a plan step reads
`Create [REDACTED]`. A plan a human cannot read is a plan they cannot
review, and review is the only thing standing between a proposal and
your data.

## Capabilities and packages

A **capability** ("photos", "media") is a set of resources owned by
`capability/<name>`, delivered as a signed package (ADR-W005,
ADR-W010, ADR-W011/W013). Installing or upgrading a package produces —
a plan, like everything else.

## Secrets are write-only

Secret values transit `wild secret set` once, travel to the node over
mTLS, and are stored encrypted in the node's local store (ADR-W012).
The protocol has no read-back operation; Keystone never persists a
value; specs reference `secret/<name>` and the agent materializes the
value only into a container's environment at create time. Rotation is
re-set + container re-create. Consequence: **your password manager is
the secret backup** — see [recovery.md](recovery.md).

## Backups run themselves, restores restore alongside

A `BackupPlan` (ADR-W011) names source volumes, a repository volume, a
`secret/...` repository password, and a schedule. The backup engine
runs pinned restic containers through the same closed op surface as
everything else, records every run in a ledger, and runs a periodic
restore **drill**. Restores go to a fresh `<plan>-restore` volume —
never over live data; moving data in place is a deliberate human act.

## The Butler proposes; humans approve

The Butler (ADR-W006, ADR-W017) is an LLM operator with read tools
(overview, status, health, diagnose, explain, plan preview) and
exactly one write tool: `propose_change`, which produces a plan in
state `proposed`. Its ceiling is structural, not conventional: the
butler role holds no approve verb, tool results are redacted,
bounded, and enveloped as untrusted data, and every conversation is a
persisted, principal-bound session (`butler` acting-for you). The
morning report is deterministic — no LLM involved — and keeps working
with the Butler off.
