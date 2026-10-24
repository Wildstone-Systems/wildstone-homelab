package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Observed is the latest observed status for one resource — the runtime
// half of the spec/status split (ADR-W004 A1: status never touches the
// Statefile). The reconciler writes it (E3); the public API serves it.
type Observed struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Node    string `json:"node,omitempty"`
	Healthy bool   `json:"healthy"`
	// AwaitingApproval means this resource is not converged because a
	// plan is waiting for a human, not because anything failed (F75).
	// Distinct from Healthy so a surface can say "waiting for you"
	// rather than colouring it the same red as a broken service.
	AwaitingApproval bool `json:"awaitingApproval,omitempty"`
	// AppHealth is the container's OWN healthcheck, verbatim from Docker
	// ("starting", "healthy", "unhealthy"), and empty when the image
	// declares none.
	//
	// Distinct from Healthy for the reason AwaitingApproval is: it
	// answers a different question. Healthy means Wildstone got the
	// container into the shape desired state asked for; this means the
	// thing inside it says it can serve. Empty is NO OPINION and must
	// never be rendered as a problem — most images declare no
	// healthcheck at all.
	AppHealth string    `json:"appHealth,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// UpsertObserved records the latest observation for (kind, name).
func (db *DB) UpsertObserved(o Observed) error {
	if o.Kind == "" || o.Name == "" {
		return fmt.Errorf("runtimedb: observed kind and name required")
	}
	_, err := db.sql.Exec(
		`INSERT INTO observed (kind, name, node, healthy, detail, awaiting_approval, app_health, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (kind, name) DO UPDATE SET
		   node = excluded.node, healthy = excluded.healthy,
		   detail = excluded.detail, awaiting_approval = excluded.awaiting_approval,
		   app_health = excluded.app_health,
		   updated_at = excluded.updated_at`,
		o.Kind, o.Name, o.Node, boolInt(o.Healthy), o.Detail, boolInt(o.AwaitingApproval), o.AppHealth, now())
	if err != nil {
		return fmt.Errorf("runtimedb: upserting observed %s/%s: %w", o.Kind, o.Name, err)
	}
	return nil
}

// DeleteObserved removes the observation for (kind, name) — a resource
// that has been successfully deleted has no status. Absent rows are a
// no-op.
func (db *DB) DeleteObserved(kind, name string) error {
	if _, err := db.sql.Exec(`DELETE FROM observed WHERE kind = ? AND name = ?`, kind, name); err != nil {
		return fmt.Errorf("runtimedb: deleting observed %s/%s: %w", kind, name, err)
	}
	return nil
}

// ListObserved returns every observation, (kind, name)-ordered.
func (db *DB) ListObserved() ([]Observed, error) {
	rows, err := db.sql.Query(
		`SELECT kind, name, node, healthy, detail, awaiting_approval, app_health, updated_at FROM observed ORDER BY kind, name`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing observed: %w", err)
	}
	defer rows.Close()
	var out []Observed
	for rows.Next() {
		var o Observed
		var healthy, awaiting int
		var ts string
		if err := rows.Scan(&o.Kind, &o.Name, &o.Node, &healthy, &o.Detail, &awaiting, &o.AppHealth, &ts); err != nil {
			return nil, err
		}
		o.Healthy, o.AwaitingApproval = healthy != 0, awaiting != 0
		if o.UpdatedAt, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return nil, fmt.Errorf("runtimedb: bad observed timestamp %q: %w", ts, err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------- statefile head

// StatefileHead returns the accepted revision: the last Statefile commit
// Keystone wrote or accepted, or "" if none has been recorded yet (fresh
// install). It is the only commit that is desired state (ADR-W007 A6,
// ADR-W008 A1 as amended); the branch head may be ahead of it until the
// ingestion loop has checked what was committed there.
func (db *DB) StatefileHead() (string, error) {
	var head string
	err := db.sql.QueryRow(`SELECT head FROM statefile_head WHERE id = 1`).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("runtimedb: reading statefile head: %w", err)
	}
	return head, nil
}

// SetStatefileHead records the Statefile commit hash Keystone just wrote
// or accepted (ADR-W007 A6).
func (db *DB) SetStatefileHead(head string) error {
	if head == "" {
		return fmt.Errorf("runtimedb: statefile head required")
	}
	_, err := db.sql.Exec(
		`INSERT INTO statefile_head (id, head) VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET head = excluded.head`, head)
	if err != nil {
		return fmt.Errorf("runtimedb: setting statefile head: %w", err)
	}
	return nil
}
