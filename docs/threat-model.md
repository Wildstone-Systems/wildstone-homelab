# Threat model: boundaries, evidence and residual risks

Documentation review: 2026-10-22, baseline
`60df9dcbc1daa99d43f7692eb83f7d72f7070956`. This updates selected stale
claims by reading implementation, tests and accepted ADRs; it is not a fresh
penetration test or a complete security audit. Some detailed sections retain
historical evidence. [ADR-W033](decisions/ADR-W033-the-agent-design.md) and
the [agent design](agent-system-design.md) govern newer Agent semantics.

## Assets

1. Family data in volumes (photos, documents, media).
2. Secret values (DB passwords, OIDC client secrets, restic repo
   passwords, API tokens).
3. Control of the nodes (Docker) — the ability to run code.
4. Desired state integrity (the Statefile).
5. Identity/ingress (who can reach what, who can log in).

## Trust boundaries and mitigations

### Internet → public API

- Public management routes, browser sessions, machine credentials and Agent
  transports have different authentication entry points. Do not interpret
  the older claim that every route uses one bearer middleware chain literally.
  See `keystone/api/api.go`, `keystone/consoleauth`, `keystone/principal`
  and the external enrollment/OAuth handlers.
- Console is Owner-only with OIDC/PKCE and authentication freshness; machine
  API tokens are opaque and stored hashed. Person roles, Agent classes and
  machine credential roles are distinct.
- Redaction and bounded outputs are defense in depth. Explicit response and
  credential-mint handling must be reviewed per route; no assertion that only
  two responses are exempt from encoding is made here.

### Keystone → nodes

- mTLS with a Keystone-local CA; nodes join with single-use tokens,
  pinned by CA fingerprint (ADR-W003, ADR-W014). Compromising the
  network gets you neither a join nor a replay.
- **Closed op surface, no Exec** (ADR-W003): a fully compromised
  Keystone can manage containers/volumes/managed files on nodes but
  has no generic host-shell operation and cannot use managed-file operations
  to read arbitrary paths or write outside managed prefixes (agent-side path resolution refuses
  traversal and symlink escapes).
- Images must be digest-pinned; the agent refuses un-pinned refs.
- Ownership labels: the agent refuses to touch containers it does not
  own — hand-managed workloads are out of reach by construction.

### Operator input → state

- Infrastructure proposals use Plans; a human approves them (ADR-W004/W007). Policy (ADR-W009) validates every proposed
  tree — public routes require auth, secret-shaped literals are
  rejected from specs (`SecretShaped` shares its definition with the
  redactor, so they cannot disagree).
- The Statefile is git-backed. Do not assume it is an atomic authority
  boundary across Git, the worktree and SQLite. The proposed
  [publication/approval contract](design/desired-state-publication-and-approval-contract.md)
  addresses those boundaries and is not implementation authority.

### Secrets

- Write-only path (ADR-W012): values transit once, are stored
  encrypted per-node, and the protocol has **no individual read-back arm**.
  Keystone logs, API responses, and Butler transcripts are all behind the
  shared redactor.
- Whole-site recovery adds one closed export operation, not a reveal path.
  Wardstone opens its store only in-process and encrypts the complete bundle
  to an offline X25519 recovery public key; Keystone, the model provider and
  the object-store provider receive ciphertext only. Import is accepted only
  by an empty fresh-node store and immediately re-seals values under its new
  node-local key. Recovery keys and bundle bytes are excluded from Wardstone's
  audit allowlist.
- Household recovery seals the vault key, not a second plaintext copy of each
  value. The SQLite snapshot retains ciphertext, grants and audit history; the
  offline private key restores the vault key before the database is opened.
- The recovery private key is the new high-value offline asset. Compromise of
  that key plus the bucket and repository passwords exposes the backed-up
  secrets. It is never stored in the Statefile or on a running Wildstone host;
  the runbook requires a password manager and paper/offline copy.
- During an explicit node import the private key transits the authenticated
  Keystone-to-Wardstone operation. A control plane compromised at that exact
  recovery moment can copy it, so recovery must happen on a freshly rebuilt,
  trusted host and the key must be removed immediately afterward.
- A malicious or compromised running Keystone can request fresh sealed bundles
  but cannot decrypt them without the offline private key. It can cause denial
  of service or upload stale/invalid sets; the manifest hashes detect damaged
  artifacts, while authenticity still relies on control of the restic
  repository and its password.

