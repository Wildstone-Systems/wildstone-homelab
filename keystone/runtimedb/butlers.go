package runtimedb

// Butlers: user-created specialists (ADR-W031, work package B1).
//
// A Butler is a bot with a name, a directive, one assigned model, a
// surface it lives on, and a list of people who may talk to it. The
// household creates as many as it needs — a health Butler, a nutrition
// Butler, an accounting Butler — and a routine is assigned to one.
//
// The model is ASSIGNED and never chosen per call (A2). That is the
// load-bearing decision and it is worth restating where the code lives:
// anything that decides which model should handle a message must read
// the message to decide, so the deciding step is itself the disclosure
// it was meant to prevent. An assignment has no such step, and asked
// which model saw a piece of material it answers by name rather than
// "whichever was free".
//
// Everything here is written for the Console holder, who owns every
// Butler (A10). A Butler cannot create or modify a Butler; that path is
// Plan-mediated and lives above this layer, because without it
// self-escalation is one step — a Butler creates a cloud-model Butler
// and hands it the material a local one held.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrButlerUnreachable reports that a Butler exists but cannot answer:
// retired, disabled, or its model is not available.
//
// It is separate from ErrNotFound because the two mean opposite things
// to a caller. Not-found is "no such Butler"; this is "that Butler is
// real and is not answering", which A6 says must produce a WAIT and a
// report rather than a substitution.
var ErrButlerUnreachable = errors.New("runtimedb: that butler cannot answer right now")

// ErrNoButlerAccess reports that this person may not talk to this
// Butler.
//
// Like a review token, it is deliberately indistinguishable from
// not-found at the edge: "that is not yours" confirms the Butler exists
// and roughly what it is for.
var ErrNoButlerAccess = errors.New("runtimedb: that person has no access to that butler")

