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
	if _, _, err := db.PersonalFilesDir("alex", "sam"); err == nil {
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
