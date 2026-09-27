package domestic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/pantry"
)

// scriptedPantry answers pantry-1 capabilities from a script, and
// records what it was asked.
type scriptedPantry struct {
	answers map[string]func(req map[string]any) (adapter.Answer, error)
	asked   []string
}

func (s *scriptedPantry) Name() string { return "scripted" }
func (s *scriptedPantry) Capabilities(context.Context) ([]adapter.Capability, error) {
	return nil, nil
}
func (s *scriptedPantry) Query(_ context.Context, capability string, raw json.RawMessage) (adapter.Answer, error) {
	return s.call("query "+capability, capability, raw)
}
func (s *scriptedPantry) Act(_ context.Context, capability string, raw json.RawMessage) (adapter.Answer, error) {
	return s.call("act "+capability, capability, raw)
}
func (s *scriptedPantry) call(label, capability string, raw json.RawMessage) (adapter.Answer, error) {
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	s.asked = append(s.asked, fmt.Sprintf("%s %v", label, req))
	f, ok := s.answers[capability]
	if !ok {
		return adapter.Answer{}, fmt.Errorf("%w: %s", adapter.ErrNoSuchCapability, capability)
	}
	return f(req)
}

func ok(data string) func(map[string]any) (adapter.Answer, error) {
	return func(map[string]any) (adapter.Answer, error) {
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			panic(err)
		}
		return adapter.Answer{Data: m}, nil
	}
}

func empty(data string) func(map[string]any) (adapter.Answer, error) {
	f := ok(data)
	return func(req map[string]any) (adapter.Answer, error) {
		a, err := f(req)
		a.Empty = true
		return a, err
	}
}

func failing(err error) func(map[string]any) (adapter.Answer, error) {
	return func(map[string]any) (adapter.Answer, error) { return adapter.Answer{}, err }
}

func pantryGateway(t *testing.T, up adapter.Upstream, installed bool) (*Gateway, *[]ConfirmRequest, *Confirmation) {
	t.Helper()
	asked := &[]ConfirmRequest{}
	answer := ConfirmAsked
	g := &Gateway{
		Authority: permittedExecution{},
		Declared: func(capability, tool string) (string, bool) {
			if capability != PantryCapability {
				return "", false
			}
			tier, ok := PantryTools[tool]
			return tier, ok
		},
		Confirm: func(_ context.Context, req ConfirmRequest) (Confirmation, error) {
			*asked = append(*asked, req)
			return answer, nil
		},
	}
	p := &Pantry{Authority: permittedExecution{}, Upstream: up, Enabled: func() (bool, error) { return installed, nil }}
	for _, e := range p.Executors() {
		g.Register(e)
	}
	return g, asked, &answer
}

func pantryCall(tool string, params map[string]string) Call {
	return Call{Tool: PantryCapability + "." + tool, Tier: PantryTools[tool], Params: params,
		Subject: "sam", Attended: true}
}

func TestPantryToolsNeedPantryOSInstalledAndConnected(t *testing.T) {
	g, _, _ := pantryGateway(t, &scriptedPantry{}, false)
	if _, err := g.Dispatch(context.Background(), pantryCall(PantryToolShowGroceries, nil)); err == nil ||
		!strings.Contains(err.Error(), "no PantryOS installed") {
		t.Fatalf("not installed = %v", err)
	}
	g, _, _ = pantryGateway(t, nil, true)
	if _, err := g.Dispatch(context.Background(), pantryCall(PantryToolShowGroceries, nil)); err == nil ||
		!strings.Contains(err.Error(), "not connected") {
		t.Fatalf("not connected = %v", err)
	}
}

// Empty, refused and down are three different sentences.
func TestGroceryListKeepsItsThreeAnswersApart(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		answer func(map[string]any) (adapter.Answer, error)
		want   string
		isErr  bool
	}{
		{"items", ok(`{"list":"groceries","items":["milk","bread"]}`), "On the grocery list (2): milk and bread.", false},
		{"empty", empty(`{"list":"groceries","items":[]}`), "The grocery list is empty.", false},
		{"down", failing(fmt.Errorf("%w: connection refused", adapter.ErrUnavailable)), "not answering", true},
		{"refused", failing(fmt.Errorf("%w: pantry.list: PantryOS keeps only the grocery list", pantry.ErrRejected)),
			"PantryOS said: PantryOS keeps only the grocery list", true},
		{"old pantryos", failing(fmt.Errorf("%w: pantry.list", adapter.ErrNoSuchCapability)), "may need updating", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){pantry.CapList: tc.answer}}
			g, _, _ := pantryGateway(t, up, true)
			r, err := g.Dispatch(ctx, pantryCall(PantryToolShowGroceries, nil))
			if tc.isErr {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil || r.Summary != tc.want {
				t.Fatalf("got %q %v, want %q", r.Summary, err, tc.want)
			}
		})
	}
}

