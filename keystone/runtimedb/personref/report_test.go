package personref_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb/personref"
)

// surveyed builds a database, lets the caller write realistic legacy
// rows into it, and then surveys it through a READ-ONLY handle — the
// same way the CLI does, so the tests exercise the real guarantee.
func surveyed(t *testing.T, seed func(*runtimedb.DB)) (personref.Report, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := runtimedb.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if seed != nil {
		seed(db)
	}
	db.Close()

	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	defer ro.Close()
	rep, err := personref.Survey(ro)
	if err != nil {
		t.Fatalf("Survey: %v", err)
	}
	return rep, path
}

func find(t *testing.T, rep personref.Report, handle string) personref.Handle {
	t.Helper()
	for _, h := range rep.Handles {
		if h.Value == handle {
			return h
		}
	}
	t.Fatalf("the report never mentions %q", handle)
	return personref.Handle{}
}

// people adds Persons and admits them, which is what a real deployment
// looks like.
func people(t *testing.T, db *runtimedb.DB, handles ...string) {
	t.Helper()
	for _, h := range handles {
		if err := db.RecordButlerMember(h, runtimedb.RoleMember, "", ""); err != nil {
			t.Fatalf("RecordButlerMember(%s): %v", h, err)
		}
	}
}

// The core negative case: a dry run is a read.
func TestTheSurveyWritesNothing(t *testing.T) {
	var before []byte
	rep, path := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "sam")
		if _, err := db.RecordMemory(runtimedb.MemoryRecord{
			Kind: runtimedb.MemoryFact, Subject: "household", Content: "the bins go out on Tuesday",
			Audience: runtimedb.ButlerAudienceShared, PermissionLabel: "household",
			Owner: "sam", Author: "sam", SourceKind: "conversation", SourceID: "v-1",
		}); err != nil {
			t.Fatalf("RecordMemory: %v", err)
		}
	})
	if len(rep.Handles) == 0 {
		t.Fatal("the fixture produced no handles, so this proves nothing")
	}
	// Read the file AFTER the survey, and again, to show it is settled.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before = after
	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := personref.Survey(ro); err != nil {
		t.Fatalf("second Survey: %v", err)
	}
	ro.Close()
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(again) {
		t.Fatal("the database changed across a survey; a dry run must write nothing")
	}
}

// The negative case a literal reading of #203 gets wrong: machine
// principals are not missing people.
func TestMachinePrincipalActorsAreNotOrphans(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		// Every real deployment has this principal, and every real
		// deployment has rows it wrote.
		if err := db.CreatePrincipal("root-admin", runtimedb.RoleAdmin, ""); err != nil {
			t.Fatalf("CreatePrincipal: %v", err)
		}
		people(t, db, "mika")
		if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
			t.Fatalf("AdmitButlerMember: %v", err)
		}
	})

	h := find(t, rep, "root-admin")
	if h.Verdict != personref.MachinePrincipal {
		t.Errorf("root-admin is %q, want machine-principal", h.Verdict)
	}
	if h.Blocking() {
		t.Error("a machine principal in an actor column blocks the migration; " +
			"every deployment has these, so this would abort every real run")
	}
	if len(rep.Blocking()) != 0 {
		t.Errorf("blocking = %+v, want none for an ordinary deployment", rep.Blocking())
	}
}

// Case variants are reported for a decision, never merged.
func TestCaseVariantHandlesAreReportedNotMerged(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "Sam")
		if _, err := db.RecordMemory(runtimedb.MemoryRecord{
			Kind: runtimedb.MemoryFact, Subject: "sam", Content: "prefers oat milk",
			Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "personal",
			Owner: "sam", Author: "sam", SourceKind: "conversation", SourceID: "v-2",
		}); err != nil {
			t.Fatalf("RecordMemory: %v", err)
		}
	})

	h := find(t, rep, "sam")
	if h.Verdict != personref.CaseVariant {
		t.Fatalf("sam is %q, want case-variant against the Person 'Sam'", h.Verdict)
	}
	if h.PersonID != "" {
		t.Error("a case variant was resolved to a person_id; that is the silent merge this must never do")
	}
	if len(h.Candidates) != 1 || h.Candidates[0] != "Sam" {
		t.Errorf("candidates = %v, want the Person it nearly matches named for review", h.Candidates)
	}
	if !h.Blocking() {
		t.Error("an unresolved handle in a human column does not block; the real run would guess")
	}
}

