// Package imapread is the read-only IMAP reader behind the bundled
// mail adapter (epic #326, slice S04).
//
// One Reader serves one Personal connection: one account and app
// password on the package's inherited IMAP endpoint, one source scope
// (Filters), one key. Nothing is shared between Readers, so two people
// on the same provider get separate sessions, separate cursors,
// unrelated fingerprints and references that only their own connection
// accepts.
//
// # Read-only by construction
//
// The reader opens folders with EXAMINE, finds messages with UID
// SEARCH and reads them with UID FETCH BODY.PEEK — so it never sets
// \Seen and never changes provider mail. The command surface is a
// closed interface in session.go; see the tests there and the fake
// server's violation log for the proof.
//
// # Cursor semantics (at-least-once, never skip)
//
// A Cursor holds, per folder, the UIDVALIDITY it was taken under and
// the last UID the caller has COMMITTED. Changes is a pure function of
// (cursor, mailbox): it reads the next bounded batch after the cursor
// and returns the messages plus the cursor to commit once they have
// been processed. The reader keeps no state between calls. So:
//
//   - a crash before the caller commits replays the same batch — the
//     keyed fingerprint lets the caller recognise what it has seen;
//   - a dropped connection or timeout mid-batch returns `unavailable`
//     and the unchanged cursor: nothing partial is ever committed;
//   - a message expunged before it could be read is passed over (it no
//     longer exists; UIDs are never reused under one UIDVALIDITY);
//   - a UIDVALIDITY change is reported as an explicit `cursor_reset`
//     for that folder, with a cursor that restarts it from the
//     beginning of the new numbering. Nothing is silently skipped;
//     already-processed messages come back with the same fingerprint;
//   - a folder the cursor has never seen starts at "now" (baseline):
//     monitoring is for new mail, not an import of the archive.
//
// # Outcomes
//
// Empty, unavailable, stale and auth_failed are distinct, as the epic
// requires; see Outcome. Errors carry categories, never provider text,
// so no credential or server banner reaches a log or a model.
//
// # One contract
//
// The wire contract — the four operation names, request decoding and
// bounds, the Cursor, and how each Outcome becomes an adapter result —
// belongs to keystone/adapter/mail, and this package shares its types
// (Cursor, FolderCursor and FolderStatus are aliases). There is no
// second dispatcher here: MailReader adapts a Reader to mail.Reader,
// and mail.Upstream is the only thing that turns an operation name into
// a call.
package imapread

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail/imapread/mimenorm"
)

// Outcome is the closed set of call-level results. Its values are the
// mail contract's outcome codes where the two meet.
type Outcome string

const (
	// OutcomeOK: the answer has content.
	OutcomeOK Outcome = mail.OutcomeOK
	// OutcomeEmpty: the mailbox answered and there is nothing (new).
	OutcomeEmpty Outcome = mail.OutcomeEmpty
	// OutcomeUnavailable: the provider could not be reached, refused
	// for a reason other than credentials, or broke mid-call. Retry
	// later from the same cursor.
	OutcomeUnavailable Outcome = "unavailable"
	// OutcomeStale: the request names something from an older state of
	// the mailbox (a reference taken under a previous UIDVALIDITY).
	OutcomeStale Outcome = mail.OutcomeStale
	// OutcomeAuthFailed: the provider rejected the credential. Retrying
	// will not help; the Person must replace the app password.
	OutcomeAuthFailed Outcome = mail.OutcomeAuthFailed
	// OutcomeCursorReset: at least one folder's UIDVALIDITY changed.
	// The returned cursor restarts those folders; commit it and call
	// again.
	OutcomeCursorReset Outcome = mail.OutcomeCursorReset
)

// Security is the transport mode, named as the mail-roundcube package's
// imap_encryption parameter names it. There is no plaintext mode.
type Security string

const (
	// SecurityTLS is implicit TLS (imap_encryption: ssl, port 993).
	SecurityTLS Security = "ssl"
	// SecurityStartTLS is STARTTLS (imap_encryption: tls, port 143).
	SecurityStartTLS Security = "tls"
)

// Batch and run bounds (epic #326 brief).
const (
	DefaultBatch = 50
	MaxBatch     = 50
	// DefaultMaxRunBytes bounds the raw bytes one Changes call fetches.
	DefaultMaxRunBytes = 32 << 20
	// MaxSearchWindow bounds mail.search's date range.
	MaxSearchWindow = 366 * 24 * time.Hour
)

