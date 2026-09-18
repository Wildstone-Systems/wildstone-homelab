package api

// The Catalog: everything the household could run, and the request that
// turns "ask Ray for an app" into something the owner can act on.
//
// SENDING INSTALLS NOTHING, and this file deliberately does not propose
// a plan either. §12.5 draws the line precisely: a member drafts, the
// owner approves "and picks the machine" — so the one field a plan
// cannot do without is the one field a member must not fill in. Filing a
// half-made plan would either invent a node on their behalf or put a
// plan in the approval queue that cannot be approved without editing,
// and an owner cannot tell the second from a bug.
//
// So a request is a HOUSEHOLD EVENT. That is not a workaround: it is the
// stream the Console already reads, it is already audience-scoped, the
// owner already sees it, and it needs no new table to hold a thing whose
// whole lifetime is "somebody asked, then somebody installed it". The
// state a member sees is derived — waiting while the capability is
// absent, installed once desired state has it — so it cannot go stale
// against what is actually running.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// catalogEventType is the household verb a request is recorded as.
const catalogEventType = "catalog.requested"

// catalogCard is one thing the household could add.
type catalogCard struct {
	Package    string `json:"package"`
	Capability string `json:"capability"`
	Profile    string `json:"profile"`
	Title      string `json:"title"`
	// What is the household sentence for this app, or empty when nothing
	// describes it — in which case the page says so rather than inventing
	// one. A card that made up what an app does is the one thing a person
	// deciding whether to ask for it cannot afford.
	What string `json:"what,omitempty"`
	Icon string `json:"icon,omitempty"`
	// Category is what it is FOR, which is the question a member
	// actually arrived with.
	Category string `json:"category"`
	// Sensitivity is "personal", "sensitive", or empty — and empty emits
	// no pill at all.
	Sensitivity string `json:"sensitivity,omitempty"`
	// Leaves reports that using it sends household content outside the
	// house.
	Leaves bool `json:"leaves,omitempty"`
	// Installed is whether THIS package is the one running. An installed
	// card shows in place, dimmed, so nobody requests a second Jellyfin.
	Installed bool `json:"installed"`
	// SameJobAs names an already-installed package that does this job,
	// so a duplicate request is flagged rather than refused: a household
	// comparing two music servers is doing something ordinary.
	SameJobAs string `json:"sameJobAs,omitempty"`
	// Alternatives are the other profiles of this capability, so the
	// draft can offer a choice where the catalog has one.
	Alternatives []string `json:"alternatives,omitempty"`
}

// catalogList answers what the household could add.
func (s *Server) catalogList(_ http.ResponseWriter, _ *http.Request, _ string, desired []resource.Resource) (any, int, error) {
	if s.Catalog == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this deployment has no catalog to show")
	}
	// What is installed, by capability and by package, read from desired
	// state rather than from a plan ledger: the question is what is
	// running, not what was once approved.
	installedCap := map[string]string{}
	for _, r := range desired {
		owner := strings.TrimPrefix(r.Metadata.Owner, "capability/")
		if owner == "" || owner == r.Metadata.Owner {
			continue
		}
		if r.Metadata.Package != "" {
			installedCap[owner] = r.Metadata.Package
		} else if _, seen := installedCap[owner]; !seen {
			installedCap[owner] = ""
		}
	}

	profiles := map[string][]string{}
	for _, p := range s.Catalog.List() {
		if !portal.Requestable(p.Metadata.Capability) {
			continue
		}
		profiles[p.Metadata.Capability] = append(profiles[p.Metadata.Capability], p.Metadata.Profile)
	}

	cards := []catalogCard{}
	for _, p := range s.Catalog.List() {
		cap := p.Metadata.Capability
		// The omission is part of the design: operator-class
		// capabilities are absent from this surface, not refused on it.
		if !portal.Requestable(cap) {
			continue
		}
		key := portal.DescribeKey(p.Metadata.Profile)
		title, what, described := reconcile.DescribeProfile(key)
		if !described {
			title = humanise(p.Metadata.Profile)
		}
		card := catalogCard{
			Package:     p.Metadata.Name,
			Capability:  cap,
			Profile:     p.Metadata.Profile,
			Title:       title,
			What:        what,
			Icon:        packageIcon(p),
			Category:    portal.CategoryFor(cap),
			Sensitivity: portal.SensitivityNote(p.Policies.DataSensitivity),
			Leaves:      portal.LeavesTheHouse(cap),
		}
		if running, ok := installedCap[cap]; ok {
			if running == p.Metadata.Name {
				card.Installed = true
			} else if running != "" {
				// Allowed but flagged: "this does the same job as X".
				if t, _, ok := reconcile.DescribeProfile(portal.DescribeKey(profileOf(s, running))); ok {
					card.SameJobAs = t
				} else {
					card.SameJobAs = humanise(running)
				}
			}
		}
		if alts := profiles[cap]; len(alts) > 1 {
			card.Alternatives = alts
		}
		cards = append(cards, card)
	}
	// Installed last within a category, then alphabetical: a member
	// browsing for something new should not read past what they have.
	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].Installed != cards[j].Installed {
			return !cards[i].Installed
		}
		return cards[i].Title < cards[j].Title
	})
	return map[string]any{
		"cards":      cards,
		"categories": portal.Categories,
		// The omission note, served rather than written into the page, so
		// every surface that shows this list says the same thing.
		"note": "Sign-in, the network, backups, monitoring and the machines aren't here. " +
			"Those are the owner's, and they're set up from the Console.",
	}, http.StatusOK, nil
}

