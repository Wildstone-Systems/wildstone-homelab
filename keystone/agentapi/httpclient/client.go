package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// Client implements agentapi.Client against Keystone's REST routes
// (ADR-W040 §2's endpoint map: POST /api/v1/agent/{manifest,invoke,
// recall,remember,checkpoint,start,wait,complete,fail,propose,notify}
// and POST /api/v1/agent/heartbeat). The zero value is not usable:
// BaseURL and Bearer are required.
type Client struct {
	// BaseURL is Keystone's own base, e.g.
	// "https://keystone.internal:8443". A trailing slash is trimmed.
	BaseURL string
	// Bearer is the runtime principal's `wst_` secret (ADR-W040 §2:
	// "Authorization: Bearer wst_<64 hex>"), presented on every call —
	// no session, nothing that outlives a request, so a rotation or a
	// revocation lands on this Client's very next call once Bearer is
	// updated or the credential is replaced.
	Bearer string
	// HTTP is the client used for every call. nil means defaultHTTP.
	HTTP *http.Client
}

var _ agentapi.Client = (*Client)(nil)

// ErrRefused is what every 403 this Client receives wraps: a handle
// refused, or a credential that is not bound as a runtime principal at
// all. It mirrors keystone/agentgateway.ErrRefused and
// keystone/butlerstore.ErrHandleRefused one layer out — this package
// cannot import either without becoming the Wildstone internal its own
// doc comment says it must not be — so it names its own sentinel for
// the one thing every refusal here means: something about this CALL was
// refused, never a gateway Outcome (denied/waiting/rejected), which
// arrives as an ordinary response, not an error.
var ErrRefused = errors.New("httpclient: refused")

// RefusedError is the concrete type ErrRefused matches through
// errors.Is. Message is the server's own refusal text, redacted the
// same way every other Keystone response body is (ADR-W007 A5) before
// it ever reaches this Client.
type RefusedError struct{ Message string }

func (e *RefusedError) Error() string        { return "httpclient: refused: " + e.Message }
func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

// StatusError is any HTTP failure this Client does not interpret more
// specifically: authentication failed (401), the request was malformed
// (400 — should not happen from a caller that sends agentapi's own
// shapes), the Agent API is not wired on this deployment (503), or
// anything else Keystone answered with outside the 2xx/403/501 cases
// this package gives their own types.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("httpclient: %s: %s", http.StatusText(e.Status), e.Message)
}

// httpClient returns the configured client, or a default with no
// blanket Timeout: a long-poll delivery call (ADR-W039 §8 Phase 4, not
// yet a method on this Client) legitimately waits up to ADR-W040 §4's
// bound, and a client-wide deadline shorter than that would cut such a
// call off through no fault of it. A caller bounds an individual call
// with its own context (context.WithTimeout) instead, the discipline
// net/http itself recommends for a client used this way; the transport
// below still bounds connection setup, so a genuinely unreachable
// Keystone fails fast rather than hanging forever.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTPClient
}

var defaultHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		// ResponseHeaderTimeout is deliberately unset: it would bound
		// every call, including a future long-poll one waiting on
		// ADR-W040 §4's own bound (up to 30s plus slack), the same
		// reason the Client carries no Timeout.
	},
}

// notImplementedBody mirrors keystone/api's own — the 501 shape
// handlers_agent.go answers with — without importing that package,
// which would make this one the Wildstone internal its doc comment
// says it must never be.
type notImplementedBody struct {
	Method string `json:"method"`
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
}

// errorBody is the ordinary Keystone error shape, {"error": "..."}
// (api.writeError), read back here for a readable message rather than
// the raw JSON.
func errorMessage(raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Error != "" {
		return body.Error
	}
	return strings.TrimSpace(string(raw))
}

// do is every call this Client makes: encode the request 1:1, present
// the bearer, decode the response 1:1, and translate a non-2xx status
// into the error type it means. respBody is nil for a call whose
// method returns no payload (agentapi.Client's own void methods).
func (c *Client) do(ctx context.Context, path string, reqBody, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("httpclient: encoding the request for %s: %w", path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return fmt.Errorf("httpclient: building the request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("httpclient: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("httpclient: reading the response from %s: %w", path, err)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		if respBody == nil || len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, respBody); err != nil {
			return fmt.Errorf("httpclient: decoding the response from %s: %w", path, err)
		}
		return nil
	case http.StatusForbidden:
		return &RefusedError{Message: errorMessage(raw)}
	case http.StatusNotImplemented:
		var nie notImplementedBody
		if jerr := json.Unmarshal(raw, &nie); jerr == nil && nie.Method != "" {
			return &agentapi.NotImplementedError{Method: nie.Method, Phase: nie.Phase, Reason: nie.Detail}
		}
		return &StatusError{Status: resp.StatusCode, Message: errorMessage(raw)}
	default:
		return &StatusError{Status: resp.StatusCode, Message: errorMessage(raw)}
	}
}

// Manifest implements agentapi.Client.
func (c *Client) Manifest(ctx context.Context, h agentapi.Handle) (agentapi.Manifest, error) {
	req := struct {
		Handle agentapi.Handle `json:"handle"`
	}{Handle: h}
	var res agentapi.Manifest
	err := c.do(ctx, "/api/v1/agent/manifest", req, &res)
	return res, err
}

