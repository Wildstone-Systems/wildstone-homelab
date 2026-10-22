package api

// A booking and the calendar event it makes.
//
// "Booked it" used to record a date in the commitment ledger and stop
// there: the appointment the household was tracking never reached the
// calendar the household actually reads, and moving or cancelling it on
// the calendar changed nothing here. Now a booking makes the event on
// the shared household calendar and keeps its address, and
// SyncCalendarBookings follows it — moved on the calendar, the booking
// moves; deleted there, the booking is cancelled and the commitment
// goes back to asking. The calendar is where people change appointments,
// so it is the one that wins.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/caldav"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/upkeep"
)

// calendarActor is who a booking change the calendar caused is recorded
// against. Not a member: nobody on this page did it.
const calendarActor = "calendar"

// defaultAppointmentMinutes is how long a timed appointment is when the
// member did not say.
const defaultAppointmentMinutes = 60

// bookCommitment records an appointment, puts it on the household
// calendar when there is one, and retires the booking it replaces.
//
// The booking is recorded even when the calendar refuses the event: the
// member said an appointment exists, and that is true whether or not a
// calendar server was reachable. The answer says so, so the page can
// tell them it is not on the calendar.
func (s *Server) bookCommitment(ctx context.Context, st upkeep.Standing, member string, at time.Time, req commitmentAnswer) (any, int, error) {
	s.calendarBookingsMu.Lock()
	defer s.calendarBookingsMu.Unlock()
	prev, prevErr := s.DB.LatestCommitmentBooking(st.Name)

	out := map[string]any{}
	href := ""
	calendarID := "household"
	if s.CalendarBook != nil && s.CalendarCollectionURL != "" {
		calendar, book, _, err := s.bookingCalendar("")
		if err == nil && s.CalendarBookFor != nil {
			_, _, err = s.CalendarForActor(member, calendar.ID, true)
		}
		if err != nil {
			return nil, http.StatusForbidden, err
		}
		calendarID = calendar.ID
		ans, err := book.CreateEvent(ctx, bookingEvent(st, at, req, s.calendarLoc()))
		if err != nil {
			out["calendarProblem"] = "Booked — but it couldn't be added to the household calendar: " + err.Error()
		} else if data, ok := ans.Data.(map[string]any); ok {
			href, _ = data["href"].(string)
		}
	}
	seq, err := s.DB.RecordCommitmentBooking(runtimedb.CommitmentBooking{
		Commitment: st.Name, At: at, BookedBy: member, Note: req.Note, CalendarEvent: href, CalendarID: calendarID,
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out["seq"] = seq
	if href != "" {
		out["calendarEvent"] = href
		out["calendar"] = calendarID
	}

	// Booking again replaces the appointment rather than adding a second
	// one: the old booking is cancelled, and its event comes off the
	// calendar so the family does not see the dentist twice.
	if prevErr == nil && prev.Live() {
		if err := s.DB.CancelCommitmentBooking(prev.Seq, member); err != nil {
			s.Log.Warn("rebooking: cancelling the previous booking", "commitment", st.Name, "err", err)
		}
		if prev.CalendarEvent != "" && s.CalendarBook != nil {
			calendar, book, _, err := s.bookingCalendar(prev.CalendarID)
			if err == nil && s.CalendarBookFor != nil {
				_, _, err = s.CalendarForActor(member, calendar.ID, true)
			}
			if err == nil {
				_, err = book.DeleteEvent(ctx, caldav.EventWrite{Href: prev.CalendarEvent})
			}
			if err != nil {
				s.Log.Warn("rebooking: removing the previous calendar event", "commitment", st.Name, "err", err)
			}
		}
	}
	s.pokeUpkeep()
	return out, http.StatusCreated, nil
}

// bookingEvent is the calendar event for one appointment.
//
// A day with no time is an all-day event on that day in the household's
// zone; a time is an appointment of req.Minutes. The title is the
// commitment's own words, with who it is for in front when the words do
// not already say — "Kai: Dentist" reads on a shared calendar, "Dentist"
// does not.
func bookingEvent(st upkeep.Standing, at time.Time, req commitmentAnswer, loc *time.Location) caldav.EventWrite {
	summary := strings.TrimSpace(st.Description)
	if who := strings.TrimPrefix(st.Owner, "household/"); who != st.Owner && who != "" &&
		!strings.Contains(strings.ToLower(summary), strings.ToLower(who)) {
		summary = strings.ToUpper(who[:1]) + who[1:] + ": " + summary
	}
	note := "Booked from Coming up. Move or delete it here and Wildstone keeps the reminder in step."
	w := caldav.EventWrite{Summary: &summary, Description: &note}
	if req.AllDay {
		day := at.In(loc)
		w.AllDay = true
		w.Start = day.Format("2006-01-02")
		w.End = day.AddDate(0, 0, 1).Format("2006-01-02")
		return w
	}
	minutes := req.Minutes
	if minutes <= 0 {
		minutes = defaultAppointmentMinutes
	}
	w.Start = at.UTC().Format(time.RFC3339)
	w.End = at.Add(time.Duration(minutes) * time.Minute).UTC().Format(time.RFC3339)
	return w
}

// SyncCalendarBookings brings each standing booking into line with the
// calendar event it made, and reports how many it changed.
//
// A deleted event cancels its booking. A moved one cancels the booking
// and records a new one at the new time, linked to the same event, so
// the ledger keeps both stories. A calendar that cannot be read changes
// nothing: an outage is not a cancellation.
func (s *Server) SyncCalendarBookings(ctx context.Context) (int, error) {
	s.calendarBookingsMu.Lock()
	defer s.calendarBookingsMu.Unlock()
	if s.CalendarBook == nil || s.DB == nil {
		return 0, nil
	}
	bookings, err := s.DB.LiveLinkedCommitmentBookings()
	if err != nil {
		return 0, err
	}
	loc := s.calendarLoc()
	changed := 0
	for _, b := range bookings {
		_, book, _, err := s.bookingCalendar(b.CalendarID)
		if err != nil {
			continue
		}
		ev, err := book.EventAt(ctx, b.CalendarEvent)
		if err != nil {
			s.Log.Warn("calendar bookings: reading an event", "commitment", b.Commitment, "err", err)
			continue
		}
		if ev == nil {
			if err := s.DB.CancelCommitmentBooking(b.Seq, calendarActor); err != nil {
				return changed, fmt.Errorf("cancelling %s's booking: %w", b.Commitment, err)
			}
			s.Log.Info("calendar bookings: event deleted, booking cancelled", "commitment", b.Commitment)
			changed++
			continue
		}
		when := appointmentTime(*ev, loc)
		if when.Truncate(time.Minute).Equal(b.At.Truncate(time.Minute)) {
			continue
		}
		if err := s.DB.CancelCommitmentBooking(b.Seq, calendarActor); err != nil {
			return changed, fmt.Errorf("moving %s's booking: %w", b.Commitment, err)
		}
		if _, err := s.DB.RecordCommitmentBooking(runtimedb.CommitmentBooking{
			Commitment: b.Commitment, At: when, BookedBy: calendarActor,
			Note: "moved on the calendar", CalendarEvent: b.CalendarEvent, CalendarID: b.CalendarID,
		}); err != nil {
			return changed, fmt.Errorf("moving %s's booking: %w", b.Commitment, err)
		}
		s.Log.Info("calendar bookings: event moved", "commitment", b.Commitment, "from", b.At, "to", when)
		changed++
	}
	if changed > 0 {
		s.pokeUpkeep()
	}
	return changed, nil
}

// appointmentTime is when a booking says its appointment is, for an
// event. A timed event is its start. An all-day event is the END of its
// day where the household lives, which is what the home page records
// for a booking with no time: an appointment named only by its day has
// not been missed until that day is over.
func appointmentTime(ev caldav.Event, loc *time.Location) time.Time {
	if !ev.AllDay {
		return ev.Start
	}
	y, m, d := ev.Start.In(loc).Date()
	if ev.Start.Location() == time.UTC && ev.Start.Hour() == 0 && ev.Start.Minute() == 0 {
		// A DATE value parsed without a zone is that calendar day, not
		// that instant.
		y, m, d = ev.Start.Date()
	}
	return time.Date(y, m, d, 23, 59, 59, 0, loc)
}
