# The Wildstone agent system — design contract

This document describes the intended Wildstone agent system as a clean design target. It is not a statement that the repository already implements every rule below.

The architectural reasoning and supersession history live in [`ADR-W033`](decisions/ADR-W033-the-agent-design.md). Detailed audit evidence remains in the review/audit documents.

`Agent` and `Butler` mean the same thing. Technical text may prefer Agent; product-facing text may prefer Butler.

---

# 1. System model

Wildstone separates five concerns:

1. **Identity** — who/what exists.
2. **Authority** — what may happen.
3. **Knowledge** — what is known and who may see it.
4. **Work** — what is being done and whether it succeeded.
5. **Reach** — which external/local Connection carries interaction.

Authority does not come from model output, memory content, chat text, documents, or tool results. Those may inform decisions; they never grant permission.

---

# 2. Household and people

## 2.1 Household

One household is assumed per deployment for now. Durable resources that may later need household isolation should carry household identity rather than depending on a global implicit singleton forever.

## 2.2 Person

A Person is one human being with one stable internal identity.

A Person may have many identity bindings:

- directory login
- web session
- Telegram account
- Discord account
- future voice/device bindings

Bindings prove that an external identity belongs to the Person. Display names and claimed usernames establish nothing by themselves.

## 2.3 Human roles

There are exactly two fundamental roles:

- `owner`
- `member`

A `child` is a constrained Member profile/policy preset, not a third RBAC role. Other constrained profiles may be added using the same mechanism.

Owners administer the deployment. Members consume household services and capabilities.

## 2.3.1 Scheduled subjects (ADR-W033 A8)

A Person may be a **subject** of household records without being a **principal** who acts. A subject is named, scheduled, assigned things and displayed; it needs a Person record and nothing else. A principal signs in, holds credentials, reaches an Agent and accumulates memory; it needs admission.

A Person carrying the `child` profile is a subject. That is a complete supported state and household features may rely on it.

Normative:

- A child record MUST name a guardian and MUST NOT hold any role but Member.
- A child record MUST NOT be admitted to Butler membership. `AdmitButlerMember` refuses it by every path, including a direct `INSERT`.
- A scheduled subject MUST carry no authentication binding: no directory account, no API principal, no chat binding, no session. Recording a Person as a child, or converting one, MUST remove any that exist; a deployment that cannot MUST refuse the conversion. **Admission state does not imply this** — `admitted` governs Butler membership only, and directory and principal authentication do not consult the profile.
- None of the above authorizes the Child surface, which ADR-W030 A2.1 gates separately on credentials, principal, endpoint, gateway and transport eligibility.

Acceptance: a child record cannot be admitted by any path; a child with an authentication binding is refused or has it removed at the point of recording; a household feature can name, schedule and display a child that has never signed in.

## 2.3.2 Guardian standing (ADR-W033 A9)

For a Person carrying the `child` profile, the recorded guardian has that child's standing over the child's own household records: the guardian may see and answer them.

Normative:

- Standing derives from the **recorded** guardian relationship only. It MUST NOT be inferred from a group, a role, or from being an adult.
- It is asymmetric: guardian reaches child, never the reverse.
- It is not a third role. A guardian is an ordinary Owner or Member named on a child's record.
- A sole guardian MUST NOT be removable while the child record is active.

Acceptance: a guardian sees and may complete their own child's personal-scope items; an unrelated adult sees neither; a child confers nothing over its guardian; an adult acquires nothing over another adult.

## 2.3.3 Per-person agendas on a household surface (ADR-W033 A10)

A per-person agenda is a household surface with a subject selector. Choosing a subject selects what is rendered; it does not establish who is asking, and it MUST NOT widen what the reader may be shown.

Normative, and identical in every session because no request attribute reports whether a screen is shared:

- A reader may see a subject's household-scope records whenever they can open the surface.
- A reader may see a subject's personal-scope records only when they are the subject, when the record names them as able to complete it, or when 2.3.2 gives them guardian standing over the subject.
- Selecting a subject grants no standing the reader did not already hold.

Acceptance: an adult selecting another adult is shown no personal-scope item of theirs; a guardian selecting their child is; a reader selecting themselves is.

## 2.4 First Owner bootstrap

Installation cannot complete without an active Owner.

The installer creates the initial Owner and directory identity from an administrative username/password before setup completes. Wildstone does not use a first-browser-visitor bootstrap model.

## 2.5 Last Owner invariant

At least one active Owner must always exist.

The final active Owner cannot be demoted, disabled, soft-deleted, or otherwise stripped of Owner authority until another active Owner exists.

## 2.6 Host recovery

Root-equivalent host access is the break-glass recovery authority.

Wildstone provides CLI flows equivalent to:

```text
wildstone owner reset-password <username>
wildstone owner create <username>
```

They may be used from a local root shell or a root-privileged SSH session. Recovery is audited.

---

# 3. User-facing surfaces

## 3.1 Homepage

Homepage is the household plane.

Every authenticated Owner or Member can reach Homepage, subject to entitlements. Household SSO sessions are persistent enough that normal navigation among Homepage and services does not repeatedly ask for credentials.

Homepage stays a household experience even for Owners. It may link to Console, but it does not become an administrative dashboard.

Only Member Agents are exposed on Homepage.

## 3.2 Console

Console is the administrative plane.

Only Owners may enter it. There is no delegated Member exception.

Entering Console requires fresh or recent step-up authentication even if the household SSO session is valid. Highly consequential operations require a currently fresh Owner session, but not a new password/2FA challenge for every individual operation inside that window.

Demoting an Owner immediately revokes Console access and active Console sessions.

---

# 4. Service entitlements and groups

Each package/service declares one or more Wildstone access groups.

Wildstone projects effective membership into LLDAP/Authelia using the strongest compatible authentication mechanism for that service:

- OIDC where supported
- LDAP where supported
- proxy/forward-auth only where compatible with browser and non-browser behavior

Homepage visibility is presentation only. Direct service access must still be denied when the Person lacks the service entitlement.

New Members inherit a default household access group such as `Family`.

Reusable higher-level groups may include examples such as:

- Adults
- Kids
- Media
- Documents
- Home Automation

Explicit restriction overrides inherited allow.