### Household publishers and reply tokens (ADR-W024)

- A publisher holds one verb, `emit`. A compromised integration shim
  can lie about the house — claim an arrival, a meal, a calendar
  change — and can do nothing else: not read state, not propose, not
  approve. The intake stamps an event's `source` from the token, so it
  cannot speak under another publisher's name.
- Household event content is attacker-influenceable text and is
  treated as such the moment it reaches a model: the same redactor and
  untrusted-content envelope every other external string gets.
- **A reply token travels to a household device inside a
  notification.** That is a deliberate exposure and is why the token is
  nothing more than a short-lived publisher credential: whoever reads
  it off a phone can claim the household answered a question, and
  nothing else. It expires (`-household-reply-ttl`), and the answers it
  can forge are facts a rule may read, never actions a rule may take.
- **Household escalation gives a model no new reach.** A phrasing call
  is made with an empty tool set, after the engine has already decided
  that the rule fires, what it runs, at which tier, and for whom. The
  answer replaces the text of one already-approved param and is bounded
  and stripped at the engine's own boundary. An attacker who fully
  controls an event payload can therefore influence the wording of a
  message the household was already going to receive on their own
  device, and nothing else — not whether it is sent, not what it does,
  not who it reaches. Facts still pass through the shared redactor, so
  a secret smuggled into a payload does not reach the model either.
- A household rule's action is bounded by tier twice over — policy
  refuses an infrastructure-tier action at proposal, the domestic
  gateway refuses it at dispatch — so a rule that reached the engine by
  some path nobody anticipated still cannot restart a container.

### Mailbox monitoring (epic #326; ADR-W030 A5.2–A5.9)

A Person's mailbox is the most attacker-reachable input Wildstone reads:
anyone can send mail to it. The monitor is built so that what arrives can
propose an event to its Person and nothing else.

- **Read-only, structurally.** The adapter (`wild-mail-adapter`) answers
  four read queries; the mail integration domain declares no action, the
  work queue refuses a mutating name at dispatch, and the IMAP reader only
  EXAMINEs, SEARCHes and FETCHes with BODY.PEEK. The fake provider in the
  tests records any mutating command as a violation.
- **One Person, one connection, one key.** Each connection has its own
  credential, cursor, keyed fingerprints and references. A reference minted
  for one mailbox is refused by every other, an adapter principal is bound
  to one connection on one node and claims only while that node holds the
  lease, and every candidate, review, notice and effect read resolves the
  caller to the owning Person (an Owner, another Person or an Agent gets
  "not found"). The cross-Person denial matrix
  (`keystone/api/mail_e2e_test.go`) covers message, candidate, notice,
  approval, calendar and health/status.
- **Two keys to run, either withdraws.** Background reads need the
  Person's authorization AND an Owner-approved subscription of that exact
  version, re-checked live at dispatch and again before anything is
  recorded. Revocation, offboarding, credential replacement and package
  removal fence first — the lease epoch moves and queued or claimed reads
  are withdrawn — then remove the adapter container, then erase the
  credential with node evidence. Unproven erasure leaves the connection
  `degraded` (`erasure_unverified`) and visible.
- **Hostile mail.** Headers and bodies are untrusted evidence. A model
  sees a bounded excerpt of normalized text (at most 12 KiB) as quoted
  evidence, with no tools, and must return one strict JSON object that
  Keystone validates; it cannot set the target calendar, audience, state
  or an approval. Nothing in a message can write memory, create a
  routine, change an authorization or approve: an approval is a
  single-use token bound to the Person, the exact revision and fields,
  the target calendar and the delivery route, and a typed "yes" answers
  nothing.
- **What is kept.** Only a cursor, keyed fingerprints, minimal event
  fields and a sealed excerpt of at most 300 bytes while a candidate is
  unresolved. No full body, subject or sender is stored, logged, returned
  by the API, or written to a backup; mail answers are scrubbed from the
  work queue once processed and from every backup copy. The excerpt key
  (`<DataDir>/mail-excerpts.key`) is in no backup, so a database copy holds
  only unreadable ciphertext, and a restore discards every excerpt. The
  mail paths log through `redact.LogHandler`.
