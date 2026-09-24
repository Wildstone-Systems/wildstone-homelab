package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi/httpclient"
)

// TestEndToEndDeliveryNeverLogsTheBearer runs one delivery through real
// HTTP on both sides — agentapi/httpclient.Client against a fake
// Keystone, hermesClient against a fake Hermes — and checks the
// manifest→start→run→complete→ack ordering the H3 brief asks for, and
// that WILDSTONE_RUNTIME_TOKEN never appears in a log line however this
// bridge's own calls fail or succeed.
func TestEndToEndDeliveryNeverLogsTheBearer(t *testing.T) {
	const bearer = "wst_supersecrettoken0123456789"
	var order []string
	served := false

	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+bearer {
			t.Errorf("request to %s carried Authorization %q, want Bearer %s", r.URL.Path, got, bearer)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/deliveries":
			if served {
				fmt.Fprint(w, `{"deliveries":[]}`)
				return
			}
			served = true
			payload, _ := json.Marshal(map[string]string{"handle": "task1.secret"})
			d := agentapi.Delivery{ID: "1", Kind: agentapi.DeliveryTurn, Handle: "task1.secret", Payload: payload}
			raw, _ := json.Marshal(struct {
				Deliveries []agentapi.Delivery `json:"deliveries"`
			}{[]agentapi.Delivery{d}})
			w.Write(raw) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/manifest":
			order = append(order, "manifest")
			json.NewEncoder(w).Encode(agentapi.Manifest{SystemPrompt: "be helpful"}) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/start":
			order = append(order, "start")
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/complete":
			order = append(order, "complete")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack"):
			order = append(order, "ack")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ks.Close()

	hermesRan := false
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			hermesRan = true
			order = append(order, "run-start")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"run_id":"run-1"}`)
		case r.URL.Path == "/v1/runs/run-1/events":
			order = append(order, "run-wait")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"run.completed\"}\n\n")
		default:
			t.Errorf("unexpected hermes request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer hs.Close()

	var logBuf bytes.Buffer
	br := &bridge{
		keystone: &httpclient.Client{BaseURL: ks.URL, Bearer: bearer},
		hermes:   &hermesClient{Base: hs.URL, APIKey: "herkey"},
		runs:     newRunRegistry(),
		log:      slog.New(slog.NewTextHandler(&logBuf, nil)),
		pollWait: time.Second,
		version:  "test",
	}

	deliveries, err := br.keystone.Deliveries(t.Context(), "", time.Second)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(deliveries))
	}
	if !br.handleDelivery(t.Context(), deliveries[0]) {
		t.Fatal("handleDelivery returned false, want true")
	}
	if !hermesRan {
		t.Fatal("hermes never received the run")
	}
	want := []string{"manifest", "start", "run-start", "run-wait", "complete", "ack"}
	if len(order) != len(want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call order = %v, want %v", order, want)
		}
	}

	if strings.Contains(logBuf.String(), bearer) {
		t.Fatalf("log output contains the bearer token:\n%s", logBuf.String())
	}
}