// Two Persons under one folded handle: no merge, no pick.
func TestAmbiguousHandlesAreNeverPickedBetween(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		// Exactly the ADR-W032 A4 case: two rows, differing only by
		// case, that the schema accepts today.
		people(t, db, "Admin", "admin")
		if _, err := db.RecordMemory(runtimedb.MemoryRecord{
			Kind: runtimedb.MemoryFact, Subject: "household", Content: "the router password rotates monthly",
			Audience: runtimedb.ButlerAudienceShared, PermissionLabel: "household",
			Owner: "ADMIN", Author: "ADMIN", SourceKind: "conversation", SourceID: "v-3",
		}); err != nil {
			t.Fatalf("RecordMemory: %v", err)
		}
	})

	h := find(t, rep, "ADMIN")
	if h.Verdict != personref.Ambiguous {
		t.Fatalf("ADMIN is %q, want ambiguous — two Persons answer to it", h.Verdict)
	}
	if h.PersonID != "" {
		t.Error("an ambiguous handle was resolved; this is the wrong merge that would look like data afterwards")
	}
	if len(h.Candidates) != 2 {
		t.Errorf("candidates = %v, want both Persons named so an Owner can decide", h.Candidates)
	}
	if !h.Blocking() {
		t.Error("an ambiguous handle does not block the migration")
	}
}

// A handle with no Person and no principal, in a column that means a
// person, is an orphan and must stop the run.
func TestAnOrphanedHandleIsReportedNotInvented(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "mika")
		if _, err := db.RecordMemory(runtimedb.MemoryRecord{
			Kind: runtimedb.MemoryFact, Subject: "household", Content: "left by somebody who is gone",
			Audience: runtimedb.ButlerAudienceShared, PermissionLabel: "household",
			Owner: "ghost", Author: "ghost", SourceKind: "conversation", SourceID: "v-4",
		}); err != nil {
			t.Fatalf("RecordMemory: %v", err)
		}
	})

	h := find(t, rep, "ghost")
	if h.Verdict != personref.Orphan {
		t.Fatalf("ghost is %q, want orphan", h.Verdict)
	}
	if h.PersonID != "" {
		t.Error("an orphan was given a person_id — the migration invented an identity")
	}
	if !h.Blocking() {
		t.Error("an orphan in a human column does not block the migration")
	}
}

// The report says where a handle lives, which is what makes it
// reviewable rather than a verdict to take on trust.
func TestTheReportNamesEveryColumnAHandleAppearsIn(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "sam")
		if _, err := db.RecordMemory(runtimedb.MemoryRecord{
			Kind: runtimedb.MemoryFact, Subject: "household", Content: "a fact",
			Audience: runtimedb.ButlerAudienceShared, PermissionLabel: "household",
			Owner: "sam", Author: "sam", SourceKind: "conversation", SourceID: "v-5",
		}); err != nil {
			t.Fatalf("RecordMemory: %v", err)
		}
	})

	h := find(t, rep, "sam")
	if h.Verdict != personref.Resolved || h.PersonID == "" {
		t.Fatalf("sam is %q with person_id %q, want resolved", h.Verdict, h.PersonID)
	}
	var owner, author bool
	for _, u := range h.Uses {
		if u.Table != "butler_memory" {
			continue
		}
		owner = owner || u.Column == "owner"
		author = author || u.Column == "author"
		if u.Rows < 1 {
			t.Errorf("%s.%s reports %d rows", u.Table, u.Column, u.Rows)
		}
		want := personref.Human
		if u.Column == "owner" {
			// owner also takes the `household` scope; author never does.
			want = personref.Scoped
		}
		if u.Kind != want {
			t.Errorf("%s.%s is reported as %s, want %s", u.Table, u.Column, u.Kind, want)
		}
	}
	if !owner || !author {
		t.Errorf("uses = %+v, want both butler_memory.owner and .author", h.Uses)
	}
}

// A fresh install has nothing to migrate, and the run is a no-op.
func TestTheReportIsEmptyOnAFreshInstall(t *testing.T) {
	rep, _ := surveyed(t, nil)
	if len(rep.Blocking()) != 0 {
		t.Fatalf("a fresh install has %d blocking handle(s): %+v", len(rep.Blocking()), rep.Blocking())
	}
	for _, h := range rep.Handles {
		if h.InHumanColumn() {
			t.Errorf("a fresh install has %q in a human column: %+v", h.Value, h.Uses)
		}
	}
}

