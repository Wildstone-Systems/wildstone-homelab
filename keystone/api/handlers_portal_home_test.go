package api

// The household's front page, and mostly what it refuses.
//
// The page itself is presentation and a browser will show whether it
// renders. What cannot be checked by looking at it is the set of
// properties below, every one of which is a claim the implementation
// package makes in prose and would otherwise be true only by accident:
// that a second dashboard profile does not leave a second front door
// open, that a member cannot answer somebody else's question, that
// clearing notifications cannot reach another person's phone, and that
// an event nobody has written a sentence for is dropped rather than
// printed as machine text.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// homeHarness is a deployment running the dashboard capability's
// wildstone profile — the standalone page — with a household name and a
// hue in desired state.
func homeHarness(t *testing.T) *harness {
	t.Helper()
	h, _ := portalHarness(t)
	for _, r := range []resource.Resource{
		{
			ApiVersion: "wildstone.dev/v1", Kind: resource.KindLdapGroup,
			Metadata:  resource.Metadata{Name: "homepage-users", Owner: "capability/dashboard", Package: "homepage-wildstone", Node: "n1"},
			LdapGroup: &resource.LdapGroupSpec{Name: "homepage_users"},
		},
		{
			ApiVersion: "wildstone.dev/v1", Kind: resource.KindConfigFile,
			Metadata:   resource.Metadata{Name: "homepage-settings", Owner: "capability/dashboard", Package: "homepage-wildstone", Node: "n1"},
			ConfigFile: &resource.ConfigFileSpec{Filename: "settings.yaml", Content: "householdName: |-\n  The Reed Family\naccentHue: 400\n"},
		},
	} {
		if err := h.s.State.Put(r); err != nil {
			t.Fatalf("Put(%s): %v", r.Metadata.Name, err)
		}
	}
	return h
}

// summary reads the whole page as the proxy would for one member.
func (h *harness) summary(t *testing.T, user string) homeSummaryResponse {
	t.Helper()
	res := h.member(http.MethodGet, "/api/v1/home/summary", user, "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("summary for %s: status %d", user, res.StatusCode)
	}
	var got homeSummaryResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding summary: %v", err)
	}
	return got
}

// The Homepage profile must not leave the Wildstone page answering.
//
// Both profiles install the `dashboard` capability, and only the
// wildstone one declares a group. This is the test that the shared
// capability name does not become a second front door: a household on
// gethomepage has no LdapGroup for `dashboard`, so the page and its API
// are absent rather than reachable.
func TestTheStandalonePageIsAbsentUnderTheHomepageProfile(t *testing.T) {
	h, _ := portalHarness(t) // filedrop and assistant only: no dashboard group
	for _, path := range []string{"/home/", "/api/v1/home/summary"} {
		res := h.member(http.MethodGet, path, "sandy", "homepage_users,filedrop_users", nil, "")
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s with no dashboard group: status %d, want 404 — an uninstalled surface must look absent", path, res.StatusCode)
		}
	}
}

// Membership is the permission, on every request rather than at sign-in.
func TestTheFrontPageRefusesAMemberOutsideItsGroup(t *testing.T) {
	h := homeHarness(t)
	// In the household's other groups, but not this one.
	res := h.member(http.MethodGet, "/api/v1/home/summary", "sandy", "filedrop_users,assistant_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", res.StatusCode)
	}
}

// A header anyone can set is not an authentication.
func TestTheFrontPageRefusesAnUntrustedPeer(t *testing.T) {
	h := homeHarness(t)
	h.s.SetTrustedIngress(nil) // an unconfigured trust boundary must fail closed
	res := h.member(http.MethodGet, "/api/v1/home/summary", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", res.StatusCode)
	}
}

// The identity this page is reached with holds no API verb at all, so a
// member whose browser wanders off the portal gets nothing.
func TestAFrontPageMemberCannotReachTheControlPlane(t *testing.T) {
	h := homeHarness(t)
	for _, path := range []string{
		"/api/v1/resources", "/api/v1/nodes", "/api/v1/plans", "/api/v1/audit", "/api/v1/principals",
	} {
		res := h.member(http.MethodGet, path, "sandy", "homepage_users", nil, "")
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s as a household member: status %d, want 401 — the portal identity is not a principal", path, res.StatusCode)
		}
	}
}

