package runtimedb

// The audience vocabulary (ADR-W033 design §4, §10 layer 3, §11,
// §18.5; slice 7, #208).
//
// The capability this slice exists for is the first test: an
// adults-only audience, which two words could not express at all. The
// rest are the properties that must survive it — the strictest-source
// rule, the schema backstop, and §10's hierarchy, which an audience
// must never climb.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// audienceFor builds an Adults-only audience and returns its id.
func adultsAudience(t *testing.T, db *DB) string {
	t.Helper()
	a, err := db.CreateAudienceForGroups("Adults only", groupID(t, db, SeedAdults))
	if err != nil {
		t.Fatalf("CreateAudienceForGroups: %v", err)
	}
	return a.ID
}

// THE capability the old vocabulary could not express (#208).
func TestAdultsOnlyAudienceExcludesAChild(t *testing.T) {
	db := open(t)
	parent := personOf(t, db, "mika", RoleOwner)
	// A child is a Member with the child preset, and is admitted like
	// anyone else for this test — the preset is not the point here,
	// being outside `Adults` is.
	kid := personOf(t, db, "sam", RoleMember)

	adults := groupID(t, db, SeedAdults)
	if err := db.AddPersonToGroup(adults, parent, "mika"); err != nil {
		t.Fatal(err)
	}
	aud := adultsAudience(t, db)

	people, err := db.ResolveAudience(aud, "")
	if err != nil {
		t.Fatalf("ResolveAudience: %v", err)
	}
	if len(people) != 1 || people[0] != parent {
		t.Fatalf("an adults-only audience resolved to %v, want just the adult", people)
	}
	for _, p := range people {
		if p == kid {
			t.Error("the child is inside an adults-only audience — the capability " +
				"this slice exists for is the one it got wrong")
		}
	}
}

