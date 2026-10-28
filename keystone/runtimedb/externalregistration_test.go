package runtimedb

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
)

func externalRegistrationFixture(t *testing.T) (*DB, CreatePendingExternalRegistration) {
	t.Helper()
	db := butlerFixture(t)
	agent, err := db.CreateButler(Butler{Name: "sam-external", Surface: ButlerSurfaceFamily,
		AgentClass: AgentClassMember, Model: "kitchen-box"}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GrantButlerAccess("sam-external", "sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	connection, err := db.AddButlerAddress("sam-external", "matrix", "sam-agent", "direct", "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	sam, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO chat_bindings
		(connection,account,person_id,display_name,created_at,created_by)
		VALUES(?,?,?,?,?,?)`, connection, "sam-account", sam.PersonID, "Sam", now(), "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerRuntime(agent, ButlerRuntime{Type: agentapi.RuntimeHermes,
		Delivery: agentapi.DeliveryPull, ProcessingClass: agentapi.ProcessingCloud}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	principal := "runtime-sam-external-1"
	if err := db.CreatePrincipal(principal, RoleButler, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRuntimePrincipal(principal, agent); err != nil {
		t.Fatal(err)
	}
	return db, CreatePendingExternalRegistration{PersonID: sam.PersonID, Butler: agent,
		Connection: connection, Issuer: "https://issuer.invalid", ClientID: "hermes-client",
		RuntimePrincipal: principal, ConfiguredBy: "root-admin"}
}

func activeExternalRegistrationFixture(t *testing.T) (*DB, ExternalClientRegistration) {
	t.Helper()
	db, in := externalRegistrationFixture(t)
	r, err := db.CreatePendingExternalRegistration(in)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := db.IssueExternalEnrollmentChallenge(IssueExternalEnrollmentChallenge{
		RegistrationID: r.ID, PersonID: r.PersonID, IssuedBy: r.PersonID,
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err = db.ActivateExternalClientRegistration(ActivateExternalRegistration{
		RegistrationID: r.ID, PersonID: r.PersonID, Issuer: r.Issuer,
		Subject: "byte-exact-subject", Proof: proof.Secret, ActivatedBy: r.PersonID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, r
}

func TestResolveExternalRegistrationUsesExactIdentityAndFreshAuthority(t *testing.T) {
	t.Run("exact active identity", func(t *testing.T) {
		db, r := activeExternalRegistrationFixture(t)
		got, err := db.ResolveExternalClientRegistration(r.Issuer, r.Subject, r.ClientID, r.CredentialEpoch)
		if err != nil || got.ID != r.ID || got.PersonID != r.PersonID || got.RuntimePrincipal != r.RuntimePrincipal {
			t.Fatalf("resolve = %+v, %v", got, err)
		}
		for _, tc := range []struct {
			issuer, subject, client string
			epoch                   int64
		}{
			{r.Issuer + "/", r.Subject, r.ClientID, r.CredentialEpoch},
			{r.Issuer, r.Subject + " ", r.ClientID, r.CredentialEpoch},
			{r.Issuer, r.Subject, r.ClientID + "-other", r.CredentialEpoch},
			{r.Issuer, r.Subject, r.ClientID, r.CredentialEpoch + 1},
		} {
			if _, err := db.ResolveExternalClientRegistration(tc.issuer, tc.subject, tc.client, tc.epoch); err == nil {
				t.Fatalf("inexact identity resolved: %+v", tc)
			}
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *DB, ExternalClientRegistration)
	}{
		{"Person offboarded", func(t *testing.T, db *DB, r ExternalClientRegistration) {
			_, err := db.sql.Exec(`UPDATE butler_members SET admitted=0,offboarded_at=?,offboarded_by='root-admin' WHERE person_id=?`, now(), r.PersonID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"Agent access revoked", func(t *testing.T, db *DB, r ExternalClientRegistration) {
			_, err := db.sql.Exec(`UPDATE butler_access SET revoked_at=?,revoked_by='root-admin' WHERE butler=? AND person_id=?`, now(), r.Butler, r.PersonID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"Connection revoked", func(t *testing.T, db *DB, r ExternalClientRegistration) {
			_, err := db.sql.Exec(`UPDATE butler_addresses SET revoked_at=?,revoked_by='root-admin' WHERE seq=?`, now(), r.Connection)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"runtime principal disabled", func(t *testing.T, db *DB, r ExternalClientRegistration) {
			if err := db.SetPrincipalDisabled(r.RuntimePrincipal, true); err != nil {
				t.Fatal(err)
			}
		}},
		{"registration revoked", func(t *testing.T, db *DB, r ExternalClientRegistration) {
			if _, err := db.RevokeExternalClientRegistration(r.ID, "root-admin"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, r := activeExternalRegistrationFixture(t)
			tc.mutate(t, db, r)
			if _, err := db.ResolveExternalClientRegistration(r.Issuer, r.Subject, r.ClientID, r.CredentialEpoch); err == nil {
				t.Fatal("stale authority resolved")
			}
		})
	}

	for _, tc := range []struct {
		name  string
		fence func(*DB) error
	}{
		{"execution stop", func(db *DB) error { return db.StopHouseholdExecution("mika", "test") }},
		{"restore quarantine", func(db *DB) error { return db.EnterRestoreQuarantine("test", time.Now()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, r := activeExternalRegistrationFixture(t)
			if err := tc.fence(db); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ResolveExternalClientRegistration(r.Issuer, r.Subject, r.ClientID, r.CredentialEpoch); err == nil {
				t.Fatal("execution fence did not deny resolution")
			}
		})
	}
}

func TestPendingExternalRegistrationIsInertImmutableHistory(t *testing.T) {
	db, in := externalRegistrationFixture(t)
	r, err := db.CreatePendingExternalRegistration(in)
	if err != nil {
		t.Fatalf("CreatePendingExternalRegistration: %v", err)
	}
	if r.ID == "" || r.State != ExternalRegistrationPendingConsent || r.PersonID != in.PersonID ||
		r.Butler != in.Butler || r.Connection != in.Connection || r.ConfiguredAt.IsZero() {
		t.Fatalf("registration = %+v", r)
	}
	if _, err := db.sql.Exec(`UPDATE butler_members SET member='sam-renamed' WHERE person_id=?`, in.PersonID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE butlers SET name='sam-external-renamed' WHERE seq=?`, in.Butler); err != nil {
		t.Fatal(err)
	}
	got, err := db.ExternalClientRegistration(r.ID)
	if err != nil || got.PersonID != in.PersonID || got.Butler != in.Butler || got.Connection != in.Connection {
		t.Fatalf("stable read after renames = %+v, %v", got, err)
	}
	mika, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		query string
		value any
	}{
		{"canonical Person", `UPDATE external_client_registrations SET person_id=? WHERE id=?`, mika.PersonID},
		{"issuer", `UPDATE external_client_registrations SET issuer=? WHERE id=?`, "https://other-issuer.invalid"},
		{"client ID", `UPDATE external_client_registrations SET client_id=? WHERE id=?`, "another-valid-client"},
	} {
		t.Run("immutable "+tc.name, func(t *testing.T) {
			_, err := db.sql.Exec(tc.query, tc.value, r.ID)
			if err == nil || !strings.Contains(err.Error(), "external registration identity is immutable") {
				t.Fatalf("update error = %v, want the identity-immutability trigger", err)
			}
			got, err := db.ExternalClientRegistration(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != r.ID || got.PersonID != in.PersonID || got.Butler != in.Butler ||
				got.Connection != in.Connection || got.Issuer != in.Issuer ||
				got.ClientID != in.ClientID || got.RuntimePrincipal != in.RuntimePrincipal {
				t.Fatalf("refused identity update changed registration: %+v", got)
			}
		})
	}
	revoked, err := db.RevokeExternalClientRegistration(r.ID, "root-admin")
	if err != nil || revoked.State != ExternalRegistrationRevoked || revoked.RevokedAt.IsZero() {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	again, err := db.RevokeExternalClientRegistration(r.ID, "another-owner")
	if err != nil || again.RevokedBy != "root-admin" || !again.RevokedAt.Equal(revoked.RevokedAt) {
		t.Fatalf("idempotent revoke changed history: %+v, %v", again, err)
	}
	if _, err := db.sql.Exec(`UPDATE external_client_registrations
		SET state='pending_consent',revoked_at='',revoked_by='' WHERE id=?`, r.ID); err == nil {
		t.Fatal("terminal revocation was reversible")
	}
}

func TestConcurrentExternalRegistrationRevokeIsTerminalAndIdempotent(t *testing.T) {
	db, in := externalRegistrationFixture(t)
	r, err := db.CreatePendingExternalRegistration(in)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, actor := range []string{"owner-one", "owner-two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.RevokeExternalClientRegistration(r.ID, actor)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent revoke: %v", err)
		}
	}
	got, err := db.ExternalClientRegistration(r.ID)
	if err != nil || got.State != ExternalRegistrationRevoked || got.RevokedBy == "" || got.RevokedAt.IsZero() {
		t.Fatalf("terminal registration = %+v, %v", got, err)
	}
}

