package runtimedb

// Who may reach which service (ADR-W033 design §4, §17.3, §18.2; slice 8).
//
// The entitlement half of the household's one principal namespace. §4
// requires that when entitlements, memory audiences and secret grants
// all say `Adults`, they mean the same people — so membership is read
// through slice 6's ResolveGroupMembers and never re-derived here.
//
// AND IT GRANTS NOTHING ELSE, which is the half worth defending. §4:
//
//	One namespace is not one permission. Membership of `Adults` is an
//	*identity primitive*; what it authorizes is decided by each
//	resource separately.
//
// So an entitlement says "may open the password manager" and says
// nothing about memory or secrets. There is no call from this file into
// §11 or §12 and there must not be one: the moment a service entitlement
// also widened a memory audience, the three rules would stop being
// separable and the namespace would have become a permission model
// nobody designed.
//
// DEFAULT DENY. A person with no row and no entitled group reaches
// nothing. That is not a strict reading of an ambiguous design — it is
// the only reading that fails safe, and the one §18.2's "a direct URL to
// an unauthorized service is denied" requires.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Entitlement subjects and effects, as stored.
const (
	EntitlementSubjectGroup  = "group"
	EntitlementSubjectPerson = "person"

	// EntitlementAllow admits the subject.
	EntitlementAllow = "allow"
	// EntitlementRestrict refuses them, and outranks an allow inherited
	// from a group (§4, "explicit restriction overrides inherited
	// allow"). It is a row rather than the absence of one: a refusal
	// expressed by deleting the group's allow would refuse the whole
	// group.
	EntitlementRestrict = "restrict"
)

// Entitlement is one decision about one subject and one service.
type Entitlement struct {
	Seq     int64  `json:"seq"`
	Service string `json:"service"`
	// SubjectKind is "group" or "person"; Subject is a group_id or a
	// person_id. Ids, never names: a household that renames `Adults`
	// must not thereby re-point every rule that named it.
	SubjectKind string `json:"subjectKind"`
	Subject     string `json:"subject"`
	Effect      string `json:"effect"`
	// DormantAt is set while the service is not installed (§17.3). A
	// dormant entitlement grants nothing and is not deleted.
	DormantAt time.Time `json:"dormantAt,omitzero"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy,omitempty"`
}

// Dormant reports whether this row is currently inert.
func (e Entitlement) Dormant() bool { return !e.DormantAt.IsZero() }