**These groups are the household's one principal namespace.** Service entitlements, memory audiences (11) and secret grants (12) all reference the same groups — `Adults`, `Kids` — rather than each maintaining its own parallel notion of who they mean.

**One namespace is not one permission.** Membership of `Adults` is an *identity primitive*; what it authorizes is decided by each resource separately. Being in `Adults` does not grant every adult service, every adults-only memory record, and every adults-only secret. It means that when those three rules each say "adults", they are talking about the same people.

The alternative — a separate audience vocabulary beside the entitlement one — produces two group systems that drift, and a household that cannot say why a person is in one and not the other.

---

# 5. Agents / Butlers

## 5.1 Ownership

Agents belong to the household, not the Owner who created them.

Any Owner can administer any Agent. Members cannot create, modify, clone, pause, retire, delete, or reconfigure Agents.

## 5.2 Agent classes

There are two classes:

- `owner` Agent
- `member` Agent

Owners may interact with either class.

Members may interact only with Member Agents.

The normal topology must not admit Member-to-Owner-Agent interaction and then rely on runtime narrowing as the primary safety mechanism. Runtime narrowing remains defense in depth.

## 5.3 Agent configuration

An Agent has at least:

- stable identity
- name/directive
- class
- lifecycle state
- tool grants
- ordered model/provider chain
- Connections
- memory configuration
- assigned routines/tasks
- runtime record: runtime type (`native` = Keystone-hosted, or an external `hermes`/`openclaw` runtime), its runtime credential, and for an external runtime the Owner-declared processing class (ADR-W039 §3.3)

One Agent can concurrently serve multiple people. Agent identity is shared; transient conversation state is isolated.

---

# 6. Tools and service capabilities

## 6.1 Tool classes

Each tool declares a minimum authority class:

- Member
- Owner

Member tools may be assigned to Member or Owner Agents.

Owner tools may be assigned only to Owner Agents.

A Member Agent containing an Owner tool is invalid configuration.

## 6.2 Opt-in tools

A tool may also be `opt-in`.

Opt-in tools are excluded from broad default or dynamic bundle grants even if their authority class otherwise matches.

This provides a place for unusually consequential tools without inventing a new Agent class.

## 6.3 Service bundles

Owners can grant:

- a dynamic service/class bundle
- explicit individual tools

Example:

```text
Home Assistant
[x] all Member tools
[ ] all Owner tools
```

A dynamic bundle automatically includes future tools only when they match the selected service/class and are not opt-in.

Explicit tool lists remain static.

## 6.4 Creation and class changes

A newly created Owner Agent receives all currently available Member and Owner tools by default except opt-in tools.

A Member Agent can receive only Member tools.

Downgrading Owner Agent -> Member Agent strips all existing tool grants, after which appropriate Member grants can be reapplied.

Upgrading Member Agent -> Owner Agent preserves existing Member grants but does not automatically add Owner tools.

---

# 7. Model/provider chains

Each Agent owns its own ordered model/provider chain.

There is no deployment-global fallback order that silently applies to all Agents.

Example:

```text
Alfred
1. Ollama/qwen
2. OpenAI
3. OpenRouter

Sandy
1. Ollama/gemma
2. Anthropic
```

Fallback inside one Agent is allowed only when the task/content privacy policy permits the fallback provider.

Wildstone does not automatically replace an unavailable Agent with another Agent.

For an Agent served by an external runtime (ADR-W039), the model chain is the runtime's own configuration. Wildstone records the Owner-declared processing class for that runtime and applies it as a disclosure filter: `local_only` content is withheld from any runtime not declared local-only.

---

# 8. Connections

## 8.1 Connection abstraction

Every interaction channel is a first-class `Connection`:

- Homepage
- Telegram
- Discord
- Slack
- voice
- future transports

A Connection belongs to exactly one Agent.

An Agent may have many Connections, including multiple Connections of the same type.

Each Connection has its own:

- identity
- Agent binding
- credential/secret reference
- enabled state
- admission policy
- configuration
- disclosure/privacy classification

Disabling a Connection does not pause the Agent.

## 8.2 Homepage Connection

Homepage is a real Connection type, not a special bypass.

A Person using Homepage may select any admitted Member Agent and converse through that Agent's Homepage Connection.

Owner Agents do not appear on Homepage.

## 8.3 Admission

The admission schema supports subjects of type:

- Person
- Group

MVP uses Person admission. Group support exists as a seam for future group conversations and group-based admission.

Unknown/unbound external identities are refused before model inference.

**In a group or multi-recipient destination, only a message addressed to the bot reaches the Agent** (a mention or a reply; ADR-W033 B9.1). Everything else said in the room never leaves the adapter, and Keystone refuses an unaddressed group message as a backstop. The sender must still be a bound, admitted Person, and the Agent is told it is speaking in front of a shared audience. Owner Agents answer in direct messages only.

Person admission authenticates the **speaker**, not everyone who will receive the reply — "the sender is known" is not "the audience is known". So disclosure in a group is judged against the room, not the sender, and full group semantics (speaker-aware disclosure, per-destination policy) remain future work.

Owner-Agent Connections admit Owners only.

## 8.4 Connection privacy

Connections are disclosure boundaries.

The runtime must be able to reason about characteristics such as:

- local vs external transport
- encryption class
- provider logging/retention class
- trusted UI vs external messaging surface

The exact schema is implementation-specific; the policy effect is not.

---

# 9. Sessions and conversation continuity

A conversation session is keyed around:

```text
Person + Agent + session_id
```

not around Telegram/Discord/Homepage.

Each message records the Connection it arrived on and the Connection used for delivery.

If the same Person contacts the same Agent within a configurable recency threshold, and exactly one session is eligible to continue, that session continues even if the transport changed.

Outside the threshold, Wildstone starts a new session.

**Ambiguous continuation.** If more than one session is eligible — several active or recent Person+Agent sessions inside the threshold — Wildstone does not pick one and does not merge them. It asks the Person which conversation to continue, or waits for an explicit resume.

Until the Person chooses, no candidate session's context is loaded. Silently merging two conversations, or guessing between them, joins contexts the Person kept apart.

The disambiguation prompt identifies each candidate by something the Person can recognise — last activity time and a short subject — and offers starting a new session as an option. `/new` resolves it immediately without choosing.

