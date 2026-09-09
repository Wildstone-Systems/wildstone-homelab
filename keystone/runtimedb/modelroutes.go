package runtimedb

// Named local and cloud model routes (ADR-W030 A9/A9.1, P14).
//
// Selection answers three questions in a fixed order, and the order is
// the guarantee:
//
//   Where may this material go? Local-only material reaches a local
//   route and no other. Checked first, so no fallback, escalation or
//   busy local model can route around it.
//
//   What may the answering model be trusted with? A route that has not
//   passed its probe, or passed only as QA-only, may answer questions
//   and may not carry action authority. The selection returns that
//   verdict and the caller RECOMPUTES the manifest from it — a fallback
//   to a weaker route produces a genuinely narrower session, not a full
//   one answered by a weaker model.
//
//   Is there capacity? Concurrency is per route, because "the local
//   model is busy" is a real condition and waiting behind it is usually
//   better than escalating past it.
//
// And under all of it: deterministic work does not need a model at all.
// A house whose every route is down still runs its routines; it just
// cannot phrase them nicely.

import (
	"errors"
	"fmt"
	"time"
)

// Route kinds. The boundary, and there is no third answer.
const (
	RouteLocal = "local"
	RouteCloud = "cloud"
)

// Route readiness, per route rather than per deployment.
const (
	RouteUnprobed    = "unprobed"
	RouteReady       = "ready"
	RouteQAOnly      = "qa-only"
	RouteUnavailable = "unavailable"
)

// Job classes. Background processing runs local-first at lower priority
// than anything a person is waiting on (A9.1).
const (
	JobInteractive = "interactive"
	JobBackground  = "background"
)

// Data locality: what the material in this request is allowed to do.
const (
	// DataLocalOnly must not leave the house. It reaches local routes
	// and nothing else, whatever else is true.
	DataLocalOnly = "local-only"
	// DataCloudAllowed may go to a cloud route, because its owner
	// explicitly opted in (A5, A9).
	DataCloudAllowed = "cloud-allowed"
)

// DefaultMaxEscalations bounds automatic fallback. Two is enough to get
// past a busy local model and a route that has just gone down; more
// than that and the house is trying every model it has to answer one
// question, which is a decision a person should make.
const DefaultMaxEscalations = 2

// DefaultLeaseTTL bounds an in-flight call's capacity lease, so a
// crashed caller frees its slot rather than holding it forever.
const DefaultLeaseTTL = 10 * time.Minute

// ErrNoRoute is returned when nothing may answer. Deterministic work
// continues regardless; only model-dependent work waits.
var ErrNoRoute = errors.New("runtimedb: no model route is available for this request")

// ModelRoute is one named place a question may be sent.
type ModelRoute struct {
	Seq             int64     `json:"seq"`
	Name            string    `json:"name"`
	Kind            string    `json:"kind"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model,omitempty"`
	Endpoint        string    `json:"endpoint,omitempty"`
	Readiness       string    `json:"readiness"`
	ReadinessDetail string    `json:"readinessDetail,omitempty"`
	ProbedAt        time.Time `json:"probedAt,omitzero"`
	MaxConcurrency  int       `json:"maxConcurrency"`
	Priority        int       `json:"priority"`
	Enabled         bool      `json:"enabled"`
}

// CarriesActionAuthority reports whether a session answered by this
// route may use tools that do things.
//
// Only a route that has actually passed its probe. An unprobed one has
// not been checked and a QA-only one has been checked and found
// wanting, and neither should be trusted to decide whether to unlock a
// door (ADR-W017 A4, ADR-W030 A9).
func (r ModelRoute) CarriesActionAuthority() bool {
	return r.Enabled && r.Readiness == RouteReady
}

