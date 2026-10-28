package runtimedb

// OAuth task admission (ADR-W049 §2.3, S07) is deliberately owned by the
// encrypted runtime store. It is the only place that can atomically bind the
// freshly rechecked registration, ordinary Butler task, idempotency tuple and
// recoverable task handle without persisting that handle in plaintext.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrExternalAdmissionConflict = errors.New("runtimedb: external task admission conflicts with an earlier payload")

type ExternalTaskAdmissionIdentity struct {
	RegistrationID   string
	PersonID         string
	Butler           int64
	Connection       int64
	RuntimePrincipal string
	CredentialEpoch  int64
}

type ExternalTaskAdmissionInput struct {
	Identity        ExternalTaskAdmissionIdentity
	ClientSessionID string
	IdempotencyKey  string
	PayloadDigest   string
	Objective       string
}

type ExternalTaskAdmission struct {
	TaskID           string
	Handle           string
	MCPSession       string
	SessionExpiresAt time.Time
}

const ExternalMCPSessionTTL = 30 * time.Minute

// ExternalTaskAdmissionMatches proves that a task handle belongs to the exact
// OAuth registration authenticating this request. It never infers correlation
// from an Agent, Person, session count, or a sole pending task.
func (db *DB) ExternalTaskAdmissionMatches(registrationID, taskID string) (bool, error) {
	seq, err := parseButlerTaskID(taskID)
	if err != nil || registrationID == "" {
		return false, nil
	}
	var ok int
	err = db.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM external_task_admissions
		WHERE registration_id=? AND task=?)`, registrationID, seq).Scan(&ok)
	return ok == 1, err
}

func externalHandleAAD(in ExternalTaskAdmissionInput) []byte {
	// This format predates MCP sessions and is part of the persisted handle
	// ciphertext contract. Changing it would make existing admissions prevent
	// the household store from reopening after an upgrade.
	return []byte("external-task-admission/v1/" + in.Identity.RegistrationID + "/" +
		in.ClientSessionID + "/" + in.IdempotencyKey + "/" + in.PayloadDigest)
}

func externalMCPAdmissionAAD(in ExternalTaskAdmissionInput) []byte {
	return []byte("external-task-admission/v1/mcp-session/" + in.Identity.RegistrationID + "/" +
		in.ClientSessionID + "/" + in.IdempotencyKey + "/" + in.PayloadDigest)
}

func (v *SecretVault) sealExternalValue(aad []byte, value string) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, []byte(value), aad), nil
}

func (v *SecretVault) openExternalValue(aad, sealed []byte) (string, error) {
	n := v.aead.NonceSize()
	if len(sealed) < n {
		return "", ErrSecretDenied
	}
	raw, err := v.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return "", ErrSecretDenied
	}
	defer clear(raw)
	return string(raw), nil
}

func (v *SecretVault) sealExternalHandle(in ExternalTaskAdmissionInput, handle string) ([]byte, error) {
	return v.sealExternalValue(externalHandleAAD(in), handle)
}

func (v *SecretVault) openExternalHandle(in ExternalTaskAdmissionInput, sealed []byte) (string, error) {
	return v.openExternalValue(externalHandleAAD(in), sealed)
}

func (v *SecretVault) checkExternalAdmissionsOpen() error {
	rows, err := v.db.sql.Query(`SELECT registration_id,client_session_id,idempotency_key,payload_digest,
		sealed_handle,sealed_mcp_session,mcp_session_hash
		FROM external_task_admissions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var in ExternalTaskAdmissionInput
		var sealedHandle, sealedSession []byte
		var sessionHash string
		if err := rows.Scan(&in.Identity.RegistrationID, &in.ClientSessionID, &in.IdempotencyKey,
			&in.PayloadDigest, &sealedHandle, &sealedSession, &sessionHash); err != nil {
			return err
		}
		handle, err := v.openExternalHandle(in, sealedHandle)
		if err != nil || externalHandleDigest(handle) == "" {
			return errors.New("household secret key or external task admission ciphertext invalid; refusing to open store")
		}
		if sessionHash != "" {
			session, err := v.openExternalValue(externalMCPAdmissionAAD(in), sealedSession)
			if err != nil || opaqueDigest(session) != sessionHash {
				return errors.New("household secret key or external MCP session ciphertext invalid; refusing to open store")
			}
		}
	}
	return rows.Err()
}

