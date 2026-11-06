package api

import (
	"errors"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerlive"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// inventoryPerson is the canonical Person carried by an authenticated
// Person-facing request. It deliberately does not fall back to the principal
// name: a machine principal and the Person it acts for are different
// identities, even when somebody happened to give them equal labels.
//
// An empty result is the legacy administrative/API-principal view. Keeping it
// separate preserves operator audit and recovery clients while every Console
// and linked API-principal request (both of which carry ActingFor) receives a
// Person-scoped view.
func inventoryPerson(id principal.Identity) string {
	return strings.TrimSpace(id.ActingFor)
}

// inventoryMember resolves the request's current handle to its stable Person.
// A missing or inactive Person is still a Person-facing request, but receives
// an empty view so no hidden Agent or history state is distinguishable.
func (s *Server) inventoryMember(id principal.Identity) (runtimedb.ButlerMember, bool, error) {
	person := inventoryPerson(id)
	if person == "" {
		return runtimedb.ButlerMember{}, false, nil
	}
	member, err := s.resolveRequestSurface(id)
	if errors.Is(err, runtimedb.ErrNotFound) || (err == nil && !member.Active()) {
		return runtimedb.ButlerMember{}, true, nil
	}
	return member, true, err
}

// visibleButlers returns the live Agents the current Person is admitted to.
// The boolean says whether this is a Person-facing view. An inactive Person
// receives an empty set rather than an error that could distinguish another
// Person's Agent from an absent one.
func (s *Server) visibleButlers(id principal.Identity) ([]runtimedb.Butler, bool, error) {
	member, scoped, err := s.inventoryMember(id)
	if err != nil {
		return nil, scoped, err
	}
	if !scoped {
		all, err := s.DB.ListButlers()
		return all, false, err
	}
	if member.PersonID == "" {
		return []runtimedb.Butler{}, true, nil
	}
	all, err := s.DB.ButlerInventoryFor(member.Member)
	return all, true, err
}

// visibleButler resolves a named Agent through the same set as the roster.
// Returning ErrNotFound for a hidden Agent makes hidden and absent names
// indistinguishable on every drawer endpoint.
func (s *Server) visibleButler(name string, id principal.Identity) (runtimedb.Butler, bool, error) {
	all, scoped, err := s.visibleButlers(id)
	if err != nil {
		return runtimedb.Butler{}, scoped, err
	}
	for _, b := range all {
		if b.Name == name {
			return b, scoped, nil
		}
	}
	return runtimedb.Butler{}, scoped, runtimedb.ErrNotFound
}

func (s *Server) visibleButlerNames(id principal.Identity) (map[string]bool, bool, error) {
	all, scoped, err := s.visibleButlers(id)
	if err != nil {
		return nil, scoped, err
	}
	out := make(map[string]bool, len(all))
	for _, b := range all {
		out[b.Name] = true
	}
	return out, scoped, nil
}

// butlerManifestFor answers with the same identity tuple a live Console turn
// uses: Person + Agent + Console Connection. The administrative branch is
// retained for machine principals that do not claim to be a Person.
func (s *Server) butlerManifestFor(b runtimedb.Butler, id principal.Identity) butler.CapabilityManifest {
	readOnly := !b.CarriesActionAuthority()
	person := inventoryPerson(id)
	authority := &butlerstore.Authority{DB: s.DB, Butler: b.Name}

	if person == "" {
		return s.administrativeButlerManifest(b, authority, readOnly)
	}
	authority.PersonAuthority = authority.PersonIsAdmitted()
	member := butler.NewHouseholdGateway(person, s.ButlerHost.inventorySeams())
	member.SetAuthorizer(authority)
	member.SetCaller(butler.CallerPersonFacing, person, butlerlive.ConsoleConnection)

	switch b.AgentClass {
	case runtimedb.AgentClassMember:
		return member.Capabilities(readOnly)
	case runtimedb.AgentClassOwner:
		_, live, _, _, _ := s.ButlerHost.Current()
		if live == nil {
			live = butler.NewToolGateway(nil, nil)
		}
		owner := live.Scoped(authority)
		owner.SetCaller(butler.CallerPersonFacing, person, butlerlive.ConsoleConnection)
		return butler.NewOwnerToolset(owner, member).Capabilities(readOnly)
	default:
		return butler.CapabilityManifest{}
	}
}

func (s *Server) administrativeButlerManifest(b runtimedb.Butler, authority *butlerstore.Authority, readOnly bool) butler.CapabilityManifest {
	switch b.AgentClass {
	case runtimedb.AgentClassMember:
		gw := butler.NewHouseholdGateway("", s.ButlerHost.inventorySeams()).ForAdministration()
		gw.SetAuthorizer(authority)
		return gw.Capabilities(readOnly)
	case runtimedb.AgentClassOwner:
		_, live, _, _, _ := s.ButlerHost.Current()
		if live == nil {
			live = butler.NewToolGateway(nil, nil)
		}
		member := butler.NewHouseholdGateway("", s.ButlerHost.inventorySeams()).ForAdministration()
		member.SetAuthorizer(authority)
		return butler.NewOwnerToolset(live.Scoped(authority).ForAdministration(), member).Capabilities(readOnly)
	default:
		return butler.CapabilityManifest{}
	}
}

func (s *Server) taskBelongsToInventoryPerson(task runtimedb.ButlerTask, id principal.Identity) (bool, error) {
	member, scoped, err := s.inventoryMember(id)
	if err != nil {
		return false, err
	}
	if !scoped {
		return canSeeTask(task, id), nil
	}
	return s.DB.ButlerTaskBelongsToPerson(task.ID, member.PersonID)
}

func (s *Server) taskUsesVisibleButler(task runtimedb.ButlerTask, id principal.Identity) bool {
	if inventoryPerson(id) == "" || task.SessionID == "" {
		return true
	}
	session, err := s.DB.GetButlerSession(task.SessionID)
	if err != nil {
		return false
	}
	// Sessions written before the Agent stamp existed still belong to their
	// recorded Person. There is no Agent name to disclose or reauthorize, so
	// keep that person's historical task visible rather than inventing one.
	if session.Butler == "" {
		return true
	}
	_, _, err = s.visibleButler(session.Butler, id)
	return err == nil
}