// The household's presentation comes from desired state, and a hue is an
// angle: 400 degrees is 40, not an error and not a clamp to 360.
func TestTheHouseholdNamesItselfAndItsHueWraps(t *testing.T) {
	h := homeHarness(t)
	got := h.summary(t, "sandy")
	if got.Household != "The Reed Family" {
		t.Errorf("household = %q, want %q", got.Household, "The Reed Family")
	}
	if got.AccentHue != 40 {
		t.Errorf("accentHue = %d, want 40 (400 mod 360)", got.AccentHue)
	}
	if got.Member != "sandy" {
		t.Errorf("member = %q, want sandy — the page names the person the proxy proved", got.Member)
	}
}

// An install with nothing named still renders: "Home" rather than an
// empty heading, which would read as a broken page.
func TestAnUnnamedHouseholdStillHasAHeading(t *testing.T) {
	h, _ := portalHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindLdapGroup,
		Metadata:  resource.Metadata{Name: "homepage-users", Owner: "capability/dashboard", Package: "homepage-wildstone", Node: "n1"},
		LdapGroup: &resource.LdapGroupSpec{Name: "homepage_users"},
	}); err != nil {
		t.Fatal(err)
	}
	got := h.summary(t, "sandy")
	if got.Household != "Home" || got.AccentHue != defaultAccentHue {
		t.Fatalf("household = %q hue = %d, want Home and %d", got.Household, got.AccentHue, defaultAccentHue)
	}
}

// A question asked of one member is not another member's to answer, and
// is not even visible to them.
func TestOneMembersQuestionIsNotAnothersToSeeOrAnswer(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	raised, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "lock-up", Recipient: "maya", DedupeKey: "n1", Asks: true,
	}, runtimedb.DefaultButlerSettings(), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// Sandy's page does not carry Maya's question.
	sandy := h.summary(t, "sandy")
	for _, i := range sandy.NeedsYou.Items {
		if i.Seq == raised.Seq {
			t.Fatalf("sandy's needs-you carries maya's question %d", raised.Seq)
		}
	}
	// Maya's does, and it is hers to decide.
	maya := h.summary(t, "maya")
	found := false
	for _, i := range maya.NeedsYou.Items {
		if i.Seq == raised.Seq {
			found = true
			if !i.Decidable {
				t.Error("maya's own question is not decidable by her")
			}
		}
	}
	if !found {
		t.Fatalf("maya's needs-you does not carry her question %d", raised.Seq)
	}

	// And Sandy cannot answer it by naming the sequence number: a wrong
	// answerer reads as not-found, because "that is not yours" confirms
	// it exists and who it belongs to.
	res := h.member(http.MethodPost, "/api/v1/home/notices/answer?seq="+strconv.FormatInt(raised.Seq, 10)+"&answer=yes",
		"sandy", "homepage_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("sandy answering maya's question: status %d, want 404", res.StatusCode)
	}
	// Still unanswered, and still Maya's.
	after, err := h.s.DB.GetNotice(raised.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !after.AnsweredAt.IsZero() {
		t.Fatalf("maya's question was answered by %q", after.AnsweredBy)
	}
}

