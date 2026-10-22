// Package mail is the read-only mailbox adapter's Upstream (epic #326):
// the part of wild-mail-adapter that knows what mail is, sitting on the
// generic adapter.Runner that knows how work is claimed and fenced.
//
// It is deliberately split from the IMAP client. This package owns the
// CONTRACT — the closed, read-only operation set, strict and bounded
// request decoding, and the distinct not-configured and
// authentication-failed outcomes. Reading a mailbox is a Reader's job,
// and a Reader is plugged in: until one is (and until a Person has
// onboarded a credential) the adapter answers every operation with an
// explicit not_configured unavailability and touches no mailbox at all.
//
// READ-ONLY BY CONSTRUCTION. There is no action. Act refuses everything,
// Query dispatches through a fixed table of the four queries the mail
// domain declares (keystone/integration), and the Reader interface has
// no method a write could hide behind. A mutating name — mail.send,
// mail.move, mail.mark_read — is not refused by a check that could be
// forgotten; there is nowhere for it to go.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter"
)

// The operations, exactly the mail domain's queries.
const (
	CapCapabilities = "mail.capabilities"
	CapChanges      = "mail.changes"
	CapMessage      = "mail.message"
	CapSearch       = "mail.search"
)

// Operations is the whole dispatchable surface, in declaration order.
var Operations = []string{CapCapabilities, CapChanges, CapMessage, CapSearch}

// Bounds every request is held to before a Reader sees it.
const (
	// MaxBatch caps one mail.changes or mail.search answer.
	MaxBatch = 50
	// MaxFolders caps the folders one search may name.
	MaxFolders = 20
	// MaxFolderLen and MaxRefLen bound opaque strings.
	MaxFolderLen = 255
	MaxRefLen    = 512
	// MaxFrom caps sender filters; MaxSubjectLen the subject filter.
	MaxFrom       = 20
	MaxSubjectLen = 200
	// MaxRequestBytes caps the request envelope itself.
	MaxRequestBytes = 16 << 10
	// DefaultFolder is the folder a connection reads when its scope
	// names none.
	DefaultFolder = "INBOX"
	// MaxSearchDays bounds mail.search's date window;
	// DefaultSearchDays is the window when since is left out.
	MaxSearchDays     = 366
	DefaultSearchDays = 30
)

// Outcome codes. The wire's result types are closed
// (ok|empty|unavailable|stale), and these say WHICH unavailability,
// because "not set up", "wrong password" and "provider down" ask three
// different people to do three different things (epic #326: empty,
// unavailable, stale and authentication-failed are distinct outcomes).
//
// An unavailable result's error text is always
//
//	adapter: the application is unavailable: <outcome>[: <reason>]
//
// where <outcome> is one of the codes below and <reason> a fixed
// category (timeout, tls, connection, folder_missing, uidvalidity, …),
// never provider text. An unavailable result carries no data, so a
// reader never uses one for something the caller must commit.
const (
	// OutcomeNotConfigured: no reader, no credential or no connection key.
	OutcomeNotConfigured = "not_configured"
	// OutcomeAuthFailed: the provider refused the credential.
	OutcomeAuthFailed = "auth_failed"
	// OutcomeBadRequest: the request failed decoding or bounds, or names
	// a folder or reference outside this connection.
	OutcomeBadRequest = "bad_request"
	// OutcomeProviderUnavailable: the provider could not be reached, or
	// broke mid-call. Retry from the same cursor.
	OutcomeProviderUnavailable = "provider_unavailable"
	// OutcomeStale: mail.message named a reference taken under an
	// earlier UIDVALIDITY. The message may still exist under a new UID;
	// mail.changes returns it again (same fingerprint) after its
	// cursor_reset. Carried as unavailable, not as the wire's stale
	// result type: stale means "old data, observed at T", and here there
	// is no data at all.
	OutcomeStale = "stale"
)

// Data-level outcomes. These travel INSIDE an ok or empty result's data
// (its "outcome" field), because they carry something the caller must
// act on and an unavailable result carries nothing.
const (
	// OutcomeOK and OutcomeEmpty mirror the result type.
	OutcomeOK    = "ok"
	OutcomeEmpty = "empty"
	// OutcomeCursorReset: mail.changes found at least one folder whose
	// UIDVALIDITY changed. Reported with result type ok (never empty,
	// never unavailable) so the new cursor reaches the caller: commit
	// "next" and call again. Nothing was skipped, and messages already
	// processed come back with the same fingerprint.
	OutcomeCursorReset = "cursor_reset"
)

