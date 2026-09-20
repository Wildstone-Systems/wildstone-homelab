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
// # Where the ADR decides, and the writer does not
//
// The rule above — the writer decides what a column means — was learned
// the hard way against SCHEMA COMMENTS, which state an intent nothing
// enforces. It does not survive contact with a column whose subject an
// ADR names, because there the writer may simply be the defect.
//
// ADR-W033 is authoritative where it conflicts with earlier decisions,
// and it names the subject of the Connection and session columns
// outright. A Connection admits a Person or a Group (B9, design §8.3);
// every external identity — directory login, web session, Telegram,
// Discord — attaches to the same canonical Person (A2); a session is
// Person + Agent + session_id (B10, design §9). An Owner-Agent
// Connection admits Owners, and an Owner is a Person with role `owner`,
// not a machine credential.
//
// So `principal (admin) or household member (family)` — the comment on
// chat_bindings.identity, written under ADR-W026 A3 — is not evidence of
// a polymorphic column. It is a record of the model W033 superseded, and
// principal/chatclaim.go enforcing it is the defect rather than the
// contract. W033 G retains W026 A3 only "where [it requires]
// independent Agent credentials, generalized through the Connection
// abstraction": the ADAPTER keeps its own credential, and the
// admission subject does not become that credential.
//
// The precedence, stated once so it is not re-derived:
//
//	an ADR that names the subject   >   the schema comment
//	                                >   what the write path stores today
//
// This was found in review of slice 2b and corrected there. Which
// columns it moved is recorded on each verdict below.
//
// # Columns W033 governs that are NOT reclassified here
//
// These hold a person by W033's reading and still carry an actor
// verdict, because the slice that owns the decision has not run and a
// verdict changed on speculation is the same error in the other
// direction:
//
//   - member_notice_preferences.member — E3 routes notices by
//     designated Owner/reviewer preference, which is a Person's. The
//     operator token's own preferences are the machine axis leaking
//     into a person concept, but nothing in W033 says preferences
//     cannot be held per credential. Left to the Connections slice
//     (#212), which owns delivery — still open after that slice's
//     admission half, which settled who may address an Agent and not
//     where a notice is sent.
//   - integration_secrets.member — §12.2 keeps the household secret
//     store separate from the infrastructure one, and this column is in
//     the second. Left to the secrets slice (#216).
//   - butler_capability_requests.requested_by, butler_task_feedback.author
//     — a Member asking an Owner for a capability, and a person's
//     judgement of a run. Both read as Person under B1, and both are
//     recorded by actingMember today. Left to the Agent-class slice
//     (#210), which decides the actor representation.
//
// Every other actor column records WHO ACTED, and epic #201 Decision 6
// keeps the legacy machine axis deliberately until #210.
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
	// PersonID already holds an identity rather than a handle: the
	// columns migration 0043 repointed, plus butler_members.person_id
	// itself. There is no free text here to resolve, so the survey
	// leaves them alone — a person_id that named nobody would be a
	// broken foreign key, which the database refuses on its own and
	// does not need a report to find.
	//
	// They stay classified rather than dismissed by name because the
	// record of WHICH columns moved is the useful part, and because a
	// new person_id column appearing somewhere unexpected should still
	// have to say what it is.
	PersonID Kind = "person-id"
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
// LegacyColumns is the classification as it stood BEFORE migration 0043,
// when every one of these held a handle as free text.
//
// It is kept, and surveyed, because `keystone person-refs` opens the
// database file read-only and does NOT migrate it. So this binary can
// be pointed at a deployment still on 0039 — which is exactly when the
// pre-flight is worth running, since 0043 refuses to apply while any of
// these is unresolvable. Survey skips whichever set the live schema
// does not have, so the same command answers correctly on both sides of
// the migration.
//
// Each entry names the column 0043 replaced it with, so the two tables
// read as one story rather than as a contradiction.
var LegacyColumns = []Column{
	{"butler_access", "member", Human,
		"pre-0043: the person admitted to a Butler, as a handle. Now butler_access.person_id"},
	{"butler_identity_links", "member", Human,
		"pre-0043: the person a surface identity belongs to. Now butler_identity_links.person_id"},
	{"butler_conversations", "member", Human,
		"pre-0043: the person whose conversation it is. Now butler_conversations.person_id"},
	{"butler_handoffs", "member", Human,
		"pre-0043: the person whose conversation crossed surfaces. Now butler_handoffs.person_id"},
	{"butler_memory", "author", Human,
		"pre-0043: the person who stated the record. Now butler_memory.author_person_id"},
	{"butler_memory", "owner", Scoped,
		"pre-0043: the person the record belongs to, or the literal `household` for a shared one. Now butler_memory.owner_person_id, where the household is NULL"},
	{"butler_index", "owner", Scoped,
		"pre-0043: mirrors butler_memory.owner. Now butler_index.owner_person_id"},
	{"butler_tasks", "author", Human,
		"pre-0043: the person who asked for the work. Now butler_tasks.author_person_id"},
	{"butler_tasks", "owner", Human,
		"pre-0043: who operates the work now. Now butler_tasks.owner_person_id"},
	{"household_review_tokens", "member", Human,
		"pre-0043: who the token was issued to. Now household_review_tokens.person_id"},
	{"offboarding_records", "member", Human,
		"pre-0043: the person being offboarded. Now offboarding_records.person_id"},
	{"notices", "recipient", Human,
		"pre-0043: the person the notice is for. Now notices.recipient_person_id"},
	{"butler_members", "guardian", Human,
		"pre-0043: the responsible adult for a child record. Now butler_members.guardian_person_id"},

	// Repointed by 0044 rather than 0043, and classified actor until
	// slice 2b read the ADR instead of the write path.
	{"chat_bindings", "identity", Human,
		"pre-0044: the subject a Connection admits, as a handle — or, on the admin surface, as a machine principal, which ADR-W033 B9 supersedes. Now chat_bindings.person_id"},
	{"chat_claim_codes", "identity", Human,
		"pre-0044: the subject a claim code admits, which ADR-W033 B9 says is a Person. Now chat_claim_codes.person_id"},
	{"chat_threads", "identity", Human,
		"pre-0044: the subject whose session a thread holds, which ADR-W033 B10 says is a Person. Now chat_threads.person_id"},
}