// UpsertModelRoute records a route.
func (db *DB) UpsertModelRoute(r ModelRoute, by string) error {
	switch r.Kind {
	case RouteLocal, RouteCloud:
	default:
		return fmt.Errorf("runtimedb: a model route is local or cloud, got %q", r.Kind)
	}
	if r.Name == "" || r.Provider == "" {
		return fmt.Errorf("runtimedb: a model route needs a name and a provider")
	}
	if r.Readiness == "" {
		// A new route has not been probed. Not ready — unprobed, which
		// is a different and more honest thing, and which withholds
		// action authority until something has actually checked.
		r.Readiness = RouteUnprobed
	}
	switch r.Readiness {
	case RouteUnprobed, RouteReady, RouteQAOnly, RouteUnavailable:
	default:
		return fmt.Errorf("runtimedb: route readiness must be unprobed|ready|qa-only|unavailable, got %q", r.Readiness)
	}
	if r.MaxConcurrency <= 0 {
		r.MaxConcurrency = 1
	}
	if r.MaxConcurrency > 64 {
		return fmt.Errorf("runtimedb: route concurrency must be between 1 and 64, got %d", r.MaxConcurrency)
	}
	if r.Priority <= 0 {
		// Local-first: a local route sorts ahead of a cloud one unless
		// somebody says otherwise (A9).
		r.Priority = 100
		if r.Kind == RouteCloud {
			r.Priority = 200
		}
	}
	enabled := 1
	if !r.Enabled && r.Seq == 0 && r.Readiness == RouteUnprobed {
		// A brand-new route defaults to enabled: the zero value of a
		// bool would otherwise make every created route disabled and
		// the failure would look like "routing is broken".
		enabled = 1
	} else if !r.Enabled {
		enabled = 0
	}
	_, err := db.sql.Exec(
		`INSERT INTO model_routes (name, kind, provider, model, endpoint, readiness,
		     readiness_detail, max_concurrency, priority, enabled, created_at, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   provider = excluded.provider, model = excluded.model, endpoint = excluded.endpoint,
		   max_concurrency = excluded.max_concurrency, priority = excluded.priority,
		   enabled = excluded.enabled, updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by`,
		r.Name, r.Kind, r.Provider, r.Model, r.Endpoint, r.Readiness,
		r.MaxConcurrency, r.Priority, enabled, now(), now(), by)
	if err != nil {
		return fmt.Errorf("runtimedb: recording model route: %w", err)
	}
	return nil
}

