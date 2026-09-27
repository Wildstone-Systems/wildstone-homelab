package domestic

// The PantryOS executors: the household's pantry, grocery list and
// cook-along, as an Agent reaches them (ADR-W042 A5, #382).
//
// They are a thin layer over the pantry adapter (keystone/adapter/pantry,
// contract pantry-1). PantryOS does the pantry logic — what "already on
// the list" means, how a recipe splits into steps, what finishing a cook
// takes out of stock. These executors only choose the capability, pass
// the params, and turn the answer into a sentence.
//
// THE SUMMARY IS THE WHOLE ANSWER, as for the lists executors: a family
// session hands the model Result.Summary and nothing else, so every
// summary is something a person can be read — "Step 2 of 5: add the
// onions and cook until soft (uses 1 onion)."
//
// THREE ANSWERS, NEVER CONFUSED. PantryOS having nothing to report,
// PantryOS refusing the question, and PantryOS not answering are
// different sentences, because a household told "the list is empty"
// when the pantry was simply down will not buy milk.
//
// ONLY FINISHING TOUCHES STOCK. Starting, stepping and stopping a
// cook-along change nothing in the pantry, so they are `read`, the same
// as PantryOS proposes. finish_cooking removes every ingredient at once
// and is `confirm`: the person is asked, naming the recipe, and a rule
// firing on its own cannot carry it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/pantry"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// PantryCapability is the `metadata.capability` of packages/pantry-pantryos.
// TestPantryToolsAreDeclaredByThePackage pins the two together.
const PantryCapability = "pantry"

// Pantry tool names, without the capability prefix.
const (
	PantryToolSummary        = "pantry_summary"
	PantryToolCheckStock     = "check_stock"
	PantryToolExpiring       = "whats_expiring"
	PantryToolShowGroceries  = "show_grocery_list"
	PantryToolSuggestRecipes = "suggest_recipes"
	PantryToolGetRecipe      = "get_recipe"
	PantryToolStartCooking   = "start_cooking"
	PantryToolCurrentStep    = "current_step"
	PantryToolCookingStep    = "cooking_step"
	PantryToolCompleteStep   = "complete_step"
	PantryToolReadSteps      = "read_steps"
	PantryToolSetTimer       = "set_timer"
	PantryToolListTimers     = "list_timers"
	PantryToolCancelTimer    = "cancel_timer"
	PantryToolStopCooking    = "stop_cooking"
	PantryToolAddGroceries   = "add_to_grocery_list"
	PantryToolCheckGroceries = "check_grocery_item"
	PantryToolFinishCooking  = "finish_cooking"
)

// PantryTools maps each tool to the tier it must be declared at
// (ADR-W042 A5).
var PantryTools = map[string]string{
	PantryToolSummary:        TierRead,
	PantryToolCheckStock:     TierRead,
	PantryToolExpiring:       TierRead,
	PantryToolShowGroceries:  TierRead,
	PantryToolSuggestRecipes: TierRead,
	PantryToolGetRecipe:      TierRead,
	PantryToolStartCooking:   TierRead,
	PantryToolCurrentStep:    TierRead,
	PantryToolCookingStep:    TierRead,
	// Ticking steps and kitchen timers change nothing in the pantry, like
	// moving a step (ADR-W042 A5.1).
	PantryToolCompleteStep:   TierRead,
	PantryToolReadSteps:      TierRead,
	PantryToolSetTimer:       TierRead,
	PantryToolListTimers:     TierRead,
	PantryToolCancelTimer:    TierRead,
	PantryToolStopCooking:    TierRead,
	PantryToolAddGroceries:   TierDomestic,
	PantryToolCheckGroceries: TierDomestic,
	PantryToolFinishCooking:  TierConfirm,
}

