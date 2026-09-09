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
		     readiness_detail, enabled, created_at, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   provider = excluded.provider, model = excluded.model, endpoint = excluded.endpoint,
		   enabled = excluded.enabled, updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by`,
		r.Name, r.Kind, r.Provider, r.Model, r.Endpoint, r.Readiness,
		enabled, now(), now(), by)
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
		        probed_at, enabled
		 FROM model_routes ORDER BY kind, name`)
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
			&r.Readiness, &r.ReadinessDetail, &probed, &enabled); err != nil {
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
