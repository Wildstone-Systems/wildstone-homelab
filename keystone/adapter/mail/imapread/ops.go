package imapread

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/imapread/mimenorm"
)

// ErrBadRequest wraps a request the reader refuses to run. MailReader
// reports it as the mail contract's bad_request.
var ErrBadRequest = errors.New("imapread: bad request")

// MessageResult is mail.message's answer.
type MessageResult struct {
	Outcome Outcome  `json:"outcome"`
	Reason  string   `json:"reason,omitempty"`
	Message *Message `json:"message,omitempty"`
}

// SearchRequest is mail.search's request. It is always bounded: a date
// window of at most MaxSearchWindow, at most MaxBatch results, only
// inside the connection's folders and filters.
type SearchRequest struct {
	// Folders narrows the connection's folders; empty means all of them.
	Folders []string  `json:"folders,omitempty"`
	Since   time.Time `json:"since"`
	// Before defaults to now.
	Before time.Time `json:"before,omitempty"`
	// From matches when the sender's address or name contains any
	// entry; Subject when the subject contains it. Both are
	// case-insensitive.
	From    []string `json:"from,omitempty"`
	Subject string   `json:"subject,omitempty"`
	Limit   int      `json:"limit,omitempty"`
}

// Summary is one search hit: headers and identity, no body.
type Summary struct {
	Ref           string           `json:"ref"`
	Fingerprint   string           `json:"fingerprint"`
	Folder        string           `json:"folder"`
	Size          int64            `json:"size"`
	HeaderOutcome mimenorm.Outcome `json:"headerOutcome"`
	Headers       mimenorm.Headers `json:"headers"`
}