// Unavailable builds the error for one unavailability outcome.
func Unavailable(outcome, reason string) error {
	if reason == "" {
		return fmt.Errorf("%w: %s", adapter.ErrUnavailable, outcome)
	}
	return fmt.Errorf("%w: %s: %s", adapter.ErrUnavailable, outcome, reason)
}

// ErrNotConfigured means this adapter has no mailbox to read: no reader
// is plugged in, or no credential has been onboarded for its connection.
// It is an unavailability, never an empty answer.
var ErrNotConfigured = fmt.Errorf("%w: %s", adapter.ErrUnavailable, OutcomeNotConfigured)

// ErrAuthFailed is what a Reader returns when the provider refused the
// credential. Distinct from the provider being unreachable.
var ErrAuthFailed = fmt.Errorf("%w: %s", adapter.ErrUnavailable, OutcomeAuthFailed)

// ErrBadRequest wraps a request that failed decoding or bounds.
var ErrBadRequest = fmt.Errorf("%w: %s", adapter.ErrUnavailable, OutcomeBadRequest)

// Settings is what the adapter inherits from the installed mail package
// and its placement. None of it is a credential.
type Settings struct {
	IMAPHost       string
	IMAPPort       string
	IMAPEncryption string // ssl | tls
	UsernameDomain string
	Connection     string
	Node           string
	Package        string
	PackageVersion string
}

// Credentials is one Person's mailbox login, read from the node secret
// path into this process's environment. It never leaves the process:
// String and the JSON encoding redact the password.
type Credentials struct {
	Username string
	Password string
}

// String never prints the password.
func (c Credentials) String() string { return "Credentials{" + c.Username + ", [redacted]}" }

// MarshalJSON never encodes the password.
func (c Credentials) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"username": c.Username, "password": "[redacted]"})
}

// Present reports whether a login was onboarded at all.
func (c Credentials) Present() bool { return c.Username != "" && c.Password != "" }

// FolderCursor is one folder's committed position: the UIDVALIDITY it
// was taken under and the last UID the caller has processed.
type FolderCursor struct {
	Folder      string `json:"folder"`
	UIDValidity uint32 `json:"uidValidity"`
	LastUID     uint32 `json:"lastUid"`
}

// Cursor is a connection's durable position across all its approved
// folders. mail.changes returns it as "next"; the caller commits it once
// the batch is processed and sends it back unchanged. A folder the
// cursor does not name starts at "now" (baseline): monitoring is for new
// mail, not an import of the archive.
type Cursor struct {
	Folders []FolderCursor `json:"folders"`
}

// ChangesRequest is mail.changes.
type ChangesRequest struct {
	// Folder narrows the read to one approved folder; empty reads every
	// approved folder. Either way the cursor's other folders are carried
	// through untouched.
	Folder string `json:"folder,omitempty"`
	// Cursor is the previous answer's "next"; absent starts from now.
	Cursor *Cursor `json:"cursor,omitempty"`
	Limit  int     `json:"limit,omitempty"`
}

// MessageRequest is mail.message.
type MessageRequest struct {
	Ref string `json:"ref"`
}

// SearchRequest is mail.search. Since and Before are YYYY-MM-DD (see
// Window for the defaults); the window is at most MaxSearchDays.
type SearchRequest struct {
	Folders []string `json:"folders,omitempty"`
	From    []string `json:"from,omitempty"`
	Subject string   `json:"subject,omitempty"`
	Since   string   `json:"since,omitempty"`
	Before  string   `json:"before,omitempty"`
	Limit   int      `json:"limit,omitempty"`
}

// FolderStatus is one approved folder's reachability.
type FolderStatus struct {
	Folder    string `json:"folder"`
	Available bool   `json:"available"`
	Messages  uint32 `json:"messages"`
}

// Probe is what a Reader found when it checked the mailbox.
type Probe struct {
	// Folders are the connection's approved folders and whether each
	// could be opened.
	Folders []FolderStatus `json:"folders,omitempty"`
	// Server identifies the provider software, when it says.
	Server string `json:"server,omitempty"`
	// MaxMessageBytes and MaxHeaderBytes are the reader's fetch bounds.
	MaxMessageBytes int64 `json:"maxMessageBytes,omitempty"`
	MaxHeaderBytes  int   `json:"maxHeaderBytes,omitempty"`
}

