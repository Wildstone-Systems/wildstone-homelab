package runtimedb

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestAddingCreatesTheListAndMatchesItemsWhateverTheCase(t *testing.T) {
	db := open(t)
	change, err := db.AddListItems("Shopping", []string{"Milk", " eggs ", "milk"}, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if !change.Created || change.List != "Shopping" || !reflect.DeepEqual(change.Changed, []string{"Milk", "eggs"}) {
		t.Fatalf("first add = %+v", change)
	}
	// Another spelling of the same list and the same item.
	change, err = db.AddListItems("  shopping", []string{"MILK", "bread"}, "kim")
	if err != nil {
		t.Fatal(err)
	}
	if change.Created || !reflect.DeepEqual(change.Changed, []string{"bread"}) || !reflect.DeepEqual(change.Unchanged, []string{"MILK"}) {
		t.Fatalf("second add = %+v", change)
	}
	l, err := db.HouseholdList("SHOPPING")
	if err != nil {
		t.Fatal(err)
	}
	if l.Open != 3 || l.Done != 0 || l.Items[0].Text != "Milk" || l.Items[0].AddedBy != "sam" || l.Items[2].AddedBy != "kim" {
		t.Fatalf("list = %+v", l)
	}
}

func TestTickingUntickingAndAskingAgain(t *testing.T) {
	db := open(t)
	if _, err := db.AddListItems("shopping", []string{"milk", "eggs"}, "sam"); err != nil {
		t.Fatal(err)
	}
	change, err := db.SetListItemsChecked("shopping", []string{"milk", "butter"}, true, "kim")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(change.Changed, []string{"milk"}) || !reflect.DeepEqual(change.Missing, []string{"butter"}) {
		t.Fatalf("tick = %+v", change)
	}
	if change, _ = db.SetListItemsChecked("shopping", []string{"milk"}, true, "kim"); !reflect.DeepEqual(change.Unchanged, []string{"milk"}) {
		t.Fatalf("ticking twice = %+v", change)
	}
	l, _ := db.HouseholdList("shopping")
	// Ticked items sort after open ones.
	if l.Open != 1 || l.Done != 1 || l.Items[0].Text != "eggs" || !l.Items[1].Checked || l.Items[1].CheckedBy != "kim" {
		t.Fatalf("after ticking = %+v", l)
	}
	// Asking for milk again means milk is needed again.
	if change, _ = db.AddListItems("shopping", []string{"milk"}, "sam"); !reflect.DeepEqual(change.Changed, []string{"milk"}) {
		t.Fatalf("re-adding a ticked item = %+v", change)
	}
	if l, _ = db.HouseholdList("shopping"); l.Open != 2 || l.Done != 0 {
		t.Fatalf("after re-adding = %+v", l)
	}
}

func TestRemovingClearingAndDeleting(t *testing.T) {
	db := open(t)
	if _, err := db.AddListItems("packing", []string{"passport", "charger", "socks"}, "sam"); err != nil {
		t.Fatal(err)
	}
	change, err := db.RemoveListItems("packing", []string{"socks", "towel"})
	if err != nil || !reflect.DeepEqual(change.Changed, []string{"socks"}) || !reflect.DeepEqual(change.Missing, []string{"towel"}) {
		t.Fatalf("remove = %+v %v", change, err)
	}
	if _, err := db.SetListItemsChecked("packing", []string{"passport"}, true, "sam"); err != nil {
		t.Fatal(err)
	}
	if name, n, err := db.ClearCheckedListItems("PACKING"); err != nil || name != "packing" || n != 1 {
		t.Fatalf("clear = %q %d %v", name, n, err)
	}
	if name, n, err := db.DeleteHouseholdList("packing"); err != nil || name != "packing" || n != 1 {
		t.Fatalf("delete = %q %d %v", name, n, err)
	}
	if _, err := db.HouseholdList("packing"); !errors.Is(err, ErrListNotFound) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted list = %v", err)
	}
	var orphans int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM household_list_items`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("items outlived their list: %d %v", orphans, err)
	}
	if _, err := db.SetListItemsChecked("nope", []string{"x"}, true, "sam"); !errors.Is(err, ErrListNotFound) {
		t.Fatalf("changing a list that is not there = %v", err)
	}
}

func TestListBounds(t *testing.T) {
	db := open(t)
	for _, tc := range []struct {
		name  string
		list  string
		items []string
	}{
		{"no list name", "  ", []string{"milk"}},
		{"no items", "shopping", []string{" ", ""}},
		{"long name", strings.Repeat("l", MaxListNameRunes+1), []string{"milk"}},
		{"long item", "shopping", []string{strings.Repeat("i", MaxListItemRunes+1)}},
	} {
		if _, err := db.AddListItems(tc.list, tc.items, "sam"); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	items := make([]string, MaxHouseholdListItems)
	for i := range items {
		items[i] = fmt.Sprintf("item %d", i)
	}
	if _, err := db.AddListItems("big", items, "sam"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddListItems("big", []string{"one more"}, "sam"); err == nil {
		t.Fatal("a full list took another item")
	}
	for i := 1; i < MaxHouseholdLists; i++ {
		if _, err := db.AddListItems(fmt.Sprintf("list %d", i), []string{"x"}, "sam"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AddListItems("one list too many", []string{"x"}, "sam"); err == nil {
		t.Fatal("the household went past its list limit")
	}
	lists, err := db.HouseholdLists()
	if err != nil || len(lists) != MaxHouseholdLists {
		t.Fatalf("lists = %d %v", len(lists), err)
	}
}
