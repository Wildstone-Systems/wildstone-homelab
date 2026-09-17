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
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

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
	h.s.TrustedIngress = nil // an unconfigured trust boundary must fail closed
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
	if got.Calendar.Problem != "Can't reach the calendars right now" {
		t.Errorf("problem = %q, want the unreachable copy — the household HAS a calendar server", got.Calendar.Problem)
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
