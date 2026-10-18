package runtimedb

// External-client registration storage and atomic activation (ADR-W049,
// #586, #603). Pending configuration remains inert. Activation is the one
// transaction that captures issuer identity, consumes Person proof, selects
// the OAuth profile, and retires static runtime credentials. OAuth resource
// authentication, task admission, MCP, delivery, and ack remain later slices.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ExternalRegistrationState string

const (
	ExternalRegistrationPendingConsent ExternalRegistrationState = "pending_consent"
	ExternalRegistrationActive         ExternalRegistrationState = "active"
	ExternalRegistrationRevoked        ExternalRegistrationState = "revoked"
)

// ExternalClientRegistration is immutable identity configuration plus its
// activation and terminal revocation record. Subject is deliberately excluded
// from JSON; secrets, bearers, refresh tokens, and enrollment proofs are never
// fields on this record.
type ExternalClientRegistration struct {
	ID               string                    `json:"id"`
	PersonID         string                    `json:"personId"`
	Butler           int64                     `json:"agentId"`
	Connection       int64                     `json:"connectionSeq"`
	Issuer           string                    `json:"issuer"`
	ClientID         string                    `json:"clientId"`
	RuntimePrincipal string                    `json:"runtimePrincipal"`
	Subject          string                    `json:"-"`
	State            ExternalRegistrationState `json:"state"`
	ConfiguredAt     time.Time                 `json:"configuredAt"`
	ConfiguredBy     string                    `json:"configuredBy"`
	ActivatedAt      time.Time                 `json:"activatedAt,omitzero"`
	ActivatedBy      string                    `json:"activatedBy,omitempty"`
	CredentialEpoch  int64                     `json:"credentialEpoch"`
	RevokedAt        time.Time                 `json:"revokedAt,omitzero"`
	RevokedBy        string                    `json:"revokedBy,omitempty"`
}

// ExternalRegistrationConfirmation is the registration-specific consequence
// summary a verified Person confirms. It deliberately excludes the issuer
// subject, enrollment proof, runtime principal, and every browser credential.
type ExternalRegistrationConfirmation struct {
	RegistrationID string `json:"registrationId"`
	PersonID       string `json:"personId"`
	Agent          string `json:"agent"`
	Connection     string `json:"connection"`
	ClientID       string `json:"clientId"`
	Disclosure     string `json:"disclosure"`
	Processing     string `json:"processingClass"`
	Execution      string `json:"executionProfile"`
}

type ActivateExternalRegistration struct {
	RegistrationID string
	PersonID       string
	Issuer         string
	Subject        string
	Proof          string
	ActivatedBy    string
}

type CreatePendingExternalRegistration struct {
	PersonID         string
	Butler           int64
	Connection       int64
	Issuer           string
	ClientID         string
	RuntimePrincipal string
	ConfiguredBy     string
}

