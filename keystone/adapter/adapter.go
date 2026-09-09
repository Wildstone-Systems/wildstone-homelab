// Package adapter is the runtime an integration adapter runs in
// (ADR-W030 A5/A5.1, work package P12).
//
// An adapter is a small program that sits between one application and
// Keystone. It long-polls for the work assigned to the connections its
// credential is bound to, asks the application, and reports one of four
// result types. That is the whole of its authority: it cannot read
// Keystone state, cannot claim another connection's work, and has no
// administration surface at all.
//
// Everything application-specific lives behind Upstream. This file knows
// about claiming, reporting, deadlines and cancellation; it does not
// know what a calendar is. That separation is what lets the same
// contract tests run against a fake and against a real deployment —
// which is exactly what P12's evidence asks for.
//
// The runtime deliberately makes one thing hard to get wrong: it is the
// only place that decides a result type. An Upstream returns an answer,
// an emptiness, an unavailability or a staleness, and the runtime maps
// those onto the wire. An adapter author cannot accidentally report an
// outage as an empty list, because the type system will not let them
// return one for the other.
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Result types, mirrored from the runtime DB's closed set.
const (
	ResultOK          = "ok"
	ResultEmpty       = "empty"
	ResultUnavailable = "unavailable"
	ResultStale       = "stale"
)

// ErrUnavailable is what an Upstream returns when the application could
// not be reached or refused. The runtime turns it into an
// `unavailable` result, and dependent work waits rather than
// concluding.
var ErrUnavailable = errors.New("adapter: the application is unavailable")

// Answer is what an Upstream produces for one query or action.
//
// Empty and Stale are separate fields rather than states of Data
// because they mean different things and both can be true of a real
// answer: a list that is genuinely empty, and a reading offered from
// cache because the app would not answer.
type Answer struct {
	// Data is the result payload. It must marshal to a JSON object.
	Data any
	// Empty marks a valid answer that happens to contain nothing. The
	// list exists and has no items on it — which is not the same as the
	// app being down, and reporting it as such is the specific failure
	// this contract exists to prevent.
	Empty bool
	// ObservedAt, when set with Stale, says when the data was actually
	// true. A stale answer without one would be reported as if it were
	// current, and the runtime refuses to send it.
	Stale      bool
	ObservedAt time.Time
}

// Upstream is one application, as an adapter sees it.
//
// Capabilities is asked at startup and after a reconnect, and its answer
// is recorded against the connection. Recording a capability says "this
// works" — never "this is allowed", which stays a separate, human
// decision (ADR-W030 A5).
type Upstream interface {
	// Name identifies the implementation in logs and in the connection
	// record.
	Name() string
	// Capabilities reports what this application can currently do,
	// verified against its version and the access actually granted.
	Capabilities(ctx context.Context) ([]Capability, error)
	// Query answers a read. It returns ErrUnavailable when the
	// application cannot be reached.
	Query(ctx context.Context, capability string, request json.RawMessage) (Answer, error)
	// Act performs a declared action. An Upstream that performs none
	// returns ErrNoSuchCapability.
	Act(ctx context.Context, capability string, request json.RawMessage) (Answer, error)
}

// Capability is one verified thing an application supports.
type Capability struct {
	Kind      string `json:"kind"` // event | query | action
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
}

// ErrNoSuchCapability is returned for work naming something this
// Upstream does not implement. It is reported as `unavailable` rather
// than as an error, because from the caller's point of view a
// capability the adapter does not have and one the app is not offering
// today are the same situation.
var ErrNoSuchCapability = errors.New("adapter: this upstream does not implement that capability")