// pantryUsage is each tool's params, in the words a model reads.
var pantryUsage = map[string]string{
	PantryToolSummary:    "A one-line picture of the pantry: what is expiring, what is running low, how long the grocery list is. No params.",
	PantryToolCheckStock: "What is in stock. params: item (optional; part of a name, e.g. eggs — omit for everything).",
	PantryToolExpiring:   "Food that expires soon. params: days (optional, default 7).",
	PantryToolShowGroceries: "What is on the grocery list. No params. The grocery list lives in PantryOS; " +
		"other lists (chores, packing) are the household lists tools.",
	PantryToolSuggestRecipes: "Recipes that can be made from what is in stock, best first, with each one's recipe_id. No params.",
	PantryToolGetRecipe:      "One recipe's ingredients and steps. params: recipe_id (required, from suggest_recipes).",
	PantryToolStartCooking: "Start cooking a recipe step by step; answers with step 1. Takes nothing out of the pantry. " +
		"params: recipe_id (required), servings (optional; servings to make, default the recipe as written).",
	PantryToolCurrentStep: "Which step of the recipe being cooked we are on (\"where was I?\"). No params.",
	PantryToolCookingStep: "Move through the recipe being cooked without ticking anything off. params: action (required: " +
		"next, previous, repeat or goto), step (required for goto; 1 is the first step).",
	PantryToolCompleteStep: "Tick off a step of the recipe being cooked (\"done, what's next?\") and read the next step not " +
		"yet done. params: step (optional; default the step we are on). The kitchen tablet shows the tick at once.",
	PantryToolReadSteps: "Read every step of the recipe being cooked, saying which are done — for \"what's left?\" or " +
		"\"read me the whole thing\". No params.",
	PantryToolSetTimer: "Start a kitchen timer; the kitchen tablet shows it and rings. While a recipe is being cooked, the " +
		"timer is tied to its step, and the step may suggest a time (\"simmer 20 minutes\"). params: minutes (required; " +
		"may be a fraction, 0.5 is 30 seconds), label (optional, e.g. pasta).",
	PantryToolListTimers: "The kitchen timers running or ringing, with the time left on each. No params.",
	PantryToolCancelTimer: "Stop a kitchen timer: cancels one still running, silences one ringing. params: label (optional; " +
		"which timer, e.g. pasta — omit when there is only one).",
	PantryToolStopCooking:    "Stop cooking without finishing; nothing is taken out of the pantry. No params.",
	PantryToolAddGroceries:   "Put items on the grocery list; an item already on it is not added twice. params: items (required; separate several with commas).",
	PantryToolCheckGroceries: "Tick items off the grocery list (bought). params: items (required; separate several with commas).",
	PantryToolFinishCooking: "Finish the recipe being cooked: takes its ingredients out of the pantry; the person is asked first. " +
		"params: location_id (optional).",
}

// maxSpokenRecipes bounds how many suggestions one summary reads out.
const maxSpokenRecipes = 5

// Pantry is PantryOS as the action gateway sees it. One instance backs
// every tool; Executors returns the registrations.
type Pantry struct {
	Authority HouseholdExecutionAuthority
	// Upstream is PantryOS: in production a *pantry.HTTP, configured by
	// the operator (-pantry-url and PANTRY_AGENT_KEY).
	Upstream adapter.Upstream
	// Enabled reports whether the pantry capability is INSTALLED, asked
	// per call, as Lists.Enabled. nil means "do not ask".
	Enabled func() (bool, error)
}

// Executors returns one registration per tool, in a stable order.
func (p *Pantry) Executors() []Executor {
	names := make([]string, 0, len(PantryTools))
	for name := range PantryTools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Executor, 0, len(names))
	for _, name := range names {
		out = append(out, &pantryTool{pantry: p, tool: name})
	}
	return out
}

type pantryTool struct {
	pantry *Pantry
	tool   string
}

func (t *pantryTool) Name() string { return PantryCapability + "." + t.tool }

// Usage implements UsageDescriber.
func (t *pantryTool) Usage() string { return pantryUsage[t.tool] }