**The whole candidate is disclosure, not just its subject.** A conversation held on a trusted Connection can be revealed by its subject, by its timestamp, by a neutral placeholder standing in a list, or simply by the list being one item longer. The promise that no candidate's *context* is loaded does not cover any of that: the candidate is what gets shown before anyone has chosen anything.

So before a candidate is shown on the current Connection, that Connection's disclosure policy (8.4) is evaluated against **each part of it separately**:

| Part | If not permitted here |
|---|---|
| Its **existence** | The candidate is **omitted entirely** |
| Its **subject** | Replaced with a neutral label — `Recent conversation`, `Conversation 1` |
| Its **timestamp / activity metadata** | Omitted |
| Any other identifying metadata | Omitted |

**Existence is the leg that has to be policy-dependent.** Checking only the subject leaves a protected session disclosed by the neutral label that replaces it — a placeholder in a list still says *there is a conversation here you are not being told about.*

**Count and ordering must not reveal what was omitted.** The prompt is built from the candidates that survived filtering, and it neither numbers around a gap nor preserves a position that implies one.

After filtering:

- **Exactly one safe candidate** — continue it, as an unambiguous continuation.
- **More than one** — ask the Person to choose among **those** candidates only.
- **None, while protected candidates exist** — **fail closed.** Direct the Person to a trusted Connection such as Homepage, revealing nothing about the protected sessions: not their number, not their subjects, not their timestamps, not that any particular one exists. The redirect is generic, and it reads the same whether one protected session was hidden or five.

The case this exists for: sessions begun on Homepage, then an ambiguous continuation arriving over Telegram. Safe where they were written is not safe where they would now be shown.

No candidate's context is loaded before the Person chooses, as above. That rule is unchanged; this one covers what is shown while they decide.

`/new` forces a new session immediately.

Additional commands may support listing/resuming sessions, status, or help.

The most recent accepted inbound Connection becomes the default reply path.

If that Connection is unavailable, cross-Connection fallback occurs only when explicitly configured. No fallback is enabled by default.

---

# 10. Authority

Authority is decided in **three layers, in order**. Every rule in this document sits in exactly one of them, and a later layer never widens an earlier one.

| | Layer | Asks |
|---|---|---|
| 1 | **General execution authority** | May this actor do work at all, and within what bounds? |
| 2 | **Resource-specific authority** | May it reach *this* memory record, *this* secret, *this* tool? |
| 3 | **Disclosure and audience policy** | May the result be shown to *this* audience, on *this* Connection? |

Layer 1 has three kinds of actor — an interactive **Person** (10.1), a **Task** derived from one (10.1), and an approved **routine version** running unattended (10.2). They are genuinely different principals and are not made to impersonate one another.

Layers 2 and 3 are defined by the resource: memory in section 11, secrets in section 12, Connections in 8.4 and 13. **The general rules below require those layers without enumerating them**, so a new resource adds its own grant semantics without needing this section rewritten — and, more importantly, without the general rule silently permitting what the specific rule forbids.

## 10.1 Interactive actions

For interactive work, effective authority is the intersection of:

```text
active Person authority, including applicable Group membership
∩ Agent/tool capability
∩ Connection admission/scope
∩ originating Task authority
∩ required resource-specific grants
∩ current privacy/disclosure policy
```

Missing identity fails closed.

**Task authority is derived, not granted.** A Task inherits the bounded authority of the request that created it, is re-evaluated at every authorization boundary, and can never widen beyond that originating authority. If the Person, session, Connection or grant behind it later loses authority, the Task loses it too at its next boundary (17.1).

There is no separate Task grant object to issue, hold or revoke. A Task is a *narrowing* of something that already existed.

Authority is evaluated at use time, not copied permanently into queued work.

No model, memory record, message, document, event, or tool result may widen authority.

## 10.2 Routine actions

An approved routine does not continually borrow the proposer Person's authority forever.

Instead, activation creates a bounded routine authorization tied to the exact approved version.

At execution time, the runtime intersects:

```text
exact routine-version authorization
∩ assigned Agent / capability authority
∩ required per-resource grants
∩ current service availability / capability
∩ output-audience / disclosure constraints
∩ current privacy and safety policy
```

The **per-resource grants** and **output-audience** legs are deliberately named rather than enumerated: memory defines what a routine-version memory grant means (11.8), secrets define what a routine-version `use` grant means (12.4), and each adds its own disclosure constraint. The general rule requires them without knowing them.

### A routine is its own principal for secret use

A scheduled routine runs unattended: there is no active Person, and by the rule above it does not keep borrowing its proposer's authority. Household-secret authorization is granted to Person and Group principals (12.4), so without a third kind of principal every routine needing a secret would either fail that intersection or have to impersonate a proposer indefinitely. Both are wrong, and the second is worse for being invisible.

**An approved routine is a principal in its own right, bounded to its exact approved version.** A Household secret can grant `use` to that routine version, the same way it grants to a Person or a Group.

Effective secret `use` for an unattended run intersects:

```text
exact-version routine authorization
∩ the routine/version secret-use grant
∩ assigned Agent / tool capability
∩ current service capability
∩ current privacy and safety policy
```

Any change to executable behaviour **or to the secret grants a routine requires** creates a new version and needs Owner reapproval (14.4). A routine cannot acquire a secret it was not approved with by being edited around it.

This covers `use` only. **Raw-value reveal to a human is unchanged** (12.5): a routine resolves a secret below inference to reach a service, and never reveals one.

An Owner-approved Agent **subscription** version (ADR-W039 §3.6) is the same kind of unattended principal: every rule in this section that names an approved routine version applies to it identically.

## 10.3 Surface authority

Roles primarily determine which plane a Person can reach:

- Owner: Homepage + services + Console
- Member: Homepage + entitled services

Actual tool/service capability remains separately granted.

---

# 11. Memory

## 11.1 Four scopes

Wildstone distinguishes four memory scopes:

1. Session context
2. Agent-local memory
3. Personal memory
4. Household memory

They may share infrastructure but do not share ownership semantics.

## 11.2 Session context

Session context is transient conversation/task state.

It is not automatically durable memory.

**Session context is scoped to the Person in the `Person + Agent + session_id` key.** It may only be retrieved or assembled for that Person, and session continuity never changes that audience — continuing a session across Connections carries the same scope with it, and a shared Agent does not pool session context across the people it serves.

## 11.3 Personal memory