var Columns = []Column{
	// ---- Human: repointed by slice 2b ----
	{"butler_access", "person_id", PersonID,
		"the person admitted to a Butler. Repointed by 0043 from member; by is still the actor"},
	{"butler_identity_links", "person_id", PersonID,
		"the person a surface identity belongs to. Repointed by 0043; the foreign key it already had moved from member to person_id"},
	{"butler_conversations", "person_id", PersonID,
		"the person whose conversation it is. Repointed by 0043; the foreign key moved from member to person_id"},
	{"butler_handoffs", "person_id", PersonID,
		"the person whose conversation crossed surfaces. Repointed by 0043, which also gave it the foreign key it never had"},
	{"butler_memory", "owner_person_id", PersonID,
		"the person the record belongs to. Repointed by 0043, which stores the documented `household` owner as NULL rather than as a person who is not one"},
	{"butler_memory", "author_person_id", PersonID,
		"the person who stated the record. Repointed by 0043; the legacy path that could write a principal here (#240) is refused at the write now"},
	{"butler_tasks", "author_person_id", PersonID,
		"the person who asked for the work, and it never changes. Repointed by 0043"},
	{"butler_tasks", "owner_person_id", PersonID,
		"who operates the work now; offboarding moves it. Repointed by 0043"},
	{"butler_task_feedback", "author", Actor,
		"api.addButlerTaskFeedback passes actingMember(id) — root-admin on a bare admin token. The schema says the person never the Butler, which rules out the Butler, not principals (#239 review)"},
	{"household_review_tokens", "person_id", PersonID,
		"who the token was issued to. Repointed by 0043; created_by and used_by are actors and stayed text"},
	{"integration_secrets", "member", Actor,
		"api.onboardIntegrationSecret defaults an omitted member to actingMember(id), so a bare admin token stores root-admin. The schema says PERSONAL credential; nothing enforces it (#239 review, #240)"},
	{"member_notice_preferences", "member", Actor,
		"api.setNoticePreferences stores actingMember(id) when member is omitted, and TestNoticePreferencesAreYourOwn exercises that with the bare op token. An operator having its own notice preferences is a supported path, not an accident"},
	{"offboarding_records", "person_id", PersonID,
		"the person being offboarded. Repointed by 0043; actor is the actor and stayed text"},
	{"butler_members", "guardian_person_id", PersonID,
		"the responsible adult for a child record. Repointed by 0043, which also made it nullable: '' was never an identity"},
	{"butler_members", "member", Human,
		"the handle itself. Not repointed — it IS the row person_id belongs to — but classified so the table is complete"},
	{"butler_members", "person_id", PersonID,
		"the identity the handle above is a label for (0039). Every person_id elsewhere resolves here"},
	{"butler_role_changes", "person_id", PersonID,
		"the audited person, by identity (0039). Kept beside the handle so the trail still resolves after a rename"},

	{"notices", "recipient_person_id", PersonID,
		"the person the notice is for. Repointed by 0043 from recipient; answered_by is an actor and stayed text"},
	{"household_banner", "set_by", Human,
		"SetHouseholdBanner(image, mediaType, by) and SetHouseholdBannerPosition(x, y, by); both call sites pass the portal's `member`, which the door has already resolved to the household's own spelling. A `_by` name that is NOT the actor family: the banner has no machine writer — the portal is the only surface that sets it, and a household member is the only caller it admits (#238). Still a handle: no migration repoints it, so slice 2b leaves it as recorded text"},
	// ---- Actor: person OR machine principal; left as recorded text ----
	// The three Connection identity columns, repointed by 0044. They
	// were classified actor until slice 2b, from a write path that was
	// itself the defect — see "Where the ADR decides" above, and the
	// legacy verdicts below for what they held before.
	{"chat_claim_codes", "person_id", PersonID,
		"the Person a claim code admits. ADR-W033 B9 / design §8.3: a Connection's admission subject is a Person or a Group. Repointed by 0044"},
	{"chat_bindings", "person_id", PersonID,
		"the Person the Connection admits. ADR-W033 A2 — a Telegram or Discord identity attaches to the canonical Person — and B9/§8.3. Repointed by 0044; the adapter's own credential still authenticates the bot"},
	{"chat_threads", "person_id", PersonID,
		"the Person whose session this is. ADR-W033 B10 / design §9: a session is Person + Agent + session_id, and the Connection is never the session identity. Repointed by 0044"},

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
	{"projected_memberships", "person_id", PersonID,
		"the Person whose directory membership Wildstone placed; recorded as an identity because projection resolves a directory account to a Person by handle and must not store the name it matched on (design §4, slice 8)"},
	{"service_entitlements", "subject", PersonID,
		"a person_id when subject_kind = 'person', a group_id when subject_kind = 'group' — who an entitlement is about, keyed on the slice-6 namespace rather than on names (design §4, slice 8)"},
	{"service_entitlements", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise — and the installer, which is neither"},
	{"audience_members", "ref", PersonID,
		"a person_id when kind = 'person', a group_id when kind = 'group' — the audience's members, keyed on the slice-6 namespace rather than on names (design §4, slice 7)"},
	{"butler_debug_capture", "enabled_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_evaluations", "author", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_identity_links", "created_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_identity_links", "revoked_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_index", "owner_person_id", PersonID,
		"mirrors butler_memory.owner_person_id, NULL for a household-owned chunk. Repointed by 0043"},
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
	{"butler_sessions", "revoked_by", Actor,
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
	{"butler_tool_assignments", "assigned_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_tool_assignments", "revoked_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_tool_bundles", "granted_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_tool_bundles", "revoked_by", Actor,
		"written from actingMember(id) or id.Name: a person handle for a Console caller, a machine principal name otherwise"},
	{"butler_tool_governance", "enrolled_by", Actor,
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
	{"connection_invites", "person_id", PersonID,
		"the Person invited to a chat Connection (ADR-W033 B7.1)"},
	{"connection_invites", "invited_by", Actor,
		"the Owner who made the Connection, as actingMember records them"},
	{"connection_admission", "set_by", Actor,
		"who wrote the Connection's admission policy: an Owner as actingMember " +
			"records them, or 'migration:0063' for the rows the migration derived " +
			"from the Agent class"},
	{"connection_admission", "admits", NotAPerson,
		"which ROLE a Connection admits ('owners' or 'household'), which names a " +
			"class of people and never one of them"},
	{"connection_admission", "set_at", NotAPerson, "a timestamp"},
	{"theme_backgrounds", "name", NotAPerson, "a theme's name, not a person's"},
	{"theme_backgrounds", "image", NotAPerson, "the wallpaper's bytes"},
	{"theme_backgrounds", "media_type", NotAPerson, "an image media type, decided from the bytes"},
	{"theme_backgrounds", "set_by", Actor,
		"the Owner who added the picture, as actingMember records them"},
	{"theme_backgrounds", "updated_at", NotAPerson, "a timestamp"},
	{"person_theme", "person_id", PersonID,
		"the Person whose chosen theme this is (0060)"},
	{"person_theme", "theme", NotAPerson, "a theme's name, not a person's"},
	{"person_theme", "updated_at", NotAPerson, "a timestamp"},
	{"household_themes", "name", NotAPerson, "a theme's name, not a person's"},
	{"household_themes", "colors", NotAPerson, "a pasted colors.toml"},
	{"household_themes", "added_by", Actor,
		"the Owner who pasted the theme in, as actingMember records them"},
	{"household_themes", "updated_at", NotAPerson, "a timestamp"},
	{"connection_invites", "closed_at", NotAPerson, "a timestamp"},
	{"connection_credentials", "fingerprint", NotAPerson, "a SHA-256 of a bot's platform credential"},
	{"connection_credentials", "created_at", NotAPerson, "a timestamp"},
	{"connection_invites", "closed_reason", NotAPerson, "why the invite closed: 'bound' or an Owner's withdrawal reason"},
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
	{"person_group_members", "person_id", PersonID,
		"the Person this membership is about; the groups namespace is keyed on person_id rather than the handle (design §4, slice 6)"},
	{"person_group_members", "added_by", Actor,
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

	{"commitment_bookings", "commitment", NotAPerson,
		"the metadata.name of a Commitment resource"},
	{"commitment_bookings", "booked_at", NotAPerson,
		"an RFC3339 timestamp: when the appointment was made"},
	{"commitment_bookings", "cancelled_at", NotAPerson,
		"an RFC3339 timestamp, empty while the booking stands"},
	{"commitment_bookings", "calendar_event", NotAPerson,
		"the CalDAV href of the event the booking made — an address in the household calendar, not somebody"},
	{"commitment_completions", "commitment", NotAPerson,
		"the metadata.name of a Commitment resource"},
	{"commitment_completions", "positions", NotAPerson,
		"a JSON object of meter name to the reading at completion — evidence, not people"},
	{"commitment_state", "commitment", NotAPerson,
		"the metadata.name of a Commitment resource"},
	{"commitment_state", "trigger", NotAPerson,
		"the name of the meter whose threshold decided the state"},
	{"commitment_state", "changed_at", NotAPerson,
		"an RFC3339 timestamp: when the household was last told"},

	// ---- Actor: a person on one path, a machine principal on another ----
	{"meter_readings", "recorded_by", Actor,
		"a member when the reading came from the Console, the CLI or chat; the publishing principal when a capability sent it (ADR-W035 A3)"},
	{"commitment_completions", "recorded_by", Actor,
		"whoever recorded the work as done: a member today, and a capability's principal once an adapter can close a cycle (ADR-W035 A8)"},
	{"commitment_bookings", "booked_by", Actor,
		"whoever made the appointment; the same principal text as the rest of this family until the Person/principal split"},
	{"commitment_bookings", "cancelled_by", Actor,
		"whoever cancelled it, empty while the booking stands"},
}
