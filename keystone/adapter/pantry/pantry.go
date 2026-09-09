// Package pantry is the PantryOS adapter, running against a VERSIONED
// FAKE until the real API exists (ADR-W030 A5.1, work package P12).
//
// PantryOS has no API yet. ADR-W030 Part C says to exercise it against a
// fake until it does, and the word carrying the weight is versioned: the
// fake declares a contract version, the adapter records it as the
// capability version, and when the real thing arrives its answers are
// checked against the same contract tests rather than against whatever
// this package happened to assume.
//
// The fake is not a test double bolted on beside the adapter. It IS the
// upstream, behind the same interface a real one will implement, so the
// work of swapping it is replacing one Upstream — and the contract tests
// that pass today are the ones the real API has to pass.
package pantry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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

	// ContractVersion is what this adapter was written against. It is
	// recorded as the capability version so that when the real PantryOS
	// API lands, "which contract is this" has an answer rather than an
	// assumption.
	ContractVersion = "fake-1"
)

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
		lists: map[string][]string{"groceries": {"milk", "bread"}, "hardware": {}},
		stock: map[string]int{"milk": 1, "eggs": 6},
	}
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
		if req.List == "" {
			return adapter.Answer{}, fmt.Errorf("%s needs a list name", CapList)
		}
		items, ok := f.lists[req.List]
		if !ok {
			// A list that does not exist and a list with nothing on it
			// are both empty answers about a working pantry.
			return adapter.Answer{Empty: true, Data: map[string]any{"list": req.List, "items": []string{}}}, nil
		}
		if len(items) == 0 {
			return adapter.Answer{Empty: true, Data: map[string]any{"list": req.List, "items": []string{}}}, nil
		}
		return adapter.Answer{
			Data:       map[string]any{"list": req.List, "items": append([]string{}, items...)},
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
	if err := json.Unmarshal(request, &req); err != nil || req.List == "" || req.Item == "" {
		return adapter.Answer{}, fmt.Errorf("%s needs a list and an item", CapAdd)
	}
	// Adding something already on the list is not an error and does not
	// add it twice: a routine that fires on a repeated event must not
	// produce a shopping list with milk on it four times.
	for _, existing := range f.lists[req.List] {
		if existing == req.Item {
			return adapter.Answer{Data: map[string]any{
				"list": req.List, "item": req.Item, "added": false, "reason": "already on the list",
			}}, nil
		}
	}
	f.lists[req.List] = append(f.lists[req.List], req.Item)
	return adapter.Answer{Data: map[string]any{"list": req.List, "item": req.Item, "added": true}}, nil
}

// Items exposes a list for assertions.
func (f *Fake) Items(list string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.lists[list]...)
}

var _ adapter.Upstream = (*Fake)(nil)
