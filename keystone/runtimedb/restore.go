package runtimedb

// Restore quarantine and reconciliation (ADR-W030 A10/A10.1, P16).
//
// An old backup does not contain later decisions. "We restored last
// week's database" must not mean "the thing you asked us to forget is
// back", and it must not mean "the person you offboarded on Tuesday can
// talk to the Butler again".
//
// So a restored database starts QUARANTINED, and quarantine is the
// default rather than something an operator remembers to switch on.
// Nothing Butler-facing runs while it holds: no retrieval, no task
// dispatch, no ingestion, no proactive delivery. Release requires signed
// terminal evidence retained outside the snapshot, exact prefix/sequence
// verification and replay. Administrative acknowledgement is authorization,
// not freshness evidence. Reconciliation records those facts but deliberately
// leaves quarantine active for the separate release operation.
//
// Work whose outcome was uncertain when the snapshot was taken is still
// uncertain, so it stays paused rather than being resumed by a
// coordinator that has just come up feeling optimistic.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/proto/secreterase"
)

// ErrQuarantined is returned by anything Butler-facing while a restored
// database has not been reconciled.
var ErrQuarantined = errors.New("runtimedb: this database is in restore quarantine and has not been reconciled")

// RestoreState is where a restored database is in its reconciliation.
type RestoreState struct {
	Quarantined            bool      `json:"quarantined"`
	EnteredAt              time.Time `json:"enteredAt,omitzero"`
	Reason                 string    `json:"reason,omitempty"`
	SnapshotAt             time.Time `json:"snapshotAt,omitzero"`
	JournalCurrent         bool      `json:"journalCurrent"`
	JournalAssertedBy      string    `json:"journalAssertedBy,omitempty"`
	ReplayedAt             time.Time `json:"replayedAt,omitzero"`
	ReplayedCount          int       `json:"replayedCount"`
	ReleasedAt             time.Time `json:"releasedAt,omitzero"`
	ReleasedBy             string    `json:"releasedBy,omitempty"`
	SnapshotSHA256         string    `json:"snapshotSha256,omitempty"`
	JournalEvidenceID      string    `json:"journalEvidenceId,omitempty"`
	JournalSource          string    `json:"journalSource,omitempty"`
	JournalHeadSeq         int64     `json:"journalHeadSequence"`
	JournalHeadSHA256      string    `json:"journalHeadSha256,omitempty"`
	JournalCoverageThrough time.Time `json:"journalCoverageThrough,omitzero"`
	JournalVerifiedAt      time.Time `json:"journalVerifiedAt,omitzero"`
	JournalVerifiedBy      string    `json:"journalVerifiedBy,omitempty"`
	RestoreGeneration      string    `json:"-"`
}

// EnterRestoreQuarantine marks this database as restored and holds
// everything Butler-facing.
func (db *DB) EnterRestoreQuarantine(reason string, snapshotAt time.Time) error {
	return db.enterRestoreQuarantine(reason, snapshotAt, "")
}

func (db *DB) enterRestoreQuarantine(reason string, snapshotAt time.Time, snapshotSHA256 string) error {
	if reason == "" {
		return fmt.Errorf("runtimedb: entering quarantine needs a reason — an operator reading this later has to know what was restored")
	}
	var generationBytes [16]byte
	if _, err := rand.Read(generationBytes[:]); err != nil {
		return fmt.Errorf("runtimedb: creating restore generation: %w", err)
	}
	restoreGeneration := hex.EncodeToString(generationBytes[:])
	_, err := db.sql.Exec(
		`INSERT INTO restore_state (id, quarantined, entered_at, reason, snapshot_at, snapshot_sha256, restore_generation)
		 VALUES (1, 1, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET quarantined = 1, entered_at = excluded.entered_at,
		   reason = excluded.reason, snapshot_at = excluded.snapshot_at, snapshot_sha256 = excluded.snapshot_sha256,
		   restore_generation = excluded.restore_generation,
		   journal_current = 0, journal_asserted_by = '', replayed_at = '', replayed_count = 0,
		   released_at = '', released_by = '', journal_evidence_id = '', journal_source = '',
		   journal_head_seq = 0, journal_head_sha256 = '', journal_coverage_through = '',
		   journal_verified_at = '', journal_verified_by = ''`,
		now(), reason, timeText(snapshotAt), snapshotSHA256, restoreGeneration)
	if err != nil {
		return fmt.Errorf("runtimedb: entering restore quarantine: %w", err)
	}
	// Entering quarantine revokes this process's in-memory execution handle.
	// Releasing quarantine never restores it: post-restore coordinator
	// authority is a separate, durable promotion decision (#286).
	db.executionLease.Store(nil)
	return nil
}

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

