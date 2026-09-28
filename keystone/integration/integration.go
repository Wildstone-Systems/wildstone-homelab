// Package integration is the closed vocabulary of what household apps
// answer (queries), do (actions) and report (events), by domain
// (docs/catalog-integration-contract.md §3).
//
// It is a leaf package, importing nothing, closed by review — the same
// posture as keystone/trait and policy.Classifications. A query name is
// matched against an adapter written by somebody else, for an app
// Wildstone may not run; a free string cannot tell `pantry.expiring` from
// `pantry.expirng`, and Butler's tools, routines and tests would each be
// right for one deployment only. Adding a query, action or event is a
// change to this one file.
package integration

import "strings"

// Param is one parameter of a query or action. A domain lists the FULL
// set; an implementation may honour a subset.
type Param struct {
	Name        string
	Description string
	Required    bool
}

// Query is a read.
type Query struct {
	Name        string
	Description string
	Params      []Param
	Returns     string
}

// Proposed action classes. Metadata, not authority (ADR-W030 A8): platform
// policy validates the class and an administrator may make it stricter,
// never silently weaker.
const (
	LowImpact     = "low_impact"
	Consequential = "consequential"
)

// Action is a write.
type Action struct {
	Name          string
	Description   string
	Params        []Param
	ProposedClass string
}

// Domain is one app family's vocabulary.
type Domain struct {
	Name        string
	Description string
	Queries     []Query
	Actions     []Action
	// Events are the well-known event types a package in this domain
	// publishes and Butler and shipped routines rely on.
	Events []string
}

// Pantry is PantryOS's domain (ADR-W042; PantryOS contract `pantry-1`).
var Pantry = Domain{
	Name:        "pantry",
	Description: "The household's kitchen: stock, expiry, recipes, meals, the grocery list and the cook-along.",
	Queries: []Query{
		{Name: "pantry.list", Description: "A list's items; only the grocery list.",
			Params:  []Param{{Name: "list", Description: "groceries (grocery and shopping are the same list)", Required: true}},
			Returns: "{list, items[], entries[]}"},
		{Name: "pantry.inventory", Description: "What is on hand, one row per item, across locations.",
			Params:  []Param{{Name: "search", Description: "case-insensitive part of an item name"}},
			Returns: "{inventory: [{item, count, unit, locations}]}"},
		{Name: "pantry.expiring", Description: "Food reaching its date soon.",
			Params:  []Param{{Name: "within_days", Description: "0–90, default 7"}},
			Returns: "{within_days, items: [{item, location, expires_on, days_left, quantity, unit}]}"},
		{Name: "pantry.summary", Description: "The household's pantry at a glance.", Returns: "the household summary"},
		{Name: "pantry.recipes", Description: "Recipes cookable from what is in stock.", Returns: "{recipes[]}"},
		{Name: "pantry.recipe", Description: "One recipe with its steps.",
			Params:  []Param{{Name: "recipe_id", Required: true}},
			Returns: "{recipe, servings, steps[], ingredients[]}"},
		{Name: "pantry.cook.current", Description: "The cook-along session in progress, if any.", Returns: "{session}"},
		{Name: "pantry.timers", Description: "Kitchen timers running or ringing.", Returns: "{timers[]}"},
	},
	Actions: []Action{
		{Name: "pantry.add", Description: "Put an item on the grocery list (repeat-safe).", ProposedClass: LowImpact,
			Params: []Param{{Name: "list", Required: true}, {Name: "item", Required: true}}},
		{Name: "pantry.check", Description: "Tick an item off the grocery list.", ProposedClass: LowImpact,
			Params: []Param{{Name: "list", Required: true}, {Name: "item", Required: true}}},
		{Name: "pantry.cook.start", Description: "Start the cook-along for a recipe; changes no stock.", ProposedClass: LowImpact,
			Params: []Param{{Name: "recipe_id", Required: true}, {Name: "servings"}}},
		{Name: "pantry.cook.step", Description: "Move through the steps, or tick one off; changes no stock.", ProposedClass: LowImpact,
			Params: []Param{{Name: "action", Description: "next | previous | repeat | goto | complete", Required: true}, {Name: "step"}}},
		{Name: "pantry.cook.finish", Description: "Finish the cook: takes its ingredients out of stock.", ProposedClass: Consequential,
			Params: []Param{{Name: "location_id"}, {Name: "session_id", Description: "the cook the caller confirmed"}}},
		{Name: "pantry.cook.abandon", Description: "Stop the cook-along; changes no stock.", ProposedClass: LowImpact},
		{Name: "pantry.timer.set", Description: "Start a kitchen timer.", ProposedClass: LowImpact,
			Params: []Param{{Name: "seconds", Required: true}, {Name: "label"}, {Name: "step_number"}, {Name: "member"}}},
		{Name: "pantry.timer.cancel", Description: "Stop a kitchen timer.", ProposedClass: LowImpact,
			Params: []Param{{Name: "timer_id"}, {Name: "label"}}},
		{Name: "pantry.meal.log", Description: "Log a meal by food name; an uncertain food comes back as candidates.", ProposedClass: LowImpact,
			Params: []Param{{Name: "member"}, {Name: "profile_id"}, {Name: "food", Required: true}, {Name: "quantity"},
				{Name: "unit"}, {Name: "meal_type"}, {Name: "item_id"}, {Name: "fdc_id"}, {Name: "location_id"}}},
		{Name: "pantry.meal.undo", Description: "Take back a logged meal.", ProposedClass: LowImpact,
			Params: []Param{{Name: "event_id", Required: true}}},
	},
	Events: []string{
		"pantry.meal_logged",
		"pantry.purchase_recorded",
		"pantry.recipe_cooked",
		"pantry.stock_low",
		"pantry.item_expiring",
		"pantry.item_spoiled",
		"pantry.shopping_list_changed",
		"pantry.timer_finished",
	},
}

// Domains is every declared domain.
var Domains = []Domain{Pantry}

// DomainOf returns the domain a dotted name ("pantry.list",
// "pantry.meal_logged") belongs to, if one is declared.
func DomainOf(name string) (Domain, bool) {
	prefix, _, ok := strings.Cut(name, ".")
	if !ok {
		return Domain{}, false
	}
	for _, d := range Domains {
		if d.Name == prefix {
			return d, true
		}
	}
	return Domain{}, false
}

// IsQuery reports whether name is a declared query.
func IsQuery(name string) bool {
	d, ok := DomainOf(name)
	if !ok {
		return false
	}
	for _, q := range d.Queries {
		if q.Name == name {
			return true
		}
	}
	return false
}

// IsAction reports whether name is a declared action.
func IsAction(name string) bool {
	d, ok := DomainOf(name)
	if !ok {
		return false
	}
	for _, a := range d.Actions {
		if a.Name == name {
			return true
		}
	}
	return false
}

// IsEvent reports whether name is a well-known event of its domain.
func IsEvent(name string) bool {
	d, ok := DomainOf(name)
	if !ok {
		return false
	}
	for _, e := range d.Events {
		if e == name {
			return true
		}
	}
	return false
}
