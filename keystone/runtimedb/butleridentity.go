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
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Butler profiles (ADR-W030 A1). The ceiling a member runs under, not a
// preference they can change.
const (
	ProfileHousehold = "household"
	ProfileOperator  = "operator"
	// ProfileChild is stored so guardian relationships can be recorded,
	// and cannot be admitted: the schema refuses an admitted child until
	// that surface has its own evidence.
	ProfileChild = "child"
)

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
	Member       string    `json:"member"`
	Profile      string    `json:"profile"`
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

// RecordButlerMember creates or updates a member's record WITHOUT
// admitting them. It is how a child record exists before there is a
// child surface, and how an administrator stages someone before the
// approving Plan lands.
func (db *DB) RecordButlerMember(member, profile, guardian string) error {
	switch profile {
	case ProfileHousehold, ProfileOperator, ProfileChild:
	default:
		return fmt.Errorf("runtimedb: butler profile must be household|operator|child, got %q", profile)
	}
	if member == "" {
		return fmt.Errorf("runtimedb: a butler member record requires a member name")
	}
	if profile == ProfileChild && guardian == "" {
		return fmt.Errorf("runtimedb: a child record requires a guardian (ADR-W030 A2.1)")
	}
	_, err := db.sql.Exec(
		`INSERT INTO butler_members (member, profile, admitted, generation, created_at, guardian)
		 VALUES (?, ?, 0, 1, ?, ?)
		 ON CONFLICT(member) DO UPDATE SET profile = excluded.profile, guardian = excluded.guardian`,
		member, profile, now(), guardian)
	if err != nil {
		return fmt.Errorf("runtimedb: recording butler member: %w", err)
	}
	return nil
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

	var profile string
	var generation int64
	err = tx.QueryRow(`SELECT profile, generation FROM butler_members WHERE member = ?`, member).
		Scan(&profile, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: admitting butler member: %w", err)
	}
	if profile == ProfileChild {
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: admitting butler member: %w", err)
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
	err = tx.QueryRow(`SELECT generation FROM butler_members WHERE member = ?`, member).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runtimedb: withdrawing butler access: %w", err)
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
		 WHERE member = ? AND revoked_at = ''`, stamp, by, member); err != nil {
		return fmt.Errorf("runtimedb: revoking identity links: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: withdrawing butler access: %w", err)
	}
	return nil
}

// GetButlerMember returns one member's record.
func (db *DB) GetButlerMember(member string) (ButlerMember, error) {
	return scanMember(db.sql.QueryRow(memberColumns+` WHERE member = ?`, member))
}

const memberColumns = `SELECT member, profile, admitted, generation, created_at, admitted_at,
	 admitted_by, disabled_at, disabled_by, offboarded_at, offboarded_by, guardian
	 FROM butler_members`

func scanMember(row rowScanner) (ButlerMember, error) {
	var m ButlerMember
	var admitted int
	var created, admittedAt, disabledAt, offboardedAt string
	err := row.Scan(&m.Member, &m.Profile, &admitted, &m.Generation, &created, &admittedAt,
		&m.AdmittedBy, &disabledAt, &m.DisabledBy, &offboardedAt, &m.OffboardedBy, &m.Guardian)
	if errors.Is(err, sql.ErrNoRows) {
		return ButlerMember{}, ErrNotFound
	}
	if err != nil {
		return ButlerMember{}, fmt.Errorf("runtimedb: scanning butler member: %w", err)
	}
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
	rows, err := db.sql.Query(memberColumns + ` ORDER BY member`)
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
		`INSERT INTO butler_identity_links (link_type, external_id, member, generation, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		linkType, externalID, member, m.Generation, now(), createdBy)
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

	var member string
	err = tx.QueryRow(
		`SELECT member FROM butler_identity_links
		 WHERE link_type = ? AND external_id = ? AND revoked_at = ''`, linkType, externalID).Scan(&member)
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
		`UPDATE butler_members SET generation = generation + 1 WHERE member = ?`, member); err != nil {
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
		`SELECT member, generation FROM butler_identity_links
		 WHERE link_type = ? AND external_id = ? AND revoked_at = ''`,
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
		`SELECT seq, link_type, external_id, member, generation, created_at, created_by, revoked_at, revoked_by
		 FROM butler_identity_links WHERE member = ? ORDER BY revoked_at <> '', seq`, member)
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