func externalHandleDigest(handle string) string {
	_, secret, ok := strings.Cut(handle, ".")
	if !ok || secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func opaqueDigest(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// AdmitExternalTask creates or exactly replays one OAuth task admission. The
// active registration and every local Person/Agent/Connection/runtime fence
// are checked under the same SQLite writer transaction as the task insert.
func (v *SecretVault) AdmitExternalTask(in ExternalTaskAdmissionInput) (ExternalTaskAdmission, error) {
	if v == nil || v.db == nil {
		return ExternalTaskAdmission{}, errors.New("runtimedb: encrypted task admission is unavailable")
	}
	if in.Identity.RegistrationID == "" || in.Identity.PersonID == "" || in.Identity.Butler <= 0 ||
		in.Identity.Connection <= 0 || in.Identity.RuntimePrincipal == "" || in.Identity.CredentialEpoch <= 0 ||
		in.ClientSessionID == "" || in.IdempotencyKey == "" || len(in.PayloadDigest) != sha256.Size*2 ||
		in.Objective == "" {
		return ExternalTaskAdmission{}, errors.New("runtimedb: incomplete external task admission")
	}

	tx, err := v.db.sql.Begin()
	if err != nil {
		return ExternalTaskAdmission{}, err
	}
	defer tx.Rollback()
	if err := gateTx(tx, GateDispatch); err != nil {
		return ExternalTaskAdmission{}, err
	}

	reg, err := scanExternalRegistration(tx.QueryRow(externalRegistrationColumns+` WHERE id=? AND state='active' AND credential_epoch=?`,
		in.Identity.RegistrationID, in.Identity.CredentialEpoch))
	if errors.Is(err, ErrNotFound) {
		return ExternalTaskAdmission{}, ErrNotAdmitted
	}
	if err != nil {
		return ExternalTaskAdmission{}, err
	}
	if reg.PersonID != in.Identity.PersonID || reg.Butler != in.Identity.Butler ||
		reg.Connection != in.Identity.Connection || reg.RuntimePrincipal != in.Identity.RuntimePrincipal {
		return ExternalTaskAdmission{}, ErrNotAdmitted
	}
	if err := validateExternalRegistrationAuthority(tx, reg); err != nil {
		return ExternalTaskAdmission{}, err
	}

	var priorDigest, taskID, handleHash, sessionHash, sessionExpiry string
	var sealed, sealedSession []byte
	err = tx.QueryRow(`SELECT a.payload_digest,'bt-'||a.task,a.sealed_handle,t.handle_hash,
		a.mcp_session_hash,a.sealed_mcp_session,a.mcp_session_expires_at
		FROM external_task_admissions a JOIN butler_tasks t ON t.seq=a.task
		WHERE a.registration_id=? AND a.client_session_id=? AND a.idempotency_key=?`,
		in.Identity.RegistrationID, in.ClientSessionID, in.IdempotencyKey).
		Scan(&priorDigest, &taskID, &sealed, &handleHash, &sessionHash, &sealedSession, &sessionExpiry)
	if err == nil {
		if subtle.ConstantTimeCompare([]byte(priorDigest), []byte(in.PayloadDigest)) != 1 {
			return ExternalTaskAdmission{}, ErrExternalAdmissionConflict
		}
		handle, err := v.openExternalHandle(in, sealed)
		if err != nil || externalHandleDigest(handle) == "" ||
			subtle.ConstantTimeCompare([]byte(externalHandleDigest(handle)), []byte(handleHash)) != 1 {
			return ExternalTaskAdmission{}, ErrSecretDenied
		}
		if sessionHash == "" {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return ExternalTaskAdmission{}, err
			}
			session := base64.RawURLEncoding.EncodeToString(raw)
			clear(raw)
			sessionHash = opaqueDigest(session)
			sealedSession, err = v.sealExternalValue(externalMCPAdmissionAAD(in), session)
			if err != nil {
				return ExternalTaskAdmission{}, err
			}
			expiresAt := time.Now().UTC().Add(ExternalMCPSessionTTL)
			if _, err := tx.Exec(`UPDATE external_task_admissions
				SET mcp_session_hash=?,sealed_mcp_session=?,mcp_session_expires_at=?
				WHERE registration_id=? AND client_session_id=? AND idempotency_key=?`,
				sessionHash, sealedSession, expiresAt.Format(time.RFC3339Nano), in.Identity.RegistrationID,
				in.ClientSessionID, in.IdempotencyKey); err != nil {
				return ExternalTaskAdmission{}, err
			}
			if err := tx.Commit(); err != nil {
				return ExternalTaskAdmission{}, err
			}
			return ExternalTaskAdmission{TaskID: taskID, Handle: handle, MCPSession: session, SessionExpiresAt: expiresAt}, nil
		}
		session, err := v.openExternalValue(externalMCPAdmissionAAD(in), sealedSession)
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, sessionExpiry)
		if err != nil || parseErr != nil || opaqueDigest(session) == "" ||
			subtle.ConstantTimeCompare([]byte(opaqueDigest(session)), []byte(sessionHash)) != 1 {
			return ExternalTaskAdmission{}, ErrSecretDenied
		}
		return ExternalTaskAdmission{TaskID: taskID, Handle: handle, MCPSession: session, SessionExpiresAt: expiresAt}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ExternalTaskAdmission{}, err
	}

	var member, role, butlerName, surface, agentClass, connectionName string
	err = tx.QueryRow(`SELECT m.member,m.role,b.name,b.surface,b.agent_class,a.transport||':'||a.external_id
		FROM butler_members m
		JOIN butlers b ON b.seq=? AND b.enabled=1 AND b.retired_at=''
		JOIN butler_addresses a ON a.seq=? AND a.butler=b.seq AND a.revoked_at=''
		WHERE m.person_id=? AND m.admitted=1 AND m.disabled_at='' AND m.offboarded_at=''`,
		reg.Butler, reg.Connection, reg.PersonID).
		Scan(&member, &role, &butlerName, &surface, &agentClass, &connectionName)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalTaskAdmission{}, ErrNotAdmitted
	}
	if err != nil {
		return ExternalTaskAdmission{}, err
	}

	stamp := now()
	res, err := tx.Exec(`INSERT INTO butler_tasks
		(created_at,updated_at,kind,state,author_person_id,owner_person_id,surface,agent_class,audience,
		 origin_connection,origin_connection_ref,correlation_id,objective,completion_criteria,grants_json,budget_json,
		 deadline_at,next_wakeup_at,lease_epoch,uncertainty,result_summary,retention,envelope_version,objective_kind,
		 proposal_only,as_owner,system_extra,origin_conversation,butler)
		VALUES(?,?,'conversation','pending',?,?,?,?,?,?,?,?,?,?,'{}','{}','','',0,'','','conversation',?,
		 'effectful',0,?,'',?,?)`, stamp, stamp, reg.PersonID, reg.PersonID, surface, agentClass,
		string(ButlerAudiencePrivate), connectionName, reg.Connection, in.ClientSessionID, in.Objective, "", TaskEnvelopeVersion,
		role == string(RoleOwner), in.ClientSessionID, butlerName)
	if err != nil {
		return ExternalTaskAdmission{}, fmt.Errorf("runtimedb: admitting external task: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return ExternalTaskAdmission{}, err
	}
	created, err := tx.Exec(`INSERT INTO conversation_work(session,owner_person_id,tool_calls,created_at,updated_at)
		VALUES(NULL,?,0,?,?)`, reg.PersonID, stamp, stamp)
	if err != nil {
		return ExternalTaskAdmission{}, err
	}
	work, err := created.LastInsertId()
	if err != nil {
		return ExternalTaskAdmission{}, err
	}
	if _, err := tx.Exec(`UPDATE butler_tasks SET conversation_work=? WHERE seq=?`, work, seq); err != nil {
		return ExternalTaskAdmission{}, err
	}
	if _, err := tx.Exec(`INSERT INTO butler_task_transitions
		(task,at,from_state,to_state,reason,actor) VALUES(?,?,'','pending','created',?)`, seq, stamp, member); err != nil {
		return ExternalTaskAdmission{}, err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ExternalTaskAdmission{}, err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	clear(raw)
	taskID = butlerTaskID(seq)
	handle := taskID + "." + secret
	handleHash = externalHandleDigest(handle)
	if _, err := tx.Exec(`UPDATE butler_tasks SET handle_hash=? WHERE seq=?`, handleHash, seq); err != nil {
		return ExternalTaskAdmission{}, err
	}
	sealed, err = v.sealExternalHandle(in, handle)
	if err != nil {
		return ExternalTaskAdmission{}, err
	}
	sessionRaw := make([]byte, 32)
	if _, err := rand.Read(sessionRaw); err != nil {
		return ExternalTaskAdmission{}, err
	}
	session := base64.RawURLEncoding.EncodeToString(sessionRaw)
	clear(sessionRaw)
	sessionHash = opaqueDigest(session)
	sealedSession, err = v.sealExternalValue(externalMCPAdmissionAAD(in), session)
	if err != nil {
		return ExternalTaskAdmission{}, err
	}
	expiresAt := time.Now().UTC().Add(ExternalMCPSessionTTL)
	if _, err := tx.Exec(`INSERT INTO external_task_admissions
		(registration_id,client_session_id,idempotency_key,payload_digest,task,sealed_handle,created_at,
		 mcp_session_hash,sealed_mcp_session,mcp_session_expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, reg.ID, in.ClientSessionID, in.IdempotencyKey, in.PayloadDigest, seq, sealed, stamp,
		sessionHash, sealedSession, expiresAt.Format(time.RFC3339Nano)); err != nil {
		return ExternalTaskAdmission{}, fmt.Errorf("runtimedb: recording external task admission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ExternalTaskAdmission{}, err
	}
	return ExternalTaskAdmission{TaskID: taskID, Handle: handle, MCPSession: session, SessionExpiresAt: expiresAt}, nil
}

// ResolveExternalMCPSession recovers the exact task handle only for the
// currently authenticated registration tuple and a still-live task session.
// Registration authority is rechecked in the same read transaction; the
// caller must still re-introspect its OAuth bearer immediately beforehand.
func (v *SecretVault) ResolveExternalMCPSession(identity ExternalTaskAdmissionIdentity, session string, at time.Time) (ExternalTaskAdmission, error) {
	if v == nil || v.db == nil || session == "" || identity.RegistrationID == "" {
		return ExternalTaskAdmission{}, ErrNotAdmitted
	}
	tx, err := v.db.sql.Begin()
	if err != nil {
		return ExternalTaskAdmission{}, err
	}
	defer tx.Rollback()
	reg, err := scanExternalRegistration(tx.QueryRow(externalRegistrationColumns+` WHERE id=? AND state='active' AND credential_epoch=?`,
		identity.RegistrationID, identity.CredentialEpoch))
	if err != nil || reg.PersonID != identity.PersonID || reg.Butler != identity.Butler ||
		reg.Connection != identity.Connection || reg.RuntimePrincipal != identity.RuntimePrincipal {
		return ExternalTaskAdmission{}, ErrNotAdmitted
	}
	if err := validateExternalRegistrationAuthority(tx, reg); err != nil {
		return ExternalTaskAdmission{}, err
	}
	var in ExternalTaskAdmissionInput
	var task int64
	var sealedHandle, sealedSession []byte
	var storedHash, expires, handleHash string
	err = tx.QueryRow(`SELECT client_session_id,idempotency_key,payload_digest,task,sealed_handle,
		a.sealed_mcp_session,a.mcp_session_hash,a.mcp_session_expires_at,t.handle_hash
		FROM external_task_admissions a JOIN butler_tasks t ON t.seq=a.task
		WHERE a.registration_id=? AND a.mcp_session_hash=?`,
		identity.RegistrationID, opaqueDigest(session)).Scan(&in.ClientSessionID, &in.IdempotencyKey,
		&in.PayloadDigest, &task, &sealedHandle, &sealedSession, &storedHash, &expires, &handleHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalTaskAdmission{}, ErrNotAdmitted
	}
	if err != nil {
		return ExternalTaskAdmission{}, err
	}
	in.Identity = identity
	expiresAt, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !at.Before(expiresAt) || subtle.ConstantTimeCompare([]byte(opaqueDigest(session)), []byte(storedHash)) != 1 {
		return ExternalTaskAdmission{}, ErrNotAdmitted
	}
	storedSession, err := v.openExternalValue(externalMCPAdmissionAAD(in), sealedSession)
	if err != nil || subtle.ConstantTimeCompare([]byte(storedSession), []byte(session)) != 1 {
		return ExternalTaskAdmission{}, ErrNotAdmitted
	}
	handle, err := v.openExternalHandle(in, sealedHandle)
	agentTaskID := butlerTaskID(task)
	if err != nil || !strings.HasPrefix(handle, agentTaskID+".") ||
		subtle.ConstantTimeCompare([]byte(externalHandleDigest(handle)), []byte(handleHash)) != 1 {
		return ExternalTaskAdmission{}, ErrSecretDenied
	}
	return ExternalTaskAdmission{TaskID: agentTaskID, Handle: handle, MCPSession: session, SessionExpiresAt: expiresAt}, nil
}