func getRestoreState(q queryRower) (RestoreState, error) {
	var s RestoreState
	var quarantined, journalCurrent int
	var entered, snapshot, replayed, released, coverage, verified string
	err := q.QueryRow(
		`SELECT quarantined, entered_at, reason, snapshot_at, journal_current,
		        journal_asserted_by, replayed_at, replayed_count, released_at, released_by,
		        snapshot_sha256, journal_evidence_id, journal_source, journal_head_seq,
		        journal_head_sha256, journal_coverage_through, journal_verified_at, journal_verified_by,
		        restore_generation
		 FROM restore_state WHERE id = 1`).
		Scan(&quarantined, &entered, &s.Reason, &snapshot, &journalCurrent,
			&s.JournalAssertedBy, &replayed, &s.ReplayedCount, &released, &s.ReleasedBy,
			&s.SnapshotSHA256, &s.JournalEvidenceID, &s.JournalSource, &s.JournalHeadSeq,
			&s.JournalHeadSHA256, &coverage, &verified, &s.JournalVerifiedBy,
			&s.RestoreGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		// A database that was never restored is not quarantined. This is
		// the ordinary case and must not read as an error.
		return RestoreState{}, nil
	}
	if err != nil {
		return RestoreState{}, fmt.Errorf("runtimedb: reading restore state: %w", err)
	}
	s.Quarantined, s.JournalCurrent = quarantined != 0, journalCurrent != 0
	for _, f := range []struct {
		text string
		into *time.Time
	}{{entered, &s.EnteredAt}, {snapshot, &s.SnapshotAt}, {replayed, &s.ReplayedAt}, {released, &s.ReleasedAt},
		{coverage, &s.JournalCoverageThrough}, {verified, &s.JournalVerifiedAt}} {
		if f.text == "" {
			continue
		}
		parsed, perr := time.Parse(time.RFC3339Nano, f.text)
		if perr != nil {
			return RestoreState{}, fmt.Errorf("runtimedb: bad restore timestamp %q: %w", f.text, perr)
		}
		*f.into = parsed
	}
	return s, nil
}

// GetRestoreState returns where reconciliation has got to.
func (db *DB) GetRestoreState() (RestoreState, error) {
	return getRestoreState(db.sql)
}

// CheckQuarantine is what every Butler-facing path calls before doing
// anything. It is cheap and it is meant to be called every time: a
// cached answer is how a quarantined database ends up dispatching.
func (db *DB) CheckQuarantine() error {
	s, err := db.GetRestoreState()
	if err != nil {
		return err
	}
	if s.Quarantined {
		return fmt.Errorf("%w (restored %s: %s)", ErrQuarantined,
			s.EnteredAt.Format(time.RFC3339), s.Reason)
	}
	return nil
}

// The four things quarantine holds (ADR-W030 A10). They are named
// constants because each gate reports which category refused it, and a
// person reading "Butler retrieval is held" learns more than one reading
// "quarantined".
const (
	GateRetrieval = "Butler retrieval"
	GateDispatch  = "task dispatch"
	GateIngestion = "ingestion"
	GateDelivery  = "proactive delivery"
)

