package loop

// Projecting calendar grants into Radicale's rights (ADR-W038, #372).

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/sharedcalendar"
)

func calendarDesired(t *testing.T, reg sharedcalendar.Registry) []resource.Resource {
	t.Helper()
	rights, err := reg.Rights("radicale")
	if err != nil {
		t.Fatal(err)
	}
	return []resource.Resource{
		{Kind: resource.KindConfigFile, Metadata: resource.Metadata{Name: sharedcalendar.ConfigName, Node: "n1", Owner: "capability/calendar"},
			ConfigFile: &resource.ConfigFileSpec{Filename: "calendars.json", Content: reg.JSON()}},
		{Kind: resource.KindConfigFile, Metadata: resource.Metadata{Name: calendarRightsName, Node: "n1", Owner: "capability/calendar"},
			ConfigFile: &resource.ConfigFileSpec{Filename: "rights", Content: rights}},
		{Kind: resource.KindLdapGroup, Metadata: resource.Metadata{Name: "calendar-users", Node: "n1", Owner: "capability/calendar"},
			LdapGroup: &resource.LdapGroupSpec{Name: "calendar_users"}},
	}
}

func projectedRights(t *testing.T, out []resource.Resource) string {
	t.Helper()
	for _, r := range out {
		if r.Metadata.Name == calendarRightsName {
			return r.ConfigFile.Content
		}
	}
	t.Fatal("no rights file")
	return ""
}

func TestCalendarGrantsReachRadicaleByPerson(t *testing.T) {
	_, _, db, _ := harness(t)
	for _, h := range []string{"sam", "kai", "alex"} {
		if err := db.RecordButlerMember(h, runtimedb.RoleMember, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(h, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	kids, err := db.CreateGroup("Kids at school")
	if err != nil {
		t.Fatal(err)
	}
	kai, _ := db.ResolvePersonRef("kai")
	if err := db.AddPersonToGroup(kids.ID, kai, "test"); err != nil {
		t.Fatal(err)
	}
	reg := sharedcalendar.Legacy()
	reg.Calendars = append(reg.Calendars, sharedcalendar.Calendar{ID: "school", Name: "School", Color: "#f59e0b",
		ReadGroups: []string{kids.ID, "adults"}, WriteMembers: []string{"sam", "nobody"}})
	desired := calendarDesired(t, reg)

	if err := db.RenameButlerMemberHandle("sam", "samuel"); err != nil {
		t.Fatal(err)
	}
	out, drift, err := projectCalendarRights(db, desired)
	if err != nil {
		t.Fatal(err)
	}
	rights := projectedRights(t, out)
	for _, want := range []string{"[school-rw]\nuser: samuel|nobody\n", "[school-r]\nuser: kai\ngroups: adults\n"} {
		if !strings.Contains(rights, want) {
			t.Errorf("rights lack %q:\n%s", want, rights)
		}
	}
	if strings.Contains(projectedRights(t, desired), "samuel") {
		t.Fatal("projection edited desired state instead of a copy")
	}
	reported := map[string]bool{}
	for _, d := range drift {
		reported[d.Account] = true
	}
	if !reported["nobody"] || !reported["adults"] || reported["calendar_users"] {
		t.Errorf("drift = %v", drift)
	}

	// Somebody taken out of the group stops reaching it at the next pass.
	if err := db.RemovePersonFromGroup(kids.ID, kai); err != nil {
		t.Fatal(err)
	}
	out, _, err = projectCalendarRights(db, desired)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(projectedRights(t, out), "user: kai") {
		t.Error("a person removed from the group kept the calendar")
	}
}
