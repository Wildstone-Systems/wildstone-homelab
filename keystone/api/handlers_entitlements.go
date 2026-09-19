package api

// Who may reach which service, as the Console manages it (ADR-W033
// design §4, §18.2; slice 8).
//
// NOT PLAN-MEDIATED, and that is deliberate. A plan is how Wildstone
// changes a NODE — containers, routes, files — and needs approval
// because applying it can break a deployment. An entitlement changes no
// node: it records what the household decided, and the reconcile loop
// projects it on the next tick. Routing it through a proposal would ask
// an Owner to approve their own decision, twice, and put a membership
// change behind the same gate as replacing a container image.
//
// Owner-only, by the same door as the rest of the Console (§3.2). The
// verb is Manage rather than Propose for the reason above: this writes
// the record directly, as admitting a Member does.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// serviceAccess is what the Console shows for one service.
type serviceAccess struct {
	Capability string `json:"capability"`
	// Declared is the audience the package states it is for, which is
	// where the entitlement rows came from. Shown so an Owner can see
	// what they are changing FROM rather than only what it is now.
	Declared []string `json:"declared,omitempty"`
	// Groups and People are the decisions on record.
	Groups []accessDecision `json:"groups"`
	People []accessDecision `json:"people"`
	// Reaches is who the rules resolve to right now, by handle. The
	// answer rather than the rules: an Owner reading "Adults may use
	// this" still has to work out who that is today, and getting it
	// wrong is the whole cost of a permission model.
	Reaches []string `json:"reaches"`
	// Dormant marks a service whose rows are inert because it is not
	// installed (§17.3). The decisions are kept and shown.
	Dormant bool `json:"dormant,omitempty"`
}

// accessDecision is one row, in the words a person reads.
type accessDecision struct {
	// ID is the group_id or person_id — what a write names.
	ID string `json:"id"`
	// Name is the household's word for it: a group's display name or a
	// person's handle. Never stored, always resolved, because a rename
	// must not re-point anything.
	Name   string `json:"name"`
	Effect string `json:"effect"`
}

// listServiceAccess answers who may reach one service.
func (s *Server) listServiceAccess(r *http.Request, _ principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant database is configured")
	}
	capability := r.PathValue("capability")
	if capability == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("capability required")
	}
	rows, err := s.DB.ListEntitlements(capability)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := serviceAccess{Capability: capability, Groups: []accessDecision{}, People: []accessDecision{}, Reaches: []string{}}
	if s.Catalog != nil {
		for _, pkg := range s.Catalog.List() {
			if pkg.Metadata.Capability == capability && len(pkg.AccessGroups) > 0 {
				out.Declared = pkg.AccessGroups
				break
			}
		}
	}

	groupNames, err := s.groupNames()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	handles, err := s.personHandles()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	for _, e := range rows {
		if e.Dormant() {
			out.Dormant = true
		}
		d := accessDecision{ID: e.Subject, Effect: e.Effect}
		switch e.SubjectKind {
		case runtimedb.EntitlementSubjectGroup:
			d.Name = groupNames[e.Subject]
			out.Groups = append(out.Groups, d)
		case runtimedb.EntitlementSubjectPerson:
			d.Name = handles[e.Subject]
			out.People = append(out.People, d)
		}
	}

	// Resolved through the one resolver, so what the Console shows and
	// what the door decides cannot disagree.
	for personID, handle := range handles {
		ok, err := s.DB.EntitledTo(personID, capability)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		if ok {
			out.Reaches = append(out.Reaches, handle)
		}
	}
	sort.Strings(out.Reaches)
	return out, http.StatusOK, nil
}

// setServiceAccessRequest is one decision, or its removal.
type setServiceAccessRequest struct {
	SubjectKind string `json:"subjectKind"`
	Subject     string `json:"subject"`
	// Effect is "allow", "restrict", or "" to remove the decision
	// entirely. The three are genuinely different: removing returns the
	// subject to whatever their groups say, which is not the same as
	// refusing them (TestRemovingADecisionIsNotRefusing).
	Effect string `json:"effect"`
}

