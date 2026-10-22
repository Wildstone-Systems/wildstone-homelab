package runtimedb

// Butler admission, identity links, and conversation continuity
// (ADR-W030 A1/A2/A2.1, P6).
//
// Three questions this file answers, which were previously answered by
// coincidence:
//
//   Is this person admitted to the Butler at all? Runtime state here,
//   deliberately not LDAP membership: disabling someone's Butler and
//   deleting their directory account are different acts with different
//   urgencies, and an offboarding must not wait on a directory write.
//
//   Is this authenticated surface identity that person? Only if an
//   administrator wrote a link saying so. Equal names are not proof of
//   identity (A2.1), so nothing here infers a link from a matching
//   string, an allow-list, or a config file.
//
//   Is that link still good? Every resolve compares the link's
//   generation against the member's. Unlinking, disabling and
//   offboarding all bump the generation, so revocation lands at the
//   next check rather than whenever a cache expires.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Human roles (design §2.3). There are exactly two, and the closed set
// is the point: Owners administer the deployment, Members consume it.
//
// These are NOT principals.RoleOperator and friends, which are machine
// and API roles on a different axis, and not the Butler capability
// profiles below either. A person has a role here; a credential has a
// principal role; a Butler has a capability profile. Three questions,
// three answers, deliberately not merged.
const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

// ProfileChild is the only profile preset a PERSON can carry (design
// §2.3). It is a constrained Member, not a third role: a child is a
// Member whose profile is 'child', and the schema refuses any other
// combination. It is stored so guardian relationships can be recorded,
// and cannot be admitted until that surface has its own evidence.
//
// THIS IS A PERSON'S AXIS AND NOTHING ELSE'S. Until 0058 this constant
// block also held 'household' and 'operator', which described a BUTLER's
// ceiling, and butlers.profile borrowed the whole vocabulary — so one
// symbol meant "a constraint on a child" in one table and "which gateway
// an Agent reaches" in another. An Agent's ceiling now lives in
// AgentClass (butlers.go), and 'child' never belonged there: what a
// child may do is a fact about the person, not about the Agent they are
// talking to, and one Agent serves several people at once (§5.3).
const ProfileChild = "child"

// ErrLastOwner is returned when an operation would leave the household
// with no active Owner (design §2.5, §18.1).
//
// It is a named error because this one is worth telling the caller
// about precisely: unlike ErrNotAdmitted, which collapses five states
// so a prober learns nothing, refusing to remove the last Owner
// discloses nothing an administrator does not already know, and the
// readable reason is the difference between a Console that explains
// itself and one that appears broken.
var ErrLastOwner = errors.New("runtimedb: the last active owner cannot be demoted, disabled, or offboarded while no other active owner exists")

// Identity link types. Each names a different way of being
// authenticated, and the difference is what makes "how do we know this
// is them" answerable per link.
const (
	LinkAPIPrincipal   = "api-principal"
	LinkChatBinding    = "chat-binding"
	LinkTrustedIngress = "trusted-ingress"
)

// ErrNotAdmitted is returned when a surface identity resolves to nobody
// the Butler will act for: no link, a stale link, an unadmitted member,
// a disabled one, or an offboarded one.
//
// The reasons collapse into one error on purpose. To the caller they all
// mean "this request gets nothing", and distinguishing them would tell
// someone probing the surface which of those five states they are in.
var ErrNotAdmitted = errors.New("runtimedb: this identity is not admitted to the Butler")

// ButlerMember is one person's Butler admission state.
type ButlerMember struct {
	// PersonID is the stable internal identity (design §2.2). It never
	// changes — not on a rename, not on a re-admission, not when a
	// directory login moves. Member is a handle; this is the person.
	PersonID string `json:"personId"`
	Member   string `json:"member"`
	// Role is 'owner' or 'member'.
	Role string `json:"role"`
	// Profile is the constrained-Member preset, empty for most people.
	// Empty means NULL in storage: no preset, an ordinary Member.
	Profile      string    `json:"profile,omitempty"`
	Admitted     bool      `json:"admitted"`
	Generation   int64     `json:"generation"`
	CreatedAt    time.Time `json:"createdAt"`
	AdmittedAt   time.Time `json:"admittedAt,omitzero"`
	AdmittedBy   string    `json:"admittedBy,omitempty"`
	DisabledAt   time.Time `json:"disabledAt,omitzero"`
	DisabledBy   string    `json:"disabledBy,omitempty"`
	OffboardedAt time.Time `json:"offboardedAt,omitzero"`
	OffboardedBy string    `json:"offboardedBy,omitempty"`
	Guardian     string    `json:"guardian,omitempty"`
}

