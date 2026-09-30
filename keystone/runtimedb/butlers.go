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

// Agent classes (ADR-W033 design §5.2). The ceiling an AGENT runs
// under — the tool surface it is allowed to reach.
//
// Two values and no more. 'child' is not among them and never was an
// Agent class: it reached butlers.profile only because that column
// borrowed butler_members' vocabulary wholesale. See ProfileChild in
// butleridentity.go for the axis it actually belongs to.
//
// THE CLASS NAMES THE CEILING; THE SURFACE ENFORCES IT. An Owner Agent
// runs on the admin surface and a Member Agent on the family one, and
// 0058's paired CHECK refuses any row where those disagree. The two
// columns are one fact written twice, deliberately: the gateway is what
// actually refuses a tool, and the class is the word §5.2 uses for it.
const (
	AgentClassOwner  = "owner"
	AgentClassMember = "member"
)

// ValidAgentClass reports whether s is one of the two classes.
func ValidAgentClass(s string) bool {
	return s == AgentClassOwner || s == AgentClassMember
}

// AgentClassAdmits reports whether an Agent of class `agent` may hold
// something that requires class `required` (design §6.1).
//
// Member tools may be assigned to Member OR Owner Agents; Owner tools
// only to Owner Agents. So this is an ordering, not an equality — an
// equality would refuse an Owner Agent the member tools §6.4 says it
// gets by default, which is most of what any Agent does.
func AgentClassAdmits(agent, required string) bool {
	if required == AgentClassMember {
		return agent == AgentClassMember || agent == AgentClassOwner
	}
	return agent == AgentClassOwner && required == AgentClassOwner
}

// SurfaceForAgentClass is the surface a class must run on.
//
// One function rather than a comparison spelled out at each call site:
// the pairing is an invariant, and an invariant written six times is one
// that will eventually be written five times.
func SurfaceForAgentClass(class string) ButlerSurface {
	if class == AgentClassOwner {
		return ButlerSurfaceAdmin
	}
	return ButlerSurfaceFamily
}

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
	// AgentClass is 'owner' or 'member' (design §5.2). It always agrees
	// with Surface; see the constants above for why both exist.
	AgentClass string `json:"agentClass"`
	// Model is the assigned model, by name. ModelKind is "local" or
	// "cloud" — the data boundary, and what a local-to-cloud warning
	// keys off. ProviderModel is what the provider itself calls it,
	// which is a different string and often a more specific one:
	// assigned "kitchen-box", answered by "qwen3.5:4b".
	Model         string `json:"model"`
	ModelSeq      int64  `json:"modelSeq"`
	ModelKind     string `json:"modelKind"`
	ProviderModel string `json:"providerModel,omitempty"`
	Provider      string `json:"provider,omitempty"`
	// Endpoint is the route's base URL, carried because a Butler that
	// cannot be TALKED to is not a Butler. Without it the assignment
	// names a provider and a model and still needs a second lookup to
	// reach either, and the caller that forgets the lookup falls back to
	// whatever the process was started with — which is the substitution
	// A6 exists to forbid, arrived at by omission.
	Endpoint  string `json:"endpoint,omitempty"`
	Readiness string `json:"readiness"`
	// ModelEnabled is the MODEL's switch, not the Butler's. Disabling a
	// model has to stop the Butlers already assigned to it: the 0032
	// triggers only fire when a butler's model is written, so without
	// this a model turned off yesterday still answers today.
	ModelEnabled bool      `json:"modelEnabled"`
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
	return b.Live() && b.ModelEnabled && b.Readiness != ModelUnavailable
}

// CarriesActionAuthority reports whether a session with this Butler may
// use tools that do things. Only a route that passed its probe.
func (b Butler) CarriesActionAuthority() bool {
	return b.Live() && b.Readiness == ModelReady
}