// setServiceAccess records or removes one decision.
func (s *Server) setServiceAccess(r *http.Request, id principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant database is configured")
	}
	capability := r.PathValue("capability")
	if capability == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("capability required")
	}
	var req setServiceAccessRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxProposalBytes)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	if req.Subject == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("subject required")
	}
	if req.Effect == "" {
		if err := s.DB.RemoveEntitlement(capability, req.SubjectKind, req.Subject); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		return s.listServiceAccess(r, id)
	}
	if err := s.DB.SetEntitlement(capability, req.SubjectKind, req.Subject, req.Effect, actingMember(id)); err != nil {
		// A bad subject kind or effect is the caller's input, not a
		// server fault.
		return nil, http.StatusBadRequest, err
	}
	return s.listServiceAccess(r, id)
}

// personGroup is one of the household's groups, with who is in it.
type personGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// SeedKey is set on the three groups the design names and empty on
	// every group a household made itself.
	SeedKey string `json:"seedKey,omitempty"`
	// Members are handles, resolved. A rename of a Person changes this
	// and re-points nothing, which is the point of keying on ids.
	Members []string `json:"members"`
}

// listPersonGroups answers who is in the household's groups.
//
// Distinct from GET /household/groups, which lists the DIRECTORY groups
// packages declare. These are Wildstone's own — the one principal
// namespace slice 6 built — and the two are different things that a
// Console must not blur: a directory group is where a membership is
// projected TO, and this is what the household decided.
func (s *Server) listPersonGroups(_ *http.Request, _ principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant database is configured")
	}
	groups, err := s.DB.ListGroups()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	handles, err := s.personHandles()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := make([]personGroup, 0, len(groups))
	for _, g := range groups {
		people, err := s.DB.ResolveGroupMembers(g.ID)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		members := make([]string, 0, len(people))
		for _, p := range people {
			if h := handles[p]; h != "" {
				members = append(members, h)
			}
		}
		sort.Strings(members)
		out = append(out, personGroup{ID: g.ID, Name: g.Name, SeedKey: g.SeedKey, Members: members})
	}
	return out, http.StatusOK, nil
}

// setPersonGroupRequest names a Person by handle, as the Console has
// them.
type setPersonGroupRequest struct {
	Member string `json:"member"`
	// In is false to take them out. Not a separate endpoint, because
	// the two are one decision an Owner toggles.
	In bool `json:"in"`
}

// setPersonGroup puts a Person in a group, or takes them out.
//
// Grants nothing by itself (§4). What the group reaches is whatever the
// entitlement rows say about it, and this handler has no opinion on
// that — which is worth saying at the one call site a reader might
// mistake for a permission change.
func (s *Server) setPersonGroup(r *http.Request, id principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant database is configured")
	}
	groupID := r.PathValue("group")
	var req setPersonGroupRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxProposalBytes)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	if req.Member == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("member required")
	}
	// Resolved through the same refusal projection uses: a handle that
	// answers to two active Persons is not an identity, and acting on a
	// guess is how somebody is put in a group that was meant for
	// another person with the same name.
	personID, ambiguous, err := s.DB.PersonForDirectoryAccount(req.Member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if ambiguous {
		return nil, http.StatusConflict, fmt.Errorf(
			"more than one active person answers to %q, so this cannot name one of them", req.Member)
	}
	if personID == "" {
		return nil, http.StatusNotFound, fmt.Errorf("no active person called %q", req.Member)
	}
	if req.In {
		err = s.DB.AddPersonToGroup(groupID, personID, actingMember(id))
	} else {
		err = s.DB.RemovePersonFromGroup(groupID, personID)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return s.listPersonGroups(r, id)
}

// groupNames maps group_id to the household's word for it.
func (s *Server) groupNames() (map[string]string, error) {
	groups, err := s.DB.ListGroups()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(groups))
	for _, g := range groups {
		out[g.ID] = g.Name
	}
	return out, nil
}

// personHandles maps person_id to the handle, for ACTIVE people only.
//
// Active only, because every caller here is showing or deciding access,
// and an offboarded person holds none — listing them would suggest they
// still reach something the resolver already refuses them.
func (s *Server) personHandles() (map[string]string, error) {
	members, err := s.DB.ListButlerMembers()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(members))
	for _, m := range members {
		if m.Active() {
			out[m.PersonID] = m.Member
		}
	}
	return out, nil
}