// Active reports whether this member may have work done for them right
// now. Every one of the three conditions has to hold.
func (m ButlerMember) Active() bool {
	return m.Admitted && m.DisabledAt.IsZero() && m.OffboardedAt.IsZero()
}

// newPersonID mints a stable internal identity. It is opaque on
// purpose: nothing about a Person that can change may be recoverable
// from it, or something downstream will start parsing it instead of
// joining on it.
func newPersonID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("runtimedb: generating a person id: %w", err)
	}
	return "psn_" + hex.EncodeToString(raw), nil
}

// RecordButlerMember creates or updates a person's record WITHOUT
// admitting them. It is how a child record exists before there is a
// child surface, and how an administrator stages someone before the
// approving Plan lands.
//
// profile is the constrained-Member preset and is normally empty. It
// does NOT set the role: a child is recorded with role 'member' and
// profile 'child', because a child is a constrained Member and not a
// third kind of person (design §2.3).
//
// An existing person's role is never changed here. Crossing the Owner
// boundary is an audited act with its own invariant, so it goes through
// SetButlerMemberRole and nowhere else — a staging call must not be
// able to make someone an Owner as a side effect of updating their
// guardian.
func (db *DB) RecordButlerMember(member, role, profile, guardian string) error {
	switch role {
	case RoleOwner, RoleMember:
	default:
		return fmt.Errorf("runtimedb: a person's role must be owner|member, got %q (design §2.3)", role)
	}
	switch profile {
	case "", ProfileChild:
	default:
		return fmt.Errorf("runtimedb: the only profile preset is %q, got %q (design §2.3)", ProfileChild, profile)
	}
	if member == "" {
		return fmt.Errorf("runtimedb: a butler member record requires a member name")
	}
	if profile == ProfileChild && role != RoleMember {
		return fmt.Errorf("runtimedb: a child is a constrained member, not a %s (design §2.3)", role)
	}
	if profile == ProfileChild && guardian == "" {
		return fmt.Errorf("runtimedb: a child record requires a guardian (ADR-W030 A2.1)")
	}
	personID, err := newPersonID()
	if err != nil {
		return err
	}
	// preset is NULL rather than '' when there is no preset: the column
	// is a closed enum plus NULL, and '' is neither.
	var preset any
	if profile != "" {
		preset = profile
	}
	// The guardian is a Person, so it is stored as one. Nobody named is
	// NULL rather than '': since 0043 the column holds an identity, and
	// '' is not one.
	var guardianID any
	if guardian != "" {
		resolved, err := resolvePersonID(db.sql, guardian)
		if err != nil {
			return fmt.Errorf("runtimedb: recording %q: the guardian %w", member, err)
		}
		guardianID = resolved
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_members (person_id, member, role, profile, admitted, generation, created_at, guardian_person_id)
		 VALUES (?, ?, ?, ?, 0, 1, ?, ?)
		 ON CONFLICT(member) DO UPDATE SET profile = excluded.profile, guardian_person_id = excluded.guardian_person_id`,
		personID, member, role, preset, now(), guardianID); err != nil {
		return fmt.Errorf("runtimedb: recording butler member: %w", err)
	}
	return nil
}

// SetButlerMemberRole promotes a Person to Owner or demotes them to
// Member, and is the only path that changes a role.
//
// It refuses to demote the last active Owner (design §2.5). The schema
// refuses that too, and the schema is the authority; this check exists
// so an administrator gets a sentence they can act on instead of a
// constraint failure. Both run inside one immediate transaction, so two
// Owners demoted concurrently serialize and the second one loses.
//
// Demotion is not offboarding (§17.1): this changes the role and writes
// the audit row, and deliberately does not touch admission, links or
// the generation. Owner-only access is rechecked from the live role.
// The Console handler also ends the Person's elevated sessions.
func (db *DB) SetButlerMemberRole(member, role, actor string) error {
	switch role {
	case RoleOwner, RoleMember:
	default:
		return fmt.Errorf("runtimedb: a person's role must be owner|member, got %q (design §2.3)", role)
	}
	if actor == "" {
		return fmt.Errorf("runtimedb: a role change requires the administrator who made it (design §18.1)")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: changing role: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var personID, current, profile string
	var preset sql.NullString
	var admitted int
	var disabledAt, offboardedAt string
	err = tx.QueryRow(
		`SELECT person_id, role, profile, admitted, disabled_at, offboarded_at
		 FROM butler_members WHERE member = ?`, member).
		Scan(&personID, &current, &preset, &admitted, &disabledAt, &offboardedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: changing role: %w", err)
	}
	profile = preset.String
	if current == role {
		// Nothing changed, so there is nothing to audit. Writing a row
		// here would put "promoted to owner" in the trail for someone
		// who already was one.
		return nil
	}
	if profile == ProfileChild && role != RoleMember {
		return fmt.Errorf("runtimedb: %q carries the child preset and cannot be made an %s (design §2.3)", member, role)
	}
	if role == RoleMember && current == RoleOwner {
		wasActive := admitted == 1 && disabledAt == "" && offboardedAt == ""
		if wasActive {
			var others int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM butler_members
				 WHERE role = 'owner' AND admitted = 1 AND disabled_at = '' AND offboarded_at = ''
				   AND member <> ?`, member).Scan(&others); err != nil {
				return fmt.Errorf("runtimedb: counting active owners: %w", err)
			}
			if others == 0 {
				return fmt.Errorf("%w: %s is the only one", ErrLastOwner, member)
			}
		}
	}
	if _, err := tx.Exec(
		`UPDATE butler_members SET role = ? WHERE member = ?`, role, member); err != nil {
		return fmt.Errorf("runtimedb: changing role: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO butler_role_changes (at, person_id, member, actor, old_role, new_role)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		now(), personID, member, actor, current, role); err != nil {
		return fmt.Errorf("runtimedb: auditing role change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: changing role: %w", err)
	}
	return nil
}

