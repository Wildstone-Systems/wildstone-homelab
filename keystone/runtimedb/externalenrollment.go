package runtimedb

// Person-bound one-time enrollment challenges (ADR-W049, #598).
//
// These helpers are storage primitives for a future independently
// authenticated Person service. They are not HTTP handlers, do not authenticate
// a Person, do not activate a registration, and confer no Agent API authority.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const ExternalEnrollmentChallengeTTL = 15 * time.Minute

type ExternalEnrollmentChallenge struct {
	ID                 string    `json:"id"`
	RegistrationID     string    `json:"registrationId"`
	PersonID           string    `json:"personId"`
	IssuedAt           time.Time `json:"issuedAt"`
	ExpiresAt          time.Time `json:"expiresAt"`
	IssuedBy           string    `json:"issuedBy"`
	ConsumedAt         time.Time `json:"consumedAt,omitzero"`
	ConsumedBy         string    `json:"consumedBy,omitempty"`
	InvalidatedAt      time.Time `json:"invalidatedAt,omitzero"`
	InvalidatedBy      string    `json:"invalidatedBy,omitempty"`
	InvalidationReason string    `json:"invalidationReason,omitempty"`
}

// IssuedExternalEnrollmentChallenge returns the opaque proof exactly once.
// Secret is deliberately excluded from JSON so general serialization cannot
// turn this short-lived credential into a listing or audit field.
type IssuedExternalEnrollmentChallenge struct {
	Challenge ExternalEnrollmentChallenge `json:"challenge"`
	Secret    string                      `json:"-"`
}

type IssueExternalEnrollmentChallenge struct {
	RegistrationID string
	PersonID       string
	IssuedBy       string
}

type ConsumeExternalEnrollmentChallenge struct {
	RegistrationID string
	PersonID       string
	Secret         string
	ConsumedBy     string
}

func newExternalEnrollmentMaterial() (id, secret, digest string, err error) {
	idRaw := make([]byte, 16)
	secretRaw := make([]byte, 32)
	if _, err = rand.Read(idRaw); err != nil {
		return "", "", "", fmt.Errorf("runtimedb: generating external enrollment challenge id: %w", err)
	}
	if _, err = rand.Read(secretRaw); err != nil {
		return "", "", "", fmt.Errorf("runtimedb: generating external enrollment proof: %w", err)
	}
	id = "xchal_" + hex.EncodeToString(idRaw)
	secret = "xproof_" + base64.RawURLEncoding.EncodeToString(secretRaw)
	sum := sha256.Sum256([]byte(secret))
	return id, secret, hex.EncodeToString(sum[:]), nil
}

func externalEnrollmentDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (db *DB) challengeNow() time.Time {
	if db.challengeClock != nil {
		return db.challengeClock().UTC()
	}
	return time.Now().UTC()
}

