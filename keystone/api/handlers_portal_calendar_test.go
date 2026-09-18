package api

// The calendar hand-off, checked against the two things that go wrong
// quietly: a profile that points at the wrong server, and a card that
// claims to know what is on when it does not.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func route(owner, host, upstream string) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindRoute,
		Metadata: resource.Metadata{Name: host, Owner: owner},
		Route:    &resource.RouteSpec{Host: host, Upstream: upstream, Auth: "none", Classification: "family_private"},
	}
}

var davisRoute = route("capability/mail", "calendar.example.internal", "container/davis:9000")
var ourRoute = route("capability/calendar", "cal.example.internal", "container/radicale:5232")

// A household may run BOTH. Taking whichever Route came first in desired
// state meant a profile built for the Wildstone calendar could carry
// davis's address — an account that installs cleanly, fails to connect,
// and blames a password the person does not have.
func TestOurCalendarWinsWhenAHouseholdRunsBoth(t *testing.T) {
	for _, order := range [][]resource.Resource{
		{davisRoute, ourRoute},
		{ourRoute, davisRoute},
	} {
		got, found := calendarServer(order)
		if !found {
			t.Fatal("no calendar server found")
		}
		if !strings.Contains(got, "cal.example.internal") {
			t.Errorf("with davis listed %v, the address resolved to %q — a profile would point at the "+
				"wrong server", order[0].Metadata.Owner, got)
		}
	}

	// And with only davis, the card must NOT claim the household has
	// ours: davis needs a mail password this dashboard does not hold.
	if hasWildstoneCalendar([]resource.Resource{davisRoute}) {
		t.Error("a davis-only household was treated as running the Wildstone calendar")
	}
	if !hasWildstoneCalendar([]resource.Resource{davisRoute, ourRoute}) {
		t.Error("a household running ours was not recognised")
	}
}

// The profile is built for the signed-in member, and the household
// principal comes from the collection Keystone was pointed at.
func TestTheSetupIsDerivedFromTheCollectionKeystoneWasGiven(t *testing.T) {
	s := &Server{CalendarCollectionURL: "http://radicale:5232/GROUPS/Y2FsZW5kYXJfdXNlcnM=/"}
	setup, ok := s.calendarSetupFor("mika", []resource.Resource{ourRoute})
	if !ok {
		t.Fatal("no setup derived")
	}
	if setup.OwnPrincipal != "/mika/" {
		t.Errorf("own principal = %q", setup.OwnPrincipal)
	}
	// The WHOLE path, not its parent: the shared calendar is a group
	// collection with no principal above it, and this address is for
	// saying rather than for mounting.
	if setup.HouseholdCalendar != "/GROUPS/Y2FsZW5kYXJfdXNlcnM=/" {
		t.Errorf("household calendar = %q, want the collection Keystone was pointed at", setup.HouseholdCalendar)
	}
	// The internal address must never reach a phone.
	if strings.Contains(setup.Origin, "radicale:5232") {
		t.Errorf("the container address reached the setup: %q", setup.Origin)
	}

	// No collection configured: the member still gets their own.
	bare := &Server{}
	setup, ok = bare.calendarSetupFor("mika", []resource.Resource{ourRoute})
	if !ok || setup.HouseholdCalendar != "" {
		t.Errorf("with no collection configured, household calendar = %q", setup.HouseholdCalendar)
	}
}

// "Nothing in the next three days" and "I could not look" are different
// answers, and a wall tablet must not render the second as the first.
func TestTheCardNeverClaimsNothingWhenItCouldNotLook(t *testing.T) {
	desired := []resource.Resource{ourRoute}
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)

	// Unreachable.
	broken := &Server{CalendarCollectionURL: "http://radicale:5232/GROUPS/Y2FsZW5kYXJfdXNlcnM=/"}
	broken.CalendarEvents = func(time.Time, time.Time) ([]CalendarCardEvent, error) {
		return nil, errors.New("connection refused")
	}
	card := broken.homeCalendar("mika", desired, now)
	if card.Available {
		t.Error("a calendar that could not be read rendered as available, which the page draws as " +
			"\"Nothing in the next three days\"")
	}
	if card.Problem == "" {
		t.Error("an unreadable calendar gave no reason")
	}

	// Genuinely empty: available, an empty (non-nil) list, and the
	// hand-off still offered.
	empty := &Server{CalendarCollectionURL: "http://radicale:5232/GROUPS/Y2FsZW5kYXJfdXNlcnM=/"}
	empty.CalendarEvents = func(time.Time, time.Time) ([]CalendarCardEvent, error) {
		return nil, nil
	}
	card = empty.homeCalendar("mika", desired, now)
	if !card.Available || card.Events == nil {
		t.Errorf("an empty calendar did not render as an empty answer: %+v", card)
	}
	if card.AddToPhone == "" {
		t.Error("an empty calendar is still one you want on your phone, and the card offered no way")
	}

	// With events.
	full := &Server{CalendarCollectionURL: "http://radicale:5232/GROUPS/Y2FsZW5kYXJfdXNlcnM=/"}
	full.CalendarEvents = func(time.Time, time.Time) ([]CalendarCardEvent, error) {
		return []CalendarCardEvent{{Title: "Dinner with Sam", Start: now.Add(10 * time.Hour)}}, nil
	}
	card = full.homeCalendar("mika", desired, now)
	if len(card.Events) != 1 || card.Events[0].Title != "Dinner with Sam" {
		t.Errorf("the card did not carry the event: %+v", card.Events)
	}
}

// A davis household keeps the answer docs/design/household-calendar.md
// gives: those calendars need a mail password this dashboard does not
// hold, and offering a profile for one would produce an account that
// cannot connect.
func TestADavisHouseholdIsNotOfferedAProfile(t *testing.T) {
	s := &Server{}
	card := s.homeCalendar("mika", []resource.Resource{davisRoute}, time.Now())
	if card.AddToPhone != "" {
		t.Error("a davis household was offered a Wildstone configuration profile")
	}
	if card.Problem == "" {
		t.Error("a davis household was given no explanation")
	}
}
