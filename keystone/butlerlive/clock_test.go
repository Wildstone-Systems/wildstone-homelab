package butlerlive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// A member had to tell Joan "I'm in America Chicago and it's only
// 18 SEP 2026". The Butler is told instead: the household's date, time
// and zone, in the prompt, at the time of the turn.
func TestEveryButlerKnowsTheHouseholdsDateAndZone(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	// 01:30 UTC on the 19th is still the evening of the 18th in Chicago.
	bs := &Butlers{Location: chicago, Now: func() time.Time { return time.Date(2026, 9, 19, 1, 30, 0, 0, time.UTC) }}
	line := bs.clockLine()
	for _, want := range []string{"Friday 18 September 2026", "20:30", "America/Chicago", "UTC-05:00"} {
		if !strings.Contains(line, want) {
			t.Errorf("clock line %q does not contain %q", line, want)
		}
	}

	gw := butler.NewHouseholdGateway("mika", butler.HouseholdSeams{})
	sys := systemFor(context.Background(), gw, runtimedb.Butler{Name: "Joan", Directive: "Keeps the family organised."}, Turn{}, line)
	if !strings.HasPrefix(sys, butler.HouseholdSystemPrompt) {
		t.Error("the surface's prompt must stay first: it is the ceiling")
	}
	if !strings.Contains(sys, "Friday 18 September 2026") {
		t.Errorf("the prompt a Butler is given does not carry the date:\n%s", sys)
	}

	if (&Butlers{}).clockLine() != "" {
		t.Error("with no household zone configured, no clock line should be claimed")
	}
}
