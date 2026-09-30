package imapread

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
)

// MailReader is a Reader as the adapter's mail.Upstream sees it: the one
// place an imapread Outcome becomes an adapter result. The mapping is
// the mail contract's (keystone/adapter/mail):
//
//	ok            → ok,          data.outcome "ok"
//	empty         → empty,       data.outcome "empty" (data still carries
//	                             mail.changes' "next", e.g. a baseline)
//	cursor_reset  → ok,          data.outcome "cursor_reset" — never
//	                             unavailable, because the caller must
//	                             commit the "next" it carries
//	auth_failed   → unavailable, "auth_failed: <reason>"
//	unavailable   → unavailable, "provider_unavailable: <reason>"
//	stale         → unavailable, "stale: <reason>" (mail.message only)
//	ErrBadRequest,
//	ErrBadRef     → unavailable, "bad_request: …"
//
// Requests arrive already decoded and bounded by mail.Upstream; the
// Reader re-checks scope, because only it knows the connection's folders.
type MailReader struct {
	R *Reader
	// Now resolves mail.search's default window; nil is time.Now.
	Now func() time.Time
}

var _ mail.Reader = MailReader{}

// NewMailReader adapts r.
func NewMailReader(r *Reader) MailReader { return MailReader{R: r} }

func (m MailReader) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// outcomeErr is nil for an outcome that is an answer, and the mail
// contract's error for one that is not.
func outcomeErr(o Outcome, reason string) error {
	switch o {
	case OutcomeOK, OutcomeEmpty, OutcomeCursorReset:
		return nil
	case OutcomeAuthFailed:
		return fmt.Errorf("%w: %s", mail.ErrAuthFailed, reason)
	case OutcomeStale:
		return mail.Unavailable(mail.OutcomeStale, reason)
	}
	return mail.Unavailable(mail.OutcomeProviderUnavailable, reason)
}

func answer(o Outcome, data any) adapter.Answer {
	return adapter.Answer{Data: data, Empty: o == OutcomeEmpty}
}

func badRequest(err error) error {
	switch {
	case errors.Is(err, ErrBadRef):
		return fmt.Errorf("%w: the reference was not issued for this connection", mail.ErrBadRequest)
	case errors.Is(err, ErrBadRequest):
		return fmt.Errorf("%w: %v", mail.ErrBadRequest, err)
	}
	return err
}

// Probe is mail.capabilities: log in and open every approved folder.
func (m MailReader) Probe(ctx context.Context) (mail.Probe, error) {
	res := m.R.Capabilities(ctx)
	if err := outcomeErr(res.Outcome, res.Reason); err != nil {
		return mail.Probe{}, err
	}
	return mail.Probe{Folders: res.Folders, MaxMessageBytes: res.Limits.MaxMessageBytes,
		MaxHeaderBytes: res.Limits.MaxHeaderBytes}, nil
}

// Changes is mail.changes.
func (m MailReader) Changes(ctx context.Context, req mail.ChangesRequest) (adapter.Answer, error) {
	var cur Cursor
	if req.Cursor != nil {
		cur = *req.Cursor
	}
	var res ChangesResult
	if req.Folder == "" {
		res = m.R.Changes(ctx, cur, req.Limit)
	} else {
		var err error
		if res, err = m.R.ChangesFolder(ctx, req.Folder, cur, req.Limit); err != nil {
			return adapter.Answer{}, badRequest(err)
		}
	}
	if err := outcomeErr(res.Outcome, res.Reason); err != nil {
		return adapter.Answer{}, err
	}
	return answer(res.Outcome, res), nil
}

// Message is mail.message.
func (m MailReader) Message(ctx context.Context, req mail.MessageRequest) (adapter.Answer, error) {
	res, err := m.R.Message(ctx, req.Ref)
	if err != nil {
		return adapter.Answer{}, badRequest(err)
	}
	if err := outcomeErr(res.Outcome, res.Reason); err != nil {
		return adapter.Answer{}, err
	}
	return answer(res.Outcome, res), nil
}

// Search is mail.search.
func (m MailReader) Search(ctx context.Context, req mail.SearchRequest) (adapter.Answer, error) {
	since, before := req.Window(m.now())
	res, err := m.R.Search(ctx, SearchRequest{Folders: req.Folders, Since: since, Before: before,
		From: req.From, Subject: req.Subject, Limit: req.Limit})
	if err != nil {
		return adapter.Answer{}, badRequest(err)
	}
	if err := outcomeErr(res.Outcome, res.Reason); err != nil {
		return adapter.Answer{}, err
	}
	return answer(res.Outcome, res), nil
}