// Butler is one specialist.
type Butler struct {
	Seq  int64  `json:"seq"`
	Name string `json:"name"`
	// Directive is what this Butler is for, in its owner's words. It
	// instructs and authorises nothing (A9).
	Directive string        `json:"directive,omitempty"`
	Surface   ButlerSurface `json:"surface"`
	Profile   string        `json:"profile"`
	// Route is the assigned model, by name. RouteKind is "local" or
	// "cloud" — the data boundary, and what a local-to-cloud warning
	// keys off.
	Route     string `json:"route"`
	RouteSeq  int64  `json:"routeSeq"`
	RouteKind string `json:"routeKind"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	// Endpoint is the route's base URL, carried because a Butler that
	// cannot be TALKED to is not a Butler. Without it the assignment
	// names a provider and a model and still needs a second lookup to
	// reach either, and the caller that forgets the lookup falls back to
	// whatever the process was started with — which is the substitution
	// A6 exists to forbid, arrived at by omission.
	Endpoint  string `json:"endpoint,omitempty"`
	Readiness string `json:"readiness"`
	// RouteEnabled is the ROUTE's switch, not the Butler's. Disabling a
	// route has to stop the Butlers already assigned to it: the 0032
	// triggers only fire when a butler's route is written, so without
	// this a route turned off yesterday still answers today.
	RouteEnabled bool      `json:"routeEnabled"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"createdAt"`
	CreatedBy    string    `json:"createdBy"`
	UpdatedAt    time.Time `json:"updatedAt"`
	UpdatedBy    string    `json:"updatedBy,omitempty"`
	RetiredAt    time.Time `json:"retiredAt,omitzero"`
	RetiredBy    string    `json:"retiredBy,omitempty"`
}

// Live reports whether this Butler is available to be talked to at all.
// It says nothing about the model behind it — that is Reachable.
func (b Butler) Live() bool { return b.Enabled && b.RetiredAt.IsZero() }

// Reachable reports whether a message to this Butler can actually be
// answered: it is live AND its model has not been found unavailable.
//
// An unprobed or QA-only route is still reachable. Those verdicts bound
// what a session may DO (ADR-W017 A4) and do not mean the model is
// missing, which is a different failure with a different remedy.
func (b Butler) Reachable() bool {
	return b.Live() && b.RouteEnabled && b.Readiness != RouteUnavailable
}

// CarriesActionAuthority reports whether a session with this Butler may
// use tools that do things. Only a route that passed its probe.
func (b Butler) CarriesActionAuthority() bool {
	return b.Live() && b.Readiness == RouteReady
}

// butlerColumns is the join every read uses. A Butler is only ever
// meaningful with its route resolved — a Butler whose model you have to
// look up separately is one a caller will eventually forget to look up.
const butlerColumns = `
	SELECT b.seq, b.name, b.directive, b.surface, b.profile,
	       r.name, r.seq, r.kind, r.model, r.provider, r.endpoint, r.readiness, r.enabled,
	       b.enabled, b.created_at, b.created_by, b.updated_at, b.updated_by,
	       b.retired_at, b.retired_by
	  FROM butlers b JOIN model_routes r ON r.seq = b.route`

func scanButler(s interface{ Scan(...any) error }) (Butler, error) {
	var b Butler
	var surface string
	var enabled, routeEnabled int
	var created, updated, retired string
	if err := s.Scan(&b.Seq, &b.Name, &b.Directive, &surface, &b.Profile,
		&b.Route, &b.RouteSeq, &b.RouteKind, &b.Model, &b.Provider, &b.Endpoint, &b.Readiness, &routeEnabled,
		&enabled, &created, &b.CreatedBy, &updated, &b.UpdatedBy,
		&retired, &b.RetiredBy); err != nil {
		return Butler{}, err
	}
	b.Surface = ButlerSurface(surface)
	b.Enabled, b.RouteEnabled = enabled != 0, routeEnabled != 0
	for _, f := range []struct {
		text string
		into *time.Time
	}{{created, &b.CreatedAt}, {updated, &b.UpdatedAt}, {retired, &b.RetiredAt}} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return Butler{}, fmt.Errorf("runtimedb: bad butler timestamp %q: %w", f.text, err)
		}
		*f.into = parsed
	}
	return b, nil
}

// CreateButler records a new Butler.
//
// The caller is the Console holder — creation is Plan-mediated above
// this layer, and a Butler never reaches here (A8).
func (db *DB) CreateButler(b Butler, by string) (int64, error) {
	name := strings.TrimSpace(b.Name)
	switch {
	case name == AllButlers:
		// AllButlers is the Console holder's scope sentinel. A Butler
		// literally named "*" would match it in every scope filter and
		// read every other Butler's material — A4 defeated by a name.
		return 0, fmt.Errorf("runtimedb: %q is reserved: it is the scope that means every butler, so a butler cannot be called it (ADR-W031 A4)", AllButlers)
	case name == "":
		return 0, fmt.Errorf("runtimedb: a butler needs a name — it is what a person says and what a routine is assigned to (ADR-W031 A11)")
	case by == "":
		return 0, fmt.Errorf("runtimedb: creating a butler needs who created it")
	case !ValidButlerSurface(b.Surface):
		return 0, fmt.Errorf("runtimedb: a butler's surface must be admin|family, got %q", b.Surface)
	case b.Route == "":
		return 0, fmt.Errorf("runtimedb: a butler needs an assigned model route — which model answers is not a field to fill in later (ADR-W031 A2)")
	}
	switch b.Profile {
	case ProfileHousehold, ProfileOperator, ProfileChild:
	default:
		return 0, fmt.Errorf("runtimedb: a butler's profile must be household|operator|child, got %q", b.Profile)
	}
	// The surface and the profile have to agree, or the ceiling stated
	// on the record is not the ceiling the gateway enforces.
	if (b.Profile == ProfileOperator) != (b.Surface == ButlerSurfaceAdmin) {
		return 0, fmt.Errorf(
			"runtimedb: profile %q does not belong on the %s surface — the profile is the ceiling and the surface is what enforces it (ADR-W031 A3)",
			b.Profile, b.Surface)
	}

	var routeSeq int64
	err := db.sql.QueryRow(`SELECT seq FROM model_routes WHERE name = ?`, b.Route).Scan(&routeSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: no model route named %q", ErrNotFound, b.Route)
	}
	if err != nil {
		return 0, fmt.Errorf("runtimedb: creating a butler: %w", err)
	}

	stamp := now()
	res, err := db.sql.Exec(
		`INSERT INTO butlers (name, directive, surface, profile, route, enabled,
		     created_at, created_by, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		name, b.Directive, string(b.Surface), b.Profile, routeSeq, stamp, by, stamp, by)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: creating a butler: %w", err)
	}
	return res.LastInsertId()
}

