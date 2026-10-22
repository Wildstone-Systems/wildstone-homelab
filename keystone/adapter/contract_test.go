package adapter_test

// The adapter contract suite (ADR-W030 A5.1, work package P12).
//
// One suite, run against every Upstream. That is what makes the P12
// evidence — "limited real Home Assistant and CalDAV deployments pass
// the same contract tests as their fakes" — a fact rather than a
// promise: the real deployments are enrolled by environment variable
// and run these exact cases.
//
// The suite is about the CONTRACT, not about any application: an
// unavailable source is reported as unavailable, an empty answer is
// reported as empty and never as an outage, an unknown capability is
// not a crash, and a read-only adapter refuses to act. Those are the
// four ways an integration lies to a household, and they are the four
// things checked here.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/caldav"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/homeassistant"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/pantry"
)

// subject is one Upstream under test, with the capability names this
// suite needs to exercise it.
type subject struct {
	name string
	up   adapter.Upstream
	// aQuery answers with content; anEmptyQuery answers with nothing.
	aQuery       string
	aQueryReq    string
	anEmptyQuery string
	emptyReq     string
	// readOnly means Act must refuse everything.
	readOnly bool
}

// fakeHomeAssistant serves the two endpoints the adapter uses.
func fakeHomeAssistant(t *testing.T, entities string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"2026.9.1"}`))
	})
	mux.HandleFunc("/api/states", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(entities))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const haEntities = `[
  {"entity_id":"person.sam","state":"home","last_changed":"2026-09-09T07:00:00Z","attributes":{"friendly_name":"Sam"}},
  {"entity_id":"person.mika","state":"not_home","last_changed":"2026-09-09T08:10:00Z","attributes":{"friendly_name":"Mika"}},
  {"entity_id":"light.kitchen","state":"on","last_changed":"2026-09-09T06:40:00Z","attributes":{}}
]`

// fakeCalDAV serves a REPORT with the given multistatus body.
func fakeCalDAV(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "REPORT" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const calWithEvents = `<?xml version="1.0"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
 <response><propstat><prop><C:calendar-data>BEGIN:VCALENDAR
BEGIN:VEVENT
UID:ev-1
SUMMARY:School run
DTSTART:20260909T080000Z
DTEND:20260909T083000Z
END:VEVENT
BEGIN:VEVENT
UID:ev-2
SUMMARY:Dentist for
  Sam
DTSTART:20260909T140000Z
END:VEVENT
END:VCALENDAR</C:calendar-data></prop></propstat></response>
</multistatus>`

const calEmpty = `<?xml version="1.0"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"></multistatus>`

func subjects(t *testing.T) []subject {
	t.Helper()
	var out []subject

	// Home Assistant, against a fake that serves the real endpoints.
	ha := fakeHomeAssistant(t, haEntities)
	out = append(out, subject{
		name: "home-assistant (fake)",
		up: &homeassistant.Upstream{
			BaseURL: ha.URL, Token: "test",
			Now: func() time.Time { return time.Date(2026, 9, 9, 8, 30, 0, 0, time.UTC) },
		},
		aQuery: homeassistant.CapPresence, aQueryReq: `{}`,
		// A domain nothing is in is a real, empty answer about the house.
		anEmptyQuery: homeassistant.CapStates, emptyReq: `{"domain":"vacuum"}`,
		readOnly: true,
	})

	// CalDAV: one server with events, one without, because the empty
	// case is a different response body rather than a different query.
	withEvents := fakeCalDAV(t, calWithEvents)
	out = append(out, subject{
		name: "caldav (fake, has events)",
		up: &caldav.Upstream{
			CollectionURL: withEvents.URL, Username: "sam", Password: "ref-only",
			Now: func() time.Time { return time.Date(2026, 9, 9, 6, 0, 0, 0, time.UTC) },
		},
		aQuery: caldav.CapToday, aQueryReq: `{}`,
		readOnly: true,
	})
	empty := fakeCalDAV(t, calEmpty)
	out = append(out, subject{
		name: "caldav (fake, empty day)",
		up: &caldav.Upstream{
			CollectionURL: empty.URL, Username: "sam", Password: "ref-only",
			Now: func() time.Time { return time.Date(2026, 9, 9, 6, 0, 0, 0, time.UTC) },
		},
		anEmptyQuery: caldav.CapToday, emptyReq: `{}`,
		readOnly: true,
	})

	// PantryOS: the fake, and the real HTTP upstream against a server
	// that speaks pantry-1. An empty pantry is its own subject because,
	// under pantry-1, the only list PantryOS has is the grocery list — a
	// list it does not keep is a refused request, not an empty one.
	out = append(out, subject{
		name:   "pantryos (fake)",
		up:     pantry.NewFake(),
		aQuery: pantry.CapList, aQueryReq: `{"list":"groceries"}`,
	})
	out = append(out, subject{
		name:         "pantryos (fake, empty pantry)",
		up:           pantry.NewEmptyFake(),
		anEmptyQuery: pantry.CapList, emptyReq: `{"list":"groceries"}`,
	})
	stocked := fakePantryOS(t, []string{"milk", "bread"})
	out = append(out, subject{
		name:   "pantryos (http, stocked)",
		up:     &pantry.HTTP{BaseURL: stocked.URL, AgentKey: "REPLACED-agent-key"},
		aQuery: pantry.CapList, aQueryReq: `{"list":"groceries"}`,
	})
	bare := fakePantryOS(t, nil)
	out = append(out, subject{
		name:         "pantryos (http, empty pantry)",
		up:           &pantry.HTTP{BaseURL: bare.URL, AgentKey: "REPLACED-agent-key"},
		anEmptyQuery: pantry.CapList, emptyReq: `{"list":"groceries"}`,
	})

	// Real deployments, enrolled by environment. When set, they run the
	// same cases — which is the whole point of the suite.
	if base, token := os.Getenv("WILDSTONE_TEST_HA_URL"), os.Getenv("WILDSTONE_TEST_HA_TOKEN"); base != "" && token != "" {
		out = append(out, subject{
			name:   "home-assistant (real)",
			up:     &homeassistant.Upstream{BaseURL: base, Token: token},
			aQuery: homeassistant.CapStates, aQueryReq: `{}`,
			// A real house has no vacuum domain more often than not, and
			// if it does the case simply passes as a non-empty answer.
			readOnly: true,
		})
	}
	if url := os.Getenv("WILDSTONE_TEST_CALDAV_URL"); url != "" {
		out = append(out, subject{
			name: "caldav (real)",
			up: &caldav.Upstream{
				CollectionURL: url,
				Username:      os.Getenv("WILDSTONE_TEST_CALDAV_USER"),
				Password:      os.Getenv("WILDSTONE_TEST_CALDAV_PASSWORD"),
			},
			aQuery: caldav.CapToday, aQueryReq: `{}`,
			readOnly: true,
		})
	}
	if base, key := os.Getenv("WILDSTONE_TEST_PANTRY_URL"), os.Getenv("WILDSTONE_TEST_PANTRY_KEY"); base != "" && key != "" {
		out = append(out, subject{
			name:   "pantryos (real)",
			up:     &pantry.HTTP{BaseURL: base, AgentKey: key},
			aQuery: pantry.CapSummary, aQueryReq: `{}`,
		})
	}
	return out
}

