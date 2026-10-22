package sharedcalendar

import (
	"strings"
	"testing"
)

func TestRegistryRefusesAddressesAndAmbiguousDefaults(t *testing.T) {
	good := Legacy().JSON()
	for _, raw := range []string{strings.Replace(good, `"household"`, `"../escape"`, 1), strings.Replace(good, `"default":true`, `"default":false`, 1), strings.Replace(good, `"#3b82f6"`, `"red;url(evil)"`, 1), strings.Replace(good, `"name":"Household"`, `"name":"Household","url":"https://evil"`, 1), good + good} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestCalendarAccessAndOriginConfinement(t *testing.T) {
	c := Calendar{ID: "school", ReadGroups: []string{"calendar_users"}, WriteGroups: []string{"adults"}}
	sam := func(groups ...string) Subject { return Subject{Member: "sam", Groups: groups} }
	if !c.Allows(sam("calendar_users"), false, nil) || c.Allows(sam("calendar_users"), true, nil) || !c.Allows(sam("adults"), true, nil) || c.Allows(Subject{Groups: []string{"adults"}}, true, nil) {
		t.Fatal("access rules")
	}
	base := "https://calendar.test/GROUPS/abc=/"
	for _, raw := range []string{"https://evil.test/x.ics", "//evil.test/x.ics", "/sam/personal/x.ics", "../other/x.ics", "/GROUPS/abc=/sub/x.ics", "/GROUPS/abc=/x.ics?redirect=evil", "/GROUPS/abc=/a/../x.ics", "/GROUPS/abc=/a%2fb.ics"} {
		if _, e := EventURL(base, raw); e == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if got, e := EventURL(base, "/GROUPS/abc%3D/x.ics"); e != nil || got != base+"x.ics" {
		t.Fatal(got, e)
	}
	u, e := c.CollectionURL(base)
	if e != nil || !strings.HasPrefix(u, "https://calendar.test/GROUPS/") {
		t.Fatal(u, e)
	}
}

// A member grant follows the Person it names, not the spelling (#372).
func TestMemberGrantsFollowThePerson(t *testing.T) {
	c := Calendar{ID: "private", ReadMembers: []string{"sam", "psn_00000000000000aa"}, WriteMembers: []string{"ghost"}}
	people := map[string]string{"sam": "psn_0000000000000001", "samuel": "psn_0000000000000001", "psn_00000000000000aa": "psn_00000000000000aa"}
	resolve := func(ref string) string { return people[ref] }
	renamed := Subject{Member: "samuel", Person: "psn_0000000000000001"}
	if !c.Allows(renamed, false, resolve) {
		t.Fatal("a rename detached a calendar grant")
	}
	stranger := Subject{Member: "sam"}
	if c.Allows(stranger, false, resolve) {
		t.Fatal("a directory account sharing a Person's old handle inherited their grant")
	}
	byID := Subject{Member: "alex", Person: "psn_00000000000000aa"}
	if !c.Allows(byID, false, resolve) {
		t.Fatal("a grant by person_id did not match its Person")
	}
	if (Calendar{ID: "x", ReadMembers: []string{"psn_00000000000000aa"}}).Allows(Subject{Member: "psn_00000000000000aa"}, false, nil) {
		t.Fatal("a person_id matched a handle by spelling")
	}
	// A handle naming nobody on record is a directory account Wildstone
	// does not manage, and keeps its grant.
	if !c.Allows(Subject{Member: "ghost"}, true, resolve) {
		t.Fatal("an unmanaged directory account lost its grant")
	}
	household := Calendar{ID: "school", ReadGroups: []string{"grp_00000000000000bb"}}
	if !household.Allows(Subject{Member: "alex", Groups: []string{"grp_00000000000000bb"}}, false, resolve) {
		t.Fatal("a household group grant did not match its member")
	}
}

func TestRegistryAcceptsPersonAndHouseholdGroupReferences(t *testing.T) {
	raw := `{"calendars":[{"id":"household","name":"Household","color":"#3b82f6","default":true,"readGroups":["calendar_users","grp_00000000000000bb"],"writeGroups":["calendar_users"],"readMembers":["Admin","psn_00000000000000aa"]}]}`
	if _, err := Parse(raw); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"readMembers":["a/b"]`, `"readMembers":["-x"]`, `"readGroups":["Adults"]`} {
		r := strings.Replace(raw, `"readMembers":["Admin","psn_00000000000000aa"]`, bad, 1)
		if bad == `"readGroups":["Adults"]` {
			r = strings.Replace(raw, `"readGroups":["calendar_users","grp_00000000000000bb"]`, bad, 1)
		}
		if _, err := Parse(r); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// The rights Radicale reads name usernames and directory groups, and keep
// the service account out of undeclared shared collections.
func TestRightsResolvePeopleAndConfineTheServiceAccount(t *testing.T) {
	reg := Legacy()
	reg.Calendars = append(reg.Calendars, Calendar{ID: "school", Name: "School", Color: "#f59e0b",
		ReadGroups: []string{"calendar_users", "grp_00000000000000bb", "grp_00000000000000cc"}, WriteMembers: []string{"sam", "psn_00000000000000aa", "left", "visitor"}})
	g := &Grants{
		Person: func(ref string) (string, bool) {
			return map[string]string{"sam": "Samuel", "psn_00000000000000aa": "alex", "left": ""}[ref], ref == "sam" || ref == "psn_00000000000000aa" || ref == "left"
		},
		Group: func(id string) ([]string, bool) {
			if id == "grp_00000000000000bb" {
				return []string{"Kai"}, true
			}
			return nil, false
		},
	}
	rights, err := reg.RightsWith("radicale", g)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[school-rw]\nuser: samuel|alex|visitor\n",
		"[school-r]\nuser: kai\ngroups: calendar_users,grp_00000000000000cc\n",
		"collection: (?!GROUPS/)[^/]+/[^/]+\npermissions: rw",
	} {
		if !strings.Contains(rights, want) {
			t.Errorf("rights lack %q:\n%s", want, rights)
		}
	}
	if ServiceFromRights(rights) != "radicale" {
		t.Fatal("service marker")
	}
	plain, _ := reg.Rights("radicale")
	if strings.Contains(plain, "psn_") || !strings.Contains(plain, "user: sam|left|visitor\n") {
		t.Errorf("template rights:\n%s", plain)
	}
}
