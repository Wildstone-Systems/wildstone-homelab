package api

// The Home dashboard's Lists card (packages/lists-wildstone).
//
// The same household lists the Agents' lists.* tools read and change
// (keystone/domestic/lists.go), through the same store, so a tick on the
// card and "I got the milk" in chat are the same write. The card's door is
// the `lists` capability's own group, lists_users, checked on every
// request like every other household surface's.
//
// A request names a list and items, never a person: who added or ticked
// something is the signed-in member, from the forwarded identity, exactly
// as the Agent tools take it from the session rather than a parameter.
//
// Deleting a whole list is here for the Lists page, and the page asks
// first — naming the list and how much is on it — before it sends the
// request. That is the confirm tier's point made by a person rather than
// a model: an Agent's lists.delete_list asks the person, and here the
// person is the one clicking. The card on the dashboard does not offer
// it at all.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

var surfaceLists = portalSurface{capability: "lists", title: "Lists"}

type listsCard struct {
	Lists []runtimedb.HouseholdList `json:"lists"`
	// GroceriesInPantryOS is set while PantryOS is installed: it owns the
	// grocery list then (ADR-W044 A3), so the page shows a pointer to it
	// and treats a grocery-named list here as the old one, to clear out.
	GroceriesInPantryOS bool `json:"groceriesInPantryOS,omitempty"`
}

// errGroceriesInPantryOS is the answer to adding to, or ticking, a grocery
// list here while PantryOS owns it (#405). Otherwise the Lists page and the
// assistant would each keep half a grocery list.
var errGroceriesInPantryOS = errors.New("the grocery list lives in PantryOS while it is installed: add and tick " +
	"groceries there, or ask the assistant. An old grocery list here can still be cleared or deleted")

// groceriesInPantryOS reports whether PantryOS is installed and so owns the
// grocery list.
func (s *Server) groceriesInPantryOS() (bool, error) {
	return s.CapabilityEnabled(PantryCapability)
}

// refuseGroceryWrite answers a write that would grow or tick a grocery list
// here while PantryOS owns it.
func (s *Server) refuseGroceryWrite(list string) (int, error) {
	if !domestic.IsGroceryList(list) {
		return 0, nil
	}
	owned, err := s.groceriesInPantryOS()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if owned {
		return http.StatusConflict, errGroceriesInPantryOS
	}
	return 0, nil
}

// listsRead answers every list with its items, most recently changed
// first. Household lists are short (runtimedb.MaxHouseholdLists and
// MaxHouseholdListItems bound them), so the card gets everything in one
// read.
func (s *Server) listsRead(_ http.ResponseWriter, _ *http.Request, _ string, _ []resource.Resource) (any, int, error) {
	summaries, err := s.DB.HouseholdLists()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	owned, err := s.groceriesInPantryOS()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := listsCard{Lists: make([]runtimedb.HouseholdList, 0, len(summaries)), GroceriesInPantryOS: owned}
	for _, l := range summaries {
		full, err := s.DB.HouseholdList(l.Name)
		if errors.Is(err, runtimedb.ErrListNotFound) {
			continue // deleted between the two reads
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		out.Lists = append(out.Lists, full)
	}
	return out, http.StatusOK, nil
}

type listItemsRequest struct {
	List    string   `json:"list"`
	Items   []string `json:"items"`
	Checked *bool    `json:"checked,omitempty"`
}

func decodeListRequest(r *http.Request) (listItemsRequest, int, error) {
	var req listItemsRequest
	if code, err := decodeStrict(r, &req); err != nil {
		return req, code, err
	}
	if strings.TrimSpace(req.List) == "" {
		return req, http.StatusBadRequest, fmt.Errorf("name a list")
	}
	return req, 0, nil
}

// listWriteStatus maps a store error onto what the card is told.
func listWriteStatus(err error) (int, error) {
	if errors.Is(err, runtimedb.ErrListNotFound) {
		return http.StatusNotFound, fmt.Errorf("that list is not here any more")
	}
	if errors.Is(err, runtimedb.ErrQuarantined) {
		return http.StatusServiceUnavailable, err
	}
	// Everything else the store refuses is the request's own fault: an
	// empty or over-long item, a full list, too many lists.
	return http.StatusBadRequest, errors.New(strings.TrimPrefix(err.Error(), "runtimedb: "))
}

func (s *Server) listsAdd(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	req, code, err := decodeListRequest(r)
	if err != nil {
		return nil, code, err
	}
	if code, err := s.refuseGroceryWrite(req.List); err != nil {
		return nil, code, err
	}
	change, err := s.DB.AddListItems(req.List, req.Items, member)
	if err != nil {
		code, err := listWriteStatus(err)
		return nil, code, err
	}
	return change, http.StatusOK, nil
}

func (s *Server) listsCheck(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	req, code, err := decodeListRequest(r)
	if err != nil {
		return nil, code, err
	}
	if code, err := s.refuseGroceryWrite(req.List); err != nil {
		return nil, code, err
	}
	checked := true
	if req.Checked != nil {
		checked = *req.Checked
	}
	change, err := s.DB.SetListItemsChecked(req.List, req.Items, checked, member)
	if err != nil {
		code, err := listWriteStatus(err)
		return nil, code, err
	}
	return change, http.StatusOK, nil
}

func (s *Server) listsRemove(_ http.ResponseWriter, r *http.Request, _ string, _ []resource.Resource) (any, int, error) {
	req, code, err := decodeListRequest(r)
	if err != nil {
		return nil, code, err
	}
	change, err := s.DB.RemoveListItems(req.List, req.Items)
	if err != nil {
		code, err := listWriteStatus(err)
		return nil, code, err
	}
	return change, http.StatusOK, nil
}

func (s *Server) listsClearChecked(_ http.ResponseWriter, r *http.Request, _ string, _ []resource.Resource) (any, int, error) {
	req, code, err := decodeListRequest(r)
	if err != nil {
		return nil, code, err
	}
	name, n, err := s.DB.ClearCheckedListItems(req.List)
	if err != nil {
		code, err := listWriteStatus(err)
		return nil, code, err
	}
	return map[string]any{"list": name, "cleared": n}, http.StatusOK, nil
}

func (s *Server) listsDelete(_ http.ResponseWriter, r *http.Request, _ string, _ []resource.Resource) (any, int, error) {
	req, code, err := decodeListRequest(r)
	if err != nil {
		return nil, code, err
	}
	name, n, err := s.DB.DeleteHouseholdList(req.List)
	if err != nil {
		code, err := listWriteStatus(err)
		return nil, code, err
	}
	return map[string]any{"list": name, "items": n}, http.StatusOK, nil
}
