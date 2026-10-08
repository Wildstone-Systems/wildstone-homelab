package personref_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb/personref"
)

// schema0039 is the schema as migration 0039 left it. The migrations that
// built it are squashed into the baseline, so it is a frozen copy.
func schema0039(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/schema-0039.sql")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// surveyed builds a database AS IT STOOD BEFORE MIGRATION 0043, lets
// the caller write realistic legacy rows into it, and then surveys it
// through a READ-ONLY handle — the same way the CLI does.
//
// Stopping at 0039 is the point rather than a convenience. Free-text
// person references only exist before 0043 moved them, so a deployment
// that still has them is a deployment that has not migrated, and that
// is precisely when this report is worth running: `keystone person-refs`
// opens the file read-only and does not migrate it, so this binary can
// pre-flight an un-upgraded database.
//
// The seed writes raw SQL for the same reason. The store above this
// package speaks the 0043 schema now, so a fixture built through it
// could not produce the rows these tests are about.
func surveyed(t *testing.T, seed func(*runtimedb.DB)) (personref.Report, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := runtimedb.OpenUnmigratedForTest(path)
	if err != nil {
		t.Fatalf("OpenUnmigratedForTest: %v", err)
	}
	if _, err := db.ExecForTest(string(schema0039(t))); err != nil {
		t.Fatalf("loading the 0039 schema: %v", err)
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

// people adds Persons, in the 0039 shape.
func people(t *testing.T, db *runtimedb.DB, handles ...string) {
	t.Helper()
	for _, h := range handles {
		if _, err := db.ExecForTest(
			`INSERT INTO butler_members (person_id, member, role, admitted, generation, created_at)
			 VALUES (?, ?, 'member', 0, 1, 't')`, "psn_"+h, h); err != nil {
			t.Fatalf("recording %s: %v", h, err)
		}
	}
}

// admit is AdmitButlerMember in raw SQL, so the fixture does not depend
// on a store that now speaks a later schema.
func admit(t *testing.T, db *runtimedb.DB, handle, by string) {
	t.Helper()
	if _, err := db.ExecForTest(
		`UPDATE butler_members SET admitted = 1, admitted_at = 't', admitted_by = ?,
		     generation = generation + 1 WHERE member = ?`, by, handle); err != nil {
		t.Fatalf("admitting %s: %v", handle, err)
	}
}

// legacyMemory writes one pre-0043 memory row: owner and author as free
// text, which is the whole thing the survey exists to find.
func legacyMemory(t *testing.T, db *runtimedb.DB, owner, author, audience, sourceID, content string) {
	t.Helper()
	if _, err := db.ExecForTest(
		`INSERT INTO butler_memory (kind, subject, content, audience, permission_label, owner,
		     source_kind, source_id, created_at, author, envelope_version)
		 VALUES ('fact', ?, ?, ?, 'household', ?, 'conversation', ?, 't', ?, 1)`,
		owner, content, audience, owner, sourceID, author); err != nil {
		t.Fatalf("recording legacy memory owned by %q: %v", owner, err)
	}
}

// legacyTask is the same for butler_tasks, whose author and owner were
// free text before 0043.
func legacyTask(t *testing.T, db *runtimedb.DB, author, owner string) {
	t.Helper()
	if _, err := db.ExecForTest(
		`INSERT INTO butler_tasks (created_at, updated_at, kind, state, author, owner,
		     surface, profile, audience, objective, completion_criteria, retention, envelope_version)
		 VALUES ('t', 't', 'conversation', 'pending', ?, ?, 'family', 'household', 'shared',
		     'x', 'done', 'conversation', 1)`, author, owner); err != nil {
		t.Fatalf("recording a legacy task authored by %q: %v", author, err)
	}
}

// legacyConnection writes a connection at the pre-0043 shape.
//
// These fixtures open the database through 0039 on purpose — the report
// is about rows the current schema can no longer produce — so they
// cannot go through CreateConnection, which writes the audience the
// CURRENT schema expects. That coupling was invisible until slice 7
// changed the audience vocabulary and this file started failing on a
// CHECK from four migrations ago.
func legacyConnection(t *testing.T, db *runtimedb.DB, name, ownership, owner, audience string) {
	t.Helper()
	if _, err := db.ExecForTest(
		`INSERT INTO integration_connections (name, adapter, endpoint, ownership, owner,
		     audience, state, created_at, created_by, updated_at)
		 VALUES (?, 'imap', 'imap://mail.example.com', ?, ?, ?, 'configured', 't', ?, 't')`,
		name, ownership, owner, audience, owner); err != nil {
		t.Fatalf("recording a legacy %s connection owned by %q: %v", ownership, owner, err)
	}
}

// The core negative case: a dry run is a read.
func TestTheSurveyWritesNothing(t *testing.T) {
	var before []byte
	rep, path := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "sam")
		legacyMemory(t, db, "sam", "sam", "shared", "v-1", "the bins go out on Tuesday")
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
		admit(t, db, "mika", "root-admin")
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
		legacyMemory(t, db, "sam", "sam", "private", "v-2", "prefers oat milk")
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
		legacyMemory(t, db, "ADMIN", "ADMIN", "shared", "v-3", "the router password rotates monthly")
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
		legacyMemory(t, db, "ghost", "ghost", "shared", "v-4", "left by somebody who is gone")
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
		legacyMemory(t, db, "sam", "sam", "shared", "v-5", "a fact")
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
			admit(t, db, h, "root-admin")
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
	// These are pre-0043 columns now: 0043 repointed both, so the
	// verdict that decided it lives in the legacy table. The assertion
	// is unchanged — it is what made the repoint correct.
	for _, c := range personref.LegacyColumns {
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
		legacyMemory(t, db, "root-admin", "root-admin", "shared", "v-8", "written by the machine path")
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
		admit(t, db, "mika", "root-admin")
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
		legacyMemory(t, db, "household", "mika", "shared", "v-10", "the bins go out on Tuesday")
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
		legacyTask(t, db, "household", "mika")
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

// A scope in one column does not excuse an orphan in another.
//
// The handle is one string; its uses are not. `household` owning a
// shared memory record is the scope, and the same word in
// butler_tasks.author is a person reference to nobody. An earlier
// version asked whether ANY use was scoped and so let the first excuse
// the second — a silent pass, on exactly the report a migration gate
// depends on.
func TestAScopeInOneColumnDoesNotExcuseAnOrphanInAnother(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "mika")
		// Legitimate: a household-shared memory record.
		legacyMemory(t, db, "household", "mika", "shared", "v-11", "bins on Tuesday")
		// Not legitimate: a task authored by nobody.
		legacyTask(t, db, "household", "mika")
	})

	h := find(t, rep, "household")
	if !h.Blocking() {
		t.Fatal("the memory scope excused the orphaned task author; " +
			"an unresolved person reference was declared migration-ready")
	}
	// And the report says which use is which, so an Owner can act on
	// the right one rather than hunting.
	var scoped, orphaned bool
	for _, u := range h.Uses {
		if u.Table == "butler_memory" && u.Column == "owner" {
			scoped = u.Scope
		}
		if u.Table == "butler_tasks" && u.Column == "author" {
			orphaned = !u.Scope
		}
	}
	if !scoped {
		t.Error("the shared memory row is not reported as the scope")
	}
	if !orphaned {
		t.Error("the task author is reported as a scope, and that column takes only a person")
	}
}

// The scope is licensed by the row's ownership mode, not by the word.
//
// butler_memory.owner is the household only for a SHARED record. The
// same value on a private record is not a scoped row, it is a broken
// one, and a report that cannot tell them apart is right on average
// rather than right per row.
func TestTheScopeIsLicensedByTheRowsOwnershipMode(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "mika")
		legacyMemory(t, db, "household", "mika", "private", "v-12", "a private note owned by nobody")
	})

	h := find(t, rep, "household")
	for _, u := range h.Uses {
		if u.Table == "butler_memory" && u.Column == "owner" && u.Scope {
			t.Fatal("a PRIVATE memory record owned by `household` was accepted as the scope; " +
				"the scope belongs to shared records, and this row has no owner")
		}
	}
	if !h.Blocking() {
		t.Error("a private record owned by the household scope does not block")
	}
}