// Clearing notifications is per member, and it cannot be aimed.
//
// The endpoint takes no sequence number at all, so the set it can touch
// comes from a member-scoped query. This is the test that clearing on
// the wall tablet does not clear anybody else's phone.
func TestClearingNotificationsCannotReachAnotherPerson(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	mine, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "washing-done", Recipient: "sandy", DedupeKey: "s1",
	}, runtimedb.DefaultButlerSettings(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "washing-done", Recipient: "maya", DedupeKey: "m1",
	}, runtimedb.DefaultButlerSettings(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// An unanswered question of Sandy's own, which Clear must leave
	// alone: acknowledging it would take the decision off her page
	// while whatever waits on it still waits.
	asked, err := h.s.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: "lock-up", Recipient: "sandy", DedupeKey: "s2", Asks: true,
	}, runtimedb.DefaultButlerSettings(), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	res := h.member(http.MethodPost, "/api/v1/home/notices/clear", "sandy", "homepage_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("clear: status %d", res.StatusCode)
	}

	if n, _ := h.s.DB.GetNotice(mine.Seq); n.AcknowledgedAt.IsZero() {
		t.Error("sandy's own notification was not cleared")
	}
	if n, _ := h.s.DB.GetNotice(theirs.Seq); !n.AcknowledgedAt.IsZero() {
		t.Error("maya's notification was cleared by sandy — Clear is per member")
	}
	if n, _ := h.s.DB.GetNotice(asked.Seq); !n.AcknowledgedAt.IsZero() {
		t.Error("an unanswered question was cleared away; the decision would be lost")
	}
}

// An unmapped verb is a bug, and the row is omitted rather than shown as
// machine text. The count is reported so the bug can be found.
func TestAnEventNobodyHasASentenceForIsDroppedNotPrinted(t *testing.T) {
	h := homeHarness(t)
	for _, e := range []runtimedb.HouseholdEvent{
		{EventID: "e1", Source: "home", Type: "person.arrived_home", Subject: "Sandy", OccurredAt: time.Now(), SchemaVersion: 1},
		{EventID: "e2", Source: "keystone", Type: "reconcile.applied", OccurredAt: time.Now(), SchemaVersion: 1},
	} {
		if _, _, err := h.s.DB.AppendHouseholdEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	got := h.summary(t, "sandy")
	if !got.Activity.Available {
		t.Fatalf("activity unavailable: %q", got.Activity.Problem)
	}
	for _, r := range got.Activity.Rows {
		if r.Sentence == "reconcile.applied" || r.Sentence == "" {
			t.Errorf("machine text reached the household surface: %q", r.Sentence)
		}
	}
	if got.Activity.Dropped != 1 {
		t.Errorf("dropped = %d, want 1 — an unmapped verb must be counted, not hidden", got.Activity.Dropped)
	}
	if len(got.Activity.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(got.Activity.Rows))
	}
	if got.Activity.Rows[0].Sentence != "Sandy got home" {
		t.Errorf("sentence = %q", got.Activity.Rows[0].Sentence)
	}
}