// validateExternalRegistrationAuthority re-reads the complete live binding for
// an already persisted registration. Challenge issuance and consumption use
// this instead of treating the pending row as cached authority.
func validateExternalRegistrationAuthority(q queryRower, r ExternalClientRegistration) error {
	var role, surface, agentClass, admits, subject, reach, encryption, retention, trust, runtimeType, health string
	var fallback int
	err := q.QueryRow(`
		SELECT m.role, b.surface, b.agent_class, ca.admits, ca.subject_kind,
		       cd.reach, cd.encryption, cd.retention, cd.surface, cd.accepts_fallback,
		       rt.type, rt.health
		  FROM butler_members m
		  JOIN butler_access ba ON ba.person_id=m.person_id AND ba.butler=? AND ba.revoked_at=''
		  JOIN butlers b ON b.seq=ba.butler AND b.enabled=1 AND b.retired_at=''
		  JOIN butler_addresses a ON a.seq=? AND a.butler=b.seq AND a.revoked_at=''
		  JOIN connection_admission ca ON ca.connection=a.seq
		  JOIN connection_disclosure cd ON cd.connection=a.seq
		  JOIN chat_bindings cb ON cb.connection=a.seq AND cb.person_id=m.person_id AND cb.revoked_at=''
		  JOIN butler_runtimes rt ON rt.butler=b.seq AND rt.principal=? AND rt.health<>'revoked'
		  JOIN runtime_principals rp ON rp.principal=rt.principal AND rp.butler=b.seq
		  JOIN principals p ON p.name=rp.principal AND p.role='butler' AND p.disabled=0
		 WHERE m.person_id=? AND m.admitted=1 AND m.disabled_at='' AND m.offboarded_at=''
		 LIMIT 1`, r.Butler, r.Connection, r.RuntimePrincipal, r.PersonID).
		Scan(&role, &surface, &agentClass, &admits, &subject, &reach, &encryption, &retention, &trust, &fallback, &runtimeType, &health)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: external registration binding is not current and exact", ErrNotAdmitted)
	}
	if err != nil {
		return fmt.Errorf("runtimedb: validating external registration binding: %w", err)
	}
	connection := Connection{Surface: ButlerSurface(surface), AgentClass: agentClass,
		Admission: parseAdmission(admits), AdmissionSubject: parseAdmissionSubject(subject),
		Disclosure: scanDisclosure(reach, encryption, retention, trust, int64(fallback))}
	if ok, why := connection.Admits(role); !ok {
		return fmt.Errorf("%w: connection refuses the bound Person: %s", ErrNotAdmitted, why)
	}
	if connection.Disclosure.Reach == ReachUnresolved || connection.Disclosure.Encryption == EncryptionUnresolved ||
		connection.Disclosure.Retention == RetentionUnresolved || connection.Disclosure.Surface == TrustUnresolved {
		return fmt.Errorf("%w: connection disclosure is unresolved", ErrNotAdmitted)
	}
	if runtimeType == "native" || health == "" {
		return fmt.Errorf("%w: registration requires a live external runtime", ErrButlerUnreachable)
	}

	var others int
	err = q.QueryRow(`
		SELECT COUNT(DISTINCT person_id) FROM (
		  SELECT m.person_id
		    FROM butler_access ba JOIN butler_members m ON m.person_id=ba.person_id
		   WHERE ba.butler=? AND ba.revoked_at='' AND m.admitted=1
		     AND m.disabled_at='' AND m.offboarded_at='' AND m.person_id<>?
		  UNION
		  SELECT m.person_id
		    FROM chat_bindings cb
		    JOIN butler_addresses a ON a.seq=cb.connection AND a.butler=? AND a.revoked_at=''
		    JOIN connection_admission ca ON ca.connection=a.seq AND ca.subject_kind='person'
		    JOIN butlers b ON b.seq=a.butler
		    JOIN butler_members m ON m.person_id=cb.person_id
		   WHERE cb.revoked_at='' AND m.admitted=1 AND m.disabled_at='' AND m.offboarded_at=''
		     AND m.person_id<>?
		     AND ((b.agent_class='owner' AND m.role='owner' AND ca.admits='owners') OR
		          (b.agent_class='member' AND (ca.admits='household' OR (ca.admits='owners' AND m.role='owner'))))
		)`, r.Butler, r.PersonID, r.Butler, r.PersonID).Scan(&others)
	if err != nil {
		return fmt.Errorf("runtimedb: checking dedicated external agent: %w", err)
	}
	if others != 0 {
		return fmt.Errorf("%w: external-profile agent admits another active Person", ErrNoButlerAccess)
	}
	return nil
}

func newExternalRegistrationID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("runtimedb: generating external registration id: %w", err)
	}
	return "xreg_" + hex.EncodeToString(raw), nil
}

// CreatePendingExternalRegistration records inert Owner configuration. All
// authority inputs are checked in the same transaction as the insert so a
// concurrent revoke, retirement, offboarding, or binding change wins before
// the row can be committed.
func (db *DB) CreatePendingExternalRegistration(in CreatePendingExternalRegistration) (ExternalClientRegistration, error) {
	in.PersonID = strings.TrimSpace(in.PersonID)
	in.Issuer = strings.TrimSpace(in.Issuer)
	in.ClientID = strings.TrimSpace(in.ClientID)
	in.RuntimePrincipal = strings.TrimSpace(in.RuntimePrincipal)
	in.ConfiguredBy = strings.TrimSpace(in.ConfiguredBy)
	if in.PersonID == "" || in.Butler <= 0 || in.Connection <= 0 || in.Issuer == "" ||
		in.ClientID == "" || in.RuntimePrincipal == "" || in.ConfiguredBy == "" {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: pending external registration needs person, agent, connection, issuer, client, runtime principal, and configuring actor")
	}
	id, err := newExternalRegistrationID()
	if err != nil {
		return ExternalClientRegistration{}, err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: creating pending external registration: %w", err)
	}
	defer tx.Rollback()
	if err := gateTx(tx, GateDispatch); err != nil {
		return ExternalClientRegistration{}, err
	}

	if err := validateExternalRegistrationAuthority(tx, ExternalClientRegistration{
		PersonID: in.PersonID, Butler: in.Butler, Connection: in.Connection,
		RuntimePrincipal: in.RuntimePrincipal,
	}); err != nil {
		return ExternalClientRegistration{}, err
	}

	stamp := now()
	_, err = tx.Exec(`INSERT INTO external_client_registrations
		(id,person_id,butler,connection,issuer,client_id,runtime_principal,state,configured_at,configured_by)
		VALUES(?,?,?,?,?,?,?,'pending_consent',?,?)`, id, in.PersonID, in.Butler, in.Connection,
		in.Issuer, in.ClientID, in.RuntimePrincipal, stamp, in.ConfiguredBy)
	if err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: creating pending external registration: %w", err)
	}
	if db.registrationTestHook != nil {
		db.registrationTestHook()
	}
	if err := tx.Commit(); err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: committing pending external registration: %w", err)
	}
	configuredAt, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: parsing pending registration timestamp: %w", err)
	}
	return ExternalClientRegistration{ID: id, PersonID: in.PersonID, Butler: in.Butler,
		Connection: in.Connection, Issuer: in.Issuer, ClientID: in.ClientID,
		RuntimePrincipal: in.RuntimePrincipal, State: ExternalRegistrationPendingConsent,
		ConfiguredAt: configuredAt, ConfiguredBy: in.ConfiguredBy}, nil
}

