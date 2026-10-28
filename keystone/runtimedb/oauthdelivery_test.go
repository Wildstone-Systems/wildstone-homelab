package runtimedb

import (
	"errors"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
)

func admittedDeliveryFixture(t *testing.T) (*DB, ExternalClientRegistration, ExternalTaskAdmission) {
	t.Helper()
	db, vault, in := externalAdmissionFixture(t)
	admitted, err := vault.AdmitExternalTask(in)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := db.ExternalClientRegistration(in.Identity.RegistrationID)
	if err != nil {
		t.Fatal(err)
	}
	return db, reg, admitted
}

func deliveryIdentity(reg ExternalClientRegistration) ExternalDeliveryIdentity {
	return ExternalDeliveryIdentity{RegistrationID: reg.ID, PersonID: reg.PersonID, Butler: reg.Butler,
		Connection: reg.Connection, RuntimePrincipal: reg.RuntimePrincipal, CredentialEpoch: reg.CredentialEpoch}
}

func TestOAuthDeliveryRequiresExactProvenanceAndPreservesStaticRows(t *testing.T) {
	db, reg, admitted := admittedDeliveryFixture(t)
	oauthID, err := db.RecordAgentDeliveryForTask(reg.Butler, agentapi.DeliveryTurn, admitted.TaskID,
		`{"handle":"`+admitted.Handle+`","text":"synthetic turn"}`)
	if err != nil {
		t.Fatal(err)
	}
	legacyID, err := db.RecordAgentDelivery(reg.Butler, agentapi.DeliveryEscalation, `{"instruction":"legacy"}`)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.PollExternalDeliveries(deliveryIdentity(reg), 0, 50, time.Now)
	if err != nil || len(rows) != 1 || rows[0].ID != oauthID || rows[0].ID == legacyID {
		t.Fatalf("OAuth poll = %+v, %v", rows, err)
	}
	p := rows[0].Provenance
	if p.RegistrationID != reg.ID || p.PersonID != reg.PersonID || p.Connection != reg.Connection ||
		p.RuntimePrincipal != reg.RuntimePrincipal || p.CredentialEpoch != reg.CredentialEpoch ||
		p.Task == 0 || p.Audience != ButlerAudiencePrivate {
		t.Fatalf("provenance = %+v", p)
	}
	if _, err := db.sql.Exec(`UPDATE agent_deliveries SET person_id=person_id||'-changed' WHERE id=?`, oauthID); err == nil {
		t.Fatal("OAuth delivery provenance was mutable")
	}
	staticRows, err := db.ListUnackedAgentDeliveries(reg.Butler, 0, 50)
	if err != nil || len(staticRows) != 1 || staticRows[0].ID != legacyID {
		t.Fatalf("static poll = %+v, %v", staticRows, err)
	}
	if err := db.AckAgentDelivery(reg.Butler, oauthID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("static ack of OAuth row = %v", err)
	}

	wrong := deliveryIdentity(reg)
	wrong.PersonID += "-other"
	if err := db.AckExternalDelivery(wrong, oauthID); !errors.Is(err, ErrRuntimeDeliveryDenied) {
		t.Fatalf("wrong identity ack = %v", err)
	}
	if err := db.AckExternalDelivery(deliveryIdentity(reg), legacyID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy OAuth ack = %v", err)
	}
	if err := db.AckExternalDelivery(deliveryIdentity(reg), oauthID); err != nil {
		t.Fatal(err)
	}
	if err := db.AckExternalDelivery(deliveryIdentity(reg), oauthID); err != nil {
		t.Fatalf("duplicate exact ack = %v", err)
	}
}

func TestOAuthProposalOutcomeKeepsOriginatingTask(t *testing.T) {
	db, reg, admitted := admittedDeliveryFixture(t)
	planID, err := db.InsertPlan(reg.RuntimePrincipal, reg.PersonID, "synthetic proposal", "low", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.LinkExternalPlanOrigin(planID, admitted.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordAgentDeliveryForPlan(reg.Butler, agentapi.DeliveryProposalOutcome, planID,
		`{"plan":"`+planID+`","outcome":"approved"}`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.PollExternalDeliveries(deliveryIdentity(reg), 0, 50, time.Now)
	if err != nil || len(rows) != 1 || rows[0].Kind != agentapi.DeliveryProposalOutcome || rows[0].Provenance.Task == 0 {
		t.Fatalf("proposal deliveries = %+v, %v", rows, err)
	}
}

func TestOAuthDeliveryRevocationFailsClosed(t *testing.T) {
	db, reg, admitted := admittedDeliveryFixture(t)
	if _, err := db.RecordAgentDeliveryForTask(reg.Butler, agentapi.DeliveryTurn, admitted.TaskID, `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RevokeExternalClientRegistration(reg.ID, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PollExternalDeliveries(deliveryIdentity(reg), 0, 50, time.Now); !errors.Is(err, ErrRuntimeDeliveryDenied) {
		t.Fatalf("revoked poll = %v", err)
	}
}