// GetButler reads one Butler by name, with its route resolved.
func (db *DB) GetButler(name string) (Butler, error) {
	b, err := scanButler(db.sql.QueryRow(butlerColumns+` WHERE b.name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Butler{}, ErrNotFound
	}
	if err != nil {
		return Butler{}, fmt.Errorf("runtimedb: reading butler %q: %w", name, err)
	}
	return b, nil
}

// AnyButlers reports whether this deployment has created a Butler.
//
// It exists separately from ListButlers because it is asked on the
// per-message path — every inbound chat message needs to know whether
// this deployment is on the named-Butler path or the legacy one — and
// loading every Butler and its joined route to answer "is there at
// least one" is work done once per message for a boolean.
func (db *DB) AnyButlers() (bool, error) {
	var one int
	err := db.sql.QueryRow(`SELECT 1 FROM butlers LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("runtimedb: checking for butlers: %w", err)
	}
	return true, nil
}

// ListButlers returns every Butler, retired ones included, ordered by
// surface then name.
//
// Retired Butlers are included because this is the Console holder's
// view and "what used to run here" is part of it. Callers that want
// only working ones filter on Live.
func (db *DB) ListButlers() ([]Butler, error) {
	rows, err := db.sql.Query(butlerColumns + ` ORDER BY b.surface, b.name`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butlers: %w", err)
	}
	defer rows.Close()
	var out []Butler
	for rows.Next() {
		b, err := scanButler(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetButlerRoute reassigns a Butler's model.
//
// It applies IMMEDIATELY and deliberately: the person at the Console
// owns this Butler, so a proposal they would then approve themselves is
// ceremony with no refusing party (A8). What the change produces
// instead is the record this function returns — the before and after —
// so the swap is answerable afterwards, which is the property the
// approval step was standing in for.
//
// It returns the kinds either side, because a local-to-cloud move is
// the one the caller has to warn about: the conversation so far goes to
// the new model with the next message, and the person making that
// choice is owed the consequence in the moment they make it.
func (db *DB) SetButlerRoute(name, route, by string) (from, to Butler, err error) {
	if by == "" {
		return Butler{}, Butler{}, fmt.Errorf("runtimedb: changing a butler's model needs who changed it")
	}
	from, err = db.GetButler(name)
	if err != nil {
		return Butler{}, Butler{}, err
	}
	var routeSeq int64
	err = db.sql.QueryRow(`SELECT seq FROM model_routes WHERE name = ?`, route).Scan(&routeSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return Butler{}, Butler{}, fmt.Errorf("%w: no model route named %q", ErrNotFound, route)
	}
	if err != nil {
		return Butler{}, Butler{}, fmt.Errorf("runtimedb: changing a butler's model: %w", err)
	}
	if _, err := db.sql.Exec(
		`UPDATE butlers SET route = ?, updated_at = ?, updated_by = ? WHERE seq = ?`,
		routeSeq, now(), by, from.Seq); err != nil {
		return Butler{}, Butler{}, fmt.Errorf("runtimedb: changing a butler's model: %w", err)
	}
	to, err = db.GetButler(name)
	return from, to, err
}

// CrossesToCloud reports whether a route change moves this Butler from
// a local model to a cloud one.
//
// Only that direction is worth warning about. Cloud to local narrows
// where material can go, and warning about it too would teach people to
// click through the warning that matters.
func CrossesToCloud(from, to Butler) bool {
	return from.RouteKind == RouteLocal && to.RouteKind == RouteCloud
}

// RetireButler stops a Butler without deleting what it did.
//
// Its conversations, memory and traces stay: they are evidence of what
// happened, and a household that retires a Butler has not asked to
// forget what it said. Removal of the material itself is offboarding's
// job, and it is a different act with different consequences.
func (db *DB) RetireButler(name, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: retiring a butler needs who retired it")
	}
	res, err := db.sql.Exec(
		`UPDATE butlers SET retired_at = ?, retired_by = ?, enabled = 0, updated_at = ?, updated_by = ?
		 WHERE name = ? AND retired_at = ''`, now(), by, now(), by, name)
	if err != nil {
		return fmt.Errorf("runtimedb: retiring a butler: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GrantButlerAccess lets one person talk to one Butler.
//
// A repeated grant is not an error: the state the caller asked for
// already holds, and failing would make a Console that re-renders its
// list look broken.
func (db *DB) GrantButlerAccess(butler, member, by string) error {
	if member == "" || by == "" {
		return fmt.Errorf("runtimedb: granting butler access needs the person and who granted it")
	}
	b, err := db.GetButler(butler)
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(
		`INSERT INTO butler_access (butler, member, granted_at, granted_by)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT DO NOTHING`, b.Seq, member, now(), by)
	if err != nil {
		return fmt.Errorf("runtimedb: granting butler access: %w", err)
	}
	return nil
}

// RevokeButlerAccess is an immediate denial, like every other
// revocation here (ADR-W030 A7.1). The next message from that person
// does not reach this Butler.
func (db *DB) RevokeButlerAccess(butler, member, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: revoking butler access needs who revoked it")
	}
	b, err := db.GetButler(butler)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(
		`UPDATE butler_access SET revoked_at = ?, revoked_by = ?
		 WHERE butler = ? AND member = ? AND revoked_at = ''`, now(), by, b.Seq, member)
	if err != nil {
		return fmt.Errorf("runtimedb: revoking butler access: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListButlerAccess returns the people who may currently talk to this
// Butler.
func (db *DB) ListButlerAccess(butler string) ([]string, error) {
	b, err := db.GetButler(butler)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(
		`SELECT member FROM butler_access WHERE butler = ? AND revoked_at = '' ORDER BY member`, b.Seq)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butler access: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ButlersFor returns the Butlers this person may talk to, live ones
// only.
//
// This is what a chat surface asks. A person sees the Butlers they were
// granted and does not learn that the others exist, which is what makes
// A10's access list a boundary rather than a filter over a roster
// everyone can see.
func (db *DB) ButlersFor(member string) ([]Butler, error) {
	if member == "" {
		return nil, fmt.Errorf("runtimedb: listing butlers needs the person asking")
	}
	rows, err := db.sql.Query(butlerColumns+`
		 JOIN butler_access a ON a.butler = b.seq
		 WHERE a.member = ? AND a.revoked_at = '' AND b.enabled = 1 AND b.retired_at = ''
		 ORDER BY b.name`, member)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing butlers for %q: %w", member, err)
	}
	defer rows.Close()
	var out []Butler
	for rows.Next() {
		b, err := scanButler(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ResolveButlerFor is the check every message goes through: may this
// person talk to this Butler, and can this Butler answer?
//
// The order is deliberate. Access is settled before reachability,
// because telling somebody with no access that a Butler's model is
// down has already told them the Butler exists and what state it is in.
func (db *DB) ResolveButlerFor(butler, member string) (Butler, error) {
	b, err := db.GetButler(butler)
	if err != nil {
		return Butler{}, err
	}
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_access
		 WHERE butler = ? AND member = ? AND revoked_at = ''`, b.Seq, member).Scan(&n); err != nil {
		return Butler{}, fmt.Errorf("runtimedb: resolving butler access: %w", err)
	}
	if n == 0 {
		return Butler{}, ErrNoButlerAccess
	}
	// Admission is rechecked at use, not trusted from the grant: a
	// person offboarded after being granted access is denied now
	// (ADR-W030 A2.1).
	m, err := db.GetButlerMember(member)
	if err != nil || !m.Active() {
		return Butler{}, ErrNotAdmitted
	}
	if !b.Live() {
		return Butler{}, fmt.Errorf("%w: %s has been retired or disabled", ErrButlerUnreachable, b.Name)
	}
	if !b.Reachable() {
		// A6: the work waits and says why. It does not move to another
		// Butler's model, because that substitution is the disclosure
		// this whole design exists to prevent.
		why := "unavailable"
		if !b.RouteEnabled {
			why = "turned off"
		}
		return Butler{}, fmt.Errorf("%w: %s runs on %s, which is %s — this waits rather than moving to another model (ADR-W031 A6)",
			ErrButlerUnreachable, b.Name, b.Route, why)
	}
	return b, nil
}
