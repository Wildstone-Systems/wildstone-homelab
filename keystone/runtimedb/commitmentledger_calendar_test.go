package runtimedb

import (
	"testing"
	"time"
)

// A booking keeps the calendar event it made, and only the latest
// standing booking of each commitment is followed.
func TestOnlyTheStandingLinkedBookingIsFollowed(t *testing.T) {
	db := open(t)
	at := time.Date(2026, 10, 14, 15, 30, 0, 0, time.UTC)
	old, err := db.RecordCommitmentBooking(CommitmentBooking{Commitment: "kai-dentist", At: at, BookedBy: "mika",
		CalendarEvent: "/cal/old.ics", BookedAt: at.Add(-72 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordCommitmentBooking(CommitmentBooking{Commitment: "kai-dentist", At: at.Add(time.Hour), BookedBy: "mika",
		CalendarEvent: "/cal/new.ics", BookedAt: at.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Linked but cancelled: not followed.
	gone, _ := db.RecordCommitmentBooking(CommitmentBooking{Commitment: "vet", At: at, BookedBy: "mika", CalendarEvent: "/cal/vet.ics"})
	if err := db.CancelCommitmentBooking(gone, "mika"); err != nil {
		t.Fatal(err)
	}
	// Standing but made no event: nothing to follow.
	if _, err := db.RecordCommitmentBooking(CommitmentBooking{Commitment: "filter", At: at, BookedBy: "mika"}); err != nil {
		t.Fatal(err)
	}

	got, err := db.LiveLinkedCommitmentBookings()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CalendarEvent != "/cal/new.ics" || got[0].Seq == old {
		t.Fatalf("followed %+v, want only kai-dentist's latest booking", got)
	}
	latest, err := db.LatestCommitmentBooking("kai-dentist")
	if err != nil || latest.CalendarEvent != "/cal/new.ics" {
		t.Errorf("latest booking = %+v, %v", latest, err)
	}
}

func TestLegacyBookingDefaultsToHouseholdAndMoveRetainsOtherLinks(t *testing.T) {
	db := open(t)
	at := time.Now().UTC().Truncate(time.Second)
	// An old writer/migrated row supplies no calendar column.
	_, err := db.sql.Exec(`INSERT INTO commitment_bookings(commitment,at,booked_at,booked_by,note,calendar_event) VALUES(?,?,?,?,?,?)`, "legacy", at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), "sam", "", "/household/event.ics")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := db.LatestCommitmentBooking("legacy")
	if err != nil || legacy.CalendarID != "household" || legacy.CalendarEvent != "/household/event.ics" {
		t.Fatal(legacy, err)
	}
	if _, err := db.RecordCommitmentBooking(CommitmentBooking{Commitment: "other", At: at, BookedBy: "sam", CalendarID: "school", CalendarEvent: "/household/event.ics"}); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveBookingCalendar("household", "/household/event.ics", "school", "/school/event.ics"); err != nil {
		t.Fatal(err)
	}
	legacy, err = db.LatestCommitmentBooking("legacy")
	if err != nil || legacy.CalendarID != "school" || legacy.CalendarEvent != "/school/event.ics" {
		t.Fatal(legacy, err)
	}
	other, err := db.LatestCommitmentBooking("other")
	if err != nil || other.CalendarEvent != "/household/event.ics" {
		t.Fatal(other, err)
	}
}
