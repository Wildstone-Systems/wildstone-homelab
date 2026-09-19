package runtimedb

// What the theme store is held to: one person's choice is theirs, it
// survives a rename, and removing a theme somebody is using is not a
// failure for either of them.

import (
	"errors"
	"testing"
)

func TestAThemeIsOnePersonsAndNotTheHouseholds(t *testing.T) {
	db := openWith(t, "sandy", "maya")

	if err := db.SetPersonTheme("sandy", "gruvbox"); err != nil {
		t.Fatalf("SetPersonTheme(sandy): %v", err)
	}
	if err := db.SetPersonTheme("maya", "catppuccin-latte"); err != nil {
		t.Fatalf("SetPersonTheme(maya): %v", err)
	}

	for member, want := range map[string]string{"sandy": "gruvbox", "maya": "catppuccin-latte"} {
		got, err := db.GetPersonTheme(member)
		if err != nil {
			t.Fatalf("GetPersonTheme(%s): %v", member, err)
		}
		if got != want {
			t.Errorf("%s has %q, want %q", member, got, want)
		}
	}
}

func TestChoosingNothingIsTheRestingState(t *testing.T) {
	db := openWith(t, "sandy")

	got, err := db.GetPersonTheme("sandy")
	if err != nil {
		t.Fatalf("GetPersonTheme: %v", err)
	}
	if got != "" {
		t.Errorf("a person who chose nothing has %q", got)
	}

	if err := db.SetPersonTheme("sandy", "nord"); err != nil {
		t.Fatal(err)
	}
	// Clearing is a delete, so "went back to the default" and "never
	// chose" are the same state rather than two that can drift.
	if err := db.SetPersonTheme("sandy", ""); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	got, err = db.GetPersonTheme("sandy")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("after clearing, the theme is %q", got)
	}
	var rows int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM person_theme`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("clearing left %d row(s) behind", rows)
	}
}

// The choice is keyed on person_id, so a person who changes their
// handle keeps the theme they picked.
func TestAThemeFollowsAPersonThroughARename(t *testing.T) {
	db := openWith(t, "sandy")
	if err := db.SetPersonTheme("sandy", "everforest"); err != nil {
		t.Fatal(err)
	}
	if err := db.RenameButlerMemberHandle("sandy", "alexandra"); err != nil {
		t.Fatalf("RenameButlerMemberHandle: %v", err)
	}
	got, err := db.GetPersonTheme("alexandra")
	if err != nil {
		t.Fatal(err)
	}
	if got != "everforest" {
		t.Errorf("after the rename the theme is %q, want everforest", got)
	}
}

// Reading is on the path to painting a page, so a handle nobody has a
// record of yet gets the default look rather than an error.
func TestAnUnknownHandleReadsAsNoChoice(t *testing.T) {
	db := open(t)
	got, err := db.GetPersonTheme("nobody")
	if err != nil {
		t.Fatalf("reading for an unknown handle: %v", err)
	}
	if got != "" {
		t.Errorf("an unknown handle has %q", got)
	}
	// Writing is a different matter: recording a choice against
	// somebody who does not exist is a bug in the caller.
	if err := db.SetPersonTheme("nobody", "nord"); !errors.Is(err, ErrNoSuchPerson) {
		t.Errorf("SetPersonTheme for an unknown handle = %v, want ErrNoSuchPerson", err)
	}
}

func TestAPastedThemeIsStoredAsWhatWasPasted(t *testing.T) {
	db := open(t)
	const colors = "mode = \"dark\"\nbackground = \"#101010\"\nforeground = \"#f0f0f0\"\n"

	if err := db.SaveHouseholdTheme("our-house", colors, "mika"); err != nil {
		t.Fatalf("SaveHouseholdTheme: %v", err)
	}
	all, err := db.ListHouseholdThemes()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("listed %d themes, want 1", len(all))
	}
	if all[0].Colors != colors {
		t.Errorf("the stored source is not what was pasted:\n%q", all[0].Colors)
	}
	if all[0].AddedBy != "mika" {
		t.Errorf("addedBy = %q", all[0].AddedBy)
	}

	// Pasting the same name again is a revision, not a collision: the
	// usual reason to do it is that the theme's author published one.
	const revised = "mode = \"dark\"\nbackground = \"#0a0a0a\"\nforeground = \"#f0f0f0\"\n"
	if err := db.SaveHouseholdTheme("our-house", revised, "sandy"); err != nil {
		t.Fatalf("re-pasting: %v", err)
	}
	all, err = db.ListHouseholdThemes()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Colors != revised || all[0].AddedBy != "sandy" {
		t.Errorf("re-pasting did not replace the theme: %+v", all)
	}
}

// Removing a theme people are using must not fail, and must not orphan
// them into an error state. Their row keeps naming it, which resolves
// to nothing, which is the design's own look — and if the theme comes
// back, so do they.
func TestRemovingAThemeSomebodyIsUsingLeavesThemReadable(t *testing.T) {
	db := openWith(t, "sandy")
	if err := db.SaveHouseholdTheme("our-house", "background = \"#101010\"\nforeground = \"#f0f0f0\"\n", "mika"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPersonTheme("sandy", "our-house"); err != nil {
		t.Fatal(err)
	}

	removed, err := db.DeleteHouseholdTheme("our-house")
	if err != nil {
		t.Fatalf("DeleteHouseholdTheme: %v", err)
	}
	if !removed {
		t.Error("DeleteHouseholdTheme reported nothing removed")
	}

	got, err := db.GetPersonTheme("sandy")
	if err != nil {
		t.Fatalf("reading after the theme was removed: %v", err)
	}
	if got != "our-house" {
		t.Errorf("the choice was rewritten to %q; it should be left alone", got)
	}

	again, err := db.DeleteHouseholdTheme("our-house")
	if err != nil {
		t.Fatalf("removing it twice: %v", err)
	}
	if again {
		t.Error("removing a theme that was already gone reported a removal")
	}
}

func TestAThemeNeedsANameAndColours(t *testing.T) {
	db := open(t)
	if err := db.SaveHouseholdTheme("", "background = \"#101010\"", "mika"); err == nil {
		t.Error("a nameless theme was stored")
	}
	if err := db.SaveHouseholdTheme("our-house", "", "mika"); err == nil {
		t.Error("a colourless theme was stored")
	}
}