// An audit row's retained handle is not a reference to resolve.
//
// butler_role_changes keeps the person's handle beside person_id so the
// row stays readable after that Person is purged — which is exactly the
// case where resolving the handle is guaranteed to fail. Surveying it
// would turn every audit row of a departed person into a blocker.
func TestRetainedAuditTextIsNotSurveyed(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		if err := db.CreatePrincipal("root-admin", runtimedb.RoleAdmin, ""); err != nil {
			t.Fatalf("CreatePrincipal: %v", err)
		}
		people(t, db, "mika", "sam")
		for _, h := range []string{"mika", "sam"} {
			if err := db.AdmitButlerMember(h, "root-admin"); err != nil {
				t.Fatalf("AdmitButlerMember(%s): %v", h, err)
			}
		}
		// A promotion, which writes butler_role_changes.
		if err := db.SetButlerMemberRole("sam", runtimedb.RoleOwner, "root-admin"); err != nil {
			t.Fatalf("SetButlerMemberRole: %v", err)
		}
	})

	for _, h := range rep.Handles {
		for _, u := range h.Uses {
			if u.Table == "butler_role_changes" && u.Column == "member" {
				t.Fatalf("the survey read retained audit text (%s in butler_role_changes.member); "+
					"a purged person's audit rows would then block the migration", h.Value)
			}
		}
	}
	if len(rep.Blocking()) != 0 {
		t.Fatalf("an ordinary promotion left %d blocking handle(s): %+v", len(rep.Blocking()), rep.Blocking())
	}
}

// The corrected verdicts are load-bearing: each of these columns holds a
// person handle, and a wrong Actor verdict would leave slice 2b silently
// skipping it. Found by running the report against a seeded database and
// noticing a handle turning up somewhere the classification said it
// could not.
func TestColumnsThatMirrorAPersonAreClassifiedHuman(t *testing.T) {
	want := map[string]bool{
		"butler_memory.author": true,
		"butler_tasks.author":  true,
	}
	for _, c := range personref.Columns {
		key := c.Table + "." + c.Column
		if !want[key] {
			continue
		}
		if c.Kind != personref.Human {
			t.Errorf("%s is classified %s; it holds a person handle, and 2b would skip it", key, c.Kind)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("%s has no verdict at all", key)
	}
}

// And the converse, which #239's review had to teach me: a schema
// comment states an INTENT, and the write path decides what is actually
// in the column. Where they disagree, the writer wins.
//
// Both of these are documented as a person — "author is the person,
// never the Butler", "requested_by is the MEMBER the asking session was
// acting for" — and both are written with actingMember(id), which
// yields the machine principal name when a bare admin or operator token
// calls them. Their own tests exercise exactly that route. Classifying
// them Human would make every deployment that has used those endpoints
// fail the migration gate over a principal it was always allowed to
// record.
func TestColumnsWrittenByAnyPrincipalAreNotHuman(t *testing.T) {
	writtenByActingMember := map[string]string{
		"butler_task_feedback.author":             "api.addButlerTaskFeedback",
		"butler_capability_requests.requested_by": "api.fileCapabilityRequest",
		"integration_secrets.member":              "api.onboardIntegrationSecret (default)",
		"member_notice_preferences.member":        "api.setNoticePreferences (default)",
	}
	for _, c := range personref.Columns {
		key := c.Table + "." + c.Column
		writer, ok := writtenByActingMember[key]
		if !ok {
			continue
		}
		if c.Kind != personref.Actor {
			t.Errorf("%s is classified %s, but %s writes it with actingMember(id), "+
				"which is a machine principal name on a bare token", key, c.Kind, writer)
		}
		delete(writtenByActingMember, key)
	}
	for key := range writtenByActingMember {
		t.Errorf("%s has no verdict at all", key)
	}
}

// A name on BOTH identity axes is never resolved to the Person.
//
// butler_members.member is unique only within butler_members, and
// principals.name only within principals, so a Person may be called
// root-admin. Resolving before checking principals would attach a
// machine-written row to an unrelated human — and machine-written rows
// in human columns are not hypothetical: the legacy ask path puts them
// there (#240). That is the silent wrong merge this whole slice exists
// to prevent, so it has to be caught before resolution, not after.
func TestANameOnBothAxesIsNeverResolved(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		if err := db.CreatePrincipal("root-admin", runtimedb.RoleAdmin, ""); err != nil {
			t.Fatalf("CreatePrincipal: %v", err)
		}
		// A Person who happens to share the principal's name. Nothing
		// in either schema forbids it.
		people(t, db, "root-admin")
		// A row the legacy ask path would have written: machine origin,
		// human column.
		if _, err := db.RecordMemory(runtimedb.MemoryRecord{
			Kind: runtimedb.MemoryFact, Subject: "household", Content: "written by the machine path",
			Audience: runtimedb.ButlerAudienceShared, PermissionLabel: "household",
			Owner: "root-admin", Author: "root-admin", SourceKind: "conversation", SourceID: "v-8",
		}); err != nil {
			t.Fatalf("RecordMemory: %v", err)
		}
	})

	h := find(t, rep, "root-admin")
	if h.Verdict != personref.AlsoAPrincipal {
		t.Fatalf("root-admin is %q, want also-a-principal — a Person and a principal both answer to it", h.Verdict)
	}
	if h.PersonID != "" {
		t.Fatal("a name shared with a machine principal was attached to a Person; " +
			"that is the silent wrong merge, and afterwards it looks like data")
	}
	if !h.Blocking() {
		t.Fatal("a name on both axes does not block the migration")
	}
	if len(h.Candidates) != 1 || h.Candidates[0] != "root-admin" {
		t.Errorf("candidates = %v, want the Person named so an Owner can rename one of them", h.Candidates)
	}
}

