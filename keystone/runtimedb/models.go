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
	ModelLocal = "local"
	ModelCloud = "cloud"
)

// Route readiness, per route rather than per deployment.
const (
	ModelUnprobed    = "unprobed"
	ModelReady       = "ready"
	ModelQAOnly      = "qa-only"
	ModelUnavailable = "unavailable"
)

// Model is one named model a Butler can be assigned to: which model,
// where it runs, and whether asking it something leaves the house.
//
// The bundle exists rather than a bare model string on the Butler
// because none of its parts implies the others. One Ollama endpoint
// serves gemma4:e4b off local disk and gemma4:31b-cloud from Ollama's
// own infrastructure, so neither the endpoint nor the provider says
// where the material goes — Kind does, and it is the only field here
// that cannot be changed afterwards.
type Model struct {
	Seq             int64     `json:"seq"`
	Name            string    `json:"name"`
	Kind            string    `json:"kind"`
	Provider        string    `json:"provider"`
	ProviderModel   string    `json:"providerModel,omitempty"`
	Endpoint        string    `json:"endpoint,omitempty"`
	Readiness       string    `json:"readiness"`
	ReadinessDetail string    `json:"readinessDetail,omitempty"`
	ProbedAt        time.Time `json:"probedAt,omitzero"`
	Enabled         bool      `json:"enabled"`
}

// CarriesActionAuthority reports whether a session answered by this
// model may use tools that do things.
//
// Only a model that has actually passed its probe. An unprobed one has
// not been checked and a QA-only one has been checked and found
// wanting, and neither should be trusted to decide whether to unlock a
// door (ADR-W017 A4, ADR-W030 A9).
func (r Model) CarriesActionAuthority() bool {
	return r.Enabled && r.Readiness == ModelReady
}

// UpsertModel records a model.
func (db *DB) UpsertModel(r Model, by string) error {
	switch r.Kind {
	case ModelLocal, ModelCloud:
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
		r.Readiness = ModelUnprobed
	}
	switch r.Readiness {
	case ModelUnprobed, ModelReady, ModelQAOnly, ModelUnavailable:
	default:
		return fmt.Errorf("runtimedb: route readiness must be unprobed|ready|qa-only|unavailable, got %q", r.Readiness)
	}
	enabled := 1
	if !r.Enabled && r.Seq == 0 && r.Readiness == ModelUnprobed {
		// A brand-new route defaults to enabled: the zero value of a
		// bool would otherwise make every created route disabled and
		// the failure would look like "routing is broken".
		enabled = 1
	} else if !r.Enabled {
		enabled = 0
	}
	_, err := db.sql.Exec(
		`INSERT INTO models (name, kind, provider, model, endpoint, readiness,
		     readiness_detail, enabled, created_at, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   provider = excluded.provider, model = excluded.model, endpoint = excluded.endpoint,
		   enabled = excluded.enabled, updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by`,
		r.Name, r.Kind, r.Provider, r.ProviderModel, r.Endpoint, r.Readiness,
		enabled, now(), now(), by)
	if err != nil {
		return fmt.Errorf("runtimedb: recording model route: %w", err)
	}
	return nil
}

// RecordModelReadiness stores a probe verdict for one route.
//
// It is separate from UpsertModel for the reason ADR-W017 A9 gives
// about the provider config: a verdict describes the model as it was
// probed, and carrying it across a change would let a model inherit a
// pass it never earned.
func (db *DB) RecordModelReadiness(name, readiness, detail string) error {
	switch readiness {
	case ModelUnprobed, ModelReady, ModelQAOnly, ModelUnavailable:
	default:
		return fmt.Errorf("runtimedb: route readiness must be unprobed|ready|qa-only|unavailable, got %q", readiness)
	}
	if readiness != ModelReady && detail == "" {
		return fmt.Errorf("runtimedb: a model reported %s must say why", readiness)
	}
	res, err := db.sql.Exec(
		`UPDATE models SET readiness = ?, readiness_detail = ?, probed_at = ?, updated_at = ?
		 WHERE name = ?`, readiness, detail, now(), now(), name)
	if err != nil {
		return fmt.Errorf("runtimedb: recording route readiness: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetModelEnabled turns a model off without deleting it.
func (db *DB) SetModelEnabled(name string, enabled bool, by string) error {
	res, err := db.sql.Exec(
		`UPDATE models SET enabled = ?, updated_at = ?, updated_by = ? WHERE name = ?`,
		boolInt(enabled), now(), by, name)
	if err != nil {
		return fmt.Errorf("runtimedb: setting route enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListModels returns routes in selection order.
func (db *DB) ListModels() ([]Model, error) {
	rows, err := db.sql.Query(
		`SELECT seq, name, kind, provider, model, endpoint, readiness, readiness_detail,
		        probed_at, enabled
		 FROM models ORDER BY kind, name`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing model routes: %w", err)
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		var r Model
		var probed string
		var enabled int
		if err := rows.Scan(&r.Seq, &r.Name, &r.Kind, &r.Provider, &r.ProviderModel, &r.Endpoint,
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

// GetModel reads one configured model by name.
func (db *DB) GetModel(name string) (Model, error) {
	models, err := db.ListModels()
	if err != nil {
		return Model{}, err
	}
	for _, m := range models {
		if m.Name == name {
			return m, nil
		}
	}
	return Model{}, ErrNotFound
}

// DeleteModel removes a configured model.
//
// It refuses while any Butler is assigned to it, INCLUDING a retired
// one. A retired Butler's conversations are still evidence, and the
// question they have to keep answering is "which model saw this" — so
// the row it names has to survive it. Deleting a model out from under
// that would turn an answer into a dangling id.
//
// Turning a model off is the reversible thing and the usual one; this
// is for a row that was never used, which is what accumulates when a
// household tries several and keeps one.
func (db *DB) DeleteModel(name string) error {
	var used int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butlers b JOIN models m ON m.seq = b.model WHERE m.name = ?`,
		name).Scan(&used); err != nil {
		return fmt.Errorf("runtimedb: checking whether a model is in use: %w", err)
	}
	if used > 0 {
		return fmt.Errorf(
			"runtimedb: %d butler(s) are assigned to %q — reassign or retire them first, or turn the model off instead of deleting it",
			used, name)
	}
	res, err := db.sql.Exec(`DELETE FROM models WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("runtimedb: deleting a model: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
