# Slice 15: Household and Personal secrets

Implements #216 under epic #201. One PR, based on main after #297. User decisions: local Keystone key; Personal intake belongs only to its owning Person; a voluntarily submitted Household copy can be inspected by an Owner before approval. The Personal original is never transferred or exposed.

## Implemented boundaries

- Migration 0069 adds a distinct encrypted household/user domain. Infrastructure/node credentials keep ADR-W012 semantics. AES-256-GCM binds ciphertext to object ID, scope, owning Person and version. The dedicated key lives at `<DataDir>/household-secrets.key`; no key or plaintext is stored in the runtime DB. Missing, malformed or wrong keys fail closed.
- General tool execution authority remains upstream. Resource authorization intersects live Person identity, Agent admission/class/access, a separate Agent-purpose grant, and explicit Person/Group permission. The existing group resolver is used inside the authorization transaction. Possession of an opaque object reference confers no authority.
- Personal values can be stored directly by their owning Person, without household Owner approval. Only that Person may authorize an Agent to use them. Household management is Owner-only. Use and reveal are independent. No Personal reveal or Personal value rotation/edit route exists.
- Household submissions are separate encrypted pending objects. An Owner's protected review can inspect only that explicitly submitted copy, then approve or reject it. Approval does not grant caller or Agent permissions. Rejection erases the pending ciphertext.
- Protected Household reveal requires caller and Agent grants: Member Agent through Home, Owner Agent through Console. Console requests require an authenticated browser session and consequential writes/reveal require fresh Owner authentication. Ordinary API/service bearer tokens are not reveal surfaces. A narrowly typed response bypasses ordinary value redaction only after authorization and carries no-store headers. UI values hide after 15 seconds, on blur/visibility change, or explicit dismissal.
- Personal quarantine, disable, deletion and offboarding resolution are separate Owner lifecycle actions, not manage grants. Other Persons' Personal labels are not exposed to administrative listing. Person generation changes quarantine Personal objects and invalidate grants. Secret deletion, grant withdrawal and rotation enter the deletion journal; restore replay conservatively quarantines affected objects or erases deleted ciphertext.

## Trusted use

The first consumer is the existing `calendar.get_agenda` tool. A Person can bind their own Personal credential for this configured calendar; an Owner can bind a Household credential. The endpoint and username remain deployment configuration, never model arguments. The active Person's binding takes precedence over the Household binding. Both require explicit Agent-use grants; Household bindings also require a caller grant.

Credentials are resolved into a request-scoped CalDAV client. Redirects are refused and echoed credential values are removed from results. A selected binding never falls back to the environment credential after denial. Other credential-bearing calendar operations are denied when this binding is selected; this slice does not extend secret-backed writes or unattended access. Legacy deployments without a binding retain their existing separate deployment credential behavior. This is not a new calendar account provisioning system.

## Intake and inference

Protected Home/Console forms are the primary entry path. Explicit `/secret personal Safe label = value` stores a Personal item; `/secret household Safe label = value` submits a Household copy for Owner review. Household command scope is explicit disclosure consent. Personal intake writes only a safe label and opaque reference to Personal memory. The value never becomes a memory record or model tool argument.

Recognized free-text credential statements are intercepted before model dispatch and directed to secure entry; they are not guessed apart by an LLM. External intake warns that the platform already received the value and records Person/Connection provenance without copying the value to an audit record. Group conversations remain refused. A second filter at both session and runner model-call paths removes recognized credential statements from old history and resumed tool results. Detection is bounded: arbitrary unlabelled prose cannot reliably be classified as a secret, so protected fields and explicit commands are the supported input formats.

## Validation

Tests cover shared-Agent caller denial, use versus reveal, actual provider input on continuation, protected Homepage/Console routes, Personal lifecycle boundaries, shared group namespace and revocation, explicit Household submission and owning-Person copy consent, ciphertext/key failures, restore replay, and real CalDAV credential consumption/echo suppression.

TDD checkpoints: the initial store tests failed before the API existed; a behavioral RED test reproduced a raw credential statement reaching the provider through old history; the explicit Personal-copy test failed before that operation existed. A fresh-Console API test also caught opaque identifiers being redacted, which was fixed without adding a general redaction exemption.

Four bypass mutations were killed by behavioral failures: caller-grant evaluation, substituting a different Person in the Homepage caller, bypassing the session model-call filter, and skipping calendar resolution at its actual caller. Each mutation was restored. The first caller-grant mutation caused an unused-variable compilation error and was not counted; the corrected compiling mutation failed the intended negative test.

The mocked browser check in `console/scripts/secrets-check.mjs` exercises secure entry, protected reveal, no Personal reveal button, clearing on blur and browser errors. Run `npm run secrets-check`; `PLAYWRIGHT_CHROMIUM_EXECUTABLE` can select an installed Chromium. It needs no live operator token. The separate live `ui-check` requires one and was unavailable locally.

Full slice 16 privacy policy and local_only enforcement remain separate. New secret use starts on trusted built-in surfaces; external raw reveal and unattended resolution are denied. No purchasing/payment tool, Personal reveal surface, general review queue, or infrastructure-secret reveal is added.
