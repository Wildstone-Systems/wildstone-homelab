# ADR-W049: External Agent identity and OAuth transport profile

- **Status:** proposed for maintainer acceptance; design only; not implemented by this document.
- **Proposed by:** #541 under milestone #1, coordinated by #580.
- **Product inputs already selected:** the first external profile uses a dedicated Agent for each bound Person, and the first live proof must support conversations started directly in Hermes.
- **Amends on acceptance:** ADR-W039 §§3.3–3.4, 4–5 for the Person-bound external profile; ADR-W040 §§2–3 and §6 for its OAuth resource and MCP termination.
- **Keeps:** ADR-W033 authority/privacy rules, ADR-W039's canonical Agent API and gateway, ADR-W040's REST/long-poll baseline and static sidecar profile, ADR-W041's native runtime contract, the human-approved Plan path, and the separately configured native fallback.

This ADR is the bounded companion to the source-backed
[external-Agent contract](../design/external-agent-identity-route-oauth-contract.md).
Its proposed status is deliberate: issue readiness authorizes review of this
amendment, not runtime work or automatic acceptance. A maintainer must accept
the amendment before #586 or another OAuth-profile implementation slice can
consume it.

## 1. Context

ADR-W039 and ADR-W040 describe one Agent API authenticated by a Keystone
runtime principal. That profile is implemented: a `wst_` bearer resolves
through immutable `runtime_principals`, Keystone confines it to the closed
Agent route family, and REST or the `agent-bridge` sidecar reaches the same
gateway. #584 and #585 further closed the static credential's route and
delivery/acknowledgment revocation boundaries.

That contract does not bind an independently authenticated Person and OAuth
client to one external Agent. It also assumes that a task handle already
exists, while the selected first Hermes workflow includes a Person starting a
conversation directly in Hermes. Finally, ADR-W040 §6 terminates MCP in a
shared-secret sidecar; an OAuth MCP resource must validate the Person/client
bearer at Keystone's authentication boundary before decoding model-supplied
content.

The new profile must add those facts without turning OAuth, an Owner, a client
name, or a chat address into a second authority source.

## 2. Decision

### 2.1 One dedicated external-profile Agent per Person

The first OAuth profile binds one canonical Person to one dedicated external
Agent. Existing shared, native, packaged, and static-credential Agents are not
converted or reinterpreted. The dedicated Agent still owns tool assignments,
controls, processing class, memory restrictions, Connections, and its one
current runtime principal; the Person remains the privacy and interactive
authority subject.

The first-profile registration cardinality is closed:

- one active external-client registration per dedicated Agent;
- one dedicated external Agent per bound Person for this profile;
- one exact `(issuer, subject, client_id)` tuple resolves to at most one active
  registration across all Persons, Agents, and Connections; and
- one registration names one canonical Person ID, Agent ID, chat Connection
  sequence, and immutable runtime principal binding.

This is a first-profile constraint, not a claim that every future runtime must
be single-user. Supporting multiple clients or Persons behind one Agent needs
a later accepted contract and cannot relax this profile in place.

### 2.2 Pending Owner configuration is not Person consent

Owner configuration and bound-Person consent are separate durable facts. The
registration state machine is:

```text
pending_consent -> active -> disabled | revoked
                              ^
                              | explicit re-enable only while every binding is current
```

An Owner may create a `pending_consent` record containing an opaque
server-generated registration ID, canonical Person ID, dedicated Agent ID,
chat Connection sequence, configured issuer and client ID, and current runtime
principal. The record also retains who configured it and when. It confers no
discovery, invocation, task, delivery, acknowledgment, or token-resolution
authority.

Only an issuer-authenticated consent flow completed by that exact bound Person
may capture `subject` and activate a pending record. An Owner-entered subject,
`consent=true` flag, display name, email, Console username, creator identity,
access-list membership, existing bearer, or matching label is never proof of
identity or consent.

The identity-bearing fields are immutable. Changing Person, Agent, Connection,
issuer, subject, client ID, or runtime principal creates a successor and
revokes the old registration. There is no inferred legacy backfill. Existing
static/native/packaged credentials and historical deliveries retain their
explicit profiles and do not become OAuth-visible.

Every use rechecks the live Person admission, Agent access/class and lifecycle,
Connection admission/disclosure, exact runtime binding, processing class,
credential epoch, and restore/household-stop fences. Disabling or revoking a
registration closes subsequent boundaries but does not disclose its Person's
private content to an Owner or silently transfer work to the native fallback.

### 2.3 Direct Hermes task admission is mandatory

