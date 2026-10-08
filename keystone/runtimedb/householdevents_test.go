package runtimedb

import (
	"encoding/json"
	"testing"
	"time"
)

func TestHouseholdEventsAppendDedupeAndCursor(t *testing.T) {
	db := open(t)
	at := time.Date(2026, 9, 5, 7, 30, 0, 0, time.UTC)
	seq1, dup, err := db.AppendHouseholdEvent(HouseholdEvent{
		EventID: "ha-1", Source: "publisher-home", Type: "person.left_home",
		OccurredAt: at, Subject: "mika", Payload: json.RawMessage(`{"zone":"home"}`),
	})
	if err != nil || dup {
		t.Fatalf("first append: seq=%d dup=%v err=%v", seq1, dup, err)
	}
	// Same publisher, same id: no new row, the original seq comes back.
	seq1b, dup, err := db.AppendHouseholdEvent(HouseholdEvent{
		EventID: "ha-1", Source: "publisher-home", Type: "person.left_home", OccurredAt: at,
	})
	if err != nil || !dup || seq1b != seq1 {
		t.Fatalf("retry: seq=%d dup=%v err=%v (want seq %d, dup)", seq1b, dup, err, seq1)
	}
	// A different publisher with the same id is a different event.
	seq2, dup, err := db.AppendHouseholdEvent(HouseholdEvent{
		EventID: "ha-1", Source: "publisher-pantry", Type: "pantry.meal_logged", OccurredAt: at.Add(time.Minute),
	})
	if err != nil || dup || seq2 <= seq1 {
		t.Fatalf("other source: seq=%d dup=%v err=%v", seq2, dup, err)
	}

	got, err := db.HouseholdEventsAfter(0, 0)
	if err != nil {
		t.Fatalf("HouseholdEventsAfter: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (the retry must not have written)", len(got))
	}
	e := got[0]
	if e.Seq != seq1 || e.Source != "publisher-home" || e.Subject != "mika" || string(e.Payload) != `{"zone":"home"}` ||
		!e.OccurredAt.Equal(at) || e.RecordedAt.IsZero() || e.SchemaVersion != 1 {
		t.Fatalf("row = %+v", e)
	}
	if got[1].Payload != nil {
		t.Fatalf("empty payload should read back nil, got %q", got[1].Payload)
	}
	after, err := db.HouseholdEventsAfter(seq1, 0)
	if err != nil || len(after) != 1 || after[0].Seq != seq2 {
		t.Fatalf("cursor after %d: %+v err=%v", seq1, after, err)
	}
	since, err := db.HouseholdEventsSince(at.Add(30*time.Second), 0)
	if err != nil || len(since) != 1 || since[0].Seq != seq2 {
		t.Fatalf("since: %+v err=%v", since, err)
	}
}

func TestHouseholdEventRequiredColumns(t *testing.T) {
	db := open(t)
	base := HouseholdEvent{EventID: "x", Source: "s", Type: "a.b", OccurredAt: time.Now()}
	for name, mut := range map[string]func(*HouseholdEvent){
		"no id":       func(e *HouseholdEvent) { e.EventID = "" },
		"no source":   func(e *HouseholdEvent) { e.Source = "" },
		"no type":     func(e *HouseholdEvent) { e.Type = "" },
		"no time":     func(e *HouseholdEvent) { e.OccurredAt = time.Time{} },
		"bad payload": func(e *HouseholdEvent) { e.Payload = json.RawMessage(`{not json`) },
	} {
		e := base
		mut(&e)
		if _, _, err := db.AppendHouseholdEvent(e); err == nil {
			t.Errorf("%s: appended, want error", name)
		}
	}
}

func TestRevokeTokensFor(t *testing.T) {
	db := open(t)
	if err := db.CreatePrincipal("publisher-home", RolePublisher, ""); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"h1", "h2"} {
		if err := db.InsertToken(h, "publisher-home", "", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := db.RevokeTokensFor("publisher-home")
	if err != nil || n != 2 {
		t.Fatalf("revoked %d, err=%v; want 2", n, err)
	}
	tok, err := db.GetToken("h1")
	if err != nil || tok.RevokedAt.IsZero() {
		t.Fatalf("h1 after revoke: %+v err=%v", tok, err)
	}
	if n, _ := db.RevokeTokensFor("publisher-home"); n != 0 {
		t.Fatalf("second revoke touched %d rows, want 0", n)
	}
}

func TestQueryHouseholdEventsAndCursor(t *testing.T) {
	db := open(t)
	base := time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)
	put := func(id, typ, subject, entity string, at time.Time) {
		t.Helper()
		if _, _, err := db.AppendHouseholdEvent(HouseholdEvent{EventID: id, Source: "s", Type: typ, Subject: subject, Entity: entity, OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	put("1", "pantry.meal_logged", "mika", "", base)
	put("2", "pantry.meal_logged", "sam", "", base.Add(time.Hour))
	put("3", "person.left_home", "mika", "", base.Add(2*time.Hour))
	put("4", "automation.fired", "mika", "breakfast-check", base.Add(2*time.Hour))
	put("5", "pantry.meal_logged", "", "", base.Add(3*time.Hour))

	got, err := db.QueryHouseholdEvents(HouseholdQuery{Type: "pantry.meal_logged"})
	if err != nil || len(got) != 3 {
		t.Fatalf("by type: %d, %v", len(got), err)
	}
	got, _ = db.QueryHouseholdEvents(HouseholdQuery{Type: "pantry.meal_logged", Subject: "mika", SubjectSet: true})
	if len(got) != 1 || got[0].EventID != "1" {
		t.Fatalf("by subject: %+v", got)
	}
	// SubjectSet with "" is "no subject", not "any subject".
	got, _ = db.QueryHouseholdEvents(HouseholdQuery{Type: "pantry.meal_logged", SubjectSet: true})
	if len(got) != 1 || got[0].EventID != "5" {
		t.Fatalf("empty subject: %+v", got)
	}
	got, _ = db.QueryHouseholdEvents(HouseholdQuery{From: base.Add(30 * time.Minute), To: base.Add(2 * time.Hour)})
	if len(got) != 3 || got[0].EventID != "2" || got[2].EventID != "4" {
		t.Fatalf("window: %+v", got)
	}
	got, _ = db.QueryHouseholdEvents(HouseholdQuery{Type: "automation.fired", Entity: "breakfast-check", Newest: true, Limit: 1})
	if len(got) != 1 || got[0].EventID != "4" {
		t.Fatalf("by entity newest: %+v", got)
	}
	got, _ = db.QueryHouseholdEvents(HouseholdQuery{Newest: true, Limit: 2})
	if len(got) != 2 || got[0].EventID != "5" || got[1].EventID != "4" {
		t.Fatalf("newest two: %+v", got)
	}

	if seq, err := db.AutomationCursor(); err != nil || seq != 0 {
		t.Fatalf("initial cursor = %d, %v", seq, err)
	}
	if err := db.SetAutomationCursor(3); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAutomationCursor(5); err != nil {
		t.Fatal(err)
	}
	if seq, _ := db.AutomationCursor(); seq != 5 {
		t.Fatalf("cursor = %d, want 5", seq)
	}
}
