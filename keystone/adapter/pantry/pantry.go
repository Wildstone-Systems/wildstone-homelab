// Package pantry is the PantryOS adapter (ADR-W030 A5.1 / P12, ADR-W044).
//
// Two upstreams live here. HTTP (http.go) is the real one: PantryOS's
// contract pantry-1. Fake is the versioned stand-in P12 was built
// against, kept because the crossapp scenarios and the contract suite
// need a PantryOS with no network — and it now behaves like pantry-1, so
// a test that passes against the fake is asking the same question the
// real PantryOS answers.
//
// The fake is not a test double bolted on beside the adapter. It IS an
// upstream, behind the same interface, and the contract suite runs the
// same cases against both.
package pantry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
)

const (
	// CapList reads a named list.
	CapList = "pantry.list"
	// CapInventory reads what is in stock.
	CapInventory = "pantry.inventory"
	// CapAdd puts something on a list. It is an ACTION, so it is only
	// reachable once the connection's actions switch is on and a grant
	// covers it.
	CapAdd = "pantry.add"

	// ContractVersion is the contract the fake imitates, recorded as its
	// capability version.
	ContractVersion = Contract
)

// groceryList normalises a list name the way pantry-1 does. PantryOS
// keeps ONE list, the groceries; every other list is Wildstone's, and
// asking PantryOS for one is a routing mistake that must be refused, not
// answered as "nothing on it" (ADR-W044 A3).
func groceryList(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "groceries", "grocery", "shopping":
		return "groceries", nil
	case "":
		return "", fmt.Errorf("%w: needs a list name", ErrRejected)
	}
	return "", fmt.Errorf("%w: PantryOS keeps only the grocery list, not %q; other lists belong to Wildstone", ErrRejected, name)
}

// Fake is a PantryOS stand-in. It is safe for concurrent use because an
// adapter runtime may answer several claims at once.
type Fake struct {
	mu    sync.Mutex
	lists map[string][]string
	stock map[string]int
	// Unavailable makes every call fail, so a scenario can exercise the
	// source-unavailable path without a real outage.
	Unavailable bool
	// StaleAfter, when set, makes reads report as stale — what a cache
	// serving a last-known answer looks like from here.
	StaleAfter time.Time
	Now        func() time.Time
}

// NewFake builds a fake with a small household's worth of content.
func NewFake() *Fake {
	return &Fake{
		lists: map[string][]string{"groceries": {"milk", "bread"}},
		stock: map[string]int{"milk": 1, "eggs": 6},
	}
}

// NewEmptyFake is a working PantryOS with nothing in it: an empty grocery
// list and empty shelves — which is an answer, not an outage.
func NewEmptyFake() *Fake {
	return &Fake{lists: map[string][]string{"groceries": {}}, stock: map[string]int{}}
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Fake) Name() string { return "pantryos" }

func (f *Fake) Capabilities(context.Context) ([]adapter.Capability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	available := !f.Unavailable
	return []adapter.Capability{
		{Kind: "query", Name: CapList, Version: ContractVersion, Available: available},
		{Kind: "query", Name: CapInventory, Version: ContractVersion, Available: available},
		{Kind: "action", Name: CapAdd, Version: ContractVersion, Available: available},
	}, nil
}

func (f *Fake) Query(_ context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return adapter.Answer{}, fmt.Errorf("%w: pantryos is not answering", adapter.ErrUnavailable)
	}
	var req struct {
		List string `json:"list"`
	}
	if len(request) > 0 {
		_ = json.Unmarshal(request, &req)
	}
	stale := !f.StaleAfter.IsZero() && f.now().After(f.StaleAfter)

	switch capability {
	case CapList:
		list, err := groceryList(req.List)
		if err != nil {
			return adapter.Answer{}, err
		}
		items := f.lists[list]
		if len(items) == 0 {
			return adapter.Answer{Empty: true, Data: map[string]any{"list": list, "items": []string{}}}, nil
		}
		return adapter.Answer{
			Data:       map[string]any{"list": list, "items": append([]string{}, items...)},
			Stale:      stale,
			ObservedAt: f.StaleAfter,
		}, nil

	case CapInventory:
		names := make([]string, 0, len(f.stock))
		for name := range f.stock {
			names = append(names, name)
		}
		sort.Strings(names)
		items := make([]map[string]any, 0, len(names))
		for _, name := range names {
			items = append(items, map[string]any{"item": name, "count": f.stock[name]})
		}
		if len(items) == 0 {
			return adapter.Answer{Empty: true, Data: map[string]any{"inventory": items}}, nil
		}
		return adapter.Answer{
			Data:       map[string]any{"inventory": items},
			Stale:      stale,
			ObservedAt: f.StaleAfter,
		}, nil
	}
	return adapter.Answer{}, fmt.Errorf("%w: %s", adapter.ErrNoSuchCapability, capability)
}

func (f *Fake) Act(_ context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return adapter.Answer{}, fmt.Errorf("%w: pantryos is not answering", adapter.ErrUnavailable)
	}
	if capability != CapAdd {
		return adapter.Answer{}, fmt.Errorf("%w: %s", adapter.ErrNoSuchCapability, capability)
	}
	var req struct {
		List string `json:"list"`
		Item string `json:"item"`
	}
	if err := json.Unmarshal(request, &req); err != nil || req.Item == "" {
		return adapter.Answer{}, fmt.Errorf("%w: %s needs a list and an item", ErrRejected, CapAdd)
	}
	list, err := groceryList(req.List)
	if err != nil {
		return adapter.Answer{}, err
	}
	// Adding something already on the list is not an error and does not
	// add it twice: a routine that fires on a repeated event must not
	// produce a shopping list with milk on it four times. Case does not
	// make it a different item, as in PantryOS.
	for _, existing := range f.lists[list] {
		if strings.EqualFold(existing, req.Item) {
			return adapter.Answer{Data: map[string]any{
				"list": list, "item": existing, "added": false, "reason": "already on the list",
			}}, nil
		}
	}
	f.lists[list] = append(f.lists[list], req.Item)
	return adapter.Answer{Data: map[string]any{"list": list, "item": req.Item, "added": true}}, nil
}

// Items exposes a list for assertions.
func (f *Fake) Items(list string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name, err := groceryList(list); err == nil {
		list = name
	}
	return append([]string{}, f.lists[list]...)
}

var _ adapter.Upstream = (*Fake)(nil)
