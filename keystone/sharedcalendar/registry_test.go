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
	if !c.Allows("sam", []string{"calendar_users"}, false) || c.Allows("sam", []string{"calendar_users"}, true) || !c.Allows("sam", []string{"adults"}, true) || c.Allows("", []string{"adults"}, true) {
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