// EntitledTo reports whether a Person may reach a service.
//
// THE one entitlement resolver, for the reason slice 6 has one
// membership resolver and slice 7 one audience resolver: two answers to
// "may they?" differ on some edge nobody notices until somebody reaches
// a service they were restricted from.
//
// PRECEDENCE, strongest first:
//
//  1. a restriction naming this Person
//  2. an allow naming this Person
//  3. a restriction on any group they are in
//  4. an allow on any group they are in
//  5. deny
//
// Explicit beats inherited in BOTH directions, which is what §4's
// "explicit restriction overrides inherited allow" is really saying: the
// row naming the person is the more specific statement, so it wins
// whichever way it points. A household that restricts `Kids` from the
// password manager and then allows one teenager by name has said
// something coherent, and this honours it. Within a level, restrict
// beats allow.
//
// Reads live, holds no cache — removing somebody from a group has to
// take effect at the next evaluation, exactly as in slices 6 and 7.
func (db *DB) EntitledTo(personID, service string) (bool, error) {
	if personID == "" || service == "" {
		// Not an error: an unauthenticated caller and an unnamed
		// service are both real states, and both reach nothing.
		return false, nil
	}
	rows, err := db.sql.Query(
		`SELECT subject_kind, subject, effect
		   FROM service_entitlements
		  WHERE service = ? AND dormant_at = ''`, service)
	if err != nil {
		return false, fmt.Errorf("runtimedb: resolving entitlement to %q: %w", service, err)
	}
	defer rows.Close()

	var personEffect string
	groupEffects := map[string]string{}
	for rows.Next() {
		var kind, subject, effect string
		if err := rows.Scan(&kind, &subject, &effect); err != nil {
			return false, fmt.Errorf("runtimedb: resolving entitlement to %q: %w", service, err)
		}
		switch kind {
		case EntitlementSubjectPerson:
			if subject == personID {
				personEffect = effect
			}
		case EntitlementSubjectGroup:
			groupEffects[subject] = effect
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("runtimedb: resolving entitlement to %q: %w", service, err)
	}

	// Levels 1 and 2: the row naming this Person, either way.
	if personEffect != "" {
		return personEffect == EntitlementAllow, nil
	}
	if len(groupEffects) == 0 {
		return false, nil
	}

	// Levels 3 and 4. Membership goes through slice 6's resolver — which
	// returns ACTIVE people only, so an offboarded person holds no
	// inherited entitlement even while their membership row survives.
	allowed := false
	for groupID, effect := range groupEffects {
		in, err := db.PersonIsInGroup(personID, groupID)
		if err != nil {
			return false, err
		}
		if !in {
			continue
		}
		if effect == EntitlementRestrict {
			// A restriction on any group they are in ends it. No
			// "but another group allows them": a household that put
			// somebody in a restricted group meant the restriction,
			// and the way to make an exception is the row that names
			// them, which was already checked above.
			return false, nil
		}
		allowed = true
	}
	return allowed, nil
}

// SetEntitlement records one decision, replacing whatever that subject
// had for that service.
//
// Upsert rather than insert, because a subject cannot be both allowed
// and restricted from one service. That contradiction is resolved when
// it is written — the later decision is the household's current one —
// rather than left for every read to adjudicate.
//
// Writing a decision also WAKES a dormant row. A household that changes
// its mind about who may use a service while that service is uninstalled
// has stated something about the service they are going to reinstall,
// and burying it would be a strange way to receive it.
func (db *DB) SetEntitlement(service, subjectKind, subject, effect, actor string) error {
	switch subjectKind {
	case EntitlementSubjectGroup, EntitlementSubjectPerson:
	default:
		return fmt.Errorf("runtimedb: an entitlement subject is a group or a person, got %q", subjectKind)
	}
	switch effect {
	case EntitlementAllow, EntitlementRestrict:
	default:
		return fmt.Errorf("runtimedb: an entitlement is allow or restrict, got %q", effect)
	}
	if service == "" || subject == "" {
		return fmt.Errorf("runtimedb: an entitlement needs a service and a subject")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.sql.Exec(
		`INSERT INTO service_entitlements
		     (service, subject_kind, subject, effect, dormant_at, created_at, created_by)
		 VALUES (?, ?, ?, ?, '', ?, ?)
		 ON CONFLICT (service, subject_kind, subject)
		 DO UPDATE SET effect = excluded.effect, dormant_at = '',
		               created_at = excluded.created_at, created_by = excluded.created_by`,
		service, subjectKind, subject, effect, now, actor); err != nil {
		return fmt.Errorf("runtimedb: recording entitlement to %q: %w", service, err)
	}
	return nil
}

// RemoveEntitlement deletes a decision outright.
//
// Distinct from restricting, and from going dormant. This is "the
// household never decided anything about this subject", which returns
// them to whatever their groups say. Restricting is "the household
// decided no". Conflating the two would make un-restricting somebody
// impossible to express.
func (db *DB) RemoveEntitlement(service, subjectKind, subject string) error {
	if _, err := db.sql.Exec(
		`DELETE FROM service_entitlements
		  WHERE service = ? AND subject_kind = ? AND subject = ?`,
		service, subjectKind, subject); err != nil {
		return fmt.Errorf("runtimedb: removing entitlement to %q: %w", service, err)
	}
	return nil
}

// ListEntitlements returns every decision recorded for a service,
// dormant ones included — a Console showing what a household has decided
// should show what is waiting to come back, not hide it.
func (db *DB) ListEntitlements(service string) ([]Entitlement, error) {
	rows, err := db.sql.Query(
		`SELECT seq, service, subject_kind, subject, effect, dormant_at, created_at, created_by
		   FROM service_entitlements WHERE service = ?
		  ORDER BY subject_kind, subject`, service)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing entitlements for %q: %w", service, err)
	}
	defer rows.Close()
	return scanEntitlements(rows, service)
}

// EntitledServices lists the services a Person may reach.
//
// What the Homepage renders — and PRESENTATION ONLY, which is worth
// saying at the function a card list will be built from. §18.2 requires
// that a direct URL to an unauthorized service is denied regardless of
// what the Homepage drew, so this is never the enforcement point. The
// authorization answer is EntitledTo, asked again where access happens.
func (db *DB) EntitledServices(personID string) ([]string, error) {
	rows, err := db.sql.Query(
		`SELECT DISTINCT service FROM service_entitlements WHERE dormant_at = '' ORDER BY service`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing services: %w", err)
	}
	defer rows.Close()
	var services []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("runtimedb: listing services: %w", err)
		}
		services = append(services, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runtimedb: listing services: %w", err)
	}
	// Asked through the one resolver rather than joined in SQL. A join
	// would be a second implementation of the precedence rules, agreeing
	// with EntitledTo right up until one of them was edited.
	var out []string
	for _, s := range services {
		ok, err := db.EntitledTo(personID, s)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// SeedServiceEntitlements records the groups a package declares it is
// for, and wakes anything dormant for that service (§17.3).
//
// Called when a capability is installed. It is the join between the
// manifest's `accessGroups` — what the service is FOR — and this table,
// which is what the household has DECIDED. The two are different things
// and the difference matters on reinstall: seeding must not overwrite a
// decision, so an existing row keeps its effect and only loses its
// dormancy. A household that restricted somebody from the media library,
// uninstalled it, and put it back has not thereby un-restricted them.
//
// Idempotent, so an installer that runs twice is not a second opinion.
func (db *DB) SeedServiceEntitlements(service string, seedKeys []string, actor string) error {
	if service == "" {
		return fmt.Errorf("runtimedb: seeding entitlements needs a service")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: seeding entitlements for %q: %w", service, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Waking comes first, and covers every row rather than only the
	// declared groups: a person named individually is exactly the
	// assignment §17.3 promises to restore, and they are in no group
	// the manifest could have mentioned.
	if _, err := tx.Exec(
		`UPDATE service_entitlements SET dormant_at = '' WHERE service = ?`, service); err != nil {
		return fmt.Errorf("runtimedb: restoring entitlements for %q: %w", service, err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, key := range seedKeys {
		var groupID string
		err := tx.QueryRow(`SELECT group_id FROM person_groups WHERE seed_key = ?`, key).Scan(&groupID)
		if errors.Is(err, sql.ErrNoRows) {
			// A manifest naming a group this deployment does not have.
			// Lint refuses it at authoring time; here it is a fact
			// about a catalog already on disk, and refusing the whole
			// install over it would be worse than installing a service
			// nobody is entitled to yet — which is a state an Owner can
			// see and fix, and which discloses nothing.
			continue
		}
		if err != nil {
			return fmt.Errorf("runtimedb: looking up the %q group: %w", key, err)
		}
		// DO NOTHING, not DO UPDATE: seeding states a default and must
		// never overwrite what the household decided.
		if _, err := tx.Exec(
			`INSERT INTO service_entitlements
			     (service, subject_kind, subject, effect, dormant_at, created_at, created_by)
			 VALUES (?, 'group', ?, 'allow', '', ?, ?)
			 ON CONFLICT (service, subject_kind, subject) DO NOTHING`,
			service, groupID, now, actor); err != nil {
			return fmt.Errorf("runtimedb: seeding entitlement to %q: %w", service, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: seeding entitlements for %q: %w", service, err)
	}
	return nil
}

// MakeServiceDormant marks a removed service's entitlements inert
// (§17.3, §17.4).
//
// Not a delete, and that is the whole behaviour: "removing a package
// does not erase its logical assignments". A household that uninstalls
// the photo library for a month and puts it back should not have to
// remember who could see the photos.
func (db *DB) MakeServiceDormant(service string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.sql.Exec(
		`UPDATE service_entitlements SET dormant_at = ?
		  WHERE service = ? AND dormant_at = ''`, now, service); err != nil {
		return fmt.Errorf("runtimedb: making entitlements for %q dormant: %w", service, err)
	}
	return nil
}

// AddPersonToDefaultGroup puts a Person in the household's default
// access group (§18.2, "a new Member inherits the default access
// group").
//
// `Family` by seed key rather than by name, so a household that renamed
// it still has one. A household with no `Family` group at all is not an
// error: the group is seeded by migration 0051 and its absence means
// somebody removed it on purpose, which is theirs to have done.
//
// Idempotent, and grants nothing by itself — membership is an identity
// primitive, and what `Family` reaches is whatever the entitlement rows
// say about it.
func (db *DB) AddPersonToDefaultGroup(personID, actor string) error {
	g, err := db.GroupBySeedKey(SeedFamily)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return db.AddPersonToGroup(g.ID, personID, actor)
}

func scanEntitlements(rows *sql.Rows, service string) ([]Entitlement, error) {
	var out []Entitlement
	for rows.Next() {
		var e Entitlement
		var dormant, created string
		if err := rows.Scan(&e.Seq, &e.Service, &e.SubjectKind, &e.Subject, &e.Effect,
			&dormant, &created, &e.CreatedBy); err != nil {
			return nil, fmt.Errorf("runtimedb: reading entitlements for %q: %w", service, err)
		}
		if dormant != "" {
			e.DormantAt, _ = time.Parse(time.RFC3339Nano, dormant)
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runtimedb: reading entitlements for %q: %w", service, err)
	}
	return out, nil
}
