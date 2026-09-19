package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every portal page is outside the Console.
//
// cmd/keystone puts the Console's static server in front of the API and
// hands only PortalPrefixes (and /api/) to this package; anything else
// gets the Console's index.html. A page route added in api.go without
// its prefix therefore works in every test that calls the API handler
// directly and fails in production, where the Console answers first and
// asks for a bearer token. That is what happened to /upkeep/.
//
// Read from the source because the mux is not introspectable: this
// finds every route handed to s.portal and checks its path.
func TestEveryPortalPageIsOutsideTheConsole(t *testing.T) {
	raw, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("reading api.go: %v", err)
	}
	route := regexp.MustCompile(`mux\.Handle\("(?:[A-Z]+ )?(/[^"]*)", s\.portal\(`)
	found := 0
	for _, m := range route.FindAllStringSubmatch(string(raw), -1) {
		path := m[1]
		if strings.HasPrefix(path, "/api/") {
			continue
		}
		found++
		covered := false
		for _, p := range PortalPrefixes {
			if strings.HasPrefix(path, p) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("portal route %s is not under any PortalPrefixes entry, so the Console "+
				"serves it instead and asks for a bearer token", path)
		}
	}
	if found == 0 {
		t.Fatal("found no portal page routes; the pattern no longer matches api.go")
	}
}
