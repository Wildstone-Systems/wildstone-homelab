package pantry

// The translation table between pantry-1 and the adapter's result types
// (ADR-W044 A2). The shared contract suite checks the four ways an
// integration can lie; this checks the PantryOS-specific edges: which
// 404 is "no such capability" and which is "wrong address", that a
// refused request is neither empty nor an outage, and that requests
// reach PantryOS in the shape it reads.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter"
)

type seen struct {
	method, path, query, key, body string
}

// pantryServer answers every request with status and body, and records
// what it was sent.
func pantryServer(t *testing.T, status int, body string) (*HTTP, *seen) {
	t.Helper()
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Agent-Key"), string(b)}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &HTTP{BaseURL: srv.URL + "/", AgentKey: "REPLACED-agent-key"}, got
}

func TestQueryReachesPantryOSAsAGetWithParams(t *testing.T) {
	up, got := pantryServer(t, 200, `{"contract":"pantry-1","capability":"pantry.expiring","status":"ok","data":{"items":[]}}`)
	_, err := up.Query(context.Background(), CapExpiring, json.RawMessage(`{"within_days":3,"list":null,"search":"eggs & ham"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodGet || got.path != "/api/integrations/wildstone/v1/query/pantry.expiring" {
		t.Fatalf("sent %s %s", got.method, got.path)
	}
	// Null is omitted, numbers keep no trailing zeros, text is escaped.
	if got.query != "search=eggs+%26+ham&within_days=3" {
		t.Fatalf("query = %q", got.query)
	}
	if got.key != "REPLACED-agent-key" {
		t.Fatal("the agent key was not sent")
	}
}

func TestNestedQueryParamsAreRefusedNotDropped(t *testing.T) {
	up, got := pantryServer(t, 200, `{}`)
	_, err := up.Query(context.Background(), CapInventory, json.RawMessage(`{"search":{"name":"eggs"}}`))
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if got.method != "" {
		t.Fatal("a malformed request still reached PantryOS")
	}
}

func TestActPostsTheRequestAsTheBody(t *testing.T) {
	up, got := pantryServer(t, 200, `{"contract":"pantry-1","capability":"pantry.add","status":"ok","data":{"added":true}}`)
	ans, err := up.Act(context.Background(), CapAdd, json.RawMessage(`{"list":"groceries","item":"milk"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/api/integrations/wildstone/v1/act/pantry.add" {
		t.Fatalf("sent %s %s", got.method, got.path)
	}
	if got.body != `{"list":"groceries","item":"milk"}` {
		t.Fatalf("body = %s", got.body)
	}
	if ans.Data.(map[string]any)["added"] != true {
		t.Fatalf("data = %+v", ans.Data)
	}

	// An action with no request still sends a JSON object. (The canned
	// reply names pantry.add, so it is refused as an answer — the request
	// still went out, which is what this checks.)
	_, _ = up.Act(context.Background(), CapCookAbandon, nil)
	if got.body != "{}" {
		t.Fatalf("empty act body = %q", got.body)
	}
}

func TestResultStatusesMapOntoResultTypes(t *testing.T) {
	observed := "2026-09-09T06:00:00Z"
	cases := []struct {
		name, body string
		check      func(t *testing.T, a adapter.Answer, err error)
	}{
		{"ok", `{"contract":"pantry-1","capability":"pantry.summary","status":"ok","data":{"n":1}}`, func(t *testing.T, a adapter.Answer, err error) {
			if err != nil || a.Empty || a.Stale {
				t.Fatalf("%+v %v", a, err)
			}
		}},
		{"empty", `{"contract":"pantry-1","capability":"pantry.summary","status":"empty","data":{"items":[]}}`, func(t *testing.T, a adapter.Answer, err error) {
			if err != nil || !a.Empty || a.Data == nil {
				t.Fatalf("%+v %v", a, err)
			}
		}},
		{"stale", `{"contract":"pantry-1","capability":"pantry.summary","status":"stale","observedAt":"` + observed + `","data":{}}`, func(t *testing.T, a adapter.Answer, err error) {
			want, _ := time.Parse(time.RFC3339, observed)
			if err != nil || !a.Stale || !a.ObservedAt.Equal(want) {
				t.Fatalf("%+v %v", a, err)
			}
		}},
		{"stale without a time", `{"contract":"pantry-1","capability":"pantry.summary","status":"stale","data":{}}`, func(t *testing.T, _ adapter.Answer, err error) {
			if !errors.Is(err, adapter.ErrUnavailable) {
				t.Fatalf("err = %v: a stale answer with no time would read as current", err)
			}
		}},
		{"unavailable", `{"contract":"pantry-1","capability":"pantry.summary","status":"unavailable","data":{}}`, func(t *testing.T, _ adapter.Answer, err error) {
			if !errors.Is(err, adapter.ErrUnavailable) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"unknown status", `{"contract":"pantry-1","capability":"pantry.summary","status":"maybe","data":{}}`, func(t *testing.T, _ adapter.Answer, err error) {
			if !errors.Is(err, adapter.ErrUnavailable) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"another contract", `{"contract":"pantry-2","capability":"pantry.summary","status":"ok","data":{}}`, func(t *testing.T, _ adapter.Answer, err error) {
			if !errors.Is(err, adapter.ErrUnavailable) {
				t.Fatalf("err = %v: a pantry-2 answer was accepted as pantry-1", err)
			}
		}},
		{"another capability", `{"contract":"pantry-1","capability":"pantry.list","status":"ok","data":{}}`, func(t *testing.T, _ adapter.Answer, err error) {
			if !errors.Is(err, adapter.ErrUnavailable) {
				t.Fatalf("err = %v: an answer to a different question was accepted", err)
			}
		}},
		{"not json", `<html>`, func(t *testing.T, _ adapter.Answer, err error) {
			if !errors.Is(err, adapter.ErrUnavailable) {
				t.Fatalf("err = %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := pantryServer(t, 200, tc.body)
			a, err := up.Query(context.Background(), CapSummary, nil)
			tc.check(t, a, err)
		})
	}
}

func TestErrorStatusesMapOntoResultTypes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		// PantryOS naming the capability it does not have.
		{"unknown capability", 404, `{"detail":"PantryOS has no query 'pantry.nope'"}`, adapter.ErrNoSuchCapability},
		// A 404 that is not PantryOS's: the address is wrong. That is a
		// broken connection, not a missing feature.
		{"wrong address", 404, `Not Found`, adapter.ErrUnavailable},
		// A wrong question: neither empty nor down.
		{"refused request", 422, `{"detail":"pantry.list: PantryOS keeps only the grocery list"}`, ErrRejected},
		{"revoked key", 401, `{"detail":"Invalid or revoked agent key"}`, adapter.ErrUnavailable},
		{"read-only key acting", 403, `{"detail":"Agent key lacks the 'write' scope"}`, adapter.ErrUnavailable},
		{"server error", 500, `boom`, adapter.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := pantryServer(t, tc.status, tc.body)
			_, err := up.Query(context.Background(), CapList, json.RawMessage(`{"list":"groceries"}`))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCapabilitiesFromADifferentContractAreUnavailable(t *testing.T) {
	up, _ := pantryServer(t, 200, `{"contract":"pantry-2","capabilities":[{"kind":"query","name":"pantry.list","available":true}]}`)
	caps, err := up.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != len(knownCapabilities) {
		t.Fatalf("caps = %d, want every pantry-1 capability reported", len(caps))
	}
	for _, c := range caps {
		if c.Available || c.Detail == "" {
			t.Fatalf("%+v: a contract this adapter does not speak must not look usable", c)
		}
	}
}

func TestCapabilitiesAreReadFromPantryOS(t *testing.T) {
	up, got := pantryServer(t, 200, `{"contract":"pantry-1","capabilities":[
	  {"kind":"query","name":"pantry.list","version":"pantry-1","available":true},
	  {"kind":"action","name":"pantry.cook.finish","version":"pantry-1","available":false}]}`)
	caps, err := up.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/integrations/wildstone/v1/capabilities" || got.key == "" {
		t.Fatalf("asked %s with key %q", got.path, got.key)
	}
	if len(caps) != 2 || !caps[0].Available || caps[1].Available || caps[1].Detail == "" {
		t.Fatalf("caps = %+v", caps)
	}
}

func TestTheFakeRefusesListsPantryOSDoesNotKeep(t *testing.T) {
	f := NewFake()
	if _, err := f.Query(context.Background(), CapList, json.RawMessage(`{"list":"hardware"}`)); !errors.Is(err, ErrRejected) {
		t.Fatalf("hardware list = %v, want ErrRejected", err)
	}
	if _, err := f.Act(context.Background(), CapAdd, json.RawMessage(`{"list":"hardware","item":"nails"}`)); !errors.Is(err, ErrRejected) {
		t.Fatalf("adding to hardware = %v, want ErrRejected", err)
	}
	// "Milk" is milk: already on the list, not added twice.
	ans, err := f.Act(context.Background(), CapAdd, json.RawMessage(`{"list":"shopping","item":"Milk"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ans.Data.(map[string]any)["added"] != false || len(f.Items("groceries")) != 2 {
		t.Fatalf("answer = %+v, groceries = %v", ans.Data, f.Items("groceries"))
	}
}

func TestRedirectsAreNotFollowedAndTheKeyStaysHome(t *testing.T) {
	leaked := make(chan string, 1)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get("X-Agent-Key")
		_, _ = w.Write([]byte(`{"contract":"pantry-1","capability":"pantry.add","status":"ok","data":{}}`))
	}))
	defer elsewhere.Close()
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, code)
		}))
		// Even a caller-supplied client gets the no-redirect policy.
		up := &HTTP{BaseURL: redirecting.URL, AgentKey: "REPLACED-agent-key", HTTP: &http.Client{}}
		_, err := up.Act(context.Background(), CapAdd, json.RawMessage(`{"list":"groceries","item":"milk"}`))
		redirecting.Close()
		if !errors.Is(err, adapter.ErrUnavailable) {
			t.Fatalf("%d: err = %v, want ErrUnavailable", code, err)
		}
		select {
		case key := <-leaked:
			t.Fatalf("%d: the redirect was followed and carried the key %q", code, key)
		default:
		}
		if _, err := up.Capabilities(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-leaked:
			t.Fatalf("%d: the capability request followed the redirect", code)
		default:
		}
	}
}
