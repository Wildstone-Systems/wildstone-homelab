package domestic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/pantry"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// The grocery list belongs to PantryOS while PantryOS is installed, and to
// the household lists otherwise (ADR-W044 A3, #383).

// groceryGateway is the lists gateway with a PantryOS behind it.
func groceryGateway(t *testing.T, up adapter.Upstream, pantryInstalled bool) (*Gateway, *runtimedb.DB) {
	t.Helper()
	g, db, _, _ := listsGateway(t, true)
	lists := &Lists{Authority: permittedExecution{}, Store: db, Enabled: func() (bool, error) { return true, nil },
		Groceries: &Pantry{Authority: permittedExecution{}, Upstream: up,
			Enabled: func() (bool, error) { return pantryInstalled, nil }},
	}
	for _, e := range lists.Executors() {
		g.Register(e)
	}
	return g, db
}

func pantryAdds(onList ...string) *scriptedPantry {
	have := map[string]bool{}
	for _, item := range onList {
		have[item] = true
	}
	return &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapAdd: func(req map[string]any) (adapter.Answer, error) {
			item := req["item"].(string)
			added := !have[item]
			have[item] = true
			return adapter.Answer{Data: map[string]any{"item": item, "added": added}}, nil
		},
		pantry.CapCheck: ok(`{"item":"milk","checked":true}`),
		pantry.CapList:  ok(`{"list":"groceries","items":["milk"]}`),
	}}
}