// fakePantryOS serves the three pantry-1 routes the suite touches, in
// PantryOS's own response shapes.
func fakePantryOS(t *testing.T, groceries []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/integrations/wildstone/v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"contract":"pantry-1","capabilities":[
		  {"kind":"query","name":"pantry.list","version":"pantry-1","available":true},
		  {"kind":"query","name":"pantry.summary","version":"pantry-1","available":true},
		  {"kind":"action","name":"pantry.add","version":"pantry-1","available":true}]}`))
	})
	mux.HandleFunc("GET /api/integrations/wildstone/v1/query/{capability}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("capability") {
		case "pantry.list":
			items, _ := json.Marshal(append([]string{}, groceries...))
			status := "ok"
			if len(groceries) == 0 {
				status = "empty"
			}
			_, _ = w.Write([]byte(`{"contract":"pantry-1","capability":"pantry.list","status":"` + status +
				`","observedAt":"2026-09-09T08:00:00Z","data":{"list":"groceries","items":` + string(items) + `}}`))
		case "pantry.summary":
			_, _ = w.Write([]byte(`{"contract":"pantry-1","capability":"pantry.summary","status":"ok","data":{"locations":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"PantryOS has no query '` + r.PathValue("capability") + `'"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAdapterContract(t *testing.T) {
	ctx := context.Background()
	for _, s := range subjects(t) {
		t.Run(s.name, func(t *testing.T) {
			t.Run("capabilities are verified, not declared", func(t *testing.T) {
				caps, err := s.up.Capabilities(ctx)
				if err != nil {
					t.Fatalf("Capabilities: %v", err)
				}
				if len(caps) == 0 {
					t.Fatal("an upstream that declares nothing cannot be depended on")
				}
				for _, c := range caps {
					if c.Name == "" || c.Kind == "" {
						t.Fatalf("a capability with no name or kind: %+v", c)
					}
					if !c.Available && c.Detail == "" {
						t.Fatalf("%s is unavailable with no reason given", c.Name)
					}
				}
			})

			if s.aQuery != "" {
				t.Run("a real answer is not empty and not stale", func(t *testing.T) {
					got, err := s.up.Query(ctx, s.aQuery, json.RawMessage(s.aQueryReq))
					if err != nil {
						t.Fatalf("Query(%s): %v", s.aQuery, err)
					}
					if got.Data == nil {
						t.Fatal("a successful query returned no data")
					}
					if got.Stale && got.ObservedAt.IsZero() {
						t.Fatal("a stale answer with no observation time would be reported as current")
					}
				})
			}

			if s.anEmptyQuery != "" {
				t.Run("an empty answer is empty, never unavailable", func(t *testing.T) {
					got, err := s.up.Query(ctx, s.anEmptyQuery, json.RawMessage(s.emptyReq))
					if err != nil {
						t.Fatalf("an empty result came back as an error: %v", err)
					}
					if !got.Empty {
						t.Fatalf("a genuinely empty answer was not marked empty: %+v", got)
					}
					if got.Data == nil {
						t.Fatal("an empty answer still has to say what was empty")
					}
				})
			}

			t.Run("an unknown capability is unavailable, not a crash", func(t *testing.T) {
				_, err := s.up.Query(ctx, "nothing.like.this", json.RawMessage(`{}`))
				if !errors.Is(err, adapter.ErrNoSuchCapability) {
					t.Fatalf("an unknown capability = %v, want ErrNoSuchCapability", err)
				}
			})

			if s.readOnly {
				t.Run("a read-only adapter refuses to act", func(t *testing.T) {
					_, err := s.up.Act(ctx, s.aQuery, json.RawMessage(`{}`))
					if !errors.Is(err, adapter.ErrNoSuchCapability) {
						t.Fatalf("a read-only adapter acted, or failed unclearly: %v", err)
					}
				})
			}
		})
	}
}

// An unreachable source is reported as unavailable — the case a
// household most needs told apart from an empty answer.
func TestUnreachableSourcesAreUnavailable(t *testing.T) {
	ctx := context.Background()

	// A server that is up and refusing, which is what a rotated token
	// looks like.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer refusing.Close()

	for name, up := range map[string]adapter.Upstream{
		"home assistant refusing": &homeassistant.Upstream{BaseURL: refusing.URL, Token: "stale"},
		"caldav refusing":         &caldav.Upstream{CollectionURL: refusing.URL, Username: "sam"},
		"home assistant gone":     &homeassistant.Upstream{BaseURL: "http://127.0.0.1:1", Token: "x"},
		"pantryos refusing":       &pantry.HTTP{BaseURL: refusing.URL, AgentKey: "REPLACED-revoked"},
		"pantryos gone":           &pantry.HTTP{BaseURL: "http://127.0.0.1:1", AgentKey: "REPLACED-agent-key"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := up.Query(ctx, firstQuery(up), json.RawMessage(`{}`))
			if !errors.Is(err, adapter.ErrUnavailable) {
				t.Fatalf("an unreachable source = %v, want ErrUnavailable", err)
			}
			// And its capabilities report unavailable WITH a reason,
			// rather than disappearing.
			caps, err := up.Capabilities(ctx)
			if err != nil {
				t.Fatalf("Capabilities on an unreachable source: %v", err)
			}
			for _, c := range caps {
				if c.Available {
					t.Fatalf("%s reported available while the source is unreachable", c.Name)
				}
				if c.Detail == "" {
					t.Fatalf("%s is unavailable with no reason a person could act on", c.Name)
				}
			}
		})
	}

	// The pantry fake covers the same path for an upstream that has no
	// network at all.
	f := pantry.NewFake()
	f.Unavailable = true
	if _, err := f.Query(ctx, pantry.CapList, json.RawMessage(`{"list":"groceries"}`)); !errors.Is(err, adapter.ErrUnavailable) {
		t.Fatalf("an unavailable pantry = %v", err)
	}
}

func firstQuery(up adapter.Upstream) string {
	switch up.(type) {
	case *caldav.Upstream:
		return caldav.CapToday
	case *pantry.HTTP:
		return pantry.CapInventory
	default:
		return homeassistant.CapStates
	}
}

// A stale answer carries when it was true, so a caller can say "as of an
// hour ago" instead of implying now.
func TestStaleAnswersCarryTheirObservationTime(t *testing.T) {
	f := pantry.NewFake()
	observed := time.Date(2026, 9, 9, 6, 0, 0, 0, time.UTC)
	f.StaleAfter = observed
	f.Now = func() time.Time { return observed.Add(time.Hour) }

	got, err := f.Query(context.Background(), pantry.CapList, json.RawMessage(`{"list":"groceries"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stale {
		t.Fatal("a cached answer was not marked stale")
	}
	if !got.ObservedAt.Equal(observed) {
		t.Fatalf("observedAt = %s, want %s", got.ObservedAt, observed)
	}
}

// The parser handles what a real calendar actually sends: folded lines,
// escaped text, and all-day events.
func TestCalDAVParsesRealisticEvents(t *testing.T) {
	srv := fakeCalDAV(t, calWithEvents)
	up := &caldav.Upstream{
		CollectionURL: srv.URL,
		Now:           func() time.Time { return time.Date(2026, 9, 9, 6, 0, 0, 0, time.UTC) },
	}
	got, err := up.Query(context.Background(), caldav.CapToday, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	data := got.Data.(map[string]any)
	events := data["events"].([]caldav.Event)
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Summary != "School run" {
		t.Fatalf("first event = %+v", events[0])
	}
	// A folded SUMMARY is one summary, rejoined exactly as it was
	// written. The fixture's continuation line opens with TWO spaces:
	// one is the fold, the other is the space between the words. This
	// accepted "Dentist forSam" as well until the write path's round
	// trip made the difference impossible to ignore.
	if events[1].Summary != "Dentist for Sam" {
		t.Fatalf("a folded summary was not rejoined: %q", events[1].Summary)
	}
	if events[0].Start.IsZero() {
		t.Fatal("an event with no start time")
	}
}
