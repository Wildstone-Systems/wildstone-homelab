package domestic

// The household lists executors: shopping, chores, packing — the actions
// that make `packages/lists-wildstone` usable by an Agent the moment it
// is installed.
//
// They follow the filedrop executors' rules, with one of their own.
//
// THE SUMMARY IS THE WHOLE ANSWER. A family session hands the model
// Result.Summary and nothing else, so every summary is a sentence a
// person can be read: "Added milk and eggs to Shopping. Bread was
// already on it."
//
// A WRITE SAYS WHAT IT DID, NOT WHAT IT WAS ASKED. The store reports each
// item as changed, already so, or not on the list, and the summary says
// all three. A receipt (EffectConfirmed) is issued when at least one
// named item is now in the state asked for — which is what the objective
// check binds a request like "add milk to the shopping list" to. An item
// that was already there is confirmed too: the list does have milk on it.
//
// LISTS ARE THE HOUSEHOLD'S. There is no per-person list and no
// parameter that names a person; Call.Subject is recorded as who added
// or ticked an item, for the Lists card to show, and decides nothing.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// ListsCapability is the capability these executors belong to: the
// `metadata.capability` of packages/lists-wildstone.
// TestListsToolsAreDeclaredByThePackage pins the two together.
const ListsCapability = "lists"

// List tool names, without the capability prefix.
const (
	ListToolShowAll      = "show_lists"
	ListToolShow         = "show_list"
	ListToolAdd          = "add_items"
	ListToolCheck        = "check_items"
	ListToolRemove       = "remove_items"
	ListToolClearChecked = "clear_checked"
	ListToolDelete       = "delete_list"
)

// ListsTools maps each tool to the tier it must be declared at.
//
// Adding, ticking and removing items are `domestic`: each is one small,
// visible change the same tools can undo. Deleting a whole list is
// `confirm` (ADR-W024 A5.1): it takes everything on it at once, and a
// rule firing at 3am cannot carry it at all.
var ListsTools = map[string]string{
	ListToolShowAll:      TierRead,
	ListToolShow:         TierRead,
	ListToolAdd:          TierDomestic,
	ListToolCheck:        TierDomestic,
	ListToolRemove:       TierDomestic,
	ListToolClearChecked: TierDomestic,
	ListToolDelete:       TierConfirm,
}

// maxSpokenItems bounds how many items one summary names. Past it the
// summary gives a count, because a model handed a hundred lines will read
// a hundred lines back to somebody.
const maxSpokenItems = 40

// ListStore is the household lists as these executors reach them.
// *runtimedb.DB is the implementation; keystone/domestic does not open
// the database itself.
type ListStore interface {
	HouseholdLists() ([]runtimedb.HouseholdList, error)
	HouseholdList(name string) (runtimedb.HouseholdList, error)
	AddListItems(list string, items []string, by string) (runtimedb.ListChange, error)
	SetListItemsChecked(list string, items []string, checked bool, by string) (runtimedb.ListChange, error)
	RemoveListItems(list string, items []string) (runtimedb.ListChange, error)
	ClearCheckedListItems(list string) (string, int, error)
	DeleteHouseholdList(list string) (string, int, error)
}

// Lists is the household's lists, as the action gateway sees them. One
// instance backs every tool; Executors returns the registrations.
type Lists struct {
	Authority HouseholdExecutionAuthority
	Store     ListStore
	// Enabled reports whether this deployment has the lists capability
	// INSTALLED, asked per call — so installing it works without a
	// restart and removing it stops the tools at once (ADR-W026 A8's
	// posture, as Filedrop.Enabled). nil means "do not ask".
	Enabled func() (bool, error)
	// Groceries, when set, is where the grocery list goes while PantryOS
	// is installed (ADR-W042 A3, #383). nil keeps every list here.
	Groceries GroceryList
}

// GroceryList is a grocery list kept by another capability — PantryOS,
// in practice; *Pantry is the implementation.
type GroceryList interface {
	// Installed is the routing question: is the owner installed? Asked
	// per call, never answered from whether the last call worked.
	Installed() (bool, error)
	ShowGroceries(ctx context.Context) (Result, error)
	AddGroceries(ctx context.Context, items []string) (Result, error)
	CheckGroceries(ctx context.Context, items []string) (Result, error)
}

// groceryListNames are the list names that mean the grocery list. The
// household lists call it "shopping" (it is their own example), PantryOS
// calls it "groceries"; a person says either.
var groceryListNames = map[string]bool{
	"groceries": true, "grocery": true, "shopping": true,
	"grocery list": true, "shopping list": true, "groceries list": true,
}

func isGroceryList(name string) bool {
	return groceryListNames[strings.ToLower(strings.Join(strings.Fields(name), " "))]
}

