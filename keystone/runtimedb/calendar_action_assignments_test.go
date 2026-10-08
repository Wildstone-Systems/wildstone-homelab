package runtimedb

import "testing"

var calendarActionTools = []string{
	"calendar.how_to_add", "calendar.get_agenda", "calendar.search_events",
	"calendar.check_availability", "calendar.create_event", "calendar.update_event",
	"calendar.delete_event",
}

// ADR-W034 A5: a governed Agent that reached calendar actions through
// do_household_action keeps every one of them once calendar actions are
// granted per action, and an Agent that never held that tool gains none.
func TestCalendarActionMigrationKeepsTodaysReach(t *testing.T) {
	db, alfred := governedFixture(t)
	if err := db.AssignTool(alfred, "do_household_action", "mika"); err != nil {
		t.Fatal(err)
	}
	// A second governed Agent that never held do_household_action.
	hobson, err := db.GetButler("hobson")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GovernAgent(hobson.Seq, "mika"); err != nil {
		t.Fatal(err)
	}

	body, err := migrationFS.ReadFile("migrations/0104_calendar_action_assignments.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Twice: the migration must be idempotent against the live unique index.
	for i := 0; i < 2; i++ {
		if _, err := db.sql.Exec(string(body)); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	for _, tool := range calendarActionTools {
		if got := ask(t, db, "calendar", "member", tool, false); !got.Allowed {
			t.Errorf("%s was lost by the migration: %+v", tool, got)
		}
		r, err := db.ResolveButlerTool("hobson", ToolQuery{Tool: tool, Structural: true, Service: "calendar", ToolClass: "member"})
		if err != nil {
			t.Fatal(err)
		}
		if r.Allowed {
			t.Errorf("%s was granted to an Agent that never held do_household_action", tool)
		}
	}
	if got := ask(t, db, "calendar", "member", "calendar.future_action", false); got.Allowed {
		t.Error("the migration granted an action it did not name")
	}
}
