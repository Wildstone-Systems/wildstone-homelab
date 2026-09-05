package runtimedb

import (
	"encoding/json"
	"path/filepath"
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

// The 0005 migration rebuilds the principals table to admit the publisher
// role. Rows and the tokens foreign key must survive the rebuild.
func TestMigration0005PreservesPrincipalsAndTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := openUnmigrated(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.migrateThrough("migrations/0004_nodes.sql"); err != nil {
		t.Fatalf("migrating through 0004: %v", err)
	}
	if err := db.CreatePrincipal("mika", RoleAdmin, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertToken("hash-1", "mika", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	// The old CHECK refuses the new role — that is what 0005 fixes.
	if err := db.CreatePrincipal("publisher-home", RolePublisher, ""); err == nil {
		t.Fatal("pre-0005 schema accepted role publisher; the migration under test is moot")
	}
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies 0005): %v", err)
	}
	defer db2.Close()
	p, err := db2.GetPrincipal("mika")
	if err != nil || p.Role != RoleAdmin {
		t.Fatalf("principal after rebuild: %+v err=%v", p, err)
	}
	tok, err := db2.GetToken("hash-1")
	if err != nil || tok.Principal != "mika" {
		t.Fatalf("token after rebuild: %+v err=%v", tok, err)
	}
	if err := db2.CreatePrincipal("publisher-home", RolePublisher, ""); err != nil {
		t.Fatalf("publisher role after 0005: %v", err)
	}
	// The foreign key still binds tokens to principals.
	if err := db2.InsertToken("hash-2", "nobody", "", time.Time{}); err == nil {
		t.Fatal("token for a non-existent principal was accepted: the FK did not survive the rebuild")
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