The first live proof includes a conversation initiated directly in Hermes. A
model-invisible Keystone admission operation creates the ordinary task and
handle before any model-selected capability call. The request may supply only
bounded user input plus opaque client session and retry identifiers. Keystone
derives Person, Agent, Connection, audience, processing class, grants, and
budget from the freshly resolved registration and current records; the client
cannot name or override them.

Admission is retry-idempotent on
`(registration_id, client_session_id, idempotency_key)` and returns the same
task for the same retry. Every subsequent task-scoped REST or MCP operation is
bound to that exact task. `solePending()` is not proof of correlation for this
profile. Acceptable correlation is either an explicit request-bound task grant
verified on every call or a cryptographically unguessable MCP session issued
for one admitted task and unusable for another.

If the pinned Hermes client cannot carry either form, the first-profile
acceptance case is blocked. A bounded adapter or client compatibility solution
must be reviewed and verified. Delivery-only operation may be an intermediate
test, but it is not milestone completion and does not permit silently omitting
direct conversations.

### 2.4 OAuth MCP terminates at Keystone

The OAuth MCP resource terminates inside Keystone's authenticated HTTP
boundary. It validates the bearer and closed method/path classification before
body decoding, resolves the exact active registration, then translates
`tools/list` and `tools/call` onto the canonical `agentapi` and
`agentgateway.Gateway`. OAuth does not create a second registry, permission
engine, task store, scheduler, approval path, or MCP business implementation.

The existing `agent-bridge` remains the separately configured static profile:
it authenticates its client with `MCP_SHARED_SECRET`, calls Keystone with a
`wst_` runtime token, and may keep its existing compatibility behavior. It is
not an OAuth proxy and cannot accept or forward the Person/client bearer. A
failed OAuth request never falls back to that token, another Agent, or the
native Butler.

The proposed OAuth target remains a pre-registered public client using
Authorization Code with PKCE S256, an exact Keystone resource audience,
`wildstone.agent` scope, and issuer introspection at every protected and final
poll/ack boundary. Issuer failure is fail-closed for this profile. These
transport details remain proposed until this ADR is accepted, and installed
Authelia/Hermes compatibility remains unverified until #356 records pinned,
live evidence.

## 3. Requirements mapped to current sources

| Requirement | Current source or governing ADR | Amendment or preserved boundary |
| --- | --- | --- |
| Runtime credential identifies one Agent | `keystone/runtimedb/butlerruntime.go`; ADR-W039 §3.3 | Preserve the one-current-principal model; registration adds Person/client facts without multiplying current principals. |
| Person identity is canonical and not name-derived | `keystone/runtimedb/personid.go`; ADR-W033 A0 | Bind the stable Person ID; issuer subject is captured only by that Person's authenticated consent. |
| Chat Connection is the disclosure/admission boundary | `keystone/runtimedb/connection.go`; `butler_addresses` | Store one exact chat Connection sequence; never substitute `integration_connections` or a label. |
| Runtime routes are closed before body decoding | `keystone/api/api.go`; ADR-W040 §§2–3; #584 | Preserve the delivered static gate and extend deliberate classification only for accepted OAuth routes. |
| Task authority comes from the ledger and handle | `keystone/agentgateway`; ADR-W039 §§3.1, 3.4 | Add model-invisible admission; derive every authority field server-side and bind exact correlation. |
| Static delivery/ack authority is transactionally fenced | `keystone/runtimedb/agentdeliveries.go`; ADR-W040 §4; #585 | Preserve static semantics. OAuth-visible rows later require immutable registration/Person/Connection/Agent/audience provenance and the same final fence. |
| MCP is only a translation of the canonical gateway | `cli/cmd/agent-bridge/mcp.go`; ADR-W040 §6 | Static profile stays in the sidecar; OAuth MCP translation moves to Keystone's authenticated resource boundary. |
| Human approval remains authoritative | ADR-W033 E-series; ADR-W039 §§3.2, 3.4 | Direct task admission grants no approval/apply authority; infrastructure mutation still files a Plan. |
| Native fallback is separate | ADR-W039 §§3.7, 4; ADR-W041 | No automatic work transfer, identity substitution, or processing-class downgrade. |

## 4. Exact amendments and conflicts

On acceptance, this ADR makes these scoped amendments:

1. **ADR-W039 §3.3 and §4 identity/authentication.** The static/native profile
   remains principal-to-Agent. The external OAuth profile additionally requires
   an active immutable Person/client/Connection registration and live
   registration checks. The external profile's Agent is dedicated to its bound
   Person.
