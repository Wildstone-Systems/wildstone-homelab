package runtimedb

import (
	"strings"
	"testing"
	"time"
)

func TestAnAgentCannotActivateItsOwnPlan(t *testing.T) {
	db := butlerFixture(t)
	_, err := db.CreateHouseholdRoutine(HouseholdRoutine{Name: "morning", Ownership: RoutineWildstone, Author: "sam", Document: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetHouseholdRoutineState("morning", RoutineEnabled, "sam"); err == nil {
		t.Fatal("unreviewed routine activated")
	}
}

func proposedRoutine(t *testing.T, db *DB, name string) string {
	t.Helper()
	if _, err := db.CreateHouseholdRoutine(HouseholdRoutine{Name: name, Ownership: RoutineWildstone, Author: "sam", Document: "fixture"}); err != nil {
		t.Fatal(err)
	}
	id, err := db.SubmitRoutinePlan(name, "sam", "", string(ButlerAudienceShared), "household notification", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func TestEverySubmittedPlanEntersTheQueue(t *testing.T) {
	db := butlerFixture(t)
	id := proposedRoutine(t, db, "morning")
	plain, err := db.InsertPlan("butler", "sam", "another proposal", "low", "{}")
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range []string{id, plain} {
		seq, _ := parsePlanID(plan)
		var count int
		if err = db.sql.QueryRow("SELECT count(*) FROM owner_review_queue WHERE plan=?", seq).Scan(&count); err != nil || count != 1 {
			t.Fatalf("unqueued: %s %v", plan, err)
		}
	}
}
func TestOnlyOwnersCanApproveRoutinePlans(t *testing.T) {
	db := butlerFixture(t)
	id := proposedRoutine(t, db, "morning")
	if err := db.ApproveRoutinePlan(id, "sam"); err == nil {
		t.Fatal("Member approved")
	}
	if err := db.ApproveRoutinePlan(id, "butler"); err == nil {
		t.Fatal("Agent approved")
	}
	if err := db.ApproveRoutinePlan(id, "mika"); err != nil {
		t.Fatal(err)
	}
	r, err := db.GetHouseholdRoutine("morning")
	if err != nil || !r.Active() {
		t.Fatalf("approved version inactive: %+v %v", r, err)
	}
}
func TestApprovalActivatesOnlyTheExactReviewedVersion(t *testing.T) {
	db := butlerFixture(t)
	id := proposedRoutine(t, db, "morning")
	if _, err := db.EditHouseholdRoutine("morning", "changed delivery", "sam", "edit"); err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(id, "mika"); err == nil {
		t.Fatal("stale Plan approved concurrent edit")
	}
}
func TestChangingDeliveryBehaviourRequiresReapproval(t *testing.T) {
	db := butlerFixture(t)
	id := proposedRoutine(t, db, "morning")
	if err := db.ApproveRoutinePlan(id, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EditHouseholdRoutine("morning", "changed delivery", "sam", "edit"); err != nil {
		t.Fatal(err)
	}
	r, err := db.GetHouseholdRoutine("morning")
	if err != nil || r.Active() {
		t.Fatalf("changed routine still active: %+v %v", r, err)
	}
	if err := db.SetHouseholdRoutineState("morning", RoutineEnabled, "mika"); err == nil {
		t.Fatal("edit enabled without new Plan approval")
	}
}
func TestChangingOutputAudienceRequiresReapproval(t *testing.T) {
	db := butlerFixture(t)
	id := proposedRoutine(t, db, "morning")
	if err := db.ApproveRoutinePlan(id, "mika"); err != nil {
		t.Fatal(err)
	}
	before, _ := db.GetHouseholdRoutine("morning")
	audience, err := db.CreateAudienceForGroups("Adults", groupID(t, db, SeedAdults))
	if err != nil {
		t.Fatal(err)
	}
	next, err := db.SubmitRoutinePlan("morning", "sam", "", audience.ID, "household notification", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetHouseholdRoutine("morning")
	if after.Active() || after.Revision <= before.Revision {
		t.Fatal("audience change did not require a new version")
	}
	if err = db.ApproveRoutinePlan(next, "mika"); err != nil {
		t.Fatal(err)
	}
}

func TestRevokingAPersonInvalidatesPendingApprovalsButNotApprovedRoutines(t *testing.T) {
	db := butlerFixture(t)
	approved := proposedRoutine(t, db, "approved")
	pending := proposedRoutine(t, db, "pending")
	if err := db.ApproveRoutinePlan(approved, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.DisableButlerMember("sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(pending, "mika"); err == nil {
		t.Fatal("revoked proposer retained pending Plan")
	}
	r, err := db.GetHouseholdRoutine("approved")
	if err != nil || !r.Active() {
		t.Fatalf("approved Household routine lost authorization: %+v %v", r, err)
	}
}

func TestRoutineCannotBeCreatedActive(t *testing.T) {
	db := butlerFixture(t)
	for _, state := range []string{RoutineEnabled, RoutineTrial} {
		if _, err := db.CreateHouseholdRoutine(HouseholdRoutine{Name: state, Ownership: RoutineWildstone, Author: "mika", Document: "fixture", State: state}); err == nil {
			t.Fatal("creation bypassed review", state)
		}
	}
}
func TestReviewedRoutinePayloadCannotChangeWithoutVersion(t *testing.T) {
	db := butlerFixture(t)
	id := proposedRoutine(t, db, "morning")
	if err := db.ApproveRoutinePlan(id, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec("UPDATE household_routines SET document='unreviewed' WHERE name='morning'"); err == nil {
		t.Fatal("approved document changed without a new version")
	}
	seq, _ := parsePlanID(id)
	if _, err := db.sql.Exec("UPDATE routine_plan_versions SET document='unreviewed' WHERE plan=?", seq); err == nil {
		t.Fatal("review snapshot mutated")
	}
}

func TestReviewNotificationsFollowDesignatedOwnerPreference(t *testing.T) {
	db := butlerFixture(t)
	if _, err := db.AcquireCoordinator("owner-review-test"); err != nil {
		t.Fatal(err)
	}
	at := connect(t, db, "review-agent", "telegram", ButlerSurfaceFamily)
	owner := bind(t, db, at, "mika", "owner-account")
	member := bind(t, db, at, "sam", "member-account")
	if _, err := db.InsertPlan("butler", "sam", "sensitive proposal text", "low", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueueOwnerReviewNotifications(time.Now()); err != nil {
		t.Fatal(err)
	}
	messages, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("unconfigured delivery: %v %v", messages, err)
	}
	if err = db.SetOwnerReviewPreference("mika", OwnerReviewPreference{BindingID: member.ID, ConsoleURL: "https://console.home"}); err == nil {
		t.Fatal("Member selected as Owner reviewer")
	}
	if err = db.SetOwnerReviewPreference("mika", OwnerReviewPreference{BindingID: owner.ID, ConsoleURL: "https://console.home"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = db.QueueOwnerReviewNotifications(time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = db.sql.QueryRow("SELECT count(*) FROM chat_outbound").Scan(&count); err != nil || count != 1 {
		t.Fatalf("notice duplicated: %d %v", count, err)
	}
	var body string
	var binding int64
	if err = db.sql.QueryRow("SELECT body,binding_id FROM chat_outbound").Scan(&body, &binding); err != nil {
		t.Fatal(err)
	}
	if binding != owner.ID || strings.Contains(body, "sensitive") || !strings.Contains(body, "https://console.home") {
		t.Fatalf("wrong notice: %d %s", binding, body)
	}

	var delivered string
	var first int64
	if err = db.sql.QueryRow("SELECT delivered_at,outbound FROM notices WHERE kind='owner.review'").Scan(&delivered, &first); err != nil {
		t.Fatal(err)
	}
	if delivered != "" {
		t.Fatal("queued notice marked delivered before adapter acknowledgement")
	}
	if _, err = db.sql.Exec("UPDATE chat_outbound SET expires_at='2000-01-01T00:00:00Z' WHERE seq=?", first); err != nil {
		t.Fatal(err)
	}
	if err = db.QueueOwnerReviewNotifications(time.Now()); err != nil {
		t.Fatal(err)
	}
	var retry int64
	if err = db.sql.QueryRow("SELECT outbound FROM notices WHERE kind='owner.review'").Scan(&retry); err != nil {
		t.Fatal(err)
	}
	if retry <= first {
		t.Fatal("expired outbound did not get a new deliverable queue sequence")
	}
	if err = db.AckChatOutbound(at, retry); err != nil {
		t.Fatal(err)
	}
	if err = db.sql.QueryRow("SELECT delivered_at FROM notices WHERE kind='owner.review'").Scan(&delivered); err != nil || delivered == "" {
		t.Fatalf("adapter acknowledgement not recorded: %s %v", delivered, err)
	}
}

func TestRoutineApprovalDoesNotAuthorizeInfrastructureReconciliation(t *testing.T) {
	db := butlerFixture(t)
	id := proposedRoutine(t, db, "morning")
	if err := db.ApproveRoutinePlan(id, "mika"); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PlansInState(PlanApproved)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.ID == id {
			t.Fatal("routine approval became an infrastructure execution approval")
		}
	}
}

func TestChangingAssignedButlerRequiresReview(t *testing.T) {
	db := butlerFixture(t)
	newButler(t, db, "health", "kitchen-box")
	id := proposedRoutine(t, db, "morning")
	if err := db.ApproveRoutinePlan(id, "mika"); err != nil {
		t.Fatal(err)
	}
	before, _ := db.GetHouseholdRoutine("morning")
	if err := db.AssignRoutineButler("morning", "health", "mika"); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetHouseholdRoutine("morning")
	if err != nil {
		t.Fatal(err)
	}
	if after.Active() || after.Revision <= before.Revision {
		t.Fatal("assigned Agent changed without a new reviewed version")
	}
	pending, err := db.SubmitRoutinePlan("morning", "sam", "", string(ButlerAudienceShared), "household notification", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	record, err := db.GetPlan(pending)
	if err != nil || !strings.Contains(record.PlanJSON, `"assignedButler":"health"`) {
		t.Fatalf("review omits assigned Agent: %s %v", record.PlanJSON, err)
	}
	if _, err = db.sql.Exec("UPDATE household_routines SET butler='' WHERE name='morning'"); err == nil {
		t.Fatal("SQL changed assigned Agent without a version")
	}
	if err = db.ApproveRoutinePlan(pending, "mika"); err != nil {
		t.Fatal(err)
	}
}
func TestResolvedPlanCancelsUndeliveredReviewNotice(t *testing.T) {
	db := butlerFixture(t)
	if _, err := db.AcquireCoordinator("review-test"); err != nil {
		t.Fatal(err)
	}
	at := connect(t, db, "review-agent", "telegram", ButlerSurfaceFamily)
	owner := bind(t, db, at, "mika", "owner-account")
	id, err := db.InsertPlan("butler", "sam", "review me", "low", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetOwnerReviewPreference("mika", OwnerReviewPreference{BindingID: owner.ID, ConsoleURL: "https://console.home"}); err != nil {
		t.Fatal(err)
	}
	if err = db.QueueOwnerReviewNotifications(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec("UPDATE chat_outbound SET hold_until=''"); err != nil {
		t.Fatal(err)
	}
	messages, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 1 {
		t.Fatalf("positive delivery control: %v %v", messages, err)
	}
	if err = db.TransitionPlan(id, PlanRejected, "mika"); err != nil {
		t.Fatal(err)
	}
	messages, err = db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("resolved Plan notice delivered: %v %v", messages, err)
	}
	// A worker holding an earlier queue snapshot cannot publish after resolution.
	if _, err = db.sql.Exec("DELETE FROM chat_outbound"); err != nil {
		t.Fatal(err)
	}
	_, err = db.EnqueueChatOutbound(ChatOutbound{Connection: at.Seq, Target: owner.Conversation, BindingID: owner.ID, Kind: "push", Body: "A Plan is waiting", DedupeKey: "owner-review:" + id + ":stale"})
	if err != nil {
		t.Fatal(err)
	}
	messages, err = db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("stale worker snapshot delivered: %v %v", messages, err)
	}
}

func TestReviewPreferenceChangeCancelsQueuedNotice(t *testing.T) {
	for _, mode := range []string{"optout", "url", "revoke", "binding", "demote"} {
		optOut := mode != "url"
		t.Run(mode, func(t *testing.T) {
			db := butlerFixture(t)
			if _, err := db.AcquireCoordinator("review-pref-test"); err != nil {
				t.Fatal(err)
			}
			at := connect(t, db, "review-agent", "telegram", ButlerSurfaceFamily)
			owner := bind(t, db, at, "mika", "owner-account")
			if _, err := db.InsertPlan("butler", "sam", "review me", "low", "{}"); err != nil {
				t.Fatal(err)
			}
			pref := OwnerReviewPreference{BindingID: owner.ID, ConsoleURL: "https://console.home"}
			if err := db.SetOwnerReviewPreference("mika", pref); err != nil {
				t.Fatal(err)
			}
			if err := db.QueueOwnerReviewNotifications(time.Now()); err != nil {
				t.Fatal(err)
			}
			db.sql.Exec("UPDATE chat_outbound SET hold_until=''")
			messages, err := db.ChatOutboundAfter(at, 0, 10)
			if err != nil || len(messages) != 1 {
				t.Fatalf("positive control: %v %v", messages, err)
			}
			stale := messages[0]
			if optOut {
				pref = OwnerReviewPreference{}
			} else {
				pref.ConsoleURL = "https://new-console.home"
			}
			if mode == "revoke" || mode == "demote" {
				if err = db.RecordButlerMember("backup-owner", RoleOwner, "", ""); err != nil {
					t.Fatal(err)
				}
				if err = db.AdmitButlerMember("backup-owner", "root-admin"); err != nil {
					t.Fatal(err)
				}
				if mode == "demote" {
					err = db.SetButlerMemberRole("mika", RoleMember, "root-admin")
				} else {
					err = db.DisableButlerMember("mika", "root-admin")
				}
			} else if mode == "binding" {
				err = db.RevokeChatBinding(owner.ID)
			} else {
				err = db.SetOwnerReviewPreference("mika", pref)
			}
			if err != nil {
				t.Fatal(err)
			}
			messages, err = db.ChatOutboundAfter(at, 0, 10)
			if err != nil || len(messages) != 0 {
				t.Fatalf("old preference delivered: %v %v", messages, err)
			}
			var canceled int
			if err = db.sql.QueryRow("SELECT COUNT(*) FROM notices WHERE kind='owner.review' AND undelivered_at<>''").Scan(&canceled); err != nil || canceled != 1 {
				t.Fatalf("notice cancellation: %d %v", canceled, err)
			}
			db.sql.Exec("DELETE FROM chat_outbound")
			if _, err = db.EnqueueChatOutbound(stale); err != nil {
				t.Fatal(err)
			}
			messages, err = db.ChatOutboundAfter(at, 0, 10)
			if err != nil || len(messages) != 0 {
				t.Fatalf("stale snapshot delivered: %v %v", messages, err)
			}
			if !optOut {
				if err = db.QueueOwnerReviewNotifications(time.Now()); err != nil {
					t.Fatal(err)
				}
				db.sql.Exec("UPDATE chat_outbound SET hold_until=''")
				messages, err = db.ChatOutboundAfter(at, 0, 10)
				if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Body, "new-console.home") {
					t.Fatalf("new preference missing: %v %v", messages, err)
				}
			}
		})
	}
}

func TestMixedCaseProposerRemainsBoundToRevocation(t *testing.T) {
	db := butlerFixture(t)
	if _, err := db.CreateHouseholdRoutine(HouseholdRoutine{Name: "morning-case", Ownership: RoutineWildstone, Author: "sam", Document: "fixture"}); err != nil {
		t.Fatal(err)
	}
	id, err := db.SubmitRoutinePlan("morning-case", "SaM", "", string(ButlerAudienceShared), "notify", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.DisableButlerMember("sam", "mika"); err != nil {
		t.Fatal(err)
	}
	plan, err := db.GetPlan(id)
	if err != nil || plan.State != PlanSuperseded {
		t.Fatalf("revoked mixed-case proposer retained Plan: %+v %v", plan, err)
	}
	if err = db.ApproveRoutinePlan(id, "mika"); err == nil {
		t.Fatal("revoked proposer approved")
	}
}

func TestCaseCollidingProposersDoNotBreakQueue(t *testing.T) {
	db := butlerFixture(t)
	var err error
	for _, name := range []string{"Admin", "admin"} {
		if err = db.RecordButlerMember(name, RoleMember, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]string{}
	for _, name := range []string{"Admin", "admin", "ADMIN"} {
		id, e := db.InsertPlan("butler", name, "case test", "low", "{}")
		if e != nil {
			t.Fatal(e)
		}
		ids[name] = id
	}
	for name, id := range ids {
		var actual string
		if err = db.sql.QueryRow("SELECT COALESCE(proposer_person_id,'') FROM owner_review_queue WHERE plan=?", strings.TrimPrefix(id, "plan-")).Scan(&actual); err != nil {
			t.Fatal(err)
		}
		var expected string
		if name != "ADMIN" {
			if err = db.sql.QueryRow("SELECT person_id FROM butler_members WHERE member=? COLLATE BINARY", name).Scan(&expected); err != nil {
				t.Fatal(err)
			}
		}
		if actual != expected {
			t.Fatalf("%s resolved to %q want %q", name, actual, expected)
		}
	}
}

func TestRoutineProposerUsesExactIdentityAndRejectsAmbiguity(t *testing.T) {
	db := butlerFixture(t)
	for _, name := range []string{"Admin", "admin"} {
		if err := db.RecordButlerMember(name, RoleMember, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CreateHouseholdRoutine(HouseholdRoutine{Name: "case-routine", Ownership: RoutineWildstone, Author: "sam", Document: "fixture"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Admin", "admin", "ADMIN"} {
		id, err := db.SubmitRoutinePlan("case-routine", name, "", string(ButlerAudienceShared), "notify", "fixture")
		if name == "ADMIN" {
			if err == nil {
				t.Fatal("ambiguous actor was resolved")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var actual, expected string
		if err = db.sql.QueryRow("SELECT person_id FROM butler_members WHERE member=? COLLATE BINARY", name).Scan(&expected); err != nil {
			t.Fatal(err)
		}
		if err = db.sql.QueryRow("SELECT proposer_person_id FROM owner_review_queue WHERE plan=?", strings.TrimPrefix(id, "plan-")).Scan(&actual); err != nil {
			t.Fatal(err)
		}
		if actual != expected {
			t.Fatalf("%s bound to another Person", name)
		}
	}
}

func TestApprovingRoutineSupersedesDuplicateSubmissions(t *testing.T) {
	db := butlerFixture(t)
	first := proposedRoutine(t, db, "morning-duplicates")
	second, err := db.SubmitRoutinePlan("morning-duplicates", "sam", "", string(ButlerAudienceShared), "household notification", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	unrelated := proposedRoutine(t, db, "evening-unrelated")
	if err = db.ApproveRoutinePlan(first, "mika"); err != nil {
		t.Fatal(err)
	}
	sibling, err := db.GetPlan(second)
	if err != nil || sibling.State != PlanSuperseded {
		t.Fatalf("duplicate still pending: %+v %v", sibling, err)
	}
	if err = db.ApproveRoutinePlan(second, "mika"); err == nil {
		t.Fatal("duplicate approved twice")
	}
	other, err := db.GetPlan(unrelated)
	if err != nil || other.State != PlanProposed {
		t.Fatalf("unrelated changed: %+v %v", other, err)
	}
}
