package runtimedb

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalOnlyNeverFallsBackToCloud(t *testing.T) {
	db, _, task := disclosureFixture(t)
	seq := disclosedMemory(t, db, task)
	if err := db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckMemoryPrivacy(seq, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("cloud allowed: %v", err)
	}
	if err := db.CheckMemoryPrivacy(seq, ModelLocal, MemoryDestination{BuiltIn: true}); err != nil {
		t.Fatal(err)
	}
}
func TestLocalInferenceDoesNotLicenseExternalDisclosure(t *testing.T) {
	db, at, task := disclosureFixture(t)
	seq := disclosedMemory(t, db, task)
	if err := db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckMemoryPrivacy(seq, ModelLocal, MemoryDestination{ConnectionRef: at.Seq}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("external disclosure allowed: %v", err)
	}
	if err := db.CheckMemoryPrivacy(seq, ModelLocal, MemoryDestination{BuiltIn: true}); err != nil {
		t.Fatal(err)
	}
}
func TestPersonalPrivacyCannotBeChangedByAnotherPerson(t *testing.T) {
	db, _, task := disclosureFixture(t)
	seq := disclosedMemory(t, db, task)
	if err := db.SetMemoryPrivacy("mika", seq, PrivacyPolicy{LocalOnly: true}); err == nil {
		t.Fatal("Owner changed another Person's policy")
	}
}

func TestPrivacyDerivativesKeepStrictestSourcePolicy(t *testing.T) {
	for _, op := range []string{"derive", "correct", "promote"} {
		t.Run(op, func(t *testing.T) {
			db, _, task := disclosureFixture(t)
			seq := disclosedMemory(t, db, task)
			if err := db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
				t.Fatal(err)
			}
			m, err := db.GetMemory(seq)
			if err != nil {
				t.Fatal(err)
			}
			var child int64
			switch op {
			case "derive":
				child, err = db.DeriveMemory(m, []int64{seq})
			case "correct":
				child, err = db.CorrectMemory(seq, m, "sam")
			case "promote":
				child, err = db.PromotePersonalMemory(seq, "sam")
			}
			if err != nil {
				t.Fatal(err)
			}
			p, err := db.MemoryPrivacy(child)
			if err != nil || !p.LocalOnly || !p.TrustedOnly {
				t.Fatalf("lost policy: %+v %v", p, err)
			}
		})
	}
}
func TestFirstTurnMemoryPrivacySurvivesSessionLink(t *testing.T) {
	db, _, task := disclosureFixture(t)
	seq := disclosedMemory(t, db, task)
	ctx, p, err := db.BeginPrivacyTurn(context.Background(), ModelLocal, MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = p.SetTask(task); err != nil {
		t.Fatal(err)
	}
	TrackMemoryPrivacy(ctx, seq, nil)
	if err = p.Check(); err != nil {
		t.Fatal(err)
	}
	session, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "sam", Butler: "memory-agent", Surface: ButlerSurfaceFamily, Mode: ButlerModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetButlerTaskSession(task, session); err != nil {
		t.Fatal(err)
	}
	// Tightening the original later also constrains every answer that used it.
	if err = db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err = db.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("resumed history widened: %v", err)
	}
	child := disclosedMemory(t, db, task)
	policy, err := db.MemoryPrivacy(child)
	if err != nil || !policy.LocalOnly {
		t.Fatalf("conversation lost privacy: %+v %v", policy, err)
	}
}