func localItems(t *testing.T, db *runtimedb.DB, name string) []string {
	t.Helper()
	l, err := db.HouseholdList(name)
	if errors.Is(err, runtimedb.ErrListNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range l.Items {
		out = append(out, it.Text)
	}
	return out
}

func TestGroceriesGoToPantryOSWhileItIsInstalled(t *testing.T) {
	up := pantryAdds()
	g, db := groceryGateway(t, up, true)
	ctx := context.Background()
	for _, name := range []string{"shopping", "Groceries", "grocery list"} {
		up.asked = nil
		r, err := g.Dispatch(ctx, listCall(ListToolAdd, map[string]string{"list": name, "items": "milk"}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(up.asked) != 1 || !strings.Contains(up.asked[0], "act pantry.add") {
			t.Fatalf("%s: PantryOS was asked %v", name, up.asked)
		}
		if !r.EffectConfirmed {
			t.Fatalf("%s: result = %+v", name, r)
		}
	}
	if got := localItems(t, db, "shopping"); len(got) != 0 {
		t.Fatalf("groceries were also written to the household list: %v", got)
	}

	r, err := g.Dispatch(ctx, listCall(ListToolShow, map[string]string{"list": "shopping"}))
	if err != nil || r.Summary != "On the grocery list (1): milk." {
		t.Fatalf("show = %q %v", r.Summary, err)
	}
	r, err = g.Dispatch(ctx, listCall(ListToolCheck, map[string]string{"list": "shopping", "items": "milk"}))
	if err != nil || r.Summary != "Ticked off milk on the grocery list." {
		t.Fatalf("check = %q %v", r.Summary, err)
	}
	r, err = g.Dispatch(ctx, listCall(ListToolShowAll, nil))
	if err != nil || !strings.HasSuffix(r.Summary, "The grocery list is kept in PantryOS.") {
		t.Fatalf("overview = %q %v", r.Summary, err)
	}
}

func TestOtherListsStayInTheHousehold(t *testing.T) {
	up := pantryAdds()
	g, db := groceryGateway(t, up, true)
	if _, err := g.Dispatch(context.Background(), listCall(ListToolAdd, map[string]string{"list": "chores", "items": "bins"})); err != nil {
		t.Fatal(err)
	}
	if len(up.asked) != 0 {
		t.Fatalf("a chores list went to PantryOS: %v", up.asked)
	}
	if got := localItems(t, db, "chores"); len(got) != 1 || got[0] != "bins" {
		t.Fatalf("chores = %v", got)
	}
}

func TestWithoutPantryOSGroceriesStayInTheHousehold(t *testing.T) {
	up := pantryAdds()
	g, db := groceryGateway(t, up, false)
	if _, err := g.Dispatch(context.Background(), listCall(ListToolAdd, map[string]string{"list": "shopping", "items": "milk"})); err != nil {
		t.Fatal(err)
	}
	if len(up.asked) != 0 {
		t.Fatalf("PantryOS was asked while not installed: %v", up.asked)
	}
	if got := localItems(t, db, "shopping"); len(got) != 1 {
		t.Fatalf("shopping = %v", got)
	}
}

// The case the routing rule exists for: installed but not answering is
// reported, never quietly written somewhere else.
func TestAnInstalledPantryOSThatIsDownIsNeverFallenBackFrom(t *testing.T) {
	down := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapAdd:  failing(fmt.Errorf("%w: connection refused", adapter.ErrUnavailable)),
		pantry.CapList: failing(fmt.Errorf("%w: connection refused", adapter.ErrUnavailable)),
	}}
	g, db := groceryGateway(t, down, true)
	ctx := context.Background()
	_, err := g.Dispatch(ctx, listCall(ListToolAdd, map[string]string{"list": "shopping", "items": "milk"}))
	if err == nil || !strings.Contains(err.Error(), "PantryOS is not answering") {
		t.Fatalf("err = %v", err)
	}
	if _, err := g.Dispatch(ctx, listCall(ListToolShow, map[string]string{"list": "shopping"})); err == nil ||
		!strings.Contains(err.Error(), "not answering") {
		t.Fatalf("show while down = %v", err)
	}
	if got := localItems(t, db, "shopping"); len(got) != 0 {
		t.Fatalf("fell back to the household list: %v", got)
	}

	// Installed but never connected is the same: PantryOS's list, and it
	// says why it cannot answer.
	g, db = groceryGateway(t, nil, true)
	if _, err := g.Dispatch(ctx, listCall(ListToolAdd, map[string]string{"list": "groceries", "items": "milk"})); err == nil ||
		!strings.Contains(err.Error(), "not connected") {
		t.Fatalf("not connected = %v", err)
	}
	if got := localItems(t, db, "groceries"); len(got) != 0 {
		t.Fatalf("fell back to the household list: %v", got)
	}
}

func TestGroceryToolsPantryOSHasNoMatchForAreRefusedPlainly(t *testing.T) {
	g, db := groceryGateway(t, pantryAdds(), true)
	ctx := context.Background()
	// An old household "shopping" list exists from before PantryOS.
	if _, err := db.AddListItems("shopping", []string{"old milk"}, "sam"); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{ListToolRemove, ListToolClearChecked} {
		_, err := g.Dispatch(ctx, listCall(tool, map[string]string{"list": "shopping", "items": "old milk"}))
		if err == nil || !strings.Contains(err.Error(), "lives in PantryOS") {
			t.Fatalf("%s = %v", tool, err)
		}
	}
	_, asked, _ := listsGatewayAsks(t, g)
	_, err := g.Dispatch(ctx, listCall(ListToolDelete, map[string]string{"list": "shopping"}))
	if err == nil || !strings.Contains(err.Error(), "lives in PantryOS") {
		t.Fatalf("delete = %v", err)
	}
	if len(*asked) != 0 {
		t.Fatalf("a person was asked to delete a list that lives in PantryOS: %+v", *asked)
	}
	if got := localItems(t, db, "shopping"); len(got) != 1 {
		t.Fatalf("the old list was touched: %v", got)
	}
}

// listsGatewayAsks swaps in a recording Confirm, so a test can see
// whether anyone was asked.
func listsGatewayAsks(t *testing.T, g *Gateway) (*Gateway, *[]ConfirmRequest, *Confirmation) {
	t.Helper()
	asked := &[]ConfirmRequest{}
	answer := ConfirmAsked
	g.Confirm = func(_ context.Context, req ConfirmRequest) (Confirmation, error) {
		*asked = append(*asked, req)
		return answer, nil
	}
	return g, asked, &answer
}