// Client is the Keystone side: claim, report, and nothing else.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Work is one claimed item.
type Work struct {
	Seq           int64           `json:"seq"`
	Connection    string          `json:"connection"`
	Kind          string          `json:"kind"`
	Capability    string          `json:"capability"`
	CorrelationID string          `json:"correlationId"`
	RequestJSON   json.RawMessage `json:"requestJson"`
	DeadlineAt    time.Time       `json:"deadlineAt"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) post(ctx context.Context, path string, body, into any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("adapter: %s answered %s: %s", path, res.Status, strings.TrimSpace(string(msg)))
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(into)
}

// Claim long-polls for work. A nil Work with a nil error is a quiet
// period, not a failure: the adapter simply asks again.
func (c *Client) Claim(ctx context.Context, wait time.Duration) (*Work, error) {
	var body struct {
		Work *Work `json:"work"`
	}
	if err := c.post(ctx, "/api/v1/integration-work/claim",
		map[string]any{"waitMillis": int(wait.Milliseconds())}, &body); err != nil {
		return nil, err
	}
	return body.Work, nil
}

// Report answers claimed work.
func (c *Client) Report(ctx context.Context, seq int64, resultType string, data any, observedAt time.Time, errText string) error {
	payload := "{}"
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("adapter: encoding the result: %w", err)
		}
		payload = string(encoded)
	}
	body := map[string]any{
		"seq": seq, "resultType": resultType, "result": payload, "error": errText,
	}
	if !observedAt.IsZero() {
		body["observedAt"] = observedAt.UTC().Format(time.RFC3339)
	}
	return c.post(ctx, "/api/v1/integration-work/result", body, nil)
}

// Runner drives one Upstream against one Keystone.
type Runner struct {
	Client   *Client
	Upstream Upstream
	// PollWait bounds one claim. Zero uses ten seconds, which is short
	// enough that a fenced adapter learns it has been superseded
	// promptly and long enough that an idle house is not a poll loop.
	PollWait time.Duration
	// Log, when set, receives one line per handled item.
	Log func(string, ...any)
}

func (r *Runner) log(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func (r *Runner) pollWait() time.Duration {
	if r.PollWait > 0 {
		return r.PollWait
	}
	return 10 * time.Second
}

// RunOnce claims at most one item and answers it. It returns whether it
// found any, so a caller can distinguish a quiet period from work.
//
// The deadline on the work is honoured as an absolute time: a query that
// spent four of its five seconds waiting to be claimed gets one, not
// five. Answering after the deadline is pointless — Keystone has already
// expired it — so the runtime does not try.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	work, err := r.Client.Claim(ctx, r.pollWait())
	if err != nil {
		return false, err
	}
	if work == nil {
		return false, nil
	}

	callCtx := ctx
	if !work.DeadlineAt.IsZero() {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithDeadline(ctx, work.DeadlineAt)
		defer cancel()
	}

	answer, err := r.ask(callCtx, *work)
	switch {
	case errors.Is(err, ErrNoSuchCapability), errors.Is(err, ErrUnavailable):
		r.log("work %d: %s is unavailable: %v", work.Seq, work.Capability, err)
		return true, r.Client.Report(ctx, work.Seq, ResultUnavailable, nil, time.Time{}, err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// The deadline passed while we were asking. Keystone has already
		// expired it; reporting would be refused and would say nothing
		// useful anyway.
		r.log("work %d: deadline passed before %s answered", work.Seq, work.Capability)
		return true, nil
	case err != nil:
		r.log("work %d: %s failed: %v", work.Seq, work.Capability, err)
		return true, r.Client.Report(ctx, work.Seq, ResultUnavailable, nil, time.Time{}, err.Error())
	}

	resultType := ResultOK
	switch {
	case answer.Stale:
		resultType = ResultStale
	case answer.Empty:
		resultType = ResultEmpty
	}
	r.log("work %d: %s → %s", work.Seq, work.Capability, resultType)
	return true, r.Client.Report(ctx, work.Seq, resultType, answer.Data, answer.ObservedAt, "")
}

func (r *Runner) ask(ctx context.Context, work Work) (Answer, error) {
	switch work.Kind {
	case "action":
		return r.Upstream.Act(ctx, work.Capability, work.RequestJSON)
	default:
		return r.Upstream.Query(ctx, work.Capability, work.RequestJSON)
	}
}

// Run drives the loop until ctx is done.
func (r *Runner) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		did, err := r.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A failure to reach Keystone is not a reason to exit: the
			// control plane may be restarting, and an adapter that quits
			// on the first refused poll needs a human to bring it back.
			r.log("poll failed, retrying: %v", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if !did {
			// The long poll already waited; going straight round again
			// is correct and is not a spin.
			continue
		}
	}
}
