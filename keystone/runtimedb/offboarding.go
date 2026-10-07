package runtimedb

// Cross-store offboarding (ADR-W030 A2.1/A4, P16).
//
// "We offboarded them" is a claim about nine stores, and access denied
// in six places at six different times is six windows. So this is one
// call that lands everywhere, in one transaction where it can be and in
// a recorded sequence where it cannot, and it writes down what it did to
// each store — because a claim about nine stores needs nine answers.
//
// Two rules shape what happens to the work rather than the access.
//
// Personal work PAUSES. It performs no retries, no notices and no model
// calls, and nothing is deleted: it is somebody's unfinished business,
// and the household may need to know what it was.
//
// Shared work TRANSFERS and keeps its authorship. Operational ownership
// moves to the administrator so the house can carry on; the author stays
// whoever asked for it, because rewriting that would make the history a
// lie about who wanted what.

import (
	"errors"
	"fmt"
	"time"
)

// OffboardingResult is what actually happened, per store.
type OffboardingResult struct {
	Member string         `json:"member"`
	At     time.Time      `json:"at"`
	Stores map[string]int `json:"stores"`
	// TransferredTo names who now operates the shared work.
	TransferredTo string `json:"transferredTo,omitempty"`
}

// apiTokensJournalPrefix marks a journalled link entry as the revocation of
// a principal's bearer tokens rather than of a surface link, so
// ReconcileRestore can replay it: a restored snapshot brings the tokens
// back live, and the link revocation alone does not take them away again.
const apiTokensJournalPrefix = "api-tokens:"

