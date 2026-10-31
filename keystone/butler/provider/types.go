// Package provider defines a provider-neutral abstraction for chatting with
// an LLM and driving tool calls, plus stdlib-only clients for Anthropic,
// OpenAI-compatible, and Ollama backends.
//
// This package is the S4a slice of ADR-W006 (Spike S4): the Provider
// interface, the three backend clients, and the tool-reliability probe. It
// does not build the tool gateway, agent loop, or read tools over Keystone
// state — that is keystone/butler (S4b).
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

var (
	// ErrEmbedderNotConfigured means the Agent's assigned route has no
	// server-owned embedding adapter. It is deliberately distinct from an
	// adapter that was configured and became unavailable.
	ErrEmbedderNotConfigured = errors.New("provider: embedder not configured")
	ErrEmbeddingUnavailable  = errors.New("provider: embedder unavailable")
	ErrEmbeddingBounds       = errors.New("provider: embedding bounds exceeded")
	ErrEmbeddingMalformed    = errors.New("provider: malformed embedding response")
	ErrEmbeddingDimension    = errors.New("provider: embedding dimension changed")
)

// Embedder is the bounded provider-neutral seam used by semantic memory.
// The caller supplies text only. Provider kind, endpoint, model, credentials,
// processing class and identity all remain server-owned configuration.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Tool is a provider-neutral tool declaration: a name, a human-readable
// description, and a JSON-schema-shaped parameter spec (the same shape as
// Anthropic's input_schema / OpenAI's function.parameters / Ollama's
// function.parameters).
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolCall is a request from the model to invoke a tool with a given input.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolResult is the result of executing a ToolCall, keyed back to the call
// that produced it.
type ToolResult struct {
	CallID  string
	Content string
}

// Message is one turn in a conversation. Role is one of "user", "assistant",
// or "tool". An "assistant" message may carry ToolCalls (the model asking to
// invoke tools). A "tool" message carries ToolResults (the caller's
// responses to a prior assistant turn's tool calls) and is translated by
// each provider into that API's tool-result shape.
type Message struct {
	Role        string
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// ChatRequest is a provider-neutral chat request.
type ChatRequest struct {
	System    string
	Messages  []Message
	Tools     []Tool
	Model     string
	MaxTokens int
}

// ChatResponse is a provider-neutral chat response: either assistant text,
// or one or more tool calls the caller must execute and feed back as a
// "tool" Message.
type ChatResponse struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
	// Truncated is true when StopReason indicates the model hit its output
	// token limit (Anthropic "max_tokens", OpenAI-compatible finish_reason
	// "length", Ollama done_reason "length") rather than finishing
	// naturally. A truncated response with no ToolCalls is NOT a completed
	// final answer — callers should treat Text as a partial result and
	// check this field before surfacing it as done.
	Truncated bool
}

// Provider is the interface every backend (Anthropic, OpenAI-compatible,
// Ollama) implements. Chat is stateless — the caller (the S4b agent loop)
// owns conversation history and re-sends it on every call.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	Name() string
}

// --- Shared HTTP timeout + bounded retry, used by all three provider
// clients. Session.Run (keystone/butler) sets no context deadline, so
// without a default timeout a hung LLM endpoint would hang the operator
// loop forever; and a single transient 429/5xx would otherwise abort the
// whole session.

// defaultHTTPClientTimeout bounds a provider's HTTP client when the caller
// hasn't injected one. Callers that inject their own *http.Client (tests
// pointing at an httptest server, or a caller with its own timeout policy)
// are never overridden — this only fills in the zero-value case.
const defaultHTTPClientTimeout = 60 * time.Second

// httpClientOrDefault returns c if non-nil, otherwise a client with
// defaultHTTPClientTimeout. This preserves the "caller can inject a
// client" seam (tests point HTTPClient at a fake server) while giving
// production callers who leave it nil a sane bound instead of
// http.DefaultClient's no-timeout behavior.
func httpClientOrDefault(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: defaultHTTPClientTimeout}
}

// maxChatAttempts bounds the total number of attempts (the first try plus
// retries) for a single Chat call's HTTP request. Kept low and
// deterministic-friendly per ADR-W006: this is a small, bounded retry for
// transient errors, not a long-lived backoff policy.
const maxChatAttempts = 3

// realRetrySleep is the production backoff sleep implementation: it
// respects context cancellation and returns ctx.Err() if the context is
// canceled before the sleep elapses.
func realRetrySleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retrySleep is the backoff sleep hook used between retry attempts. It is
// a package-level var, defaulting to realRetrySleep, so tests can override
// it (see the package's TestMain) to avoid sleeping for real wall-clock
// time during retry tests.
var retrySleep = realRetrySleep

// isRetryableStatus reports whether an HTTP response status should be
// retried: 429 (rate limited) or any 5xx (server error). Any other 4xx is
// a client error and is never retried.
func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status < 600)
}

// retryBackoff computes how long to wait before attempt+1, given the
// attempt number just completed (1-indexed) and the response's
// "Retry-After" header value (may be empty). Retry-After is honored when
// present and parseable (either delay-seconds or an HTTP-date); otherwise
// falls back to a small deterministic exponential backoff.
func retryBackoff(attempt int, retryAfter string) time.Duration {
	if d, ok := parseRetryAfter(retryAfter); ok {
		return d
	}
	d := time.Duration(1<<uint(attempt-1)) * 100 * time.Millisecond
	const maxBackoff = 2 * time.Second
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// parseRetryAfter parses an HTTP Retry-After header value, per RFC 9110:
// either an integer number of delay-seconds, or an HTTP-date. Returns
// ok=false if v is empty or unparseable as either form.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// doWithRetry executes an HTTP request built by newReq (called fresh on
// every attempt, since a request body reader can only be consumed once),
// retrying on 429/5xx per isRetryableStatus up to maxChatAttempts total
// attempts. It returns the last response's status code and body — even a
// non-2xx one on the final attempt — so the caller builds its usual
// provider-specific error message from status+body; doWithRetry itself
// never turns a non-2xx response into an error. It returns a non-nil error
// only for request-construction failures, transport-level failures, or
// context cancellation (including cancellation observed while backing
// off).
func doWithRetry(ctx context.Context, client *http.Client, newReq func() (*http.Request, error)) (status int, body []byte, err error) {
	client = guardedHTTPClient(client)
	for attempt := 1; attempt <= maxChatAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}

		req, err := newReq()
		if err != nil {
			return 0, nil, err
		}

		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return 0, nil, fmt.Errorf("read response: %w", err)
		}

		status, body = resp.StatusCode, respBody
		if status == http.StatusOK || !isRetryableStatus(status) || attempt == maxChatAttempts {
			return status, body, nil
		}

		if serr := retrySleep(ctx, retryBackoff(attempt, resp.Header.Get("Retry-After"))); serr != nil {
			return 0, nil, serr
		}
	}
	return status, body, nil
}
