// Package personref records which database columns name a HUMAN.
//
// Slice 2b (#203) repoints every human reference at person_id. Deciding
// which columns those are turned out to be the hard part, and the wrong
// answer is unrecoverable in both directions: repoint a column that can
// hold a machine principal and the migration aborts on every real
// deployment; miss a column that names a person and a renamed handle
// silently orphans its rows.
//
// So the verdicts live here, in code, with the evidence attached — and
// a test reconciles this table against the live schema, so a column
// added later cannot quietly arrive without one.
//
// # The two families
//
// A survey of the live schema (72 tables) found 86 columns whose name
// could mean a person. They are not one kind of thing.
//
// Columns that take a person: the value arrives as a parameter named
// `member`, or as a struct field the schema documents as a person —
// "author is the person, never the Butler", "member scopes a PERSONAL
// credential", "owner is the person the record belongs to". These are
// Human, and 2b repoints them.
//
// Columns that take an ACTOR: every `_by` and `actor` column is fed by
// api.actingMember (60 call sites) or by id.Name (22 more), which
// yields the person's handle for a Console caller and a machine
// principal name — root-admin, op, the butler principal, a chat-*
// adapter — for everything else. The column does not mean "a human"; it
// means "whoever or whatever did this". Repointing it would require
// either inventing a Person for every machine principal, which #203
// forbids outright, or a polymorphic actor representation, which is a
// decision epic #201 defers to the Agent-class slice (#210). They stay
// as recorded text, and this package says so out loud rather than
// leaving the next reader to rediscover it.
//
// # Where a person column can still hold a principal
//
// Five columns take a person even though the legacy Butler ask path can
// write a principal name into them: butler_memory.owner and .author,
// butler_tasks.author and .owner, and butler_index.owner. (The two
// owner columns are Scoped rather than Human, because they also take
// the `household` scope — a separate matter from this one.)
// handlers_butler.go builds the turn with Member: id.Name, and
// butlerlive writes that straight through (#240).
//
// They are NOT demoted to actor, and the repository owner has decided
// the gate blocks on them rather than migrating them with a null owner.
// The reasoning, recorded here so it is not re-argued every review:
//
// An operator filing feedback or requesting a capability is a coherent
// recording, so those columns are actor. A memory record OWNED by
// root-admin is not. The schema defines owner as "who may correct it,
// forget it, and export it"; a principal can do none of those,
// offboarding cannot find the row, and nothing will ever resolve it.
// The row has no owner in the only sense the column means.
//
// So the survey reporting those values as blocking is the correct
// signal, not a false alarm, and a deployment that used the legacy path
// resolves them before migrating. The alternative — migrating with a
// null owner — would weaken slice 2b from "every human reference
// resolves" to "every resolvable one", and leave the unresolvable rows
// in place indefinitely.
//
// #240 fixes the write path so the set stops growing.
//
// # Why names are not enough
//
// Two columns caught by the name heuristic are not people at all:
// butler_memory.superseded_by is an INTEGER pointing at the record that
// corrected this one, and household_events.subject is documented
// "member / device / household id". A name-based list would have
// carried both into the migration.
package personref

// Kind is what a candidate column turned out to mean.
type Kind string

const (
	// Human names a person and nothing else. Slice 2b repoints these.
	Human Kind = "human"
	// Actor names whoever acted — a person on one call path, a machine
	// principal on another. Out of scope until the Person/principal
	// split (#210); the recorded text stays.
	Actor Kind = "actor"
	// Scoped names a Person in one ownership mode and a well-known
	// non-person scope in another — butler_memory.owner is a Person for
	// a private record and the literal `household` for a shared one.
	// 2b repoints the person-owned rows and leaves the scoped ones
	// alone, which is a decision it can only make per row.
	Scoped Kind = "scoped"
	// NotAPerson is a false positive of the name heuristic.
	NotAPerson Kind = "not-a-person"
)