// Nobody having checked is not the same as everything being down.
//
// With an empty observation table the page must say it cannot check,
// rather than render a grid of red dots that reads as a total outage.
func TestUncheckedAppsAreUnknownRatherThanDown(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindRoute,
		Metadata: resource.Metadata{Name: "photos", Owner: "capability/photos", Package: "photos-immich", Node: "n1"},
		Route: &resource.RouteSpec{
			Host: "photos.example.com", Upstream: "container/immich:2283",
			Auth: "authelia_oidc", Classification: "family_public",
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := h.summary(t, "sandy")
	if got.StatusKnown {
		t.Error("statusKnown with no observations — the page would draw dots it cannot source")
	}
	for route, st := range got.AppStatus {
		if st == "bad" {
			t.Errorf("%s = bad with nothing observed; unknown is the honest answer", route)
		}
	}
}

// The reassurance card withholds reassurance it cannot source, and never
// draws a bar at zero.
func TestTheMachinesCardSaysWhatItCannotRead(t *testing.T) {
	h := homeHarness(t)
	got := h.summary(t, "sandy")
	if got.Machines.Metrics.Available {
		t.Error("machine metrics claim to be available; Keystone holds no host readings")
	}
	if got.Machines.Metrics.Problem == "" {
		t.Error("no sentence in place of the bars")
	}
	if !got.Machines.LastDrill.IsZero() || got.Machines.DrillOK {
		t.Error("a restore is claimed proved with no drill in the ledger")
	}
}

// The household's calendar server is found by the container that speaks
// CalDAV, not by a capability called "calendar".
//
// This is a real install's shape: davis ships inside the mail
// capability's roundcube package, so a check for capability/calendar
// answers "no calendar" on a household that has one in front of it.
func TestTheCalendarIsFoundByTheServerThatSpeaksCaldav(t *testing.T) {
	h := homeHarness(t)
	if got := h.summary(t, "sandy"); got.Calendar.Problem != "No calendar is set up yet." {
		t.Fatalf("with no calendar: problem = %q", got.Calendar.Problem)
	}
	// davis, owned by the MAIL capability, which is where it really lives.
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindRoute,
		Metadata: resource.Metadata{Name: "calendar", Owner: "capability/mail", Package: "mail-roundcube", Node: "n1"},
		Route: &resource.RouteSpec{
			Host: "calendar.example.com", Upstream: "container/davis:9000",
			Auth: "none", Classification: "family_private",
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := h.summary(t, "sandy")
	if got.Calendar.Available {
		t.Error("the card claims to have read a calendar it cannot read yet")
	}
	// Found, so the copy changes from "none installed" to the reason
	// this one cannot be read from here. Which sentence that is, and
	// why it is not the transient one, is pinned by
	// TestTheCalendarCardIsHonestAndHandsOff.
	if strings.Contains(got.Calendar.Problem, "No calendar") {
		t.Errorf("problem = %q — the household HAS a calendar server", got.Calendar.Problem)
	}
	if got.Calendar.OpenAt == "" {
		t.Error("a calendar server was found but no hand-off was offered")
	}
}

// A member's own movements are theirs to read; somebody else's are not.
//
// The first version of this filter withheld every member-scoped event
// from everybody, so the card omitted the arrival that had just
// happened while still showing the routine it triggered — the reader
// was the one person hidden from their own page.
func TestAMemberSeesTheirOwnMovementsAndNotAnotherPersons(t *testing.T) {
	h := homeHarness(t)
	// Presence is about a person, so it is member-scoped.
	h.s.MemberScoped = func(eventType string) bool { return strings.HasPrefix(eventType, "person.") }
	for _, e := range []runtimedb.HouseholdEvent{
		// Cased differently on purpose: a directory name and a display
		// name are not the same string.
		{EventID: "a1", Source: "home", Type: "person.arrived_home", Subject: "Mika", OccurredAt: time.Now(), SchemaVersion: 1},
		{EventID: "a2", Source: "home", Type: "person.left_home", Subject: "Giulia", OccurredAt: time.Now(), SchemaVersion: 1},
		// Not about a person: the whole house may read it.
		{EventID: "a3", Source: "automation", Type: "automation.acted", Subject: "Evening lock-up", OccurredAt: time.Now(), SchemaVersion: 1},
	} {
		if _, _, err := h.s.DB.AppendHouseholdEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	got := h.summary(t, "mika")
	var seen []string
	for _, r := range got.Activity.Rows {
		seen = append(seen, r.Sentence)
	}
	joined := strings.Join(seen, " | ")
	if !strings.Contains(joined, "Mika got home") {
		t.Errorf("mika cannot see his own arrival: %s", joined)
	}
	if strings.Contains(joined, "Giulia") {
		t.Errorf("giulia's movements reached mika's page: %s", joined)
	}
	if !strings.Contains(joined, "Evening lock-up ran on its own") {
		t.Errorf("a household-wide event was withheld: %s", joined)
	}
}

// The group order is a copy, not the renderer's own slice.
//
// Handing callers the backing array let the first one that appended to
// it rewrite the order for everybody, which is how "Quick actions" came
// back twice on the first page this served.
func TestTheGroupOrderCannotBeRewrittenByACaller(t *testing.T) {
	first := reconcile.Groups()
	_ = append(reconcile.Groups(), "Mine")
	second := reconcile.Groups()
	if len(first) != len(second) {
		t.Fatalf("group count changed from %d to %d", len(first), len(second))
	}
	seen := map[string]bool{}
	for _, g := range second {
		if seen[g] {
			t.Errorf("%q appears twice in %v", g, second)
		}
		seen[g] = true
	}
}

// The design files serve to any household member, and to nobody else.
//
// They carry no household data — a font and an icon set say nothing
// about who lives here — but Keystone's listener is reachable on the LAN
// without the identity proxy, and "nothing on this listener answers an
// unauthenticated caller but healthz" is worth keeping whole.
func TestTheDesignFilesServeToMembersOnly(t *testing.T) {
	h := homeHarness(t)
	for _, path := range []string{
		"/portal/assets/nocturne.css",
		"/portal/assets/inter-latin.woff2",
		"/portal/icons/jellyfin.svg",
	} {
		res := h.member(http.MethodGet, path, "sandy", "homepage_users", nil, "")
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s as a member: status %d, want 200", path, res.StatusCode)
			continue
		}
		if len(body) == 0 {
			t.Errorf("%s served empty", path)
		}
		// Cached, but CHECKED. This asserted `immutable` and the
		// sentence under it was "an asset's bytes never change under
		// its name" — which is false for exactly these URLs: they
		// carry no version, and a new Keystone serves new bytes at the
		// same address. `immutable` told the browser not to ask, so it
		// never did, and two deployed stylesheet fixes were invisible
		// to the only person looking at them. See portal.WriteAsset.
		cc := res.Header.Get("Cache-Control")
		if strings.Contains(cc, "immutable") {
			t.Errorf("%s is immutable over an unversioned URL, so a deploy cannot replace it: %q", path, cc)
		}
		if !strings.Contains(cc, "must-revalidate") {
			t.Errorf("%s Cache-Control = %q, want must-revalidate", path, cc)
		}
		if res.Header.Get("ETag") == "" {
			t.Errorf("%s carries no ETag, so every revalidation resends the whole file", path)
		}
	}

	// A member of ANY portal surface's group gets them, because one
	// stylesheet dresses all three surfaces.
	res := h.member(http.MethodGet, "/portal/assets/nocturne.css", "sandy", "filedrop_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("a filedrop-only member: status %d, want 200 — the stylesheet dresses that surface too", res.StatusCode)
	}

	// Nobody outside the household.
	res = h.member(http.MethodGet, "/portal/assets/nocturne.css", "sandy", "some_other_group", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a non-member: status %d, want 403", res.StatusCode)
	}
}

// The asset URL space is one stylesheet, two fonts and forty icons, and
// it must not be possible to say anything else in it.
func TestTheAssetPathCannotEscapeItsDirectories(t *testing.T) {
	h := homeHarness(t)
	for _, path := range []string{
		"/portal/assets/..%2f..%2fweb.go",
		"/portal/web/home.html",
		"/portal/assets/home.html",
		"/portal/icons/nocturne.css",
		"/portal/assets/",
	} {
		res := h.member(http.MethodGet, path, "sandy", "homepage_users", nil, "")
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("%s served 200; the asset space is a closed set", path)
		}
	}
}

// The page must actually reference the stylesheet, or the whole asset
// pipeline is dead weight and the page silently renders unstyled.
func TestTheFrontPageLinksTheDesignSystem(t *testing.T) {
	h := homeHarness(t)
	res := h.member(http.MethodGet, "/home/", "sandy", "homepage_users", nil, "")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	page := string(body)
	if !strings.Contains(page, "/portal/assets/nocturne.css") {
		t.Error("the page does not link the design system")
	}
	// And the policy it is served with has to permit that link, plus the
	// fonts the stylesheet pulls.
	csp := res.Header.Get("Content-Security-Policy")
	for _, need := range []string{"style-src 'self'", "font-src 'self'", "img-src 'self'"} {
		if !strings.Contains(csp, need) {
			t.Errorf("CSP %q is missing %q — the stylesheet or its fonts would be blocked", csp, need)
		}
	}
	// Still no external script, which is the one thing that would make
	// "read the file to know what the surface does" false.
	if strings.Contains(csp, "script-src 'self'") {
		t.Error("script-src gained 'self'; a portal page's behaviour belongs in the page")
	}
}

// Sign out goes to the identity provider, not to a path on this
// surface. It used to point at /logout here, which Keystone has no
// route for, so the one control in the banner answered 404.
func TestSignOutPointsAtTheIdentityProvider(t *testing.T) {
	h := homeHarness(t)
	// No login page in desired state yet: the page must be told to hide
	// the control rather than render a dead one.
	if got := h.summary(t, "sandy"); got.SignOut != "" {
		t.Fatalf("signOut = %q with no login page declared", got.SignOut)
	}
	// The login page is the Route the proxy serves itself rather than
	// guarding — auth: portal.
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindRoute,
		Metadata: resource.Metadata{Name: "auth", Owner: "capability/identity", Package: "identity", Node: "n1"},
		Route: &resource.RouteSpec{
			Host: "auth.example.com", Upstream: "container/authelia:9091",
			Auth: "portal", Classification: "family_public",
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := h.summary(t, "sandy")
	if !strings.HasPrefix(got.SignOut, "https://") || !strings.HasSuffix(got.SignOut, "/logout") {
		t.Fatalf("signOut = %q, want an https URL ending in /logout", got.SignOut)
	}
	// And it must not be a path on this surface, which is the whole bug.
	if strings.Contains(got.SignOut, "/home/") || got.SignOut == "/logout" {
		t.Errorf("signOut = %q, which is this surface rather than the provider", got.SignOut)
	}
}

// The household's own surfaces are the widgets at the top of this page,
// so their launcher group is not shown underneath them as well.
//
// Under the Homepage profile "Quick actions" is how Files and Assistant
// are reachable at all — it can only present links. Here they are live
// cards, and the group beneath was the same two things a second time:
// a member scrolling past their upload card to find a card that opens
// it.
func TestTheHouseholdSurfacesAreNotAlsoLauncherCards(t *testing.T) {
	h := homeHarness(t)
	got := h.summary(t, "sandy")
	for _, g := range got.Groups {
		if g == reconcile.PortalGroup {
			t.Errorf("group %q is in the launcher order alongside the widgets", g)
		}
	}
	// And out of the list too, or search would still find them.
	for _, a := range got.Apps {
		if a.Group == reconcile.PortalGroup {
			t.Errorf("%q is still a launcher card", a.Title)
		}
	}
	// The ordinary groups survive: this filters one group, it does not
	// empty the launcher.
	var household bool
	for _, g := range got.Groups {
		if g == "Household" {
			household = true
		}
	}
	if !household {
		t.Errorf("groups = %v, want Household among them", got.Groups)
	}
}

// The banner reports itself as the default until a household sets one.
func TestTheBannerDefaultsUntilAHouseholdChoosesOne(t *testing.T) {
	h := homeHarness(t)
	got := h.summary(t, "sandy")
	if got.Banner.Custom {
		t.Error("the banner claims a household picture with none set")
	}
	if got.Banner.PositionX != 50 || got.Banner.PositionY != 50 {
		t.Errorf("framing = %v/%v, want centred", got.Banner.PositionX, got.Banner.PositionY)
	}

	// A picture, and the framing it starts at.
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	res := h.member(http.MethodPost, "/api/v1/home/banner", "sandy", "homepage_users",
		bytes.NewReader(png), "application/octet-stream")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("setting the banner: status %d", res.StatusCode)
	}
	got = h.summary(t, "sandy")
	if !got.Banner.Custom {
		t.Fatal("the banner is not reported as the household's own")
	}
	if got.Banner.Version == 0 {
		t.Error("no version on the banner; a browser would keep the old picture")
	}

	// Framing it moves only the framing.
	res = h.member(http.MethodPost, "/api/v1/home/banner/frame?x=20&y=80", "sandy", "homepage_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("framing: status %d", res.StatusCode)
	}
	if got = h.summary(t, "sandy"); got.Banner.PositionX != 20 || got.Banner.PositionY != 80 {
		t.Errorf("framing = %v/%v, want 20/80", got.Banner.PositionX, got.Banner.PositionY)
	}

	// Anything that is not a picture is refused, whatever it says it is.
	res = h.member(http.MethodPost, "/api/v1/home/banner", "sandy", "homepage_users",
		strings.NewReader("<!doctype html><script>alert(1)</script>"), "image/png")
	res.Body.Close()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("an HTML upload labelled image/png: status %d, want 415", res.StatusCode)
	}

	// And putting it back is not an error the second time.
	for i := 0; i < 2; i++ {
		res = h.member(http.MethodPost, "/api/v1/home/banner/clear", "sandy", "homepage_users", nil, "")
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("clear %d: status %d", i, res.StatusCode)
		}
	}
	if got = h.summary(t, "sandy"); got.Banner.Custom {
		t.Error("the banner is still the household's own after clearing")
	}
}