Personal memory belongs to one Person.

When a Person explicitly says "remember this," that instruction is sufficient authorization to create a Personal memory record.

Agents may also autonomously extract and save relevant Personal memory from normal conversation because the resulting memory remains scoped to that Person.

**Retrieval binds to the Person on the current request.** One Agent serves several admitted people, and its admissions must never combine into an Agent-wide pool. Retrieval and prompt assembly resolve Personal memory for the **active** Person only; the Agent's other admissions are not in scope for this request, and the union of what an Agent may reach across everyone it serves is not a set anything is ever evaluated against.

Without this, an Agent admitted by two people would answer one of them out of the other's memory, and every rule above would still read as satisfied.

The Person can inspect, correct, forget, export, or restrict it.

When a Person is admitted to use an Agent, that Agent receives access to that Person's Personal memory by default. This access may be explicitly restricted.

## 11.4 Household memory

Household memory is intentionally shared household knowledge.

Agents do not autonomously promote Personal memory into Household memory.

An Agent may propose promotion, but the Person whose Personal memory it is must approve that promotion.

Member Agents receive Household-memory access by default, subject to explicit restriction and per-record privacy policy.

**Agent-level access is eligibility to query the household namespace. It is not authority to disclose every household record to every Person admitted to that Agent.**

Retrieval and prompt assembly evaluate **each candidate record against the active Person on the current request**, including that record's audience and privacy policy. A record whose audience excludes the active Person is excluded from retrieval and from assembled context, whatever the Agent itself may reach.

This matters most where it is easiest to miss: one Member Agent shared by an adult and a child. The Agent is eligible for Household memory; the child is not in an adults-only record's audience. Without the active-Person check, the Agent's eligibility would be doing the child's authorization for them.

The per-Agent restriction is unchanged and is a *second* limit, not a substitute: an Agent can be restricted below what its people could otherwise see, and it can never be widened above them.

## 11.5 Agent-local memory

Each Agent may autonomously create its own durable local memory.

Agent-local memory is isolated from other Agents initially.

Other Agents should normally learn common durable facts through Personal or Household memory rather than direct access to another Agent's local store.

**Isolation from other Agents is not isolation between people.** One Agent serves several people, so an Agent-local record learned from Person A sits inside the same store that answers Person B. Cross-Agent isolation says nothing about that, and it is the more likely leak.

So: **a derived Agent-local record inherits the strictest audience and privacy constraints of its sources, and may never widen them.** Material learned from Person A's private conversation carries Person A's constraints into the Agent's own store.

On a shared Agent, retrieval and prompt assembly evaluate the active Person against those **inherited** constraints, exactly as 11.4 does for Household records. An Agent-local record is not a way to launder a source's audience by copying it somewhere with no audience of its own.

At Agent deletion an Owner may:

- delete Agent-local memory
- export it
- transfer it to another Agent
- promote selected records

Transfers preserve provenance.

## 11.6 Administrative inspection

Owners operate the self-hosted server and may administratively inspect stored memory.

That does not make them the normal-product owner of another Person's Personal memory and does not grant them silent promotion rights.

## 11.7 Memory record requirements

Durable memory records should preserve enough structure for:

- subject
- scope/owner
- content
- kind
- audience
- provenance
- stated vs inferred confidence
- lifecycle status
- sensitivity/privacy policy

Corrections supersede instead of silently rewriting history.

Derivatives inherit the strictest privacy/disclosure requirements of their sources.

## 11.8 Retrieval

Retrieval combines keyword/exact and semantic candidates under the same authorization/privacy filtering.

**Filtering is evaluated for the active Person on the current request, in every scope.** Personal memory resolves to that Person (11.3); Household records are checked against that Person's audience (11.4). An Agent's own reach is never the authorization.

### Unattended routines have no active Person

A scheduled routine runs with nobody present (10.2), so the rule above has no subject to evaluate. The answer is not to borrow one: **an approved routine version is a bounded principal for memory retrieval**, exactly as it is for Household-secret `use` (10.2). It does not inherit or impersonate its proposer, its approver, or any other Person.

Effective unattended memory access intersects:

```text
exact routine-version authorization
∩ exact routine-version memory grant
∩ record audience / privacy policy
∩ assigned Agent eligibility
∩ approved routine output audience
∩ current privacy and safety policy
```

The **output audience** leg is what makes this safe rather than merely well-typed. A routine reads in order to deliver, so what it may read is bounded by who will read its output.

Per scope:

- **Household memory** — a routine version may be granted access, and each record must still satisfy its own audience and the routine's approved output audience. An adults-only record must not feed a routine that delivers to a Child audience.
- **Agent-local memory** — a routine may use records its assigned Agent can reach only where the record's own or inherited restrictions (11.5) permit the routine's approved output audience. Source inheritance is not bypassed by running unattended.
- **Personal memory** — **Owner approval of a routine does not grant another Person's Personal memory.** Use requires a grant rooted in the owning Person, or in some future policy that legitimately represents them. *Owner approval authorizes the work; it does not transfer ownership of the knowledge the work may consume.*

Personal-memory-backed unattended routines are not prohibited architecturally. For MVP they may simply require grant support that does not exist yet, and so not work until it does.

Any change to a routine's executable behaviour, its **output audience**, or its required memory grants creates a new version needing Owner reapproval (14.4). Personal-memory authorization still comes from the owning Person, never from that reapproval.

Filtering occurs before unauthorized content is assembled into prompt context.

Destination disclosure is checked again before delivery.

Live applications remain authoritative for live state.

## 11.9 Forgetting

Forget/delete operations must remove/suppress active memory and derived representations and must survive restore from backup through durable deletion journaling.

---

# 12. Sensitive memory and secrets

## 12.1 Sensitive semantic memory

Sensitive facts that Agents need to reason about stay in memory rather than becoming opaque secrets.

A record may carry policy such as:

```text
sensitivity: protected
cloud_processing: local_only
external_delivery: restricted
```

Processing provenance records whether the information was previously handled by local or cloud inference.

If required privacy cannot be satisfied, execution fails loudly.

## 12.2 Secret store

Secret values live in a separate household/user secret domain rather than normal memory.

The existing infrastructure/node secret store keeps its own semantics and is not weakened to provide household secret reveal.

Secret scopes include at least Personal and Household, and they do not share authorization rules.

