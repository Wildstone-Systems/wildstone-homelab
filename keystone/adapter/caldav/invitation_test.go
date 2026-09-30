package caldav

import (
	"strings"
	"testing"
)

func TestParseInvitationKeepsTimesUnresolved(t *testing.T) {
	inv, err := ParseInvitation(strings.Join([]string{
		"BEGIN:VCALENDAR", "METHOD:request", "BEGIN:VTIMEZONE", "TZID:Custom", "BEGIN:STANDARD",
		"DTSTART:19701101T020000", "END:STANDARD", "END:VTIMEZONE",
		"BEGIN:VEVENT", "UID:u-1", "SEQUENCE:2", "SUMMARY:Family night\\, gym", "DTSTART;TZID=Custom:20261015T180000",
		"DURATION:PT1H30M", "RRULE:FREQ=WEEKLY", "BEGIN:VALARM", "DESCRIPTION:alarm text", "END:VALARM",
		"DESCRIPTION:Bring a", " dish", "END:VEVENT",
		"BEGIN:VEVENT", "UID:u-2", "STATUS:cancelled", "DTSTART;VALUE=DATE:20261016", "END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if inv.Method != "REQUEST" || len(inv.Events) != 2 {
		t.Fatalf("invitation: %+v", inv)
	}
	a := inv.Events[0]
	if a.UID != "u-1" || a.Sequence != 2 || a.Summary != "Family night, gym" || a.Description != "Bring adish" ||
		a.Start != (ICSTime{Value: "20261015T180000", TZID: "Custom"}) || a.Duration != "PT1H30M" || !a.Recurring {
		t.Fatalf("first event: %+v", a)
	}
	b := inv.Events[1]
	if b.Status != "CANCELLED" || !b.Start.Date || b.Start.UTC {
		t.Fatalf("second event: %+v", b)
	}
	if _, err := ParseInvitation("hello"); err == nil {
		t.Fatal("non-calendar text parsed")
	}
}