// Config is one Personal connection.
type Config struct {
	// Addr is imap_host:imap_port from the installed package.
	Addr     string
	Security Security
	// ServerName overrides the TLS name checked (defaults to Addr's host).
	ServerName string
	// RootCAs, when nil, is the system pool. Certificate validation is
	// never disabled.
	RootCAs *x509.CertPool
	// Username is the Person's login; UsernameDomain (the package's
	// username_domain) is appended to a bare name.
	Username       string
	UsernameDomain string
	Password       string
	// Key is this connection's secret for fingerprints and references,
	// at least 32 bytes. It must differ per connection.
	Key     []byte
	Filters Filters
	Limits  mimenorm.Limits
	// MaxRunBytes bounds raw bytes fetched per Changes call.
	MaxRunBytes int64
	// DialTimeout bounds connecting; Timeout bounds a whole operation.
	DialTimeout time.Duration
	Timeout     time.Duration
}

// String never renders the password or key.
func (c Config) String() string {
	return fmt.Sprintf("imapread.Config{Addr:%s Security:%s User:%s Folders:%v}", c.Addr, c.Security, c.loginName(), c.Filters.folders())
}

// GoString keeps %#v from printing secrets too.
func (c Config) GoString() string { return c.String() }

func (c *Config) loginName() string {
	if c.UsernameDomain != "" && !strings.Contains(c.Username, "@") {
		return c.Username + "@" + strings.TrimPrefix(c.UsernameDomain, "@")
	}
	return c.Username
}

func (c *Config) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return 15 * time.Second
}

func (c *Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 2 * time.Minute
}

// Reader reads one connection's mailbox.
type Reader struct {
	cfg    Config
	limits mimenorm.Limits
	keys   keys
	dial   func(context.Context, *Config) (readSession, error)
}

// New validates cfg and returns a Reader. It does not connect.
func New(cfg Config) (*Reader, error) {
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		return nil, fmt.Errorf("imapread: address must be host:port")
	}
	switch cfg.Security {
	case SecurityTLS, SecurityStartTLS:
	case "":
		cfg.Security = SecurityTLS
	default:
		return nil, fmt.Errorf("imapread: unsupported security mode %q (want ssl or tls)", cfg.Security)
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("imapread: username and password are required")
	}
	if len(cfg.Key) < 32 {
		return nil, errors.New("imapread: connection key must be at least 32 bytes")
	}
	if err := cfg.Filters.Validate(); err != nil {
		return nil, err
	}
	cfg.Key = append([]byte(nil), cfg.Key...)
	limits := cfg.Limits
	if limits == (mimenorm.Limits{}) {
		limits = mimenorm.DefaultLimits()
	}
	limits = normalizedLimits(limits)
	if cfg.MaxRunBytes <= 0 {
		cfg.MaxRunBytes = DefaultMaxRunBytes
	}
	return &Reader{cfg: cfg, limits: limits, keys: deriveKeys(cfg.Key), dial: dialSession}, nil
}

func normalizedLimits(l mimenorm.Limits) mimenorm.Limits {
	d := mimenorm.DefaultLimits()
	if l.MaxMessageBytes <= 0 {
		l.MaxMessageBytes = d.MaxMessageBytes
	}
	if l.MaxHeaderBytes <= 0 {
		l.MaxHeaderBytes = d.MaxHeaderBytes
	}
	return l
}

// FolderCursor is one folder's committed position (the mail contract's
// type).
type FolderCursor = mail.FolderCursor

// Cursor is a connection's committed position across its folders (the
// mail contract's type). The zero Cursor means "never read": each
// folder starts at now.
type Cursor = mail.Cursor

func cursorFolder(c Cursor, folder string) (FolderCursor, bool) {
	for _, f := range c.Folders {
		if f.Folder == folder {
			return f, true
		}
	}
	return FolderCursor{}, false
}

func cursorWith(c Cursor, fc FolderCursor) Cursor {
	out := Cursor{Folders: make([]FolderCursor, 0, len(c.Folders)+1)}
	replaced := false
	for _, f := range c.Folders {
		if f.Folder == fc.Folder {
			out.Folders = append(out.Folders, fc)
			replaced = true
			continue
		}
		out.Folders = append(out.Folders, f)
	}
	if !replaced {
		out.Folders = append(out.Folders, fc)
	}
	return out
}

// Message is one message's bounded evidence.
type Message struct {
	// Ref is the opaque reference for mail.message; only this
	// connection accepts it.
	Ref string `json:"ref"`
	// Fingerprint is the stable keyed identity (see keys.fingerprint).
	Fingerprint string            `json:"fingerprint"`
	Folder      string            `json:"folder"`
	Size        int64             `json:"size"`
	Content     *mimenorm.Message `json:"content"`
}

// FolderResult says what happened to one folder in a Changes call.
type FolderResult struct {
	Folder string `json:"folder"`
	// Outcome is ok, empty, baseline, cursor_reset or missing.
	Outcome  string `json:"outcome"`
	Scanned  int    `json:"scanned"`
	Filtered int    `json:"filtered"`
	// Gone counts messages expunged between SEARCH and FETCH.
	Gone int `json:"gone"`
	// Previous/Current UIDVALIDITY, set on cursor_reset.
	PreviousUIDValidity uint32 `json:"previousUidValidity,omitempty"`
	CurrentUIDValidity  uint32 `json:"currentUidValidity,omitempty"`
}