**A Personal secret belongs to exactly one Person.** Its audience is that Person alone. **No other Person may use or reveal its raw value, including an Owner acting through normal product authority.**

**Secret permissions** are product-level:

- **use** — **interactive only.** The owning Person is active on the request, through an Agent they have admitted, holding an explicit grant for that secret. Admission alone grants nothing, and neither does the grant without the Person (below).
- **manage** — create, edit, grant, revoke, and control reveal. That Person only.
- **reveal** — surface the raw value to a person. See the MVP boundary below.

**Administrative lifecycle operations are a different thing, and must not be confused with `manage`.** An Owner runs the household's server, so an Owner can quarantine, disable, delete, and resolve ownership during offboarding. Those are operator actions on the *object*.

They **never reveal the raw value, and never confer secret `manage` permission.** An Owner can make a Personal secret stop existing; an Owner cannot read it, grant it, or decide who else may. This is the same boundary 11.6 draws for Personal memory: administration is not ownership.

**MVP boundary.** Members may propose Personal secrets and use explicitly authorized Personal secrets through tools. Direct Personal-secret create, edit and delete controls are deferred (12.6), and so is **raw reveal**: MVP exposes no Personal-secret reveal path at all, and one exists only if a separately approved trusted reveal surface is added later. Household-secret reveal is unaffected and works as 12.5 describes.

#### Unattended routines cannot use Personal secrets. They fail closed.

A Person's grant to an Agent authorizes **that Person's own interactive use** through that Agent. It is not a standing capability the Agent carries into work the Person is not part of.

So when a routine runs with no active Person (10.2), **a Personal secret does not resolve.** The attempt is denied rather than approximated.

**Nothing substitutes for the absent owning Person** — not the Agent's grant, not Owner approval of the routine, not the routine's own approval or version, not a shared-Agent relationship, not the proposer's identity, and not the approver's. The list is exhaustive on purpose: each entry is a plausible-looking stand-in, and the whole point is that a Personal secret has exactly one legitimate authority and they are not there.

The case this forecloses: Person A grants a shared Agent use of A's Personal secret; an Owner approves an unattended routine assigned to that same Agent; the routine resolves A's secret on A's behalf without A ever being involved.

**Household secrets remain the supported secret type for unattended routines**, through the exact-version routine principal (12.4). A routine that needs a credential uses a Household one.

This is an MVP boundary, not a permanent prohibition. Personal-secret use by routines would need explicit Person-authorized exact-version delegation, with its own revocation, audit and interface semantics — **and none of that is designed here.** Until it is, the answer is no.

Revoking a Person invalidates their outstanding secret references immediately (17.1) and leaves the secrets themselves for explicit resolution rather than silent destruction.

A **Household** secret is the shared case, and the management and reveal rules below apply to it.

### Slice 15 intake clarification

A Person may securely submit their own Personal secret and explicitly authorize Agent use without household Owner review. This narrow intake flow does not add direct Personal editing, raw reveal, or authority for another Person. An explicit Household submission is a separate copy whose value the submitter authorizes an Owner to inspect through protected Console review before approval; it never transfers or exposes the Personal original. This clarifies the proposal/approval wording in 12.6 for slice 15.

## 12.3 Model exposure

Models receive opaque secret references rather than raw values by default.

Trusted runtime/tool code resolves the value below inference after rechecking identity, permission, destination, and validity.

## 12.4 Secret permissions

At least three permissions exist conceptually:

- use
- manage
- reveal

A Member Agent may use an authorized secret through a trusted tool without seeing the raw value.

### Household-secret authorization is principal/object, not an Agent-wide boolean

A Household secret grants its permissions to **explicit principals**. An Agent grant makes that Agent *technically capable* of using the secret through a tool; it never substitutes for authorizing the caller.

There are **two kinds of principal, because there are two kinds of execution**, and this general rule recognises both. A routine is not an exception carved out of it.

**Interactive — the principal is the active Person**, directly or through Group membership:

```text
active Person / Group
      ∩ the Agent/tool capability
      ∩ the secret's own permission grant
      ∩ the trusted Connection / reveal-surface policy
```

**Unattended — the principal is the exact approved routine version** (10.2). No active Person is required, and the runtime never borrows or impersonates the proposer or approver. This path is for **Household secrets only**; Personal secrets fail closed when no Person is active (12.2):

```text
exact approved routine-version principal
      ∩ the Agent/tool capability
      ∩ a Household-secret `use` grant held by that exact routine version
      ∩ current service capability
      ∩ current privacy and safety policy
```

Three constraints hold across both:

- A routine principal gets **`use` only** — never human raw-value `reveal`.
- **`manage` remains Owner-controlled** for Household secrets.
- Changing a routine's executable behaviour or its required secret grants creates a new version needing Owner reapproval (14.4).

Stating only the interactive form would put this section in contradiction with 10.2, and a literal implementation would have to either refuse every unattended secret-backed routine or bypass this section — which is how a bypass quietly becomes the real authorization rule.

The case this exists for: a Member Agent admitted by two people. Person B must not use or reveal a secret granted only to Person A, or only to an adults-only group, merely because the Agent they share holds a grant. Treating an Agent-level grant as reveal authorization for every caller on Homepage is exactly the reading this forecloses.

Reveal surfaces are unchanged (12.5): Member Agent through trusted Homepage, Owner Agent through trusted Console. This decides **who** may reveal, not **where**.

## 12.5 Reveal surfaces

Owner Agents are reachable through Console, not Homepage. Authorized secret reveal for an Owner Agent occurs through trusted Console UI/chat.

Member Agents may reveal authorized Household-shared secrets through trusted Homepage.

Both of those are **Household** secrets. There is no Personal-secret reveal path in MVP, through either surface (12.2).

Raw values should be rendered by trusted UI/runtime code as protected components rather than becoming normal model-authored transcript text whenever possible.

External messaging Connections are not reveal surfaces for raw secrets.

## 12.6 MVP management scope

MVP may intentionally be stricter than the long-term model.

MVP behavior:

- Members can propose secrets.
- Members/Member Agents can use authorized secrets through tools.
- Members can reveal authorized Household-shared secrets through trusted Homepage.
- Owners manage Household secrets and approve proposals.
- Owners reveal through trusted Console — **Household secrets only.**
- No Personal secret is revealed to anyone, through any surface.
- Owners may run administrative lifecycle operations on a Personal secret — quarantine, disable, delete, offboarding resolution — without reading it and without gaining `manage` over it (12.2).

