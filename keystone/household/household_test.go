package household

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

var now = time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC)

func good() Envelope {
	return Envelope{
		EventID: "01J6ZK8Q2M7Y4N3R5T9V1W0X2A", OccurredAt: now.Add(-time.Minute),
		Subject: "mika", Type: "person.left_home", Payload: json.RawMessage(`{"zone":"home"}`),
	}
}

func TestValidateAccepts(t *testing.T) {
	e := good()
	if err := e.Validate(now); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if e.SchemaVersion != 1 {
		t.Fatalf("schemaVersion defaulted to %d, want 1", e.SchemaVersion)
	}
	for _, typ := range []string{"pantry.meal_logged", "calendar.school.closed", "a1.b_2"} {
		e := good()
		e.Type = typ
		if err := e.Validate(now); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		mut   func(*Envelope)
		field string
	}{
		"no id":          {func(e *Envelope) { e.EventID = "" }, "eventId"},
		"long id":        {func(e *Envelope) { e.EventID = strings.Repeat("a", MaxEventIDBytes+1) }, "eventId"},
		"control in id":  {func(e *Envelope) { e.EventID = "a\nb" }, "eventId"},
		"no time":        {func(e *Envelope) { e.OccurredAt = time.Time{} }, "occurredAt"},
		"far future":     {func(e *Envelope) { e.OccurredAt = now.Add(time.Hour) }, "occurredAt"},
		"no type":        {func(e *Envelope) { e.Type = "" }, "type"},
		"flat type":      {func(e *Envelope) { e.Type = "left_home" }, "type"},
		"upper type":     {func(e *Envelope) { e.Type = "Person.Left" }, "type"},
		"long subject":   {func(e *Envelope) { e.Subject = strings.Repeat("s", MaxFieldBytes+1) }, "subject"},
		"control entity": {func(e *Envelope) { e.Entity = "x\x00y" }, "entity"},
		"payload array":  {func(e *Envelope) { e.Payload = json.RawMessage(`[1,2]`) }, "payload"},
		"payload junk":   {func(e *Envelope) { e.Payload = json.RawMessage(`{`) }, "payload"},
		"payload big":    {func(e *Envelope) { e.Payload = json.RawMessage(`{"k":"` + strings.Repeat("x", MaxPayloadBytes) + `"}`) }, "payload"},
		"neg version":    {func(e *Envelope) { e.SchemaVersion = -1 }, "schemaVersion"},
	} {
		e := good()
		tc.mut(&e)
		err := e.Validate(now)
		ve, ok := err.(*ValidationError)
		if !ok {
			t.Errorf("%s: err = %v, want *ValidationError", name, err)
			continue
		}
		if ve.Field != tc.field {
			t.Errorf("%s: field = %q, want %q", name, ve.Field, tc.field)
		}
	}
	// Near-future skew inside the bound is a clock difference, not a fault.
	e := good()
	e.OccurredAt = now.Add(MaxFutureSkew - time.Second)
	if err := e.Validate(now); err != nil {
		t.Fatalf("within skew: %v", err)
	}
}

func TestStoreAppendStampsSourceAndDedupes(t *testing.T) {
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{DB: db, Now: func() time.Time { return now }}

	r1, err := s.Append(good(), "publisher-home")
	if err != nil || r1.Duplicate || r1.Seq == 0 {
		t.Fatalf("append: %+v err=%v", r1, err)
	}
	r2, err := s.Append(good(), "publisher-home")
	if err != nil || !r2.Duplicate || r2.Seq != r1.Seq {
		t.Fatalf("retry: %+v err=%v", r2, err)
	}
	if _, err := s.Append(good(), ""); err == nil {
		t.Fatal("empty source accepted")
	}
	bad := good()
	bad.Type = "nope"
	if _, err := s.Append(bad, "publisher-home"); err == nil {
		t.Fatal("invalid envelope reached the store")
	}
	got, err := s.After(0, 0)
	if err != nil || len(got) != 1 || got[0].Source != "publisher-home" {
		t.Fatalf("after: %+v err=%v", got, err)
	}
}

// pkgspec and resource carry their own copies of the event-type pattern
// so they stay leaf packages; the three must never drift.
func TestEventTypePatternMatchesPkgspecAndResource(t *testing.T) {
	if EventTypeRegexp.String() != pkgspec.EventTypeRegexp.String() {
		t.Fatalf("household %q != pkgspec %q", EventTypeRegexp, pkgspec.EventTypeRegexp)
	}
	// resource's copy is unexported; a rule whose trigger type passes the
	// household pattern must validate, and one that fails it must not.
	for _, tc := range []struct {
		typ string
		ok  bool
	}{{"person.left_home", true}, {"calendar.school.closed", true}, {"left_home", false}, {"Person.Left", false}} {
		if EventTypeRegexp.MatchString(tc.typ) != tc.ok {
			t.Fatalf("household pattern on %q = %v, want %v", tc.typ, !tc.ok, tc.ok)
		}
		r := resource.Resource{
			ApiVersion: resource.ApiVersion, Kind: resource.KindAutomation,
			Metadata: resource.Metadata{Name: "x", Owner: "household"},
			Automation: &resource.AutomationSpec{
				Description: "x", Trigger: resource.AutomationTrigger{EventType: tc.typ},
				Action: resource.AutomationAction{Tool: "a.b", Tier: "domestic"},
			},
		}
		if got := r.Validate() == nil; got != tc.ok {
			t.Fatalf("resource pattern on %q accepted=%v, want %v", tc.typ, got, tc.ok)
		}
	}
}
