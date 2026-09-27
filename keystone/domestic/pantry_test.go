package domestic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
const lastStep = `{"session":{"id":"s-flatbread","recipe_name":"Flatbread","servings_made":"2","step_number":3,"total_steps":3,
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

// A cook-along session as PantryOS answers it after "done" on step 1 of 3,
// with the step's own words suggesting a timer (ADR-W044 A5.1).
const afterTickingOne = `{"session":{"recipe_name":"Pasta","step_number":2,"total_steps":3,
 "step_text":"Cook the pasta 9 minutes.","is_last_step":false,"step_done":false,"steps_left":2,
 "step_timers":[{"seconds":540,"label":"9 minutes"}],
 "steps":[{"number":1,"text":"Boil the water.","done":true},{"number":2,"text":"Cook the pasta 9 minutes.","done":false},
          {"number":3,"text":"Drain.","done":false}]}}`

func TestTickingOffAStepReadsTheNextOne(t *testing.T) {
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapCookStep:    ok(afterTickingOne),
		pantry.CapCookCurrent: ok(afterTickingOne),
	}}
	g, asked, _ := pantryGateway(t, up, true)
	ctx := context.Background()

	r, err := g.Dispatch(ctx, pantryCall(PantryToolCompleteStep, nil))
	want := "Done. Next — Pasta, step 2 of 3: Cook the pasta 9 minutes. I can set a 9 minutes timer for this step."
	if err != nil || r.Summary != want {
		t.Fatalf("complete = %q %v", r.Summary, err)
	}
	if got := up.asked[0]; got != "act pantry.cook.step map[action:complete]" {
		t.Fatalf("PantryOS was asked %q", got)
	}
	// cooking_step's "complete" is the same thing.
	if _, err := g.Dispatch(ctx, pantryCall(PantryToolCookingStep, map[string]string{"action": "complete"})); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Dispatch(ctx, pantryCall(PantryToolCompleteStep, map[string]string{"step": "zero"})); err == nil {
		t.Fatal("a step that is not a number was sent to PantryOS")
	}

	r, err = g.Dispatch(ctx, pantryCall(PantryToolReadSteps, nil))
	want = "Pasta has 3 steps, 2 left. Step 1 (done): Boil the water. Step 2: Cook the pasta 9 minutes. Step 3: Drain."
	if err != nil || r.Summary != want {
		t.Fatalf("read steps = %q %v", r.Summary, err)
	}
	// Ticking steps is a read-tier act: nobody was asked to confirm it.
	if len(*asked) != 0 {
		t.Fatalf("ticking a step asked for confirmation: %v", *asked)
	}
}

func TestTheLastTickSaysEverythingIsDone(t *testing.T) {
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapCookStep: ok(`{"session":{"recipe_name":"Pasta","step_number":3,"total_steps":3,"step_text":"Drain.",
			"is_last_step":true,"step_done":true,"steps_left":0}}`),
	}}
	g, _, _ := pantryGateway(t, up, true)
	r, err := g.Dispatch(context.Background(), pantryCall(PantryToolCompleteStep, nil))
	if err != nil || !strings.HasPrefix(r.Summary, "Done — that's every step of Pasta.") {
		t.Fatalf("last tick = %q %v", r.Summary, err)
	}
}

func TestKitchenTimers(t *testing.T) {
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapTimerSet: ok(`{"timer":{"id":"t1","label":"Pasta","remaining_seconds":540,"state":"running"}}`),
		pantry.CapTimers: ok(`{"timers":[{"label":"Garlic bread","remaining_seconds":0,"state":"ringing"},
			{"label":"Pasta","remaining_seconds":5390,"state":"running"}]}`),
		pantry.CapTimerCancel: ok(`{"timer":{"label":"Pasta","stopped":"cancelled"}}`),
	}}
	g, _, _ := pantryGateway(t, up, true)
	ctx := context.Background()

	r, err := g.Dispatch(ctx, pantryCall(PantryToolSetTimer, map[string]string{"minutes": "9", "label": "pasta"}))
	if err != nil || r.Summary != "Started a 9 minutes timer for Pasta. The kitchen tablet will ring." || !r.EffectConfirmed {
		t.Fatalf("set = %q %v", r.Summary, err)
	}
	// Sam asked, so Sam is who PantryOS will say the timer went off for.
	if got := up.asked[0]; got != "act pantry.timer.set map[label:pasta member:sam seconds:540]" {
		t.Fatalf("PantryOS was asked %q", got)
	}
	for _, bad := range []string{"", "0", "-3", "soon", "2000", "NaN", "Inf", "-Inf"} {
		if _, err := g.Dispatch(ctx, pantryCall(PantryToolSetTimer, map[string]string{"minutes": bad})); err == nil {
			t.Fatalf("minutes %q was sent to PantryOS", bad)
		}
	}

	r, err = g.Dispatch(ctx, pantryCall(PantryToolListTimers, nil))
	if err != nil || r.Summary != "Garlic bread is ringing and Pasta has 1 hour 30 minutes left." {
		t.Fatalf("list = %q %v", r.Summary, err)
	}
	r, err = g.Dispatch(ctx, pantryCall(PantryToolCancelTimer, map[string]string{"label": "pasta"}))
	if err != nil || r.Summary != "Cancelled the Pasta timer." {
		t.Fatalf("cancel = %q %v", r.Summary, err)
	}
}

func TestNoTimersIsSaidPlainly(t *testing.T) {
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapTimers: empty(`{"timers":[]}`),
	}}
	g, _, _ := pantryGateway(t, up, true)
	r, err := g.Dispatch(context.Background(), pantryCall(PantryToolListTimers, nil))
	if err != nil || r.Summary != "No kitchen timers are running." {
		t.Fatalf("no timers = %q %v", r.Summary, err)
	}
}

func TestDurationsAreSpoken(t *testing.T) {
	for in, want := range map[int]string{
		1: "1 second", 45: "45 seconds", 60: "1 minute", 540: "9 minutes", 3600: "1 hour",
		5400: "1 hour 30 minutes", 7170: "2 hours",
	} {
		if got := duration(in); got != want {
			t.Errorf("duration(%d) = %q, want %q", in, got, want)
		}
	}
}

// An answer PantryOS gave from what it last saw is still said — but with
// when it was seen, never as if it were current. Both through a tool and
// through the grocery routing that calls ShowGroceries directly.
func TestAStaleAnswerSaysHowOldItIs(t *testing.T) {
	ctx := context.Background()
	seen := time.Date(2026, 9, 27, 8, 30, 0, 0, time.UTC)
	stale := func(map[string]any) (adapter.Answer, error) {
		return adapter.Answer{Data: map[string]any{"list": "groceries", "items": []any{"milk"}}, Stale: true, ObservedAt: seen}, nil
	}
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){pantry.CapList: stale}}
	g, _, _ := pantryGateway(t, up, true)
	r, err := g.Dispatch(ctx, pantryCall(PantryToolShowGroceries, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Summary, "On the grocery list (1): milk.") || !strings.Contains(r.Summary, "may be out of date") {
		t.Fatalf("summary = %q", r.Summary)
	}
	if r.Detail["stale"] != "true" || r.Detail["observed_at"] != "2026-09-27T08:30:00Z" {
		t.Fatalf("detail = %v", r.Detail)
	}

	p := &Pantry{Authority: permittedExecution{}, Upstream: up}
	direct, err := p.ShowGroceries(ctx)
	if err != nil || !strings.Contains(direct.Summary, "may be out of date") {
		t.Fatalf("ShowGroceries = %q %v", direct.Summary, err)
	}

	// A current answer carries no warning.
	up.answers[pantry.CapList] = ok(`{"list":"groceries","items":["milk"]}`)
	if r, _ := g.Dispatch(ctx, pantryCall(PantryToolShowGroceries, nil)); strings.Contains(r.Summary, "out of date") {
		t.Fatalf("current answer warned: %q", r.Summary)
	}
}

// An approval is for the dish it named. If the tablet switches to another
// cook after the question was asked, the retry is a different call — a new
// question with a new consent key — and the approved finish can only ever be
// sent for the session it was about.
func TestAnApprovedFinishCannotFinishADifferentCook(t *testing.T) {
	current := lastStep
	var finishedSession any
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapCookCurrent: func(req map[string]any) (adapter.Answer, error) { return ok(current)(req) },
		pantry.CapCookFinish: func(req map[string]any) (adapter.Answer, error) {
			finishedSession = req["session_id"]
			return adapter.Answer{Data: map[string]any{
				"session": map[string]any{"recipe_name": "Flatbread"},
				"cook":    map[string]any{"cook_event_id": "c1"},
			}}, nil
		},
	}}
	g, asked, answer := pantryGateway(t, up, true)
	ctx := context.Background()
	finish := pantryCall(PantryToolFinishCooking, nil)

	if _, err := g.Dispatch(ctx, finish); err == nil {
		t.Fatal("finished without asking")
	}
	*answer = ConfirmApproved // the person said yes — to Flatbread

	// Meanwhile the tablet stopped Flatbread and started Pasta.
	current = `{"session":{"id":"s-pasta","recipe_name":"Pasta","servings_made":"4","step_number":1,"total_steps":2,"step_text":"Boil."}}`
	*answer = ConfirmAsked
	if _, err := g.Dispatch(ctx, finish); err == nil {
		t.Fatal("the Flatbread approval finished Pasta")
	}
	if len(*asked) != 2 || (*asked)[0].Key == (*asked)[1].Key || !strings.Contains((*asked)[1].Question, "Pasta") {
		t.Fatalf("questions = %+v", *asked)
	}
	if finishedSession != nil {
		t.Fatalf("PantryOS was asked to finish %v without approval", finishedSession)
	}

	// Approved for Pasta: PantryOS is asked to finish exactly that session.
	*answer = ConfirmApproved
	if _, err := g.Dispatch(ctx, finish); err != nil {
		t.Fatal(err)
	}
	if finishedSession != "s-pasta" {
		t.Fatalf("finished session = %v, want s-pasta", finishedSession)
	}
}

// The question names the cook the approval is bound to. If the tablet
// switches dishes between binding the call and asking, the person is still
// shown — and approves — the dish whose id is in the key.
func TestTheFinishQuestionNamesTheBoundCook(t *testing.T) {
	reads := 0
	pasta := `{"session":{"id":"s-pasta","recipe_name":"Pasta","servings_made":"4","step_number":1,"total_steps":2,"step_text":"Boil."}}`
	up := &scriptedPantry{answers: map[string]func(map[string]any) (adapter.Answer, error){
		pantry.CapCookCurrent: func(req map[string]any) (adapter.Answer, error) {
			reads++
			if reads == 1 {
				return ok(lastStep)(req) // bound: Flatbread
			}
			return ok(pasta)(req) // the tablet switched right after
		},
	}}
	g, asked, _ := pantryGateway(t, up, true)
	_, _ = g.Dispatch(context.Background(), pantryCall(PantryToolFinishCooking, nil))
	if len(*asked) != 1 || !strings.Contains((*asked)[0].Question, "Flatbread") {
		t.Fatalf("asked %+v; the question must name the bound cook (Flatbread)", *asked)
	}
}