Future work may allow Members to directly manage their own Personal secrets, and may add a separately approved trusted surface through which a Person can reveal their own.

## 12.7 External disclosure

If a Person types a secret into an external chat, the secret is already disclosed to that platform.

Wildstone warns, stops repeating/using the value in normal chat, offers a secret-storage proposal, and records disclosure provenance.

---

# 13. Privacy policy

Privacy has two independent dimensions:

1. **Processing** — which model/provider may process the content.
2. **Delivery** — which Connection may carry the content.

Examples:

- local model + Telegram can still fail delivery privacy
- cloud model + Homepage can still fail processing privacy

No privacy requirement is silently weakened to keep work moving.

---

# 14. Plans and Owner review

## 14.1 Plan primitive

Durable/consequential proposals are versioned `Plan` objects.

A Plan carries at least:

- proposer Person
- proposing Agent
- version
- requested capabilities/tools
- targets/parameters
- trigger/schedule where relevant
- delivery behavior
- approval requirement
- state

States include at least:

```text
draft
submitted
awaiting_review
approved
rejected
superseded
```

MVP Plans remain pending indefinitely until resolved.

## 14.2 Proposal authority

Member Agents may propose Member-class Plans.

Owner Agents may propose Member- or Owner-class Plans.

Agents cannot approve their own Plans.

A human Owner approves activation.

## 14.3 Owner Review Queue

Every submitted Plan enters a persistent Owner Review Queue visible to all Owners.

Notifications follow a designated Owner/reviewer preference. For example, Alfred may message the designated Owner over Telegram that a Plan is waiting.

The notifying Agent/Connection is only the review interface. The human Owner is the approver.

## 14.4 Version binding

Approval binds the exact Plan version.

Editing executable behavior creates a new version and requires a separate explicit approval action.

---

# 15. Routines

## 15.1 Household ownership

An approved routine is owned by the Household.

It records provenance such as proposer Person, proposing Agent, approving Owner, and assigned Agent, but its continued existence does not depend on those actors remaining present.

## 15.2 Activation authorization

Owner approval creates bounded routine authorization over the exact version, including:

- tools
- targets
- parameters/limits
- schedule/trigger
- **output audience**
- delivery behavior
- assigned Agent
- required per-resource grants (memory, secrets)

**Output audience and delivery behaviour are different things**, and the distinction is load-bearing:

- **Output audience** — *who may receive or learn from the result.* This is an authorization input: it bounds what the routine may read (11.8), because a routine reads in order to deliver.
- **Delivery behaviour** — *where and how* the result is delivered: the Connection, the destination, what happens when nobody responds.

A routine can have a correct delivery destination and a wrong output audience, which is why the audience has to be approved rather than inferred from the destination.

Normal runs do not ask for repeated approval.

An Owner-approved Agent subscription version (ADR-W039 §3.6) is treated exactly like an approved routine version: exact-version Owner approval, reapproval on any edit, its own unattended principal, and any Owner may stop it (15.6).

## 15.3 Tool ceiling

Member-class routines contain only Member tools.

Owner Agents may propose routines that use Owner tools, but activation still requires human Owner approval.

## 15.4 Deterministic vs inference work

The scheduler/routine engine owns routine execution.

Deterministic steps do not require an Agent to remain online.

If the assigned Agent is unavailable:

- deterministic steps continue
- inference steps block/fail loudly
- no automatic Agent substitution occurs

Changing the assigned Agent requires a new approved version.

## 15.5 Capability loss and recovery

If a required service/tool disappears, the routine becomes `blocked` and preserves its definition.

If the exact compatible capability returns, a routine blocked only for that reason automatically returns to service and the transition is recorded.

## 15.6 Stopping a routine

Everything above makes an approved routine durable: it runs without per-run approval, it outlives its proposer and its approving Owner, and only a missing capability blocks it. Durability without a stop is a routine nobody can turn off.

**Any Owner can stop a routine immediately, without a Plan and without approval.** Stopping is never gated on the ceremony that starting required. An Owner who has decided a recurring consequential effect must end does not wait for a review queue.

Routine authorization states:

```text
active      approved and running
blocked     a required capability is missing (15.5)
disabled    an Owner stopped it; the authorization is preserved
revoked     the authorization is withdrawn
```

- **Disable** stops future runs and preserves the bounded authorization, so an Owner can re-enable without a new approval.
- **Revoke** withdraws the authorization. Returning to service requires a new approved version, exactly as a first activation would.
- **Soft deletion ends execution authority at the soft delete, not at the purge.** A soft-deleted routine does not run while it sits recoverable (17.4).

**In-flight runs stop at their next boundary** — the same semantics as 17.1: before delivery, before the next model round, before a retry, before prepared context reaches a prompt. A run already past a boundary may have had an effect, which is recorded rather than assumed away.

**A household-wide stop halts all routine execution at once**, for the case where an Owner needs everything to stop now and will work out which one was the problem afterwards.

---

# 16. Tasks, turns, and evidence

A Task is the user-recognizable unit of work and may span multiple turns.

Each turn owns a fresh ledger of readings, attempts, tool results, and evidence.

A later turn does not silently reuse a prior turn's stale readings.

Budgets, overall status, and uncertain outcome belong to the Task.

Completion requires evidence from the execution path; model narration alone is not proof.

For consequential external effects:

1. use idempotency/deduplication where available
2. after timeout, inspect external outcome before retry
3. if confirmation is unavailable, record `uncertain`
4. do not blindly repeat the effect

Reusable learned procedures are structured, versioned definitions and follow the same Plan/approval path as routines.

---

# 17. Lifecycle and deletion

## 17.1 Person lifecycle

### Demotion is not offboarding

**Owner → Member demotion recomputes authority downward. It does not run the full revocation below.** A demoted Owner is still a Member of this household, and treating demotion as offboarding would disable the identity that remains.

Revoked immediately on demotion:

- Console access and elevated/active Owner sessions (3.2)
- Owner-only admissions and permissions
- pending Owner-bound approvals and prepared authorization artifacts

Preserved:

- the Person's valid Member identity
- Homepage and entitled-service access
- Member-Agent admissions
- their Personal memory
- every other Member-level authority

