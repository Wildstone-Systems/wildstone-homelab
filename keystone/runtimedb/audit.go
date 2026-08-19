package runtimedb

import (
	"fmt"
	"time"
)

// AuditRecord is one public-plane audit row (ADR-W007 A4): one per request
// that passed authn, allowed or denied. The agent plane keeps its own dual
// audit trail (ADR-W003 W7); they answer different questions.
type AuditRecord struct {
	Seq       int64     `json:"seq"`
	Time      time.Time `json:"time"`
	Principal string    `json:"principal"`
	ActingFor string    `json:"actingFor,omitempty"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Remote    string    `json:"remote,omitempty"`
}

// AppendAudit appends one audit row. Time is stamped here.
func (db *DB) AppendAudit(r AuditRecord) error {
	if r.Principal == "" || r.Method == "" || r.Path == "" {
		return fmt.Errorf("runtimedb: audit principal, method, and path required")
	}
	_, err := db.sql.Exec(
		`INSERT INTO audit (time, principal, acting_for, method, path, status, remote)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		now(), r.Principal, r.ActingFor, r.Method, r.Path, r.Status, r.Remote)
	if err != nil {
		return fmt.Errorf("runtimedb: appending audit: %w", err)
	}
	return nil
}

// ListAudit returns the newest limit audit rows, newest first.
func (db *DB) ListAudit(limit int) ([]AuditRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.sql.Query(
		`SELECT seq, time, principal, acting_for, method, path, status, remote
		 FROM audit ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing audit: %w", err)
	}
	defer rows.Close()
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		var ts string
		if err := rows.Scan(&r.Seq, &ts, &r.Principal, &r.ActingFor, &r.Method, &r.Path, &r.Status, &r.Remote); err != nil {
			return nil, err
		}
		if r.Time, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return nil, fmt.Errorf("runtimedb: bad audit timestamp %q: %w", ts, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