// IssueExternalEnrollmentChallenge is called only after a future service has
// independently authenticated the exact canonical Person. PersonID is not a
// client/body claim: callers must pass the identity established at that trusted
// service boundary. This method rechecks it and every stored binding in the
// same gated transaction as issuance.
func (db *DB) IssueExternalEnrollmentChallenge(in IssueExternalEnrollmentChallenge) (IssuedExternalEnrollmentChallenge, error) {
	in.RegistrationID = strings.TrimSpace(in.RegistrationID)
	in.PersonID = strings.TrimSpace(in.PersonID)
	in.IssuedBy = strings.TrimSpace(in.IssuedBy)
	if in.RegistrationID == "" || in.PersonID == "" || in.IssuedBy == "" {
		return IssuedExternalEnrollmentChallenge{}, fmt.Errorf("runtimedb: issuing an external enrollment challenge needs registration, canonical Person, and authenticated actor")
	}
	id, secret, digest, err := newExternalEnrollmentMaterial()
	if err != nil {
		return IssuedExternalEnrollmentChallenge{}, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return IssuedExternalEnrollmentChallenge{}, err
	}
	defer tx.Rollback()
	if err := gateTx(tx, GateDispatch); err != nil {
		return IssuedExternalEnrollmentChallenge{}, err
	}
	r, err := externalRegistrationForChallenge(tx, in.RegistrationID, in.PersonID)
	if err != nil {
		return IssuedExternalEnrollmentChallenge{}, err
	}
	if err := validateExternalRegistrationAuthority(tx, r); err != nil {
		return IssuedExternalEnrollmentChallenge{}, err
	}
	issuedAt := db.challengeNow()
	expiresAt := issuedAt.Add(ExternalEnrollmentChallengeTTL)
	_, err = tx.Exec(`INSERT INTO external_enrollment_challenges
		(id,registration_id,person_id,secret_hash,issued_at,expires_at,issued_by)
		VALUES(?,?,?,?,?,?,?)`, id, r.ID, r.PersonID, digest,
		issuedAt.Format(time.RFC3339Nano), expiresAt.Format(time.RFC3339Nano), in.IssuedBy)
	if err != nil {
		return IssuedExternalEnrollmentChallenge{}, fmt.Errorf("runtimedb: issuing external enrollment challenge: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return IssuedExternalEnrollmentChallenge{}, err
	}
	challenge, err := db.ExternalEnrollmentChallenge(id)
	if err != nil {
		return IssuedExternalEnrollmentChallenge{}, err
	}
	return IssuedExternalEnrollmentChallenge{Challenge: challenge, Secret: secret}, nil
}

func externalRegistrationForChallenge(q queryRower, registrationID, personID string) (ExternalClientRegistration, error) {
	r, err := scanExternalRegistration(q.QueryRow(externalRegistrationColumns+` WHERE id=? AND person_id=?`, registrationID, personID))
	if err != nil {
		return ExternalClientRegistration{}, err
	}
	if r.State != ExternalRegistrationPendingConsent {
		return ExternalClientRegistration{}, ErrNotAdmitted
	}
	return r, nil
}

const externalEnrollmentColumns = `SELECT id,registration_id,person_id,issued_at,expires_at,issued_by,
	consumed_at,consumed_by,invalidated_at,invalidated_by,invalidation_reason
	FROM external_enrollment_challenges`

func scanExternalEnrollmentChallenge(row rowScanner) (ExternalEnrollmentChallenge, error) {
	var c ExternalEnrollmentChallenge
	var issued, expires, consumed, invalidated string
	err := row.Scan(&c.ID, &c.RegistrationID, &c.PersonID, &issued, &expires, &c.IssuedBy,
		&consumed, &c.ConsumedBy, &invalidated, &c.InvalidatedBy, &c.InvalidationReason)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalEnrollmentChallenge{}, ErrNotFound
	}
	if err != nil {
		return ExternalEnrollmentChallenge{}, fmt.Errorf("runtimedb: reading external enrollment challenge: %w", err)
	}
	for _, field := range []struct {
		value  string
		target *time.Time
	}{{issued, &c.IssuedAt}, {expires, &c.ExpiresAt}, {consumed, &c.ConsumedAt}, {invalidated, &c.InvalidatedAt}} {
		if field.value == "" {
			continue
		}
		parsed, parseErr := time.Parse(time.RFC3339Nano, field.value)
		if parseErr != nil {
			return ExternalEnrollmentChallenge{}, fmt.Errorf("runtimedb: bad enrollment challenge timestamp %q: %w", field.value, parseErr)
		}
		*field.target = parsed
	}
	return c, nil
}

func (db *DB) ExternalEnrollmentChallenge(id string) (ExternalEnrollmentChallenge, error) {
	return scanExternalEnrollmentChallenge(db.sql.QueryRow(externalEnrollmentColumns+` WHERE id=?`, strings.TrimSpace(id)))
}

