package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
)

// bookingHarness is Kai's overdue checkup, answerable by mika, on a
// household that has a shared calendar.
func bookingHarness(t *testing.T) (*harness, *fakeBook) {
	t.Helper()
	h := commitmentHarness(t)
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	book := &fakeBook{}
	h.s.HouseholdLoc = loc
	h.s.CalendarCollectionURL = testCollection
	h.s.CalendarBook = book
	overdueCheckup(t, h, []string{"household/mika"}, "")
	return h, book
}

func latestBooking(t *testing.T, h *harness) runtimedb.CommitmentBooking {
	t.Helper()
	b, err := h.s.DB.LatestCommitmentBooking("kai-checkup")
	if err != nil {
		t.Fatalf("latest booking: %v", err)
	}
	return b
}

// "Booked it" with a time puts an hour-long appointment on the household
// calendar and keeps its address.
func TestBookingPutsTheAppointmentOnTheCalendar(t *testing.T) {
	h, book := bookingHarness(t)
	when := time.Now().AddDate(0, 0, 14).Truncate(time.Minute).UTC()
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/kai-checkup/booked", "mika",
		map[string]any{"at": when}); got != http.StatusCreated {
		t.Fatalf("booking: status %d", got)
	}
	if len(book.writes) != 1 {
		t.Fatalf("calendar writes = %d, want 1", len(book.writes))
	}
	w := book.writes[0]
	if w.AllDay || w.Start != when.Format(time.RFC3339) || w.End != when.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("event = %+v, want a one-hour appointment at %s", w, when)
	}
	// The description already names Kai, so the title does not repeat it.
	if w.Summary == nil || *w.Summary != "Kai's annual well-child visit" {
		t.Errorf("title = %v", w.Summary)
	}
	b := latestBooking(t, h)
	if b.CalendarEvent == "" || !b.At.Equal(when) {
		t.Errorf("booking = %+v, want linked to the event at %s", b, when)
	}
}

// A day with no time is an all-day event on that day where the household
// lives, not on whatever day it is in UTC.
func TestADayWithNoTimeIsAnAllDayEvent(t *testing.T) {
	h, book := bookingHarness(t)
	// 23:59:59 local on the 14th, as the page sends it, is the 15th in UTC.
	day := time.Now().AddDate(0, 0, 14)
	endOfDay := time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, h.s.HouseholdLoc)
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/kai-checkup/booked", "mika",
		map[string]any{"at": endOfDay, "allDay": true}); got != http.StatusCreated {
		t.Fatalf("booking: status %d", got)
	}
	w := book.writes[0]
	if !w.AllDay || w.Start != endOfDay.Format("2006-01-02") || w.End != endOfDay.AddDate(0, 0, 1).Format("2006-01-02") {
		t.Errorf("event = %+v, want all day on %s", w, endOfDay.Format("2006-01-02"))
	}
}

// Booking again replaces the appointment: the old booking is cancelled
// and its event leaves the calendar, so the family sees one dentist.
func TestRebookingReplacesTheEvent(t *testing.T) {
	h, book := bookingHarness(t)
	first := time.Now().AddDate(0, 0, 14).Truncate(time.Minute)
	h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/kai-checkup/booked", "mika", map[string]any{"at": first})
	old := latestBooking(t, h)
	h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/kai-checkup/booked", "mika",
		map[string]any{"at": first.AddDate(0, 0, 7)})
	now := latestBooking(t, h)
	if now.Seq == old.Seq || now.CalendarEvent == old.CalendarEvent {
		t.Fatalf("rebooking did not make a new linked booking: %+v", now)
	}
	if len(book.deletes) != 1 || book.deletes[0] != old.CalendarEvent {
		t.Errorf("deleted %v, want the old event %s", book.deletes, old.CalendarEvent)
	}
	linked, _ := h.s.DB.LiveLinkedCommitmentBookings()
	if len(linked) != 1 || linked[0].Seq != now.Seq {
		t.Errorf("following %+v, want only the new booking", linked)
	}
}