// Invoke implements agentapi.Client. denied, waiting and rejected
// outcomes arrive here in res with a nil error, exactly as the
// in-process client returns them (ADR-W039 §4 item 5): only a
// transport or protocol failure is an error.
func (c *Client) Invoke(ctx context.Context, req agentapi.InvokeRequest) (agentapi.InvokeResult, error) {
	var res agentapi.InvokeResult
	err := c.do(ctx, "/api/v1/agent/invoke", req, &res)
	return res, err
}

// Recall implements agentapi.Client.
func (c *Client) Recall(ctx context.Context, req agentapi.RecallRequest) ([]agentapi.MemoryHit, error) {
	var res []agentapi.MemoryHit
	err := c.do(ctx, "/api/v1/agent/recall", req, &res)
	return res, err
}

// Remember implements agentapi.Client.
func (c *Client) Remember(ctx context.Context, req agentapi.RememberRequest) error {
	return c.do(ctx, "/api/v1/agent/remember", req, nil)
}

// Checkpoint implements agentapi.Client.
func (c *Client) Checkpoint(ctx context.Context, cp agentapi.Checkpoint) (agentapi.CheckpointReceipt, error) {
	var res agentapi.CheckpointReceipt
	err := c.do(ctx, "/api/v1/agent/checkpoint", cp, &res)
	return res, err
}

// Start implements agentapi.Client.
func (c *Client) Start(ctx context.Context, w agentapi.WorkStart) error {
	return c.do(ctx, "/api/v1/agent/start", w, nil)
}

// Wait implements agentapi.Client.
func (c *Client) Wait(ctx context.Context, w agentapi.WorkWait) error {
	return c.do(ctx, "/api/v1/agent/wait", w, nil)
}

// Complete implements agentapi.Client.
func (c *Client) Complete(ctx context.Context, w agentapi.WorkComplete) error {
	return c.do(ctx, "/api/v1/agent/complete", w, nil)
}

// Fail implements agentapi.Client.
func (c *Client) Fail(ctx context.Context, w agentapi.WorkFail) error {
	return c.do(ctx, "/api/v1/agent/fail", w, nil)
}

// Propose implements agentapi.Client.
func (c *Client) Propose(ctx context.Context, req agentapi.ProposeRequest) (agentapi.ProposalRef, error) {
	var res agentapi.ProposalRef
	err := c.do(ctx, "/api/v1/agent/propose", req, &res)
	return res, err
}

// Notify implements agentapi.Client.
func (c *Client) Notify(ctx context.Context, req agentapi.NotifyRequest) error {
	return c.do(ctx, "/api/v1/agent/notify", req, nil)
}

// Heartbeat implements agentapi.Client. It carries no handle, matching
// the in-process client's own signature (a runtime reports for itself,
// not for a task).
func (c *Client) Heartbeat(ctx context.Context, hb agentapi.Heartbeat) error {
	return c.do(ctx, "/api/v1/agent/heartbeat", hb, nil)
}

var _ agentapi.DeliveryClient = (*Client)(nil)

// deliveriesBody is GET /api/v1/agent/deliveries' own response shape.
type deliveriesBody struct {
	Deliveries []agentapi.Delivery `json:"deliveries"`
}

// Deliveries implements agentapi.DeliveryClient: GET
// /api/v1/agent/deliveries?wait=<duration>&after=<id> (ADR-W039 §3.6,
// §4.1 "pull fallback"; Phase 4, #341). wait travels as a Go duration
// string, which the server parses the same way; Keystone caps it
// server-side, so a Client here need not know the cap to behave
// correctly.
func (c *Client) Deliveries(ctx context.Context, after string, wait time.Duration) ([]agentapi.Delivery, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if wait > 0 {
		q.Set("wait", wait.String())
	}
	path := "/api/v1/agent/deliveries"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var res deliveriesBody
	if err := c.getJSON(ctx, path, &res); err != nil {
		return nil, err
	}
	if res.Deliveries == nil {
		res.Deliveries = []agentapi.Delivery{}
	}
	return res.Deliveries, nil
}

// Ack implements agentapi.DeliveryClient: POST
// /api/v1/agent/deliveries/{id}/ack.
func (c *Client) Ack(ctx context.Context, id string) error {
	return c.do(ctx, "/api/v1/agent/deliveries/"+url.PathEscape(id)+"/ack", nil, nil)
}

// getJSON is do's GET sibling — every other Agent API route is a POST
// with the handle inside the body (ADR-W040 §7's "no handle in a URL,
// a log line or an audit row"), but the deliveries long-poll names no
// handle at all, and GET is what a long-poll with query parameters
// (wait, after) actually is.
func (c *Client) getJSON(ctx context.Context, path string, respBody any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return fmt.Errorf("httpclient: building the request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Bearer)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("httpclient: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("httpclient: reading the response from %s: %w", path, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if respBody == nil || len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, respBody); err != nil {
			return fmt.Errorf("httpclient: decoding the response from %s: %w", path, err)
		}
		return nil
	case http.StatusForbidden:
		return &RefusedError{Message: errorMessage(raw)}
	default:
		return &StatusError{Status: resp.StatusCode, Message: errorMessage(raw)}
	}
}