// butlerColumns is the join every read uses. A Butler is only ever
// meaningful with its model resolved — a Butler whose model you have to
// look up separately is one a caller will eventually forget to look up.
const butlerColumns = `
	SELECT b.seq, b.name, b.directive, b.surface, b.agent_class,
	       m.name, m.seq, m.kind, m.model, m.provider, m.endpoint, m.readiness, m.enabled,
	       b.enabled, b.created_at, b.created_by, b.updated_at, b.updated_by,
	       b.retired_at, b.retired_by
	  FROM butlers b JOIN models m ON m.seq = b.model`

func scanButler(s interface{ Scan(...any) error }) (Butler, error) {
	var b Butler
	var surface string
	var enabled, modelEnabled int
	var created, updated, retired string
	if err := s.Scan(&b.Seq, &b.Name, &b.Directive, &surface, &b.AgentClass,
		&b.Model, &b.ModelSeq, &b.ModelKind, &b.ProviderModel, &b.Provider, &b.Endpoint, &b.Readiness, &modelEnabled,
		&enabled, &created, &b.CreatedBy, &updated, &b.UpdatedBy,
		&retired, &b.RetiredBy); err != nil {
		return Butler{}, err
	}
	b.Surface = ButlerSurface(surface)
	b.Enabled, b.ModelEnabled = enabled != 0, modelEnabled != 0
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
	case b.Model == "":
		return 0, fmt.Errorf("runtimedb: a butler needs an assigned model route — which model answers is not a field to fill in later (ADR-W031 A2)")
	}
	if !ValidAgentClass(b.AgentClass) {
		return 0, fmt.Errorf("runtimedb: an agent class must be %s|%s, got %q (design §5.2)",
			AgentClassOwner, AgentClassMember, b.AgentClass)
	}
	// The surface and the class have to agree, or the ceiling stated on
	// the record is not the ceiling the gateway enforces.
	if SurfaceForAgentClass(b.AgentClass) != b.Surface {
		return 0, fmt.Errorf(
			"runtimedb: a %s agent does not belong on the %s surface — the class names the ceiling and the surface enforces it (ADR-W033 design §5.2)",
			b.AgentClass, b.Surface)
	}

	var modelSeq int64
	err := db.sql.QueryRow(`SELECT seq FROM models WHERE name = ?`, b.Model).Scan(&modelSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: no model route named %q", ErrNotFound, b.Model)
	}
	if err != nil {
		return 0, fmt.Errorf("runtimedb: creating a butler: %w", err)
	}

	stamp := now()
	res, err := db.sql.Exec(
		`INSERT INTO butlers (name, directive, surface, agent_class, model, enabled,
		     created_at, created_by, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		name, b.Directive, string(b.Surface), b.AgentClass, modelSeq, stamp, by, stamp, by)
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

// GetButlerBySeq reads one Butler by its row id.
//
// The seq is what a grant names, because a name can be reused after a
// Butler is retired and a grant must not follow the name onto whatever
// is created next.
func (db *DB) GetButlerBySeq(seq int64) (Butler, error) {
	b, err := scanButler(db.sql.QueryRow(butlerColumns+` WHERE b.seq = ?`, seq))
	if errors.Is(err, sql.ErrNoRows) {
		return Butler{}, ErrNotFound
	}
	if err != nil {
		return Butler{}, fmt.Errorf("runtimedb: reading butler %d: %w", seq, err)
	}
	return b, nil
}

// nullableSeq turns a zero row id into a SQL NULL.
//
// Zero is not a valid butlers.seq, and storing it would make a grant
// reference a Butler that cannot exist. NULL says what is actually true:
// this grant names no Agent.
func nullableSeq(seq int64) any {
	if seq == 0 {
		return nil
	}
	return seq
}

// SetAgentClass moves an Agent between classes, taking its surface with
// it, and strips what the move must not leave behind (design §18.3).
//
// THE ASYMMETRY IS THE POINT, and it is worth stating at the one place
// that implements it:
//
//   - Downgrading owner -> member REVOKES every one of this Agent's live
//     grants, not only the owner-class ones. §6.4 is explicit — "strips
//     all existing tool grants, after which appropriate Member grants
//     can be reapplied" — and the wider rule is the safer one to have
//     written down: a downgrade is a decision that this Agent should be
//     trusted with less, so it starts from nothing and an Owner puts
//     back what they meant, rather than keeping whatever happened to be
//     member-class already.
//   - Upgrading member -> owner PRESERVES the member grants it already
//     had and adds no owner tools (§6.4). A wider gateway with nothing
//     new on it is a wider gateway nobody can use yet, which is exactly
//     what makes the upgrade safe to allow at all.
//
// Revoked rather than deleted: a grant that was in force is evidence of
// what this Agent could do yesterday, and revoking keeps that while
// making the row permanently inert — revoked_at is never cleared, so no
// later upgrade can revive it.
//
// Only THIS Agent's grants. A class-level row (the pre-0058 shape, with
// no Agent named) belongs to no Agent and is left alone; another Agent's
// rows are not this one's to touch. That is the whole reason 0058 added
// the column.
func (db *DB) SetAgentClass(name, class, by string) (revoked int, err error) {
	if !ValidAgentClass(class) {
		return 0, fmt.Errorf("runtimedb: an agent class must be %s|%s, got %q (design §5.2)",
			AgentClassOwner, AgentClassMember, class)
	}
	if by == "" {
		return 0, fmt.Errorf("runtimedb: changing an agent's class needs who changed it")
	}
	b, err := db.GetButler(name)
	if err != nil {
		return 0, err
	}
	if b.AgentClass == class {
		// Not an error: asking for the class it already has is a
		// no-op, and reporting it as a failure would make a retry of a
		// half-finished change look like a new problem.
		return 0, nil
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: changing agent class: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Both columns in one statement. The 0058 trigger refuses them
	// moving apart, so this is not merely tidy: two statements would be
	// two aborts.
	if _, err := tx.Exec(
		`UPDATE butlers SET agent_class = ?, surface = ?, updated_at = ?, updated_by = ?
		 WHERE seq = ?`,
		class, string(SurfaceForAgentClass(class)), now(), by, b.Seq); err != nil {
		return 0, fmt.Errorf("runtimedb: changing agent class: %w", err)
	}

	if b.AgentClass == AgentClassOwner && class == AgentClassMember {
		res, err := tx.Exec(
			`UPDATE butler_tool_grants SET revoked_at = ?, revoked_by = ?
			 WHERE butler = ? AND revoked_at = ''`,
			now(), by, b.Seq)
		if err != nil {
			return 0, fmt.Errorf("runtimedb: stripping owner grants: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		revoked = int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: changing agent class: %w", err)
	}
	return revoked, nil
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
	var modelSeq int64
	err = db.sql.QueryRow(`SELECT seq FROM models WHERE name = ?`, route).Scan(&modelSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return Butler{}, Butler{}, fmt.Errorf("%w: no model route named %q", ErrNotFound, route)
	}
	if err != nil {
		return Butler{}, Butler{}, fmt.Errorf("runtimedb: changing a butler's model: %w", err)
	}
	if _, err := db.sql.Exec(
		`UPDATE butlers SET model = ?, updated_at = ?, updated_by = ? WHERE seq = ?`,
		modelSeq, now(), by, from.Seq); err != nil {
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
	return from.ModelKind == ModelLocal && to.ModelKind == ModelCloud
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

// SetButlerDirective rewrites what a Butler is for, in its owner's
// words.
//
// A directive instructs and authorises nothing (ADR-W031 A9), which is
// why this is a plain edit rather than a Plan: rewriting it cannot widen
// a tool, reach a connection, or change a memory scope, so there is
// nothing here for an approval to protect. What it CAN do is change how
// a Butler talks and what it believes it is for, and that is worth
// recording — the caller is named, and the change stamps updated_by.
//
// A retired Butler is refused. Editing the purpose of something that no
// longer answers reads as reviving it, and it does not.
func (db *DB) SetButlerDirective(name, directive, by string) (Butler, error) {
	if by == "" {
		return Butler{}, fmt.Errorf("runtimedb: changing a butler's directive needs who changed it")
	}
	res, err := db.sql.Exec(
		`UPDATE butlers SET directive = ?, updated_at = ?, updated_by = ?
		 WHERE name = ? AND retired_at = ''`,
		strings.TrimSpace(directive), now(), by, name)
	if err != nil {
		return Butler{}, fmt.Errorf("runtimedb: setting a butler's directive: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Butler{}, ErrNotFound
	}
	return db.GetButler(name)
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
	person, err := resolvePersonID(db.sql, member)
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(
		`INSERT INTO butler_access (butler, person_id, granted_at, granted_by)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT DO NOTHING`, b.Seq, person, now(), by)
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
	person, err := resolvePersonID(db.sql, member)
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(
		`UPDATE butler_access SET revoked_at = ?, revoked_by = ?
		 WHERE butler = ? AND person_id = ? AND revoked_at = ''`, now(), by, b.Seq, person)
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
		`SELECT m.member FROM butler_access a JOIN butler_members m ON m.person_id = a.person_id
		 WHERE a.butler = ? AND a.revoked_at = '' ORDER BY m.member`, b.Seq)
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
		 JOIN butler_members p ON p.person_id = a.person_id
		 WHERE p.member = ? AND a.revoked_at = '' AND b.enabled = 1 AND b.retired_at = ''
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
		`SELECT COUNT(*) FROM butler_access a JOIN butler_members p ON p.person_id = a.person_id
		 WHERE a.butler = ? AND p.member = ? AND a.revoked_at = ''`, b.Seq, member).Scan(&n); err != nil {
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
		if !b.ModelEnabled {
			why = "turned off"
		}
		return Butler{}, fmt.Errorf("%w: %s runs on %s, which is %s — this waits rather than moving to another model (ADR-W031 A6)",
			ErrButlerUnreachable, b.Name, b.Model, why)
	}
	return b, nil
}

// RestoreButler brings a retired Butler back.
//
// It is a separate verb from creation, and the difference is the whole
// point of retiring rather than deleting: the same row comes back, with
// the same seq, so its scope, its narrowings, its memory and its
// conversations are still its own. A household that retired a Butler by
// mistake gets the Butler back, not a namesake.
//
// The route is checked again on the way in. A Butler retired six weeks
// ago may have been assigned to a model somebody has since turned off,
// and bringing it back onto nowhere would produce exactly the silent
// non-answering A6 says must be a stated wait instead.
func (db *DB) RestoreButler(name, by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: restoring a butler needs who restored it")
	}
	b, err := db.GetButler(name)
	if err != nil {
		return err
	}
	if b.RetiredAt.IsZero() {
		return fmt.Errorf("runtimedb: %s is not retired", name)
	}
	if !b.ModelEnabled {
		return fmt.Errorf(
			"runtimedb: %s is assigned to %q, which is turned off — turn the route back on, or assign another, before bringing the butler back (ADR-W031 A6)",
			name, b.Model)
	}
	res, err := db.sql.Exec(
		`UPDATE butlers SET retired_at = '', retired_by = '', enabled = 1, updated_at = ?, updated_by = ?
		 WHERE name = ? AND retired_at <> ''`, now(), by, name)
	if err != nil {
		return fmt.Errorf("runtimedb: restoring a butler: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DuplicateButler copies a Butler's SHAPE and none of its history.
//
// What is copied is what somebody making a second Butler like this one
// would otherwise retype: the directive, the surface and profile, the
// route, and the tools it has been narrowed out of (narrowings only
// ever narrow, so they cannot widen the copy). What is deliberately NOT
// copied is everything that names a person — who may talk to it, what
// it remembers, what it has been asked — and everything that carries
// authority: connection grants, Agent secret grants, bot addresses and
// runtime credentials. A copy that inherited an access list would
// silently grant a new Butler to people who agreed to the old one, a
// copy that inherited memory would move somebody's notes into a scope
// they never chose (ADR-W031 A4/A5), and a copy that inherited
// credentials or grants could act as the original. Those must be
// granted fresh (design §7 "no automatic inheritance of another
// agent's credentials", §17.2).
//
// The steps run in sequence, not in one transaction.
func (db *DB) DuplicateButler(name, newName, by string) (int64, error) {
	src, err := db.GetButler(name)
	if err != nil {
		return 0, err
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return 0, fmt.Errorf("runtimedb: a copy needs a name of its own")
	}
	seq, err := db.CreateButler(Butler{
		Name: newName, Directive: src.Directive, Surface: src.Surface,
		AgentClass: src.AgentClass, Model: src.Model,
	}, by)
	if err != nil {
		return 0, err
	}
	narrowed, err := db.ButlerToolNarrowings(name)
	if err != nil {
		return 0, err
	}
	for tool, reason := range narrowed {
		if err := db.NarrowButlerTool(newName, tool, reason, by); err != nil {
			return 0, fmt.Errorf("runtimedb: copying %s's tool narrowings: %w", name, err)
		}
	}
	return seq, nil
}

// ButlerImpact is what stops working if a Butler is retired.
//
// Every number here is counted rather than estimated, because the
// screen that shows it is asking somebody to make a decision they
// cannot see the consequences of otherwise. "2 routines call it" is
// only worth printing if it is two.
type ButlerImpact struct {
	Butler string `json:"butler"`
	// People who message it and would lose it.
	People []string `json:"people"`
	// Routines assigned to it. Each names its own state, because a
	// disabled routine breaking is not news.
	Routines []ButlerRoutineRef `json:"routines"`
	// Sessions and MemoryCount are kept, not deleted — they are said
	// so that retiring reads as the reversible thing it is.
	Sessions    int `json:"sessions"`
	MemoryCount int `json:"memoryCount"`
	// Successors are the other live Butlers on the same surface, which
	// is what a routine could be pointed at instead.
	Successors []string `json:"successors"`
}

// ButlerRoutineRef is one routine that calls a Butler.
type ButlerRoutineRef struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// ButlerImpactOf counts what retiring this Butler would break.
func (db *DB) ButlerImpactOf(name string) (ButlerImpact, error) {
	b, err := db.GetButler(name)
	if err != nil {
		return ButlerImpact{}, err
	}
	out := ButlerImpact{Butler: b.Name, People: []string{}, Routines: []ButlerRoutineRef{}, Successors: []string{}}
	if out.People, err = db.ListButlerAccess(name); err != nil {
		return ButlerImpact{}, err
	}
	rows, err := db.sql.Query(
		`SELECT name, state FROM household_routines WHERE butler = ? ORDER BY name`, name)
	if err != nil {
		return ButlerImpact{}, fmt.Errorf("runtimedb: counting a butler's routines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r ButlerRoutineRef
		if err := rows.Scan(&r.Name, &r.State); err != nil {
			return ButlerImpact{}, err
		}
		out.Routines = append(out.Routines, r)
	}
	if err := rows.Err(); err != nil {
		return ButlerImpact{}, err
	}
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_sessions WHERE butler = ?`, name).Scan(&out.Sessions); err != nil {
		return ButlerImpact{}, fmt.Errorf("runtimedb: counting a butler's sessions: %w", err)
	}
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_memory WHERE butler = ?`, name).Scan(&out.MemoryCount); err != nil {
		return ButlerImpact{}, fmt.Errorf("runtimedb: counting a butler's memory: %w", err)
	}
	all, err := db.ListButlers()
	if err != nil {
		return ButlerImpact{}, err
	}
	for _, other := range all {
		if other.Name != b.Name && other.Surface == b.Surface && other.RetiredAt.IsZero() {
			out.Successors = append(out.Successors, other.Name)
		}
	}
	return out, nil
}