// profileOf recovers a package's profile from the catalog by name, for
// naming the thing an installed sibling is.
func profileOf(s *Server, pkg string) string {
	for _, p := range s.Catalog.List() {
		if p.Metadata.Name == pkg {
			return p.Metadata.Profile
		}
	}
	return pkg
}

// humanise turns a package or profile name into something to show when
// nothing describes it: "calibre-web" becomes "Calibre Web".
func humanise(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// catalogRequest files one request.
//
// It records a household event and nothing else: no plan, no resource,
// no install. What the member supplies is the package, optionally which
// profile of it, and a note — never a node, a path, an address or a
// secret, because there is no field here in which to say one.
func (s *Server) catalogRequest(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	if s.Catalog == nil || s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this deployment has no catalog")
	}
	pkg := strings.TrimSpace(r.URL.Query().Get("package"))
	if pkg == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("which app?")
	}
	// The package must be one the catalog actually has AND one this
	// surface offers. Checked here rather than trusted from the list the
	// client was given, which is the same rule the Assistant's roster
	// follows: the client may hide entries, the server decides.
	var found bool
	var capability string
	for _, p := range s.Catalog.List() {
		if p.Metadata.Name == pkg {
			found, capability = true, p.Metadata.Capability
			break
		}
	}
	if !found {
		return nil, http.StatusNotFound, fmt.Errorf("there's no such app in the catalog")
	}
	if !portal.Requestable(capability) {
		// Shaped as not-found rather than forbidden: that the catalog
		// contains an ingress package is not something a household
		// surface should confirm.
		return nil, http.StatusNotFound, fmt.Errorf("there's no such app in the catalog")
	}
	note := strings.TrimSpace(r.URL.Query().Get("note"))
	if len([]rune(note)) > 500 {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("that note is longer than this form carries")
	}
	profile := strings.TrimSpace(r.URL.Query().Get("profile"))

	payload := fmt.Sprintf(`{"package":%q,"capability":%q,"profile":%q,"note":%q}`,
		pkg, capability, profile, note)
	// Deduplicated on the member and the package, so pressing Send twice
	// is one request rather than two rows for the owner to read.
	if _, _, err := s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID:       "catalog:" + member + ":" + pkg,
		Source:        "catalog",
		Type:          catalogEventType,
		Subject:       member,
		Entity:        pkg,
		Payload:       []byte(payload),
		OccurredAt:    time.Now(),
		SchemaVersion: 1,
	}); err != nil {
		s.Log.Error("catalog request", "err", err)
		return nil, http.StatusInternalServerError, fmt.Errorf("that didn't send")
	}
	// The operator's log too, because this is a thing a person asked the
	// household to do and the owner's record should name it.
	s.event("catalog.requested", "", principal.Identity{Name: portalPrincipal, ActingFor: member},
		fmt.Sprintf("%s asked for %s (%s)", member, pkg, capability))
	return map[string]any{"package": pkg, "sent": true}, http.StatusOK, nil
}

// catalogRequests lists this member's own requests, newest first.
//
// State is DERIVED rather than stored: waiting while the capability is
// absent from desired state, installed once it is there. A stored state
// would be a second answer to "is this running", and the second answer
// is the one that goes stale — a member would be told their request is
// still waiting a week after the owner installed it.
// maxCatalogRequests bounds the list. A household's request history
// grows without limit and a page does not.
const maxCatalogRequests = 100

func (s *Server) catalogRequests(_ http.ResponseWriter, _ *http.Request, member string, desired []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this deployment has no catalog")
	}
	installed := map[string]bool{}
	for _, r := range desired {
		if r.Metadata.Package != "" {
			installed[r.Metadata.Package] = true
		}
	}
	// Asked for as this member's catalog requests rather than as the
	// newest four hundred household events. A busy week of uploads and
	// routines used to fill that window, and a member whose requests
	// were older than it saw an empty page while the owner still had
	// their request waiting.
	events, err := s.DB.HouseholdEventsBefore(0, maxCatalogRequests, runtimedb.HouseholdEventFilter{
		Type: catalogEventType, Subject: member,
	})
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("can't read your requests")
	}
	type request struct {
		Package string    `json:"package"`
		At      time.Time `json:"at"`
		State   string    `json:"state"`
		Note    string    `json:"note,omitempty"`
	}
	out := []request{}
	for _, e := range events {
		state := "waiting on the owner"
		if installed[e.Entity] {
			state = "installed"
		}
		out = append(out, request{
			Package: e.Entity,
			At:      e.OccurredAt,
			State:   state,
			Note:    noteFrom(e.Payload),
		})
	}
	return map[string]any{
		"requests": out,
		"member":   member,
		// Said plainly, because it is the thing a member most needs to
		// know about this list and the thing a "Send" button most
		// invites them to forget.
		"note": "The owner installs these, and may say no.",
	}, http.StatusOK, nil
}

// noteFrom pulls the member's note back out of a recorded request.
//
// A deliberately small reader rather than a JSON decode into a struct:
// the payload is written by catalogRequest three functions up, the only
// field a person typed is this one, and a parse failure yields no note
// rather than an error a member would see on their own request list.
// DECODED RATHER THAN SCANNED FOR. This used to look for `"note":"`
// and stop at the next quote, which is not where the value ends: JSON
// escapes a quote inside a string as \", so a note reading
//
//	Use the "family" edition
//
// came back to the member as `Use the \` — and every other escape, a
// newline or an apostrophe run through \u, reached the page still
// encoded. A member's own words are the one thing on this page that
// must survive the round trip intact.
func noteFrom(payload []byte) string {
	var decoded struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		// A payload that is not the shape this expects is a note that
		// is absent, not an error a member would see on their own
		// request list.
		return ""
	}
	return decoded.Note
}
