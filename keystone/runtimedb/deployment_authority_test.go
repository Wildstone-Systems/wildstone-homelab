package runtimedb

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func deploymentApprovalFixture(t *testing.T) (*DB, string, string, DeploymentPlanDocument) {
	t.Helper()
	db := open(t)
	for _, member := range []string{"example-owner", "example-other-owner"} {
		if err := db.RecordButlerMember(member, RoleOwner, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(member, "example-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AcquireCoordinator("example-coordinator"); err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("a", 40)
	if err := db.SetStatefileHead(source); err != nil {
		t.Fatal(err)
	}
	owner, err := db.GetButlerMember("example-owner")
	if err != nil {
		t.Fatal(err)
	}
	doc := DeploymentPlanDocument{BasedOnHead: source, Summary: "Example deployment", Deployment: &DeploymentApproval{
		Binding: strings.Repeat("b", 64), SourceRevision: source, OwnerPersonID: owner.PersonID, OwnerGeneration: owner.Generation,
	}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertPlan("example-owner", "", doc.Summary, "high", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return db, id, string(raw), doc
}

func TestDeploymentApprovalRefusesInvalidAuthority(t *testing.T) {
	for _, change := range []string{"proposed", "rejected", "superseded", "applied", "wrong-document", "wrong-binding", "wrong-source", "moved-source", "disabled", "readmitted", "offboarded", "demoted", "role-restored", "wrong-approver", "machine-approver", "coordinator", "no-lease", "stop", "quarantine"} {
		t.Run(change, func(t *testing.T) {
			db, id, raw, doc := deploymentApprovalFixture(t)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			actor := "example-owner"
			if change == "wrong-approver" {
				actor = "example-other-owner"
			}
			if change == "machine-approver" {
				actor = "example-agent"
			}
			if change != "proposed" && change != "rejected" {
				must(db.TransitionPlan(id, PlanApproved, actor))
			}
			switch change {
			case "rejected":
				must(db.TransitionPlan(id, PlanRejected, actor))
			case "superseded":
				must(db.TransitionPlan(id, PlanSuperseded, actor))
			case "applied":
				must(db.TransitionPlan(id, PlanApplying, ""))
				must(db.TransitionPlan(id, PlanApplied, ""))
			case "wrong-document":
				raw += " "
			case "wrong-binding":
				doc.Deployment.Binding = strings.Repeat("c", 64)
			case "wrong-source":
				doc.Deployment.SourceRevision = strings.Repeat("c", 40)
			case "moved-source":
				must(db.SetStatefileHead(strings.Repeat("c", 40)))
			case "disabled":
				must(db.DisableButlerMember(actor, "example-other-owner"))
			case "readmitted":
				must(db.DisableButlerMember(actor, "example-other-owner"))
				must(db.AdmitButlerMember(actor, "example-other-owner"))
			case "offboarded":
				must(db.OffboardButlerMember(actor, "example-other-owner"))
			case "demoted", "role-restored":
				must(db.SetButlerMemberRole(actor, RoleMember, "example-other-owner"))
				if change == "role-restored" {
					must(db.SetButlerMemberRole(actor, RoleOwner, "example-other-owner"))
				}
			case "coordinator":
				_, err := db.PromoteCoordinator("example-successor")
				must(err)
			case "no-lease":
				db.executionLease.Store(nil)
			case "stop":
				must(db.StopHouseholdExecution(actor, "example stop"))
			case "quarantine":
				must(db.EnterRestoreQuarantine("example restore", time.Now()))
			}
			called := false
			err := db.WithDeploymentApproval(id, raw, doc.Deployment.Binding, doc.Deployment.SourceRevision, func() error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("unsafe authority reached effect: called=%v err=%v", called, err)
			}
		})
	}
}

func TestDeploymentApprovalRequiresStandalonePlan(t *testing.T) {
	for _, extra := range []string{`"auditOnly":true`, `"put":["example"]`, `"delete":[]`, `"plan":{}`, `"routine":{}`, `"setting":{}`, `"subscription":{}`, `"research":{}`, `"origin":{}`, `"drift":true`, `"unexpected":true`} {
		t.Run(extra, func(t *testing.T) {
			db, id, raw, doc := deploymentApprovalFixture(t)
			raw = raw[:len(raw)-1] + "," + extra + "}"
			id, err := db.InsertPlan("example-owner", "", "Example mixed deployment", "high", raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.TransitionPlan(id, PlanApproved, "example-owner"); err != nil {
				t.Fatal(err)
			}
			called := false
			if err := db.WithDeploymentApproval(id, raw, doc.Deployment.Binding, doc.Deployment.SourceRevision, func() error { called = true; return nil }); err == nil || called {
				t.Fatal("mixed Plan reached effect", err)
			}
		})
	}
}

func TestDeploymentApprovalEffectFailureAndRetry(t *testing.T) {
	db, id, raw, doc := deploymentApprovalFixture(t)
	if err := db.TransitionPlan(id, PlanApproved, "example-owner"); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("example local Git failure")
	if err := db.WithDeploymentApproval(id, raw, doc.Deployment.Binding, doc.Deployment.SourceRevision, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	calls := 0
	for _, state := range []PlanState{PlanApproved, PlanApplying} {
		if state == PlanApplying {
			if err := db.TransitionPlan(id, state, ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.WithDeploymentApproval(id, raw, doc.Deployment.Binding, doc.Deployment.SourceRevision, func() error { calls++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal("exact retry did not execute")
	}
}

func TestDeploymentApprovalSerializesConcurrentAuthorityWrites(t *testing.T) {
	// Separate handles prove database serialization, rather than a Go mutex.
	for _, change := range []string{"revoke", "source", "coordinator", "supersede", "stop"} {
		t.Run(change, func(t *testing.T) {
			db, id, raw, doc := deploymentApprovalFixture(t)
			if err := db.TransitionPlan(id, PlanApproved, "example-owner"); err != nil {
				t.Fatal(err)
			}
			other, err := Open(db.path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			entered := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- db.WithDeploymentApproval(id, raw, doc.Deployment.Binding, doc.Deployment.SourceRevision, func() error { close(entered); <-release; return nil })
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatal("effect not entered", err)
			case <-time.After(5 * time.Second):
				t.Fatal("effect timed out")
			}
			started := make(chan struct{})
			written := make(chan error, 1)
			go func() {
				close(started)
				switch change {
				case "revoke":
					written <- other.DisableButlerMember("example-owner", "example-other-owner")
				case "source":
					written <- other.SetStatefileHead(strings.Repeat("c", 40))
				case "coordinator":
					_, err := other.PromoteCoordinator("example-successor")
					written <- err
				case "supersede":
					written <- other.TransitionPlan(id, PlanSuperseded, "")
				case "stop":
					written <- other.StopHouseholdExecution("example-other-owner", "example stop")
				}
			}()
			<-started
			select {
			case err := <-written:
				close(release)
				<-done
				t.Fatal("authority write committed during effect", err)
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			called := false
			if err := db.WithDeploymentApproval(id, raw, doc.Deployment.Binding, doc.Deployment.SourceRevision, func() error { called = true; return nil }); err == nil || called {
				t.Fatal("retry ignored committed authority change", err)
			}
		})
	}
}