func TestExternalRegistrationRevocationRemainsAvailableUnderExecutionFences(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fence func(*DB) error
	}{
		{"household execution stop", func(db *DB) error {
			return db.StopHouseholdExecution("mika", "test stop")
		}},
		{"restore quarantine", func(db *DB) error {
			return db.EnterRestoreQuarantine("test restore", time.Now())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, in := externalRegistrationFixture(t)
			r, err := db.CreatePendingExternalRegistration(in)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.fence(db); err != nil {
				t.Fatal(err)
			}
			revoked, err := db.RevokeExternalClientRegistration(r.ID, "root-admin")
			if err != nil || revoked.State != ExternalRegistrationRevoked || revoked.RevokedAt.IsZero() {
				t.Fatalf("revoke under fence = %+v, %v", revoked, err)
			}
		})
	}
}

func TestPendingExternalRegistrationRequiresExactDedicatedLiveBinding(t *testing.T) {
	t.Run("another admitted person", func(t *testing.T) {
		db, in := externalRegistrationFixture(t)
		if err := db.GrantButlerAccess("sam-external", "mika", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreatePendingExternalRegistration(in); !errors.Is(err, ErrNoButlerAccess) {
			t.Fatalf("create with second Person = %v, want ErrNoButlerAccess", err)
		}
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM external_client_registrations`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("refused create wrote %d rows: %v", n, err)
		}
	})

	for _, tc := range []struct {
		name         string
		breakBinding func(*testing.T, *DB, *CreatePendingExternalRegistration)
	}{
		{"offboarded Person", func(t *testing.T, db *DB, in *CreatePendingExternalRegistration) {
			if _, err := db.sql.Exec(`UPDATE butler_members SET offboarded_at=?,offboarded_by='root-admin',admitted=0 WHERE person_id=?`, now(), in.PersonID); err != nil {
				t.Fatal(err)
			}
		}},
		{"inaccessible Person", func(t *testing.T, db *DB, in *CreatePendingExternalRegistration) {
			if _, err := db.sql.Exec(`UPDATE butler_access SET revoked_at=?,revoked_by='root-admin'
				WHERE butler=? AND person_id=? AND revoked_at=''`, now(), in.Butler, in.PersonID); err != nil {
				t.Fatal(err)
			}
		}},
		{"cross-Person binding", func(t *testing.T, db *DB, in *CreatePendingExternalRegistration) {
			other, err := db.GetButlerMember("mika")
			if err != nil {
				t.Fatal(err)
			}
			in.PersonID = other.PersonID
		}},
		{"disabled Agent", func(t *testing.T, db *DB, in *CreatePendingExternalRegistration) {
			if _, err := db.sql.Exec(`UPDATE butlers SET enabled=0 WHERE seq=?`, in.Butler); err != nil {
				t.Fatal(err)
			}
		}},
		{"retired Agent", func(t *testing.T, db *DB, in *CreatePendingExternalRegistration) {
			if _, err := db.sql.Exec(`UPDATE butlers SET retired_at=?,retired_by='root-admin' WHERE seq=?`, now(), in.Butler); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong Connection", func(_ *testing.T, _ *DB, in *CreatePendingExternalRegistration) { in.Connection++ }},
		{"cross-Agent Connection", func(t *testing.T, db *DB, in *CreatePendingExternalRegistration) {
			other, err := db.CreateButler(Butler{Name: "other-external", Surface: ButlerSurfaceFamily,
				AgentClass: AgentClassMember, Model: "kitchen-box"}, "root-admin")
			if err != nil {
				t.Fatal(err)
			}
			connection, err := db.AddButlerAddress("other-external", "matrix", "other-agent", "direct", "root-admin")
			if err != nil {
				t.Fatal(err)
			}
			if other == in.Butler {
				t.Fatal("fixture did not create a second Agent")
			}
			in.Connection = connection
		}},
		{"disabled principal", func(t *testing.T, db *DB, in *CreatePendingExternalRegistration) {
			if err := db.SetPrincipalDisabled(in.RuntimePrincipal, true); err != nil {
				t.Fatal(err)
			}
		}},
		{"superseded principal", func(t *testing.T, db *DB, in *CreatePendingExternalRegistration) {
			if err := db.CreatePrincipal("runtime-successor", RoleButler, ""); err != nil {
				t.Fatal(err)
			}
			if err := db.BindRuntimePrincipal("runtime-successor", in.Butler); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, in := externalRegistrationFixture(t)
			tc.breakBinding(t, db, &in)
			if _, err := db.CreatePendingExternalRegistration(in); err == nil {
				t.Fatal("invalid binding was accepted")
			}
		})
	}
}

func TestExternalRegistrationMigrationCreatesNoRows(t *testing.T) {
	db := open(t)
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM external_client_registrations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("fresh migration synthesized %d registrations: %v", n, err)
	}
}

// sqlExecer is the common surface shared by *sql.Tx and *sql.DB for the
// authority mutations exercised around registration creation.
type externalRegistrationExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func mutateExternalRegistrationAuthority(exec externalRegistrationExecer, in CreatePendingExternalRegistration, name string) error {
	switch name {
	case "Person offboarding":
		_, err := exec.Exec(`UPDATE butler_members
			SET offboarded_at=?, offboarded_by='root-admin', admitted=0 WHERE person_id=?`, now(), in.PersonID)
		return err
	case "runtime principal disablement":
		_, err := exec.Exec(`UPDATE principals SET disabled=1 WHERE name=?`, in.RuntimePrincipal)
		return err
	case "Agent access withdrawal":
		_, err := exec.Exec(`UPDATE butler_access SET revoked_at=?, revoked_by='root-admin'
			WHERE butler=? AND person_id=? AND revoked_at=''`, now(), in.Butler, in.PersonID)
		return err
	default:
		return errors.New("unknown authority mutation")
	}
}

func assertNoExternalRegistrations(t *testing.T, db *DB) {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM external_client_registrations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused create wrote %d registrations: %v", n, err)
	}
}

func TestExternalRegistrationCreationSerializesAfterAuthorityWithdrawal(t *testing.T) {
	for _, name := range []string{"Person offboarding", "runtime principal disablement", "Agent access withdrawal"} {
		t.Run(name, func(t *testing.T) {
			db, in := externalRegistrationFixture(t)
			withdrawal, err := db.sql.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if err := mutateExternalRegistrationAuthority(withdrawal, in, name); err != nil {
				_ = withdrawal.Rollback()
				t.Fatal(err)
			}

			// Hold SQLite's immediate write transaction while creation is
			// launched. Creation cannot validate a stale snapshot; after this
			// authority withdrawal commits, its own transaction must refuse.
			started := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				close(started)
				_, err := db.CreatePendingExternalRegistration(in)
				result <- err
			}()
			<-started
			if err := withdrawal.Commit(); err != nil {
				t.Fatal(err)
			}
			err = <-result
			if !errors.Is(err, ErrNotAdmitted) {
				t.Fatalf("create after %s = %v, want ErrNotAdmitted", name, err)
			}
			assertNoExternalRegistrations(t, db)
		})
	}
}

func TestExternalRegistrationCreatedBeforeAuthorityWithdrawalRemainsInert(t *testing.T) {
	for _, name := range []string{"Person offboarding", "runtime principal disablement", "Agent access withdrawal"} {
		t.Run(name, func(t *testing.T) {
			db, in := externalRegistrationFixture(t)
			validated := make(chan struct{})
			allowCommit := make(chan struct{})
			db.registrationTestHook = func() {
				close(validated)
				<-allowCommit
			}
			type createResult struct {
				registration ExternalClientRegistration
				err          error
			}
			created := make(chan createResult, 1)
			go func() {
				r, err := db.CreatePendingExternalRegistration(in)
				created <- createResult{registration: r, err: err}
			}()
			<-validated // the creation transaction holds the immediate write lock

			withdrawn := make(chan error, 1)
			mutationStarted := make(chan struct{})
			go func() {
				close(mutationStarted)
				withdrawn <- mutateExternalRegistrationAuthority(db.sql, in, name)
			}()
			<-mutationStarted // withdrawal is queued behind the uncommitted create
			close(allowCommit)

			result := <-created
			db.registrationTestHook = nil
			if result.err != nil {
				t.Fatalf("valid creation should commit first: %v", result.err)
			}
			if err := <-withdrawn; err != nil {
				t.Fatalf("subsequent %s: %v", name, err)
			}
			got, err := db.ExternalClientRegistration(result.registration.ID)
			if err != nil || got.State != ExternalRegistrationPendingConsent ||
				got.PersonID != in.PersonID || got.Butler != in.Butler ||
				got.Connection != in.Connection || got.RuntimePrincipal != in.RuntimePrincipal {
				t.Fatalf("inert record after subsequent %s = %+v, %v", name, got, err)
			}
		})
	}
}

func TestExternalRegistrationMigrationPreservesPopulatedPre0105Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-0105.db")
	db, err := openThrough(t, path, baselineMigration)
	if err != nil {
		t.Fatalf("open through %s: %v", baselineMigration, err)
	}

	for _, member := range []struct{ handle, role string }{
		{"mika", RoleOwner}, {"sam", RoleMember},
	} {
		if err := db.RecordButlerMember(member.handle, member.role, "", ""); err != nil {
			t.Fatalf("record %s: %v", member.handle, err)
		}
		if err := db.AdmitButlerMember(member.handle, "root-admin"); err != nil {
			t.Fatalf("admit %s: %v", member.handle, err)
		}
	}
	if err := db.UpsertModel(Model{Name: "migration-fixture-model", Kind: ModelLocal,
		Provider: "ollama", ProviderModel: "test-model", Readiness: ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	agent, err := db.CreateButler(Butler{Name: "migration-fixture-agent", Surface: ButlerSurfaceFamily,
		AgentClass: AgentClassMember, Model: "migration-fixture-model"}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GrantButlerAccess("migration-fixture-agent", "sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	connection, err := db.AddButlerAddress("migration-fixture-agent", "matrix", "migration-fixture-bot", "direct", "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	person, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO chat_bindings
		(connection,account,person_id,display_name,created_at,created_by)
		VALUES(?,?,?,?,?,?)`, connection, "migration-fixture-account", person.PersonID, "Sam", now(), "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerRuntime(agent, ButlerRuntime{Type: agentapi.RuntimeHermes,
		Delivery: agentapi.DeliveryPull, ProcessingClass: agentapi.ProcessingCloud}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	principal := "migration-fixture-runtime"
	if err := db.CreatePrincipal(principal, RoleButler, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRuntimePrincipal(principal, agent); err != nil {
		t.Fatal(err)
	}
	const tokenHash = "synthetic-migration-fixture-token-hash"
	if err := db.InsertToken(tokenHash, principal, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendAudit(AuditRecord{Principal: principal, Method: "GET",
		Path: "/synthetic-migration-fixture", Status: 200}); err != nil {
		t.Fatal(err)
	}

	verifyPreserved := func(stage string, db *DB, migrated bool) {
		t.Helper()
		var gotPerson, gotAgent, gotConnection, gotPrincipal string
		var gotRuntimeAgent int64
		if err := db.sql.QueryRow(`SELECT person_id FROM butler_members WHERE person_id=?`, person.PersonID).Scan(&gotPerson); err != nil {
			t.Fatalf("%s: Person: %v", stage, err)
		}
		if err := db.sql.QueryRow(`SELECT name FROM butlers WHERE seq=?`, agent).Scan(&gotAgent); err != nil {
			t.Fatalf("%s: Agent: %v", stage, err)
		}
		if err := db.sql.QueryRow(`SELECT account FROM chat_bindings WHERE connection=? AND person_id=? AND revoked_at=''`,
			connection, person.PersonID).Scan(&gotConnection); err != nil {
			t.Fatalf("%s: chat binding: %v", stage, err)
		}
		if err := db.sql.QueryRow(`SELECT principal FROM runtime_principals WHERE principal=? AND butler=?`,
			principal, agent).Scan(&gotPrincipal); err != nil {
			t.Fatalf("%s: runtime principal binding: %v", stage, err)
		}
		if err := db.sql.QueryRow(`SELECT butler FROM runtime_principals WHERE principal=?`, principal).Scan(&gotRuntimeAgent); err != nil {
			t.Fatalf("%s: runtime relationship: %v", stage, err)
		}
		if gotPerson != person.PersonID || gotAgent != "migration-fixture-agent" ||
			gotConnection != "migration-fixture-account" || gotPrincipal != principal || gotRuntimeAgent != agent {
			t.Fatalf("%s: migrated relationships changed: person=%q agent=%q connection=%q principal=%q runtimeAgent=%d",
				stage, gotPerson, gotAgent, gotConnection, gotPrincipal, gotRuntimeAgent)
		}
		token, err := db.GetToken(tokenHash)
		if err != nil || token.Principal != principal {
			t.Fatalf("%s: runtime credential = %+v, %v", stage, token, err)
		}
		audit, err := db.ListAudit(10)
		if err != nil || len(audit) != 1 || audit[0].Principal != principal ||
			audit[0].Path != "/synthetic-migration-fixture" || audit[0].Status != 200 {
			t.Fatalf("%s: audit history = %+v, %v", stage, audit, err)
		}
		if migrated {
			var registrations int
			if err := db.sql.QueryRow(`SELECT COUNT(*) FROM external_client_registrations`).Scan(&registrations); err != nil || registrations != 0 {
				t.Fatalf("%s: migration inferred %d external registrations: %v", stage, registrations, err)
			}
		} else {
			var table int
			if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='external_client_registrations'`).Scan(&table); err != nil || table != 0 {
				t.Fatalf("%s: registration table exists before 0105: count=%d err=%v", stage, table, err)
			}
		}
	}

	verifyPreserved("before migration", db, false)
	if err := db.migrateThrough("migrations/0105_external_client_registrations.sql"); err != nil {
		t.Fatalf("apply 0105: %v", err)
	}
	verifyPreserved("after 0105", db, true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Open reuses the actual migration runner: 0105 must be recorded once,
	// remain a no-op on reopen, and preserve the same legacy relationships.
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	verifyPreserved("after reopen", db, true)
	applied, err := db.AppliedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, name := range applied {
		if name == "migrations/0105_external_client_registrations.sql" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("0105 recorded %d times after reopen; migrations=%v", count, applied)
	}
}