// Reader is a read-only mailbox. It is the seam the IMAP implementation
// (keystone/adapter/mail/imapread) plugs into; every request it receives
// has already been decoded strictly and bounded, and it is only ever
// asked to READ.
//
// Implementations return ErrNotConfigured, ErrAuthFailed, ErrBadRequest
// or Unavailable(OutcomeProviderUnavailable|OutcomeStale, reason) for
// the kinds of "could not look", and an adapter.Answer with Empty set
// for "looked and found nothing". Every answer's data carries an
// "outcome": OutcomeOK, OutcomeEmpty, or for mail.changes
// OutcomeCursorReset. They never return message bodies beyond the
// bounded, normalized text the contract allows.
type Reader interface {
	// Probe verifies the credential and lists visible folders. It is
	// what mail.capabilities reports and what readiness checks.
	Probe(ctx context.Context) (Probe, error)
	// Changes returns at most req.Limit messages after req.Cursor, and
	// the cursor to resume from.
	Changes(ctx context.Context, req ChangesRequest) (adapter.Answer, error)
	// Message returns one message by opaque reference.
	Message(ctx context.Context, req MessageRequest) (adapter.Answer, error)
	// Search runs one bounded search.
	Search(ctx context.Context, req SearchRequest) (adapter.Answer, error)
}

// NotConfigured is the Reader of an adapter with no mailbox: it reads
// nothing and says so. It is the reader a freshly placed adapter runs
// with until a credential exists and an IMAP reader is plugged in.
type NotConfigured struct {
	// Reason is appended to the outcome for the operator.
	Reason string
}

func (n NotConfigured) err() error {
	if n.Reason == "" {
		return ErrNotConfigured
	}
	return fmt.Errorf("%w: %s", ErrNotConfigured, n.Reason)
}

// Probe reports not configured.
func (n NotConfigured) Probe(context.Context) (Probe, error) { return Probe{}, n.err() }

// Changes reports not configured.
func (n NotConfigured) Changes(context.Context, ChangesRequest) (adapter.Answer, error) {
	return adapter.Answer{}, n.err()
}

// Message reports not configured.
func (n NotConfigured) Message(context.Context, MessageRequest) (adapter.Answer, error) {
	return adapter.Answer{}, n.err()
}

// Search reports not configured.
func (n NotConfigured) Search(context.Context, SearchRequest) (adapter.Answer, error) {
	return adapter.Answer{}, n.err()
}

// Upstream is the mail adapter as adapter.Runner sees it.
type Upstream struct {
	Settings Settings
	// Reader reads the mailbox. nil behaves as NotConfigured.
	Reader Reader
}

var _ adapter.Upstream = (*Upstream)(nil)

func (u *Upstream) reader() Reader {
	if u.Reader == nil {
		return NotConfigured{}
	}
	return u.Reader
}

// Name identifies the implementation.
func (u *Upstream) Name() string { return "wild-mail-adapter" }

// Capabilities reports the four read queries, available only when the
// reader can actually open the mailbox. Found is not enabled: this is a
// report, and the package declaration stays the ceiling (ADR-W030 A5).
func (u *Upstream) Capabilities(ctx context.Context) ([]adapter.Capability, error) {
	_, err := u.reader().Probe(ctx)
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	out := make([]adapter.Capability, 0, len(Operations))
	for _, op := range Operations {
		out = append(out, adapter.Capability{Kind: "query", Name: op, Version: u.Settings.PackageVersion,
			Available: err == nil, Detail: detail})
	}
	return out, nil
}

// Query dispatches one of the four reads. Anything else — including
// every mutating mail name — is ErrNoSuchCapability.
func (u *Upstream) Query(ctx context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	r := u.reader()
	switch capability {
	case CapCapabilities:
		if err := decodeStrict(request, &struct{}{}); err != nil {
			return adapter.Answer{}, err
		}
		probe, err := r.Probe(ctx)
		if err != nil {
			return adapter.Answer{}, err
		}
		return adapter.Answer{Data: map[string]any{
			"outcome":    OutcomeOK,
			"readOnly":   true,
			"operations": Operations,
			"folders":    probe.Folders,
			"server":     probe.Server,
			"limits": map[string]int64{
				"batch": MaxBatch, "folders": MaxFolders, "from": MaxFrom, "searchDays": MaxSearchDays,
				"messageBytes": probe.MaxMessageBytes, "headerBytes": int64(probe.MaxHeaderBytes),
			},
		}}, nil
	case CapChanges:
		var req ChangesRequest
		if err := decodeStrict(request, &req); err != nil {
			return adapter.Answer{}, err
		}
		if err := req.normalize(); err != nil {
			return adapter.Answer{}, err
		}
		return r.Changes(ctx, req)
	case CapMessage:
		var req MessageRequest
		if err := decodeStrict(request, &req); err != nil {
			return adapter.Answer{}, err
		}
		if err := checkOpaque("ref", req.Ref, MaxRefLen, true); err != nil {
			return adapter.Answer{}, err
		}
		return r.Message(ctx, req)
	case CapSearch:
		var req SearchRequest
		if err := decodeStrict(request, &req); err != nil {
			return adapter.Answer{}, err
		}
		if err := req.normalize(); err != nil {
			return adapter.Answer{}, err
		}
		return r.Search(ctx, req)
	}
	return adapter.Answer{}, fmt.Errorf("%w: %q is not a read this adapter offers", adapter.ErrNoSuchCapability, capability)
}