func (t *pantryTool) Execute(ctx context.Context, c Call) (Result, error) {
	if PantryTools[t.tool] != TierRead {
		if err := checkExecution(ctx, t.pantry.Authority); err != nil {
			return Result{}, err
		}
		if err := runtimedb.CheckPrivacyDelivery(ctx, runtimedb.MemoryDestination{}); err != nil {
			return Result{}, err
		}
	}
	if err := t.pantry.ready(); err != nil {
		return Result{}, err
	}
	p := t.pantry
	switch t.tool {
	case PantryToolSummary:
		return p.summary(ctx)
	case PantryToolCheckStock:
		return p.checkStock(ctx, c)
	case PantryToolExpiring:
		return p.expiring(ctx, c)
	case PantryToolShowGroceries:
		return p.showGroceries(ctx)
	case PantryToolSuggestRecipes:
		return p.suggestRecipes(ctx)
	case PantryToolGetRecipe:
		return p.getRecipe(ctx, c)
	case PantryToolStartCooking:
		return p.startCooking(ctx, c)
	case PantryToolCurrentStep:
		return p.currentStep(ctx)
	case PantryToolCookingStep:
		return p.cookingStep(ctx, c)
	case PantryToolCompleteStep:
		return p.completeStep(ctx, c)
	case PantryToolReadSteps:
		return p.readSteps(ctx)
	case PantryToolSetTimer:
		return p.setTimer(ctx, c)
	case PantryToolListTimers:
		return p.listTimers(ctx)
	case PantryToolCancelTimer:
		return p.cancelTimer(ctx, c)
	case PantryToolStopCooking:
		return p.stopCooking(ctx)
	case PantryToolAddGroceries:
		return p.addGroceries(ctx, c)
	case PantryToolCheckGroceries:
		return p.checkGroceries(ctx, c)
	case PantryToolFinishCooking:
		return p.finishCooking(ctx, c)
	}
	return Result{}, fmt.Errorf("pantry has no tool %q", t.tool)
}

// Describe implements Describer for the confirm-tier finish, so the
// person is asked about a dish rather than a tool call. It reads the
// cook-along first: nobody is asked to finish a cook that is not
// happening.
func (t *pantryTool) Describe(ctx context.Context, _ Call) (string, error) {
	if t.tool != PantryToolFinishCooking {
		return "", nil
	}
	if err := t.pantry.ready(); err != nil {
		return "", err
	}
	s, err := t.pantry.session(ctx)
	if err != nil {
		return "", err
	}
	if s == nil {
		return "", errors.New("nothing is being cooked, so there is nothing to finish")
	}
	return fmt.Sprintf("Finish cooking %s%s? This takes its ingredients out of the pantry.",
		s.Recipe, servingsPhrase(s.Servings)), nil
}

// Installed reports whether the household has PantryOS INSTALLED — the
// question grocery routing turns on (ADR-W042 A3). Not "is it answering":
// a PantryOS that is installed but down still owns the grocery list, and
// the answer to a grocery request is then that it is down, never a quiet
// write to some other list. Installed-but-not-connected likewise routes
// here and says so.
func (p *Pantry) Installed() (bool, error) {
	if p == nil {
		return false, nil
	}
	if p.Enabled == nil {
		return p.Upstream != nil, nil
	}
	return p.Enabled()
}

func (p *Pantry) ready() error {
	if p == nil || p.Upstream == nil {
		return errors.New("PantryOS is not connected to this control plane")
	}
	if p.Enabled == nil {
		return nil
	}
	on, err := p.Enabled()
	if err != nil {
		return fmt.Errorf("checking whether the household has PantryOS: %w", err)
	}
	if !on {
		return errors.New("this household has no PantryOS installed")
	}
	return nil
}

// ask sends one query or action and keeps the three answers apart.
func (p *Pantry) ask(ctx context.Context, act bool, capability string, req map[string]any) (map[string]any, bool, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, false, err
	}
	var ans adapter.Answer
	if act {
		ans, err = p.Upstream.Act(ctx, capability, raw)
	} else {
		ans, err = p.Upstream.Query(ctx, capability, raw)
	}
	switch {
	case errors.Is(err, pantry.ErrNotConnected):
		return nil, false, errors.New("PantryOS is installed but not connected yet — an Owner needs to pair it (or set -pantry-url and PANTRY_AGENT_KEY)")
	case errors.Is(err, pantry.ErrRejected):
		// PantryOS understood and said no: its reason is the answer.
		return nil, false, fmt.Errorf("PantryOS said: %s", rejection(err))
	case errors.Is(err, adapter.ErrNoSuchCapability):
		return nil, false, errors.New("this PantryOS does not offer that yet; it may need updating")
	case errors.Is(err, adapter.ErrUnavailable):
		return nil, false, errors.New("PantryOS is not answering right now, so I can't tell — nothing was changed")
	case err != nil:
		return nil, false, err
	}
	data, _ := ans.Data.(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return data, ans.Empty, nil
}

// rejection is PantryOS's own words, without the error chain around them.
func rejection(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		msg = msg[i+2:]
	}
	return msg
}