// The calendar card tells the truth about a permanent fact, and hands
// the member somewhere to go.
//
// "Can't reach the calendars right now" is §7's copy for a TRANSIENT
// failure, and using it here would promise that a permanent
// architectural fact will clear on its own — the household's CalDAV
// server authenticates with each member's mail password, which this
// surface does not hold and will not (docs/design/household-calendar.md).
func TestTheCalendarCardIsHonestAndHandsOff(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindRoute,
		Metadata: resource.Metadata{Name: "calendar", Owner: "capability/mail", Package: "mail-roundcube", Node: "n1"},
		Route: &resource.RouteSpec{
			Host: "calendar.example.com", Upstream: "container/davis:9000",
			Auth: "none", Classification: "family_private",
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := h.summary(t, "sandy").Calendar
	if got.Available {
		t.Error("the card claims to have read a calendar")
	}
	if strings.Contains(got.Problem, "right now") {
		t.Errorf("problem = %q — that copy promises a transient failure", got.Problem)
	}
	if !strings.Contains(got.Problem, "password") {
		t.Errorf("problem = %q, want it to name the actual obstacle", got.Problem)
	}
	// And somewhere to go: a member told no should be one tap from the
	// place they can sign in, not left to find it.
	if got.OpenAt == "" {
		t.Error("no hand-off to the calendar app")
	}
	if !strings.HasPrefix(got.OpenAt, "https://") {
		t.Errorf("openAt = %q, want a browser-usable address", got.OpenAt)
	}
}

// With no calendar server at all there is nothing to hand off to, and
// the card must not offer a link to nowhere.
func TestNoCalendarServerOffersNoHandOff(t *testing.T) {
	h := homeHarness(t)
	got := h.summary(t, "sandy").Calendar
	if got.OpenAt != "" {
		t.Errorf("openAt = %q with no calendar installed", got.OpenAt)
	}
	if !strings.Contains(got.Problem, "No calendar") {
		t.Errorf("problem = %q", got.Problem)
	}
}

// A polled read writes no audit row; everything else still does.
//
// The front page refreshes every twenty seconds, nothing prunes the
// audit table, and four thousand rows a day saying a dashboard
// refreshed itself would bury the rows that say a person did something.
// What must NOT be traded away is the refusals and the writes.
func TestAPolledReadIsNotAuditedButEverythingElseIs(t *testing.T) {
	h := homeHarness(t)

	countRows := func() int {
		rows, err := h.s.DB.ListAudit(500)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			if r.Principal == portalPrincipal {
				n++
			}
		}
		return n
	}

	before := countRows()

	// Four successful polls: no rows.
	for i := 0; i < 4; i++ {
		h.summary(t, "sandy")
	}
	if got := countRows(); got != before {
		t.Errorf("a successful poll wrote %d audit rows", got-before)
	}

	// A refusal on the same path still writes one — somebody outside
	// the group hammering this endpoint is what the log is for.
	res := h.member(http.MethodGet, "/api/v1/home/summary", "stranger", "some_other_group", nil, "")
	res.Body.Close()
	if got := countRows(); got != before+1 {
		t.Errorf("a refused poll wrote %d rows, want 1", got-before)
	}

	// And a write is always recorded, even on the polled surface.
	before = countRows()
	res = h.member(http.MethodPost, "/api/v1/home/notices/clear", "sandy", "homepage_users", nil, "")
	res.Body.Close()
	if got := countRows(); got <= before {
		t.Error("a write on the portal was not audited")
	}
}