// ChangesResult is mail.changes's answer.
type ChangesResult struct {
	Outcome Outcome `json:"outcome"`
	// Reason is a category for unavailable/auth_failed (never provider
	// text).
	Reason   string         `json:"reason,omitempty"`
	Messages []Message      `json:"messages,omitempty"`
	Folders  []FolderResult `json:"folders,omitempty"`
	// Next is the cursor to commit after processing Messages. On
	// unavailable/auth_failed it equals the input cursor.
	Next Cursor `json:"next"`
	// More reports that the batch or run limit stopped the read early.
	More bool `json:"more,omitempty"`
}

// failure maps an error onto an outcome and a category.
func failure(err error) (Outcome, string) {
	switch {
	case errors.Is(err, errAuth):
		return OutcomeAuthFailed, "authentication"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return OutcomeUnavailable, "timeout"
	case errors.Is(err, context.Canceled):
		return OutcomeUnavailable, "cancelled"
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var recErr tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) ||
		errors.As(err, &certInvalid) || errors.As(err, &recErr) {
		return OutcomeUnavailable, "tls"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return OutcomeUnavailable, "timeout"
	}
	if strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:") {
		return OutcomeUnavailable, "tls"
	}
	return OutcomeUnavailable, "connection"
}

func (r *Reader) open(ctx context.Context) (readSession, context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.timeout())
	s, err := r.dial(ctx, &r.cfg)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return s, ctx, cancel, nil
}

// InScope reports whether folder is one of the connection's approved
// folders.
func (r *Reader) InScope(folder string) bool { return r.cfg.Filters.hasFolder(folder) }

// Changes returns the next bounded batch after cur across every approved
// folder. limit <= 0 means DefaultBatch; it is capped at MaxBatch and
// counts messages scanned (in scope or not), so one call's work is
// bounded.
func (r *Reader) Changes(ctx context.Context, cur Cursor, limit int) ChangesResult {
	return r.changes(ctx, r.cfg.Filters.folders(), cur, limit)
}

// ChangesFolder is Changes narrowed to one approved folder. The cursor's
// other folders are carried through untouched. A folder outside the
// scope is ErrBadRequest.
func (r *Reader) ChangesFolder(ctx context.Context, folder string, cur Cursor, limit int) (ChangesResult, error) {
	if !r.InScope(folder) {
		return ChangesResult{}, fmt.Errorf("%w: folder %q is outside this connection's scope", ErrBadRequest, folder)
	}
	return r.changes(ctx, []string{folder}, cur, limit), nil
}

