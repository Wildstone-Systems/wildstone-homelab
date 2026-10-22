package runtimedb

// The combined Part C acceptance suite (ADR-W030 P16).
//
// Part C ends with a list of things implementation PRs must prove. Each
// package proved its own along the way; this runs the list as one
// sequence against one database, because the interesting failures are
// between packages — an offboarding that denies access in six places at
// six different times is six windows, and a restore that reconciles
// memory but not admission is a person talking to the Butler again.
//
// It is deliberately one long test rather than several. The sequence is
// the point: each step depends on the state the last one left.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPartCAcceptance(t *testing.T) {
	db := open(t)
	const admin = "mika"

	// ---- setup: two admitted adults, some work, some memory, a
	// connection, a routine.
	for _, m := range []struct{ name, role string }{
		{admin, RoleOwner}, {"sam", RoleMember},
	} {
		if err := db.RecordButlerMember(m.name, m.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(m.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := db.LinkSurfaceIdentity(LinkAPIPrincipal, m.name, m.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}

	personal := newAcceptanceTask(t, db, "sam", ButlerAudiencePrivate, "sort out my thing")
	shared := newAcceptanceTask(t, db, "sam", ButlerAudienceShared, "book the school thing")
	privateMemory, err := db.RecordMemory(MemoryRecord{
		Kind: MemoryFact, Subject: "sam", Content: "sam's therapy is on Tuesdays",
		Audience: ButlerAudiencePrivate, PermissionLabel: "sam", Owner: "sam", Author: "sam",
		SourceKind: "conversation", SourceID: "bs-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.IndexDocumentInto(IndexDocument{
		SourceKind: "memory", SourceID: "m1", MemorySeq: privateMemory, Owner: "sam",
		Audience: ButlerAudiencePrivate, PermissionLabel: "sam",
		Content: "sam's therapy is on Tuesdays", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateConnection(IntegrationConnection{
		Name: "sams-calendar", Adapter: "caldav", Ownership: OwnershipPersonal,
		Owner: "sam", CreatedBy: "root-admin",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GrantTool(ToolGrant{
		Scope: GrantScopeTask, ScopeID: personal, AgentClass: AgentClassMember,
		Tool: "restricted", GrantedBy: "root-admin",
	}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RaiseNotice(NoticeRequest{
		Kind: "task-update", Recipient: "sam", DedupeKey: "n-sam-1",
	}, DefaultButlerSettings(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// ---- 1. Offboarding denies access across surfaces and nodes,
	// pauses personal work, transfers shared ownership without changing
	// authorship, and quarantines private data.
	t.Run("offboarding lands everywhere at once", func(t *testing.T) {
		result, err := db.OffboardMember("sam", "root-admin", admin)
		if err != nil {
			t.Fatalf("OffboardMember: %v", err)
		}

		// Denied, immediately and everywhere an identity resolves.
		if _, err := db.ResolveSurfaceIdentity(LinkAPIPrincipal, "sam"); !errors.Is(err, ErrNotAdmitted) {
			t.Fatalf("an offboarded member still resolves: %v", err)
		}
		if _, err := db.StartButlerConversation("sam", "hello"); !errors.Is(err, ErrNotAdmitted) {
			t.Fatal("an offboarded member started a conversation")
		}

		// Personal work paused, nothing deleted.
		task, err := db.GetButlerTask(personal)
		if err != nil {
			t.Fatal(err)
		}
		if task.State != ButlerTaskPausedOffboarded {
			t.Fatalf("personal work = %q, want paused", task.State)
		}
		if task.Objective == "" {
			t.Fatal("personal work was deleted rather than paused")
		}

		// Shared work transferred, authorship intact.
		sharedTask, err := db.GetButlerTask(shared)
		if err != nil {
			t.Fatal(err)
		}
		if sharedTask.Owner != admin {
			t.Fatalf("shared work is owned by %q, want the administrator", sharedTask.Owner)
		}
		if sharedTask.Author != "sam" {
			t.Fatalf("shared work's author became %q — the history now lies about who asked", sharedTask.Author)
		}
		if sharedTask.State != ButlerTaskPausedOffboarded {
			t.Fatalf("transferred work = %q; A4 says it pauses for grant review", sharedTask.State)
		}

		// Grants ended.
		grants, err := db.ListToolGrants(GrantScopeTask, personal)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range grants {
			if g.RevokedAt.IsZero() {
				t.Fatalf("a grant survived the offboarding: %+v", g)
			}
		}

		// Private data quarantined and unsearchable.
		m, err := db.GetMemory(privateMemory)
		if err != nil {
			t.Fatal(err)
		}
		if m.ForgottenAt.IsZero() {
			t.Fatal("private memory survived the offboarding")
		}
		if hits, _ := db.SearchIndex(IndexQuery{Reader: admin, Text: "therapy"}); len(hits) != 0 {
			t.Fatalf("private material is still searchable: %+v", hits)
		}

		// The personal connection is fenced.
		conn, err := db.GetConnection("sams-calendar")
		if err != nil {
			t.Fatal(err)
		}
		if conn.State != ConnectionUnavailable {
			t.Fatalf("a personal connection = %q after its owner left", conn.State)
		}

		// And there is an answer per store, rather than one claim.
		if len(result.Stores) < 5 {
			t.Fatalf("offboarding reported %d stores: %+v", len(result.Stores), result.Stores)
		}
		records, err := db.ListOffboardingRecords("sam")
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 0 {
			t.Fatal("the offboarding left no record of what it did")
		}
	})

	// ---- 2. A restore of an older snapshot cannot revive deleted or
	// revoked content, and cannot resume uncertain work.
	t.Run("a restore cannot revive what was removed", func(t *testing.T) {
		// Simulate the restore: the snapshot predates the offboarding,
		// so the restored rows say sam is admitted and their memory is
		// live. The journal is what knows better.
		if _, err := db.ExecForTest(
			`UPDATE butler_members SET admitted = 1, offboarded_at = '', offboarded_by = '' WHERE member = 'sam'`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecForTest(
			`UPDATE butler_memory SET forgotten_at = '', forgotten_by = '' WHERE seq = ?`, privateMemory); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecForTest(
			`UPDATE butler_identity_links SET revoked_at = '', revoked_by = ''
			 WHERE person_id = (SELECT person_id FROM butler_members WHERE member = 'sam')`); err != nil {
			t.Fatal(err)
		}

		if err := db.EnterRestoreQuarantine("restored from the nightly backup",
			time.Now().Add(-24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		// Nothing Butler-facing runs while quarantine holds.
		if err := db.CheckQuarantine(); !errors.Is(err, ErrQuarantined) {
			t.Fatalf("a restored database was not quarantined: %v", err)
		}

		// Release requires BOTH the assertion and the replay. An
		// administrator's word alone is not evidence.
		if err := db.ReleaseRestoreQuarantine("root-admin"); err == nil {
			t.Fatal("quarantine was released with neither assertion nor replay")
		}
		if err := db.AssertJournalCurrent("root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := db.ReleaseRestoreQuarantine("root-admin"); err == nil {
			t.Fatal("quarantine was released on an administrator's word alone")
		}

		applied, err := db.ReconcileRestore()
		if err != nil {
			t.Fatalf("ReconcileRestore: %v", err)
		}
		if applied == 0 {
			t.Fatal("reconciliation applied nothing, so it proved nothing")
		}
		if err := db.ReleaseRestoreQuarantine("root-admin"); err != nil {
			t.Fatalf("releasing after a full reconciliation: %v", err)
		}

		// The offboarding held through the restore.
		if _, err := db.ResolveSurfaceIdentity(LinkAPIPrincipal, "sam"); !errors.Is(err, ErrNotAdmitted) {
			t.Fatal("a restore revived an offboarded member's access")
		}
		m, err := db.GetMemory(privateMemory)
		if err != nil {
			t.Fatal(err)
		}
		if m.ForgottenAt.IsZero() {
			t.Fatal("a restore revived deleted private memory")
		}
		if hits, _ := db.SearchIndex(IndexQuery{Reader: admin, Text: "therapy"}); len(hits) != 0 {
			t.Fatal("a restore made deleted material searchable again")
		}
		state, err := db.GetRestoreState()
		if err != nil {
			t.Fatal(err)
		}
		if state.Quarantined || state.ReplayedCount == 0 {
			t.Fatalf("restore state = %+v", state)
		}
	})

	// ---- 3. Uncertain work stays paused across a restore.
	t.Run("uncertain work is not resumed by a restore", func(t *testing.T) {
		uncertain := newAcceptanceTask(t, db, admin, ButlerAudiencePrivate, "did that send?")
		if err := db.TransitionButlerTask(uncertain, ButlerTaskRunning, "", ""); err != nil {
			t.Fatal(err)
		}
		step, err := db.AppendButlerTaskStep(uncertain, ButlerTaskStep{
			Kind: ButlerStepTool, Name: "send_message",
		})
		if err != nil {
			t.Fatal(err)
		}
		// The attempt went out and never came back: the outcome is
		// unknown, which is exactly what must not be retried.
		if _, err := db.StartButlerAttempt(step); err != nil {
			t.Fatal(err)
		}

		if err := db.EnterRestoreQuarantine("restored again", time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := db.AssertJournalCurrent("root-admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ReconcileRestore(); err != nil {
			t.Fatal(err)
		}
		if err := db.ReleaseRestoreQuarantine("root-admin"); err != nil {
			t.Fatal(err)
		}

		task, err := db.GetButlerTask(uncertain)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == ButlerTaskRunning {
			t.Fatal("a restore resumed work whose outcome was unknown")
		}
	})

	// ---- 4. The previous coordinator cannot dispatch after promotion.
	t.Run("promotion fences the previous coordinator", func(t *testing.T) {
		if _, err := db.AcquireCoordinator("keystone-a"); err != nil {
			t.Fatal(err)
		}
		if err := db.CheckCoordinator("keystone-a", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PromoteCoordinator("keystone-b"); err != nil {
			t.Fatal(err)
		}
		if err := db.CheckCoordinator("keystone-a", 1); !errors.Is(err, ErrCoordinatorFenced) {
			t.Fatalf("the previous coordinator can still dispatch: %v", err)
		}
	})

	// ---- 5. The standing refusals still hold, end to end.
	t.Run("the standing refusals hold", func(t *testing.T) {
		// A household tool ceiling cannot be widened by a control.
		if err := db.SetToolControl(AgentClassMember, "propose_change", ToolEnabled, "root-admin", ""); err != nil {
			t.Fatal(err)
		}
		d, err := db.ResolveToolAuthority(ToolQuery{
			AgentClass: AgentClassMember, Tool: "propose_change", Structural: false,
		})
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			t.Fatal("a control widened the household ceiling")
		}
		// Local-only material stays local. It is no longer a router
		// refusing a request: a Butler is assigned a model, and
		// material given to a local-model Butler is not reachable by a
		// cloud-model one (ADR-W031 A2/A4).
		for _, r := range []Model{
			{Name: "kitchen-box", Kind: ModelLocal, Provider: "ollama", Readiness: ModelReady, Enabled: true},
			{Name: "cloud", Kind: ModelCloud, Provider: "anthropic", Readiness: ModelReady, Enabled: true},
		} {
			if err := db.UpsertModel(r, "root-admin"); err != nil {
				t.Fatal(err)
			}
		}
		for _, b := range []struct{ name, route string }{
			{"in-house", "kitchen-box"}, {"outside", "cloud"},
		} {
			if _, err := db.CreateButler(Butler{
				Name: b.name, Surface: ButlerSurfaceFamily, AgentClass: AgentClassMember, Model: b.route,
			}, admin); err != nil {
				t.Fatal(err)
			}
		}
		held, err := db.RecordButlerMemory("in-house", MemoryRecord{
			Kind: MemoryFact, Subject: admin, Content: "material that must not leave",
			Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
			Author: admin, SourceKind: "conversation", SourceID: "acc-local",
		})
		if err != nil {
			t.Fatal(err)
		}
		reached, err := db.RetrieveMemory(MemoryQuery{Reader: admin, Butler: "outside"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range reached {
			if r.Seq == held {
				t.Fatal("local-only material reached a cloud-model butler")
			}
		}
		if own, _ := db.RetrieveMemory(MemoryQuery{Reader: admin, Butler: "in-house"}); len(own) == 0 {
			t.Fatal("the butler holding the material could not read it, so the check above proves nothing")
		}
		// A task cannot complete without evidence.
		bare := newAcceptanceTask(t, db, admin, ButlerAudiencePrivate, "nothing")
		if err := db.TransitionButlerTask(bare, ButlerTaskCompleted, "", ""); err == nil {
			t.Fatal("a task completed with no evidence")
		}
		// Silence is not approval.
		raised, err := db.RaiseNotice(NoticeRequest{
			Kind: "approval", Recipient: admin, DedupeKey: "acc-1", Asks: true,
			TTL: time.Millisecond,
		}, DefaultButlerSettings(), time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExpireNotices(time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AnswerNotice(raised.Seq, "yes", admin); !errors.Is(err, ErrNoticeNotAnswerable) {
			t.Fatal("an expired question was answered")
		}
	})
}

func newAcceptanceTask(t *testing.T, db *DB, owner string, audience ButlerTaskAudience, objective string) string {
	t.Helper()
	surface, profile := ButlerSurfaceFamily, AgentClassMember
	if owner == "mika" {
		surface, profile = ButlerSurfaceAdmin, AgentClassOwner
	}
	id, err := db.InsertButlerTask(ButlerTask{
		Kind: ButlerTaskConversation, Author: owner, Owner: owner,
		Surface: surface, AgentClass: profile, Audience: audience,
		Objective: objective, CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	return id
}

// The quarantine gate is checked, not assumed: a database that was
// never restored is not quarantined, and that ordinary case must not
// read as an error.
func TestQuarantineIsAbsentUntilARestore(t *testing.T) {
	db := open(t)
	if err := db.CheckQuarantine(); err != nil {
		t.Fatalf("a database that was never restored reported %v", err)
	}
	state, err := db.GetRestoreState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Quarantined {
		t.Fatal("a fresh database is quarantined")
	}
	if err := db.EnterRestoreQuarantine("", time.Now()); err == nil {
		t.Fatal("quarantine was entered with no reason recorded")
	}
	if err := db.EnterRestoreQuarantine("nightly backup", time.Now()); err != nil {
		t.Fatal(err)
	}
	err = db.CheckQuarantine()
	if !errors.Is(err, ErrQuarantined) {
		t.Fatalf("CheckQuarantine = %v", err)
	}
	// The message says what was restored, so an operator reading it a
	// week later knows which snapshot they are looking at.
	if !strings.Contains(err.Error(), "nightly backup") {
		t.Fatalf("the quarantine message does not say what was restored: %v", err)
	}
	// And the schema refuses a release with only an assertion, for any
	// writer.
	if err := db.AssertJournalCurrent("root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecForTest(
		`UPDATE restore_state SET released_at = 't', quarantined = 0 WHERE id = 1`); err == nil {
		t.Fatal("a direct write released quarantine without a replay")
	}
}
