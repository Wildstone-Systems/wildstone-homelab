package runtimedb

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func externalAdmissionFixture(t *testing.T) (*DB, *SecretVault, ExternalTaskAdmissionInput) {
	t.Helper()
	db, reg := activeExternalRegistrationFixture(t)
	vault, err := db.OpenSecretVault(filepath.Join(t.TempDir(), "household-secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	return db, vault, ExternalTaskAdmissionInput{
		Identity: ExternalTaskAdmissionIdentity{
			RegistrationID: reg.ID, PersonID: reg.PersonID, Butler: reg.Butler,
			Connection: reg.Connection, RuntimePrincipal: reg.RuntimePrincipal,
			CredentialEpoch: reg.CredentialEpoch,
		},
		ClientSessionID: "session-1", IdempotencyKey: "turn-1",
		PayloadDigest: strings.Repeat("a", 64), Objective: "check the greenhouse",
	}
}

func TestExternalTaskAdmissionExactReplayAndCiphertext(t *testing.T) {
	db, vault, in := externalAdmissionFixture(t)
	first, err := vault.AdmitExternalTask(in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := vault.AdmitExternalTask(in)
	if err != nil || second != first {
		t.Fatalf("exact retry = %+v, %v; want %+v", second, err, first)
	}

	var tasks, admissions int
	var sealed, sealedSession []byte
	var sessionHash string
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM butler_tasks`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*),sealed_handle,sealed_mcp_session,mcp_session_hash
		FROM external_task_admissions`).Scan(&admissions, &sealed, &sealedSession, &sessionHash); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || admissions != 1 {
		t.Fatalf("tasks=%d admissions=%d", tasks, admissions)
	}
	if strings.Contains(string(sealed), first.Handle) || strings.Contains(string(sealed), strings.TrimPrefix(first.Handle, first.TaskID+".")) {
		t.Fatal("task handle was persisted in plaintext")
	}
	if first.MCPSession == "" || first.SessionExpiresAt.IsZero() || sessionHash != opaqueDigest(first.MCPSession) ||
		strings.Contains(string(sealedSession), first.MCPSession) {
		t.Fatal("MCP session was absent, unhashed, or persisted in plaintext")
	}
	if err := os.Remove(vault.keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OpenSecretVault(vault.keyPath); err == nil || !strings.Contains(err.Error(), "restore the original key") {
		t.Fatalf("missing key did not close the admission store: %v", err)
	}

	changed := in
	changed.PayloadDigest = strings.Repeat("b", 64)
	if _, err := vault.AdmitExternalTask(changed); !errors.Is(err, ErrExternalAdmissionConflict) {
		t.Fatalf("changed retry = %v", err)
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM butler_tasks`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("conflict created a task: count=%d err=%v", tasks, err)
	}
}

func TestExternalTaskAdmissionPreMCPSessionCiphertextStillOpens(t *testing.T) {
	db, vault, in := externalAdmissionFixture(t)
	admitted, err := vault.AdmitExternalTask(in)
	if err != nil {
		t.Fatal(err)
	}

	// Migration 0112 adds empty MCP session fields to admissions created by
	// older versions. Their sealed handles must retain the original AAD so an
	// upgrade can reopen the store and lazily issue a session on exact replay.
	if _, err := db.sql.Exec(`UPDATE external_task_admissions
		SET mcp_session_hash='',sealed_mcp_session=X'',mcp_session_expires_at=''`); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.OpenSecretVault(vault.keyPath)
	if err != nil {
		t.Fatalf("open store with pre-MCP admission: %v", err)
	}
	replayed, err := reopened.AdmitExternalTask(in)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.TaskID != admitted.TaskID || replayed.Handle != admitted.Handle || replayed.MCPSession == "" {
		t.Fatalf("upgraded replay = %+v; want task and handle from %+v plus a session", replayed, admitted)
	}
}

func TestExternalMCPSessionIsExactBoundAndExpiryBounded(t *testing.T) {
	_, vault, in := externalAdmissionFixture(t)
	admitted, err := vault.AdmitExternalTask(in)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := vault.ResolveExternalMCPSession(in.Identity, admitted.MCPSession,
		admitted.SessionExpiresAt.Add(-time.Nanosecond))
	if err != nil || resolved != admitted {
		t.Fatalf("resolved = %+v, %v; want %+v", resolved, err, admitted)
	}
	wrong := in.Identity
	wrong.PersonID = "person_someone_else"
	if _, err := vault.ResolveExternalMCPSession(wrong, admitted.MCPSession, time.Now()); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("cross-Person session resolution = %v", err)
	}
	if _, err := vault.ResolveExternalMCPSession(in.Identity, "another-session", time.Now()); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("unknown session resolution = %v", err)
	}
	if _, err := vault.ResolveExternalMCPSession(in.Identity, admitted.MCPSession,
		admitted.SessionExpiresAt); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("exact-expiry session resolution = %v", err)
	}
}

func TestExternalTaskAdmissionConcurrentDuplicateHasOneOutcome(t *testing.T) {
	db, vault, in := externalAdmissionFixture(t)
	const callers = 8
	results := make(chan ExternalTaskAdmission, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := vault.AdmitExternalTask(in)
			results <- got
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent admission: %v", err)
		}
	}
	var first ExternalTaskAdmission
	for got := range results {
		if first.TaskID == "" {
			first = got
		} else if got != first {
			t.Fatalf("duplicate outcomes: %+v and %+v", first, got)
		}
	}
	var tasks int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM butler_tasks`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("tasks=%d err=%v", tasks, err)
	}
}

func TestExternalTaskAdmissionRechecksRegistration(t *testing.T) {
	db, vault, in := externalAdmissionFixture(t)
	if _, err := db.sql.Exec(`UPDATE external_client_registrations SET state='revoked',revoked_at=?,revoked_by='root-admin'
		WHERE id=?`, now(), in.Identity.RegistrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.AdmitExternalTask(in); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("revoked registration admitted: %v", err)
	}
	var tasks int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM butler_tasks`).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("revocation left partial task: count=%d err=%v", tasks, err)
	}
}
