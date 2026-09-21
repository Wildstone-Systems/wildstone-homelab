package domestic

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
)

// An Agent proposes; nothing changes here (ADR-W033 E2).
func TestProposingATimezoneRecordsWhoAsked(t *testing.T) {
	var got struct{ zone, reason, by string }
	p := &TimezoneProposer{Propose: func(_ context.Context, zone, reason, by string) (string, string, error) {
		got.zone, got.reason, got.by = zone, reason, by
		return "plan-9", "UTC", nil
	}}
	res, err := p.Execute(context.Background(), Call{Tool: p.Name(), Subject: "mika",
		Params: map[string]string{"zone": "America/Chicago", "reason": "we live in Chicago"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.zone != "America/Chicago" || got.by != "mika" || got.reason != "we live in Chicago" {
		t.Errorf("proposed %+v", got)
	}
	if !strings.Contains(res.Summary, "plan-9") || !strings.Contains(res.Summary, "until an Owner approves") {
		t.Errorf("summary = %q", res.Summary)
	}
	for _, bad := range []string{"", "Local", "Chicago", "../etc/passwd"} {
		if _, err := p.Execute(context.Background(), Call{Tool: p.Name(), Params: map[string]string{"zone": bad}}); err == nil {
			t.Errorf("zone %q was accepted", bad)
		}
	}
}

// The calendar reads a time with no zone in the household's LIVE zone,
// so an approved change applies to the next event without a restart.
func TestTheCalendarFollowsTheLiveHouseholdZone(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata")
	}
	zone := householdtz.New(time.UTC)
	c := &Calendar{Authority: permittedExecution{}, Zone: zone}
	at, err := c.moment("2026-09-20T15:00", "start")
	if err != nil || at.UTC().Hour() != 15 {
		t.Fatalf("in UTC: %v %v", at, err)
	}
	zone.Set(chicago)
	at, err = c.moment("2026-09-20T15:00", "start")
	if err != nil || at.UTC().Hour() != 20 {
		t.Fatalf("after the household moved to Chicago, 3pm = %v UTC (%v), want 20:00", at.UTC(), err)
	}
}