2. **ADR-W039 §3.4 and §4.1 task origin.** A Wildstone-issued handle remains
   mandatory. Direct Hermes work gains one model-invisible admission seam so a
   handle can be issued before the first capability call; no runtime claim
   supplies Person, Connection, audience, or grants.
3. **ADR-W039 §5 Hermes profile.** “One Hermes profile per Agent” is narrowed
   for this first OAuth profile to one dedicated Agent per bound Person. This
   does not alter existing shared/static/native Agents.
4. **ADR-W040 §2 authentication.** `wst_` runtime-principal bearer remains the
   implemented static profile. The OAuth profile uses a validated resource
   access token to resolve the immutable registration, then constructs the same
   internal runtime-principal context. The external bearer stays request-local
   and never reaches gateway business code or an upstream service.
5. **ADR-W040 §3 endpoint map.** A future accepted implementation may add the
   model-invisible task-admission endpoint and the OAuth `/mcp` endpoint to the
   closed classifier. Their presence in this proposed ADR does not register or
   authorize them.
6. **ADR-W040 §6 MCP location and correlation.** The sidecar remains the
   static/shared-secret profile. The OAuth profile terminates in Keystone and
   forbids `solePending()`; exact request or task-bound-session correlation is
   mandatory.

No other ADR-W039/W040 transport, delivery, gateway, capability, memory,
proposal, audit, or runtime rule changes.

## 5. Bounded future interfaces

Names below define responsibility, not an instruction to create all interfaces
in one implementation slice:

```text
CreatePendingExternalRegistration(owner, personID, agentID, connectionSeq,
                                  issuer, clientID, runtimePrincipal)
  -> inert registrationID

ActivateExternalRegistration(registrationID, validatedIssuerConsent)
  -> immutable active registration | deny

ResolveExternalRuntime(accessToken, method, canonicalResource)
  -> registrationID, personID, agentID, connectionSeq,
     runtimePrincipal, clientID, credentialEpoch, requestLocalRecheckProof

AdmitExternalTask(resolvedRegistration, clientSessionID, idempotencyKey,
                  boundedObjective)
  -> exact taskID, handle, expiry

BindMCPTask(resolvedRegistration, handle, requestOrSessionProof)
  -> exact current task context | deny

RecheckExternalRuntime(requestLocalRecheckProof, operation, taskOrDeliveryID)
  -> freshly resolved identity | deny
```

#586 is limited to inert `pending_consent` persistence and typed accessors. It
does not implement activation, token resolution, task admission, MCP, delivery
provenance, or deployment. Those remain separate, explicitly scheduled slices
after their prerequisites are accepted.

## 6. Required proof before implementation is complete

Repository tests for later slices must use synthetic Persons, dedicated Agents,
clients, Connections, tasks, and a fake issuer and prove:

1. pending configuration grants no Agent API or MCP authority and cannot be
   activated with Owner-supplied identity data;
2. one active registration per Agent and one active exact
   issuer/subject/client tuple are enforced under concurrency;
3. no name, email, creator, access-list, Connection label, or legacy row is
   used as identity or backfill evidence;
4. direct task admission derives all authority, is retry-idempotent, and fails
   closed for every stale Person/Agent/Connection/runtime/restore fence;
5. absent, mismatched, expired, replayed, or cross-session task correlation is
   denied, including when only one run is pending;
6. OAuth route denial occurs before body decoding, while REST/native/MCP share
   identical gateway outcomes and audit identity;
7. token, registration, Person, Agent, Connection, or runtime revocation during
   poll/ack prevents disclosure or mutation without changing static retry
   semantics; and
8. no rejected OAuth call falls back to a static credential or native Agent.

#356 must separately verify the installed Hermes and Authelia versions,
metadata, callback, PKCE, resource/audience/scopes, refresh and revocation,
direct-conversation correlation, restart/lost-ack behavior, issuer outage, and
LAN-only limitations on an explicitly authorized disposable target. Fixtures
or generic upstream documentation do not satisfy that live gate.

## 7. Consequences

The first external profile has more records and checks than the static profile,
but every authority-bearing fact becomes explicit and revocable. Owners can
configure and revoke a client without impersonating its Person. Direct Hermes
conversations become a real acceptance obligation instead of an optional
capability silently dropped when correlation is inconvenient. Keystone remains
the only public authority boundary; the sidecar remains useful without being
mistaken for an OAuth resource.

The cost is a hard dependency on verified client correlation and the selected
issuer behavior. Until those are proven, the correct state is blocked, not a
weaker identity inference, cached authority, broad static token, or automatic
fallback.