func TestAddingGroceriesSaysWhatHappened(t *testing.T) {
	onList := map[string]bool{"milk": true}
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapAdd: func(req map[string]any) (adapter.Answer, error) {
			item := req["item"].(string)
			added := !onList[strings.ToLower(item)]
			onList[strings.ToLower(item)] = true
			return adapter.Answer{Data: map[string]any{"list": "groceries", "item": item, "added": added}}, nil
		},
	}}
	g, _, _ := pantryGateway(t, up, true)
	r, err := g.Dispatch(context.Background(), pantryCall(PantryToolAddGroceries, map[string]string{"items": "eggs, Milk, and bread"}))
	if err != nil {
		t.Fatal(err)
	}
	if !r.EffectConfirmed || r.Summary != "Added eggs and bread to the grocery list. Milk is already on it." {
		t.Fatalf("result = %+v", r)
	}
	if got := strings.Join(up.asked, " | "); !strings.Contains(got, "list:groceries") {
		t.Fatalf("asked = %s", got)
	}
}

func TestAFailedAddSaysWhatWasAlreadyAdded(t *testing.T) {
	calls := 0
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapAdd: func(req map[string]any) (adapter.Answer, error) {
			calls++
			if calls == 2 {
				return adapter.Answer{}, fmt.Errorf("%w: timeout", adapter.ErrUnavailable)
			}
			return adapter.Answer{Data: map[string]any{"item": req["item"], "added": true}}, nil
		},
	}}
	g, _, _ := pantryGateway(t, up, true)
	_, err := g.Dispatch(context.Background(), pantryCall(PantryToolAddGroceries, map[string]string{"items": "eggs, milk"}))
	if err == nil || !strings.Contains(err.Error(), "already added: eggs") {
		t.Fatalf("err = %v", err)
	}
}

const stepTwo = `{"session":{"recipe_name":"Flatbread","servings_made":"2","step_number":2,"total_steps":3,
  "step_text":"Rest ten minutes","is_last_step":false,"step_ingredients":[]}}`
const lastStep = `{"session":{"recipe_name":"Flatbread","servings_made":"2","step_number":3,"total_steps":3,
  "step_text":"Bake with the flour.","is_last_step":true,
  "step_ingredients":[{"item":"Flour","quantity":"4","unit":"g"}]}}`

func TestTheCookAlongReadsSteps(t *testing.T) {
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapCookStart:   ok(strings.Replace(stepTwo, `"step_number":2`, `"step_number":1`, 1)),
		pantry.CapCookStep:    ok(lastStep),
		pantry.CapCookCurrent: empty(`{"session":null}`),
	}}
	g, _, _ := pantryGateway(t, up, true)
	ctx := context.Background()

	r, err := g.Dispatch(ctx, pantryCall(PantryToolStartCooking, map[string]string{"recipe_id": "r1", "servings": "2"}))
	if err != nil || r.Summary != "Starting Flatbread (2 servings). Flatbread, step 1 of 3: Rest ten minutes." {
		t.Fatalf("start = %q %v", r.Summary, err)
	}
	r, err = g.Dispatch(ctx, pantryCall(PantryToolCookingStep, map[string]string{"action": "next"}))
	if err != nil || !strings.Contains(r.Summary, "step 3 of 3: Bake with the flour (uses 4 g Flour).") ||
		!strings.Contains(r.Summary, "last step") {
		t.Fatalf("next = %q %v", r.Summary, err)
	}
	if _, err := g.Dispatch(ctx, pantryCall(PantryToolCookingStep, map[string]string{"action": "sideways"})); err == nil {
		t.Fatal("an unknown step action was sent to PantryOS")
	}
	if _, err := g.Dispatch(ctx, pantryCall(PantryToolCookingStep, map[string]string{"action": "goto"})); err == nil {
		t.Fatal("goto with no step was sent to PantryOS")
	}
	r, err = g.Dispatch(ctx, pantryCall(PantryToolCurrentStep, nil))
	if err != nil || r.Summary != "Nothing is being cooked right now." {
		t.Fatalf("current = %q %v", r.Summary, err)
	}
	// Starting and stepping changed nothing in the pantry, so none of it
	// claims an effect.
	if r.EffectConfirmed {
		t.Fatal("a read claimed an effect")
	}
}

