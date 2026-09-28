package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func installLists(t *testing.T, h *harness) {
	t.Helper()
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindLdapGroup,
		Metadata:  resource.Metadata{Name: "lists-users", Owner: "capability/lists", Package: "lists-wildstone", Node: "n1"},
		LdapGroup: &resource.LdapGroupSpec{Name: "lists_users"},
	}); err != nil {
		t.Fatal(err)
	}
}

func listsDo(t *testing.T, h *harness, method, path, user, groups string, body any, out any) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	res := h.member(method, path, user, groups, reader, "application/json")
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

// The Lists card's door is the lists capability's own group: absent until
// the package is installed, closed to a member outside the group.
func TestTheListsCardIsBehindItsOwnGroup(t *testing.T) {
	h, _ := portalHarness(t)
	if code := listsDo(t, h, http.MethodGet, "/api/v1/lists", "sam", "lists_users", nil, nil); code != http.StatusNotFound {
		t.Fatalf("before install = %d, want 404", code)
	}
	installLists(t, h)
	if code := listsDo(t, h, http.MethodGet, "/api/v1/lists", "sam", "homepage_users", nil, nil); code != http.StatusForbidden {
		t.Fatalf("outside the group = %d, want 403", code)
	}
	if code := listsDo(t, h, http.MethodGet, "/api/v1/lists", "sam", "lists_users", nil, nil); code != http.StatusOK {
		t.Fatalf("in the group = %d", code)
	}
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/items", "sam", "homepage_users",
		map[string]any{"list": "shopping", "items": []string{"milk"}}, nil); code != http.StatusForbidden {
		t.Fatalf("a write outside the group = %d, want 403", code)
	}
}

// Add, tick, untick, remove and clear from the card, recorded as the
// signed-in member and read back the way the Agents read it.
func TestTheListsCardReadsAndChangesTheHouseholdLists(t *testing.T) {
	h, _ := portalHarness(t)
	installLists(t, h)
	const groups = "lists_users"

	var change runtimedb.ListChange
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/items", "sam", groups,
		map[string]any{"list": "Shopping", "items": []string{"milk", "eggs", "bread"}}, &change); code != http.StatusOK ||
		!change.Created || len(change.Changed) != 3 {
		t.Fatalf("add = %d %+v", code, change)
	}
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/check", "kim", groups,
		map[string]any{"list": "shopping", "items": []string{"milk", "eggs"}}, &change); code != http.StatusOK || len(change.Changed) != 2 {
		t.Fatalf("tick = %d %+v", code, change)
	}
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/check", "kim", groups,
		map[string]any{"list": "shopping", "items": []string{"eggs"}, "checked": false}, &change); code != http.StatusOK || len(change.Changed) != 1 {
		t.Fatalf("untick = %d %+v", code, change)
	}
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/remove", "kim", groups,
		map[string]any{"list": "shopping", "items": []string{"bread"}}, &change); code != http.StatusOK || len(change.Changed) != 1 {
		t.Fatalf("remove = %d %+v", code, change)
	}

	var card listsCard
	if code := listsDo(t, h, http.MethodGet, "/api/v1/lists", "sam", groups, nil, &card); code != http.StatusOK || len(card.Lists) != 1 {
		t.Fatalf("read = %d %+v", code, card)
	}
	l := card.Lists[0]
	if l.Name != "Shopping" || l.Open != 1 || l.Done != 1 || len(l.Items) != 2 ||
		l.Items[0].Text != "eggs" || l.Items[0].AddedBy != "sam" || !l.Items[1].Checked || l.Items[1].CheckedBy != "kim" {
		t.Fatalf("card = %+v", l)
	}

	var cleared struct {
		Cleared int `json:"cleared"`
	}
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/clear", "sam", groups,
		map[string]any{"list": "shopping"}, &cleared); code != http.StatusOK || cleared.Cleared != 1 {
		t.Fatalf("clear = %d %+v", code, cleared)
	}

	// Bad requests are the request's fault, and say so.
	for name, tc := range map[string]struct {
		path string
		body map[string]any
		want int
	}{
		"no list":            {"/api/v1/lists/items", map[string]any{"items": []string{"milk"}}, http.StatusBadRequest},
		"no items":           {"/api/v1/lists/items", map[string]any{"list": "shopping", "items": []string{" "}}, http.StatusBadRequest},
		"a list not there":   {"/api/v1/lists/check", map[string]any{"list": "holiday", "items": []string{"x"}}, http.StatusNotFound},
		"an unknown field":   {"/api/v1/lists/items", map[string]any{"list": "shopping", "items": []string{"x"}, "by": "kim"}, http.StatusBadRequest},
		"clearing no list":   {"/api/v1/lists/clear", map[string]any{"list": "holiday"}, http.StatusNotFound},
		"removing from none": {"/api/v1/lists/remove", map[string]any{"list": "holiday", "items": []string{"x"}}, http.StatusNotFound},
	} {
		if code := listsDo(t, h, http.MethodPost, tc.path, "sam", groups, tc.body, nil); code != tc.want {
			t.Errorf("%s = %d, want %d", name, code, tc.want)
		}
	}
}