func TestDisclosurePolicyIsEvaluatedPerDestination(t *testing.T) {
	db, at, task := disclosureFixture(t)
	seq := disclosedMemory(t, db, task)
	external := db.BindMemoryRead("sam", "memory-agent", MemoryDestination{ConnectionRef: at.Seq})
	if hits, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "memory-agent", ReadContext: external}); err != nil || len(hits) != 1 {
		t.Fatalf("positive control: %v %v", hits, err)
	}
	if err := db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	for _, trusted := range []bool{false, true} {
		destination := MemoryDestination{ConnectionRef: at.Seq}
		if trusted {
			destination = MemoryDestination{BuiltIn: true}
		}
		read := db.BindMemoryRead("sam", "memory-agent", destination)
		records, err := db.RetrieveMemory(MemoryQuery{Reader: "sam", Butler: "memory-agent", ReadContext: read})
		if trusted {
			if err != nil || len(records) != 1 {
				t.Fatalf("trusted: %v %v", records, err)
			}
		} else if !errors.Is(err, ErrPrivacy) || len(records) != 0 {
			t.Fatalf("external: %v %v", records, err)
		}
		hits, err := db.SearchIndex(IndexQuery{Reader: "sam", Butler: "memory-agent", Text: "jasmine", ReadContext: read})
		if trusted {
			if err != nil || len(hits) != 1 {
				t.Fatalf("trusted recall: %v %v", hits, err)
			}
		} else if !errors.Is(err, ErrPrivacy) || len(hits) != 0 {
			t.Fatalf("external recall: %v %v", hits, err)
		}
	}
}
func TestFallbackNeverWidensProcessingOrDisclosure(t *testing.T) {
	db, at, task := disclosureFixture(t)
	seq := disclosedMemory(t, db, task)
	if err := db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{ModelLocal, ModelCloud, ""} {
		for _, trusted := range []bool{false, true} {
			d := MemoryDestination{ConnectionRef: at.Seq}
			if trusted {
				d = MemoryDestination{BuiltIn: true}
			}
			ctx, p, err := db.BeginPrivacyTurn(context.Background(), model, d, "")
			if err != nil {
				t.Fatal(err)
			}
			TrackMemoryPrivacy(ctx, seq, nil)
			err = p.Check()
			if model == ModelLocal && trusted {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrPrivacy) {
				t.Fatalf("widened %q trusted=%v: %v", model, trusted, err)
			}
		}
	}
}