// A personal connection owned by a machine principal does not block.
//
// api.createConnection defaults an omitted owner to actingMember(id),
// so a bare admin creating a personal connection stores root-admin.
// That is a supported path, and this was settled once already — then
// reintroduced when the column was moved to Scoped for the sake of its
// `household` sentinel, which put the principal back in a person-
// bearing column. The sentinel is not worth a regression on the case
// the column actually sees.
func TestAPersonalConnectionOwnedByAPrincipalDoesNotBlock(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		if err := db.CreatePrincipal("root-admin", runtimedb.RoleAdmin, ""); err != nil {
			t.Fatalf("CreatePrincipal: %v", err)
		}
		people(t, db, "mika")
		legacyConnection(t, db, "mail", "personal", "root-admin", "private")
	})

	for _, h := range rep.Blocking() {
		for _, u := range h.Uses {
			if u.Table == "integration_connections" && u.Column == "owner" {
				t.Fatalf("a personal connection owned by %q blocks the migration; "+
					"that is the supported path the endpoint takes by default", h.Value)
			}
		}
	}
}

// And the household sentinel there is equally harmless, so the verdict
// costs nothing on the other mode either.
func TestAHouseholdConnectionDoesNotBlock(t *testing.T) {
	rep, _ := surveyed(t, func(db *runtimedb.DB) {
		people(t, db, "mika")
		legacyConnection(t, db, "calendar", "household", "household", "shared")
	})
	if len(rep.Blocking()) != 0 {
		t.Fatalf("a household-owned connection blocks: %+v", rep.Blocking())
	}
}