// The Home summary tells the page whether to draw the Lists card.
func TestTheHomeSummaryNamesTheListsSurface(t *testing.T) {
	h := homeHarness(t)
	summary := func() map[string]bool {
		var out struct {
			Surfaces map[string]bool `json:"surfaces"`
		}
		res := h.member(http.MethodGet, "/api/v1/home/summary", "sam", "homepage_users", nil, "")
		defer res.Body.Close()
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out.Surfaces
	}
	if summary()["lists"] {
		t.Fatal("the Lists card is offered before the package is installed")
	}
	installLists(t, h)
	if !summary()["lists"] {
		t.Fatal("the Lists card is not offered once the package is installed")
	}
}

// The Lists page is behind the same door as the card, and deleting a
// whole list — which the page asks about first — is too.
func TestTheListsPageAndDeletingAList(t *testing.T) {
	h, _ := portalHarness(t)
	page := func(groups string) int {
		res := h.member(http.MethodGet, "/lists/", "sam", groups, nil, "")
		res.Body.Close()
		return res.StatusCode
	}
	if code := page("lists_users"); code != http.StatusNotFound {
		t.Fatalf("the page before install = %d, want 404", code)
	}
	installLists(t, h)
	if code := page("homepage_users"); code != http.StatusForbidden {
		t.Fatalf("the page outside the group = %d, want 403", code)
	}
	if code := page("lists_users"); code != http.StatusOK {
		t.Fatalf("the page = %d", code)
	}

	const groups = "lists_users"
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/items", "sam", groups,
		map[string]any{"list": "Packing", "items": []string{"passport", "charger"}}, nil); code != http.StatusOK {
		t.Fatalf("add = %d", code)
	}
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/delete", "sam", "homepage_users",
		map[string]any{"list": "packing"}, nil); code != http.StatusForbidden {
		t.Fatalf("a delete outside the group = %d, want 403", code)
	}
	var out struct {
		List  string `json:"list"`
		Items int    `json:"items"`
	}
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/delete", "sam", groups,
		map[string]any{"list": "packing"}, &out); code != http.StatusOK || out.List != "Packing" || out.Items != 2 {
		t.Fatalf("delete = %d %+v", code, out)
	}
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/delete", "sam", groups,
		map[string]any{"list": "packing"}, nil); code != http.StatusNotFound {
		t.Fatalf("deleting it again = %d, want 404", code)
	}
	var card listsCard
	if listsDo(t, h, http.MethodGet, "/api/v1/lists", "sam", groups, nil, &card); len(card.Lists) != 0 {
		t.Fatalf("after delete = %+v", card)
	}
}

// #405: while PantryOS is installed it owns the grocery list, so the Lists
// page cannot grow or tick a grocery list of its own — that would be half a
// grocery list next to the assistant's. An old grocery list can still be
// cleared out and deleted, and other lists are untouched.
func TestWhilePantryOSIsInstalledTheGroceryListIsNotKeptHere(t *testing.T) {
	h, _ := portalHarness(t)
	installLists(t, h)
	const groups = "lists_users"
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/items", "sam", groups,
		map[string]any{"list": "Shopping", "items": []string{"milk", "eggs"}}, nil); code != http.StatusOK {
		t.Fatalf("before PantryOS: add = %d", code)
	}
	installPantry(t, h)

	var card listsCard
	if code := listsDo(t, h, http.MethodGet, "/api/v1/lists", "sam", groups, nil, &card); code != http.StatusOK || !card.GroceriesInPantryOS {
		t.Fatalf("read = %d %+v", code, card)
	}
	for _, write := range []struct {
		path string
		body map[string]any
	}{
		{"/api/v1/lists/items", map[string]any{"list": "shopping", "items": []string{"bread"}}},
		{"/api/v1/lists/items", map[string]any{"list": "Groceries", "items": []string{"bread"}}},
		{"/api/v1/lists/check", map[string]any{"list": "shopping", "items": []string{"milk"}}},
	} {
		var out map[string]any
		if code := listsDo(t, h, http.MethodPost, write.path, "sam", groups, write.body, &out); code != http.StatusConflict {
			t.Fatalf("%s %v = %d, want 409", write.path, write.body["list"], code)
		}
	}
	// Other lists are the Lists page's as ever.
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/items", "sam", groups,
		map[string]any{"list": "Chores", "items": []string{"bins"}}, nil); code != http.StatusOK {
		t.Fatalf("chores = %d", code)
	}
	// The old grocery list can be cleared out and deleted.
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/remove", "sam", groups,
		map[string]any{"list": "shopping", "items": []string{"milk"}}, nil); code != http.StatusOK {
		t.Fatalf("remove = %d", code)
	}
	if code := listsDo(t, h, http.MethodPost, "/api/v1/lists/delete", "sam", groups,
		map[string]any{"list": "shopping"}, nil); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
}
