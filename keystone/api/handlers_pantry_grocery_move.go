package api

// Moving the household's old grocery list into PantryOS (ADR-W044 A3.1,
// #387).
//
// While PantryOS is installed it owns the grocery list (A3), so a
// household that kept groceries in the Wildstone lists has an old list
// nobody's requests reach any more. This moves it across: every open
// item goes to PantryOS through pantry.add, which is repeat-safe, and
// each item leaves the old list only once PantryOS has it. When nothing
// open is left, the old list goes too, so the household sees one grocery
// list.
//
// OFFERED, NEVER AUTOMATIC. An Owner starts it from the pantry service's
// Connection panel. It needs a connected PantryOS, which is why it is not
// part of the install Plan: at install time there is nothing to move to.
//
// NOTHING IS LOST. A PantryOS that stops answering part-way leaves the
// items it did not take on the old list, and says how far it got;
// running the move again finishes it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// PantryGroceries is PantryOS's grocery list; *domestic.Pantry is the
// implementation.
type PantryGroceries interface {
	AddGroceries(ctx context.Context, items []string) (domestic.Result, error)
}

// groceryMoveList is one old grocery list and its open items.
type groceryMoveList struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

// oldGroceryLists are the household lists a move takes across: those
// whose name means the grocery list and that still have open items.
func (s *Server) oldGroceryLists() ([]groceryMoveList, error) {
	all, err := s.DB.HouseholdLists()
	if err != nil {
		return nil, err
	}
	var out []groceryMoveList
	for _, l := range all {
		if !domestic.IsGroceryList(l.Name) || l.Open == 0 {
			continue
		}
		full, err := s.DB.HouseholdList(l.Name)
		if errors.Is(err, runtimedb.ErrListNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		m := groceryMoveList{Name: full.Name}
		for _, it := range full.Items {
			if !it.Checked {
				m.Items = append(m.Items, it.Text)
			}
		}
		if len(m.Items) > 0 {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Server) groceryMovePreview(_ *http.Request, _ principal.Identity) (any, int, error) {
	lists, err := s.oldGroceryLists()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if lists == nil {
		lists = []groceryMoveList{}
	}
	return map[string]any{"lists": lists}, http.StatusOK, nil
}

// groceryMoveResult is what happened to one old list.
type groceryMoveResult struct {
	Name    string   `json:"name"`
	Moved   []string `json:"moved"`
	Removed bool     `json:"removed"`
	// Left is how many open items are still on the old list: ones added
	// while the move ran, or ones PantryOS did not take.
	Left int `json:"left"`
}

func (s *Server) moveGroceries(r *http.Request, id principal.Identity) (any, int, error) {
	if s.PantryGroceries == nil {
		return nil, http.StatusServiceUnavailable, errors.New("PantryOS is not wired on this control plane")
	}
	lists, err := s.oldGroceryLists()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	results := []groceryMoveResult{}
	total := 0
	for _, l := range lists {
		res := groceryMoveResult{Name: l.Name, Moved: []string{}}
		// One item at a time, so an item leaves the old list only once
		// PantryOS has said it has it.
		var failed error
		for _, item := range l.Items {
			if _, err := s.PantryGroceries.AddGroceries(r.Context(), []string{item}); err != nil {
				failed = err
				break
			}
			res.Moved = append(res.Moved, item)
		}
		if len(res.Moved) > 0 {
			removed, left, err := s.DB.RetireMovedListItems(l.Name, res.Moved)
			if err != nil && !errors.Is(err, runtimedb.ErrListNotFound) {
				return nil, http.StatusInternalServerError, fmt.Errorf("%s reached PantryOS but could not be taken off %s: %w",
					spokenCount(len(res.Moved), "item"), l.Name, err)
			}
			res.Removed, res.Left = removed || errors.Is(err, runtimedb.ErrListNotFound), left
		} else {
			res.Left = len(l.Items)
		}
		total += len(res.Moved)
		results = append(results, res)
		if failed != nil {
			s.groceryMoveEvent("pantry.groceries-move-failed", id, total)
			if total == 0 {
				return nil, http.StatusBadGateway, fmt.Errorf("nothing was moved: %v", failed)
			}
			return nil, http.StatusBadGateway, fmt.Errorf("moved %s before PantryOS stopped taking them (%v); the rest are still on %s — move again to finish",
				spokenCount(total, "item"), failed, l.Name)
		}
	}
	if total > 0 {
		s.groceryMoveEvent("pantry.groceries-moved", id, total)
	}
	return map[string]any{"moved": total, "lists": results}, http.StatusOK, nil
}

func (s *Server) groceryMoveEvent(typ string, id principal.Identity, n int) {
	if _, err := s.DB.AppendEvent(runtimedb.Event{
		Type: typ, Name: PantryCapability,
		Message: fmt.Sprintf("%s moved to PantryOS's grocery list", spokenCount(n, "item")),
		Attrs:   map[string]string{"by": id.Name},
	}); err != nil && s.Log != nil {
		s.Log.Error("grocery move event append failed", "type", typ, "err", err)
	}
}

func spokenCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