func (r *Reader) changes(ctx context.Context, folders []string, cur Cursor, limit int) ChangesResult {
	if limit <= 0 {
		limit = DefaultBatch
	}
	if limit > MaxBatch {
		limit = MaxBatch
	}
	fail := func(err error) ChangesResult {
		o, reason := failure(err)
		return ChangesResult{Outcome: o, Reason: reason, Next: cur}
	}
	s, _, cancel, err := r.open(ctx)
	if err != nil {
		return fail(err)
	}
	defer cancel()
	defer s.logout()

	res := ChangesResult{Next: cur}
	remaining := limit
	var runBytes int64
	resets, missing := 0, 0
	for _, folder := range folders {
		fr := FolderResult{Folder: folder}
		st, err := s.examine(folder)
		if errors.Is(err, errFolderMissing) {
			fr.Outcome = "missing"
			res.Folders = append(res.Folders, fr)
			missing++
			continue
		}
		if err != nil {
			return fail(err)
		}
		fc, known := cursorFolder(cur, folder)
		switch {
		case !known:
			last, err := r.highestUID(s, st)
			if err != nil {
				return fail(err)
			}
			res.Next = cursorWith(res.Next, FolderCursor{Folder: folder, UIDValidity: st.UIDValidity, LastUID: last})
			fr.Outcome = "baseline"
			res.Folders = append(res.Folders, fr)
			continue
		case fc.UIDValidity != st.UIDValidity:
			res.Next = cursorWith(res.Next, FolderCursor{Folder: folder, UIDValidity: st.UIDValidity, LastUID: 0})
			fr.Outcome = string(OutcomeCursorReset)
			fr.PreviousUIDValidity, fr.CurrentUIDValidity = fc.UIDValidity, st.UIDValidity
			res.Folders = append(res.Folders, fr)
			resets++
			continue
		}
		if st.UIDNext != 0 && st.UIDNext <= fc.LastUID+1 {
			fr.Outcome = string(OutcomeEmpty)
			res.Folders = append(res.Folders, fr)
			continue
		}
		if remaining == 0 {
			res.More = true
			fr.Outcome = string(OutcomeEmpty)
			res.Folders = append(res.Folders, fr)
			continue
		}
		uids, err := s.searchUIDs(searchQuery{UseUID: true, AfterUID: fc.LastUID})
		if err != nil {
			return fail(err)
		}
		// "UID n:*" matches the highest UID even when it is below n
		// (RFC 3501 §6.4.8); keep only what is really after the cursor.
		uids = after(uids, fc.LastUID)
		if len(uids) > remaining {
			uids = uids[:remaining]
			res.More = true
		}
		headers, err := s.fetchHeaders(uids, r.limits.MaxHeaderBytes)
		if err != nil {
			return fail(err)
		}
		last := fc.LastUID
		for _, uid := range uids {
			h, ok := headers[uid]
			if !ok {
				fr.Gone++
				last = uid
				continue
			}
			if runBytes > 0 && runBytes+min64(h.Size, r.limits.MaxMessageBytes) > r.cfg.MaxRunBytes {
				// Stop before this message; it leads the next batch.
				res.More = true
				remaining = 0
				break
			}
			msg, status, err := r.read(s, folder, st.UIDValidity, uid, h)
			if err != nil {
				return fail(err)
			}
			last = uid
			switch status {
			case readGone:
				fr.Gone++
				continue
			case readFiltered:
				fr.Scanned++
				fr.Filtered++
				continue
			}
			fr.Scanned++
			runBytes += msg.Size
			res.Messages = append(res.Messages, *msg)
		}
		if remaining > 0 {
			remaining -= len(uids)
			if remaining < 0 {
				remaining = 0
			}
		}
		res.Next = cursorWith(res.Next, FolderCursor{Folder: folder, UIDValidity: st.UIDValidity, LastUID: last})
		if fr.Scanned-fr.Filtered > 0 {
			fr.Outcome = string(OutcomeOK)
		} else {
			fr.Outcome = string(OutcomeEmpty)
		}
		res.Folders = append(res.Folders, fr)
	}
	switch {
	case resets > 0:
		res.Outcome = OutcomeCursorReset
	case len(res.Messages) > 0:
		res.Outcome = OutcomeOK
	case missing == len(folders):
		res.Outcome, res.Reason = OutcomeUnavailable, "folder_missing"
	default:
		res.Outcome = OutcomeEmpty
	}
	return res
}

func after(uids []uint32, last uint32) []uint32 {
	out := uids[:0:0]
	seen := map[uint32]bool{}
	for _, u := range uids {
		if u > last && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// highestUID is the baseline for a folder the cursor has not seen.
func (r *Reader) highestUID(s readSession, st folderState) (uint32, error) {
	if st.UIDNext != 0 {
		return st.UIDNext - 1, nil
	}
	if st.Messages == 0 {
		return 0, nil
	}
	uids, err := s.searchUIDs(searchQuery{Highest: true})
	if err != nil {
		return 0, err
	}
	var hi uint32
	for _, u := range uids {
		if u > hi {
			hi = u
		}
	}
	return hi, nil
}

type readStatus int

const (
	readInScope readStatus = iota
	readFiltered
	readGone
)

// read turns one fetched header into evidence, fetching the body only
// when the message is in scope and within the size limit.
func (r *Reader) read(s readSession, folder string, validity, uid uint32, h fetchedHeader) (*Message, readStatus, error) {
	hdr, hOutcome, hReason := mimenorm.NormalizeHeaders(h.Header, r.limits)
	msg := &Message{
		Ref:         r.keys.encodeRef(ref{Folder: folder, UIDValidity: validity, UID: uid}),
		Fingerprint: r.keys.fingerprint(hdr, h.Size, h.Header),
		Folder:      folder,
		Size:        h.Size,
	}
	if hOutcome != mimenorm.OK {
		// Scope cannot be verified on unreadable headers: with a
		// narrowing filter the message stays out; without one it is
		// reported as the bounded outcome it is.
		if r.cfg.Filters.narrowing() {
			return nil, readFiltered, nil
		}
		msg.Content = &mimenorm.Message{Outcome: hOutcome, Reason: hReason}
		return msg, readInScope, nil
	}
	if !r.cfg.Filters.Match(hdr) {
		return nil, readFiltered, nil
	}
	if h.Size > r.limits.MaxMessageBytes {
		msg.Content = &mimenorm.Message{Outcome: mimenorm.Oversized, Reason: "message", Headers: hdr}
		return msg, readInScope, nil
	}
	raw, found, err := s.fetchBody(uid, r.limits.MaxMessageBytes)
	if err != nil {
		return nil, readInScope, err
	}
	if !found {
		// Expunged between the header and the body fetch.
		return nil, readGone, nil
	}
	msg.Content = mimenorm.Normalize(raw, r.limits)
	return msg, readInScope, nil
}