// The converse, so the fix cannot pass by blocking everything: a
// principal whose name NO Person shares is still the ordinary,
// non-blocking case in an actor column.
func TestAPrincipalAloneIsStillNotBlocking(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		if err := db.CreatePrincipal("root-admin", runtimedb.RoleAdmin, ""); err != nil {
			t.Fatalf("CreatePrincipal: %v", err)
		}
		people(t, db, "mika")
		if err := db.AdmitButlerMember("mika", "root-admin"); err != nil {
			t.Fatalf("AdmitButlerMember: %v", err)
		}
	})
	h := find(t, rep, "root-admin")
	if h.Verdict != personref.MachinePrincipal {
		t.Fatalf("root-admin is %q, want machine-principal when no Person shares the name", h.Verdict)
	}
	if len(rep.Blocking()) != 0 {
		t.Fatalf("blocking = %+v, want none", rep.Blocking())
	}
}

// Household-shared memory does not block the migration.
//
// butler_memory.owner takes the literal `household` for a shared
// record — the schema says so in the sentence after the one about
// persons, which I had been quoting without — and butler_index.owner
// mirrors it. That is a supported, permanent mode, not a defect, so
// reporting it as an orphan would refuse to migrate every deployment
// that has any shared memory at all.
func TestHouseholdOwnedMemoryIsAScopeNotAnOrphan(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "mika")
		if _, err := db.RecordMemory(runtimedb.MemoryRecord{
			Kind: runtimedb.MemoryFact, Subject: "household", Content: "the bins go out on Tuesday",
			Audience: runtimedb.ButlerAudienceShared, PermissionLabel: "household",
			Owner: "household", Author: "mika", SourceKind: "conversation", SourceID: "v-10",
		}); err != nil {
			t.Fatalf("RecordMemory: %v", err)
		}
	})

	h := find(t, rep, "household")
	if h.Verdict != personref.Scope {
		t.Fatalf("household is %q, want scope — it is a supported owner, not a missing person", h.Verdict)
	}
	if h.PersonID != "" {
		t.Error("the household scope was resolved to a Person")
	}
	if h.Blocking() {
		t.Error("household-shared memory blocks the migration; every deployment with shared memory would be refused")
	}
	if len(rep.Blocking()) != 0 {
		t.Fatalf("blocking = %+v, want none for an ordinary shared record", rep.Blocking())
	}
	// It is still reported, because 2b has to know those rows exist and
	// are deliberately not repointed.
	if !h.InHumanColumn() {
		t.Error("the scope is not reported as appearing in a column 2b touches")
	}
}

// A PERSON who happens to be called household, in a column that takes
// no scope, is not a scope. The sentinel's meaning is per column.
func TestTheScopeIsOnlyAScopeWhereAColumnTakesOne(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "mika")
		// butler_tasks.author takes a person and never a scope.
		if _, err := db.InsertButlerTask(runtimedb.ButlerTask{
			Kind: runtimedb.ButlerTaskConversation, Author: "household", Owner: "mika",
			Surface: runtimedb.ButlerSurfaceFamily, Profile: runtimedb.ProfileHousehold,
			Audience:  runtimedb.ButlerAudienceShared,
			Objective: "x", CompletionCriteria: "done",
			Retention: runtimedb.ButlerRetentionConversation,
		}); err != nil {
			t.Fatalf("InsertButlerTask: %v", err)
		}
	})
	h := find(t, rep, "household")
	if h.Verdict == personref.Scope {
		t.Fatal("a value in butler_tasks.author was treated as a scope; that column takes a person, " +
			"so the value is an orphan and the migration must say so")
	}
	if !h.Blocking() {
		t.Error("an orphan in a person-only column does not block")
	}
}
