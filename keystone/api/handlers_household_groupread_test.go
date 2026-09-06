package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/household"
)

// The group read rule (ADR-W026 A7): a room may hear about the house and
// its devices, and about whoever is asking. It may not hear about the
// member who is not in it.
//
// Whose fact a type carries comes from the `subject: member` a package
// declares (ADR-W024 A10) — not from the shape of the subject string,
// and not from the model's judgement.
func seedGroupTimeline(t *testing.T, h *harness) {
	t.Helper()
	store := h.s.householdStore()
	for i, ev := range []struct {
		typ, subject, payload string
	}{
		{"person.left_home", "sam", `{"to":"not_home"}`},
		{"person.left_home", "mika", `{"to":"not_home"}`},
		{"door.unlocked", "front-door", `{"by":"keypad"}`},
		{"bins.collected", "", `{"kind":"recycling"}`},
		{"calendar.today", "", `{"note":"Sam therapy 3pm"}`},
	} {
		if _, err := store.Append(household.Envelope{
			EventID:    "seed-" + itoa(int64(i)),
			OccurredAt: time.Now().Add(-time.Duration(i) * time.Minute),
			Type:       ev.typ,
			Subject:    ev.subject,
			Payload:    json.RawMessage(ev.payload),
		}, "test"); err != nil {
			t.Fatalf("seeding %s: %v", ev.typ, err)
		}
	}
}

// memberScopedFixture is what the catalog would report: person.* events
// are about a person, the door and the bins are not.
func memberScopedFixture() func(string) bool {
	return MemberScopedTypes(func() map[string]bool {
		return map[string]bool{"person.left_home": true, "person.arrived_home": true}
	})
}

func TestGroupReadHidesOtherMembersAndKeepsTheHouse(t *testing.T) {
	h := newHarness(t)
	seedGroupTimeline(t, h)
	seam := HouseholdEventSeam(h.s.DB, memberScopedFixture())

	facts, err := seam(context.Background(), butler.HouseholdLookup{
		GroupSafe: true, Member: "mika", Limit: 10,
	})
	if err != nil {
		t.Fatalf("seam: %v", err)
	}
	var got []string
	for _, f := range facts {
		got = append(got, f.Type+"/"+f.Subject)
	}
	joined := strings.Join(got, " ")

	// Another member's movements: withheld.
	if strings.Contains(joined, "person.left_home/sam") {
		t.Fatalf("a group read returned another member's movements: %v", got)
	}
	// The asker's own: kept — the room already knows who is talking.
	if !strings.Contains(joined, "person.left_home/mika") {
		t.Fatalf("a group read dropped the asker's own facts: %v", got)
	}
	// A device: kept. This is the case the first attempt got wrong —
	// "is the front door unlocked?" is exactly a kitchen-group question.
	if !strings.Contains(joined, "door.unlocked/front-door") {
		t.Fatalf("a group read hid a device: %v", got)
	}
	// The household's own: kept.
	if !strings.Contains(joined, "bins.collected/") {
		t.Fatalf("a group read hid a household-wide fact: %v", got)
	}
}

// Naming another member explicitly does not smuggle their facts through.
func TestGroupReadIgnoresANamedOtherMember(t *testing.T) {
	h := newHarness(t)
	seedGroupTimeline(t, h)
	seam := HouseholdEventSeam(h.s.DB, memberScopedFixture())

	facts, err := seam(context.Background(), butler.HouseholdLookup{
		GroupSafe: true, Member: "mika", Subject: "sam", Limit: 10,
	})
	if err != nil {
		t.Fatalf("seam: %v", err)
	}
	if len(facts) != 0 {
		t.Fatalf("asking about another member in a group returned %+v", facts)
	}
}

// A DIRECT session is unchanged: one-to-one, the household timeline is
// shared, and asking about another member is the normal case.
func TestDirectReadIsUnchanged(t *testing.T) {
	h := newHarness(t)
	seedGroupTimeline(t, h)
	seam := HouseholdEventSeam(h.s.DB, memberScopedFixture())

	facts, err := seam(context.Background(), butler.HouseholdLookup{Member: "mika", Limit: 10})
	if err != nil {
		t.Fatalf("seam: %v", err)
	}
	if len(facts) != 5 {
		t.Fatalf("direct read returned %d facts, want all 5", len(facts))
	}
}

// With no catalog there are no declarations to reason from, so every
// attributed fact except the asker's is withheld. A deployment that has
// declared nothing is not one where everyone's movements are public.
func TestGroupReadWithoutDeclarationsWithholdsAttributedFacts(t *testing.T) {
	h := newHarness(t)
	seedGroupTimeline(t, h)
	seam := HouseholdEventSeam(h.s.DB, nil)

	facts, err := seam(context.Background(), butler.HouseholdLookup{
		GroupSafe: true, Member: "mika", Limit: 10,
	})
	if err != nil {
		t.Fatalf("seam: %v", err)
	}
	for _, f := range facts {
		if f.Subject != "" && f.Subject != "mika" {
			t.Fatalf("an undeclared attributed fact reached a group read: %+v", f)
		}
	}
	// The household's own facts still come through, so the room is not
	// left with nothing.
	if len(facts) == 0 {
		t.Fatalf("a group read with no declarations returned nothing at all")
	}
}

// The limit is honoured AFTER the rule removes rows: asking for two and
// having three withheld must not answer with fewer than the timeline can
// actually offer.
func TestGroupReadFillsItsLimit(t *testing.T) {
	h := newHarness(t)
	store := h.s.householdStore()
	// Ten of somebody else's facts, then two the room may hear.
	for i := 0; i < 10; i++ {
		if _, err := store.Append(household.Envelope{
			EventID: "sam-" + itoa(int64(i)), OccurredAt: time.Now().Add(-time.Duration(i+10) * time.Minute),
			Type: "person.left_home", Subject: "sam",
		}, "test"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := store.Append(household.Envelope{
			EventID: "bins-" + itoa(int64(i)), OccurredAt: time.Now().Add(-time.Duration(i) * time.Minute),
			Type: "bins.collected",
		}, "test"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seam := HouseholdEventSeam(h.s.DB, memberScopedFixture())
	facts, err := seam(context.Background(), butler.HouseholdLookup{
		GroupSafe: true, Member: "mika", Limit: 2,
	})
	if err != nil {
		t.Fatalf("seam: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("group read returned %d facts, want 2 — the rule ate the page", len(facts))
	}
	for _, f := range facts {
		if f.Type != "bins.collected" {
			t.Fatalf("unexpected fact in a group read: %+v", f)
		}
	}
}
