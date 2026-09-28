package loop

// Projecting the calendar registry's grants into Radicale's rights file
// (ADR-W038; #372).
//
// The registry names people as Persons — a handle they carry or once
// carried, or a person_id — and groups by the household's own group ids.
// Radicale knows neither: it checks a username and the directory groups
// that username signed in with. The package template renders the file
// without a database, so it can only take a handle as the username it
// was when it was written. This pass renders it again with the Persons
// resolved, on a COPY of desired state, every tick — the same posture as
// projectEntitlements above — so renaming somebody or changing who is in
// `Adults` reaches Radicale without anybody editing the registry.
//
// The rights file is mounted into the Radicale container, so a change to
// it changes the container's config digest and Radicale restarts to read
// it. Radicale reads its rights only at start.

import (
	"errors"
	"fmt"
	"sort"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/sharedcalendar"
)

// calendarRightsName is the ConfigFile the calendar-radicale package
// renders its rights into.
const calendarRightsName = "radicale-rights"

// projectCalendarRights returns a copy of desired whose calendar rights
// file names the current usernames of the Persons and household groups
// the registry grants, and a finding for every grant that reaches nobody.
func projectCalendarRights(db *runtimedb.DB, desired []resource.Resource) ([]resource.Resource, []MembershipDrift, error) {
	if db == nil {
		return desired, nil, nil
	}
	registries := map[string]sharedcalendar.Registry{}
	for _, r := range desired {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil && r.Metadata.Name == sharedcalendar.ConfigName {
			reg, err := sharedcalendar.Parse(r.ConfigFile.Content)
			if err != nil {
				// The Plan that declared it already validated it; one
				// that no longer parses is left exactly as rendered.
				continue
			}
			registries[r.Metadata.Node] = reg
		}
	}
	if len(registries) == 0 {
		return desired, nil, nil
	}
	declaredGroups := map[string]bool{}
	for _, r := range desired {
		if r.Kind == resource.KindLdapGroup && r.LdapGroup != nil {
			declaredGroups[r.LdapGroup.Name] = true
		}
	}

	var drift []MembershipDrift
	var failure error
	unresolved := map[string]string{}
	grants := &sharedcalendar.Grants{
		Person: func(ref string) (string, bool) {
			id, err := db.ResolvePersonRef(ref)
			if err != nil {
				failure = err
				return "", true
			}
			if id == "" {
				unresolved[ref] = "names nobody on record, so it only reaches a directory account of that name"
				return "", false
			}
			handle, err := db.PersonHandle(id)
			if err != nil {
				failure = err
				return "", true
			}
			m, err := db.GetButlerMember(handle)
			if err != nil {
				failure = err
				return "", true
			}
			if !m.Active() {
				return "", true
			}
			return handle, true
		},
		Group: func(id string) ([]string, bool) {
			if !groupExists(db, id, &failure) {
				unresolved[id] = "is not a household group, so nobody is in it"
				return nil, false
			}
			people, err := db.ResolveGroupMembers(id)
			if err != nil {
				failure = err
				return nil, true
			}
			var names []string
			for _, p := range people {
				handle, err := db.PersonHandle(p)
				if err != nil {
					failure = err
					return nil, true
				}
				names = append(names, handle)
			}
			return names, true
		},
	}

	out := make([]resource.Resource, len(desired))
	copy(out, desired)
	for i, r := range out {
		if r.Kind != resource.KindConfigFile || r.ConfigFile == nil || r.Metadata.Name != calendarRightsName {
			continue
		}
		reg, ok := registries[r.Metadata.Node]
		if !ok {
			continue
		}
		service := sharedcalendar.ServiceFromRights(r.ConfigFile.Content)
		if service == "" {
			continue
		}
		rendered, err := reg.RightsWith(service, grants)
		if failure != nil {
			// A database that cannot answer must not become a rights file
			// that grants less, or more, than the household decided.
			return nil, nil, fmt.Errorf("resolving calendar grants: %w", failure)
		}
		if err != nil {
			return nil, nil, err
		}
		spec := *r.ConfigFile
		spec.Content = rendered
		out[i].ConfigFile = &spec
		for _, c := range reg.Calendars {
			for _, g := range append(append([]string(nil), c.ReadGroups...), c.WriteGroups...) {
				if !sharedcalendar.IsHouseholdGroup(g) && !declaredGroups[g] {
					unresolved[g] = "is not a directory group any installed package declares; a directory group made by hand still works"
				}
			}
		}
	}
	for ref, reason := range unresolved {
		drift = append(drift, MembershipDrift{Account: ref, Reason: "a shared calendar grant " + reason})
	}
	sort.Slice(drift, func(i, j int) bool { return drift[i].Account < drift[j].Account })
	return out, drift, nil
}

func groupExists(db *runtimedb.DB, id string, failure *error) bool {
	groups, err := db.ListGroups()
	if err != nil {
		*failure = errors.Join(*failure, err)
		return false
	}
	for _, g := range groups {
		if g.ID == id {
			return true
		}
	}
	return false
}
