package main

// The Hermes half of the bridge: start a run, watch it to completion,
// probe liveness. Three calls against Hermes's own OpenAI-compatible API
// server (hermes.md research brief §6), all bearer-authenticated with
// HERMES_API_KEY — the secret this bridge and Hermes share and Wildstone
// never sees (design doc §2.1).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// hermesRunner is what the delivery loop needs from Hermes, narrowed to
// an interface so a test can drive handleDelivery without a real HTTP
// server on this side.
type hermesRunner interface {
	StartRun(ctx context.Context, req hermesRunRequest, idempotencyKey string) (runID string, err error)
	WaitForRun(ctx context.Context, runID string) (runOutcome, error)
	Health(ctx context.Context) error
}

var _ hermesRunner = (*hermesClient)(nil)

// hermesClient implements hermesRunner against a real Hermes gateway.
type hermesClient struct {
	// Base is Hermes's API server base, e.g. "http://hermes:8642" — the
	// packaged shape reaches it by container name (design doc §2.2).
	Base string
	// APIKey is Hermes's own API_SERVER_KEY.
	APIKey string
	HTTP   *http.Client
	// Log, when set, records each tool a run calls (logToolCall).
	Log *slog.Logger
}

func (c *hermesClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultHermesTimeout}
}

// turnPayload is this bridge's read of a turn/subscription delivery's
// payload: butlerlive.ExternalTurn for a turn (#364) — the handle, what
// the person said and the conversation so far. A subscription delivery
// carries only the handle. Read defensively: an absent or malformed
// field just means less context for the run.
type turnPayload struct {
	Handle  string `json:"handle"`
	Text    string `json:"text,omitempty"`
	History []struct {
		Role string `json:"role"`
		Text string `json:"text"`
	} `json:"history,omitempty"`
}

// defaultRunInput is what this bridge asks Hermes to do when the
// delivery carries no turn text of its own (a subscription). It tells the runtime to reach for its
// Wildstone tools rather than inventing an answer from nothing.
const defaultRunInput = "A Wildstone task is waiting on you. Use the wildstone MCP tools available " +
	"to you to read what is being asked and to act; do not answer from assumption."

// hermesRunRequest is this bridge's request body for POST /v1/runs, in
// the shape Hermes's API server reads it (gateway/platforms/
// api_server_runs.py upstream). TestBuildRunRequestShape pins exactly
// what this bridge sends.
type hermesRunRequest struct {
	// Input is the text this run answers. See defaultRunInput.
	Input string `json:"input"`
	// Instructions is the manifest's own system prompt (agentapi.
	// Manifest.SystemPrompt): Wildstone's instruction hierarchy for this
	// task. Hermes adds it as an ephemeral system prompt beside its own.
	Instructions string `json:"instructions,omitempty"`
	// ConversationHistory is the conversation before this turn.
	ConversationHistory []hermesMessage `json:"conversation_history,omitempty"`
}

// hermesMessage is one conversation_history entry.
type hermesMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// buildRunRequest assembles the run body from a fetched Manifest and the
// delivery's own payload.
func buildRunRequest(m agentapi.Manifest, payload []byte) hermesRunRequest {
	var tp turnPayload
	_ = json.Unmarshal(payload, &tp) // best-effort; absent/malformed just means no text (see turnPayload)
	input := strings.TrimSpace(tp.Text)
	if input == "" {
		input = defaultRunInput
	}
	req := hermesRunRequest{Input: input, Instructions: m.SystemPrompt}
	for _, u := range tp.History {
		if (u.Role == "user" || u.Role == "assistant") && strings.TrimSpace(u.Text) != "" {
			req.ConversationHistory = append(req.ConversationHistory, hermesMessage{Role: u.Role, Content: u.Text})
		}
	}
	return req
}

type hermesRunResponse struct {
	RunID string `json:"run_id"`
}

