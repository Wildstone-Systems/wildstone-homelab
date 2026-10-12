# External Agent Identity, Route, and OAuth Contract

Status: **proposed for review**, not accepted and not implemented. This is the
bounded contract deliverable for [#541](https://github.com/chickenrat17/wildstone_homelab-control-plane/issues/541)
under milestone tracker [#580](https://github.com/chickenrat17/wildstone_homelab-control-plane/issues/580).
It is grounded in `main` at `461272024aeae3b155fb67a25e84999da708d067`.

This proposal does not amend an accepted ADR by itself. ADR-W033, ADR-W039,
ADR-W040, and ADR-W041 continue to govern until a reviewer accepts the choices
below and any necessary ADR amendment is merged. In particular, this document
does not authorize a migration, OAuth deployment, runtime-route change, or live
household test.

## 1. Boundary and non-goals

The first external-client boundary has one purpose: let an independently
managed runtime act for one explicitly bound active Person through one Agent,
without making the runtime, OAuth client, or model a new authority source.

Keystone remains the authority for Person and Agent identity, admissions,
capability discovery, authorization, credentials, work, delivery, and audit.
The REST Agent API remains canonical. MCP is a presentation of the same
`agentapi` and `agentgateway` operations. Wardstone remains the typed host
executor. The native Butler remains a separately configured Agent and is never
an automatic substitute for the external Agent.

This contract deliberately excludes:

- MCP resources, prompts, sampling, specialist delegation, and unsolicited
  `tools/list_changed` notifications;
- generic event matching or dispatch, which still needs its own atomic
  audience/deduplication/recovery contract;
- a second permission engine, registry, scheduler, event broker, or approval
  path;
- runtime schema migrations and implementation work;
- automatic fallback, processing-class downgrade, or transfer of private work;
- dynamic OAuth client registration for the first compatibility target.

## 2. Source-backed current state

### 2.1 What already exists

`runtime_principals` gives every runtime credential an immutable binding to one
Agent. `BindRuntimePrincipal` requires a `butler`-role principal, refuses a
principal already bound to another Agent, and makes the bound principal the
Agent's current runtime credential. Every `/api/v1/agent/*` handler calls
`agentContext`, which resolves that binding before the shared gateway sees the
request. The gateway then resolves the task handle and rechecks the task's
Person, Agent, Connection, audience, grants, processing class, budgets, and
fences.

The current runtime route family is registered together in
`keystone/api/api.go`: manifest, idle tool discovery, invoke, recall, remember,
checkpoint, start/wait/complete/fail, propose, notify, heartbeat, delivery poll,
and delivery acknowledgment. This is the reusable public contract; ordinary
Keystone resource, Plan, approval, secret, event, and management routes are not
part of it.

The current `agent-bridge` translates MCP `tools/list` to `Manifest`/`Tools`
and `tools/call` to `Invoke`, using the same HTTP Agent API client. It declares
MCP revision `2025-06-18`, but authenticates Hermes with a shared secret. It
does not expose OAuth protected-resource metadata, validate an OAuth access
token, validate `Origin`, or enforce the negotiated `MCP-Protocol-Version`
header. Its task correlation accepts an explicit `X-Wildstone-Handle`, then an
`X-Hermes-Run-Id` lookup, then `solePending()` when exactly one run is active.
The package fixes `max_concurrent_runs: 1`, but a sole pending run is not proof
that an otherwise unbound caller owns that task.

### 2.2 Compatibility evidence and limits

The repository packages Hermes release `v2026.9.21` by its OCI index digest
(`packages/agent-hermes`, package version `0.1.7`). The repository-pinned MCP
revision is `2025-06-18`. The identity package pins an Authelia image by digest,
but the repository does not record the corresponding Authelia release number.
The old test fixture names Authelia `4.38.16`; accepted Console design and code
exercise 4.39-era behavior. Neither is evidence of the version installed on a
live target.

No live deployment inventory, OAuth callback, refresh/revocation trace, or
packaged Hermes conformance result exists in this repository. Current upstream
Hermes documentation describes OAuth metadata discovery, Authorization Code
with PKCE, pre-registered clients, token refresh, and headless login, but that
documentation has moved beyond the pinned `v2026.9.21` image. It is a proposed
compatibility target, not proof that the pinned image implements it. H10/#356
remains the live conformance gate.

MCP revision `2025-06-18` requires HTTP protected-resource metadata, OAuth
authorization-server metadata discovery, the `resource` parameter, an access
token on every HTTP request, audience validation, and PKCE. Streamable HTTP
also requires `Origin` validation and protocol-version handling. A server may
return 405 for an unused GET stream, so server-initiated SSE is not required by
this first tools-only profile.

## 3. Identity model

The following identities are distinct. No display name, username, OAuth claim,
or creator field substitutes for another row.

| Identity | Stable key | Created or admitted by | Supplies | Never supplies |
|---|---|---|---|---|
| Person | Keystone Person ID | Existing Wildstone enrolment | Interactive subject, privacy owner, admissions, disclosure audience | Agent identity, runtime credential, approval merely because they authenticated |
| Agent | Keystone Agent/Butler ID | Existing Owner administration | Tool assignments, controls, memory class, runtime selection | Person identity or a transferable bearer |
| External client registration | New opaque Wildstone registration ID plus exact `(issuer, client_id)` | Owner registers the client and the bound Person consents; both facts are durable | The allowed Person, Agent, Connection, and runtime-principal binding for this client | Grants, tool visibility, task authority, or ownership inferred from a name |
| Runtime principal | Existing Keystone principal name and immutable `runtime_principals` Agent binding | Consequential-fresh Owner action | Internal audit actor and the existing Agent API authentication context | A Person, approval authority, or access to general Keystone routes |
| OAuth access token | Issuer token identifier plus validated claims | Authelia after the Person completes Code + PKCE | Proof for one request that the issuer authenticated the subject for the registered client and resource | A Wildstone task, Agent assignment, Connection, audience, or transferable upstream credential |
| Task/session | Wildstone task ID and secret handle | Existing delivery path or the bounded admission operation below | Exact Person/Agent/Connection/audience/budget and derived authority for one unit of work | A durable grant, a different session, or authority after revocation |

The proposed external-client registration is immutable in its identity-bearing
fields:

```text
ExternalClientRegistration
  id
  issuer
  oauth_client_id
  person_id
  agent_id
  connection_id
  runtime_principal
  state: active | disabled | revoked
  credential_epoch
  created_at / disabled_at / revoked_at
```

Changing the issuer, OAuth client ID, Person, Agent, Connection, or runtime
principal creates a new registration and revokes the old one. Rotation changes
the credential epoch; it never rebinds an existing registration. An Owner may
disable or revoke a registration but cannot use that administrative power to
read the bound Person's private content. Offboarding the Person, disabling the
Agent or Connection, revoking the runtime principal, or entering restore
quarantine makes the registration unusable at the next boundary.

### 3.1 Token-to-Wildstone resolution

For every protected request, the resource server must:

1. introspect the access token with the configured Authelia issuer;
2. require `active`, the exact issuer, the canonical MCP resource audience,
   the registered `client_id`, and scope `wildstone.agent`;
3. resolve the stable subject to an already admitted Person through the
   existing authenticated identity binding, never through a name or email;
4. load the exact active external-client registration for
   `(issuer, client_id, Person ID)`;
5. recheck its Agent, Connection, runtime principal, processing class, and
   current lifecycle state;
6. construct the existing internal runtime-principal context, without passing
   the Authelia token to Keystone, Wardstone, a tool adapter, or an upstream
   service.

An ID token is not an access token and is never accepted at the Agent API or
MCP resource. Claims supplied in a request body are ignored for identity.

## 4. Task admission and exact correlation

Delivery-initiated work already carries a Wildstone-issued handle. Independently
initiated client work needs one new, model-invisible admission operation. The
proposed canonical REST shape is:

```text
POST /api/v1/agent/tasks
Authorization: Bearer <resource access token>

{
  "clientSessionId": "opaque runtime-local session identifier",
  "idempotencyKey": "opaque retry key",
  "objective": "bounded user request"
}

201 {
  "taskId": "...",
  "handle": "...",
  "expiresAt": "..."
}
```

The request cannot name a Person, Agent, Connection, audience, role, processing
class, or capability set. Those are derived from the validated registration
and current Wildstone records. Admission requires the Person, registration,
Agent, Connection, and runtime principal all to be active; checks restore
quarantine and current privacy/processing policy; bounds the objective and body;
and creates a normal task and ledger. `(registration ID, clientSessionId,
idempotencyKey)` is unique so a retry returns the same task rather than creating
parallel authority.

The returned handle must accompany every task-scoped REST operation. For MCP,
the client must bind each `tools/call` to that exact handle through one of these
reviewed mechanisms:

1. an explicit per-request task-grant header supported and verified by the
   pinned client; or
2. a task-bound MCP session whose unguessable session ID is issued only after
   task admission and cannot be reused for another task.

`solePending()` is not an accepted ownership proof and must not be available in
the OAuth profile. An `X-Hermes-Run-Id` is acceptable only after the pinned
Hermes release is shown to put that exact value on every outbound MCP request
and the bridge has an atomic, one-to-one, expiry-bounded run-to-handle mapping.
If the pinned client supports none of these mechanisms, independently initiated
Hermes tasks are excluded from the first PoC. Delivery-initiated work may still
proceed because the bridge receives its handle from Wildstone before starting
the run, but live conformance must still prove that no other session can borrow
it.

## 5. Route contract

The server boundary uses a closed method-and-path classifier. A runtime-bound
identity is denied before body decoding on every unclassified route. Adding a
new Keystone route does not make it reachable until this classifier and its
negative tests are deliberately updated.

| Family | Allowed methods and paths | Task required | Notes |
|---|---|---:|---|
| MCP tools | `POST /mcp`; `GET /mcp` may return 405 | Calls yes; idle list no | Tools only; shared gateway and registry; OAuth metadata is served separately |
| Discovery | `GET /api/v1/agent/tools`, `POST /api/v1/agent/manifest` | Manifest yes; tools no | Idle discovery is filtered to the bound Agent; execution still rechecks |
| Task admission | `POST /api/v1/agent/tasks` | Creates one | OAuth profile only; no caller-selected identity or audience |
| Capability and memory | `POST /api/v1/agent/invoke`, `/recall`, `/remember` | Yes | Same gateway, disclosure, processing-class, budget, and audit checks |
| Work lifecycle | `POST /api/v1/agent/checkpoint`, `/start`, `/wait`, `/complete`, `/fail` | Yes | Attempt and coordinator fences remain mandatory |
| Proposal and notification | `POST /api/v1/agent/propose`, `/notify` | Yes | May propose or report only; never approve or apply |
| Runtime health | `POST /api/v1/agent/heartbeat` | No | Applies only to the registration's bound runtime principal |
| Delivery | `GET /api/v1/agent/deliveries`, `POST /api/v1/agent/deliveries/{id}/ack` | Delivery supplies task | Final authority recheck is part of #582 |

Explicitly denied examples include `/api/v1/plans`, approval/apply routes,
ordinary Person/Agent/Connection management, raw events, secrets, general
resource reads, runtime credential issuance, and every unknown route. A broad
`read`, `propose`, Owner, or administrator role does not override this route
classification. `/api/v1/agent/propose` is allowed only because it is the
existing task-bound Agent API operation and still files a human-reviewed Plan;
it is not permission to call general Plan endpoints.

REST, native, and MCP continue to call the same `agentgateway.Gateway`. OAuth
scope is only coarse resource admission and never replaces Agent assignments,
task-derived authority, per-resource grants, disclosure policy, or Plan rules.
Existing separately provisioned runtime-principal credentials remain available
only to their explicitly configured native, packaged, or recovery paths. They
are not accepted as a fallback after an OAuth failure and do not acquire access
to routes outside this table.

## 6. OAuth and MCP compatibility target

The proposed first target is intentionally narrow:

| Item | Proposed target |
|---|---|
| MCP revision | `2025-06-18`, tools-only Streamable HTTP request/response |
| Authorization server | The deployment's Authelia issuer |
| Client | Pre-registered public Hermes client, one registration per bound Person/Agent/Connection |
| Grant | Authorization Code with PKCE S256; exact registered loopback or HTTPS callback |
| Metadata | RFC 9728 protected-resource metadata and RFC 8414 authorization-server metadata |
| Resource | Exact canonical MCP URL, sent as `resource` in authorization and token requests |
| Resource scope | `wildstone.agent`; no approval, admin, secret, event, or host scope |
| OIDC scopes | `openid` for stable subject binding; `offline_access` only when refresh is enabled and consented |
| Token validation | Authelia introspection on every protected request; require active token, issuer, subject, client, resource audience, and scope |
| Refresh | Refresh token remains with Hermes; rotated/revoked according to Authelia; every refreshed access token is revalidated and re-resolved |
| Issuer outage | Fail closed for new and in-flight boundary checks; no cached grant and no automatic static-token fallback |
| Downstream token | A separate internal runtime-principal context; never token passthrough |

Pre-registration is compatible with MCP's allowance for authorization servers
that do not offer dynamic registration and with Hermes's current documented
pre-registered-client configuration. It also keeps enrolment an explicit
Wildstone action. The callback URI and client ID are deployment configuration,
not source fixtures.

Opaque-token introspection is selected for the first target because immediate
client/Person revocation and issuer-checked audience are required. Authelia's
documentation notes that token audience cannot generally be determined without
introspection unless a standardized JWT access-token profile is deliberately
configured, and it discourages stateless JWT introspection because it assumes
the token has not been revoked. Offline JWT validation is therefore a later,
explicit availability-versus-revocation decision, not an automatic outage
fallback.

The MCP resource must also validate `Origin`, require the negotiated
`MCP-Protocol-Version` on subsequent requests, return an RFC 9728
`WWW-Authenticate` challenge on 401, reject tokens in URLs, cap bodies, and
return 403 for insufficient scope without disclosing protected data.

## 7. Interfaces consumed by #581 and #582

The reviewed implementation should expose concepts equivalent to these without
creating a second authorization engine:

```text
ResolveExternalRuntime(accessToken, method, canonicalResource)
  -> {registrationID, personID, agentID, connectionID,
      runtimePrincipal, clientID, credentialEpoch}

ClassifyRuntimeRoute(method, path)
  -> {family, taskRequired} | deny

RecheckRuntimeAuthority(identity, operation, taskOrDeliveryID)
  -> allow | deny
```

#581 owns the closed route classifier and integration with the existing
authentication wrapper. Resolution must occur before a request body is decoded,
then the existing gateway remains responsible for task and resource authority.
No OAuth-specific branch may dispatch a tool directly.

#582 owns the final recheck immediately before a delivery payload is returned
and inside the acknowledgment transaction. That recheck includes token/client
epoch, registration, Person admission, Agent, Connection, runtime principal,
restore quarantine, task audience/privacy, and processing class. A revoked poll
wakes or times out without content. A denied/replayed/cross-Agent acknowledgment
changes no row. Disabling a registration may preserve queued work; it must not
silently hand that work to the native fallback.

## 8. Testable acceptance for later implementation

The proposal is implementable only after review accepts the identity and outage
choices above. Focused implementation must then prove with synthetic Persons,
Agents, clients, Connections, tasks, and a fake issuer:

1. A token for Person A/client A cannot discover, invoke, poll, or acknowledge
   Person B, Agent B, another Connection, or another task.
2. A valid token on every general Keystone route is denied before body decoding;
   adding an unclassified route leaves it denied.
3. Idle discovery returns only the bound Agent's currently assigned tools, and
   dispatch independently refuses a removed or ungranted tool.
4. Task admission ignores caller-supplied identity fields, is idempotent, and
   refuses an inactive Person, client, Agent, Connection, runtime principal, or
   quarantined restore.
5. Two concurrent client sessions cannot borrow one pending run's handle. An
   absent, expired, replayed, or mismatched task correlation fails closed.
6. Revocation during a waiting poll prevents payload disclosure; revocation
   before acknowledgment changes no delivery row.
7. Wrong issuer, subject, client, audience/resource, scope, token type, expiry,
   and credential epoch fail. An ID token fails.
8. An Authelia outage fails closed; no legacy `wst_` credential or native Butler
   is substituted.
9. `propose` can only file the existing human-reviewed Plan and cannot approve
   or apply it.
10. REST and MCP produce the same manifest/invoke outcomes and audit identity
    after translation.

Live evidence on an explicitly selected disposable target must separately record
the actual Authelia and Hermes versions/digests, metadata documents, callback,
PKCE S256, resource/audience/scopes, refresh rotation, client and Person
revocation, issuer outage, concurrent-session correlation, and limitations.
Fixture tests do not satisfy that gate.

## 9. Review decisions

Reviewers are asked to accept or amend these bounded choices before code:

1. one immutable external-client registration binds exact issuer/client,
   Person, Agent, Connection, and runtime principal;
2. both Owner registration and bound-Person consent are required, while Owner
   administration never grants private disclosure;
3. `POST /api/v1/agent/tasks` is the sole client-initiated task admission seam
   and derives every authority field server-side;
4. task correlation must be explicit per request or cryptographically
   task-bound per MCP session; `solePending()` is prohibited in the OAuth
   profile;
5. the route table in section 5 is closed and deny-by-default;
6. the first OAuth target is a pre-registered public client using Code + PKCE
   S256, exact resource audience, and introspection on every request;
7. issuer outage fails closed, with no JWT/static-token/native fallback;
8. independent Hermes task initiation is omitted from the first PoC if the
   pinned release cannot carry exact per-task correlation.

Accepting this proposal should produce a companion ADR amendment before or with
implementation because ADR-W039 currently describes only runtime-principal
credentials and task handles, not Person-bound OAuth client registrations or
client-initiated task admission.

## 10. References

- [ADR-W033](../decisions/ADR-W033-the-agent-design.md)
- [ADR-W039](../decisions/ADR-W039-canonical-agent-api-and-pluggable-runtimes.md)
- [ADR-W040](../decisions/ADR-W040-agent-api-transport.md)
- [ADR-W041](../decisions/ADR-W041-agent-runtime-contract.md)
- [`runtime_principals` implementation](../../keystone/runtimedb/butlerruntime.go)
- [Agent HTTP handlers](../../keystone/api/handlers_agent.go)
- [MCP bridge](../../cli/cmd/agent-bridge/mcp.go)
- [Hermes package](../../packages/agent-hermes/package.yaml)
- [MCP 2025-06-18 authorization](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization)
- [MCP 2025-06-18 transports](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)
- [Authelia OIDC provider](https://www.authelia.com/integration/openid-connect/introduction/)
- [Authelia OIDC configuration](https://www.authelia.com/configuration/identity-providers/openid-connect/provider/)
- [Hermes MCP configuration](https://hermes-agent.nousresearch.com/docs/reference/mcp-config-reference)