// OffboardMember denies access everywhere and settles the work.
//
// Order matters and is not arbitrary. Admission and links go first —
// that is the denial, and every moment after it is a moment they cannot
// act. Everything below is cleanup that must not be able to restore
// access if it fails, which is exactly why it comes second.
func (db *DB) OffboardMember(member, actor, administrator string) (OffboardingResult, error) {
	if member == "" || actor == "" {
		return OffboardingResult{}, fmt.Errorf("runtimedb: offboarding needs a member and who did it")
	}
	if administrator == "" {
		return OffboardingResult{}, fmt.Errorf(
			"runtimedb: offboarding needs the administrator who takes over shared work (ADR-W030 A4)")
	}
	result := OffboardingResult{
		Member: member, At: time.Now().UTC(), Stores: map[string]int{},
		TransferredTo: administrator,
	}

	// Collect the live api-principal links before the denial: step 1
	// revokes every link, and afterwards an earlier-revoked link cannot be
	// told apart from one this offboarding revoked, and may belong to
	// someone else now.
	before, err := db.ListSurfaceLinks(member)
	if err != nil {
		return result, err
	}
	var principals []string
	for _, l := range before {
		if l.LinkType == LinkAPIPrincipal && l.RevokedAt.IsZero() {
			principals = append(principals, l.ExternalID)
		}
	}

	// 1. Deny. Admission off, generation bumped, every link revoked.
	//    Nothing below can put this back.
	if err := db.OffboardButlerMember(member, actor); err != nil {
		return result, fmt.Errorf("runtimedb: offboarding: %w", err)
	}
	if err := db.journal("member", member, "offboarded", actor, "butler admission withdrawn"); err != nil {
		return result, err
	}
	links, err := db.ListSurfaceLinks(member)
	if err != nil {
		return result, err
	}
	revoked := 0
	for _, l := range links {
		if !l.RevokedAt.IsZero() {
			revoked++
			if err := db.journal("link", l.LinkType+":"+l.ExternalID, "revoked", actor, ""); err != nil {
				return result, err
			}
		}
	}
	result.Stores["identity-links"] = revoked

	// 1b. Bearer tokens. Only principals the Person was linked to — equal
	//     names are not proof of identity. The principal is neither
	//     disabled nor deleted; an Owner can mint a token on purpose.
	for _, principal := range principals {
		n, err := db.RevokeTokensFor(principal)
		if err != nil {
			return result, err
		}
		result.Stores["api-tokens"] += int(n)
		if err := db.journal("link", apiTokensJournalPrefix+principal, "revoked", actor, "api tokens revoked: owner offboarded"); err != nil {
			return result, err
		}
	}

	// 2. Chat bindings. The link table already refuses them, but a
	//    revoked binding is also what a Console shows, and someone
	//    reading it should not see a live chat account for a person who
	//    has none.
	bindings, err := db.ListChatBindings(true)
	if err != nil {
		return result, err
	}
	for _, b := range bindings {
		if b.Identity != member || b.Revoked() {
			continue
		}
		if err := db.RevokeChatBinding(b.ID); err != nil {
			return result, fmt.Errorf("runtimedb: revoking a chat binding: %w", err)
		}
		result.Stores["chat-bindings"]++
		if err := db.journal("binding", fmt.Sprintf("%d", b.ID), "revoked", actor, ""); err != nil {
			return result, err
		}
	}
	// A queued answer may have been composed before revocation. Expire it
	// without logging its target or content; the poll boundary also checks
	// current binding and Person admission if this sweep misses a row.
	stopped, err := db.sql.Exec(`UPDATE chat_outbound SET expires_at = ?
	 WHERE delivered_at = '' AND expires_at > ? AND binding_id IN (
	   SELECT b.id FROM chat_bindings b JOIN butler_members p ON p.person_id = b.person_id
	   WHERE p.member = ?)`, now(), now(), member)
	if err != nil {
		return result, fmt.Errorf("runtimedb: stopping queued deliveries: %w", err)
	}
	n, _ := stopped.RowsAffected()
	result.Stores["queued-deliveries-stopped"] = int(n)

	// 3. Work. Personal pauses, shared transfers with authorship intact.
	memberPerson, err := resolvePersonID(db.sql, member)
	if err != nil {
		return result, err
	}
	taskRows, err := db.sql.Query(butlerTaskColumns+` WHERE t.owner_person_id=? ORDER BY t.seq`, memberPerson)
	if err != nil {
		return result, err
	}
	tasks, err := collectButlerTasks(taskRows)
	_ = taskRows.Close()
	if err != nil {
		return result, err
	}
	// Ownership moves to a Person, so the administrator taking it on
	// has to be one. Resolved once, before any transfer, so a bad
	// administrator fails before half the work has moved — and resolved
	// only when there IS shared work, because an offboarding with
	// nothing to hand over does not need somebody to hand it to. A
	// household whose operator is not itself a declared person can
	// still offboard a member who owns nothing shared.
	adminPerson := ""
	if anySharedWork(tasks) {
		var err error
		if adminPerson, err = resolvePersonID(db.sql, administrator); err != nil {
			return result, fmt.Errorf("runtimedb: transferring shared work: the administrator %w", err)
		}
		admin, err := db.GetButlerMember(administrator)
		if err != nil || !admin.Active() || admin.Member == member {
			return result, fmt.Errorf("runtimedb: transferring shared work needs another active Person")
		}
	}
	for _, t := range tasks {
		switch t.State {
		case ButlerTaskPending, ButlerTaskRunning, ButlerTaskWaiting:
		default:
			continue
		}
		if t.Audience == ButlerAudienceShared {
			// Operational ownership moves; the author does not. The
			// schema enforces the second half.
			// THE CONNECTION goes with the person. The SESSION stays.
			//
			// A4 re-homes the work; it does not re-home the request
			// that made it. The Connection was the offboarded person's
			// — offboarding fences the Connections they own a few steps
			// below — and leaving it here would bind the
			// administrator's work to a withdrawn person's Connection
			// and refuse its first tool call for ever: nobody can
			// continue that session (continueButlerSession requires the
			// caller BE its person) and every other path arrives over a
			// different Connection. Cleared rather than repointed,
			// because what the new owner will continue it over is not
			// known here and must not be guessed.
			//
			// The session is a different thing and is KEPT. It is not
			// what blocks — derived authority asks whether that session
			// was withdrawn, and offboarding withdraws the PERSON, which
			// the owner leg already covers. What it is, is the only way
			// back to this task: ButlerTaskForSession is the single
			// production lookup (butlerlive/serve.go), so clearing it
			// would strand the verified ledger A4 transfers the work in
			// order to preserve. An earlier version of this fix did
			// exactly that; a review bot on #287 caught it.
			//
			// WHAT IS STILL MISSING, stated so the next slice finds it:
			// preserving the handle is not the same as being able to
			// USE it. An administrator has no way to resume transferred
			// work — there is no resume-by-task path, and the session
			// belongs to somebody who can no longer call. That is A4's
			// to finish; §10.1's job here is not to make the gap worse,
			// and keeping the association is what that takes.
			if _, err := db.sql.Exec(
				`UPDATE butler_tasks
				 SET owner_person_id = ?, origin_connection = '', origin_connection_ref = 0, origin_conversation = '',
				     updated_at = ?
				 WHERE seq = ?`,
				adminPerson, now(), t.Seq); err != nil {
				return result, fmt.Errorf("runtimedb: transferring shared work: %w", err)
			}
			// It still pauses: A4 says transferred work waits for grant
			// and connection review before resuming.
			if err := db.TransitionButlerTask(t.ID, ButlerTaskPausedOffboarded,
				"owner offboarded; transferred for review", actor); err != nil {
				return result, err
			}
			result.Stores["shared-tasks-transferred"]++
			continue
		}
		if err := db.TransitionButlerTask(t.ID, ButlerTaskPausedOffboarded,
			"owner offboarded", actor); err != nil {
			return result, err
		}
		result.Stores["personal-tasks-paused"]++
	}

	// 4. Grants. Every temporary permission on their work ends now.
	for _, t := range tasks {
		n, err := db.RevokeGrantsForScope(GrantScopeTask, t.ID, actor)
		if err != nil {
			return result, err
		}
		result.Stores["grants-revoked"] += n
	}

	// 5. Connections they own. Fenced rather than deleted: the adapter
	//    stops being able to act, and what to do with the connection is
	//    a decision for the household.
	connections, err := db.ListConnections()
	if err != nil {
		return result, err
	}
	for _, c := range connections {
		if c.Owner != member || c.Ownership != OwnershipPersonal {
			continue
		}
		if _, err := db.BumpConnectionEpoch(c.Name); err != nil {
			return result, err
		}
		if err := db.SetConnectionState(c.Name, ConnectionUnavailable,
			fmt.Sprintf("%s was offboarded; this was their personal connection", member)); err != nil {
			return result, err
		}
		result.Stores["connections-fenced"]++
		if err := db.journal("connection", c.Name, "revoked", actor, "owner offboarded"); err != nil {
			return result, err
		}
	}

	// 6. Undelivered notices. Nothing more is sent to somebody who no
	//    longer has access, and anything queued is marked rather than
	//    dropped, so the record shows they were never told.
	if _, err := db.sql.Exec(
		`UPDATE notices SET undelivered_at = ?
		 WHERE recipient_person_id = (SELECT person_id FROM butler_members WHERE member = ?)
		   AND delivered_at = '' AND undelivered_at = ''`,
		now(), member); err != nil {
		return result, fmt.Errorf("runtimedb: stopping notices: %w", err)
	}
	result.Stores["notices-stopped"]++

	// 7. Private memory. Marked forgotten and journalled, so a restore
	//    of an older snapshot cannot bring it back. Shared memory stays:
	//    it is the household's, and it was never only theirs.
	records, err := db.ExportMemoryFor(member)
	if err != nil {
		return result, err
	}
	for _, m := range records {
		if m.Scope != MemoryScopePersonal || m.Owner != member || !m.ForgottenAt.IsZero() {
			continue
		}
		if err := db.ForgetMemory(m.Seq, actor); err != nil {
			return result, err
		}
		if _, err := db.RemoveIndexForMemory(m.Seq); err != nil {
			return result, err
		}
		result.Stores["private-memory-quarantined"]++
		if err := db.journal("memory", fmt.Sprintf("%d", m.Seq), "deleted", actor,
			"owner offboarded"); err != nil {
			return result, err
		}
	}

	// 8. Personal credentials. Revoked where nothing live still uses
	//    them; where something does, the placement removal path is what
	//    fences it first, and this records that it is outstanding.
	for _, c := range connections {
		secrets, err := db.ListConnectionSecrets(c.Name)
		if err != nil {
			return result, err
		}
		managed := false
		for _, s := range secrets {
			if s.Member != member || !s.RevokedAt.IsZero() {
				continue
			}
			if s.Managed() {
				managed = true
				continue
			}
			if err := db.RevokeConnectionSecret(c.Name, s.Seq); err != nil {
				// A referenced credential cannot be erased yet. That is
				// not a failure of the offboarding — access is already
				// denied — and it is recorded so the cleanup is visible
				// as outstanding rather than assumed done.
				if err := db.recordOffboarding(member, actor, "credentials-outstanding", 1,
					fmt.Sprintf("%s on %s is still provisioned to a live placement", s.SecretRef, c.Name)); err != nil {
					return result, err
				}
				result.Stores["credentials-outstanding"]++
				continue
			}
			result.Stores["credentials-revoked"]++
			if err := db.journal("secret", s.SecretRef, "revoked", actor, "owner offboarded"); err != nil {
				return result, err
			}
		}
		// A Personal credential onboarded through the intake is fenced
		// here — revoked, its background authorization with it — and its
		// copies become erasures the API proves against the nodes. Until
		// each is proven the connection says erasure_unverified.
		if c.Owner == member && c.Ownership == OwnershipPersonal {
			if err := db.RevokeBackgroundAuthorization(c.Name, actor, "owner offboarded"); err == nil {
				result.Stores["background-authorizations-revoked"]++
			} else if !errors.Is(err, ErrNotFound) {
				return result, err
			}
		}
		if managed {
			n, err := db.RevokePersonalCredentials(c.Name, actor, "owner offboarded")
			if err != nil {
				return result, err
			}
			result.Stores["credentials-revoked"] += n
			pending, err := db.OutstandingCredentialErasures(c.Name)
			if err != nil {
				return result, err
			}
			if len(pending) > 0 {
				result.Stores["credential-erasures-pending"] += len(pending)
				if err := db.recordOffboarding(member, actor, "credential-erasures-pending", len(pending),
					fmt.Sprintf("%s: %d credential copies await verified erasure", c.Name, len(pending))); err != nil {
					return result, err
				}
			}
		}
	}

	// 8b. What their mailbox monitors learned. Only now — after step 5
	//     moved the epochs and step 8 revoked the authorizations and
	//     credentials — so nothing is recording while it goes: their
	//     candidates, reviews, excerpts, effects, dedupe state and
	//     checkpoints are deleted, and journalled so a restore deletes
	//     them again (epic #326 S08).
	for _, c := range connections {
		if c.Owner != member || c.Ownership != OwnershipPersonal {
			continue
		}
		erased, err := db.EraseMailConnectionState(c.Name, actor, "owner offboarded", time.Now().UTC())
		if err != nil {
			return result, fmt.Errorf("runtimedb: erasing %s's mailbox state: %w", c.Name, err)
		}
		if erased.Total() == 0 {
			continue
		}
		result.Stores["mail-state-erased"] += erased.Total()
		if _, err := db.sql.Exec(`INSERT OR IGNORE INTO offboarding_resource_dispositions
		 (person_id,resource_kind,resource_ref,disposition,decided_at,decided_by)
		 VALUES(?,'mail-monitor',?,'deleted',?,?)`, memberPerson, c.Name, now(), actor); err != nil {
			return result, err
		}
	}

	// 9. The durable inventory is an explicit policy decision for every
	// associated object, not an implication inferred later from a count.
	// Shared material stays with the household; personal material remains
	// quarantined or paused until an Owner deliberately purges it.
	decisions, err := db.recordOffboardingDispositions(member, actor, tasks)
	if err != nil {
		return result, err
	}
	result.Stores["resource-dispositions"] = decisions

	for store, n := range result.Stores {
		if err := db.recordOffboarding(member, actor, store, n, ""); err != nil {
			return result, err
		}
	}
	return result, nil
}