// StartRun begins one Hermes run and returns its run id.
// idempotencyKey is the delivery's own ID (agentapi.Delivery.ID), so a
// redelivered delivery starts at most one run (ADR-W040 §4, §7).
func (c *hermesClient) StartRun(ctx context.Context, req hermesRunRequest, idempotencyKey string) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("hermes: encoding run request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Base, "/")+"/v1/runs", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("hermes: building run request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("hermes: starting run: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("hermes: reading run response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("hermes: POST /v1/runs: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out hermesRunResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("hermes: decoding run response: %w", err)
	}
	if out.RunID == "" {
		return "", fmt.Errorf("hermes: POST /v1/runs returned no run_id")
	}
	return out.RunID, nil
}

// runOutcome is how a Hermes run ended, read off its SSE event stream.
type runOutcome struct {
	// Terminal is one of "completed", "failed", "cancelled", "interrupted"
	// (hermes.md brief §6's own run.* event names).
	Terminal string
	// Detail is whatever reason/error text the terminal event carried.
	Detail string
	// Output is a completed run's final answer.
	Output string
}

// sseEvent is the handful of fields this bridge reads off a run event.
// Every other field Hermes's SSE stream carries (tool.started,
// tool.completed, subagent.*, approval.request — brief §6) is
// deliberately ignored: this bridge does not narrate a run, and
// answering an approval.request is out of scope here (the Wildstone MCP
// server is `trust: full`, design doc §5.1, so Hermes's own approval
// surface should never gate a Wildstone tool call; a sandbox-command
// approval on Tier 2/3 is denied by `unattended_mode: deny` before it
// would ever reach this stream as something to answer).
//
// Hermes names the event in an "event" key ({"event":"run.completed",
// "run_id":…,"output":…}); "type" is read too, as a fallback for a
// runtime that follows the more common SSE-JSON convention.
type sseEvent struct {
	Event  string `json:"event"`
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	Output string `json:"output,omitempty"`
}

func (ev sseEvent) name() string {
	if ev.Event != "" {
		return ev.Event
	}
	return ev.Type
}

// terminalOutcome maps a parsed sseEvent to a runOutcome, or reports it
// is not a terminal event at all.
func terminalOutcome(ev sseEvent) (runOutcome, bool) {
	switch ev.name() {
	case "run.completed":
		return runOutcome{Terminal: "completed", Output: ev.Output}, true
	case "run.failed":
		return runOutcome{Terminal: "failed", Detail: ev.Error}, true
	case "run.cancelled":
		return runOutcome{Terminal: "cancelled", Detail: ev.Reason}, true
	case "run.interrupted":
		return runOutcome{Terminal: "interrupted", Detail: ev.Reason}, true
	default:
		return runOutcome{}, false
	}
}

// WaitForRun reads runID's SSE event stream until a terminal event
// arrives, and returns it. A non-terminal event (tool.started, and every
// other named field sseEvent does not decode) is silently skipped: this
// bridge reports lifecycle, not narration.
func (c *hermesClient) WaitForRun(ctx context.Context, runID string) (runOutcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.Base, "/")+"/v1/runs/"+url.PathEscape(runID)+"/events", nil)
	if err != nil {
		return runOutcome{}, fmt.Errorf("hermes: building events request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	client := c.httpClient()
	// The events stream is long-lived by design (it lasts as long as the
	// run does); a client carrying a blanket Timeout would cut a healthy
	// run off, so this call uses one with none, mirroring
	// agentapi/httpclient's own reasoning for its default client.
	if client.Timeout != 0 {
		cp := *client
		cp.Timeout = 0
		client = &cp
	}
	resp, err := client.Do(req)
	if err != nil {
		return runOutcome{}, fmt.Errorf("hermes: watching run %s: %w", runID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return runOutcome{}, fmt.Errorf("hermes: GET /v1/runs/%s/events: %s: %s", runID, resp.Status, strings.TrimSpace(string(body)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var data []string
	tryFlush := func() (runOutcome, bool) {
		if len(data) == 0 {
			return runOutcome{}, false
		}
		joined := strings.Join(data, "\n")
		data = data[:0]
		c.logToolCall(runID, []byte(joined))
		var ev sseEvent
		if err := json.Unmarshal([]byte(joined), &ev); err != nil {
			return runOutcome{}, false // an SSE comment or a shape we don't decode; not an error
		}
		return terminalOutcome(ev)
	}
	for sc.Scan() {
		if ctx.Err() != nil {
			return runOutcome{}, ctx.Err()
		}
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if out, done := tryFlush(); done {
				return out, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return runOutcome{}, fmt.Errorf("hermes: reading run %s events: %w", runID, err)
	}
	if out, done := tryFlush(); done {
		return out, nil
	}
	return runOutcome{}, fmt.Errorf("hermes: run %s's events stream ended with no terminal event", runID)
}

// logToolCall logs the tool a run calls, from its tool.started event:
// the tool's name only, never its arguments or result. It is how a turn
// that ended "not done" can be told apart from one whose tool was
// refused, and a Hermes-native tool from a Wildstone one.
func (c *hermesClient) logToolCall(runID string, data []byte) {
	if c.Log == nil {
		return
	}
	var ev struct {
		Event string `json:"event"`
		Type  string `json:"type"`
		Tool  string `json:"tool"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	name := ev.Event
	if name == "" {
		name = ev.Type
	}
	if name == "tool.started" && ev.Tool != "" {
		c.Log.Info("hermes tool call", "run", runID, "tool", ev.Tool)
	}
}

// Health probes Hermes's own liveness endpoint.
func (c *hermesClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.Base, "/")+"/health", nil)
	if err != nil {
		return fmt.Errorf("hermes: building health request: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("hermes: GET /health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNoHealthEndpoint
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hermes: GET /health: %s", resp.Status)
	}
	return nil
}

// defaultHermesTimeout bounds StartRun and Health, which are short
// request/response calls, not the events stream above.
const defaultHermesTimeout = 30 * time.Second