// Act refuses everything. The mail surface has no action to perform, and
// no configuration changes that.
func (u *Upstream) Act(context.Context, string, json.RawMessage) (adapter.Answer, error) {
	return adapter.Answer{}, fmt.Errorf("%w: the mailbox adapter is read-only", adapter.ErrNoSuchCapability)
}

func decodeStrict(raw json.RawMessage, into any) error {
	if len(raw) > MaxRequestBytes {
		return fmt.Errorf("%w: request is larger than %d bytes", ErrBadRequest, MaxRequestBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after the request", ErrBadRequest)
	}
	return nil
}

func checkOpaque(field, v string, max int, required bool) error {
	if v == "" {
		if required {
			return fmt.Errorf("%w: %s is required", ErrBadRequest, field)
		}
		return nil
	}
	if len(v) > max {
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrBadRequest, field, max)
	}
	if strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: %s contains a control character", ErrBadRequest, field)
	}
	return nil
}

func checkLimit(limit *int) error {
	switch {
	case *limit == 0:
		*limit = MaxBatch
	case *limit < 0 || *limit > MaxBatch:
		return fmt.Errorf("%w: limit must be 1–%d", ErrBadRequest, MaxBatch)
	}
	return nil
}

func checkDate(field, v string) error {
	if v == "" {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, v); err != nil {
		return fmt.Errorf("%w: %s must be YYYY-MM-DD", ErrBadRequest, field)
	}
	return nil
}

func (r *ChangesRequest) normalize() error {
	if err := checkOpaque("folder", r.Folder, MaxFolderLen, false); err != nil {
		return err
	}
	if r.Cursor != nil {
		if len(r.Cursor.Folders) > MaxFolders {
			return fmt.Errorf("%w: a cursor names at most %d folders", ErrBadRequest, MaxFolders)
		}
		seen := map[string]bool{}
		for _, fc := range r.Cursor.Folders {
			if err := checkOpaque("cursor folder", fc.Folder, MaxFolderLen, true); err != nil {
				return err
			}
			if seen[fc.Folder] {
				return fmt.Errorf("%w: the cursor names folder %q twice", ErrBadRequest, fc.Folder)
			}
			seen[fc.Folder] = true
		}
	}
	return checkLimit(&r.Limit)
}

// normalize bounds a search. No folders means every approved folder;
// the reader, which knows the connection's scope, refuses one outside it.
func (r *SearchRequest) normalize() error {
	if len(r.Folders) > MaxFolders {
		return fmt.Errorf("%w: at most %d folders", ErrBadRequest, MaxFolders)
	}
	for _, f := range r.Folders {
		if err := checkOpaque("folder", f, MaxFolderLen, true); err != nil {
			return err
		}
	}
	if len(r.From) > MaxFrom {
		return fmt.Errorf("%w: at most %d senders", ErrBadRequest, MaxFrom)
	}
	for _, f := range r.From {
		if err := checkOpaque("from", f, MaxFolderLen, true); err != nil {
			return err
		}
	}
	if err := checkOpaque("subject", r.Subject, MaxSubjectLen, false); err != nil {
		return err
	}
	if err := checkDate("since", r.Since); err != nil {
		return err
	}
	if err := checkDate("before", r.Before); err != nil {
		return err
	}
	if r.Since != "" && r.Before != "" {
		since, before := r.Window(time.Time{})
		if !before.After(since) || before.Sub(since) > MaxSearchDays*24*time.Hour {
			return fmt.Errorf("%w: since must be before before, at most %d days apart", ErrBadRequest, MaxSearchDays)
		}
	}
	return checkLimit(&r.Limit)
}

// Window resolves the search's date range, as UTC midnights: Before
// defaults to the day after now (so today's mail is included), Since to
// DefaultSearchDays before Before. Call it only on a normalized request.
func (r SearchRequest) Window(now time.Time) (since, before time.Time) {
	now = now.UTC()
	before = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	if r.Before != "" {
		before, _ = time.Parse(time.DateOnly, r.Before)
	}
	since = before.AddDate(0, 0, -DefaultSearchDays)
	if r.Since != "" {
		since, _ = time.Parse(time.DateOnly, r.Since)
	}
	return since, before
}