const externalRegistrationColumns = `SELECT id,person_id,butler,connection,issuer,client_id,
	runtime_principal,subject,state,configured_at,configured_by,activated_at,activated_by,
	credential_epoch,revoked_at,revoked_by
	FROM external_client_registrations`

func scanExternalRegistration(row rowScanner) (ExternalClientRegistration, error) {
	var r ExternalClientRegistration
	var state, configured, activated, revoked string
	err := row.Scan(&r.ID, &r.PersonID, &r.Butler, &r.Connection, &r.Issuer, &r.ClientID,
		&r.RuntimePrincipal, &r.Subject, &state, &configured, &r.ConfiguredBy,
		&activated, &r.ActivatedBy, &r.CredentialEpoch, &revoked, &r.RevokedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalClientRegistration{}, ErrNotFound
	}
	if err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: reading external registration: %w", err)
	}
	r.State = ExternalRegistrationState(state)
	if r.ConfiguredAt, err = time.Parse(time.RFC3339Nano, configured); err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: bad registration timestamp %q: %w", configured, err)
	}
	if activated != "" {
		if r.ActivatedAt, err = time.Parse(time.RFC3339Nano, activated); err != nil {
			return ExternalClientRegistration{}, fmt.Errorf("runtimedb: bad registration timestamp %q: %w", activated, err)
		}
	}
	if revoked != "" {
		if r.RevokedAt, err = time.Parse(time.RFC3339Nano, revoked); err != nil {
			return ExternalClientRegistration{}, fmt.Errorf("runtimedb: bad registration timestamp %q: %w", revoked, err)
		}
	}
	return r, nil
}

func (db *DB) ExternalClientRegistration(id string) (ExternalClientRegistration, error) {
	return scanExternalRegistration(db.sql.QueryRow(externalRegistrationColumns+` WHERE id=?`, id))
}

// ExternalRegistrationConfirmation rechecks every live authority input before
// producing the bounded facts shown to the Person. The returned value is safe
// to render; callers bind a digest of it into their short-lived confirmation.
func (db *DB) ExternalRegistrationConfirmation(id, personID string) (ExternalRegistrationConfirmation, error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return ExternalRegistrationConfirmation{}, err
	}
	defer tx.Rollback()
	if err := gateTx(tx, GateDispatch); err != nil {
		return ExternalRegistrationConfirmation{}, err
	}
	r, err := externalRegistrationForChallenge(tx, strings.TrimSpace(id), strings.TrimSpace(personID))
	if err != nil {
		return ExternalRegistrationConfirmation{}, err
	}
	if err := validateExternalRegistrationAuthority(tx, r); err != nil {
		return ExternalRegistrationConfirmation{}, err
	}
	var out ExternalRegistrationConfirmation
	var transport, externalID, reach, encryption, retention, surface string
	err = tx.QueryRow(`
		SELECT b.name,a.transport,a.external_id,cd.reach,cd.encryption,cd.retention,cd.surface,
		       rt.processing_class,rt.execution_profile
		  FROM butlers b
		  JOIN butler_addresses a ON a.seq=? AND a.butler=b.seq AND a.revoked_at=''
		  JOIN connection_disclosure cd ON cd.connection=a.seq
		  JOIN butler_runtimes rt ON rt.butler=b.seq AND rt.principal=?
		 WHERE b.seq=?`, r.Connection, r.RuntimePrincipal, r.Butler).
		Scan(&out.Agent, &transport, &externalID, &reach, &encryption, &retention, &surface,
			&out.Processing, &out.Execution)
	if err != nil {
		return ExternalRegistrationConfirmation{}, fmt.Errorf("runtimedb: reading external registration consequences: %w", err)
	}
	out.RegistrationID, out.PersonID, out.ClientID = r.ID, r.PersonID, r.ClientID
	out.Connection = transport + ":" + externalID
	out.Disclosure = strings.Join([]string{reach, encryption, retention, surface}, "/")
	return out, nil
}