// --- reads ---

func (p *Pantry) summary(ctx context.Context) (Result, error) {
	d, _, err := p.ask(ctx, false, pantry.CapSummary, nil)
	if err != nil {
		return Result{}, err
	}
	days := intOf(d["within_days"], 7)
	parts := []string{
		fmt.Sprintf("%s expiring in the next %d days", countOf(intOf(d["expiring_soon"], 0)), days),
	}
	if n := intOf(d["expired"], 0); n > 0 {
		parts = append(parts, fmt.Sprintf("%s already past %s date", countOf(n), itsTheir(n)))
	}
	parts = append(parts,
		fmt.Sprintf("%s running low", countOf(intOf(d["low_stock_items"], 0))),
		fmt.Sprintf("%s on the grocery list", countOf(intOf(d["shopping_pending"], 0))))
	return Result{Summary: "Pantry: " + strings.Join(parts, ", ") + "."}, nil
}

func (p *Pantry) checkStock(ctx context.Context, c Call) (Result, error) {
	search := strings.TrimSpace(c.Params["item"])
	req := map[string]any{}
	if search != "" {
		req["search"] = search
	}
	d, empty, err := p.ask(ctx, false, pantry.CapInventory, req)
	if err != nil {
		return Result{}, err
	}
	rows := listOf(d["inventory"])
	if empty || len(rows) == 0 {
		if search != "" {
			return Result{Summary: fmt.Sprintf("There is no %s in the pantry.", search)}, nil
		}
		return Result{Summary: "The pantry has nothing in stock."}, nil
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		line := fmt.Sprintf("%s: %s", str(r["item"]), amount(r["count"], str(r["unit"])))
		if locs := strs(r["locations"]); len(locs) > 0 {
			line += " (" + strings.Join(locs, ", ") + ")"
		}
		lines = append(lines, line)
	}
	return Result{Summary: "In stock — " + spoken(lines) + "."}, nil
}