// private → the owner, shared → the household (#208).
//
// The migration's own mapping, read back through the resolver rather
// than by inspecting the column: what matters is who the record ends up
// disclosed to, not which literal was written.
func TestMigrationMapsPrivateAndShared(t *testing.T) {
	db := open(t)
	owner := personOf(t, db, "mika", RoleOwner)
	other := personOf(t, db, "giulia", RoleMember)

	// 'private' became the owner audience: one person, and it is the
	// record's own owner rather than a fixed name.
	people, err := db.ResolveAudience(AudienceOwner, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 || people[0] != owner {
		t.Errorf("the owner audience resolved to %v, want just the owner", people)
	}
	if people, _ := db.ResolveAudience(AudienceOwner, other); len(people) != 1 || people[0] != other {
		t.Errorf("the owner audience did not follow the record's owner: %v", people)
	}

	// 'shared' became the household: everybody active.
	household, err := db.ResolveAudience(AudienceHousehold, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(household) != 2 {
		t.Errorf("the household audience resolved to %v, want both people", household)
	}
}

// MANDATORY negative case (#208): a derivative is never wider than the
// intersection of its sources'.
func TestDerivativeNeverWidensBeyondTheIntersectionOfItsSources(t *testing.T) {
	db := open(t)
	parent := personOf(t, db, "mika", RoleOwner)
	personOf(t, db, "giulia", RoleMember)
	if err := db.AddPersonToGroup(groupID(t, db, SeedAdults), parent, "mika"); err != nil {
		t.Fatal(err)
	}
	adults := adultsAudience(t, db)

	// Adults ∩ household = Adults, because Adults is the subset.
	got, err := db.StrictestAudience([]string{adults, AudienceHousehold})
	if err != nil {
		t.Fatalf("StrictestAudience: %v", err)
	}
	if got != adults {
		t.Errorf("deriving from an Adults record and a household record gave %q, "+
			"want the Adults audience — a derivative must not be wider than the "+
			"narrowest source", got)
	}
	// Order must not decide it.
	if got, err := db.StrictestAudience([]string{AudienceHousehold, adults}); err != nil || got != adults {
		t.Errorf("the answer depends on argument order (%q, %v)", got, err)
	}
}

// And when no source is narrowest, it refuses rather than inventing an
// audience.
//
// Two groups that overlap in nobody have no narrowest member. The old
// code could not reach this state — private and shared are always
// comparable — so it is new ground, and the answer has to be the same
// one the two-owner case already gives: refuse.
func TestADerivativeOfIncomparableAudiencesIsRefused(t *testing.T) {
	db := open(t)
	parent := personOf(t, db, "mika", RoleOwner)
	kid := personOf(t, db, "sam", RoleMember)
	if err := db.AddPersonToGroup(groupID(t, db, SeedAdults), parent, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPersonToGroup(groupID(t, db, SeedKids), kid, "mika"); err != nil {
		t.Fatal(err)
	}
	adults := adultsAudience(t, db)
	kids, err := db.CreateAudienceForGroups("Kids only", groupID(t, db, SeedKids))
	if err != nil {
		t.Fatal(err)
	}

	if got, err := db.StrictestAudience([]string{adults, kids.ID}); err == nil {
		t.Errorf("two audiences overlapping in nobody produced %q; neither is "+
			"narrower, so a derivative of both has no correct audience and "+
			"inventing one is how a summary reaches both sides", got)
	}
}

// The schema backstop (#208), restated for set-valued audiences.
func TestAudienceCannotLoosenAfterWrite(t *testing.T) {
	db := open(t)
	owner := personOf(t, db, "mika", RoleOwner)
	seq, err := db.RecordMemory(MemoryRecord{
		Kind: "fact", Subject: "mika", Content: "a private thing",
		Audience: ButlerAudiencePrivate, PermissionLabel: "mika", Owner: "mika",
		SourceKind: "conversation", SourceID: "c-1", Author: "mika", EnvelopeVersion: 1,
	})
	if err != nil {
		t.Fatalf("RecordMemory: %v", err)
	}
	_ = owner

	// Widening is refused, as it always was.
	if _, err := db.sql.Exec(
		`UPDATE butler_memory SET audience = ? WHERE seq = ?`, AudienceHousehold, seq); err == nil {
		t.Error("a private memory was widened to the household by a direct UPDATE")
	}
	// And so is any other change, because SQL cannot decide whether one
	// resolved set contains another — the members are in another table
	// and the answer depends on who is active right now. Narrowing
	// needs a verb that proves it, which is a decision somebody makes.
	adults := adultsAudience(t, db)
	if _, err := db.sql.Exec(
		`UPDATE butler_memory SET audience = ? WHERE seq = ?`, adults, seq); err == nil {
		t.Error("an audience was changed by a direct UPDATE; the backstop must fail " +
			"closed on the changes it cannot adjudicate")
	}
}

// §10's hierarchy: an audience is layer 3 and never widens layer 2
// (#208).
//
// The Butler scope is the layer-2 grant that exists today. A record
// whose audience includes a reader is still unreachable through a
// Butler that does not hold it — disclosure policy does not hand out
// resource authority.
func TestAudienceDoesNotWidenResourceAuthority(t *testing.T) {
	db := open(t)
	personOf(t, db, "mika", RoleOwner)

	// Held by the health Butler, and disclosed to the whole household.
	if _, err := db.RecordMemory(MemoryRecord{
		Kind: "fact", Subject: "house", Content: "the bins go out on tuesday",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		SourceKind: "conversation", SourceID: "c-1", Author: "mika",
		EnvelopeVersion: 1, Butler: "health",
	}); err != nil {
		t.Fatalf("RecordMemory: %v", err)
	}

	// The widest possible audience, asked for through a Butler that does
	// not hold the record.
	hits, err := db.RetrieveMemory(MemoryQuery{Reader: "mika", Butler: "kitchen"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Butler == "health" {
			t.Error("a household audience handed the kitchen Butler material held by " +
				"the health Butler; §10 layer 3 must never widen layer 2")
		}
	}
}

// No table keeps a private notion of who a group means (#208).
//
// Structural, because the claim is about the code rather than about a
// behaviour: three queries each carried their own copy of the audience
// rule before this slice, which was fine while the rule was one
// comparison and three places to forget a group the moment it was not.
func TestEveryAudienceTableUsesTheSameResolver(t *testing.T) {
	for _, file := range []string{"butlermemory.go", "butlerindex.go"} {
		src := readSourceFile(t, file)
		for _, stale := range []string{
			"audience = 'shared'", "audience = 'private'",
			"audience = 'aud_household'", "audience = 'aud_owner'",
		} {
			if strings.Contains(src, stale) {
				t.Errorf("%s still spells the audience rule itself (%q); it must go "+
					"through visibleAudienceClause, or a group added to an audience "+
					"is honoured in some queries and not others", file, stale)
			}
		}
	}
	// And the clause itself is written once.
	aud := readSourceFile(t, "audiences.go")
	if n := strings.Count(aud, "func (db *DB) visibleAudienceClause"); n != 1 {
		t.Errorf("visibleAudienceClause is defined %d times", n)
	}
}

// An audience resolving to nobody discloses to nobody (#208).
func TestEmptyAudienceFailsClosed(t *testing.T) {
	db := open(t)
	reader := personOf(t, db, "mika", RoleOwner)

	// Adults exists and is empty: nobody has been put in it.
	aud := adultsAudience(t, db)
	people, err := db.ResolveAudience(aud, "")
	if err != nil {
		t.Fatalf("ResolveAudience: %v", err)
	}
	if len(people) != 0 {
		t.Fatalf("an empty group resolved to %v", people)
	}

	// And the reader cannot see it. The failure this guards is the
	// opposite reading — that an audience naming nobody must mean
	// everybody — which is how an emptied group becomes a broadcast.
	visible, err := db.AudiencesVisibleTo(reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range visible {
		if v == aud {
			t.Error("an audience that resolves to nobody was visible; empty must " +
				"disclose to nobody, never to everybody")
		}
	}
}

// A record may name an audience the household invented, and may not
// name one that does not exist (#208).
//
// validateMemory used to hold the closed set itself — `private|shared`
// — which is exactly the list that had to stop being a list. It now
// checks only that an audience was named, and the FOREIGN KEY is what
// refuses a name that means nothing. That is the right division: the
// key refuses it on every path into the table, including the ones
// nobody remembered to route through a validator.
func TestARecordMayNameAnAudienceTheHouseholdInvented(t *testing.T) {
	db := open(t)
	parent := personOf(t, db, "mika", RoleOwner)
	if err := db.AddPersonToGroup(groupID(t, db, SeedAdults), parent, "mika"); err != nil {
		t.Fatal(err)
	}
	adults := adultsAudience(t, db)

	seq, err := db.RecordMemory(MemoryRecord{
		Kind: "fact", Subject: "house", Content: "the spare key is in the drawer",
		Audience: ButlerTaskAudience(adults), PermissionLabel: "household", Owner: "mika",
		SourceKind: "conversation", SourceID: "c-1", Author: "mika", EnvelopeVersion: 1,
	})
	if err != nil {
		t.Fatalf("an adults-only memory record was refused: %v — this is the "+
			"capability the slice exists for", err)
	}
	got, err := db.GetMemory(seq)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Audience) != adults {
		t.Errorf("stored audience %q, want %q", got.Audience, adults)
	}

	// And a name that means nothing is refused, by the key rather than
	// by a list in Go.
	if _, err := db.RecordMemory(MemoryRecord{
		Kind: "fact", Subject: "house", Content: "something else",
		Audience: "aud_does_not_exist", PermissionLabel: "household", Owner: "mika",
		SourceKind: "conversation", SourceID: "c-2", Author: "mika", EnvelopeVersion: 1,
	}); err == nil {
		t.Error("a memory record naming an audience that does not exist was stored; " +
			"the foreign key is the backstop and it must hold")
	}
	// An audience omitted entirely is a caller error rather than a
	// default, because there is no audience a record silently belongs
	// to — and it is refused IN WORDS. The key refuses it too, with
	// "FOREIGN KEY constraint failed", which tells a caller nothing
	// about what they left out; that is the whole job of the remaining
	// check in validateMemory, so this asserts the message and not
	// merely that something went wrong.
	_, err = db.RecordMemory(MemoryRecord{
		Kind: "fact", Subject: "house", Content: "a third thing",
		PermissionLabel: "household", Owner: "mika",
		SourceKind: "conversation", SourceID: "c-3", Author: "mika", EnvelopeVersion: 1,
	})
	if err == nil {
		t.Error("a memory record with no audience at all was stored")
	} else if !strings.Contains(err.Error(), "audience") {
		t.Errorf("a record with no audience was refused with %q, which does not say "+
			"what was missing; the validator exists to name it", err)
	}
}

// A correction cannot widen what it corrects (#208).
//
// "Actually, it is X" must not be a way to publish. This held for two
// words by pinning the private case; with sets it is the same subset
// test the derivation rule uses, so the two cannot drift apart.
func TestACorrectionCannotWidenAnAudience(t *testing.T) {
	db := open(t)
	parent := personOf(t, db, "mika", RoleOwner)
	kid := personOf(t, db, "sam", RoleMember)
	if err := db.AddPersonToGroup(groupID(t, db, SeedAdults), parent, "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPersonToGroup(groupID(t, db, SeedKids), kid, "mika"); err != nil {
		t.Fatal(err)
	}
	adults := adultsAudience(t, db)

	seq, err := db.RecordMemory(MemoryRecord{
		Kind: "fact", Subject: "house", Content: "the appointment is on friday",
		Audience: ButlerTaskAudience(adults), PermissionLabel: "household", Owner: "mika",
		SourceKind: "conversation", SourceID: "c-1", Author: "mika", EnvelopeVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Corrected with the household named: clamped back to Adults.
	next, err := db.CorrectMemory(seq, MemoryRecord{
		Kind: "fact", Subject: "house", Content: "the appointment is on saturday",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "mika",
		SourceKind: "conversation", SourceID: "c-2", Author: "mika", EnvelopeVersion: 1,
	}, "mika")
	if err != nil {
		t.Fatalf("CorrectMemory: %v", err)
	}
	got, err := db.GetMemory(next)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Audience) != adults {
		t.Errorf("the correction landed on audience %q, want %q — a correction "+
			"naming a wider audience must be clamped, not honoured", got.Audience, adults)
	}

	// And an incomparable one is refused rather than guessed at, the
	// same answer StrictestAudience gives everywhere else.
	kids, err := db.CreateAudienceForGroups("Kids only", groupID(t, db, SeedKids))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CorrectMemory(next, MemoryRecord{
		Kind: "fact", Subject: "house", Content: "the appointment moved again",
		Audience: ButlerTaskAudience(kids.ID), PermissionLabel: "household", Owner: "mika",
		SourceKind: "conversation", SourceID: "c-3", Author: "mika", EnvelopeVersion: 1,
	}, "mika"); err == nil {
		t.Error("a correction re-pointed an Adults record at Kids; neither audience " +
			"contains the other and there is no audience the result could have")
	}
}

// The rebuild is clean on a realistic household (#208; the discipline
// 0043 earned).
//
// 0052 rebuilds SIX tables to widen one column, and 0043 rebuilt these
// same tables by hand and silently dropped two. The central assertion
// is therefore generic and about no column in particular: every column
// that existed before the migration still exists after it. That
// question needs no editing when somebody adds a column, and it is the
// question both of 0043's defects failed.
//
// The column lists in 0052 were generated from the live schema rather
// than retyped, which is the other half of the same lesson — but a
// generator can be wrong too, and this is what would notice.
func TestTheAudienceMigrationIsCleanOnARealisticSnapshot(t *testing.T) {
	const before = "migrations/0051_person_groups.sql"

	path := filepath.Join(t.TempDir(), "keystone.db")
	old, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.migrateThrough(before); err != nil {
		t.Fatalf("migrating through %s: %v", before, err)
	}
	seedAudienceHousehold(t, old)

	shapeBefore := schemaShape(t, old)
	rowsBefore := rowCounts(t, old, audienceTables)
	// Copied AFTER closing, not before: the database runs in WAL mode,
	// so until the handle closes the rows live in the -wal file and a
	// copy of the .db alone is an empty schema. The first version of
	// this copied first and failed with "no such table".
	old.Close()
	beforePath := filepath.Join(t.TempDir(), "before.db")
	if err := copyFile(beforePath, path); err != nil {
		t.Fatal(err)
	}
	old2, err := openUnmigrated(beforePath)
	if err != nil {
		t.Fatal(err)
	}
	defer old2.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("the audience migration refused a realistic household: %v", err)
	}
	defer db.Close()

	// 1. The schema is still self-consistent. Six rebuilds repoint
	//    foreign keys, and several of these tables reference each other.
	if bad := foreignKeyCheck(t, db); len(bad) > 0 {
		t.Fatalf("foreign_key_check after the migration: %v", bad)
	}

	// 2. Nothing silently disappeared — 104 columns is more than anyone
	//    checks twice.
	shapeAfter := schemaShape(t, db)
	for table, cols := range shapeBefore {
		var gone []string
		for _, c := range cols {
			if !survived(table, c, shapeAfter[table]) {
				gone = append(gone, c)
			}
		}
		if len(gone) > 0 {
			t.Errorf("%s lost %v — the defect 0043 shipped, in the same tables", table, gone)
		}
	}

	// 3. Every row survived. An INSERT ... SELECT that filters too hard
	//    loses history without failing, which is the quietest way a
	//    rebuild goes wrong.
	rowsAfter := rowCounts(t, db, audienceTables)
	for table, want := range rowsBefore {
		if got := rowsAfter[table]; got != want {
			t.Errorf("%s has %d rows, had %d", table, got, want)
		}
	}

	// 3b. And every COLUMN still carries its data.
	//
	// This is the assertion the first version of this test was missing,
	// and mutation testing is what found the gap. Dropping `butler`
	// from the INSERT and SELECT of the butler_memory rebuild left the
	// column declared — so the shape check above passed — and the row
	// count unchanged, while every value in it silently became NULL.
	//
	// That is the likelier defect now that the CREATE TABLE is
	// generated from the live schema: the declaration cannot drift, but
	// the copy still can. Comparing how many rows hold a value in each
	// column catches a column that stopped being populated, which
	// neither "does the column exist" nor "how many rows" can see.
	// Which columns this fixture actually exercises, said out loud. A
	// per-column check is worth only as much as the fixture's coverage,
	// and an uncovered column is one the assertion cannot see — so the
	// uncovered set is named here, and a column joining it fails this
	// test rather than quietly widening the blind spot.
	var uncovered []string
	for _, table := range audienceTables {
		for _, col := range shapeBefore[table] {
			if populated(t, old2, table, col) == 0 {
				uncovered = append(uncovered, table+"."+col)
			}
		}
	}
	sortStrings(uncovered)
	if diff := strings.Join(uncovered, " "); diff != strings.Join(fixtureDoesNotCover, " ") {
		t.Errorf("the columns this fixture leaves empty have changed.\n got: %v\nwant: %v\n"+
			"A column that holds nothing is a column the per-column check below cannot "+
			"see; seed it, or add it to fixtureDoesNotCover with a reason.",
			uncovered, fixtureDoesNotCover)
	}

	for _, table := range audienceTables {
		for _, col := range shapeBefore[table] {
			if col == "audience" {
				continue // legitimately rewritten by this migration
			}
			// A renamed column is read under its new name on the way
			// out. Skipping it instead would stop checking that its
			// CONTENTS survived, which is the half of this test that
			// catches an INSERT ... SELECT with a column in the wrong
			// position.
			after := col
			if to, ok := renamedTo[table+"."+col]; ok {
				after = to
			}
			want := populated(t, old2, table, col)
			if got := populated(t, db, table, after); got != want {
				t.Errorf("%s.%s holds a value in %d rows, held one in %d — the column "+
					"survived the rebuild and its contents did not", table, after, got, want)
			}
		}
	}

	// 4. And the mapping landed: no row still carries an old word, and
	//    every audience names a row that exists.
	for _, table := range audienceTables {
		var stale int
		if err := db.sql.QueryRow(
			`SELECT COUNT(*) FROM ` + table +
				` WHERE audience IN ('private', 'shared')`).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		if stale != 0 {
			t.Errorf("%s still has %d row(s) in the old vocabulary", table, stale)
		}
		var orphan int
		if err := db.sql.QueryRow(
			`SELECT COUNT(*) FROM ` + table + ` r
			  WHERE NOT EXISTS (SELECT 1 FROM audiences a WHERE a.audience_id = r.audience)`).
			Scan(&orphan); err != nil {
			t.Fatal(err)
		}
		if orphan != 0 {
			t.Errorf("%s has %d row(s) naming an audience that does not exist", table, orphan)
		}
	}
}

// audienceTables are the six 0052 rebuilds. Named once: a table left
// out here is a table the snapshot never checks.
var audienceTables = []string{
	"butler_memory", "butler_index", "butler_tasks",
	"butler_task_evidence", "butler_work_trace", "integration_connections",
}

// seedAudienceHousehold fills every rebuilt table, in BOTH audiences,
// at the pre-0052 shape. A rebuild tested only on empty tables proves
// the DDL parses and nothing else.
//
// EVERY COLUMN CARRIES A VALUE, including the optional ones and the
// foreign keys — which is why this seeds butler_sessions, a step and
// an attempt that nothing else in the test looks at. A column left
// empty here is invisible to the per-column check in the snapshot
// test: it holds nothing before the migration and nothing after, so
// dropping it from the rebuild's INSERT changes no count. That is
// exactly how the first version of this fixture let a dropped
// `butler` column through.
func seedAudienceHousehold(t *testing.T, db *DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecForTest(q, args...); err != nil {
			t.Fatalf("seeding: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO butler_members (person_id, member, role, admitted, created_at)
	      VALUES ('psn_a', 'mika', 'owner', 1, 't'), ('psn_b', 'giulia', 'member', 1, 't')`)
	// Parent rows for the nullable foreign keys below. They exist only
	// so butler_tasks.session and butler_work_trace.step/.attempt can
	// hold something.
	exec(`INSERT INTO butler_sessions (seq, created_at, principal, acting_for, provider, mode,
	          final_text, capped, transcript_json)
	      VALUES (1, 't', 'butler', 'mika', 'p', 'full', 'f', 0, '[]')`)

	for i, aud := range []string{"private", "shared"} {
		n := i + 1
		exec(`INSERT INTO butler_memory (seq, kind, subject, content, audience, permission_label,
		          owner_person_id, source_kind, source_id, observed_at, confidence,
		          safety_relevant, created_at, author_person_id, forgotten_at, forgotten_by,
		          envelope_version, butler)
		      VALUES (?, 'fact', 's', 'c', ?, 'household', 'psn_a', 'conversation', 'c1', 'o',
		          'stated', 1, 't', 'psn_a', 'f', 'fb', 1, 'health')`, n, aud)
		exec(`INSERT INTO butler_index (seq, source_kind, source_id, memory, owner_person_id,
		          audience, permission_label, content, observed_at, indexed_at, volatile,
		          embedding, tokens, butler)
		      VALUES (?, 'memory', 'm1', ?, 'psn_a', ?, 'household', 'text', 'o', 't', 0,
		          x'01020304', 3, 'health')`, n, n, aud)
		exec(`INSERT INTO butler_tasks (seq, created_at, updated_at, kind, state,
		          author_person_id, owner_person_id, surface, profile, audience, session,
		          origin_turn, correlation_id, objective, completion_criteria, grants_json,
		          budget_json, deadline_at, next_wakeup_at, lease_epoch, uncertainty,
		          result_summary, retention, envelope_version, butler)
		      VALUES (?, 't','t','conversation','pending','psn_a','psn_a','family','household',?,
		          1, 7, 'corr', 'x','done','{"g":1}','{"b":1}','d','w', 1, 'u', 'r',
		          'conversation', 1, 'health')`, n, aud)
		exec(`INSERT INTO butler_task_steps (seq, task, ordinal, kind, name, state, created_at,
		          updated_at, params_json, envelope_version)
		      VALUES (?, ?, ?, 'tool', 'n', 'proposed', 't', 't', '{}', 1)`, n, n, n)
		exec(`INSERT INTO butler_task_attempts (seq, task, step, attempt, epoch, state,
		          started_at, ended_at, result_json, error_text, envelope_version)
		      VALUES (?, ?, ?, 1, 1, 'dispatched', 't', 't', '{}', 'e', 1)`, n, n, n)
		exec(`INSERT INTO butler_task_evidence (task, recorded_at, kind, description, source_id,
		          observed_at, permission_label, audience, envelope_version)
		      VALUES (?,'t','observation','d','s','o','household',?,1)`, n, aud)
		exec(`INSERT INTO butler_work_trace (task, at, type, step, attempt, epoch, actor,
		          model_endpoint, model_verdict, prompt_version, policy_version,
		          manifest_version, source_ids, permission_label, audience, redacted,
		          payload_json, envelope_version)
		      VALUES (?,'t','read',?,?,1,'butler','http://m','ok','pv','plv','mv','s1',
		          'household',?,1,'{}',1)`, n, n, n, aud)
		exec(`INSERT INTO integration_connections (name, adapter, endpoint, ownership, owner,
		          audience, state, degraded_reason, created_at, created_by, updated_at)
		      VALUES ('c-'||?, 'imap', 'imap://x', 'personal', 'mika', ?, 'configured',
		          'why', 't', 'mika', 't')`, aud, aud)
	}

	// superseded_by can only point at a row that already exists, so it
	// is set once both memories are in.
	exec(`UPDATE butler_memory SET superseded_by = 2 WHERE seq = 1`)
}

// fixtureDoesNotCover names the columns seedAudienceHousehold leaves
// empty, and there should be none: every column of every rebuilt table
// is seeded above. The snapshot test compares the real uncovered set
// against this one, so a column that stops being populated — a new
// column added to a rebuilt table, or a seed that quietly stopped
// working — fails the test instead of silently widening the blind spot
// in the per-column data check.
var fixtureDoesNotCover = []string{}

// populated counts the rows whose column holds something.
//
// "Something" excludes NULL and the empty string, because a rebuild
// that forgets a column leaves exactly those behind — and this repo
// spells an absent text value as ” far more often than as NULL.
func populated(t *testing.T, db *DB, table, col string) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM ` + table + ` WHERE ` + col + ` IS NOT NULL AND ` + col + ` <> ''`).
		Scan(&n); err != nil {
		t.Fatalf("counting %s.%s: %v", table, col, err)
	}
	return n
}

// copyFile duplicates the database file so the pre-migration state can
// be read after the migration has run on the original.
func copyFile(dst, src string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