**Full Person-wide revocation is for disable, delete and offboard** — the rest of this section. Demotion borrows its *mechanism* (recompute now, recheck at every boundary) without borrowing its *scope*.

### Disable, delete, offboard

Revoking, disabling, or deleting a Person immediately removes active authority:

- revoke sessions
- remove Console access as applicable
- update directory/group membership
- revoke Agent admission
- invalidate Person-bound approvals that have not yet been consumed
- deny new actions
- stop queued and in-flight work at its next boundary

**Queued and in-flight work.** "Deny new actions" is not enough on its own: the word doing the work is *new*. At the moment of revocation there is already work that was authorized when it started — a reply sitting in a delivery queue, a model turn mid-flight, a retry about to fire, prepared private context assembled for a prompt.

Each of these **rechecks authority at its next boundary and fails there**: before delivery, before the next model round, before a retry, before prepared context reaches a prompt. A delivery that fails this way reports the failure without disclosing what it was carrying.

Revocation is prompt because it sweeps; it is *correct* because everything rechecks anyway. Neither half replaces the other, and a sweep that missed something must still fail closed.

**What "invalidate approvals" reaches.** Revocation invalidates authorization still bound to that Person: pending and unconsumed approvals, queued Plans awaiting their review, approval sessions, and prepared authorization artifacts such as issued secret references.

It does **not** invalidate an already-materialized Household routine authorization that was validly approved before the revocation. That authorization belongs to the Household (15.1), not to the Person who proposed or approved it, and an Owner leaving must not silently disable the household's durable routines.

One exception, resolved rather than assumed: a routine that **delivers to** the revoked Person, or that depends on an admission only that Person held, is paused for Owner resolution. It is not deleted, and it does not keep delivering to someone who has left.

Durable shared resources are resolved explicitly rather than silently destroyed.

Before final purge, Wildstone inventories associated resources and allows Owner choices such as transfer, retain, export, retire, block, or delete.

## 17.2 Agent lifecycle

Agents support at least:

```text
active
paused
retired
pending_deletion
soft_deleted
purged
```

A retired Agent remains inspectable in Console and may have history/config/memory exported, but cannot execute.

Deleting an Agent resolves:

- Agent-local memory
- assigned routines
- Connections
- outstanding work

Personal and Household memory survive Agent deletion.

## 17.3 Service/package lifecycle

Removing a package/service does not erase its logical assignments.

Service groups, user assignments, Agent grants, and routine definitions become dormant/unavailable.

Reinstalling the same compatible capability can reactivate those bindings automatically.

## 17.4 Soft deletion

Durable resources are soft-deleted by default and remain recoverable until an Owner explicitly performs final purge.

MVP requires no automatic purge timer.

---

# 18. Acceptance criteria

These are end-to-end design gates. Implementations may add more tests, but must not weaken these.

## 18.1 Identity and access

- A fresh install cannot complete without one active Owner.
- A Member cannot access Console.
- Demoting an Owner immediately invalidates Console access/session.
- The last active Owner cannot be demoted, disabled, or deleted.
- Root CLI recovery can reset an Owner password or create a new Owner.

- A Task never exceeds the authority of the request that created it, and loses authority at its next boundary when the originating Person, session, Connection or grant does.
- Groups such as `Adults` resolve to the same people whether referenced by a service entitlement, a memory audience, or a secret grant.
- Membership of a group does not by itself grant every service, memory record or secret that group appears in.

## 18.2 Service entitlements

- A new Member inherits the default access group.
- A direct URL to an unauthorized service is denied regardless of Homepage card visibility.
- Removing a service makes entitlements dormant rather than erasing them.
- Reinstalling the same compatible service restores dormant assignments.

## 18.3 Agents and tools

- A Member Agent cannot be configured with an Owner tool.
- A new Owner Agent gets eligible Member + Owner tools except opt-in tools.
- Downgrading Owner Agent -> Member strips tool grants.
- Upgrading Member Agent -> Owner does not auto-add Owner tools.
- A dynamic service/class grant absorbs new matching non-opt-in tools but not higher-class or opt-in tools.

## 18.4 Connections and sessions

- An unknown external sender is refused before inference.
- A Member cannot reach an Owner Agent even if an admission configuration is malformed.
- Same Person + same Agent can continue a recent session across Connections.
- Multiple eligible recent sessions produce a disambiguation prompt, never a silent merge or a guess.
- No candidate session's context is loaded before the Person chooses.
- A session begun on trusted Homepage with a protected subject, continued ambiguously from an external Connection, has that subject suppressed or replaced with a neutral label — while the Person still receives enough non-sensitive metadata (such as a timestamp) to pick the session.
- A candidate whose **timestamp** is not disclosable on the current Connection is listed without it.
- A candidate whose **existence** is not disclosable on the current Connection is **omitted entirely** — not shown under a neutral label — and the remaining candidates' count and ordering do not reveal that anything was removed.
- Where **no** candidate is disclosable on the current Connection and protected candidates exist, the Person receives only a generic redirect to a trusted surface: no count, no ordering, no subjects, no timestamps, and no indication of how many sessions were withheld.
- `/new` starts a distinct session.
- Cross-Connection delivery fallback occurs only when explicitly configured.
- Homepage exposes admitted Member Agents and never Owner Agents.
- In a group or multi-recipient destination, only a message that addresses the bot reaches a Member Agent; an Owner Agent answers in direct messages only (ADR-W033 B9.1).
- No Personal or Household memory reaches a group destination in MVP.

## 18.5 Memory