// Column is one verdict, with the evidence that produced it. Why is not
// decoration: it is what a reviewer checks instead of re-deriving the
// classification, and what tells a later reader whether a changed write
// path has invalidated the verdict.
type Column struct {
	Table  string
	Column string
	Kind   Kind
	Why    string
}

// Columns is the whole classification. Every candidate column in the
// live schema must appear here — TestEveryCandidateColumnHasAVerdict
// fails the build otherwise.
var Columns = []Column{
	// ---- Human: repointed by slice 2b ----
	{"butler_access", "member", Human,
		"GrantButlerAccess(butler, member, by) — member is the person admitted to a Butler; by is the actor"},
	{"butler_identity_links", "member", Human,
		"LinkSurfaceIdentity(linkType, externalID, member, createdBy) — already REFERENCES butler_members(member)"},
	{"butler_conversations", "member", Human,
		"the person whose conversation it is; GetButlerMember(conv.Member) resolves it in butlerhandoff.go"},
	{"butler_handoffs", "member", Human,
		"REFERENCES butler_members(member); GetButlerMember(member) validates it before the handoff"},
	{"butler_memory", "owner", Scoped,
		"schema, in full: `owner is the person the record belongs to ... For household-shared records it is the household`. A Person for a private record, the literal `household` for a shared one. Also reachable by the legacy ask path (#240)"},
	{"butler_memory", "author", Human,
		"the person who stated it, paired with owner on MemoryRecord. Same legacy-path exposure as owner (#240)"},
	{"butler_tasks", "author", Human,
		"schema: author is the member who asked for the work and never changes. Same legacy-path exposure (#240)"},
	{"butler_tasks", "owner", Human,
		"schema: owner is who operates it now, and offboarding moves it — a person by construction. Same legacy-path exposure (#240)"},
	{"butler_task_feedback", "author", Actor,
		"api.addButlerTaskFeedback passes actingMember(id) — root-admin on a bare admin token. The schema says the person never the Butler, which rules out the Butler, not principals (#239 review)"},
	{"household_review_tokens", "member", Human,
		"IssueHouseholdReviewToken(planID, member, issuedBy, …); schema: 'member is who it was issued to'"},
	{"integration_secrets", "member", Actor,
		"api.onboardIntegrationSecret defaults an omitted member to actingMember(id), so a bare admin token stores root-admin. The schema says PERSONAL credential; nothing enforces it (#239 review, #240)"},
	{"member_notice_preferences", "member", Actor,
		"api.setNoticePreferences stores actingMember(id) when member is omitted, and TestNoticePreferencesAreYourOwn exercises that with the bare op token. An operator having its own notice preferences is a supported path, not an accident"},
	{"offboarding_records", "member", Human,
		"recordOffboarding(member, actor, …) — member is the person being offboarded; actor is the actor"},
	{"butler_members", "guardian", Human,
		"schema: 'guardian is required for a child record' — names the responsible adult"},
	{"butler_members", "member", Human,
		"the handle itself. Not repointed — it IS the row person_id belongs to — but classified so the table is complete"},

	{"notices", "recipient", Human,
		"schema: `recipient is the member`. Runtime filters, authorizes replies and offboards by it. Missed by the first name heuristic (#239 review)"},
	{"household_banner", "set_by", Human,
		"SetHouseholdBanner(image, mediaType, by) and SetHouseholdBannerPosition(x, y, by); both call sites pass the portal's `member`, which the door has already resolved to the household's own spelling. A `_by` name that is NOT the actor family: the banner has no machine writer — the portal is the only surface that sets it, and a household member is the only caller it admits (#238)"},
	// ---- Actor: person OR machine principal; left as recorded text ----
	{"chat_claim_codes", "identity", Actor,
		"the identity a claim code was minted for; principal or member, as chat_bindings.identity"},
	{"chat_bindings", "identity", Actor,
		"schema: `principal (admin) or household member (family)` — polymorphic by construction, the actor family in one column"},
	{"chat_threads", "identity", Actor,
		"the bound identity a thread belongs to; principal or member, as chat_bindings.identity"},

	{"audit", "principal", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},
	{"audit", "acting_for", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},
	{"butler_access", "granted_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_access", "revoked_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_addresses", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_addresses", "revoked_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_capability_requests", "requested_by", Actor,
		"api.fileCapabilityRequest passes actingMember(id); TestCapabilityRequestPermissionsAndSeparation stores `op`. The schema states the INTENT, and nothing enforces it (#239 review)"},
	{"butler_capability_requests", "decided_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_config", "updated_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_connection_scope", "granted_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_connection_scope", "revoked_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_debug_capture", "enabled_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_evaluations", "author", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_identity_links", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_identity_links", "revoked_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_index", "owner", Scoped,
		"mirrors butler_memory.owner, so it carries the same `household` scope for shared documents, and the same legacy-path exposure (#240)"},
	{"butler_members", "admitted_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_members", "disabled_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_members", "offboarded_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_memory", "forgotten_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_memory_digest", "actor", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_memory_suppressed_sources", "suppressed_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_pool_disclosures", "promoted_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_role_changes", "member", Human,
		"the audited person handle, retained beside person_id for readability after a purge. Already carries its id, so 2b does not repoint it"},
	{"butler_role_changes", "actor", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_runtime_settings", "updated_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_sessions", "principal", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},
	{"butler_sessions", "acting_for", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},
	{"butler_task_transitions", "actor", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_task_waits", "resolved_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_tool_controls", "updated_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_tool_grants", "granted_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_tool_grants", "revoked_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_tool_scope", "updated_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_work_trace", "actor", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butlers", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butlers", "updated_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butlers", "retired_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"chat_bindings", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"chat_claim_codes", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"deletion_journal", "actor", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"household_review_tokens", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"household_review_tokens", "used_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"household_routine_revisions", "author", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"household_routines", "author", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"household_routines", "updated_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"household_routines", "disabled_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"integration_connections", "owner", Actor,
		"holds the literal `household` for a household-owned connection AND, because api.createConnection defaults an omitted owner to actingMember(id), a machine principal for a personal one. Actor because neither may block: an operator-owned personal connection is a supported path (#239 review round 4). 2b still needs ownership-aware handling here, from the row rather than from this verdict"},
	{"integration_connections", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"integration_placements", "added_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"integration_principals", "principal", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},
	{"integration_principals", "bound_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"integration_secrets", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"integration_work", "claimed_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"models", "updated_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"notice_kinds", "updated_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"notices", "answered_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"offboarding_records", "actor", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"plans", "principal", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},
	{"plans", "acting_for", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},
	{"plans", "resolved_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"principals", "acting_for", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},
	{"restore_state", "journal_asserted_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"restore_state", "released_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"tokens", "principal", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},
	{"tokens", "acting_for", Actor,
		"a machine principal from the principals table — the legacy machine axis (#201 Decision 6, slice #210)"},

	// ---- Not a person: false positives of the name heuristic ----
	{"butler_conversations", "subject", NotAPerson,
		"what the conversation is about"},
	{"butler_memory", "subject", NotAPerson,
		"schema: `subject is who or what the record is about` — a person OR a thing"},
	{"butler_memory", "superseded_by", NotAPerson,
		"INTEGER REFERENCES butler_memory(seq) — the record that corrected this one"},
	{"chat_questions", "subject", NotAPerson,
		"the topic the question is about, not who asked it"},
	{"household_events", "subject", NotAPerson,
		"schema: `member / device / household id` — polymorphic subject, not a person column"},
	{"meter_readings", "meter", NotAPerson,
		"the metadata.name of a Meter resource — a thing the household counts, not somebody who counts it"},

	// ---- Actor: a person on one path, a machine principal on another ----
	{"meter_readings", "recorded_by", Actor,
		"a member when the reading came from the Console, the CLI or chat; the publishing principal when a capability sent it (ADR-W034 A3)"},
}