func TestRememberedRestrictionConstrainsItsSourceConversation(t *testing.T) {
	for _, source := range []string{"task", "session", "group-task", "group-session"} {
		t.Run(source, func(t *testing.T) {
			db, at, task := disclosureFixture(t)
			session, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "sam", Butler: "memory-agent", Surface: ButlerSurfaceFamily, Mode: ButlerModeFull})
			if err != nil {
				t.Fatal(err)
			}
			if err = db.SetButlerTaskSession(task, session); err != nil {
				t.Fatal(err)
			}
			sourceID := task
			if strings.HasSuffix(source, "session") {
				sourceID = session
			}
			m := scopedFact(t, MemoryScopePersonal, "memory-agent", "sam")
			m.SourceKind = "conversation"
			actor := "sam"
			if strings.HasPrefix(source, "group-") {
				m = scopedFact(t, MemoryScopeHousehold, "memory-agent", HouseholdOwner)
				m.SourceKind = "group-conversation"
				actor = "mika"
			}
			m.SourceID = sourceID
			seq, err := db.RecordMemory(m)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{ConnectionRef: at.Seq}); err != nil {
				t.Fatal(err)
			}
			if err = db.SetMemoryPrivacy(actor, seq, PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
				t.Fatal(err)
			}
			if err = db.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
				t.Fatalf("source history sent to cloud: %v", err)
			}
			if err = db.CheckSessionPrivacy(session, ModelLocal, MemoryDestination{ConnectionRef: at.Seq}); !errors.Is(err, ErrPrivacy) {
				t.Fatalf("source history sent externally: %v", err)
			}
			if err = db.CheckSessionPrivacy(session, ModelLocal, MemoryDestination{BuiltIn: true}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrivacyPatchDoesNotMakeInheritedRestrictionsPermanent(t *testing.T) {
	db, _, task := disclosureFixture(t)
	source := disclosedMemory(t, db, task)
	if err := db.SetMemoryPrivacy("sam", source, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetMemory(source)
	if err != nil {
		t.Fatal(err)
	}
	child, err := db.DeriveMemory(m, []int64{source})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := db.PatchMemoryPrivacy("sam", child, PrivacyPatch{TrustedOnly: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMemoryPrivacy("sam", source, PrivacyPolicy{}); err != nil {
		t.Fatal(err)
	}
	p, err := db.MemoryPrivacy(child)
	if err != nil || p.LocalOnly || !p.TrustedOnly {
		t.Fatalf("inherited flag became direct: %+v %v", p, err)
	}
}

func TestTurnTaskHandlePreservesPrivacyWithoutSecondaryLink(t *testing.T) {
	db, _, task := disclosureFixture(t)
	seq := disclosedMemory(t, db, task)
	if err := db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	session, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "sam", Butler: "memory-agent", Surface: ButlerSurfaceFamily, Mode: ButlerModeFull, Turns: []ButlerTurn{{Role: ButlerRoleUser, Text: "remember", TaskID: task}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("orphaned privacy: %v", err)
	}
	got, err := db.ButlerTaskForSession(session)
	if err != nil || got.ID != task {
		t.Fatalf("orphaned work: %+v %v", got, err)
	}
}

func TestPersonalExportIncludesEffectivePrivacy(t *testing.T) {
	db, _, task := disclosureFixture(t)
	source := disclosedMemory(t, db, task)
	if err := db.SetMemoryPrivacy("sam", source, PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetMemory(source)
	if err != nil {
		t.Fatal(err)
	}
	child, err := db.DeriveMemory(m, []int64{source})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.ExportMemoryFor("sam")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, row := range rows {
		if row.Seq == source || row.Seq == child {
			seen++
			if !row.Privacy.LocalOnly || !row.Privacy.TrustedOnly {
				t.Fatalf("export lost restrictions: %+v", row.Privacy)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("expected source and derivative: %d", seen)
	}
}

func TestCurrentTaskWritesRecheckedBeforeProcessingAndDelivery(t *testing.T) {
	for _, kind := range []string{"conversation", "group-conversation"} {
		t.Run(kind, func(t *testing.T) {
			db, _, task := disclosureFixture(t)
			ctx, turn, err := db.BeginPrivacyTurn(context.Background(), ModelCloud, MemoryDestination{BuiltIn: true}, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = turn.SetTask(task); err != nil {
				t.Fatal(err)
			}
			m := scopedFact(t, MemoryScopePersonal, "memory-agent", "sam")
			m.SourceKind, m.SourceID = kind, task
			seq, err := db.RecordMemory(m)
			if err != nil {
				t.Fatal(err)
			}
			if err = turn.Check(); err != nil {
				t.Fatal(err)
			}
			if err = db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{LocalOnly: true}); err != nil {
				t.Fatal(err)
			}
			if err = turn.Check(); !errors.Is(err, ErrPrivacy) {
				t.Fatalf("current write reached cloud: %v", err)
			}
			if err = db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{TrustedOnly: true}); err != nil {
				t.Fatal(err)
			}
			if err = turn.Check(); err != nil {
				t.Fatal(err)
			}
			if err = CheckPrivacyDelivery(ctx, MemoryDestination{}); !errors.Is(err, ErrPrivacy) {
				t.Fatalf("current write disclosed externally: %v", err)
			}
		})
	}
}

func TestPrivacyMigrationProtectsUntrackedHistoricalRecall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.migrateThrough("migrations/0069_household_secrets.sql"); err != nil {
		t.Fatal(err)
	}
	for _, member := range []struct{ name, role string }{{"mika", RoleOwner}, {"sam", RoleMember}} {
		if err = db.RecordButlerMember(member.name, member.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err = db.AdmitButlerMember(member.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.UpsertModel(Model{Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", ProviderModel: "fixture", Readiness: ModelReady, Enabled: true}, "mika"); err != nil {
		t.Fatal(err)
	}
	newButler(t, db, "memory-agent", "kitchen-box")
	oldFact := scopedFact(t, MemoryScopePersonal, "memory-agent", "sam")
	oldFact.SourceID = "bs-900"
	private, err := db.RecordMemory(oldFact)
	if err != nil {
		t.Fatal(err)
	}
	otherFact := scopedFact(t, MemoryScopePersonal, "memory-agent", "mika")
	otherFact.SourceID = "bs-901"
	unrelated, err := db.RecordMemory(otherFact)
	if err != nil {
		t.Fatal(err)
	}
	session, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "sam", Butler: "memory-agent", Surface: ButlerSurfaceFamily, Mode: ButlerModeFull, FinalText: "historically recalled personal fact"})
	if err != nil {
		t.Fatal(err)
	}
	// Seed the historical schema directly: future task writers may require later columns.
	result, err := db.sql.Exec(`INSERT INTO butler_tasks(created_at,updated_at,kind,state,author_person_id,owner_person_id,surface,agent_class,audience,objective,completion_criteria,retention,envelope_version)
 SELECT ?,?,'conversation','pending',person_id,person_id,'family','member','aud_owner','recall a fact','','conversation',? FROM butler_members WHERE member='sam'`, now(), now(), TaskEnvelopeVersion)
	if err != nil {
		t.Fatal(err)
	}
	taskSeq, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	taskID := fmt.Sprintf("bt-%d", taskSeq)
	if _, err = db.AcquireCoordinator("legacy-privacy-upgrade"); err != nil {
		t.Fatal(err)
	}
	oldAt := connect(t, db, "legacy-push", "telegram", ButlerSurfaceFamily)
	oldPush, err := db.EnqueueChatOutbound(ChatOutbound{Connection: oldAt.Seq, Target: "room", Kind: "push", Body: "old recalled fact", DedupeKey: "old-push"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.migrate(); err != nil {
		t.Fatal(err)
	}
	messages, err := db.ChatOutboundAfter(oldAt, 0, 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("unknown legacy push survived upgrade: %v %v", messages, err)
	}
	fresh := mustEnqueue(t, db, ChatOutbound{Connection: oldAt.Seq, Target: "room", Kind: "push", Body: "new ordinary notification", DedupeKey: "new-push"})
	messages, err = db.ChatOutboundAfter(oldAt, oldPush, 10)
	if err != nil || len(messages) != 1 || messages[0].Seq != fresh {
		t.Fatalf("new notifications blocked: %v %v", messages, err)
	}
	if err = db.SetMemoryPrivacy("mika", unrelated, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err = db.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{BuiltIn: true}); err != nil {
		t.Fatalf("unrelated Person restricted this session: %v", err)
	}
	if err = db.SetMemoryPrivacy("sam", private, PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err = db.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("historical recall leaked to cloud: %v", err)
	}
	if err = db.CheckSessionPrivacy(session, ModelLocal, MemoryDestination{}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("historical recall leaked externally: %v", err)
	}
	if err = db.CheckSessionPrivacy(session, ModelLocal, MemoryDestination{BuiltIn: true}); err != nil {
		t.Fatal(err)
	}
	_, turn, err := db.BeginPrivacyTurn(context.Background(), ModelCloud, MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = turn.SetTask(taskID); err != nil {
		t.Fatal(err)
	}
	if err = turn.Check(); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("pre-migration task cached recall reached cloud: %v", err)
	}
}

// TestPrivacyMigrationLinksPlanSessionSourcesFromExistingTurns covers
// migration 0070's plan_session_sources backfill against a pre-existing
// butler_session_turns.plan_ids value. That column is a comma-joined list
// of "plan-<seq>" strings (see insertTurns / scanButlerTurn), never JSON,
// so a deployment with real history exercises this in a way a fixture
// built only through the current schema does not.
func TestPrivacyMigrationLinksPlanSessionSourcesFromExistingTurns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.migrateThrough("migrations/0069_household_secrets.sql"); err != nil {
		t.Fatal(err)
	}
	planID, err := db.InsertPlan("butler", "mika", "pre-migration proposal", "low", "{}")
	if err != nil {
		t.Fatal(err)
	}
	secondPlanID, err := db.InsertPlan("butler", "mika", "another proposal", "low", "{}")
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := db.InsertButlerSession(ButlerSession{
		Principal: "butler", ActingFor: "mika", Surface: ButlerSurfaceFamily, Mode: ButlerModeFull,
		FinalText: "proposed something",
		Turns: []ButlerTurn{
			{Role: ButlerRoleUser, Text: "do something"},
			{Role: ButlerRoleAssistant, Text: "done", PlanIDs: []string{planID, secondPlanID, "plan-999999", "plan-01"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.migrate(); err != nil {
		t.Fatal(err)
	}
	var linked int
	planSeq := strings.TrimPrefix(planID, "plan-")
	sessionSeq := strings.TrimPrefix(sessionID, "bs-")
	if err = db.sql.QueryRow(
		`SELECT count(*) FROM plan_session_sources WHERE plan=? AND session=?`, planSeq, sessionSeq,
	).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("migration 0070 did not link plan %s to session %s (plan_ids is comma-separated, not JSON)", planID, sessionID)
	}
	if err = db.sql.QueryRow(`SELECT count(*) FROM plan_session_sources WHERE session=?`, sessionSeq).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 2 {
		t.Fatalf("migration linked %d plans, want only the two canonical existing IDs", linked)
	}
}

func TestQueuedReplyRechecksPrivacyAtAdapterPoll(t *testing.T) {
	db, at, _ := chatTestDB(t)
	if err := db.RecordButlerMember("mika", RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	session, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "mika", Provider: "test", Mode: ButlerModeFull, Surface: ButlerSurfaceFamily})
	if err != nil {
		t.Fatal(err)
	}
	record, err := db.GetButlerSession(session)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := db.RecordMemory(MemoryRecord{Scope: MemoryScopePersonal, Kind: "fact", Content: "jasmine", Owner: "mika", Author: "mika", Audience: ButlerAudiencePrivate, PermissionLabel: "mika", SourceKind: "conversation", SourceID: session})
	if err != nil {
		t.Fatal(err)
	}
	// The field is populated by the reply producer, before the adapter is online.
	outbound := mustEnqueue(t, db, ChatOutbound{Connection: at.Seq, Target: "room", Body: "jasmine", DedupeKey: "privacy-queued", PrivacySession: record.Seq})

	_ = outbound
	messages, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 1 || messages[0].Body != "jasmine" {
		t.Fatalf("ordinary reply lost: %v %v", messages, err)
	}
	if err = db.SetMemoryPrivacy("mika", seq, PrivacyPolicy{TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	messages, err = db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 1 || strings.Contains(messages[0].Body, "jasmine") {
		t.Fatalf("queued protected reply disclosed: %v %v", messages, err)
	}
}

func TestQueuedTaskPushRechecksPrivacyAtAdapterPoll(t *testing.T) {
	db, at, task := disclosureFixture(t)
	if _, err := db.AcquireCoordinator("outbound-privacy-test"); err != nil {
		t.Fatal(err)
	}
	seq := disclosedMemory(t, db, task)
	ctx, guard, err := db.BeginPrivacyTurn(context.Background(), ModelLocal, MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = guard.SetTask(task); err != nil {
		t.Fatal(err)
	}
	outbound, err := WithChatOutboundPrivacy(ctx, ChatOutbound{Connection: at.Seq, Target: "room", Body: "jasmine", Kind: "push", DedupeKey: "privacy-push"})
	if err != nil {
		t.Fatal(err)
	}
	mustEnqueue(t, db, outbound)
	messages, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 1 || messages[0].Body != "jasmine" {
		t.Fatalf("positive push: %v %v", messages, err)
	}
	if err = db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	messages, err = db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 1 || strings.Contains(messages[0].Body, "jasmine") {
		t.Fatalf("queued task context leaked: %v %v", messages, err)
	}
}

func TestLegacyFirstTurnDenialStillPersistsSources(t *testing.T) {
	db, _, task := disclosureFixture(t)
	seq := disclosedMemory(t, db, task)
	ctx, p, err := db.BeginPrivacyTurn(context.Background(), ModelCloud, MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = TrackMemoryPrivacy(ctx, seq, nil); err != nil {
		t.Fatal(err)
	}
	if err = p.Check(); err != nil {
		t.Fatal(err)
	}
	session, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "sam", Butler: "memory-agent", Surface: ButlerSurfaceFamily, Mode: ButlerModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err = p.SetSession(session); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("changed privacy not denied: %v", err)
	}
	if err = db.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("denial orphaned transcript provenance: %v", err)
	}
}

func TestQueuedLegacyFirstTurnRetainsMemorySources(t *testing.T) {
	db, at, task := disclosureFixture(t)
	if _, err := db.AcquireCoordinator("outbound-privacy-test"); err != nil {
		t.Fatal(err)
	}
	seq := disclosedMemory(t, db, task)
	ctx, _, err := db.BeginPrivacyTurn(context.Background(), ModelLocal, MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = TrackMemoryPrivacy(ctx, seq, nil); err != nil {
		t.Fatal(err)
	}
	outbound, err := WithChatOutboundPrivacy(ctx, ChatOutbound{Connection: at.Seq, Target: "room", Body: "jasmine", Kind: "push", DedupeKey: "privacy-push"})
	if err != nil {
		t.Fatal(err)
	}
	mustEnqueue(t, db, outbound)
	messages, err := db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 1 || messages[0].Body != "jasmine" {
		t.Fatalf("positive push: %v %v", messages, err)
	}
	if err = db.SetMemoryPrivacy("sam", seq, PrivacyPolicy{TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	messages, err = db.ChatOutboundAfter(at, 0, 10)
	if err != nil || len(messages) != 1 || strings.Contains(messages[0].Body, "jasmine") {
		t.Fatalf("queued task context leaked: %v %v", messages, err)
	}
}

func TestTaskRetentionPreservesSessionMemoryPrivacy(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprint(linked), func(t *testing.T) {
			db, at, task := disclosureFixture(t)
			memory := disclosedMemory(t, db, task)
			session, err := db.InsertButlerSession(ButlerSession{Principal: "butler", ActingFor: "sam", Butler: "memory-agent", Surface: ButlerSurfaceFamily, Mode: ButlerModeFull, Turns: []ButlerTurn{{Role: ButlerRoleUser, Text: "remember", TaskID: task}}})
			if err != nil {
				t.Fatal(err)
			}
			if linked {
				if err = db.SetButlerTaskSession(task, session); err != nil {
					t.Fatal(err)
				}
			}
			other := disclosedMemory(t, db, session)
			if _, err = db.sql.Exec("DELETE FROM butler_task_transitions WHERE task=CAST(substr(?,4) AS INTEGER)", task); err != nil {
				t.Fatal(err)
			}
			if _, err = db.sql.Exec("DELETE FROM butler_tasks WHERE seq=CAST(substr(?,4) AS INTEGER)", task); err != nil {
				t.Fatal(err)
			}
			if err = db.CheckSessionPrivacy(session, ModelCloud, MemoryDestination{ConnectionRef: at.Seq}); err != nil {
				t.Fatalf("unrestricted retained history: %v", err)
			}
			if err = db.SetMemoryPrivacy("sam", memory, PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
				t.Fatal(err)
			}
			for _, check := range []struct {
				model       string
				destination MemoryDestination
			}{{ModelCloud, MemoryDestination{BuiltIn: true}}, {ModelLocal, MemoryDestination{ConnectionRef: at.Seq}}} {
				if err = db.CheckSessionPrivacy(session, check.model, check.destination); !errors.Is(err, ErrPrivacy) {
					t.Errorf("retention erased session provenance: %v", err)
				}
			}
			if err = db.SetMemoryPrivacy("sam", memory, PrivacyPolicy{}); err != nil {
				t.Fatal(err)
			}
			if err = db.SetMemoryPrivacy("sam", other, PrivacyPolicy{LocalOnly: true, TrustedOnly: true}); err != nil {
				t.Fatal(err)
			}
			if err = db.CheckMemoryPrivacy(memory, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
				t.Errorf("retention erased memory's source history: %v", err)
			}
		})
	}
}

func TestTaskRetentionPreservesMemoryReadSourcesWithoutSession(t *testing.T) {
	db, _, task := disclosureFixture(t)
	source := disclosedMemory(t, db, "independent-source")
	ctx, guard, err := db.BeginPrivacyTurn(context.Background(), ModelLocal, MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = guard.SetTask(task); err != nil {
		t.Fatal(err)
	}
	if err = TrackMemoryPrivacy(ctx, source, nil); err != nil {
		t.Fatal(err)
	}
	if err = guard.Check(); err != nil {
		t.Fatal(err)
	}
	written := disclosedMemory(t, db, task)
	if _, err = db.sql.Exec("DELETE FROM butler_task_transitions WHERE task=CAST(substr(?,4) AS INTEGER)", task); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec("DELETE FROM butler_tasks WHERE seq=CAST(substr(?,4) AS INTEGER)", task); err != nil {
		t.Fatal(err)
	}
	if err = db.SetMemoryPrivacy("sam", source, PrivacyPolicy{LocalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err = db.CheckMemoryPrivacy(written, ModelCloud, MemoryDestination{BuiltIn: true}); !errors.Is(err, ErrPrivacy) {
		t.Fatalf("retention erased recalled sources: %v", err)
	}
}