// SearchResult is mail.search's answer, newest first per folder.
type SearchResult struct {
	Outcome   Outcome   `json:"outcome"`
	Reason    string    `json:"reason,omitempty"`
	Results   []Summary `json:"results,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
}

// FolderStatus is one folder's reachability (the mail contract's type).
type FolderStatus = mail.FolderStatus

// CapabilitiesResult is mail.capabilities's answer.
type CapabilitiesResult struct {
	Outcome    Outcome        `json:"outcome"`
	Reason     string         `json:"reason,omitempty"`
	ReadOnly   bool           `json:"readOnly"`
	Operations []string       `json:"operations"`
	Folders    []FolderStatus `json:"folders,omitempty"`
	Limits     struct {
		MaxBatch        int   `json:"maxBatch"`
		MaxMessageBytes int64 `json:"maxMessageBytes"`
		MaxHeaderBytes  int   `json:"maxHeaderBytes"`
	} `json:"limits"`
}

// Capabilities verifies the credential and each folder.
func (r *Reader) Capabilities(ctx context.Context) CapabilitiesResult {
	res := CapabilitiesResult{ReadOnly: true, Operations: append([]string(nil), mail.Operations...)}
	res.Limits.MaxBatch = MaxBatch
	res.Limits.MaxMessageBytes = r.limits.MaxMessageBytes
	res.Limits.MaxHeaderBytes = r.limits.MaxHeaderBytes
	s, _, cancel, err := r.open(ctx)
	if err != nil {
		res.Outcome, res.Reason = failure(err)
		return res
	}
	defer cancel()
	defer s.logout()
	available := 0
	for _, folder := range r.cfg.Filters.folders() {
		st, err := s.examine(folder)
		switch {
		case errors.Is(err, errFolderMissing):
			res.Folders = append(res.Folders, FolderStatus{Folder: folder})
			continue
		case err != nil:
			res.Folders = nil
			res.Outcome, res.Reason = failure(err)
			return res
		}
		available++
		res.Folders = append(res.Folders, FolderStatus{Folder: folder, Available: true, Messages: st.Messages})
	}
	if available == 0 {
		res.Outcome, res.Reason = OutcomeUnavailable, "folder_missing"
		return res
	}
	res.Outcome = OutcomeOK
	return res
}

// Message returns one message by reference. A reference this
// connection did not issue, or for a folder outside its scope, is
// ErrBadRef. A message that is gone — or no longer inside the filters —
// is empty (the two are deliberately indistinguishable). A reference
// from an earlier UIDVALIDITY is stale.
func (r *Reader) Message(ctx context.Context, reference string) (MessageResult, error) {
	ref, err := r.keys.decodeRef(reference)
	if err != nil {
		return MessageResult{}, err
	}
	if !r.cfg.Filters.hasFolder(ref.Folder) {
		return MessageResult{}, ErrBadRef
	}
	s, _, cancel, err := r.open(ctx)
	if err != nil {
		o, reason := failure(err)
		return MessageResult{Outcome: o, Reason: reason}, nil
	}
	defer cancel()
	defer s.logout()
	st, err := s.examine(ref.Folder)
	if errors.Is(err, errFolderMissing) {
		return MessageResult{Outcome: OutcomeEmpty}, nil
	}
	if err != nil {
		o, reason := failure(err)
		return MessageResult{Outcome: o, Reason: reason}, nil
	}
	if st.UIDValidity != ref.UIDValidity {
		return MessageResult{Outcome: OutcomeStale, Reason: "uidvalidity"}, nil
	}
	headers, err := s.fetchHeaders([]uint32{ref.UID}, r.limits.MaxHeaderBytes)
	if err != nil {
		o, reason := failure(err)
		return MessageResult{Outcome: o, Reason: reason}, nil
	}
	h, ok := headers[ref.UID]
	if !ok {
		return MessageResult{Outcome: OutcomeEmpty}, nil
	}
	msg, status, err := r.read(s, ref.Folder, ref.UIDValidity, ref.UID, h)
	if err != nil {
		o, reason := failure(err)
		return MessageResult{Outcome: o, Reason: reason}, nil
	}
	if status != readInScope {
		return MessageResult{Outcome: OutcomeEmpty}, nil
	}
	return MessageResult{Outcome: OutcomeOK, Message: msg}, nil
}

func (req *SearchRequest) validate(f Filters, now time.Time) error {
	if req.Since.IsZero() {
		return fmt.Errorf("%w: since is required", ErrBadRequest)
	}
	if req.Before.IsZero() {
		req.Before = now
	}
	if !req.Before.After(req.Since) || req.Before.Sub(req.Since) > MaxSearchWindow {
		return fmt.Errorf("%w: the window must be positive and at most %s", ErrBadRequest, MaxSearchWindow)
	}
	if len(req.From) > maxFilterEntries {
		return fmt.Errorf("%w: at most %d senders", ErrBadRequest, maxFilterEntries)
	}
	for _, term := range append([]string{req.Subject}, req.From...) {
		if len(term) > maxFilterEntry || strings.ContainsAny(term, "\r\n\x00") {
			return fmt.Errorf("%w: search terms too long or not single-line", ErrBadRequest)
		}
	}
	if req.Limit <= 0 {
		req.Limit = 20
	}
	if req.Limit > MaxBatch {
		req.Limit = MaxBatch
	}
	for _, folder := range req.Folders {
		if !f.hasFolder(folder) {
			return fmt.Errorf("%w: folder %q is outside this connection's scope", ErrBadRequest, folder)
		}
	}
	if len(req.Folders) == 0 {
		req.Folders = f.folders()
	}
	return nil
}

// Search runs a bounded search inside the connection's scope and
// returns header summaries only.
func (r *Reader) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	if err := req.validate(r.cfg.Filters, time.Now()); err != nil {
		return SearchResult{}, err
	}
	s, _, cancel, err := r.open(ctx)
	if err != nil {
		o, reason := failure(err)
		return SearchResult{Outcome: o, Reason: reason}, nil
	}
	defer cancel()
	defer s.logout()

	var res SearchResult
	remaining := req.Limit
	for _, folder := range req.Folders {
		if remaining == 0 {
			res.Truncated = true
			break
		}
		st, err := s.examine(folder)
		if errors.Is(err, errFolderMissing) {
			continue
		}
		if err != nil {
			o, reason := failure(err)
			return SearchResult{Outcome: o, Reason: reason}, nil
		}
		q := searchQuery{Since: req.Since, Before: req.Before, Subject: req.Subject}
		if len(req.From) == 1 {
			// One sender can be a server-side prefilter; several are
			// matched here only (an IMAP OR tree is not worth the
			// provider variance), inside the same bounded budget.
			q.From = req.From[0]
		}
		uids, err := s.searchUIDs(q)
		if err != nil {
			o, reason := failure(err)
			return SearchResult{Outcome: o, Reason: reason}, nil
		}
		// Newest first. Headers are fetched in slices of the budget so a
		// folder where most hits fall outside the filters cannot force
		// an unbounded header read: at most 4x the limit is examined.
		sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
		budget := 4 * req.Limit
		if len(uids) > budget {
			uids = uids[:budget]
			res.Truncated = true
		}
		headers, err := s.fetchHeaders(uids, r.limits.MaxHeaderBytes)
		if err != nil {
			o, reason := failure(err)
			return SearchResult{Outcome: o, Reason: reason}, nil
		}
		for _, uid := range uids {
			h, ok := headers[uid]
			if !ok {
				continue
			}
			hdr, hOutcome, _ := mimenorm.NormalizeHeaders(h.Header, r.limits)
			if hOutcome != mimenorm.OK || !r.cfg.Filters.Match(hdr) || !searchMatch(req, hdr) {
				continue
			}
			if remaining == 0 {
				res.Truncated = true
				break
			}
			res.Results = append(res.Results, Summary{
				Ref:           r.keys.encodeRef(ref{Folder: folder, UIDValidity: st.UIDValidity, UID: uid}),
				Fingerprint:   r.keys.fingerprint(hdr, h.Size, h.Header),
				Folder:        folder,
				Size:          h.Size,
				HeaderOutcome: hOutcome,
				Headers:       hdr,
			})
			remaining--
		}
	}
	if len(res.Results) == 0 {
		res.Outcome = OutcomeEmpty
	} else {
		res.Outcome = OutcomeOK
	}
	return res, nil
}

// searchMatch re-applies the request's terms to decoded headers: the
// server's SEARCH is a prefilter whose matching rules vary by provider.
func searchMatch(req SearchRequest, h mimenorm.Headers) bool {
	if req.Subject != "" && !strings.Contains(strings.ToLower(h.Subject), strings.ToLower(req.Subject)) {
		return false
	}
	if len(req.From) == 0 {
		return true
	}
	for _, want := range req.From {
		want = strings.ToLower(strings.TrimSpace(want))
		for _, a := range h.From {
			if strings.Contains(strings.ToLower(a.Address), want) || strings.Contains(strings.ToLower(a.Name), want) {
				return true
			}
		}
	}
	return false
}