// gate is what every Butler-facing chokepoint calls.
//
// The gates live on the STORE rather than on the HTTP routes, and that
// is the whole design decision. Route middleware only catches callers
// that arrive over HTTP, so a background loop, a chat adapter, or a
// route somebody adds next month would walk straight past it. Putting
// the check where the work actually happens means a new caller is
// covered the day it is written rather than the day somebody remembers
// to add it to a list.
//
// It costs one indexed single-row read per call, against operations
// that are already doing real queries. That is the right trade: a
// cached answer is exactly how a quarantined database ends up
// dispatching anyway.
func (db *DB) gate(what string) error {
	return gateQuery(db.sql, what)
}

// gateTx gives operations whose authority and effect are one transaction the
// same fail-closed restore and household-stop checks inside that transaction.
func gateTx(tx *sql.Tx, what string) error {
	return gateQuery(tx, what)
}

func gateQuery(q queryRower, what string) error {
	s, err := getRestoreState(q)
	if err != nil {
		// A store that cannot say whether it is quarantined has not said
		// no. Fail closed: this is the one place where being unable to
		// check must not read as permission.
		return fmt.Errorf("%w: could not check restore state before %s: %v", ErrQuarantined, what, err)
	}
	if !s.Quarantined {
		if what == GateDispatch || what == GateDelivery {
			// A release opens reconciled data but does not grant execution.
			// Every dispatch/delivery chokepoint remains durably closed for a
			// restore generation until #286 records a separate promotion.
			if s.RestoreGeneration != "" {
				return ErrCoordinatorRestoreFenced
			}
			return checkHouseholdStop(q, what)
		}
		return nil
	}
	return fmt.Errorf("%w: %s is held until the restore is reconciled (restored %s: %s)",
		ErrQuarantined, what, s.EnteredAt.Format(time.RFC3339), s.Reason)
}

