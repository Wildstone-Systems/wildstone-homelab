package runtimedb

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

func activateFixture(t *testing.T, subject string) (*DB, CreatePendingExternalRegistration, ExternalClientRegistration, IssuedExternalEnrollmentChallenge) {
	t.Helper()
	db, in, registration := enrollmentFixture(t)
	issued := issueEnrollmentChallenge(t, db, registration)
	if _, err := db.ActivateExternalClientRegistration(ActivateExternalRegistration{
		RegistrationID: registration.ID, PersonID: registration.PersonID, Issuer: registration.Issuer,
		Subject: subject, Proof: issued.Secret, ActivatedBy: registration.PersonID,
	}); err != nil {
		t.Fatal(err)
	}
	return db, in, registration, issued
}

func TestExternalRegistrationActivationIsAtomicAndRetiresStaticTokens(t *testing.T) {
	db, in, registration := enrollmentFixture(t)
	const runtimeHash = "synthetic-runtime-token-hash"
	if err := db.InsertToken(runtimeHash, in.RuntimePrincipal, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreatePrincipal("unrelated-runtime", RoleButler, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertToken("synthetic-unrelated-token-hash", "unrelated-runtime", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	issued := issueEnrollmentChallenge(t, db, registration)
	const exactSubject = "Case-Sensitive-Subject"
	active, err := db.ActivateExternalClientRegistration(ActivateExternalRegistration{
		RegistrationID: registration.ID, PersonID: registration.PersonID, Issuer: registration.Issuer,
		Subject: exactSubject, Proof: issued.Secret, ActivatedBy: registration.PersonID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if active.State != ExternalRegistrationActive || active.Subject != exactSubject ||
		active.CredentialEpoch != 1 || active.ActivatedAt.IsZero() || active.ActivatedBy != registration.PersonID {
		t.Fatalf("active registration = %+v", active)
	}
	encoded, err := json.Marshal(active)
	if err != nil || strings.Contains(string(encoded), exactSubject) {
		t.Fatalf("registration JSON disclosed exact subject: %s, %v", encoded, err)
	}
	challenge, err := db.ExternalEnrollmentChallenge(issued.Challenge.ID)
	if err != nil || challenge.ConsumedAt.IsZero() {
		t.Fatalf("challenge = %+v, %v", challenge, err)
	}
	token, err := db.GetToken(runtimeHash)
	if err != nil || token.RevokedAt.IsZero() {
		t.Fatalf("runtime token = %+v, %v", token, err)
	}
	unrelated, err := db.GetToken("synthetic-unrelated-token-hash")
	if err != nil || !unrelated.RevokedAt.IsZero() {
		t.Fatalf("unrelated token = %+v, %v", unrelated, err)
	}
	if err := db.InsertToken("synthetic-new-token-hash", in.RuntimePrincipal, "", time.Time{}); err == nil {
		t.Fatal("minted a static token for an active external profile")
	}
}

func TestActiveExternalRegistrationAtomicallyRefusesStaticTokenTouch(t *testing.T) {
	db, in, registration := enrollmentFixture(t)
	const tokenHash = "synthetic-profile-fence-token-hash"
	if err := db.InsertToken(tokenHash, in.RuntimePrincipal, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	issued := issueEnrollmentChallenge(t, db, registration)
	if _, err := db.ActivateExternalClientRegistration(ActivateExternalRegistration{
		RegistrationID: registration.ID, PersonID: registration.PersonID, Issuer: registration.Issuer,
		Subject: "subject", Proof: issued.Secret, ActivatedBy: registration.PersonID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchUsableStaticToken(tokenHash, in.RuntimePrincipal); !errors.Is(err, ErrNotFound) {
		t.Fatalf("static token touch after activation = %v, want ErrNotFound", err)
	}
}

func TestExternalRegistrationActivationRollbackPreservesProofAndToken(t *testing.T) {
	db, in, registration := enrollmentFixture(t)
	const tokenHash = "synthetic-rollback-token-hash"
	if err := db.InsertToken(tokenHash, in.RuntimePrincipal, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	issued := issueEnrollmentChallenge(t, db, registration)
	_, err := db.ActivateExternalClientRegistration(ActivateExternalRegistration{
		RegistrationID: registration.ID, PersonID: registration.PersonID, Issuer: registration.Issuer,
		Subject: "subject", Proof: "wrong-proof", ActivatedBy: registration.PersonID,
	})
	if err == nil {
		t.Fatal("activation accepted a forged proof")
	}
	stored, err := db.ExternalClientRegistration(registration.ID)
	if err != nil || stored.State != ExternalRegistrationPendingConsent || stored.Subject != "" {
		t.Fatalf("registration after rollback = %+v, %v", stored, err)
	}
	challenge, err := db.ExternalEnrollmentChallenge(issued.Challenge.ID)
	if err != nil || !challenge.ConsumedAt.IsZero() {
		t.Fatalf("challenge after rollback = %+v, %v", challenge, err)
	}
	token, err := db.GetToken(tokenHash)
	if err != nil || !token.RevokedAt.IsZero() {
		t.Fatalf("token after rollback = %+v, %v", token, err)
	}
}

func TestExternalRegistrationSubjectIsByteExactAndUnique(t *testing.T) {
	db, in, first := enrollmentFixture(t)
	firstProof := issueEnrollmentChallenge(t, db, first)
	if _, err := db.ActivateExternalClientRegistration(ActivateExternalRegistration{
		RegistrationID: first.ID, PersonID: first.PersonID, Issuer: first.Issuer,
		Subject: "Subject-A", Proof: firstProof.Secret, ActivatedBy: first.PersonID,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := db.ExternalClientRegistration(first.ID)
	if err != nil || stored.Subject != "Subject-A" {
		t.Fatalf("subject was normalized: %+v, %v", stored, err)
	}
	// Case-distinct subjects are distinct tuples. Revoke the first profile so
	// the Person/Agent uniqueness constraints do not mask that fact.
	if _, err := db.RevokeExternalClientRegistration(first.ID, "root-admin"); err != nil {
		t.Fatal(err)
	}
	second, err := db.CreatePendingExternalRegistration(in)
	if err != nil {
		t.Fatal(err)
	}
	secondProof := issueEnrollmentChallenge(t, db, second)
	if got, err := db.ActivateExternalClientRegistration(ActivateExternalRegistration{
		RegistrationID: second.ID, PersonID: second.PersonID, Issuer: second.Issuer,
		Subject: "subject-a", Proof: secondProof.Secret, ActivatedBy: second.PersonID,
	}); err != nil || got.Subject != "subject-a" {
		t.Fatalf("case-distinct subject activation = %+v, %v", got, err)
	}
}

func TestConcurrentExternalRegistrationActivationHasOneWinner(t *testing.T) {
	db, in, first := enrollmentFixture(t)
	second, err := db.CreatePendingExternalRegistration(in)
	if err != nil {
		t.Fatal(err)
	}
	proofs := []IssuedExternalEnrollmentChallenge{issueEnrollmentChallenge(t, db, first), issueEnrollmentChallenge(t, db, second)}
	registrations := []ExternalClientRegistration{first, second}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := range registrations {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := db.ActivateExternalClientRegistration(ActivateExternalRegistration{
				RegistrationID: registrations[i].ID, PersonID: registrations[i].PersonID,
				Issuer: registrations[i].Issuer, Subject: "subject-concurrent",
				Proof: proofs[i].Secret, ActivatedBy: registrations[i].PersonID,
			}); err == nil {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("activation winners = %d, want 1", winners.Load())
	}
}

func TestActiveExternalRegistrationHasIndependentPersonAgentAndTupleUniqueness(t *testing.T) {
	db, in, first, _ := activateFixture(t, "Subject-A")
	mika, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := db.CreateButler(Butler{Name: "other-external-profile", Surface: ButlerSurfaceAdmin,
		AgentClass: AgentClassOwner, Model: "kitchen-box"}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	otherConnection, err := db.AddButlerAddress("other-external-profile", "matrix", "other-profile", "direct", "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetButlerRuntime(otherAgent, ButlerRuntime{Type: agentapi.RuntimeHermes,
		Delivery: agentapi.DeliveryPull, ProcessingClass: agentapi.ProcessingCloud}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreatePrincipal("runtime-other-profile", RoleButler, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRuntimePrincipal("runtime-other-profile", otherAgent); err != nil {
		t.Fatal(err)
	}
	insertActive := func(id, person string, agent, connection int64, issuer, subject, client, runtime string) error {
		_, err := db.sql.Exec(`INSERT INTO external_client_registrations
			(id,person_id,butler,connection,issuer,client_id,runtime_principal,subject,state,
			 configured_at,configured_by,activated_at,activated_by,credential_epoch)
			VALUES(?,?,?,?,?,?,?,?,'active',?,'root-admin',?, ?,1)`,
			id, person, agent, connection, issuer, client, runtime, subject, now(), now(), person)
		return err
	}
	if err := insertActive("xreg_duplicate_person", first.PersonID, otherAgent, otherConnection,
		first.Issuer, "different-subject", "different-client", "runtime-other-profile"); err == nil {
		t.Fatal("active Person uniqueness was not enforced")
	}
	if err := insertActive("xreg_duplicate_agent", mika.PersonID, first.Butler, first.Connection,
		first.Issuer, "different-subject", "different-client", in.RuntimePrincipal); err == nil {
		t.Fatal("active Agent uniqueness was not enforced")
	}
	if err := insertActive("xreg_duplicate_tuple", mika.PersonID, otherAgent, otherConnection,
		first.Issuer, "Subject-A", first.ClientID, "runtime-other-profile"); err == nil {
		t.Fatal("active issuer/subject/client uniqueness was not enforced")
	}
}

func TestActiveExternalRegistrationRevocationIsTerminalAndIdempotent(t *testing.T) {
	db, _, registration, _ := activateFixture(t, "subject")
	first, err := db.RevokeExternalClientRegistration(registration.ID, "root-admin")
	if err != nil || first.State != ExternalRegistrationRevoked || first.Subject != "subject" {
		t.Fatalf("first revoke = %+v, %v", first, err)
	}
	second, err := db.RevokeExternalClientRegistration(registration.ID, "someone-else")
	if err != nil || second.RevokedBy != "root-admin" || !second.RevokedAt.Equal(first.RevokedAt) {
		t.Fatalf("idempotent revoke = %+v, %v", second, err)
	}
	if _, err := db.sql.Exec(`UPDATE external_client_registrations SET state='active' WHERE id=?`, registration.ID); err == nil {
		t.Fatal("revoked registration returned to active")
	}
}

func TestActiveExternalRegistrationBlocksEverySecondPersonAdmissionPath(t *testing.T) {
	t.Run("Agent access", func(t *testing.T) {
		db, _, _, _ := activateFixture(t, "subject")
		if err := db.GrantButlerAccess("sam-external", "mika", "root-admin"); err == nil {
			t.Fatal("granted another Person access to the active external Agent")
		}
	})

	t.Run("chat binding", func(t *testing.T) {
		db, in, _, _ := activateFixture(t, "subject")
		mika, _ := db.GetButlerMember("mika")
		if _, err := db.sql.Exec(`INSERT INTO chat_bindings
			(connection,account,person_id,display_name,created_at,created_by)
			VALUES(?,?,?,?,?,?)`, in.Connection, "other-account", mika.PersonID, "Other", now(), "root-admin"); err == nil {
			t.Fatal("created another Person's binding on the active external Agent")
		}
	})

	t.Run("readmission and role", func(t *testing.T) {
		db, in, registration := enrollmentFixture(t)
		if err := db.RecordButlerMember("joan", RoleMember, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember("joan", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := db.GrantButlerAccess("sam-external", "joan", "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := db.DisableButlerMember("joan", "root-admin"); err != nil {
			t.Fatal(err)
		}
		issued := issueEnrollmentChallenge(t, db, registration)
		if _, err := db.ActivateExternalClientRegistration(ActivateExternalRegistration{
			RegistrationID: registration.ID, PersonID: registration.PersonID, Issuer: in.Issuer,
			Subject: "subject", Proof: issued.Secret, ActivatedBy: registration.PersonID,
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember("joan", "root-admin"); err == nil {
			t.Fatal("re-admitted a second Person with a live access row")
		}
		if err := db.SetButlerMemberRole("joan", RoleOwner, "root-admin"); err != nil && !errors.Is(err, ErrLastOwner) {
			// A disabled Person's role alone does not admit them. The important
			// restoration path above remains denied; keep unrelated role rules intact.
			t.Fatalf("disabled role change failed unexpectedly: %v", err)
		}
	})
}
