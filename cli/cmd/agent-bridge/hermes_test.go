package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
)

// TestBuildRunRequestShape pins exactly what this bridge sends as the
// body of POST /v1/runs: the field names Hermes's API server reads
// (input, instructions, conversation_history of {role, content}). A
// deliberate, reviewed diff is how that shape should change, not a
// silent one.
func TestBuildRunRequestShape(t *testing.T) {
	m := agentapi.Manifest{SystemPrompt: "You are the household's assistant."}
	req := buildRunRequest(m, nil)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"input":"` + defaultRunInput + `","instructions":"You are the household's assistant."}`
	if string(raw) != want {
		t.Fatalf("run request shape =\n%s\nwant\n%s", raw, want)
	}

	payload := []byte(`{"handle":"t.s","text":"and tomorrow?","history":[` +
		`{"role":"user","text":"what's on today?"},{"role":"assistant","text":"nothing"},{"role":"tool","text":"x"}]}`)
	raw, err = json.Marshal(buildRunRequest(m, payload))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"input":"and tomorrow?","instructions":"You are the household's assistant.",` +
		`"conversation_history":[{"role":"user","content":"what's on today?"},{"role":"assistant","content":"nothing"}]}`
	if string(raw) != want {
		t.Fatalf("run request shape =\n%s\nwant\n%s", raw, want)
	}
}

func TestBuildRunRequestUsesPayloadTextWhenPresent(t *testing.T) {
	m := agentapi.Manifest{SystemPrompt: "sys"}
	payload := []byte(`{"handle":"t.s","text":"what's for dinner?"}`)
	req := buildRunRequest(m, payload)
	if req.Input != "what's for dinner?" {
		t.Fatalf("Input = %q, want the payload's own text", req.Input)
	}
}

func TestBuildRunRequestFallsBackOnMalformedPayload(t *testing.T) {
	req := buildRunRequest(agentapi.Manifest{}, []byte(`not json`))
	if req.Input != defaultRunInput {
		t.Fatalf("Input = %q, want the default", req.Input)
	}
}

func TestStartRunSendsBearerAndIdempotencyKey(t *testing.T) {
	var gotAuth, gotIdem, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		body := make([]byte, r.ContentLength)
		r.Body.Read(body) //nolint:errcheck
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"run_id":"run-abc"}`)
	}))
	defer srv.Close()

	c := &hermesClient{Base: srv.URL, APIKey: "herkey"}
	runID, err := c.StartRun(context.Background(), hermesRunRequest{Input: "hi"}, "delivery-1")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "run-abc" {
		t.Fatalf("runID = %q, want run-abc", runID)
	}
	if gotAuth != "Bearer herkey" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotIdem != "delivery-1" {
		t.Fatalf("Idempotency-Key = %q, want delivery-1", gotIdem)
	}
	if !strings.Contains(gotBody, `"input":"hi"`) {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestStartRunNoRunIDIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := &hermesClient{Base: srv.URL, APIKey: "k"}
	if _, err := c.StartRun(context.Background(), hermesRunRequest{}, ""); err == nil {
		t.Fatal("want an error when /v1/runs returns no run_id")
	}
}

func TestStartRunNon2xxIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `unauthorized`)
	}))
	defer srv.Close()
	c := &hermesClient{Base: srv.URL, APIKey: "wrong"}
	if _, err := c.StartRun(context.Background(), hermesRunRequest{}, ""); err == nil {
		t.Fatal("want an error on a non-2xx response")
	}
}

func TestWaitForRunParsesTerminalEvents(t *testing.T) {
	tests := []struct {
		name   string
		event  string
		want   string
		output string
	}{
		// Hermes's own framing: the name under "event", the answer
		// under "output" (api_server_runs.py upstream).
		{"hermes-completed", `{"event":"run.completed","run_id":"run-1","completed":true,"output":"It's sunny."}`, "completed", "It's sunny."},
		{"hermes-failed", `{"event":"run.failed","run_id":"run-1","error":"boom"}`, "failed", ""},
		{"hermes-cancelled", `{"event":"run.cancelled","run_id":"run-1","interrupted":true}`, "cancelled", ""},
		{"completed", `{"type":"run.completed"}`, "completed", ""},
		{"failed", `{"type":"run.failed","error":"boom"}`, "failed", ""},
		{"cancelled", `{"type":"run.cancelled","reason":"stopped"}`, "cancelled", ""},
		{"interrupted", `{"type":"run.interrupted","reason":"steer"}`, "interrupted", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "id: 1\ndata: {\"event\":\"message.delta\",\"delta\":\"It's\"}\n\n")
				fmt.Fprintf(w, "data: %s\n\n", tt.event)
			}))
			defer srv.Close()
			c := &hermesClient{Base: srv.URL, APIKey: "k"}
			outcome, err := c.WaitForRun(context.Background(), "run-1")
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Terminal != tt.want {
				t.Fatalf("Terminal = %q, want %q", outcome.Terminal, tt.want)
			}
			if outcome.Output != tt.output {
				t.Fatalf("Output = %q, want %q", outcome.Output, tt.output)
			}
		})
	}
}

func TestWaitForRunStreamEndsWithoutTerminalEventIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "data: {\"type\":\"tool.started\"}\n\n")
	}))
	defer srv.Close()
	c := &hermesClient{Base: srv.URL, APIKey: "k"}
	if _, err := c.WaitForRun(context.Background(), "run-1"); err == nil {
		t.Fatal("want an error when the stream ends with no terminal event")
	}
}

func TestHealthOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := &hermesClient{Base: srv.URL, APIKey: "k", HTTP: &http.Client{Timeout: 5 * time.Second}}
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHealthNon200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := &hermesClient{Base: srv.URL, APIKey: "k"}
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("want an error on a non-200 health response")
	}
}

// TestWaitForRunLogsToolCalls: each tool.started names its tool in the
// bridge's log, by name only.
func TestWaitForRunLogsToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"event\":\"tool.started\",\"run_id\":\"run-1\",\"tool\":\"mcp_wildstone_remember\",\"preview\":\"favorite color\"}\n\n")
		fmt.Fprint(w, "data: {\"event\":\"tool.completed\",\"run_id\":\"run-1\",\"tool\":\"mcp_wildstone_remember\",\"error\":false}\n\n")
		fmt.Fprint(w, "data: {\"event\":\"run.completed\",\"run_id\":\"run-1\",\"output\":\"noted\"}\n\n")
	}))
	defer srv.Close()
	var buf bytes.Buffer
	c := &hermesClient{Base: srv.URL, APIKey: "k", Log: slog.New(slog.NewTextHandler(&buf, nil))}
	outcome, err := c.WaitForRun(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Output != "noted" {
		t.Fatalf("Output = %q, want the run's answer", outcome.Output)
	}
	logged := buf.String()
	if strings.Count(logged, "hermes tool call") != 1 || !strings.Contains(logged, "tool=mcp_wildstone_remember") {
		t.Fatalf("log = %q, want one tool call line naming the tool", logged)
	}
	if strings.Contains(logged, "favorite color") {
		t.Fatalf("log = %q: a tool's arguments reached the log", logged)
	}
}