// ActivateExternalClientRegistration owns the complete transition from inert
// configuration to the mutually-exclusive OAuth profile. Every failure rolls
// back subject capture, proof consumption, token retirement, and state.
func (db *DB) ActivateExternalClientRegistration(in ActivateExternalRegistration) (ExternalClientRegistration, error) {
	in.RegistrationID = strings.TrimSpace(in.RegistrationID)
	in.PersonID = strings.TrimSpace(in.PersonID)
	in.Issuer = strings.TrimSpace(in.Issuer)
	in.ActivatedBy = strings.TrimSpace(in.ActivatedBy)
	// Subject is intentionally not trimmed or normalized. A non-empty string
	// is the complete accepted vocabulary and byte equality is its identity.
	if in.RegistrationID == "" || in.PersonID == "" || in.Issuer == "" ||
		in.Subject == "" || in.Proof == "" || in.ActivatedBy == "" {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: activating an external registration needs registration, Person, issuer subject, proof, and actor")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return ExternalClientRegistration{}, err
	}
	defer tx.Rollback()
	if err := gateTx(tx, GateDispatch); err != nil {
		return ExternalClientRegistration{}, err
	}
	r, err := externalRegistrationForChallenge(tx, in.RegistrationID, in.PersonID)
	if err != nil {
		return ExternalClientRegistration{}, err
	}
	if err := validateExternalRegistrationAuthority(tx, r); err != nil {
		return ExternalClientRegistration{}, err
	}
	if r.Issuer != in.Issuer {
		return ExternalClientRegistration{}, ErrNotAdmitted
	}
	proof := ConsumeExternalEnrollmentChallenge{RegistrationID: r.ID, PersonID: r.PersonID,
		Secret: in.Proof, ConsumedBy: in.ActivatedBy}
	if _, err := db.verifyExternalEnrollmentChallengeTx(tx, proof); err != nil {
		return ExternalClientRegistration{}, err
	}
	stamp := db.challengeNow().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE tokens SET revoked_at=? WHERE principal=? AND revoked_at=''`,
		stamp, r.RuntimePrincipal); err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: retiring external runtime tokens: %w", err)
	}
	if _, err := db.consumeExternalEnrollmentChallengeTx(tx, proof); err != nil {
		return ExternalClientRegistration{}, err
	}
	res, err := tx.Exec(`UPDATE external_client_registrations
		SET subject=?,state='active',activated_at=?,activated_by=?,credential_epoch=credential_epoch+1
		WHERE id=? AND state='pending_consent'`, in.Subject, stamp, in.ActivatedBy, r.ID)
	if err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: activating external registration: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ExternalClientRegistration{}, ErrNotAdmitted
	}
	if err := tx.Commit(); err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: committing external registration activation: %w", err)
	}
	return db.ExternalClientRegistration(r.ID)
}

// HasActiveExternalRegistrationForPrincipal is the static-token profile
// fence. The runtime principal remains as the internal Agent binding, but an
// active OAuth registration makes every bearer for it unusable.
func (db *DB) HasActiveExternalRegistrationForPrincipal(principal string) (bool, error) {
	var one int
	err := db.sql.QueryRow(`SELECT 1 FROM external_client_registrations
		WHERE runtime_principal=? AND state='active' LIMIT 1`, principal).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("runtimedb: checking external authentication profile: %w", err)
	}
	return true, nil
}

// RevokeExternalClientRegistration is terminal and idempotent. It retains all
// immutable identity and audit fields; there is deliberately no re-enable.
func (db *DB) RevokeExternalClientRegistration(id, by string) (ExternalClientRegistration, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(by) == "" {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: revoking an external registration needs id and actor")
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return ExternalClientRegistration{}, err
	}
	defer tx.Rollback()
	// Revocation only removes future authority, so it remains available
	// while execution is stopped or a restored database is quarantined.
	r, err := scanExternalRegistration(tx.QueryRow(externalRegistrationColumns+` WHERE id=?`, id))
	if err != nil {
		return ExternalClientRegistration{}, err
	}
	if r.State == ExternalRegistrationRevoked {
		return r, nil
	}
	stamp := now()
	if err := invalidateExternalEnrollmentChallengesTx(tx, id, by, "registration revoked", stamp); err != nil {
		return ExternalClientRegistration{}, err
	}
	if _, err := tx.Exec(`UPDATE external_client_registrations
		SET state='revoked',revoked_at=?,revoked_by=? WHERE id=? AND state IN ('pending_consent','active')`, stamp, by, id); err != nil {
		return ExternalClientRegistration{}, fmt.Errorf("runtimedb: revoking external registration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ExternalClientRegistration{}, err
	}
	return db.ExternalClientRegistration(id)
}
