package runtimedb

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
)

func enrollmentFixture(t *testing.T) (*DB, CreatePendingExternalRegistration, ExternalClientRegistration) {
	t.Helper()
	db, in := externalRegistrationFixture(t)
	r, err := db.CreatePendingExternalRegistration(in)
	if err != nil {
		t.Fatal(err)
	}
	return db, in, r
}

func issueEnrollmentChallenge(t *testing.T, db *DB, r ExternalClientRegistration) IssuedExternalEnrollmentChallenge {
	t.Helper()
	issued, err := db.IssueExternalEnrollmentChallenge(IssueExternalEnrollmentChallenge{
		RegistrationID: r.ID, PersonID: r.PersonID, IssuedBy: "person-service:sam",
	})
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func consumeEnrollmentChallenge(t *testing.T, db *DB, in ConsumeExternalEnrollmentChallenge, commit bool) (ExternalEnrollmentChallenge, error) {
	t.Helper()
	tx, err := db.sql.Begin()
	if err != nil {
		return ExternalEnrollmentChallenge{}, err
	}
	defer tx.Rollback()
	c, err := db.consumeExternalEnrollmentChallengeTx(tx, in)
	if err != nil {
		return ExternalEnrollmentChallenge{}, err
	}
	if commit {
		err = tx.Commit()
	}
	return c, err
}

func TestExternalEnrollmentChallengeStoresOnlyBoundDigest(t *testing.T) {
	db, _, r := enrollmentFixture(t)
	base := time.Date(2026, 10, 14, 1, 2, 3, 400500600, time.UTC)
	db.challengeClock = func() time.Time { return base }
	issued := issueEnrollmentChallenge(t, db, r)
	if issued.Secret == "" || issued.Challenge.RegistrationID != r.ID || issued.Challenge.PersonID != r.PersonID ||
		!issued.Challenge.IssuedAt.Equal(base) || !issued.Challenge.ExpiresAt.Equal(base.Add(ExternalEnrollmentChallengeTTL)) {
		t.Fatalf("issued challenge = %+v", issued)
	}
	var storedHash string
	if err := db.sql.QueryRow(`SELECT secret_hash FROM external_enrollment_challenges WHERE id=?`, issued.Challenge.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != externalEnrollmentDigest(issued.Secret) || strings.Contains(storedHash, issued.Secret) {
		t.Fatalf("stored material = %q", storedHash)
	}
	encoded, err := json.Marshal(issued)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), issued.Secret) || strings.Contains(string(encoded), storedHash) {
		t.Fatalf("serialized challenge disclosed credential material: %s", encoded)
	}
	var auditMatches int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM audit WHERE path LIKE ? OR principal LIKE ?`,
		"%"+issued.Secret+"%", "%"+issued.Secret+"%").Scan(&auditMatches); err != nil {
		t.Fatal(err)
	}
	if auditMatches != 0 {
		t.Fatalf("secret appeared in %d audit rows", auditMatches)
	}

	mika, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO external_enrollment_challenges
		(id,registration_id,person_id,secret_hash,issued_at,expires_at,issued_by)
		VALUES(?,?,?,?,?,?,?)`, "xchal_cross_person", r.ID, mika.PersonID, strings.Repeat("0", 64),
		base.Format(time.RFC3339Nano), base.Add(time.Minute).Format(time.RFC3339Nano), "person-service:mika"); err == nil {
		t.Fatal("schema accepted a registration/challenge Person mismatch")
	}
	if _, err := db.IssueExternalEnrollmentChallenge(IssueExternalEnrollmentChallenge{
		RegistrationID: r.ID, PersonID: mika.PersonID, IssuedBy: "person-service:mika",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-Person issuance = %v, want ErrNotFound", err)
	}
	var count int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM external_enrollment_challenges`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("cross-Person issuance wrote rows: count=%d err=%v", count, err)
	}
}

func TestExternalEnrollmentChallengeConsumptionCommitsWithFutureActivation(t *testing.T) {
	db, _, r := enrollmentFixture(t)
	issued := issueEnrollmentChallenge(t, db, r)
	in := ConsumeExternalEnrollmentChallenge{RegistrationID: r.ID, PersonID: r.PersonID,
		Secret: issued.Secret, ConsumedBy: "issuer-callback:sam"}

	rolledBack, err := consumeEnrollmentChallenge(t, db, in, false)
	if err != nil || rolledBack.ConsumedAt.IsZero() {
		t.Fatalf("rolled-back consume = %+v, %v", rolledBack, err)
	}
	stored, err := db.ExternalEnrollmentChallenge(issued.Challenge.ID)
	if err != nil || !stored.ConsumedAt.IsZero() {
		t.Fatalf("rollback burned challenge: %+v, %v", stored, err)
	}
	committed, err := consumeEnrollmentChallenge(t, db, in, true)
	if err != nil || committed.ConsumedAt.IsZero() || committed.ConsumedBy != in.ConsumedBy {
		t.Fatalf("committed consume = %+v, %v", committed, err)
	}
	if _, err := consumeEnrollmentChallenge(t, db, in, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay = %v, want ErrNotFound", err)
	}
	registration, err := db.ExternalClientRegistration(r.ID)
	if err != nil || registration.State != ExternalRegistrationPendingConsent {
		t.Fatalf("challenge consumption activated registration: %+v, %v", registration, err)
	}
}

func TestConcurrentExternalEnrollmentChallengeConsumptionHasOneWinner(t *testing.T) {
	db, _, r := enrollmentFixture(t)
	issued := issueEnrollmentChallenge(t, db, r)
	in := ConsumeExternalEnrollmentChallenge{RegistrationID: r.ID, PersonID: r.PersonID,
		Secret: issued.Secret, ConsumedBy: "issuer-callback:sam"}
	var winners atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := consumeEnrollmentChallenge(t, db, in, true)
			if err == nil {
				winners.Add(1)
			} else {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	if winners.Load() != 1 {
		t.Fatalf("consumption winners = %d, want 1", winners.Load())
	}
	for err := range errs {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("loser = %v, want ErrNotFound", err)
		}
	}
}

func TestExternalEnrollmentChallengeExpiryUsesPostAdmissionClock(t *testing.T) {
	db, _, r := enrollmentFixture(t)
	issuedAt := time.Date(2026, 10, 14, 1, 2, 3, 999999500, time.UTC)
	var clockNanos atomic.Int64
	clockNanos.Store(issuedAt.UnixNano())
	db.challengeClock = func() time.Time { return time.Unix(0, clockNanos.Load()).UTC() }
	issued := issueEnrollmentChallenge(t, db, r)

	holder, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(`UPDATE external_enrollment_challenges SET invalidation_reason=invalidation_reason WHERE id=?`, issued.Challenge.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := consumeEnrollmentChallenge(t, db, ConsumeExternalEnrollmentChallenge{
			RegistrationID: r.ID, PersonID: r.PersonID, Secret: issued.Secret, ConsumedBy: "issuer-callback:sam",
		}, true)
		result <- err
	}()
	// The consumer is waiting to begin its immediate transaction. Advance past
	// the fractional expiry before releasing the writer lock.
	clockNanos.Store(issued.Challenge.ExpiresAt.Add(time.Nanosecond).UnixNano())
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-contention expired consume = %v, want ErrNotFound", err)
	}
	stored, err := db.ExternalEnrollmentChallenge(issued.Challenge.ID)
	if err != nil || !stored.ConsumedAt.IsZero() {
		t.Fatalf("expired challenge changed: %+v, %v", stored, err)
	}
}

func TestExternalEnrollmentChallengeRechecksAuthorityAndFences(t *testing.T) {
	for _, authority := range []string{"Person offboarding", "runtime principal disablement", "Agent access withdrawal"} {
		t.Run(authority, func(t *testing.T) {
			db, in, r := enrollmentFixture(t)
			issued := issueEnrollmentChallenge(t, db, r)
			if err := mutateExternalRegistrationAuthority(db.sql, in, authority); err != nil {
				t.Fatal(err)
			}
			_, err := consumeEnrollmentChallenge(t, db, ConsumeExternalEnrollmentChallenge{
				RegistrationID: r.ID, PersonID: r.PersonID, Secret: issued.Secret, ConsumedBy: "issuer-callback:sam",
			}, true)
			if !errors.Is(err, ErrNotAdmitted) {
				t.Fatalf("consume after %s = %v, want ErrNotAdmitted", authority, err)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		fence func(*DB) error
	}{
		{"execution stop", func(db *DB) error { return db.StopHouseholdExecution("mika", "test stop") }},
		{"restore quarantine", func(db *DB) error { return db.EnterRestoreQuarantine("test restore", time.Now()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _, r := enrollmentFixture(t)
			issued := issueEnrollmentChallenge(t, db, r)
			if err := tc.fence(db); err != nil {
				t.Fatal(err)
			}
			if _, err := db.IssueExternalEnrollmentChallenge(IssueExternalEnrollmentChallenge{
				RegistrationID: r.ID, PersonID: r.PersonID, IssuedBy: "person-service:sam",
			}); err == nil {
				t.Fatal("issuance crossed execution fence")
			}
			if _, err := consumeEnrollmentChallenge(t, db, ConsumeExternalEnrollmentChallenge{
				RegistrationID: r.ID, PersonID: r.PersonID, Secret: issued.Secret, ConsumedBy: "issuer-callback:sam",
			}, true); err == nil {
				t.Fatal("consumption crossed execution fence")
			}
			if err := db.InvalidateExternalEnrollmentChallenges(r.ID, "root-admin", "fenced cleanup"); err != nil {
				t.Fatalf("denial-only invalidation under fence: %v", err)
			}
			got, err := db.ExternalEnrollmentChallenge(issued.Challenge.ID)
			if err != nil || got.InvalidatedAt.IsZero() {
				t.Fatalf("invalidated challenge = %+v, %v", got, err)
			}
		})
	}
}

func TestRegistrationRevocationInvalidatesEnrollmentChallenges(t *testing.T) {
	db, _, r := enrollmentFixture(t)
	issued := issueEnrollmentChallenge(t, db, r)
	if _, err := db.RevokeExternalClientRegistration(r.ID, "root-admin"); err != nil {
		t.Fatal(err)
	}
	got, err := db.ExternalEnrollmentChallenge(issued.Challenge.ID)
	if err != nil || got.InvalidatedAt.IsZero() || got.InvalidatedBy != "root-admin" || got.InvalidationReason != "registration revoked" {
		t.Fatalf("revocation history = %+v, %v", got, err)
	}
	if _, err := consumeEnrollmentChallenge(t, db, ConsumeExternalEnrollmentChallenge{
		RegistrationID: r.ID, PersonID: r.PersonID, Secret: issued.Secret, ConsumedBy: "issuer-callback:sam",
	}, true); err == nil {
		t.Fatal("revoked registration's challenge remained usable")
	}
}

func TestExternalEnrollmentChallengeRejectsForgedAndCrossBoundProofs(t *testing.T) {
	db, _, r := enrollmentFixture(t)
	issued := issueEnrollmentChallenge(t, db, r)
	other, err := db.CreatePendingExternalRegistration(CreatePendingExternalRegistration{
		PersonID: r.PersonID, Butler: r.Butler, Connection: r.Connection, Issuer: r.Issuer,
		ClientID: r.ClientID + "-successor", RuntimePrincipal: r.RuntimePrincipal, ConfiguredBy: "root-admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, registration, person, secret string }{
		{"forged", r.ID, r.PersonID, "xproof_forged"},
		{"successor registration", other.ID, other.PersonID, issued.Secret},
		{"wrong Person", r.ID, "psn_not-the-bound-person", issued.Secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := consumeEnrollmentChallenge(t, db, ConsumeExternalEnrollmentChallenge{
				RegistrationID: tc.registration, PersonID: tc.person, Secret: tc.secret, ConsumedBy: "issuer-callback:sam",
			}, true)
			if err == nil {
				t.Fatal("mismatched proof was accepted")
			}
		})
	}
	got, err := db.ExternalEnrollmentChallenge(issued.Challenge.ID)
	if err != nil || !got.ConsumedAt.IsZero() || !got.InvalidatedAt.IsZero() {
		t.Fatalf("failed proofs changed challenge: %+v, %v", got, err)
	}
}

func TestExternalEnrollmentChallengeMigrationPreservesRegistrationHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-0106.db")
	db, err := openThrough(t, path, "migrations/0105_external_client_registrations.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []struct{ handle, role string }{{"mika", RoleOwner}, {"sam", RoleMember}} {
		if err := db.RecordButlerMember(member.handle, member.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(member.handle, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertModel(Model{Name: "migration-model", Kind: ModelLocal, Provider: "ollama",
		ProviderModel: "test-model", Readiness: ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	agent, err := db.CreateButler(Butler{Name: "migration-external", Surface: ButlerSurfaceFamily,
		AgentClass: AgentClassMember, Model: "migration-model"}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GrantButlerAccess("migration-external", "sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	connection, err := db.AddButlerAddress("migration-external", "matrix", "migration-agent", "direct", "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	person, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO chat_bindings
		(connection,account,person_id,display_name,created_at,created_by) VALUES(?,?,?,?,?,?)`,
		connection, "migration-account", person.PersonID, "Sam", now(), "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerRuntime(agent, ButlerRuntime{Type: agentapi.RuntimeHermes,
		Delivery: agentapi.DeliveryPull, ProcessingClass: agentapi.ProcessingCloud}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	principal := "migration-external-runtime"
	if err := db.CreatePrincipal(principal, RoleButler, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRuntimePrincipal(principal, agent); err != nil {
		t.Fatal(err)
	}
	registration, err := db.CreatePendingExternalRegistration(CreatePendingExternalRegistration{
		PersonID: person.PersonID, Butler: agent, Connection: connection, Issuer: "https://issuer.invalid",
		ClientID: "migration-client", RuntimePrincipal: principal, ConfiguredBy: "root-admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	var table int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='external_enrollment_challenges'`).Scan(&table); err != nil || table != 0 {
		t.Fatalf("challenge table exists before 0106: count=%d err=%v", table, err)
	}
	if err := db.migrateThrough("migrations/0106_external_enrollment_challenges.sql"); err != nil {
		t.Fatal(err)
	}
	verify := func(stage string, db *DB) {
		t.Helper()
		got, err := db.ExternalClientRegistration(registration.ID)
		if err != nil || got.PersonID != registration.PersonID || got.State != ExternalRegistrationPendingConsent {
			t.Fatalf("%s registration = %+v, %v", stage, got, err)
		}
		var challenges int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM external_enrollment_challenges`).Scan(&challenges); err != nil || challenges != 0 {
			t.Fatalf("%s inferred %d challenges: %v", stage, challenges, err)
		}
	}
	verify("after migration", db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	verify("after reopen", db)
	applied, err := db.AppliedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, name := range applied {
		if name == "migrations/0106_external_enrollment_challenges.sql" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("0106 recorded %d times after reopen; migrations=%v", count, applied)
	}
}