func TestFinishingACookAsksFirstNamingTheDish(t *testing.T) {
	finished := false
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapCookCurrent: ok(lastStep),
		pantry.CapCookFinish: func(map[string]any) (adapter.Answer, error) {
			finished = true
			return adapter.Answer{Data: map[string]any{
				"session": map[string]any{"recipe_name": "Flatbread"},
				"cook":    map[string]any{"cook_event_id": "c1", "capped_ingredients": []any{}},
			}}, nil
		},
	}}
	g, asked, answer := pantryGateway(t, up, true)
	ctx := context.Background()
	finish := pantryCall(PantryToolFinishCooking, nil)

	_, err := g.Dispatch(ctx, finish)
	var refusal *RefusalError
	if !errors.As(err, &refusal) || len(*asked) != 1 ||
		(*asked)[0].Question != "Finish cooking Flatbread (2 servings)? This takes its ingredients out of the pantry." {
		t.Fatalf("first ask = %v, questions %+v", err, *asked)
	}
	if finished {
		t.Fatal("stock was taken before the person answered")
	}

	*answer = ConfirmApproved
	r, err := g.Dispatch(ctx, finish)
	if err != nil || !r.EffectConfirmed || !finished ||
		r.Summary != "Finished cooking Flatbread and took its ingredients out of the pantry." {
		t.Fatalf("approved finish = %+v %v", r, err)
	}
}

func TestFinishingWithNothingCookingIsNotAsked(t *testing.T) {
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapCookCurrent: empty(`{"session":null}`),
	}}
	g, asked, _ := pantryGateway(t, up, true)
	if _, err := g.Dispatch(context.Background(), pantryCall(PantryToolFinishCooking, nil)); err == nil {
		t.Fatal("finishing nothing succeeded")
	}
	if len(*asked) != 0 {
		t.Fatalf("a person was asked to finish a cook that is not happening: %+v", *asked)
	}
}

func TestPantryReadsAreSpoken(t *testing.T) {
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapSummary:   ok(`{"within_days":7,"expiring_soon":2,"expired":1,"low_stock_items":0,"shopping_pending":3}`),
		pantry.CapInventory: ok(`{"inventory":[{"item":"Eggs","count":6,"unit":"each","locations":["Fridge"]}]}`),
		pantry.CapExpiring:  ok(`{"items":[{"item":"Milk","days_left":1},{"item":"Yogurt","days_left":0}]}`),
		pantry.CapRecipes: ok(`{"recipes":[{"recipe_id":"r1","recipe_name":"Omelette","missing_items":[]},
		  {"recipe_id":"r2","recipe_name":"Chili","missing_items":["onion"]}]}`),
	}}
	g, _, _ := pantryGateway(t, up, true)
	ctx := context.Background()
	for tool, want := range map[string]string{
		PantryToolSummary: "Pantry: 2 things expiring in the next 7 days, 1 thing already past its date, " +
			"0 things running low, 3 things on the grocery list.",
		PantryToolCheckStock:     "In stock — Eggs: 6 (Fridge).",
		PantryToolExpiring:       "Expiring in the next 7 days: Milk (tomorrow) and Yogurt (today).",
		PantryToolSuggestRecipes: "You could make: Omelette [recipe_id r1]; Chili (missing onion) [recipe_id r2].",
	} {
		r, err := g.Dispatch(ctx, pantryCall(tool, nil))
		if err != nil || r.Summary != want {
			t.Errorf("%s = %q %v\nwant %q", tool, r.Summary, err, want)
		}
	}
}

// Writes go through the household execution check; reads do not need it.
func TestPantryWritesAreHeldByTheExecutionAuthority(t *testing.T) {
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapAdd: ok(`{"item":"milk","added":true}`),
	}}
	p := &Pantry{Upstream: up} // no Authority wired
	for _, e := range p.Executors() {
		if e.Name() != PantryCapability+"."+PantryToolAddGroceries {
			continue
		}
		if _, err := e.Execute(context.Background(), pantryCall(PantryToolAddGroceries, map[string]string{"items": "milk"})); err == nil {
			t.Fatal("a write ran with no execution authority")
		}
	}
	if len(up.asked) != 0 {
		t.Fatalf("PantryOS was asked: %v", up.asked)
	}
}
