package domestic

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// listsGateway is the real gateway over the lists executors and a real
// runtime database, declared exactly as the package declares them.
func listsGateway(t *testing.T, enabled bool) (*Gateway, *runtimedb.DB, *[]ConfirmRequest, *Confirmation) {
	t.Helper()
	db, err := runtimedb.Open(migrated.Into(t, filepath.Join(t.TempDir(), "lists.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	asked := &[]ConfirmRequest{}
	answer := ConfirmAsked
	g := &Gateway{
		Authority: permittedExecution{},
		Declared: func(capability, tool string) (string, bool) {
			if capability != ListsCapability {
				return "", false
			}
			tier, ok := ListsTools[tool]
			return tier, ok
		},
		Confirm: func(_ context.Context, req ConfirmRequest) (Confirmation, error) {
			*asked = append(*asked, req)
			return answer, nil
		},
	}
	lists := &Lists{Authority: permittedExecution{}, Store: db, Enabled: func() (bool, error) { return enabled, nil }}
	for _, e := range lists.Executors() {
		g.Register(e)
	}
	return g, db, asked, &answer
}

func listCall(tool string, params map[string]string) Call {
	return Call{Tool: ListsCapability + "." + tool, Tier: ListsTools[tool], Params: params,
		Subject: "sam", Attended: true}
}

func TestAListFromFirstItemToDone(t *testing.T) {
	g, db, _, _ := listsGateway(t, true)
	ctx := context.Background()
	do := func(tool string, params map[string]string) Result {
		t.Helper()
		r, err := g.Dispatch(ctx, listCall(tool, params))
		if err != nil {
			t.Fatalf("%s %v: %v", tool, params, err)
		}
		return r
	}

	if r := do(ListToolShowAll, nil); !strings.Contains(r.Summary, "no lists yet") || r.EffectConfirmed {
		t.Fatalf("no lists = %+v", r)
	}
	r := do(ListToolAdd, map[string]string{"list": "Shopping", "items": "milk, eggs, and bread"})
	if r.Summary != "Added milk, eggs and bread to Shopping (a new list)." || !r.EffectConfirmed {
		t.Fatalf("first add = %+v", r)
	}
	r = do(ListToolAdd, map[string]string{"list": "shopping", "items": "Milk\ncoffee"})
	if r.Summary != "Added coffee to Shopping. Milk is already on Shopping." || !r.EffectConfirmed {
		t.Fatalf("second add = %+v", r)
	}
	r = do(ListToolCheck, map[string]string{"list": "shopping", "items": "milk, butter"})
	if r.Summary != "Ticked off milk on Shopping. Butter is not on Shopping." || !r.EffectConfirmed {
		t.Fatalf("tick = %+v", r)
	}
	r = do(ListToolShow, map[string]string{"list": "shopping"})
	if r.Summary != "Shopping — still to do: eggs, bread and coffee. Ticked off: milk." || r.EffectConfirmed {
		t.Fatalf("show = %+v", r)
	}
	if r = do(ListToolShowAll, nil); r.Summary != "The household has 1 list: Shopping (3 left, 1 ticked off)." {
		t.Fatalf("show all = %+v", r)
	}
	r = do(ListToolRemove, map[string]string{"list": "shopping", "items": "coffee"})
	if r.Summary != "Took coffee off Shopping." || !r.EffectConfirmed {
		t.Fatalf("remove = %+v", r)
	}
	r = do(ListToolClearChecked, map[string]string{"list": "shopping"})
	if r.Summary != "Cleared 1 item ticked off from Shopping." || !r.EffectConfirmed {
		t.Fatalf("clear = %+v", r)
	}

	// Who added and ticked is recorded from the session, not a param.
	l, err := db.HouseholdList("shopping")
	if err != nil || len(l.Items) != 2 || l.Items[0].AddedBy != "sam" {
		t.Fatalf("stored = %+v %v", l, err)
	}
}

// A write that changed nothing it was asked about has no receipt: there
// is nothing for it to vouch for.
func TestAWriteThatFoundNothingHasNoReceipt(t *testing.T) {
	g, _, _, _ := listsGateway(t, true)
	ctx := context.Background()
	if _, err := g.Dispatch(ctx, listCall(ListToolAdd, map[string]string{"list": "shopping", "items": "milk"})); err != nil {
		t.Fatal(err)
	}
	r, err := g.Dispatch(ctx, listCall(ListToolCheck, map[string]string{"list": "shopping", "items": "caviar"}))
	if err != nil || r.EffectConfirmed || r.Summary != "Caviar is not on shopping." {
		t.Fatalf("ticking something not there = %+v %v", r, err)
	}
	if _, err := g.Dispatch(ctx, listCall(ListToolShow, map[string]string{"list": "holiday"})); err == nil ||
		!strings.Contains(err.Error(), `no list called "holiday"`) {
		t.Fatalf("a list that is not there = %v", err)
	}
}

func TestListsRefuseWhenNotInstalledOrUnderspecified(t *testing.T) {
	off, _, _, _ := listsGateway(t, false)
	if _, err := off.Dispatch(context.Background(), listCall(ListToolShowAll, nil)); err == nil ||
		!strings.Contains(err.Error(), "no lists installed") {
		t.Fatalf("uninstalled = %v", err)
	}
	g, _, _, _ := listsGateway(t, true)
	for name, c := range map[string]Call{
		"no list":  listCall(ListToolAdd, map[string]string{"items": "milk"}),
		"no items": listCall(ListToolAdd, map[string]string{"list": "shopping", "items": " , ,"}),
		"no show":  listCall(ListToolShow, nil),
	} {
		if _, err := g.Dispatch(context.Background(), c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Deleting a whole list asks first, about the list by name and size, and
// deletes nothing until the answer is yes. A rule cannot carry it.
func TestDeletingAListAsksFirst(t *testing.T) {
	g, db, asked, answer := listsGateway(t, true)
	ctx := context.Background()
	if _, err := g.Dispatch(ctx, listCall(ListToolAdd, map[string]string{"list": "Packing", "items": "passport, charger"})); err != nil {
		t.Fatal(err)
	}
	del := listCall(ListToolDelete, map[string]string{"list": "packing"})

	_, err := g.Dispatch(ctx, del)
	var refusal *RefusalError
	if !errors.As(err, &refusal) || len(*asked) != 1 || (*asked)[0].Question != `Delete the list "Packing" and the 2 items on it?` {
		t.Fatalf("first ask = %v, questions %+v", err, *asked)
	}
	if _, err := db.HouseholdList("packing"); err != nil {
		t.Fatalf("deleted before the answer: %v", err)
	}

	*answer = ConfirmApproved
	r, err := g.Dispatch(ctx, del)
	if err != nil || !r.EffectConfirmed || r.Summary != "Deleted the list Packing and the 2 items on it." {
		t.Fatalf("approved delete = %+v %v", r, err)
	}
	if _, err := db.HouseholdList("packing"); !errors.Is(err, runtimedb.ErrListNotFound) {
		t.Fatalf("after delete = %v", err)
	}

	unattended := listCall(ListToolDelete, map[string]string{"list": "shopping"})
	unattended.Attended = false
	if _, err := g.Dispatch(ctx, unattended); err == nil {
		t.Fatal("a rule deleted a list")
	}
}

func TestSplittingItems(t *testing.T) {
	got := splitItems("milk, eggs and bread;\n\n  butter  ")
	want := []string{"milk", "eggs and bread", "butter"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		// "and" is only split when it starts an item: "eggs and bread" as
		// one item is fine, and a model separating with commas is the
		// documented shape.
		t.Fatalf("split = %q, want %q", got, want)
	}
	if got := splitItems("salt, and pepper"); strings.Join(got, "|") != "salt|pepper" {
		t.Fatalf("leading and = %q", got)
	}
}
