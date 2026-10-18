# External Agent enrollment and activation contract

**Status:** proposed for maintainer review under #580. This document is not an
accepted ADR, implementation authority, or evidence of live Authelia/Hermes
compatibility.

**Scope:** one bounded follow-up to ADR-W049 and the completed #586/#598
storage slices: authenticate the exact configured Person, complete one
issuer-backed consent callback, and atomically activate the pending external
registration. OAuth resource-token resolution, MCP, task admission, delivery
provenance, UI polish, deployment, and live conformance remain separate work.

## 1. Current implemented boundary

Main at `83e9258f15da2cef581ecfbf005a575c1420cd51` contains:

- `external_client_registrations`, which stores inert immutable
  Person/Agent/Connection/issuer/client/runtime-principal configuration and
  terminal revocation history;
- `external_enrollment_challenges`, which stores only a hash of a short-lived,
  Person/registration-bound proof and exposes verification and consumption only
  inside a caller-owned database transaction; and
- Console Authorization Code + PKCE helpers that verify issuer, signature,
  audience, nonce, expiry, authentication time, and `preferred_username`.

Those pieces deliberately do not expose an enrollment endpoint, capture an
issuer subject, activate a registration, or grant Agent authority. The Console
client is Owner-only and proves access to the administrative Console. It must
not be repurposed as the enrollment client for a bound household Member, and a
Console username must not be copied into the registration's OAuth subject.

The missing boundary is therefore a browser ceremony, not a generic activation
setter.

## 2. Proposed ceremony

### 2.1 A distinct Keystone enrollment relying party

Add one public OIDC client owned by the Wildstone Console capability but
separate from `wildstone-console`:

```text
client_id: wildstone-external-enrollment
grant: authorization_code
PKCE: S256 required
scopes: openid profile
consent: explicit
callback: the canonical private Keystone enrollment callback
eligible users: admitted household Persons, not Owners only
```

The client exists only to authenticate consent to a pending registration and
capture the issuer's stable `sub`. It is not the external runtime client stored
in `external_client_registrations.client_id`, receives no `wildstone.agent`
scope, and cannot call the Agent API. Adding the desired-state resource and
applying it are later human-approved Plan work; source tests use a fake issuer
and synthetic origins.

The enrollment RP reuses the narrow `consoleauth.Provider` mechanics only after
generalizing its verified claims to return both `sub` and
`preferred_username`. It does not reuse the Console session, its Owner group,
or its implicit-consent client.

### 2.2 Start boundary

The start route accepts only an opaque registration ID. It is reached through
the trusted private ingress and performs these steps before redirecting:

1. resolve the ingress-authenticated account through the current
   `trusted-ingress` identity link;
2. require the resolved canonical Person to be active and equal the pending
   registration's immutable `person_id`;
3. re-read the pending registration and its live Person, Agent, Connection,
   disclosure, access, runtime, execution, and restore fences;
4. issue the existing short-lived enrollment challenge for that exact
   registration and canonical Person; and
5. create bounded in-memory pending state containing the registration ID,
   Person ID, challenge secret, random browser binding, PKCE verifier, nonce,
   safe return path, and creation time.

The browser receives only the random state and an HttpOnly, Secure, SameSite=Lax
browser-binding cookie. It never receives the challenge secret. Pending state
is single-use, expires no later than the challenge, is capped like Console
sign-in state, and is lost on Keystone restart. Restart therefore invalidates
the ceremony without changing the pending registration.

Registration IDs are selectors, not credentials. A copied start link used by a
different signed-in Person is refused before a challenge is issued.

### 2.3 Issuer callback

The callback is the only caller of activation. In this order it:

1. takes the one-time pending state and requires the same browser-binding
   cookie;
2. rejects issuer errors, missing/expired state, or a changed ingress account;
3. exchanges the code with the saved PKCE verifier;
4. verifies signature, exact configured issuer, enrollment-client audience,
   nonce, expiry, issued-at, fresh authentication time, non-empty `sub`, and
   `preferred_username`;
5. re-resolves `preferred_username` through the live `trusted-ingress` link and
   requires the same canonical Person stored in pending state and the
   registration; and
6. calls one transaction-scoped activation operation with the verified issuer,
   subject, registration, canonical Person, and saved one-time proof.

`preferred_username` connects an already approved Wildstone surface identity
to its canonical Person. It does not become the OAuth subject. The subject is
only the verified `sub` claim and is stored with its exact issuer. Email,
display name, Owner identity, Agent creator, access-list membership, and
matching labels are never identity evidence.

The callback returns a no-store success or generic refusal page. Logs and audit
records may contain registration ID, canonical Person ID, issuer, and outcome;
they never contain the authorization code, PKCE verifier, challenge secret,
token, cookie, or raw ID token.

## 3. Atomic activation

The activation storage operation owns one immediate write transaction. No HTTP
handler may update the registration directly. The transaction:

1. applies the dispatch/restore gate and re-reads the exact pending
   registration;
2. revalidates the live dedicated-Person binding with the existing registration
   authority check;
3. verifies exact issuer equality and a non-empty normalized subject;
4. verifies the saved Person/registration proof and, only after every other
   check passes, consumes it in this same transaction;