// errGroceriesElsewhere answers the list tools PantryOS has no match for.
var errGroceriesElsewhere = errors.New("the grocery list lives in PantryOS, which can show it, add items and " +
	"tick them off; removing or clearing items is done in PantryOS itself")

// routeGroceries sends a call about the grocery list to PantryOS while
// PantryOS is installed (ADR-W042 A3). routed=false means the call is this
// store's to answer. There is no fallback: when PantryOS is installed, a
// grocery call is PantryOS's whatever happens next — a PantryOS that is
// down is reported down, because quietly writing here would leave the
// household two half-lists.
func (l *Lists) routeGroceries(ctx context.Context, tool string, c Call) (bool, Result, error) {
	if l.Groceries == nil || tool == ListToolShowAll {
		return false, Result{}, nil
	}
	name := strings.TrimSpace(c.Params["list"])
	if name == "" || !isGroceryList(name) {
		return false, Result{}, nil
	}
	installed, err := l.Groceries.Installed()
	if err != nil {
		return true, Result{}, fmt.Errorf("checking whether the household has PantryOS: %w", err)
	}
	if !installed {
		return false, Result{}, nil
	}
	switch tool {
	case ListToolShow:
		r, err := l.Groceries.ShowGroceries(ctx)
		return true, r, err
	case ListToolAdd, ListToolCheck:
		items := splitItems(c.Params["items"])
		if len(items) == 0 {
			return true, Result{}, fmt.Errorf("param %q is required: which items (separate several with commas)", "items")
		}
		if tool == ListToolAdd {
			r, err := l.Groceries.AddGroceries(ctx, items)
			return true, r, err
		}
		r, err := l.Groceries.CheckGroceries(ctx, items)
		return true, r, err
	}
	return true, Result{}, errGroceriesElsewhere
}

// Executors returns one registration per tool, in a stable order.
func (l *Lists) Executors() []Executor {
	names := make([]string, 0, len(ListsTools))
	for name := range ListsTools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Executor, 0, len(names))
	for _, name := range names {
		out = append(out, &listTool{lists: l, tool: name})
	}
	return out
}

type listTool struct {
	lists *Lists
	tool  string
}

func (t *listTool) Name() string { return ListsCapability + "." + t.tool }

// listsUsage is each tool's params, in the words a model reads.
var listsUsage = map[string]string{
	ListToolShowAll: "The household's lists and how many items each has left. No params.",
	ListToolShow: "What is on one list. params: list (required, e.g. shopping). While PantryOS is installed, the " +
		"grocery list (groceries or shopping) is PantryOS's.",
	ListToolAdd: "Put items on a list; the list is created if it does not exist yet, and an item " +
		"that was ticked off is put back. params: list (required, e.g. shopping), items (required; " +
		"separate several with commas or new lines).",
	ListToolCheck: "Tick items off a list (bought, done, packed). params: list, items (both " +
		"required; separate several with commas or new lines).",
	ListToolRemove: "Take items off a list altogether. params: list, items (both required; " +
		"separate several with commas or new lines).",
	ListToolClearChecked: "Remove every ticked-off item from a list. params: list (required).",
	ListToolDelete:       "Delete a whole list and everything on it; the person is asked first. params: list (required).",
}

// Usage implements UsageDescriber.
func (t *listTool) Usage() string { return listsUsage[t.tool] }

func (t *listTool) Execute(ctx context.Context, c Call) (Result, error) {
	if ListsTools[t.tool] != TierRead {
		if err := checkExecution(ctx, t.lists.Authority); err != nil {
			return Result{}, err
		}
		if err := runtimedb.CheckPrivacyDelivery(ctx, runtimedb.MemoryDestination{}); err != nil {
			return Result{}, err
		}
	}
	if err := t.lists.ready(); err != nil {
		return Result{}, err
	}
	if routed, r, err := t.lists.routeGroceries(ctx, t.tool, c); routed {
		return r, err
	}
	switch t.tool {
	case ListToolShowAll:
		r, err := t.lists.showAll()
		if err == nil && t.lists.Groceries != nil {
			// Said on every overview, so a Butler reading "Shopping (3
			// left)" from an older list does not take it for the groceries.
			if on, ierr := t.lists.Groceries.Installed(); ierr == nil && on {
				r.Summary += " The grocery list is kept in PantryOS."
			}
		}
		return r, err
	case ListToolShow:
		return t.lists.show(c)
	case ListToolAdd:
		return t.lists.add(c)
	case ListToolCheck:
		return t.lists.check(c)
	case ListToolRemove:
		return t.lists.remove(c)
	case ListToolClearChecked:
		return t.lists.clearChecked(c)
	case ListToolDelete:
		return t.lists.deleteList(c)
	default:
		return Result{}, fmt.Errorf("lists has no tool %q", t.tool)
	}
}