func (p *Pantry) expiring(ctx context.Context, c Call) (Result, error) {
	days := 7
	if v := strings.TrimSpace(c.Params["days"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 90 {
			return Result{}, fmt.Errorf("days must be a whole number from 0 to 90, not %q", v)
		}
		days = n
	}
	d, empty, err := p.ask(ctx, false, pantry.CapExpiring, map[string]any{"within_days": days})
	if err != nil {
		return Result{}, err
	}
	rows := listOf(d["items"])
	if empty || len(rows) == 0 {
		return Result{Summary: fmt.Sprintf("Nothing in the pantry expires in the next %d days.", days)}, nil
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%s (%s)", str(r["item"]), whenLeft(intOf(r["days_left"], 0))))
	}
	return Result{Summary: fmt.Sprintf("Expiring in the next %d days: %s.", days, spoken(lines))}, nil
}

// ShowGroceries reads PantryOS's grocery list (see AddGroceries).
func (p *Pantry) ShowGroceries(ctx context.Context) (Result, error) {
	if err := p.ready(); err != nil {
		return Result{}, err
	}
	return p.showGroceries(ctx)
}

func (p *Pantry) showGroceries(ctx context.Context) (Result, error) {
	d, empty, err := p.ask(ctx, false, pantry.CapList, map[string]any{"list": "groceries"})
	if err != nil {
		return Result{}, err
	}
	items := strs(d["items"])
	if empty || len(items) == 0 {
		return Result{Summary: "The grocery list is empty."}, nil
	}
	return Result{Summary: fmt.Sprintf("On the grocery list (%d): %s.", len(items), spoken(items))}, nil
}

func (p *Pantry) suggestRecipes(ctx context.Context) (Result, error) {
	d, empty, err := p.ask(ctx, false, pantry.CapRecipes, nil)
	if err != nil {
		return Result{}, err
	}
	rows := listOf(d["recipes"])
	if empty || len(rows) == 0 {
		return Result{Summary: "PantryOS has no recipes it can suggest from what is in stock."}, nil
	}
	if len(rows) > maxSpokenRecipes {
		rows = rows[:maxSpokenRecipes]
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		line := str(r["recipe_name"])
		if missing := strs(r["missing_items"]); len(missing) > 0 {
			line += " (missing " + spoken(missing) + ")"
		}
		lines = append(lines, line+" [recipe_id "+str(r["recipe_id"])+"]")
	}
	return Result{Summary: "You could make: " + strings.Join(lines, "; ") + "."}, nil
}

func (p *Pantry) getRecipe(ctx context.Context, c Call) (Result, error) {
	id := strings.TrimSpace(c.Params["recipe_id"])
	if id == "" {
		return Result{}, errors.New("get_recipe needs a recipe_id (suggest_recipes lists them)")
	}
	d, _, err := p.ask(ctx, false, pantry.CapRecipe, map[string]any{"recipe_id": id})
	if err != nil {
		return Result{}, err
	}
	var ingredients []string
	for _, r := range listOf(d["ingredients"]) {
		ingredients = append(ingredients, amount(r["quantity"], str(r["unit"]))+" "+str(r["item"]))
	}
	steps := strs(d["steps"])
	var b strings.Builder
	fmt.Fprintf(&b, "%s (serves %d).", str(d["recipe"]), intOf(d["servings"], 1))
	if len(ingredients) > 0 {
		fmt.Fprintf(&b, " Ingredients: %s.", spoken(ingredients))
	}
	for i, s := range steps {
		fmt.Fprintf(&b, " %d. %s", i+1, s)
	}
	return Result{Summary: b.String()}, nil
}

// --- the cook-along ---

// cookSession is the part of a pantry-1 session these summaries read.
type cookSession struct {
	Recipe      string
	Servings    float64
	Step, Total int
	Text        string
	Last        bool
	Uses        []string
	// Done is whether the current step is ticked off; Left is how many
	// steps are not; Steps is every step, for read_steps.
	Done  bool
	Left  int
	Steps []cookStep
	// Suggest is the timer the step's own words name ("20 minutes").
	Suggest string
}

type cookStep struct {
	Number int
	Text   string
	Done   bool
}

func sessionOf(v any) *cookSession {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil
	}
	s := &cookSession{
		Recipe: str(m["recipe_name"]),
		Step:   intOf(m["step_number"], 0),
		Total:  intOf(m["total_steps"], 0),
		Text:   str(m["step_text"]),
		Last:   m["is_last_step"] == true,
		Done:   m["step_done"] == true,
		Left:   intOf(m["steps_left"], -1),
	}
	for _, st := range listOf(m["steps"]) {
		s.Steps = append(s.Steps, cookStep{Number: intOf(st["number"], 0), Text: str(st["text"]), Done: st["done"] == true})
	}
	if timers := listOf(m["step_timers"]); len(timers) > 0 {
		s.Suggest = str(timers[0]["label"])
	}
	if f, ok := number(m["servings_made"]); ok {
		s.Servings = f
	}
	for _, r := range listOf(m["step_ingredients"]) {
		s.Uses = append(s.Uses, amount(r["quantity"], str(r["unit"]))+" "+str(r["item"]))
	}
	return s
}

// read is one step, spoken.
func (s *cookSession) read() string {
	if s.Total == 0 {
		return fmt.Sprintf("%s has no written steps.", s.Recipe)
	}
	out := fmt.Sprintf("%s, step %d of %d: %s", s.Recipe, s.Step, s.Total, s.Text)
	if len(s.Uses) > 0 {
		out = strings.TrimRight(out, ". ") + " (uses " + spoken(s.Uses) + ")"
	}
	if !strings.HasSuffix(out, ".") {
		out += "."
	}
	if s.Done {
		out += " (Already done.)"
	}
	if s.Suggest != "" {
		out += fmt.Sprintf(" I can set a %s timer for this step.", s.Suggest)
	}
	switch {
	case s.Left == 0:
		out += " That's every step done — say when to finish, and I'll take the ingredients out of the pantry."
	case s.Last:
		out += " That's the last step — say when it's done and I'll take the ingredients out of the pantry."
	}
	return out
}

func (p *Pantry) session(ctx context.Context) (*cookSession, error) {
	d, empty, err := p.ask(ctx, false, pantry.CapCookCurrent, nil)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, nil
	}
	return sessionOf(d["session"]), nil
}

