package caldav

import (
	"strings"
	"testing"
	"time"
)

const workDayICS = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:w\r\nSUMMARY:Work Day\r\n" +
	"DTSTART:20260919T110000Z\r\nDTEND:20260920T000000Z\r\n" +
	"LOCATION:At work for most of the day\\, mostly. Need to take out the trash.\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// "Anything else you can tell me about it?" had no answer: the reader
// kept a title and two times, and dropped the place and the note.
func TestAnEventCarriesItsPlaceAndNotes(t *testing.T) {
	ics := strings.Replace(workDayICS, "END:VEVENT", "DESCRIPTION:Bring lunch\\nand the badge\r\nEND:VEVENT", 1)
	evs := parseICS(ics, time.UTC)
	if len(evs) != 1 {
		t.Fatalf("want one event, got %d", len(evs))
	}
	if want := "At work for most of the day, mostly. Need to take out the trash."; evs[0].Location != want {
		t.Errorf("location = %q, want %q", evs[0].Location, want)
	}
	if want := "Bring lunch\nand the badge"; evs[0].Description != want {
		t.Errorf("description = %q, want %q", evs[0].Description, want)
	}
}

// Moving a note out of the place field: the place is emptied and the
// note written. Empty REMOVES the property rather than leaving a blank
// "LOCATION:" some clients draw as an empty place.
func TestAnEmptyPlaceIsRemovedAndANoteAdded(t *testing.T) {
	empty, notes := "", "Need to take out the trash."
	out, changed := applyChanges(workDayICS, EventWrite{Location: &empty, Description: &notes},
		time.Time{}, time.Time{}, time.Date(2026, 9, 18, 22, 0, 0, 0, time.UTC))
	if !changed {
		t.Fatal("the change was reported as no change")
	}
	if strings.Contains(out, "LOCATION") {
		t.Errorf("an emptied place is still in the event:\n%s", out)
	}
	if !strings.Contains(out, "DESCRIPTION:Need to take out the trash.") {
		t.Errorf("the note was not written:\n%s", out)
	}
	if !strings.Contains(out, "DTSTART:20260919T110000Z") || !strings.Contains(out, "DTEND:20260920T000000Z") {
		t.Errorf("the times moved while only the place and note changed:\n%s", out)
	}
	if _, changed := applyChanges(strings.Replace(workDayICS, "LOCATION", "X-NOT-LOCATION", 1),
		EventWrite{Location: &empty}, time.Time{}, time.Time{}, time.Now()); changed {
		t.Error("clearing a place the event never had was reported as a change")
	}
}