// Describe implements Describer for the confirm-tier delete, so the
// person is asked about a list rather than about a tool call. It
// resolves the list first: nobody is asked to approve deleting something
// that is not there.
func (t *listTool) Describe(ctx context.Context, c Call) (string, error) {
	if t.tool != ListToolDelete {
		return "", nil
	}
	if err := t.lists.ready(); err != nil {
		return "", err
	}
	// Nobody is asked to approve deleting a grocery list that lives in
	// PantryOS: the call would be refused after the yes.
	if routed, _, err := t.lists.routeGroceries(ctx, t.tool, c); routed {
		return "", err
	}
	name, err := listParam(c)
	if err != nil {
		return "", err
	}
	l, err := t.lists.Store.HouseholdList(name)
	if err != nil {
		return "", listError(name, err)
	}
	return fmt.Sprintf("Delete the list %q and the %s on it?", l.Name, countItems(l.Open+l.Done)), nil
}

func (l *Lists) ready() error {
	if l == nil || l.Store == nil {
		return fmt.Errorf("household lists are not wired on this control plane")
	}
	if l.Enabled == nil {
		return nil
	}
	on, err := l.Enabled()
	if err != nil {
		return fmt.Errorf("checking whether the household has lists: %w", err)
	}
	if !on {
		return fmt.Errorf("this household has no lists installed")
	}
	return nil
}

// --- reads ---

func (l *Lists) showAll() (Result, error) {
	lists, err := l.Store.HouseholdLists()
	if err != nil {
		return Result{}, err
	}
	if len(lists) == 0 {
		return Result{Summary: "The household has no lists yet. Adding an item to one creates it."}, nil
	}
	parts := make([]string, 0, len(lists))
	for _, x := range lists {
		s := fmt.Sprintf("%s (%d left", x.Name, x.Open)
		if x.Done > 0 {
			s += fmt.Sprintf(", %d ticked off", x.Done)
		}
		parts = append(parts, s+")")
	}
	return Result{Summary: fmt.Sprintf("The household has %s: %s.",
		plural(len(lists), "list", "lists"), strings.Join(parts, ", "))}, nil
}

func (l *Lists) show(c Call) (Result, error) {
	name, err := listParam(c)
	if err != nil {
		return Result{}, err
	}
	x, err := l.Store.HouseholdList(name)
	if err != nil {
		return Result{}, listError(name, err)
	}
	if len(x.Items) == 0 {
		return Result{Summary: fmt.Sprintf("%s is empty.", x.Name)}, nil
	}
	var open, done []string
	for _, it := range x.Items {
		if it.Checked {
			done = append(done, it.Text)
		} else {
			open = append(open, it.Text)
		}
	}
	var b strings.Builder
	if len(open) == 0 {
		fmt.Fprintf(&b, "Everything on %s is ticked off", x.Name)
	} else {
		fmt.Fprintf(&b, "%s — still to do: %s", x.Name, spoken(open))
	}
	if len(done) > 0 && len(open) > 0 {
		fmt.Fprintf(&b, ". Ticked off: %s", spoken(done))
	} else if len(done) > 0 {
		fmt.Fprintf(&b, ": %s", spoken(done))
	}
	b.WriteString(".")
	return Result{Summary: b.String()}, nil
}

// --- writes ---

func (l *Lists) add(c Call) (Result, error) {
	name, items, err := listAndItems(c)
	if err != nil {
		return Result{}, err
	}
	change, err := l.Store.AddListItems(name, items, strings.TrimSpace(c.Subject))
	if err != nil {
		return Result{}, err
	}
	var b strings.Builder
	if len(change.Changed) > 0 {
		fmt.Fprintf(&b, "Added %s to %s", spoken(change.Changed), change.List)
		if change.Created {
			b.WriteString(" (a new list)")
		}
		b.WriteString(".")
	}
	if len(change.Unchanged) > 0 {
		sep(&b)
		fmt.Fprintf(&b, "%s %s already on %s.", capitalise(spoken(change.Unchanged)), isAre(len(change.Unchanged)), change.List)
	}
	return listResult(b.String(), change), nil
}

func (l *Lists) check(c Call) (Result, error) {
	name, items, err := listAndItems(c)
	if err != nil {
		return Result{}, err
	}
	change, err := l.Store.SetListItemsChecked(name, items, true, strings.TrimSpace(c.Subject))
	if err != nil {
		return Result{}, listError(name, err)
	}
	var b strings.Builder
	if len(change.Changed) > 0 {
		fmt.Fprintf(&b, "Ticked off %s on %s.", spoken(change.Changed), change.List)
	}
	if len(change.Unchanged) > 0 {
		sep(&b)
		fmt.Fprintf(&b, "%s %s already ticked off.", capitalise(spoken(change.Unchanged)), isAre(len(change.Unchanged)))
	}
	missing(&b, change)
	return listResult(b.String(), change), nil
}