func (p *Pantry) startCooking(ctx context.Context, c Call) (Result, error) {
	id := strings.TrimSpace(c.Params["recipe_id"])
	if id == "" {
		return Result{}, errors.New("start_cooking needs a recipe_id (suggest_recipes lists them)")
	}
	req := map[string]any{"recipe_id": id}
	if v := strings.TrimSpace(c.Params["servings"]); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 {
			return Result{}, fmt.Errorf("servings must be a number above zero, not %q", v)
		}
		req["servings"] = n
	}
	d, _, err := p.ask(ctx, true, pantry.CapCookStart, req)
	if err != nil {
		return Result{}, err
	}
	s := sessionOf(d["session"])
	if s == nil {
		return Result{}, errors.New("PantryOS started cooking but did not say what the first step is")
	}
	return Result{Summary: "Starting " + s.Recipe + servingsPhrase(s.Servings) + ". " + s.read()}, nil
}

func (p *Pantry) currentStep(ctx context.Context) (Result, error) {
	s, err := p.session(ctx)
	if err != nil {
		return Result{}, err
	}
	if s == nil {
		return Result{Summary: "Nothing is being cooked right now."}, nil
	}
	return Result{Summary: s.read()}, nil
}

func (p *Pantry) cookingStep(ctx context.Context, c Call) (Result, error) {
	action := strings.ToLower(strings.TrimSpace(c.Params["action"]))
	req := map[string]any{"action": action}
	switch action {
	case "next", "previous", "repeat":
	case "complete":
		return p.completeStep(ctx, c)
	case "goto":
		n, err := strconv.Atoi(strings.TrimSpace(c.Params["step"]))
		if err != nil || n < 1 {
			return Result{}, errors.New("goto needs a step number, 1 or more")
		}
		req["step"] = n
	default:
		return Result{}, errors.New("action must be next, previous, repeat or goto")
	}
	d, _, err := p.ask(ctx, true, pantry.CapCookStep, req)
	if err != nil {
		return Result{}, err
	}
	s := sessionOf(d["session"])
	if s == nil {
		return Result{}, errors.New("PantryOS moved the step but did not say which it is now")
	}
	return Result{Summary: s.read()}, nil
}

// completeStep ticks a step off and reads the next one not yet done.
func (p *Pantry) completeStep(ctx context.Context, c Call) (Result, error) {
	req := map[string]any{"action": "complete"}
	if v := strings.TrimSpace(c.Params["step"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Result{}, errors.New("step is a step number, 1 or more")
		}
		req["step"] = n
	}
	d, _, err := p.ask(ctx, true, pantry.CapCookStep, req)
	if err != nil {
		return Result{}, err
	}
	s := sessionOf(d["session"])
	if s == nil {
		return Result{}, errors.New("PantryOS ticked the step but did not say which step is next")
	}
	if s.Left == 0 {
		return Result{Summary: fmt.Sprintf("Done — that's every step of %s. Say when to finish, and I'll take the "+
			"ingredients out of the pantry.", s.Recipe)}, nil
	}
	return Result{Summary: "Done. Next — " + s.read()}, nil
}