// AssertJournalCurrent retains the older administrative assertion for audit
// and compatibility with internal replay callers.
//
// It is not evidence. The schema refuses release of a restored snapshot until
// ReconcileRestoreEvidence has stored a verified evidence identity.
func (db *DB) AssertJournalCurrent(by string) error {
	if by == "" {
		return fmt.Errorf("runtimedb: asserting the journal is current needs who asserted it")
	}
	res, err := db.sql.Exec(
		`UPDATE restore_state SET journal_current = 1, journal_asserted_by = ? WHERE id = 1`, by)
	if err != nil {
		return fmt.Errorf("runtimedb: asserting the journal is current: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// replayed narrows a replay result to the errors that actually matter.
//
// Replaying a deletion journal over restored state re-applies things
// that in many cases are already applied — the snapshot may postdate
// some of them. ErrNotFound is what "already done" looks like from
// every revoke in this package, and treating it as a failure would
// make an ordinary reconciliation refuse to finish.
//
// Everything else propagates. A replay that swallows a real failure
// and still reports success is precisely the hole quarantine exists to
// close: the journal says this was revoked, the release says the
// journal was replayed, and the credential is still live.
func replayed(err error) error {
	if err == nil || errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ReconcileRestore replays the deletion journal over restored state.
//
// This is the mutation/replay half. It does not prove that the in-database
// journal is fresh; ReconcileRestoreEvidence does that first. Every supplied
// deletion and revocation is re-applied, so content the snapshot still contains and that was
// removed afterwards is removed again — a forgotten memory stays
// forgotten, an offboarded member stays denied, a revoked link stays
// revoked.
//
// It returns how many entries it applied, because "reconciliation ran"
// with no number behind it is indistinguishable from reconciliation that
// found nothing to do.
func (db *DB) ReconcileRestore() (applied int, err error) {
	return db.reconcileRestore("")
}

// reconcileRestore binds journal rows generated by replay-time triggers to
// recoveryEvidenceID. Public, non-evidence replay passes an empty id and keeps
// the ordinary journal behavior.
func (db *DB) reconcileRestore(recoveryEvidenceID string) (applied int, err error) {
	rows, err := db.sql.Query(
		`SELECT subject_kind, subject_id, action, actor FROM deletion_journal ORDER BY seq`)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: reading the deletion journal: %w", err)
	}
	type entry struct{ kind, id, action, actor string }
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.kind, &e.id, &e.action, &e.actor); err != nil {
			rows.Close()
			return 0, err
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, e := range entries {
		switch e.kind {
		case "member":
			// Re-deny. A member offboarded after the snapshot is still
			// offboarded, whatever the restored admission row says.
			if m, err := db.GetButlerMember(e.id); err == nil && m.Active() {
				if err := db.offboardButlerMemberForRecovery(e.id, e.actor, recoveryEvidenceID); err != nil {
					return applied, err
				}
			}
		case "link":
			// Token revocation: the id is "api-tokens:<principal>".
			if principal, ok := strings.CutPrefix(e.id, apiTokensJournalPrefix); ok {
				if _, err := db.RevokeTokensFor(principal); err != nil {
					return applied, err
				}
				applied++
				continue
			}
			// Re-revoke. The id is "<type>:<external>".
			for i := range e.id {
				if e.id[i] == ':' {
					if err := replayed(db.revokeSurfaceLinkForRecovery(e.id[:i], e.id[i+1:], e.actor, recoveryEvidenceID)); err != nil {
						return applied, err
					}
					break
				}
			}
		case "memory":
			var seq int64
			if _, err := fmt.Sscanf(e.id, "%d", &seq); err == nil {
				if m, err := db.GetMemory(seq); err == nil && m.ForgottenAt.IsZero() {
					if err := db.ForgetMemory(seq, e.actor); err != nil {
						return applied, err
					}
				}
				if _, err := db.RemoveIndexForMemory(seq); err != nil {
					return applied, err
				}
			}
		case "index-source":
			// A source whose indexing was revoked must not be
			// searchable again.
			for i := range e.id {
				if e.id[i] == ':' {
					if _, err := db.RemoveIndexForSource(e.id[:i], e.id[i+1:]); err != nil {
						return applied, err
					}
					break
				}
			}
		case "connection":
			if _, err := db.GetConnection(e.id); err == nil {
				if _, err := db.BumpConnectionEpoch(e.id); err != nil {
					return applied, err
				}
				if err := db.SetConnectionState(e.id, ConnectionUnavailable,
					"revoked after the restored snapshot was taken"); err != nil {
					return applied, err
				}
			}
			if e.action == "deleted" {
				// A mailbox monitor whose owner was offboarded after the
				// snapshot: what it learned is deleted again, before
				// anyone can read it (epic #326 S08).
				var erased MailStateErased
				if err := db.eraseMailConnectionState(e.id, &erased); err != nil {
					return applied, err
				}
			}
		case "routine":
			if r, err := db.GetHouseholdRoutine(e.id); err == nil && r.DisabledAt.IsZero() {
				if err := db.DisableHouseholdRoutine(e.id, e.actor); err != nil {
					return applied, err
				}
			}
		case "binding":
			var id int64
			if _, err := fmt.Sscanf(e.id, "%d", &id); err == nil {
				if err := replayed(db.RevokeChatBinding(id)); err != nil {
					return applied, err
				}
			}
		case "grant":
			// A Person's background authorization revoked after the
			// snapshot stays revoked: the restored row is not a reason to
			// run again.
			var seq int64
			if id, ok := strings.CutPrefix(e.id, backgroundAuthorizationJournalPrefix); !ok {
				break
			} else if _, err := fmt.Sscanf(id, "%d", &seq); err == nil {
				if err := replayed(db.RevokeBackgroundAuthorizationSeq(seq, e.actor,
					"revoked after the restored snapshot was taken")); err != nil {
					return applied, err
				}
			}
		case "secret":
			if strings.HasPrefix(e.id, secreterase.PersonalPrefix) {
				// A managed Personal credential: fenced again — revoked,
				// its authorizations with it, every copy an erasure to
				// prove — and left erasure_unverified until the nodes
				// prove it, because nobody has been to them since.
				if err := replayed(db.FencePersonalCredentialRef(e.id, e.actor,
					"revoked after the restored snapshot was taken")); err != nil {
					return applied, err
				}
				break
			}
			if strings.HasPrefix(e.id, "household-secret/") {
				state := "quarantine"
				if e.action == "deleted" {
					state = "deleted"
				}
				id := strings.TrimPrefix(e.id, "household-secret/")
				tx, xerr := db.sql.Begin()
				if xerr != nil {
					return applied, xerr
				}
				_, xerr = tx.Exec(`UPDATE household_secrets SET state=?,ciphertext=CASE WHEN ?='deleted' THEN X'' ELSE ciphertext END WHERE id=?`, state, state, id)
				if xerr == nil {
					_, xerr = tx.Exec(`DELETE FROM household_secret_agents WHERE secret_id=?`, id)
				}
				if xerr == nil {
					xerr = tx.Commit()
				} else {
					tx.Rollback()
				}
				if xerr != nil {
					return applied, xerr
				}
				break
			}

			// Secrets are revoked per connection; the journal records
			// the reference, and the sweep below finds it wherever it
			// still lives.
			connections, err := db.ListConnections()
			if err != nil {
				return applied, err
			}
			for _, c := range connections {
				secrets, err := db.ListConnectionSecrets(c.Name)
				if err != nil {
					return applied, err
				}
				for _, s := range secrets {
					if s.SecretRef != e.id || !s.RevokedAt.IsZero() || s.Managed() {
						continue
					}
					err := db.RevokeConnectionSecret(c.Name, s.Seq)
					if errors.Is(err, ErrSecretReferenced) {
						// The snapshot still shows a live placement
						// holding a credential the journal says was
						// revoked. A restored placement row is not a
						// reason to leave it live, so the placements
						// are fenced into removal first — which bumps
						// the epoch, stopping the old container's work
						// — and then the revocation goes through.
						//
						// Their removal is deliberately left UNPROVEN.
						// Nobody has been to those machines, so the
						// connection stays degraded and says so, which
						// is the true statement about a credential
						// that may still be sitting on one.
						placements, perr := db.ListPlacements(c.Name)
						if perr != nil {
							return applied, perr
						}
						for _, p := range placements {
							if !p.SecretProvisioned || p.Removing {
								continue
							}
							if rerr := replayed(db.BeginPlacementRemoval(c.Name, p.Node)); rerr != nil {
								return applied, rerr
							}
						}
						err = db.RevokeConnectionSecret(c.Name, s.Seq)
					}
					if err := replayed(err); err != nil {
						return applied, err
					}
				}
			}
		}
		applied++
	}

	// Work whose outcome was unknown stays paused. A restored
	// coordinator that resumes it is doing the one thing A4 forbids:
	// retrying an action that may already have happened.
	if _, err := db.sql.Exec(
		`UPDATE butler_tasks SET state = ?, updated_at = ?
		 WHERE state IN (?, ?) AND (uncertainty <> '' OR seq IN (
		     SELECT DISTINCT task FROM butler_task_attempts WHERE state = ?))`,
		ButlerTaskPausedOffboarded, now(), ButlerTaskRunning, ButlerTaskWaiting,
		ButlerAttemptDispatched); err != nil {
		return applied, fmt.Errorf("runtimedb: pausing uncertain work after a restore: %w", err)
	}

	if _, err := db.sql.Exec(
		`UPDATE restore_state SET replayed_at = ?, replayed_count = ? WHERE id = 1`,
		now(), applied); err != nil {
		return applied, fmt.Errorf("runtimedb: recording the replay: %w", err)
	}
	return applied, nil
}
