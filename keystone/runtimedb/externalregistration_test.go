package runtimedb

import (
	"errors"
	"sync"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
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
	if _, err := db.sql.Exec(`UPDATE external_client_registrations SET person_id=? WHERE id=?`, "somebody-else", r.ID); err == nil {
		t.Fatal("identity-bearing registration fields were mutable")
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
		{"retired Agent", func(t *testing.T, db *DB, in *CreatePendingExternalRegistration) {
			if _, err := db.sql.Exec(`UPDATE butlers SET retired_at=?,retired_by='root-admin' WHERE seq=?`, now(), in.Butler); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong Connection", func(_ *testing.T, _ *DB, in *CreatePendingExternalRegistration) { in.Connection++ }},
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