// readSteps reads the whole recipe being cooked, marking what is done.
func (p *Pantry) readSteps(ctx context.Context) (Result, error) {
	s, err := p.session(ctx)
	if err != nil {
		return Result{}, err
	}
	if s == nil {
		return Result{Summary: "Nothing is being cooked right now."}, nil
	}
	if len(s.Steps) == 0 {
		return Result{Summary: fmt.Sprintf("%s has no written steps.", s.Recipe)}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d steps", s.Recipe, len(s.Steps))
	if s.Left >= 0 {
		fmt.Fprintf(&b, ", %d left", s.Left)
	}
	b.WriteString(".")
	for _, st := range s.Steps {
		mark := ""
		if st.Done {
			mark = " (done)"
		}
		fmt.Fprintf(&b, " Step %d%s: %s", st.Number, mark, strings.TrimSpace(st.Text))
		if !strings.HasSuffix(b.String(), ".") {
			b.WriteString(".")
		}
	}
	return Result{Summary: b.String()}, nil
}

// maxTimerMinutes matches PantryOS's own bound: a day.
const maxTimerMinutes = 24 * 60

func (p *Pantry) setTimer(ctx context.Context, c Call) (Result, error) {
	raw := strings.TrimSpace(c.Params["minutes"])
	minutes, err := strconv.ParseFloat(raw, 64)
	if err != nil || minutes <= 0 || minutes > maxTimerMinutes {
		return Result{}, fmt.Errorf("minutes must be a number above zero and at most a day, not %q", raw)
	}
	seconds := int(minutes*60 + 0.5)
	if seconds < 1 {
		seconds = 1
	}
	req := map[string]any{"seconds": seconds}
	if label := strings.TrimSpace(c.Params["label"]); label != "" {
		req["label"] = label
	}
	d, _, err := p.ask(ctx, true, pantry.CapTimerSet, req)
	if err != nil {
		return Result{}, err
	}
	t, _ := d["timer"].(map[string]any)
	return Result{EffectConfirmed: true, Summary: fmt.Sprintf("Started a %s timer for %s. The kitchen tablet will ring.",
		duration(seconds), timerName(t)), Detail: map[string]string{"timer_id": str(t["id"])}}, nil
}

func (p *Pantry) listTimers(ctx context.Context) (Result, error) {
	d, empty, err := p.ask(ctx, false, pantry.CapTimers, nil)
	if err != nil {
		return Result{}, err
	}
	timers := listOf(d["timers"])
	if empty || len(timers) == 0 {
		return Result{Summary: "No kitchen timers are running."}, nil
	}
	parts := make([]string, 0, len(timers))
	for _, t := range timers {
		if str(t["state"]) == "ringing" {
			parts = append(parts, timerName(t)+" is ringing")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s has %s left", timerName(t), duration(intOf(t["remaining_seconds"], 0))))
	}
	return Result{Summary: capitalise(spoken(parts)) + "."}, nil
}

func (p *Pantry) cancelTimer(ctx context.Context, c Call) (Result, error) {
	req := map[string]any{}
	if label := strings.TrimSpace(c.Params["label"]); label != "" {
		req["label"] = label
	}
	d, _, err := p.ask(ctx, true, pantry.CapTimerCancel, req)
	if err != nil {
		return Result{}, err
	}
	t, _ := d["timer"].(map[string]any)
	verb := "Cancelled"
	if str(t["stopped"]) == "dismissed" {
		verb = "Silenced"
	}
	return Result{EffectConfirmed: true, Summary: fmt.Sprintf("%s the %s timer.", verb, timerName(t))}, nil
}

// timerName is how a timer is spoken of: its label, as given.
func timerName(t map[string]any) string {
	if name := strings.TrimSpace(str(t["label"])); name != "" {
		return name
	}
	return "kitchen"
}

// duration speaks a number of seconds: "9 minutes", "1 hour 30 minutes",
// "45 seconds".
func duration(seconds int) string {
	if seconds < 60 {
		return counted(seconds, "second")
	}
	h, m := seconds/3600, (seconds%3600+30)/60
	if m == 60 {
		h, m = h+1, 0
	}
	switch {
	case h == 0:
		return counted(m, "minute")
	case m == 0:
		return counted(h, "hour")
	}
	return counted(h, "hour") + " " + counted(m, "minute")
}

func counted(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (p *Pantry) stopCooking(ctx context.Context) (Result, error) {
	d, empty, err := p.ask(ctx, true, pantry.CapCookAbandon, nil)
	if err != nil {
		return Result{}, err
	}
	if empty {
		return Result{Summary: "Nothing was being cooked."}, nil
	}
	s := sessionOf(d["session"])
	name := "the recipe"
	if s != nil && s.Recipe != "" {
		name = s.Recipe
	}
	return Result{Summary: fmt.Sprintf("Stopped cooking %s. Nothing was taken out of the pantry.", name)}, nil
}

// --- writes ---

func (p *Pantry) addGroceries(ctx context.Context, c Call) (Result, error) {
	items := splitItems(c.Params["items"])
	if len(items) == 0 {
		return Result{}, errors.New("add_to_grocery_list needs items")
	}
	return p.AddGroceries(ctx, items)
}

// AddGroceries puts items on PantryOS's grocery list. Exported for the
// household lists, which send a grocery list here while PantryOS is
// installed (ADR-W042 A3).
func (p *Pantry) AddGroceries(ctx context.Context, items []string) (Result, error) {
	if err := p.ready(); err != nil {
		return Result{}, err
	}
	var added, already []string
	for _, item := range items {
		d, _, err := p.ask(ctx, true, pantry.CapAdd, map[string]any{"list": "groceries", "item": item})
		if err != nil {
			return Result{}, partial(err, "added", added)
		}
		if d["added"] == true {
			added = append(added, str(d["item"]))
		} else {
			already = append(already, str(d["item"]))
		}
	}
	var b strings.Builder
	if len(added) > 0 {
		fmt.Fprintf(&b, "Added %s to the grocery list.", spoken(added))
	}
	if len(already) > 0 {
		sep(&b)
		fmt.Fprintf(&b, "%s %s already on it.", capitalise(spoken(already)), isAre(len(already)))
	}
	return Result{EffectConfirmed: true, Summary: b.String(), Detail: map[string]string{
		"list": "groceries", "added": strings.Join(added, ", "), "unchanged": strings.Join(already, ", "),
	}}, nil
}

func (p *Pantry) checkGroceries(ctx context.Context, c Call) (Result, error) {
	items := splitItems(c.Params["items"])
	if len(items) == 0 {
		return Result{}, errors.New("check_grocery_item needs items")
	}
	return p.CheckGroceries(ctx, items)
}

// CheckGroceries ticks items off PantryOS's grocery list (see AddGroceries).
func (p *Pantry) CheckGroceries(ctx context.Context, items []string) (Result, error) {
	if err := p.ready(); err != nil {
		return Result{}, err
	}
	var checked, missing []string
	for _, item := range items {
		d, _, err := p.ask(ctx, true, pantry.CapCheck, map[string]any{"list": "groceries", "item": item})
		if err != nil {
			return Result{}, partial(err, "ticked off", checked)
		}
		if d["checked"] == true {
			checked = append(checked, str(d["item"]))
		} else {
			missing = append(missing, item)
		}
	}
	var b strings.Builder
	if len(checked) > 0 {
		fmt.Fprintf(&b, "Ticked off %s on the grocery list.", spoken(checked))
	}
	if len(missing) > 0 {
		sep(&b)
		fmt.Fprintf(&b, "%s %s not on the grocery list.", capitalise(spoken(missing)), isAre(len(missing)))
	}
	return Result{EffectConfirmed: len(checked) > 0, Summary: b.String(), Detail: map[string]string{
		"list": "groceries", "changed": strings.Join(checked, ", "), "missing": strings.Join(missing, ", "),
	}}, nil
}

func (p *Pantry) finishCooking(ctx context.Context, c Call) (Result, error) {
	req := map[string]any{}
	if loc := strings.TrimSpace(c.Params["location_id"]); loc != "" {
		req["location_id"] = loc
	}
	d, _, err := p.ask(ctx, true, pantry.CapCookFinish, req)
	if err != nil {
		return Result{}, err
	}
	name := "the recipe"
	if s := sessionOf(d["session"]); s != nil && s.Recipe != "" {
		name = s.Recipe
	}
	cook, _ := d["cook"].(map[string]any)
	summary := fmt.Sprintf("Finished cooking %s and took its ingredients out of the pantry.", name)
	if capped := listOf(cook["capped_ingredients"]); len(capped) > 0 {
		summary += fmt.Sprintf(" %s ran short and used what was there.", countOf(len(capped)))
	}
	return Result{EffectConfirmed: true, Summary: summary, Detail: map[string]string{
		"cook_event_id": str(cook["cook_event_id"]),
	}}, nil
}

// partial names what a multi-item write had already done when it failed,
// so "it failed" is never mistaken for "nothing happened".
func partial(err error, verb string, done []string) error {
	if len(done) == 0 {
		return err
	}
	return fmt.Errorf("%w (already %s: %s)", err, verb, spoken(done))
}

// --- reading pantry-1 data ---

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func strs(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s := str(x); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func listOf(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func intOf(v any, fallback int) int {
	if f, ok := number(v); ok {
		return int(f)
	}
	return fallback
}

func amount(q any, unit string) string {
	s := str(q)
	if unit == "" || unit == "each" {
		return s
	}
	return s + " " + unit
}

func servingsPhrase(n float64) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%s servings)", strconv.FormatFloat(n, 'f', -1, 64))
}

func whenLeft(days int) string {
	switch days {
	case 0:
		return "today"
	case 1:
		return "tomorrow"
	}
	return fmt.Sprintf("in %d days", days)
}

func itsTheir(n int) string {
	if n == 1 {
		return "its"
	}
	return "their"
}