// OffboardingResourceDisposition is the recorded fate of one durable
// resource. It contains identifiers and policy only, never memory or
// secret content.
type OffboardingResourceDisposition struct {
	Kind        string `json:"kind"`
	Ref         string `json:"ref"`
	Disposition string `json:"disposition"`
	DecidedAt   string `json:"decidedAt"`
	DecidedBy   string `json:"decidedBy"`
}

func (db *DB) recordOffboardingDispositions(member, actor string, ownedTasks []ButlerTask) (int, error) {
	person, err := resolvePersonID(db.sql, member)
	if err != nil {
		return 0, err
	}
	// Capture ownership from before the transfer. A shared task may have
	// been authored by another Person, so querying only its new owner or
	// its author after the transfer would omit the departed owner's work.
	for _, task := range ownedTasks {
		decision := "retained"
		switch task.State {
		case ButlerTaskPending, ButlerTaskRunning, ButlerTaskWaiting:
			if task.Audience == ButlerAudienceShared {
				decision = "transferred"
			} else {
				decision = "retained-paused"
			}
		}
		if _, err := db.sql.Exec(`INSERT OR IGNORE INTO offboarding_resource_dispositions
		 (person_id,resource_kind,resource_ref,disposition,decided_at,decided_by)
		 VALUES(?,'task',?,?,?,?)`, person, task.ID, decision, now(), actor); err != nil {
			return 0, err
		}
	}
	type source struct {
		kind, query string
	}
	// Each SELECT returns ref and the action that already happened during
	// offboarding. The member handle is used only for legacy rows whose
	// author/owner has not been moved to person_id.
	sources := []source{
		{"task", `SELECT 'bt-'||seq, CASE WHEN state='paused-offboarded' AND owner_person_id<>? THEN 'transferred' WHEN state='paused-offboarded' THEN 'retained-paused' ELSE 'retained' END FROM butler_tasks WHERE author_person_id=?`},
		{"memory", `SELECT CAST(seq AS TEXT), CASE WHEN scope='personal' AND owner_person_id=? THEN 'retained-quarantined' ELSE 'retained' END FROM butler_memory WHERE owner_person_id=? OR author_person_id=?`},
		{"secret", `SELECT id, CASE WHEN scope='personal' THEN 'retained-quarantined' ELSE 'retained' END FROM household_secrets WHERE owner_person_id=?`},
		{"connection", `SELECT name, 'blocked' FROM integration_connections WHERE owner=? AND ownership='personal'`},
		{"routine", `SELECT name, CASE WHEN deleted_at<>'' THEN 'deleted' WHEN paused_reason<>'' THEN 'retained-paused' ELSE 'retained' END FROM household_routines WHERE author=?`},
		{"agent", `SELECT name, CASE WHEN retired_at<>'' THEN 'retired' ELSE 'retained' END FROM butlers WHERE created_by=?`},
	}
	args := [][]any{{person, person}, {person, person, person}, {person}, {member}, {member}, {member}}
	for i, src := range sources {
		rows, err := db.sql.Query(src.query, args[i]...)
		if err != nil {
			return 0, fmt.Errorf("runtimedb: inventorying %s on offboarding: %w", src.kind, err)
		}
		for rows.Next() {
			var ref, decision string
			if err = rows.Scan(&ref, &decision); err != nil {
				break
			}
			_, err = db.sql.Exec(`INSERT OR IGNORE INTO offboarding_resource_dispositions
			 (person_id,resource_kind,resource_ref,disposition,decided_at,decided_by)
			 VALUES(?,?,?,?,?,?)`, person, src.kind, ref, decision, now(), actor)
			if err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return 0, fmt.Errorf("runtimedb: recording %s disposition: %w", src.kind, err)
		}
	}
	var count int
	err = db.sql.QueryRow(`SELECT count(*) FROM offboarding_resource_dispositions WHERE person_id=?`, person).Scan(&count)
	return count, err
}