// The suppression is a closed set, not a rule about GETs.
func TestOnlyTheNamedProjectionsGoUnaudited(t *testing.T) {
	get := func(path string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// The one that polls.
	if auditWorthy(get("/api/v1/home/summary"), 200) {
		t.Error("the polled summary is audited on success")
	}
	// Everything else a member can read is not in the set.
	for _, path := range []string{
		"/api/v1/activity", "/api/v1/filedrop/list", "/api/v1/filedrop/file",
		"/api/v1/assistant/memory", "/api/v1/assistant/conversations",
	} {
		if !auditWorthy(get(path), 200) {
			t.Errorf("%s went unaudited; the set is meant to be short", path)
		}
	}
	// A refusal and a write on the polled path are still worthy.
	if !auditWorthy(get("/api/v1/home/summary"), 403) {
		t.Error("a refused poll is not audited")
	}
	post, _ := http.NewRequest(http.MethodPost, "/api/v1/home/summary", nil)
	if !auditWorthy(post, 200) {
		t.Error("a POST went unaudited because its path is in the polled set")
	}
}

// The group names the front page compares against are the ones the
// renderer actually emits.
//
// The page decides two things by group name: which group carries the
// "Add something" link into the catalog, and which one says it is
// reachable over the tailnet. Both are string comparisons against names
// that live in Go (reconcile.Groups), so a rename there would silently
// take the catalog link off the page — no error, no failing test, just
// a household that can no longer find the way to ask for an app.
func TestThePageAndTheRendererAgreeOnGroupNames(t *testing.T) {
	page, ok := portal.Page("home")
	if !ok {
		t.Fatal("the front page is not served")
	}
	// The names the page compares against, read out of its own source
	// rather than restated here — a copy in this test would drift with
	// the page instead of catching it.
	decl := regexp.MustCompile(`var householdGroup = "([^"]+)", adminGroup = "([^"]+)";`).
		FindStringSubmatch(string(page))
	if decl == nil {
		t.Fatal("the page no longer declares the group names where this test can read them")
	}

	emitted := map[string]bool{}
	for _, g := range reconcile.Groups() {
		emitted[g] = true
	}
	for _, name := range []string{decl[1], decl[2]} {
		if !emitted[name] {
			t.Errorf("the page looks for group %q, which the renderer does not emit (it emits %v)",
				name, reconcile.Groups())
		}
	}
}

// The dashboard links an Owner to the Console and nobody else
// (ADR-W033 A4.1): the address is sent only to a viewer the Console's
// own door would let in.
func TestTheDashboardLinksOnlyOwnersToTheConsole(t *testing.T) {
	h := homeHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindRoute,
		Metadata: resource.Metadata{Name: "wildstone-console", Owner: "capability/console", Package: "wildstone-console", Node: "n1"},
		Route:    &resource.RouteSpec{Host: "console.home.test", Upstream: "127.0.0.1:9441", Auth: "authelia_oidc", Classification: "admin_tailscale"},
	}); err != nil {
		t.Fatal(err)
	}
	for member, role := range map[string]string{"mika": runtimedb.RoleOwner, "sandy": runtimedb.RoleMember} {
		if err := h.s.DB.RecordButlerMember(member, role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(member, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	link := func(user, groups string) string {
		res := h.member(http.MethodGet, "/api/v1/home/summary", user, groups, nil, "")
		defer res.Body.Close()
		var got homeSummaryResponse
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("summary for %s: %d %v", user, res.StatusCode, err)
		}
		return got.Console
	}
	if got := link("mika", "homepage_users,"+ConsoleGroup); got != "https://console.home.test/" {
		t.Errorf("the Owner's Console link = %q", got)
	}
	// A Member, even one somebody put in the directory group.
	if got := link("sandy", "homepage_users,"+ConsoleGroup); got != "" {
		t.Errorf("a Member was given the Console's address: %q", got)
	}
	// An Owner the proxy does not put in the group would be refused.
	if got := link("mika", "homepage_users"); got != "" {
		t.Errorf("an Owner outside %s was given the link: %q", ConsoleGroup, got)
	}
}
