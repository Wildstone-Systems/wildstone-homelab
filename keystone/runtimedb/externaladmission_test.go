package runtimedb

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
	var sealed []byte
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM butler_tasks`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*),sealed_handle FROM external_task_admissions`).Scan(&admissions, &sealed); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || admissions != 1 {
		t.Fatalf("tasks=%d admissions=%d", tasks, admissions)
	}
	if strings.Contains(string(sealed), first.Handle) || strings.Contains(string(sealed), strings.TrimPrefix(first.Handle, first.TaskID+".")) {
		t.Fatal("task handle was persisted in plaintext")
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