func (db *DB) ListOffboardingResourceDispositions(member string) ([]OffboardingResourceDisposition, error) {
	person, err := resolvePersonID(db.sql, member)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`SELECT resource_kind,resource_ref,disposition,decided_at,decided_by
	 FROM offboarding_resource_dispositions WHERE person_id=? ORDER BY resource_kind,resource_ref`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OffboardingResourceDisposition
	for rows.Next() {
		var d OffboardingResourceDisposition
		if err := rows.Scan(&d.Kind, &d.Ref, &d.Disposition, &d.DecidedAt, &d.DecidedBy); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// journal records a deletion or revocation separately from the data it
// concerns, so a restore of an older snapshot can be reconciled against
// decisions the snapshot does not contain.
func (db *DB) journal(kind, id, action, actor, detail string) error {
	_, err := db.sql.Exec(
		`INSERT INTO deletion_journal (subject_kind, subject_id, action, at, actor, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`, kind, id, action, now(), actor, detail)
	if err != nil {
		return fmt.Errorf("runtimedb: journalling a %s of %s: %w", action, kind, err)
	}
	return nil
}

// Journal is the exported way to record a deletion or revocation that
// happened outside OffboardMember, so the journal stays complete.
func (db *DB) Journal(kind, id, action, actor, detail string) error {
	return db.journal(kind, id, action, actor, detail)
}

func (db *DB) recordOffboarding(member, actor, store string, affected int, detail string) error {
	person, err := resolvePersonID(db.sql, member)
	if err != nil {
		return fmt.Errorf("runtimedb: recording offboarding: %w", err)
	}
	_, err = db.sql.Exec(
		`INSERT INTO offboarding_records (person_id, at, actor, store, affected, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`, person, now(), actor, store, affected, detail)
	if err != nil {
		return fmt.Errorf("runtimedb: recording offboarding: %w", err)
	}
	return nil
}

// OffboardingRecord is one store's answer.
type OffboardingRecord struct {
	Member   string    `json:"member"`
	At       time.Time `json:"at"`
	Actor    string    `json:"actor"`
	Store    string    `json:"store"`
	Affected int       `json:"affected"`
	Detail   string    `json:"detail,omitempty"`
}

// ListOffboardingRecords returns what an offboarding did.
func (db *DB) ListOffboardingRecords(member string) ([]OffboardingRecord, error) {
	rows, err := db.sql.Query(
		`SELECT p.member, o.at, o.actor, o.store, o.affected, o.detail
		 FROM offboarding_records o JOIN butler_members p ON p.person_id = o.person_id
		 WHERE p.member = ? ORDER BY o.seq`, member)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing offboarding records: %w", err)
	}
	defer rows.Close()
	var out []OffboardingRecord
	for rows.Next() {
		var r OffboardingRecord
		var at string
		if err := rows.Scan(&r.Member, &at, &r.Actor, &r.Store, &r.Affected, &r.Detail); err != nil {
			return nil, err
		}
		if r.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// anySharedWork reports whether any of these tasks is shared work still
// in flight — the only kind an offboarding transfers.
//
// It repeats the loop's own two conditions rather than being folded
// into it, because the administrator has to resolve BEFORE the first
// transfer: discovering a bad one partway through would leave some work
// moved and the rest not, which is the state this function exists to
// avoid.
func anySharedWork(tasks []ButlerTask) bool {
	for _, t := range tasks {
		switch t.State {
		case ButlerTaskPending, ButlerTaskRunning, ButlerTaskWaiting:
		default:
			continue
		}
		if t.Audience == ButlerAudienceShared {
			return true
		}
	}
	return false
}