- **Calendar effects at most once.** A deterministic event UID, a
  conditional create, and a lookup by UID before any resend; the crash
  tests (`keystone/api/mail_e2e_crash_test.go`) kill the path at every
  durable boundary.
- **Residual.** A provider that cannot issue a read-only credential gives
  the app password wider power upstream than Wildstone uses; the
  credential lives on the placement nodes and in the running adapter's
  environment, so a compromised node can read it. Recovery does not
  restore Personal credentials: each Person re-enters theirs.

### LLM (the Butler) — treated as a confused deputy, not a trusted user

ADR-W006/W017; the red-team suite (`keystone/butler/redteam_test.go`,
`injection_test.go`) is pinned and release-blocking:

- Infrastructure proposals remain subject to human approval. The older
  read-plus-propose-only description applies to the infrastructure path, not
  every Agent tool. Current Owner/Member gateways expose granted application
  tools and an opt-in Owner `host.*` family with its own tier/confirmation
  checks. See `keystone/butler/owner_toolset.go`, `hostops_manifest_test.go`
  and `keystone/domestic`; Agents cannot approve their own Plans.
- **A container is never handed the node's secrets.** The agent's data
  directory (node identity, the secret-store key, the encrypted secrets)
  lives outside every managed prefix, and `EnsureContainer` refuses a bind
  mount whose host path overlaps it in either direction. Both halves
  matter: the layout keeps the secrets out of the tree packages mount, and
  the check stops a package asking for them anyway. This was found by
  deploying the identity package — `/etc/wildstone` was bind-mounted into
  the internet-facing Caddy, and the data directory defaulted to a path
  inside it, so the reverse proxy could read the node's mTLS identity and
  decrypt its secret store. `readOnly` and 0700 were both irrelevant: the
  container runs as uid 0, and reading is the compromise.
- Every tool result is centrally redacted, size-bounded, and — where
  attacker-influenceable (health, diagnose) — wrapped in an
  untrusted-data envelope with case/spacing-tolerant breakout
  neutralization.
- QA-only mode refuses mutating tools at dispatch, not just by
  omission from the offered set.
- Ask input is bounded; transcripts are persisted redacted; provider
  keys come only from environment variables.

### Supply chain

- Packages are signed and verified (ADR-W010); registry fetches verify
  digests; images are digest-pinned everywhere (including the restic
  image); CI runs gitleaks with a custom rule for the `wst_` prefix.

## Residual risks (known, accepted for alpha)

- The public API binds loopback/tailnet by default and offers no TLS
  of its own — exposure beyond that perimeter needs a TLS reverse
  proxy in front; public app ingress is Route-managed Caddy, but the
  API must not be exposed wholesale. Explicit enrollment/callback routes have
  their own ingress and identity contracts; do not infer safe exposure for
  every route from their existence.
- A malicious human admin is out of scope: admin approves plans, and
  approved plans run.
- Docker and the workload host kernel are trusted execution dependencies.
  A container escape or compromised allowed image can compromise the node;
  host networking, D-Bus, device mappings and selected network capabilities
  increase exposure. A closed wire vocabulary does not remove this risk.
- DoS beyond token-bucket rate limiting is out of scope.
- A reply token is bounded by time, not by use: within its TTL it can
  post more than the one answer it was minted for. Single-use reply
  tokens are the obvious tightening and are deliberately not in the
  first cut, because the blast radius is one forged household fact.
- The drift classifier protects foreign resources but cannot protect
  Wildstone-owned resources from hand edits on the node; drift repair
  reverts what it observes, Low-risk only.

## Proposed failure-domain and Proxmox boundary (not implemented)

[ADR-W051](decisions/ADR-W051-failure-domains-and-recovery.md)
introduces requirements, not new credentials or execution authority. Under
ADR-W050, Proxmox (if used) only hosts k3s node VMs: no Proxmox HA for
workloads, no Docker or k3s directly on the hypervisor by default, and no
Proxmox credentials for the Butler. Several VMs on one machine do not protect
against shared power/storage failure. Guest backups must explicitly
account for external mounts, passthrough and application-consistent state.

Keep management recovery independent of household SSO and keep an external
observer for complete host loss. Never promote two Keystone coordinators from
replicated state. Proposed application adapters must reuse existing authority
and secret boundaries; declaring a supported action is not permission to run it.