func (l *Lists) remove(c Call) (Result, error) {
	name, items, err := listAndItems(c)
	if err != nil {
		return Result{}, err
	}
	change, err := l.Store.RemoveListItems(name, items)
	if err != nil {
		return Result{}, listError(name, err)
	}
	var b strings.Builder
	if len(change.Changed) > 0 {
		fmt.Fprintf(&b, "Took %s off %s.", spoken(change.Changed), change.List)
	}
	missing(&b, change)
	return listResult(b.String(), change), nil
}

func (l *Lists) clearChecked(c Call) (Result, error) {
	name, err := listParam(c)
	if err != nil {
		return Result{}, err
	}
	stored, n, err := l.Store.ClearCheckedListItems(name)
	if err != nil {
		return Result{}, listError(name, err)
	}
	summary := fmt.Sprintf("Nothing on %s was ticked off, so nothing was cleared.", stored)
	if n > 0 {
		summary = fmt.Sprintf("Cleared %s ticked off from %s.", countItems(n), stored)
	}
	// Either way the list now has nothing ticked on it, which is what was
	// asked.
	return Result{EffectConfirmed: true, Summary: summary,
		Detail: map[string]string{"list": stored, "cleared": fmt.Sprint(n)}}, nil
}

func (l *Lists) deleteList(c Call) (Result, error) {
	name, err := listParam(c)
	if err != nil {
		return Result{}, err
	}
	stored, n, err := l.Store.DeleteHouseholdList(name)
	if err != nil {
		return Result{}, listError(name, err)
	}
	return Result{EffectConfirmed: true,
		Summary: fmt.Sprintf("Deleted the list %s and the %s on it.", stored, countItems(n)),
		Detail:  map[string]string{"list": stored, "items": fmt.Sprint(n)}}, nil
}

// --- params and rendering ---

func listParam(c Call) (string, error) {
	name := strings.TrimSpace(c.Params["list"])
	if name == "" {
		return "", fmt.Errorf("param %q is required: which list (e.g. shopping)", "list")
	}
	return name, nil
}

// listAndItems reads the list and the items a write names. Items are one
// string, separated by commas or new lines — the shape a model reliably
// produces from "milk, eggs and bread" once "and" is split too.
func listAndItems(c Call) (string, []string, error) {
	name, err := listParam(c)
	if err != nil {
		return "", nil, err
	}
	items := splitItems(c.Params["items"])
	if len(items) == 0 {
		return "", nil, fmt.Errorf("param %q is required: which items (separate several with commas)", "items")
	}
	return name, items, nil
}

func splitItems(raw string) []string {
	raw = strings.NewReplacer("\r", "\n", ";", "\n", ",", "\n").Replace(raw)
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		// "eggs and bread" as the last of a comma list.
		line = strings.TrimSpace(strings.TrimPrefix(line, "and "))
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// listResult turns a write's report into its Result. The receipt vouches
// for the items now in the state asked for — changed by this call or
// already so — and there is none when not one item was.
func listResult(summary string, change runtimedb.ListChange) Result {
	confirmed := len(change.Changed)+len(change.Unchanged) > 0 || change.Created
	return Result{EffectConfirmed: confirmed, Summary: summary, Detail: map[string]string{
		"list":      change.List,
		"changed":   strings.Join(change.Changed, ", "),
		"unchanged": strings.Join(change.Unchanged, ", "),
		"missing":   strings.Join(change.Missing, ", "),
	}}
}

func listError(name string, err error) error {
	if errors.Is(err, runtimedb.ErrListNotFound) {
		return fmt.Errorf("there is no list called %q", name)
	}
	return err
}

func missing(b *strings.Builder, change runtimedb.ListChange) {
	if len(change.Missing) == 0 {
		return
	}
	sep(b)
	fmt.Fprintf(b, "%s %s not on %s.", capitalise(spoken(change.Missing)), isAre(len(change.Missing)), change.List)
}

func sep(b *strings.Builder) {
	if b.Len() > 0 {
		b.WriteString(" ")
	}
}

// spoken joins items the way a person says them: "milk, eggs and bread".
func spoken(items []string) string {
	extra := 0
	if len(items) > maxSpokenItems {
		extra = len(items) - maxSpokenItems
		items = items[:maxSpokenItems]
	}
	var s string
	switch len(items) {
	case 0:
		return ""
	case 1:
		s = items[0]
	default:
		s = strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
	if extra > 0 {
		s += fmt.Sprintf(" (and %d more)", extra)
	}
	return s
}

func capitalise(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func countItems(n int) string { return plural(n, "item", "items") }

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
