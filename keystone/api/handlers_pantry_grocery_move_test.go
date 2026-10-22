package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/domestic"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// fakeGroceries is PantryOS's grocery list: it takes items until failAt
// of them have arrived, then stops answering.
type fakeGroceries struct {
	mu     sync.Mutex
	got    []string
	failAt int // 0: never fail
}

func (f *fakeGroceries) AddGroceries(_ context.Context, items []string) (domestic.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt > 0 && len(f.got) >= f.failAt {
		return domestic.Result{}, errors.New("PantryOS is not answering right now, so I can't tell — nothing was changed")
	}
	f.got = append(f.got, items...)
	return domestic.Result{EffectConfirmed: true}, nil
}

func groceryMoveHarness(t *testing.T, f *fakeGroceries) *harness {
	t.Helper()
	h := newHarness(t)
	h.s.PantryGroceries = f
	for _, seed := range []struct {
		list  string
		items []string
	}{
		{"Shopping", []string{"milk", "eggs", "bread"}},
		{"Chores", []string{"bins"}},
	} {
		if _, err := h.s.DB.AddListItems(seed.list, seed.items, "sam"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.s.DB.SetListItemsChecked("shopping", []string{"bread"}, true, "sam"); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestGroceryMovePreviewShowsOnlyTheOpenGroceryItems(t *testing.T) {
	h := groceryMoveHarness(t, &fakeGroceries{})
	var out struct {
		Lists []groceryMoveList `json:"lists"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/pantry/grocery-move", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d", resp.StatusCode)
	}
	want := []groceryMoveList{{Name: "Shopping", Items: []string{"milk", "eggs"}}}
	if !reflect.DeepEqual(out.Lists, want) {
		t.Fatalf("preview = %+v, want %+v (the chores list and ticked bread are not groceries to move)", out.Lists, want)
	}
}

func TestMovingGroceriesLeavesOneGroceryList(t *testing.T) {
	f := &fakeGroceries{}
	h := groceryMoveHarness(t, f)
	var out struct {
		Moved int                 `json:"moved"`
		Lists []groceryMoveResult `json:"lists"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/pantry/grocery-move", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("move = %d", resp.StatusCode)
	}
	if !reflect.DeepEqual(f.got, []string{"milk", "eggs"}) {
		t.Fatalf("PantryOS received %v, want milk and eggs", f.got)
	}
	if out.Moved != 2 || len(out.Lists) != 1 || !out.Lists[0].Removed || out.Lists[0].Left != 0 {
		t.Fatalf("move answered %+v", out)
	}
	if _, err := h.s.DB.HouseholdList("shopping"); !errors.Is(err, runtimedb.ErrListNotFound) {
		t.Fatalf("the old grocery list is still there: %v", err)
	}
	if l, err := h.s.DB.HouseholdList("chores"); err != nil || l.Open != 1 {
		t.Fatalf("the chores list was touched: %+v, %v", l, err)
	}

	// Again: nothing is left to move, and nothing is sent twice.
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/pantry/grocery-move", nil, &out); resp.StatusCode != http.StatusOK || out.Moved != 0 {
		t.Fatalf("second move = %d %+v", resp.StatusCode, out)
	}
	if len(f.got) != 2 {
		t.Fatalf("a second move sent %v", f.got)
	}
}

func TestAGroceryMoveThatStopsPartWayLosesNothing(t *testing.T) {
	f := &fakeGroceries{failAt: 1}
	h := groceryMoveHarness(t, f)
	var out map[string]any
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/pantry/grocery-move", nil, &out); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a move PantryOS stopped answering = %d, want 502", resp.StatusCode)
	}
	l, err := h.s.DB.HouseholdList("shopping")
	if err != nil || l.Open != 1 || l.Items[0].Text != "eggs" {
		t.Fatalf("after moving only milk the old list is %+v, %v; want eggs still on it", l, err)
	}

	// PantryOS is back: moving again finishes the job.
	f.failAt = 0
	if resp := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/pantry/grocery-move", nil, &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("the finishing move = %d %v", resp.StatusCode, out)
	}
	if !reflect.DeepEqual(f.got, []string{"milk", "eggs"}) {
		t.Fatalf("PantryOS received %v", f.got)
	}
	if _, err := h.s.DB.HouseholdList("shopping"); !errors.Is(err, runtimedb.ErrListNotFound) {
		t.Fatalf("the old grocery list is still there: %v", err)
	}
}

func TestMovingGroceriesIsAnOwnersCall(t *testing.T) {
	h := groceryMoveHarness(t, &fakeGroceries{})
	for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler, runtimedb.RolePublisher} {
		// A publisher is refused at the door (401: emit is all it may do).
		if resp := h.do(role, http.MethodPost, "/api/v1/pantry/grocery-move", nil, nil); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s moving groceries = %d, want refused", role, resp.StatusCode)
		}
	}
	if l, err := h.s.DB.HouseholdList("shopping"); err != nil || l.Open != 2 {
		t.Fatalf("a refused move changed the list: %+v, %v", l, err)
	}
}
