package runtimedb

// What person_handles is held to (#372): a reference written as a handle
// keeps reaching the same Person after a rename, and a retired handle is
// never handed to somebody else.

import (
	"path/filepath"
	"testing"
)

func TestAReferenceFollowsItsPersonThroughARename(t *testing.T) {
	db := openWith(t, "sam", "alex")
	before, err := db.ResolvePersonRef("sam")
	if err != nil || before == "" {
		t.Fatalf("ResolvePersonRef(sam) = %q, %v", before, err)
	}
	if err := db.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	for _, ref := range []string{"sam", "samuel", before} {
		got, err := db.ResolvePersonRef(ref)
		if err != nil || got != before {
			t.Errorf("ResolvePersonRef(%q) = %q, %v; want %q", ref, got, err, before)
		}
	}
	if handle, err := db.PersonHandle(before); err != nil || handle != "samuel" {
		t.Errorf("PersonHandle = %q, %v; want samuel", handle, err)
	}
	for _, ref := range []string{"nobody", "", "psn_0000000000000000", "Sam"} {
		if got, err := db.ResolvePersonRef(ref); err != nil || got != "" {
			t.Errorf("ResolvePersonRef(%q) = %q, %v; want nobody", ref, got, err)
		}
	}
}

func TestARetiredHandleIsNeverGivenToSomebodyElse(t *testing.T) {
	db := openWith(t, "sam", "alex")
	sam, _ := db.ResolvePersonRef("sam")
	if err := db.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := db.RecordButlerMember("sam", RoleMember, "", ""); err == nil {
		t.Fatal("a new Person took a retired handle, inheriting every grant written for it")
	}
	if err := db.RenameButlerMemberHandle("alex", "sam"); err == nil {
		t.Fatal("another Person was renamed onto a retired handle")
	}
	// Their own old name is theirs to take back.
	if err := db.RenameButlerMemberHandle("samuel", "sam"); err != nil {
		t.Fatalf("taking an own handle back: %v", err)
	}
	if got, _ := db.ResolvePersonRef("samuel"); got != sam {
		t.Errorf("samuel resolves to %q, want %q", got, sam)
	}
	// Re-recording an existing Person is still an update of that Person.
	if err := db.RecordButlerMember("sam", RoleMember, "", ""); err != nil {
		t.Fatalf("re-recording sam: %v", err)
	}
}

func TestCalendarVisibilitySurvivesARename(t *testing.T) {
	db := openWith(t, "sam")
	if err := db.SetCalendarVisibility("sam", "school", false); err != nil {
		t.Fatal(err)
	}
	if err := db.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatal(err)
	}
	prefs, err := db.CalendarVisibility("samuel")
	if err != nil || prefs["school"] {
		t.Fatalf("prefs after rename = %v, %v", prefs, err)
	}
	if v, ok := prefs["school"]; !ok || v {
		t.Fatalf("hidden calendar lost across rename: %v", prefs)
	}
	if err := db.SetCalendarVisibility("stranger", "school", false); err == nil {
		t.Fatal("stored a preference for nobody on record")
	}
}

func TestAPersonalFolderStaysPutThroughARename(t *testing.T) {
	db := openWith(t, "sam", "alex")
	dir, ok, err := db.PersonalFilesDir("sam", "sam")
	if err != nil || !ok || dir != "sam" {
		t.Fatalf("first folder = %q, %v, %v", dir, ok, err)
	}
	if err := db.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatal(err)
	}
	dir, ok, err = db.PersonalFilesDir("samuel", "samuel")
	if err != nil || !ok || dir != "sam" {
		t.Fatalf("folder after rename = %q, %v, %v; want the same folder", dir, ok, err)
	}
	if dir, _, err := db.PersonalFilesDir("alex", "sam"); err != nil || dir != "alex" {
		t.Fatalf("alex's folder = %q %v; a recorded folder is never replaced by a proposal", dir, err)
	}
	// A new Person whose name gives a folder somebody already holds is not
	// put in it.
	record(t, db, "Sam")
	if _, _, err := db.PersonalFilesDir("Sam", "sam"); err == nil {
		t.Fatal("two People were given one folder")
	}
	if _, ok, err := db.PersonalFilesDir("stranger", "stranger"); ok || err != nil {
		t.Fatalf("somebody with no Person got a recorded folder: %v, %v", ok, err)
	}
}