// The calendar wins: an appointment moved there moves the booking, one
// deleted there cancels it and the checkup asks again, and a calendar
// that cannot be read changes nothing.
func TestTheBookingFollowsItsCalendarEvent(t *testing.T) {
	h, book := bookingHarness(t)
	when := time.Now().AddDate(0, 0, 14).Truncate(time.Minute).UTC()
	h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/kai-checkup/booked", "mika", map[string]any{"at": when})
	b := latestBooking(t, h)

	// Untouched: nothing to do.
	if n, err := h.s.SyncCalendarBookings(context.Background()); err != nil || n != 0 {
		t.Fatalf("sync of an unchanged event = %d, %v", n, err)
	}

	// Moved two days later on somebody's phone.
	moved := when.AddDate(0, 0, 2)
	ev := book.stored[b.CalendarEvent]
	ev.Start = moved
	book.stored[b.CalendarEvent] = ev
	if n, err := h.s.SyncCalendarBookings(context.Background()); err != nil || n != 1 {
		t.Fatalf("sync of a moved event = %d, %v", n, err)
	}
	now := latestBooking(t, h)
	if !now.At.Equal(moved) || now.BookedBy != calendarActor || now.CalendarEvent != b.CalendarEvent || !now.Live() {
		t.Errorf("after the move the booking is %+v, want %s from the calendar", now, moved)
	}

	// An outage is not a cancellation.
	book.readErr = errors.New("radicale is down")
	if n, _ := h.s.SyncCalendarBookings(context.Background()); n != 0 || !latestBooking(t, h).Live() {
		t.Error("an unreachable calendar cancelled the booking")
	}
	book.readErr = nil

	// Deleted: the booking is cancelled, and the checkup asks again.
	delete(book.stored, b.CalendarEvent)
	if n, err := h.s.SyncCalendarBookings(context.Background()); err != nil || n != 1 {
		t.Fatalf("sync of a deleted event = %d, %v", n, err)
	}
	if latestBooking(t, h).Live() {
		t.Error("the booking stands after its event was deleted")
	}
	if _, ok := commitmentItem(h.needsYou(t, "mika"), "kai-checkup"); !ok {
		t.Error("the checkup is not asking again after its appointment was cancelled")
	}
}

// With no calendar, booking still works — it is a date, as before.
func TestBookingWithNoCalendarIsStillABooking(t *testing.T) {
	h := commitmentHarness(t)
	overdueCheckup(t, h, []string{"household/mika"}, "")
	if got := h.memberJSON(t, http.MethodPost, "/api/v1/home/commitments/kai-checkup/booked", "mika",
		map[string]any{"at": time.Now().AddDate(0, 0, 3)}); got != http.StatusCreated {
		t.Fatalf("booking: status %d", got)
	}
	if b := latestBooking(t, h); b.CalendarEvent != "" || !b.Live() {
		t.Errorf("booking = %+v, want a standing booking with no event", b)
	}
}

// A commitment about somebody who is not in its title is titled with
// their name on the shared calendar.
func TestTheEventSaysWhoTheAppointmentIsFor(t *testing.T) {
	st := struct{ desc, owner, want string }{"Dentist", "household/kai", "Kai: Dentist"}
	w := bookingEvent(standingFor(st.desc, st.owner), time.Now(), commitmentAnswer{}, time.UTC)
	if w.Summary == nil || *w.Summary != st.want {
		t.Errorf("title = %v, want %q", w.Summary, st.want)
	}
	w = bookingEvent(standingFor("Bins", "household"), time.Now(), commitmentAnswer{}, time.UTC)
	if *w.Summary != "Bins" {
		t.Errorf("a household commitment was titled %q", *w.Summary)
	}
}

// The form's "Who is it for?" suggestions are the household's current
// people, and choosing one makes the commitment theirs.
func TestTheTrackerOffersThePeopleACommitmentCanBeAbout(t *testing.T) {
	h := commitmentHarness(t)
	h.person("mika", "kai")
	if got := h.upkeep(t, "mika").People; len(got) != 2 || got[0] != "kai" || got[1] != "mika" {
		t.Errorf("people = %v, want [kai mika]", got)
	}
	if code, out := h.authored(t, http.MethodPost, "/api/v1/home/commitments", "mika", map[string]any{
		"description": "Dentist", "about": "kai", "every": "6mo", "rollover": resource.RolloverFromSchedule,
		"anchor": "2026-04-01", "requiresBooking": true,
	}); code != http.StatusCreated {
		t.Fatalf("status %d: %+v", code, out)
	}
	current, _ := h.s.State.Load()
	for _, r := range current {
		if r.Kind == resource.KindCommitment && r.Commitment.Description == "Dentist" {
			if r.Metadata.Owner != "household/kai" || !r.Commitment.RequiresBooking {
				t.Errorf("dentist = owner %q, requiresBooking %v", r.Metadata.Owner, r.Commitment.RequiresBooking)
			}
			return
		}
	}
	t.Fatal("the dentist was not declared")
}

func standingFor(desc, owner string) upkeep.Standing {
	return upkeep.Standing{Name: "x", Description: desc, Owner: owner}
}
