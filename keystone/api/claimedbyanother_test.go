package api

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The live case: calendar-radicale 2.0.0 rendered Route/calendar, the
// Route mail-roundcube's Davis already held, and the install re-pointed
// it at Radicale.
func TestEnableRefusesAnotherCapabilitysResource(t *testing.T) {
	current := []resource.Resource{
		byPkg(resource.KindRoute, "calendar", "capability/mail", "mail-roundcube"),
		byPkg(resource.KindContainer, "davis", "capability/mail", "mail-roundcube"),
	}
	rendered := []resource.Resource{
		owned(resource.KindRoute, "calendar", "capability/calendar"),
		owned(resource.KindContainer, "radicale", "capability/calendar"),
	}
	err := claimedByAnotherCapability(testPkg("calendar-radicale", "calendar"), current, rendered)
	if err == nil {
		t.Fatal("want a refusal: Route/calendar belongs to capability/mail")
	}
	if !strings.Contains(err.Error(), "Route/calendar (held by capability/mail)") {
		t.Errorf("the refusal should name the resource and its owner: %v", err)
	}
}

// Sibling profiles of one capability share names on purpose, and an
// upgrade re-renders what the same capability already owns. Neither is
// a takeover. Neither is a resource nobody's capability declared.
func TestEnableAllowsSameCapabilityAndUnownedNames(t *testing.T) {
	current := []resource.Resource{
		byPkg(resource.KindSecret, "restic-books", "capability/books", "books-kavita"),
		owned(resource.KindRoute, "handmade", ""),
	}
	rendered := []resource.Resource{
		owned(resource.KindSecret, "restic-books", "capability/books"),
		owned(resource.KindRoute, "handmade", "capability/books"),
	}
	if err := claimedByAnotherCapability(testPkg("books-calibre-web", "books"), current, rendered); err != nil {
		t.Errorf("want no refusal, got %v", err)
	}
}
