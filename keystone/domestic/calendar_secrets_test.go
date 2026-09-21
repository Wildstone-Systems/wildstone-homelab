package domestic

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCalendarSecretUseDoesNotFallBackOrEcho(t *testing.T) {
	value := "generated-calendar-credential"
	f := newFakeCalendar(t, vevent("event", value, "20260918T120000Z", "20260918T123000Z"))
	cal := calendarUnder(f)
	cal.SecretUse = func(_ context.Context, c Call, consume func(string) error) (bool, error) { return true, consume(value) }
	result, err := run(t, cal, CalToolAgenda, map[string]string{"date": "2026-09-18"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.requests) == 0 {
		t.Fatal("no credential-consuming request")
	}
	_, got, _ := f.requests[0].BasicAuth()
	if got != value {
		t.Fatal("credential not resolved below tool")
	}
	if strings.Contains(result.Summary, value) {
		t.Fatal("upstream reflected raw credential")
	}
	if cal.Upstream.Password == value {
		t.Fatal("shared client retained credential")
	}
	before := len(f.requests)
	cal.SecretUse = func(context.Context, Call, func(string) error) (bool, error) { return true, errors.New("denied") }
	if _, err = run(t, cal, CalToolAgenda, map[string]string{}); err == nil {
		t.Fatal("denied binding fell back")
	}
	if len(f.requests) != before {
		t.Fatal("denied call reached upstream")
	}
}