// RecordRouteReadiness stores a probe verdict for one route.
//
// It is separate from UpsertModelRoute for the reason ADR-W017 A9 gives
// about the provider config: a verdict describes the route as it was
// probed, and carrying it across a change would let a model inherit a
// pass it never earned.
func (db *DB) RecordRouteReadiness(name, readiness, detail string) error {
	switch readiness {
	case RouteUnprobed, RouteReady, RouteQAOnly, RouteUnavailable:
	default:
		return fmt.Errorf("runtimedb: route readiness must be unprobed|ready|qa-only|unavailable, got %q", readiness)
	}
	if readiness != RouteReady && detail == "" {
		return fmt.Errorf("runtimedb: a route reported %s must say why", readiness)
	}
	res, err := db.sql.Exec(
		`UPDATE model_routes SET readiness = ?, readiness_detail = ?, probed_at = ?, updated_at = ?
		 WHERE name = ?`, readiness, detail, now(), now(), name)
	if err != nil {
		return fmt.Errorf("runtimedb: recording route readiness: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetModelRouteEnabled turns a route off without deleting it.
func (db *DB) SetModelRouteEnabled(name string, enabled bool, by string) error {
	res, err := db.sql.Exec(
		`UPDATE model_routes SET enabled = ?, updated_at = ?, updated_by = ? WHERE name = ?`,
		boolInt(enabled), now(), by, name)
	if err != nil {
		return fmt.Errorf("runtimedb: setting route enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListModelRoutes returns routes in selection order.
func (db *DB) ListModelRoutes() ([]ModelRoute, error) {
	rows, err := db.sql.Query(
		`SELECT seq, name, kind, provider, model, endpoint, readiness, readiness_detail,
		        probed_at, max_concurrency, priority, enabled
		 FROM model_routes ORDER BY priority, seq`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing model routes: %w", err)
	}
	defer rows.Close()
	var out []ModelRoute
	for rows.Next() {
		var r ModelRoute
		var probed string
		var enabled int
		if err := rows.Scan(&r.Seq, &r.Name, &r.Kind, &r.Provider, &r.Model, &r.Endpoint,
			&r.Readiness, &r.ReadinessDetail, &probed, &r.MaxConcurrency, &r.Priority, &enabled); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		if probed != "" {
			if r.ProbedAt, err = time.Parse(time.RFC3339Nano, probed); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RouteRequest describes what is being asked and of what material.
type RouteRequest struct {
	// Locality is the data policy for this request's material. It is
	// checked first and cannot be overridden by anything below.
	Locality string
	// Job separates interactive work from background processing.
	Job string
	// Holder identifies the caller for the capacity lease.
	Holder string
	// Attempt is which escalation this is: 0 is the first choice.
	Attempt int
	// MaxEscalations bounds automatic fallback. Zero uses the default.
	MaxEscalations int
}

// RouteSelection is what was chosen, and what the caller must now
// recompute from.
type RouteSelection struct {
	Route ModelRoute `json:"route"`
	// LeaseSeq must be released when the call finishes.
	LeaseSeq int64 `json:"leaseSeq"`
	// Verdict is the model verdict this route carries: "full" when it
	// passed its probe, "qa-only" otherwise. The caller RECOMPUTES the
	// tool manifest from this rather than reusing one built for an
	// earlier selection — which is what stops a fallback widening tools.
	Verdict string `json:"verdict"`
	// Escalated is true when this was not the first choice.
	Escalated bool `json:"escalated"`
	// Reason explains a non-obvious selection, for the trace and for a
	// person reading it.
	Reason string `json:"reason,omitempty"`
}

// SelectModelRoute chooses a route for one request, taking a capacity
// lease on it.
func (db *DB) SelectModelRoute(req RouteRequest) (RouteSelection, error) {
	switch req.Locality {
	case DataLocalOnly, DataCloudAllowed:
	default:
		// An unstated locality is treated as the strict one. Defaulting
		// the other way would make every unlabelled request eligible for
		// the cloud, which is exactly the mistake worth making
		// impossible.
		req.Locality = DataLocalOnly
	}
	if req.Job == "" {
		req.Job = JobInteractive
	}
	if req.MaxEscalations <= 0 {
		req.MaxEscalations = DefaultMaxEscalations
	}
	if req.Attempt > req.MaxEscalations {
		return RouteSelection{}, fmt.Errorf(
			"%w: %d escalations already, which is the bound — trying every model in the house to answer one question is a decision for a person",
			ErrNoRoute, req.Attempt)
	}

	routes, err := db.ListModelRoutes()
	if err != nil {
		return RouteSelection{}, err
	}
	if err := db.sweepExpiredLeases(); err != nil {
		return RouteSelection{}, err
	}

	for _, r := range routes {
		if !r.Enabled || r.Readiness == RouteUnavailable {
			continue
		}
		// 1. Locality, first and absolutely. Local-only material reaches
		//    a local route and nothing else.
		if req.Locality == DataLocalOnly && r.Kind != RouteLocal {
			continue
		}
		// Background processing is local-first: it does not escalate to
		// a cloud route just because the local one is busy, because
		// nobody is waiting for it (A9.1).
		if req.Job == JobBackground && r.Kind != RouteLocal {
			continue
		}
		// 2. Capacity.
		inFlight, err := db.routeLeaseCount(r.Seq)
		if err != nil {
			return RouteSelection{}, err
		}
		if inFlight >= r.MaxConcurrency {
			continue
		}
		leaseSeq, err := db.takeRouteLease(r.Seq, req.Holder, req.Job)
		if err != nil {
			return RouteSelection{}, err
		}
		// 3. Authority. A route that has not passed its probe answers
		//    questions and carries no action authority, and the caller
		//    recomputes its manifest from this verdict.
		sel := RouteSelection{
			Route: r, LeaseSeq: leaseSeq, Verdict: VerdictFullConst,
			Escalated: req.Attempt > 0,
		}
		if !r.CarriesActionAuthority() {
			sel.Verdict = VerdictQAOnlyConst
			sel.Reason = fmt.Sprintf("%s is %s, so this session answers questions and uses no tools that do things",
				r.Name, r.Readiness)
		}
		if sel.Escalated && sel.Reason == "" {
			sel.Reason = fmt.Sprintf("the earlier choices were unavailable or busy, so %s answered", r.Name)
		}
		return sel, nil
	}

	why := "every route is unavailable, disabled, or busy"
	if req.Locality == DataLocalOnly {
		why = "this material must not leave the house, and no local route is available"
	} else if req.Job == JobBackground {
		why = "background work runs on local routes only, and none is available"
	}
	return RouteSelection{}, fmt.Errorf("%w: %s", ErrNoRoute, why)
}

// Verdict constants, mirroring the Butler's own vocabulary without
// importing it.
const (
	VerdictFullConst   = "full"
	VerdictQAOnlyConst = "qa-only"
)

func (db *DB) routeLeaseCount(route int64) (int, error) {
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM model_route_leases WHERE route = ? AND released_at = '' AND expires_at > ?`,
		route, now()).Scan(&n); err != nil {
		return 0, fmt.Errorf("runtimedb: counting route leases: %w", err)
	}
	return n, nil
}

func (db *DB) takeRouteLease(route int64, holder, job string) (int64, error) {
	if holder == "" {
		holder = "unnamed"
	}
	res, err := db.sql.Exec(
		`INSERT INTO model_route_leases (route, holder, job, taken_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		route, holder, job, now(), time.Now().UTC().Add(DefaultLeaseTTL).Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("runtimedb: taking a route lease: %w", err)
	}
	return res.LastInsertId()
}

// sweepExpiredLeases releases slots whose holder went away. A crashed
// caller must not hold a local model's only slot forever.
func (db *DB) sweepExpiredLeases() error {
	_, err := db.sql.Exec(
		`UPDATE model_route_leases SET released_at = ? WHERE released_at = '' AND expires_at <= ?`,
		now(), now())
	if err != nil {
		return fmt.Errorf("runtimedb: sweeping route leases: %w", err)
	}
	return nil
}

// ReleaseModelRoute frees a capacity slot.
func (db *DB) ReleaseModelRoute(leaseSeq int64) error {
	_, err := db.sql.Exec(
		`UPDATE model_route_leases SET released_at = ? WHERE seq = ? AND released_at = ''`,
		now(), leaseSeq)
	if err != nil {
		return fmt.Errorf("runtimedb: releasing a route lease: %w", err)
	}
	return nil
}