5. enforces one active registration per Person, one per Agent, and one per exact
   `(issuer, subject, client_id)` tuple;
6. records subject, activation time, activating canonical Person/actor, and a
   monotonically changing registration credential epoch;
7. revokes every live `wst_` token for the immutable runtime principal while
   preserving the principal-to-Agent binding used as the internal execution
   identity; and
8. changes the registration from `pending_consent` to `active` and commits.

Any failed check rolls back proof consumption, token revocation, subject
capture, and registration state together. Concurrent activations have one
winner through partial unique indexes plus the immediate transaction. A retry
after success is a generic already-used refusal; it does not return the active
record or mint another credential.

The migration extends the registration table with subject and activation audit
columns, admits the `active` state, and adds partial unique indexes for active
Person, Agent, and issuer/subject/client bindings. Identity columns, including
subject after capture, remain immutable. The migration creates no active row,
infers no subject, and preserves all pending and revoked history.

Activation does not disable the `principals` row: OAuth resolution later needs
the same immutable runtime-principal-to-Agent binding as an internal context.
It revokes its bearer tokens and makes active registration state a second
authentication-wrapper fence, so a concurrently presented or newly minted
`wst_` bearer is denied while the OAuth profile is active. Token minting for a
runtime principal whose Agent has an active external registration must also be
refused. Returning to static authentication requires terminal OAuth revocation
and explicit successor credential provisioning; there is no automatic outage
fallback.

## 4. Public surface and ownership

The bounded implementation may add only:

```text
GET /api/v1/external-enrollment/{registration}/start
GET /api/v1/external-enrollment/callback
```

Both are browser endpoints behind trusted ingress, bodyless, rate-limited, and
no-store. They are not runtime routes and are never admitted by the Agent API
classifier. Owner configuration continues to use the existing administrative
service boundary; the enrollment callback cannot create or modify its Person,
Agent, Connection, issuer, client, or runtime-principal inputs.

The current Console capability owns the browser presentation and client
declaration; Keystone API owns authentication and callback handling;
`runtimedb` owns durable activation invariants. No second identity mapper,
permission engine, scheduler, registry, event broker, or OAuth token resolver
is introduced.

## 5. Required automated evidence

The implementation issue is ready only when its acceptance includes synthetic
tests proving:

1. a copied start link used by Person B cannot issue or consume Person A's
   proof, including when B has a valid issuer login;
2. state, browser binding, PKCE, nonce, issuer, audience, freshness, subject,
   ingress account, and canonical Person mismatches each fail without changing
   registration, proof, or tokens;
3. the proof secret never appears in HTTP, JSON, logs, audit, or persisted
   plaintext;
4. activation rechecks every current Person/Agent/Connection/runtime/restore
   fence after callback latency;
5. one active registration per Person and Agent and one per exact
   issuer/subject/client tuple hold under concurrent activation;
6. rollback preserves an unconsumed proof and live static token, while commit
   consumes the proof, captures subject once, and revokes every static token;
7. a token presented or minted after activation is refused, while unrelated
   Agents' credentials remain usable;
8. migration replay preserves populated registration/challenge history and
   creates no inferred subject or active registration; and
9. the enrollment client declaration renders as public Code + PKCE S256 with
   explicit consent and no Agent/API scopes.

Focused database and API tests, migration replay, formatting, vet, and the
repository-wide race suite are required. Fake-provider tests establish the
contract only. #356 still owns installed Authelia/Hermes versions, real
callback and consent behavior, resource tokens, refresh/revocation, direct
Hermes correlation, restart, issuer-outage, and LAN-only evidence.

## 6. Explicit exclusions and follow-ups

This contract does not authorize access-token introspection, OAuth resource
authentication, `/mcp`, direct task admission, task correlation, delivery
provenance, disable/re-enable, UI management screens, deployment, live
credentials, or a live household test. It does not activate generic events or
transfer work to a native fallback.

After maintainer acceptance, create one focused implementation issue for this
ceremony and atomic activation. Later issues may consume the active record for
resource-token resolution and task admission only after separately verifying
their own final-boundary and provenance requirements.

## 7. Decision requested

Accept this first ceremony or request an amendment to one of these three
choices:

1. a distinct household-member enrollment OIDC client rather than the
   Owner-only Console client;
2. trusted-ingress identity plus a fresh verified enrollment ID token as the
   two independent inputs that bind canonical Person and issuer subject; and
3. activation-time revocation of all static runtime tokens while preserving
   the runtime principal as the internal Agent binding.

Until that decision is explicit, this document remains a proposal and no
activation endpoint or migration is implementation-ready.

## 8. References

- [ADR-W049](../decisions/ADR-W049-external-agent-identity-and-oauth-transport.md)
- [External Agent identity, route, and OAuth contract](external-agent-identity-route-oauth-contract.md)
- [`externalregistration.go`](../../keystone/runtimedb/externalregistration.go)
- [`externalenrollment.go`](../../keystone/runtimedb/externalenrollment.go)
- [Console sign-in boundary](../../keystone/api/auth_console_session.go)
- [OIDC verifier](../../keystone/consoleauth/oidc.go)
- [Wildstone Console client declaration](../../packages/wildstone-console/package.yaml)