// A household on 0092 keeps its display choices and its folders, and its
// existing handles resolve, once 0093 has run.
func TestMigration0093CarriesHandlesPreferencesAndFolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	old, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.migrateThrough("migrations/0092_chat_outbound_speech.sql"); err != nil {
		t.Fatalf("migrating through 0092: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO butler_members (person_id, member, role, admitted, generation, created_at)
		 VALUES ('psn_00000000000000aa', 'Sam', 'owner', 1, 1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO calendar_visibility (member, calendar_id, visible) VALUES ('Sam', 'school', 0), ('ghost', 'school', 0)`,
	} {
		if _, err := old.sql.Exec(stmt); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies 0093): %v", err)
	}
	defer db.Close()
	if got, _ := db.ResolvePersonRef("Sam"); got != "psn_00000000000000aa" {
		t.Fatalf("Sam resolves to %q", got)
	}
	if prefs, err := db.CalendarVisibility("Sam"); err != nil || len(prefs) != 1 || prefs["school"] {
		t.Fatalf("carried preferences = %v, %v", prefs, err)
	}
	if dir, ok, err := db.PersonalFilesDir("Sam", ""); err != nil || !ok || dir != "sam" {
		t.Fatalf("carried folder = %q, %v, %v; want the lower-cased folder already in use", dir, ok, err)
	}
}

func TestPersonHandlesListsTheCurrentNameFirst(t *testing.T) {
	db := openWith(t, "sam")
	if err := db.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatal(err)
	}
	got, err := db.PersonHandles("samuel")
	if err != nil || len(got) != 2 || got[0] != "samuel" || got[1] != "sam" {
		t.Fatalf("PersonHandles = %v, %v", got, err)
	}
	// A name that is only somebody's old handle is not that somebody.
	if got, _ := db.PersonHandles("sam"); len(got) != 1 || got[0] != "sam" {
		t.Fatalf("an old handle reached its Person's history: %v", got)
	}
}

// Somebody signed in under a name another Person used to carry gets none
// of that Person's own things (#426 review).
func TestAnOldNameDoesNotSignInAsItsPerson(t *testing.T) {
	db := openWith(t, "sam")
	if _, _, err := db.PersonalFilesDir("sam", "sam"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCalendarVisibility("sam", "school", false); err != nil {
		t.Fatal(err)
	}
	if err := db.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatal(err)
	}
	// "sam" is now only a directory account with no Person.
	if dir, ok, err := db.PersonalFilesDir("sam", "sam"); ok || err == nil {
		t.Fatalf("the old name reached the Person's folder: %q %v %v", dir, ok, err)
	}
	if prefs, err := db.CalendarVisibility("sam"); err != nil || len(prefs) != 0 {
		t.Fatalf("the old name read the Person's choices: %v %v", prefs, err)
	}
	if err := db.SetCalendarVisibility("sam", "school", true); err == nil {
		t.Fatal("the old name wrote the Person's choices")
	}
	samuel, _ := db.ResolvePersonRef("samuel")
	for _, login := range []string{"sam", "SAM", "samuel"} {
		if owners, err := db.HandleOwners(login); err != nil || len(owners) != 1 || owners[0] != samuel {
			t.Fatalf("HandleOwners(%s) = %v %v", login, owners, err)
		}
	}
}

// Somebody who used Files as a plain directory account, became a Person and
// was renamed before opening Files again keeps their folder, and a new
// account under the old name does not get it (#426 review).
func TestAFolderUsedBeforeBecomingAPersonStaysTheirs(t *testing.T) {
	db := openWith(t)
	if _, ok, err := db.PersonalFilesDir("sam", "sam"); ok || err != nil {
		t.Fatalf("no Person yet: %v %v", ok, err)
	}
	record(t, db, "sam")
	if err := db.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatal(err)
	}
	if dir, ok, err := db.PersonalFilesDir("samuel", "samuel"); err != nil || !ok || dir != "sam" {
		t.Fatalf("samuel's folder = %q %v %v; want the one they had", dir, ok, err)
	}
	if dir, ok, err := db.PersonalFilesDir("sam", "sam"); ok || err == nil {
		t.Fatalf("a new account under the old name got the folder: %q %v %v", dir, ok, err)
	}
	// A case variant of a Person's handle is theirs too.
	if _, ok, err := db.PersonalFilesDir("SAMUEL", "samuel"); ok || err == nil {
		t.Fatal("a case variant reached the Person's folder")
	}
}

// A Person recorded between 0093 and 0094 is backfilled by 0095 from the handle
// they were created with, so a rename before their next Files visit keeps
// their folder (#426 review).
func TestMigration0095BackfillsPeopleMissedBy0093(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	old, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.migrateThrough("migrations/0093_person_handles.sql"); err != nil {
		t.Fatalf("migrating through 0093: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO butler_members (person_id, member, role, admitted, generation, created_at)
		 VALUES ('psn_00000000000000aa', 'sam', 'member', 1, 1, '2026-01-01T00:00:00Z'),
		        ('psn_00000000000000bb', 'Alex', 'member', 1, 1, '2026-01-01T00:00:00Z'),
		        ('psn_00000000000000cc', 'alex', 'member', 1, 1, '2026-01-01T00:00:00Z')`,
		`UPDATE butler_members SET member = 'samuel' WHERE member = 'sam'`,
	} {
		if _, err := old.sql.Exec(stmt); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	// A pilot that already applied 0094 as first published, before the
	// backfill existed.
	if err := old.migrateThrough("migrations/0094_person_file_dirs_at_creation.sql"); err != nil {
		t.Fatalf("migrating through 0094: %v", err)
	}
	old.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies 0095): %v", err)
	}
	defer db.Close()
	if dir, ok, err := db.PersonalFilesDir("samuel", "samuel"); err != nil || !ok || dir != "sam" {
		t.Fatalf("samuel's folder = %q %v %v; want the one made as sam", dir, ok, err)
	}
	// Two people whose names fold to one folder: only the earlier gets it.
	a, _, errA := db.PersonalFilesDir("Alex", "alex")
	b, _, errB := db.PersonalFilesDir("alex", "alex")
	if (errA == nil) == (errB == nil) || (a != "alex" && b != "alex") {
		t.Fatalf("Alex=%q %v, alex=%q %v; want exactly one of them in the folder", a, errA, b, errB)
	}
}
