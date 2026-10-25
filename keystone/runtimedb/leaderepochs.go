package runtimedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LeaderEpoch is one issued leadership term (safety contract #180 §2.1).
type LeaderEpoch struct {
	Epoch    uint64
	Leader   string
	IssuedAt time.Time
}

// IssueLeaderEpoch records leader as the holder of the next epoch and
// returns it. The next epoch is above both every epoch issued here and
// floor, the epoch the cluster's authority record already holds: after a
// restore from an older backup this database can be behind the cluster,
// and an epoch must never be reused. It is one transaction: two
// concurrent calls never get the same epoch, and epochs only grow.
func (db *DB) IssueLeaderEpoch(leader string, floor uint64) (uint64, error) {
	if leader == "" {
		return 0, fmt.Errorf("runtimedb: leader identity required")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: issuing leader epoch: %w", err)
	}
	defer tx.Rollback()
	var last uint64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(epoch), 0) FROM leader_epochs`).Scan(&last); err != nil {
		return 0, fmt.Errorf("runtimedb: issuing leader epoch: %w", err)
	}
	next := max(last, floor) + 1
	if _, err := tx.Exec(`INSERT INTO leader_epochs (epoch, leader, issued_at) VALUES (?, ?, ?)`,
		next, leader, now()); err != nil {
		return 0, fmt.Errorf("runtimedb: issuing leader epoch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: issuing leader epoch: %w", err)
	}
	return next, nil
}

// CurrentLeaderEpoch is the newest issued epoch, or ok=false if none was.
func (db *DB) CurrentLeaderEpoch() (e LeaderEpoch, ok bool, err error) {
	var issued string
	err = db.sql.QueryRow(`SELECT epoch, leader, issued_at FROM leader_epochs ORDER BY epoch DESC LIMIT 1`).
		Scan(&e.Epoch, &e.Leader, &issued)
	if errors.Is(err, sql.ErrNoRows) {
		return LeaderEpoch{}, false, nil
	}
	if err != nil {
		return LeaderEpoch{}, false, fmt.Errorf("runtimedb: reading leader epoch: %w", err)
	}
	if e.IssuedAt, err = time.Parse(time.RFC3339Nano, issued); err != nil {
		return LeaderEpoch{}, false, fmt.Errorf("runtimedb: reading leader epoch: %w", err)
	}
	return e, true, nil
}