// RenameButlerMemberHandle changes the display/login handle. person_id
// does not move, which is the whole reason it exists.
//
// Since 0043 this is one UPDATE of one column, and that is the point of
// the slice. It used to carry butler_identity_links and
// butler_conversations along under defer_foreign_keys, because those
// two held a foreign key on the handle — and it could not carry the
// dozen-odd tables that stored the handle as free text with no key at
// all, so a rename silently orphaned them. Now nothing else stores the
// handle: every provably-human reference names the person, and a
// person who is renamed is still the same person.
//
// Actor columns are the deliberate exception. They keep the text
// recorded at the time, because the value there may be a machine
// principal rather than anybody, and an audit row that says who acted
// should go on saying it after they are renamed.
func (db *DB) RenameButlerMemberHandle(member, newHandle string) error {
	if newHandle == "" {
		return fmt.Errorf("runtimedb: a handle cannot be empty")
	}
	if newHandle == member {
		return nil
	}
	res, err := db.sql.Exec(
		`UPDATE butler_members SET member = ? WHERE member = ?`, newHandle, member)
	if err != nil {
		return fmt.Errorf("runtimedb: renaming %q to %q: %w", member, newHandle, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("runtimedb: renaming %q: %w", member, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ButlerRoleChange is one audited crossing of the Owner boundary.
type ButlerRoleChange struct {
	Seq      int64     `json:"seq"`
	At       time.Time `json:"at"`
	PersonID string    `json:"personId"`
	Member   string    `json:"member"`
	Actor    string    `json:"actor"`
	OldRole  string    `json:"oldRole"`
	NewRole  string    `json:"newRole"`
}

// ListButlerRoleChanges returns one Person's role history, oldest
// first. It takes a person_id rather than a handle because the handle
// may have changed since.
func (db *DB) ListButlerRoleChanges(personID string) ([]ButlerRoleChange, error) {
	rows, err := db.sql.Query(
		`SELECT seq, at, person_id, member, actor, old_role, new_role
		 FROM butler_role_changes WHERE person_id = ? ORDER BY seq`, personID)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing role changes: %w", err)
	}
	defer rows.Close()
	var out []ButlerRoleChange
	for rows.Next() {
		var c ButlerRoleChange
		var at string
		if err := rows.Scan(&c.Seq, &at, &c.PersonID, &c.Member, &c.Actor, &c.OldRole, &c.NewRole); err != nil {
			return nil, err
		}
		if c.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("runtimedb: bad role change timestamp %q: %w", at, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AdmitButlerMember admits a member to the Butler.
//
// It bumps the generation, which is what makes re-admission a NEW
// decision rather than an undo: links written under the old generation
// stay stale, so a person re-admitted after an offboarding starts with
// nothing rather than with whatever they had before.
func (db *DB) AdmitButlerMember(member, admittedBy string) error {
	if admittedBy == "" {
		return fmt.Errorf("runtimedb: admission requires the administrator who approved it (ADR-W030 A2.1)")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: admitting butler member: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var profile sql.NullString
	var generation int64
	err = tx.QueryRow(`SELECT profile, generation FROM butler_members WHERE member = ?`, member).
		Scan(&profile, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: admitting butler member: %w", err)
	}
	if profile.String == ProfileChild {
		return fmt.Errorf("runtimedb: the child surface is not enabled, so %q cannot be admitted (ADR-W030 Part C)", member)
	}
	stamp := now()
	if _, err := tx.Exec(
		`UPDATE butler_members SET admitted = 1, generation = ?, admitted_at = ?, admitted_by = ?,
		     disabled_at = '', disabled_by = '', offboarded_at = '', offboarded_by = ''
		 WHERE member = ?`,
		generation+1, stamp, admittedBy, member); err != nil {
		return fmt.Errorf("runtimedb: admitting butler member: %w", err)
	}
	// The new Member inherits the household's default access group
	// (ADR-W033 design §18.2, slice 8). In the same transaction as the
	// admission, because a Person who is admitted but in no group is a
	// state with no meaning: entitlements default to deny, so they would
	// be a member of the household who can reach nothing, and nothing
	// would ever come back to finish the job.
	//
	// Membership still grants nothing by itself (§4). What `Family`
	// reaches is whatever the entitlement rows say about it, and this
	// line has no opinion on that.
	if err := addToDefaultGroupTx(tx, member, admittedBy, stamp); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: admitting butler member: %w", err)
	}
	return nil
}

// addToDefaultGroupTx is AddPersonToDefaultGroup inside a transaction,
// keyed on the handle the caller already has.
//
// A household with no `Family` group is not an error. 0051 seeds it, so
// its absence means somebody removed it deliberately, and refusing to
// admit anybody until they put it back would be a strange thing for that
// to cause.
func addToDefaultGroupTx(tx *sql.Tx, member, actor, stamp string) error {
	var personID string
	if err := tx.QueryRow(`SELECT person_id FROM butler_members WHERE member = ?`, member).
		Scan(&personID); err != nil {
		return fmt.Errorf("runtimedb: reading the person admitted: %w", err)
	}
	// Asked of the schema rather than inferred from a failed query. A
	// database below 0051 has no groups at all, and "no such table"
	// arrives as a generic SQL error indistinguishable from a real one
	// — folding those together is the mistake GroupBySeedKey already
	// made once (TestAFailedGroupLookupIsNotAMissingGroup).
	var hasGroups int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'person_groups'`).
		Scan(&hasGroups); err != nil {
		return fmt.Errorf("runtimedb: checking for the group namespace: %w", err)
	}
	if hasGroups == 0 {
		return nil
	}
	var groupID string
	err := tx.QueryRow(`SELECT group_id FROM person_groups WHERE seed_key = ?`, SeedFamily).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runtimedb: looking up the default access group: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO person_group_members (group_id, person_id, added_at, added_by)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (group_id, person_id) DO NOTHING`,
		groupID, personID, stamp, actor); err != nil {
		return fmt.Errorf("runtimedb: adding %q to the default access group: %w", member, err)
	}
	return nil
}

// DisableButlerMember withdraws Butler access without offboarding. It
// bumps the generation, so every link and derived grant goes stale at
// the next check.
func (db *DB) DisableButlerMember(member, disabledBy string) error {
	return db.denyMember(member, disabledBy, "disabled_at", "disabled_by")
}

// OffboardButlerMember is the runtime denial half of offboarding
// (ADR-W030 A2.1): one audited action that immediately stops retrieval,
// dispatch, ingestion and proactive delivery for this person.
//
// It does not clean up desired state — that follows through Plans — and
// deliberately does not depend on that cleanup succeeding. Failed
// cleanup never restores access, so the denial is recorded here first
// and separately.
func (db *DB) OffboardButlerMember(member, offboardedBy string) error {
	return db.denyMember(member, offboardedBy, "offboarded_at", "offboarded_by")
}

func (db *DB) denyMember(member, by, atColumn, byColumn string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: withdrawing butler access requires the administrator who did it (ADR-W030 A2.1)")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: withdrawing butler access: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var generation int64
	var role string
	var admitted int
	var disabledAt, offboardedAt string
	err = tx.QueryRow(
		`SELECT generation, role, admitted, disabled_at, offboarded_at
		 FROM butler_members WHERE member = ?`, member).
		Scan(&generation, &role, &admitted, &disabledAt, &offboardedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: withdrawing butler access: %w", err)
	}
	// Disabling and offboarding both strip an Owner of Owner authority
	// just as completely as a demotion does, so the Last Owner
	// invariant covers them on the same terms (design §2.5, §18.1). The
	// schema refuses it either way; this is the readable half.
	if role == RoleOwner && admitted == 1 && disabledAt == "" && offboardedAt == "" {
		var others int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM butler_members
			 WHERE role = 'owner' AND admitted = 1 AND disabled_at = '' AND offboarded_at = ''
			   AND member <> ?`, member).Scan(&others); err != nil {
			return fmt.Errorf("runtimedb: counting active owners: %w", err)
		}
		if others == 0 {
			return fmt.Errorf("%w: %s is the only one", ErrLastOwner, member)
		}
	}
	stamp := now()
	// admitted goes to 0 in the same statement as the timestamp: a
	// member whose denial and admission flag disagree is a race nobody
	// should have to reason about.
	if _, err := tx.Exec(
		`UPDATE butler_members SET admitted = 0, generation = ?, `+atColumn+` = ?, `+byColumn+` = ?
		 WHERE member = ?`, generation+1, stamp, by, member); err != nil {
		return fmt.Errorf("runtimedb: withdrawing butler access: %w", err)
	}
	// Every live link is revoked with it. Leaving them and relying on
	// the generation check would work, and would also leave a Console
	// listing someone's chat account as still bound after they were
	// offboarded.
	if _, err := tx.Exec(
		`UPDATE butler_identity_links SET revoked_at = ?, revoked_by = ?
		 WHERE person_id = (SELECT person_id FROM butler_members WHERE member = ?)
		   AND revoked_at = ''`, stamp, by, member); err != nil {
		return fmt.Errorf("runtimedb: revoking identity links: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: withdrawing butler access: %w", err)
	}
	return nil
}

// GetButlerMember returns one member's record.
func (db *DB) GetButlerMember(member string) (ButlerMember, error) {
	return scanMember(db.sql.QueryRow(memberColumns+` WHERE b.member = ?`, member))
}

// The guardian is stored as an identity and read back as the handle it
// currently carries, so a guardian who is renamed stays the guardian and
// the caller above never sees the id. COALESCE because the column is
// NULL for everybody who has no guardian, which is most people.
const memberColumns = `SELECT b.person_id, b.member, b.role, b.profile, b.admitted, b.generation,
	 b.created_at, b.admitted_at, b.admitted_by, b.disabled_at, b.disabled_by,
	 b.offboarded_at, b.offboarded_by,
	 COALESCE((SELECT g.member FROM butler_members g WHERE g.person_id = b.guardian_person_id), '')
	 FROM butler_members b`

func scanMember(row rowScanner) (ButlerMember, error) {
	var m ButlerMember
	var admitted int
	var created, admittedAt, disabledAt, offboardedAt string
	// profile is NULL for anyone with no preset, which is most people.
	var preset sql.NullString
	err := row.Scan(&m.PersonID, &m.Member, &m.Role, &preset, &admitted, &m.Generation, &created, &admittedAt,
		&m.AdmittedBy, &disabledAt, &m.DisabledBy, &offboardedAt, &m.OffboardedBy, &m.Guardian)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerMember{}, ErrNotFound
	}
	if err != nil {
		return ButlerMember{}, fmt.Errorf("runtimedb: scanning butler member: %w", err)
	}
	m.Profile = preset.String
	m.Admitted = admitted != 0
	for _, f := range []struct {
		text string
		into *time.Time
	}{
		{created, &m.CreatedAt}, {admittedAt, &m.AdmittedAt},
		{disabledAt, &m.DisabledAt}, {offboardedAt, &m.OffboardedAt},
	} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return ButlerMember{}, fmt.Errorf("runtimedb: bad butler member timestamp %q: %w", f.text, err)
		}
		*f.into = parsed
	}
	return m, nil
}

// ListButlerMembers returns every member record, admitted or not.
func (db *DB) ListButlerMembers() ([]ButlerMember, error) {
	rows, err := db.sql.Query(memberColumns + ` ORDER BY b.member`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler members: %w", err)
	}
	defer rows.Close()
	var out []ButlerMember
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ButlerIdentityLink is one authenticated surface identity bound to a
// member.
type ButlerIdentityLink struct {
	Seq        int64     `json:"seq"`
	LinkType   string    `json:"linkType"`
	ExternalID string    `json:"externalId"`
	Member     string    `json:"member"`
	Generation int64     `json:"generation"`
	CreatedAt  time.Time `json:"createdAt"`
	CreatedBy  string    `json:"createdBy"`
	RevokedAt  time.Time `json:"revokedAt,omitzero"`
	RevokedBy  string    `json:"revokedBy,omitempty"`
}

// LinkSurfaceIdentity records that an authenticated surface identity IS
// a member. createdBy is the administrator who approved it, and there is
// no code path that passes the Butler here.
func (db *DB) LinkSurfaceIdentity(linkType, externalID, member, createdBy string) error {
	switch linkType {
	case LinkAPIPrincipal, LinkChatBinding, LinkTrustedIngress:
	default:
		return fmt.Errorf("runtimedb: link type must be api-principal|chat-binding|trusted-ingress, got %q", linkType)
	}
	if externalID == "" || member == "" || createdBy == "" {
		return fmt.Errorf("runtimedb: an identity link requires an external id, a member, and the administrator who approved it (ADR-W030 A2.1)")
	}
	m, err := db.GetButlerMember(member)
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(
		`INSERT INTO butler_identity_links (link_type, external_id, person_id, generation, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		linkType, externalID, m.PersonID, m.Generation, now(), createdBy)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("runtimedb: %s %q is already linked to someone", linkType, externalID)
		}
		return fmt.Errorf("runtimedb: linking surface identity: %w", err)
	}
	return nil
}

// RevokeSurfaceLink withdraws one link and bumps the member's
// generation, so anything derived from that link goes stale immediately
// rather than at the end of a session.
func (db *DB) RevokeSurfaceLink(linkType, externalID, revokedBy string) error {
	if revokedBy == "" {
		return fmt.Errorf("runtimedb: revoking a link requires who did it")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: revoking surface link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var person string
	err = tx.QueryRow(
		`SELECT person_id FROM butler_identity_links
		 WHERE link_type = ? AND external_id = ? AND revoked_at = ''`, linkType, externalID).Scan(&person)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: revoking surface link: %w", err)
	}
	stamp := now()
	if _, err := tx.Exec(
		`UPDATE butler_identity_links SET revoked_at = ?, revoked_by = ?
		 WHERE link_type = ? AND external_id = ? AND revoked_at = ''`,
		stamp, revokedBy, linkType, externalID); err != nil {
		return fmt.Errorf("runtimedb: revoking surface link: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE butler_members SET generation = generation + 1 WHERE person_id = ?`, person); err != nil {
		return fmt.Errorf("runtimedb: revoking surface link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: revoking surface link: %w", err)
	}
	return nil
}

// ResolveSurfaceIdentity turns an authenticated surface identity into
// the member it belongs to, or ErrNotAdmitted.
//
// This is the call A2.1 means by "check current membership and link
// generation at every retrieval and dispatch". It is cheap and it is
// meant to be made every time, not cached: a cached answer is exactly
// the thing that keeps someone talking to the Butler after they were
// offboarded.
func (db *DB) ResolveSurfaceIdentity(linkType, externalID string) (ButlerMember, error) {
	var member string
	var generation int64
	err := db.sql.QueryRow(
		`SELECT m.member, l.generation
		 FROM butler_identity_links l JOIN butler_members m ON m.person_id = l.person_id
		 WHERE l.link_type = ? AND l.external_id = ? AND l.revoked_at = ''`,
		linkType, externalID).Scan(&member, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerMember{}, ErrNotAdmitted
	}
	if err != nil {
		return ButlerMember{}, fmt.Errorf("runtimedb: resolving surface identity: %w", err)
	}
	m, err := db.GetButlerMember(member)
	if errors.Is(err, ErrNotFound) {
		return ButlerMember{}, ErrNotAdmitted
	}
	if err != nil {
		return ButlerMember{}, err
	}
	if m.Generation != generation || !m.Active() {
		return ButlerMember{}, ErrNotAdmitted
	}
	return m, nil
}

// ResolveChatBinding is ResolveSurfaceIdentity for a chat binding id,
// which is an integer everywhere else in the codebase.
func (db *DB) ResolveChatBinding(bindingID int64) (ButlerMember, error) {
	return db.ResolveSurfaceIdentity(LinkChatBinding, strconv.FormatInt(bindingID, 10))
}

// ListSurfaceLinks returns a member's links, live ones first.
func (db *DB) ListSurfaceLinks(member string) ([]ButlerIdentityLink, error) {
	rows, err := db.sql.Query(
		`SELECT l.seq, l.link_type, l.external_id, m.member, l.generation,
		        l.created_at, l.created_by, l.revoked_at, l.revoked_by
		 FROM butler_identity_links l JOIN butler_members m ON m.person_id = l.person_id
		 WHERE m.member = ? ORDER BY l.revoked_at <> '', l.seq`, member)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing surface links: %w", err)
	}
	defer rows.Close()
	var out []ButlerIdentityLink
	for rows.Next() {
		var l ButlerIdentityLink
		var created, revoked string
		if err := rows.Scan(&l.Seq, &l.LinkType, &l.ExternalID, &l.Member, &l.Generation,
			&created, &l.CreatedBy, &revoked, &l.RevokedBy); err != nil {
			return nil, err
		}
		if l.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		if revoked != "" {
			if l.RevokedAt, err = time.Parse(time.RFC3339Nano, revoked); err != nil {
				return nil, err
			}
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ButlerMembersByHandleFold returns every person whose handle matches
// case-insensitively.
//
// It returns a LIST because the schema permits more than one match.
// ADR-W032 A4 records that member names are compared exactly and that
// `Admin` and `admin` are therefore two records; normalising them is
// listed there as a thing to fix regardless, and has not been done. A
// caller that asked for one row would have to pick between them, and
// picking is exactly the wrong move for an authorization check.
//
// This exists for checks that REFUSE. A caller deciding whether someone
// may be let in must not use it to decide they may: equal names are not
// proof of identity (ADR-W030 A2.1), and a looser comparison is looser
// proof. Used the other way it is sound, and strictly safer than an
// exact match — a person the household has disabled cannot slip past a
// lifecycle check by being spelled with a different capital letter.
//
// COLLATE NOCASE is ASCII-only, which is what handles are here.
func (db *DB) ButlerMembersByHandleFold(handle string) ([]ButlerMember, error) {
	rows, err := db.sql.Query(memberColumns+` WHERE b.member = ? COLLATE NOCASE ORDER BY b.member`, handle)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: matching butler members by handle: %w", err)
	}
	defer rows.Close()
	var out []ButlerMember
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// HouseholdHasAnOwner reports whether any active Owner is on record.
//
// It answers one question: has this household been provisioned at all?
// False means nobody has ever been admitted as an Owner — a fresh
// deployment, before the installer or an operator has created the first
// one. It is not a way to ask whether a PARTICULAR person may do
// something, and it must never be used as one.
//
// The answer is durable in one direction, which is what makes it usable
// as a gate. The Last Owner invariant refuses to demote, disable or
// offboard the only active Owner, so once this is true it stays true:
// the un-provisioned state is a window a household leaves once and
// cannot re-enter by any supported path.
func (db *DB) HouseholdHasAnOwner() (bool, error) {
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_members
		 WHERE role = 'owner' AND admitted = 1 AND disabled_at = '' AND offboarded_at = ''`).
		Scan(&n); err != nil {
		return false, fmt.Errorf("runtimedb: counting active owners: %w", err)
	}
	return n > 0, nil
}
