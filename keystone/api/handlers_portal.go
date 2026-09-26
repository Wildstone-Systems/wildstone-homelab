package api

// Serving the portal's own pages.
//
// Four documents, embedded in the binary, each behind the same door as
// the endpoints it calls. A page is authorised before it is served —
// rather than served to anyone and made useless by failing API calls —
// so a household member who is not in the group meets one refusal
// instead of a working-looking surface that answers 403 to everything.

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// PortalPrefixes are the URL prefixes the portal's pages live under,
// exported because cmd/keystone puts the Console's static file server
// in front of this handler and has to know which paths are not the
// Console's. Without it the dashboard's iframe renders the Console's
// index.html, which is a blank screen and a very confusing bug report.
//
// Every portal page registered in api.go must be under one of these, and
// TestEveryPortalPageIsOutsideTheConsole holds that: /upkeep/ was added
// as a route and never here, so the homepage's "Coming up" link opened
// the Console and asked a family member for a bearer token. /agenda/
// then did exactly the same thing, which is why the test is worth more
// than the comment.
var PortalPrefixes = []string{"/filedrop/", "/assistant/", "/home/", "/portal/", "/activity/", "/catalog/", "/calendar/", "/upkeep/", "/agenda/", "/lists/"}

// dashboardCapabilities are the capabilities whose Routes serve a
// household dashboard — the pages entitled to frame a portal surface.
//
// A closed set, because it is a security answer: anything else framing
// these pages is a site putting a household member's signed-in session
// in front of them and driving it.
var dashboardCapabilities = map[string]bool{"dashboard": true, "homarr": true}

// frameAncestors is the browser origins that may embed a portal page,
// derived from the dashboards this household actually runs.
//
// From desired state rather than configuration, and through the same
// helper the dashboard's own links use: under the tailnet posture a
// Route is served at the node's name and a port, so an origin computed
// any other way is one the browser never sees and a CSP that blocks the
// dashboard it was meant to allow.
func frameAncestors(desired []resource.Resource) []string {
	posture := ingressorigin.Of(desired)
	seen := map[string]bool{}
	var out []string
	for _, r := range desired {
		if r.Kind != resource.KindRoute || r.Route == nil || r.Route.Host == "" {
			continue
		}
		if !dashboardCapabilities[strings.TrimPrefix(r.Metadata.Owner, "capability/")] {
			continue
		}
		served := posture.SiteAddress(r.Route.Host)
		// Both forms of a default-port origin. The dashboard is on 443
		// under the tailnet posture, and a browser framing from it sends
		// an origin with NO port in it — so a source expression carrying
		// :443 is relying on the user agent applying the default-port
		// equivalence rule. Most do; the cost of one of them not doing
		// so is a blank frame on the family's dashboard with nothing
		// anywhere saying why, which is too quiet a failure to gamble on
		// for the sake of one string.
		for _, origin := range []string{"https://" + served, "https://" + strings.TrimSuffix(served, ":443")} {
			if !seen[origin] {
				seen[origin] = true
				out = append(out, origin)
			}
		}
	}
	sort.Strings(out)
	return out
}

// portalPage serves one embedded page.
func (s *Server) portalPage(name string) portalFunc {
	return func(w http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
		body, ok := portal.Page(name)
		if !ok {
			return nil, http.StatusNotFound, fmt.Errorf("no such page")
		}
		// The viewer's theme is rendered INTO the page rather than
		// fetched by it. Keystone already knows who is asking — the
		// door resolved them before this handler ran — so a second
		// round trip would buy nothing and cost a visible flash of the
		// default palette on every navigation, on the wall tablet,
		// where somebody is standing watching it.
		portal.WritePage(w, portal.InjectTheme(body, s.themeCSS(member)), frameAncestors(desired))
		return nil, http.StatusOK, nil
	}
}

// portalAssetFile serves one static design file by "<dir>/<name>".
//
// The path comes from two separate wildcards rather than one catch-all,
// so a request can express exactly two segments and nothing deeper.
// portal.Asset checks them again against its own closed sets: this is a
// URL space of one stylesheet, two fonts and forty icons, and it should
// not be possible to say anything else in it.
func (s *Server) portalAssetFile(w http.ResponseWriter, r *http.Request, _ string, _ []resource.Resource) (any, int, error) {
	body, ctype, ok := portal.Asset(r.PathValue("dir") + "/" + r.PathValue("name"))
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("no such file")
	}
	portal.WriteAsset(w, r, body, ctype)
	return nil, http.StatusOK, nil
}