- Agent-local memory is unreadable by other Agents by default.
- Two people share one Member Agent: an Agent-local record derived from Person A's private material is excluded from retrieval **and** assembled context for Person B, because it inherited Person A's constraints.
- A derived Agent-local record never carries a wider audience than the strictest of its sources.
- Session context is returned only for the Person in its `Person + Agent + session_id` key, including after the session continues on a different Connection, and is never pooled across the people a shared Agent serves.
- An unattended Household-memory-backed routine retrieves only records authorized to that exact routine version **and** permitted by its approved output audience.
- An adults-only Household record is excluded from a routine whose approved output audience includes a Child/Kids audience.
- Agent-local records retain their inherited source restrictions during unattended routine retrieval, exactly as in an attended request.
- A routine cannot reach Person A's Personal memory merely because an Owner approved the routine.
- A routine never impersonates, nor indefinitely borrows, its proposer's or approver's identity for memory access.
- A Person's admitted Agent can use that Person's Personal memory by default.
- An Agent admitted by two people, asked by Person B, returns nothing from Person A's Personal memory — in retrieval and in assembled prompt context.
- Member Agents can use Household memory by default unless restricted.
- One shared Member Agent, two people with different audiences (for example Adult and Child): a restricted Household record is excluded from retrieval **and** from assembled context for the Person its audience does not include.
- Per-Agent Household-memory restriction still applies on top of the active-Person check, and never widens it.
- Agents may autonomously create Agent-local and Personal memories.
- Personal -> Household promotion requires approval from the Person whose memory it is.
- Deleting an Agent can transfer Agent-local memory to another Agent with provenance preserved.
- Forget/delete remains effective after backup restore.

## 18.6 Secrets and privacy

- Models receive references rather than raw secret values by default.
- An Agent may use a secret through a tool without reveal authority.
- Two people share one Member Agent: Person B cannot **use** a Household secret granted only to Person A or to an adults-only group, despite the shared Agent holding a grant.
- Same pair: Person B cannot **reveal** that secret on Homepage. An Agent-level grant is not caller authorization.
- `manage` on a Household secret is Owner-controlled.
- An unattended routine satisfies the **general** Household-secret rule (12.4) through its exact-version principal, not through a routine-specific exemption from it.
- An unattended routine cannot use a Household secret unless **that exact routine version** holds a `use` grant for it — a grant to the proposer, the approver, the assigned Agent, or an earlier version of the same routine is not enough.
- A routine principal is never accepted for raw-value `reveal`.
- Person A grants a shared Agent interactive use of A's Personal secret; an Owner approves an unattended routine assigned to that same Agent; the routine's attempt to resolve A's Personal secret is **denied**, despite the Agent's grant.
- No Agent grant, Owner approval, routine approval or version, shared-Agent relationship, proposer identity or approver identity permits unattended Personal-secret use.
- An unattended routine run resolves only the Household secrets granted to that exact routine version, and no others — without impersonating or borrowing its proposer's or approver's identity.
- Changing a routine's required secret grants creates a new version requiring Owner reapproval; the old version's grants do not carry over.
- One Person's Personal secret is not used, revealed or managed for another Person, including by an Owner acting through normal product authority.
- An Owner's administrative lifecycle operations on a Personal secret — quarantine, disable, delete, offboarding resolution — complete without revealing the raw value and without granting that Owner `manage` over it.
- MVP exposes no Personal-secret reveal path: no surface, Homepage or Console, returns a Personal secret's raw value to anyone.
- A Member Agent can reveal an authorized Household secret only through trusted Homepage behavior.
- An Owner Agent reveals through trusted Console behavior.
- External secret disclosure triggers warning/provenance handling and the value is not repeatedly echoed in normal chat.
- `local_only` content never silently falls back to cloud.
- Content forbidden on an external Connection is blocked even when inference was local.

## 18.7 Plans and routines

- Agents can propose Plans but cannot activate them.
- Every submitted Plan enters the Owner Review Queue.
- Notifications follow the designated Owner review preference.
- Editing creates a new version that requires explicit approval.
- Approval activates only the exact reviewed version.
- An approved routine runs repeatedly without per-run approval.
- A routine survives removal of its proposer.
- A routine survives removal of its approving Owner, including when that Owner was the last to approve it.
- Revoking a Person invalidates their pending approvals, queued Plans and approval sessions, and leaves already-approved Household routine authorization intact.
- A routine that delivers to a revoked Person is paused for Owner resolution rather than continuing or being deleted.
- Deterministic routine steps continue when the assigned Agent is unavailable.
- Inference-dependent steps fail/block visibly when the Agent is unavailable.
- Reassigning an Agent requires reapproval.
- A routine's approved version records its **output audience** distinctly from its delivery behaviour, and changing either requires reapproval.
- A literal implementation of the general routine rule (10.2) requires the per-resource grants and output-audience constraints that sections 11 and 12 define, rather than permitting what those sections forbid.
- Missing capabilities block routines without deleting them.
- Any Owner can disable or revoke an approved routine immediately, with no Plan and no approval step.
- A disabled routine stops running and can be re-enabled without a new approval; a revoked one needs a new approved version.
- A run in flight when a routine is stopped halts at its next boundary, and an effect already committed is recorded rather than assumed not to have happened.
- A soft-deleted routine does not execute while it remains recoverable.
- A household-wide stop halts all routine execution.
- Restoration of the same compatible capability automatically returns eligible routines to service.

## 18.8 Lifecycle

- Owner → Member demotion revokes Console access, elevated Owner sessions, Owner-only admissions and pending Owner-bound approvals, while the Person keeps Homepage and entitled-service access, Member-Agent admissions, and their Personal memory.
- Demotion does not revoke Member-level authority, and does not run the offboarding path.
- Offboarding immediately removes active authority.
- Work queued or in flight at the moment of revocation fails at its next boundary: a queued delivery is not delivered, an active turn does not continue into another model round, a retry does not fire, and prepared private context does not reach a prompt.
- A delivery stopped this way reports the failure without disclosing the payload.
- Shared/durable resources are not silently destroyed during offboarding.
- Retired Agents remain inspectable but cannot execute.
- Soft-deleted resources remain until explicit Owner purge.

---

# 19. Implementation dependency order

Detailed implementation sequencing belongs in issues/planning. The architectural dependency order is:

1. Human identity and surfaces
2. Service entitlements
3. Agent classes and tools
4. Connections and sessions
5. Memory
6. Secrets and privacy
7. Plans and Owner Review Queue
8. Routine execution
9. Lifecycle and soft deletion
10. Audit remediation, migrations, and rollout

---

# 20. Deliberately open implementation details

The design does not freeze:

- the numeric session-recency timeout
- exact slash-command names beyond the need to force a new session
- exact Connection privacy field schema
- exact model/provider products
- exact embedding/vector implementation
- exact numeric work budgets
- exact UI layout for Plan review
- exact UI layout for protected secret reveal
- exact soft-delete purge workflow

Implementations may choose these details as long as they preserve the boundaries and acceptance criteria above.