// verifyExternalEnrollmentChallengeTx verifies without consuming. It is kept
// transaction-scoped so the future activation transaction can add issuer
// state/nonce, subject, uniqueness, and profile checks before atomically
// calling consumeExternalEnrollmentChallengeTx and committing.
func (db *DB) verifyExternalEnrollmentChallengeTx(tx *sql.Tx, in ConsumeExternalEnrollmentChallenge) (ExternalEnrollmentChallenge, error) {
	in.RegistrationID = strings.TrimSpace(in.RegistrationID)
	in.PersonID = strings.TrimSpace(in.PersonID)
	in.Secret = strings.TrimSpace(in.Secret)
	if in.RegistrationID == "" || in.PersonID == "" || in.Secret == "" {
		return ExternalEnrollmentChallenge{}, ErrNotFound
	}
	if err := gateTx(tx, GateDispatch); err != nil {
		return ExternalEnrollmentChallenge{}, err
	}
	r, err := externalRegistrationForChallenge(tx, in.RegistrationID, in.PersonID)
	if err != nil {
		return ExternalEnrollmentChallenge{}, err
	}
	if err := validateExternalRegistrationAuthority(tx, r); err != nil {
		return ExternalEnrollmentChallenge{}, err
	}
	c, err := scanExternalEnrollmentChallenge(tx.QueryRow(externalEnrollmentColumns+`
		WHERE registration_id=? AND person_id=? AND secret_hash=?`, r.ID, r.PersonID, externalEnrollmentDigest(in.Secret)))
	if err != nil {
		return ExternalEnrollmentChallenge{}, err
	}
	if !c.ConsumedAt.IsZero() || !c.InvalidatedAt.IsZero() || !db.challengeNow().Before(c.ExpiresAt) {
		return ExternalEnrollmentChallenge{}, ErrNotFound
	}
	return c, nil
}

// consumeExternalEnrollmentChallengeTx marks a verified proof spent inside
// the caller's transaction. There is intentionally no DB-level standalone
// consumer: proof consumption and future activation must commit or roll back
// together.
func (db *DB) consumeExternalEnrollmentChallengeTx(tx *sql.Tx, in ConsumeExternalEnrollmentChallenge) (ExternalEnrollmentChallenge, error) {
	in.ConsumedBy = strings.TrimSpace(in.ConsumedBy)
	if in.ConsumedBy == "" {
		return ExternalEnrollmentChallenge{}, fmt.Errorf("runtimedb: consuming an external enrollment challenge needs an authenticated actor")
	}
	c, err := db.verifyExternalEnrollmentChallengeTx(tx, in)
	if err != nil {
		return ExternalEnrollmentChallenge{}, err
	}
	stamp := db.challengeNow().Format(time.RFC3339Nano)
	res, err := tx.Exec(`UPDATE external_enrollment_challenges SET consumed_at=?,consumed_by=?
		WHERE id=? AND consumed_at='' AND invalidated_at=''`, stamp, in.ConsumedBy, c.ID)
	if err != nil {
		return ExternalEnrollmentChallenge{}, fmt.Errorf("runtimedb: consuming external enrollment challenge: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ExternalEnrollmentChallenge{}, ErrNotFound
	}
	return scanExternalEnrollmentChallenge(tx.QueryRow(externalEnrollmentColumns+` WHERE id=?`, c.ID))
}

// InvalidateExternalEnrollmentChallenges terminally invalidates every
// outstanding proof for one registration. Denial-only invalidation stays
// available during execution stop and restore quarantine.
func (db *DB) InvalidateExternalEnrollmentChallenges(registrationID, by, reason string) error {
	registrationID = strings.TrimSpace(registrationID)
	by = strings.TrimSpace(by)
	reason = strings.TrimSpace(reason)
	if registrationID == "" || by == "" || reason == "" {
		return fmt.Errorf("runtimedb: invalidating enrollment challenges needs registration, actor, and reason")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := db.challengeNow().Format(time.RFC3339Nano)
	if err := invalidateExternalEnrollmentChallengesTx(tx, registrationID, by, reason, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func invalidateExternalEnrollmentChallengesTx(tx *sql.Tx, registrationID, by, reason, stamp string) error {
	_, err := tx.Exec(`UPDATE external_enrollment_challenges
		SET invalidated_at=?,invalidated_by=?,invalidation_reason=?
		WHERE registration_id=? AND consumed_at='' AND invalidated_at=''`, stamp, by, reason, registrationID)
	if err != nil {
		return fmt.Errorf("runtimedb: invalidating external enrollment challenges: %w", err)
	}
	return nil
}
