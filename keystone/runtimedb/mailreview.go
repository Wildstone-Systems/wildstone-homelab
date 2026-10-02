package runtimedb

// Exact private review of mail event candidates, and the calendar
// effects a review authorizes (migration 0101, epic #326 S06, #459;
// ADR-W030 A5.7, A8.1).
//
// A candidate (mailcandidates.go) is a question. A REVIEW is one offer
// of one exact revision of it to the Person who owns it, and it is the
// only thing that can turn the question into a calendar write:
//
//   - A review is bound to the Person, the candidate id and revision,
//     the target calendar, a digest of the exact event fields, the live
//     authorization and subscription version it was issued under, and
//     the route it was delivered on. It expires 72 hours after issue or
//     at the event's start, whichever is earlier.
//   - Its typed actions (Add, Edit, Dismiss — Apply and Dismiss for a
//     correction to an event already created) each carry a single-use
//     token. Only the sha256 of a token is stored; the value is derived
//     from a key that is in no backup (MailExcerptVault).
//   - Using ANY action closes the review, atomically, in the same
//     transaction that moves the candidate. A second click, a replay, a
//     stale revision, changed fields, an expired or revoked review,
//     another Person, an Owner, an Agent, a group reply or a different
//     conversation all find nothing open to use.
//   - Add and Apply record the calendar effect as "dispatching" in that
//     same transaction, BEFORE anything is sent. The write that follows
//     uses a deterministic event UID, so a timeout, a lost response or a
//     crash is settled by looking the UID up — never by a blind retry —
//     and at most one event ever exists.
//
// Nothing here parses a free-text answer. A "yes" in chat, silence, a
// delivered or acknowledged notice, and a Butler's say-so are not inputs
// to anything in this file.

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Review kinds.
const (
	// MailReviewCreate offers a new event.
	MailReviewCreate = "create"
	// MailReviewUpdate offers to apply a correction (a later message's
	// candidate) to the event an earlier candidate created.
	MailReviewUpdate = "update"
	// MailReviewCancel offers to remove an event the organizer cancelled.
	MailReviewCancel = "cancel"
)

// Typed review actions.
const (
	MailActionAdd     = "add"
	MailActionEdit    = "edit"
	MailActionDismiss = "dismiss"
	MailActionApply   = "apply"
)

// Review states. Only open can change, and only once.
const (
	MailReviewOpen    = "open"
	MailReviewUsed    = "used"
	MailReviewRevoked = "revoked"
	MailReviewExpired = "expired"
)

// Availability observed when a review was issued.
const (
	MailAvailabilityFree        = "free"
	MailAvailabilityBusy        = "busy"
	MailAvailabilityUnavailable = "unavailable"
	MailAvailabilityNotChecked  = "not_checked"
)

// Calendar effect states.
const (
	MailEffectDispatching = "dispatching"
	MailEffectConfirmed   = "confirmed"
	MailEffectReconciled  = "reconciled"
	MailEffectReturned    = "returned"
	MailEffectFailed      = "failed"
)

// MailRouteNotice is a review delivered as the Person's in-app notice
// only (no one-to-one chat Connection was found). A chat route is
// "chat:<binding id>".
const MailRouteNotice = "notice"

// MailReviewWindow is the longest a review stays open.
const MailReviewWindow = 72 * time.Hour

// maxMailConflicts bounds the conflicts a review names.
const maxMailConflicts = 12

var (
	// ErrMailReviewRefused is every refusal of a review action that is
	// not "no such candidate" or "stale": used, revoked, expired, not
	// offered, wrong conversation, group, changed authority. Nothing
	// changed except, where it says so, the review being closed.
	ErrMailReviewRefused = errors.New("runtimedb: this approves nothing")
	// ErrNoMailReview: the candidate has no open review.
	ErrNoMailReview = errors.New("runtimedb: this candidate has no open review")
)

// MailEventUID is the deterministic VEVENT UID of the event a candidate
// creates. Deterministic so that whether the event exists is a question
// the calendar can answer after a timeout or a crash.
func MailEventUID(candidate string) string { return "wildstone-candidate-" + candidate }

// MailEventWindow is the event's exact [start, end): the end, or the
// start plus the duration, or a whole day for an all-day event. ok is
// false when the fields do not determine one.
func MailEventWindow(f MailCandidateFields) (start, end time.Time, ok bool) {
	if f.Start.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	start, end = f.Start, f.End
	if end.IsZero() && f.DurationMinutes > 0 {
		end = start.Add(time.Duration(f.DurationMinutes) * time.Minute)
	}
	if end.IsZero() && f.AllDay {
		end = start.AddDate(0, 0, 1)
	}
	if end.IsZero() || !end.After(start) {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

// MailFieldsDigest digests exactly what an approval writes: the kind,
// the calendar, the event UID it acts on, and every event field.
func MailFieldsDigest(kind, calendar, eventUID string, f MailCandidateFields) string {
	material, _ := json.Marshal([]any{"mail-review-fields/v1", kind, calendar, eventUID,
		strings.TrimSpace(f.Title), mailTS(f.Start), mailTS(f.End), f.DurationMinutes, f.AllDay, f.TZ,
		f.Location, f.Notes})
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:])
}

// MailConflict is one event already on a calendar the Person can read
// that overlaps the candidate.
type MailConflict struct {
	Calendar string    `json:"calendar"`
	Title    string    `json:"title"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end,omitzero"`
	AllDay   bool      `json:"allDay,omitempty"`
}

// MailReview is one offer.
type MailReview struct {
	ID                  string         `json:"id"`
	Candidate           string         `json:"candidate"`
	PersonID            string         `json:"-"`
	Revision            int            `json:"revision"`
	Kind                string         `json:"kind"`
	TargetCalendar      string         `json:"targetCalendar"`
	EventUID            string         `json:"eventUid,omitempty"`
	FieldsDigest        string         `json:"fieldsDigest"`
	AuthorizationDigest string         `json:"-"`
	SubscriptionVersion int            `json:"-"`
	Route               string         `json:"route"`
	Availability        string         `json:"availability"`
	Conflicts           []MailConflict `json:"conflicts"`
	CheckedCalendars    []string       `json:"checkedCalendars"`
	Notice              int64          `json:"notice,omitempty"`
	IssuedAt            time.Time      `json:"issuedAt"`
	ExpiresAt           time.Time      `json:"expiresAt"`
	State               string         `json:"state"`
	StateReason         string         `json:"stateReason,omitempty"`
	UsedAction          string         `json:"usedAction,omitempty"`
	UsedVia             string         `json:"usedVia,omitempty"`
	ClosedAt            time.Time      `json:"closedAt,omitzero"`
	// Actions are the typed actions this review offers.
	Actions []string `json:"actions"`
}

// mailReviewActions is what a review of kind offers a candidate in
// state. Needs-details offers no Add: nobody adds an invented time.
func mailReviewActions(kind string, c MailCandidate) []string {
	switch kind {
	case MailReviewCreate:
		switch c.State {
		case MailCandidateProposed:
			return []string{MailActionAdd, MailActionEdit, MailActionDismiss}
		case MailCandidateNeedsDetails:
			return []string{MailActionEdit, MailActionDismiss}
		}
	case MailReviewUpdate:
		switch c.State {
		case MailCandidateProposed:
			return []string{MailActionApply, MailActionEdit, MailActionDismiss}
		case MailCandidateNeedsDetails:
			return []string{MailActionEdit, MailActionDismiss}
		}
	case MailReviewCancel:
		if c.State == MailCandidateCreated && c.UpdatePending == MailUpdateCancel {
			return []string{MailActionApply, MailActionDismiss}
		}
	}
	return nil
}

// reviewToken derives one action's token. Never stored.
func (v *MailExcerptVault) reviewToken(review, action string) string {
	mac := hmac.New(sha256.New, v.tokenKey)
	mac.Write([]byte("mail-review/v1\x00" + review + "\x00" + action))
	return "mrt_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func mailTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newMailReviewID() (string, error) {
	id, err := newMailCandidateID()
	if err != nil {
		return "", err
	}
	return "mr_" + strings.TrimPrefix(id, "mc_"), nil
}

const mailReviewColumns = `SELECT id, candidate, person_id, revision, kind, target_calendar, event_uid, fields_digest,
	authorization_digest, subscription_version, route, availability, conflicts_json, checked_calendars, notice,
	issued_at, expires_at, state, state_reason, used_action, used_via, closed_at FROM mail_reviews`

func scanMailReview(row rowScanner) (MailReview, error) {
	var r MailReview
	var conflicts, checked, issued, expires, closed string
	err := row.Scan(&r.ID, &r.Candidate, &r.PersonID, &r.Revision, &r.Kind, &r.TargetCalendar, &r.EventUID,
		&r.FieldsDigest, &r.AuthorizationDigest, &r.SubscriptionVersion, &r.Route, &r.Availability, &conflicts,
		&checked, &r.Notice, &issued, &expires, &r.State, &r.StateReason, &r.UsedAction, &r.UsedVia, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return MailReview{}, ErrNoMailReview
	}
	if err != nil {
		return MailReview{}, fmt.Errorf("runtimedb: scanning mail review: %w", err)
	}
	if err := json.Unmarshal([]byte(conflicts), &r.Conflicts); err != nil {
		return MailReview{}, err
	}
	if err := json.Unmarshal([]byte(checked), &r.CheckedCalendars); err != nil {
		return MailReview{}, err
	}
	if r.Conflicts == nil {
		r.Conflicts = []MailConflict{}
	}
	if r.CheckedCalendars == nil {
		r.CheckedCalendars = []string{}
	}
	for _, f := range []struct {
		s    string
		into *time.Time
	}{{issued, &r.IssuedAt}, {expires, &r.ExpiresAt}, {closed, &r.ClosedAt}} {
		if *f.into, err = parseMailTS(f.s); err != nil {
			return MailReview{}, err
		}
	}
	r.Actions = []string{}
	return r, nil
}

// withActions fills Actions for an open review from its candidate.
func (r MailReview) withActions(c MailCandidate) MailReview {
	if r.State == MailReviewOpen {
		if a := mailReviewActions(r.Kind, c); a != nil {
			r.Actions = a
		}
	}
	return r
}

// MailReviewIssue is what the review engine asks to offer.
type MailReviewIssue struct {
	Candidate string
	Revision  int
	Kind      string
	// TargetCalendar is the calendar the effect lands on: the live
	// authorization's for a create, the existing event's for an update
	// or cancel.
	TargetCalendar string
	// EventUID is the existing event's UID (update, cancel).
	EventUID string
	// Grant is the live authorization the review is issued under.
	Grant BackgroundGrant
	// Route is how it is delivered (MailRouteNotice or "chat:<id>").
	Route        string
	Availability string
	Conflicts    []MailConflict
	Checked      []string
	At           time.Time
}

// IssueMailReview opens a review of the candidate's exact current
// revision, closing any review still open for it (an Edit or a returned
// Add invalidates earlier tokens). It returns the review and its action
// tokens, which exist nowhere else.
func (db *DB) IssueMailReview(v *MailExcerptVault, in MailReviewIssue) (MailReview, map[string]string, error) {
	if v == nil || len(v.tokenKey) == 0 {
		return MailReview{}, nil, fmt.Errorf("runtimedb: no review key: reviews cannot be issued")
	}
	switch in.Kind {
	case MailReviewCreate, MailReviewUpdate, MailReviewCancel:
	default:
		return MailReview{}, nil, fmt.Errorf("runtimedb: unknown review kind %q", in.Kind)
	}
	if in.TargetCalendar == "" || in.Grant.AuthorizationDigest == "" || in.Route == "" {
		return MailReview{}, nil, fmt.Errorf("runtimedb: a review needs its calendar, authority and route")
	}
	if in.Kind != MailReviewCreate && in.EventUID == "" {
		return MailReview{}, nil, fmt.Errorf("runtimedb: an update or cancel review names the event it acts on")
	}
	switch in.Availability {
	case MailAvailabilityFree, MailAvailabilityBusy, MailAvailabilityUnavailable, MailAvailabilityNotChecked:
	default:
		return MailReview{}, nil, fmt.Errorf("runtimedb: unknown availability %q", in.Availability)
	}
	conflicts := in.Conflicts
	if len(conflicts) > maxMailConflicts {
		conflicts = conflicts[:maxMailConflicts]
	}
	for i := range conflicts {
		conflicts[i].Title = BoundMailText(conflicts[i].Title, 120)
		conflicts[i].Calendar = BoundMailText(conflicts[i].Calendar, 64)
	}
	if conflicts == nil {
		conflicts = []MailConflict{}
	}
	checked := in.Checked
	if checked == nil {
		checked = []string{}
	}
	conflictJSON, _ := json.Marshal(conflicts)
	checkedJSON, _ := json.Marshal(checked)

	tx, err := db.sql.Begin()
	if err != nil {
		return MailReview{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := mailCandidateTx(tx, in.Candidate)
	if err != nil {
		return MailReview{}, nil, err
	}
	owner, err := resolvePersonID(tx, in.Grant.Person)
	if err != nil || owner != c.PersonID {
		return MailReview{}, nil, ErrMailCandidateNotFound
	}
	if c.Revision != in.Revision {
		return MailReview{}, nil, ErrMailCandidateStale
	}
	if mailReviewActions(in.Kind, c) == nil {
		return MailReview{}, nil, fmt.Errorf("%w: a %s candidate cannot be offered for %s", ErrMailCandidateStale, c.State, in.Kind)
	}
	var inflight int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM mail_candidate_effects WHERE candidate = ? AND state = 'dispatching'`,
		c.ID).Scan(&inflight); err != nil {
		return MailReview{}, nil, err
	}
	if inflight > 0 {
		return MailReview{}, nil, fmt.Errorf("%w: a calendar write for it is still being settled", ErrMailCandidateStale)
	}
	expires := in.At.Add(MailReviewWindow)
	if s := c.Fields.Start; !s.IsZero() && s.Before(expires) {
		expires = s
	}
	if !expires.After(in.At) {
		return MailReview{}, nil, fmt.Errorf("%w: the event has already started", ErrMailReviewRefused)
	}
	if _, err := tx.Exec(`UPDATE mail_reviews SET state = 'revoked', state_reason = 'replaced by a fresh review',
		closed_at = ? WHERE candidate = ? AND state = 'open'`, mailTS(in.At), c.ID); err != nil {
		return MailReview{}, nil, err
	}
	id, err := newMailReviewID()
	if err != nil {
		return MailReview{}, nil, err
	}
	digest := MailFieldsDigest(in.Kind, in.TargetCalendar, in.EventUID, c.Fields)
	if _, err := tx.Exec(`INSERT INTO mail_reviews (id, candidate, person_id, revision, kind, target_calendar,
		event_uid, fields_digest, authorization_digest, subscription_version, route, availability, conflicts_json,
		checked_calendars, issued_at, expires_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open')`,
		id, c.ID, c.PersonID, c.Revision, in.Kind, in.TargetCalendar, in.EventUID, digest,
		in.Grant.AuthorizationDigest, in.Grant.SubscriptionVersion, in.Route, in.Availability,
		string(conflictJSON), string(checkedJSON), mailTS(in.At), mailTS(expires)); err != nil {
		return MailReview{}, nil, fmt.Errorf("runtimedb: recording mail review: %w", err)
	}
	tokens := map[string]string{}
	for _, action := range mailReviewActions(in.Kind, c) {
		tok := v.reviewToken(id, action)
		if _, err := tx.Exec(`INSERT INTO mail_review_tokens (token_hash, review, action) VALUES (?, ?, ?)`,
			mailTokenHash(tok), id, action); err != nil {
			return MailReview{}, nil, err
		}
		tokens[action] = tok
	}
	if err := mailAudit(tx, c.ID, c.Revision, "review", c.State, c.State, MailMonitorActor, in.Kind, in.At); err != nil {
		return MailReview{}, nil, err
	}
	r, err := scanMailReview(tx.QueryRow(mailReviewColumns+` WHERE id = ?`, id))
	if err != nil {
		return MailReview{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return MailReview{}, nil, err
	}
	return r.withActions(c), tokens, nil
}

// SetMailReviewNotice records the notice a review was delivered as.
func (db *DB) SetMailReviewNotice(review string, notice int64) error {
	_, err := db.sql.Exec(`UPDATE mail_reviews SET notice = ? WHERE id = ?`, notice, review)
	return err
}

// MailReviewFor returns the acting Person's latest review of one of
// their candidates (open or not), or ErrNoMailReview.
func (db *DB) MailReviewFor(actor, candidate string) (MailReview, error) {
	c, err := db.GetMailCandidate(actor, candidate)
	if err != nil {
		return MailReview{}, err
	}
	r, err := scanMailReview(db.sql.QueryRow(mailReviewColumns+` WHERE candidate = ? AND person_id = ?
		ORDER BY issued_at DESC, rowid DESC LIMIT 1`, c.ID, c.PersonID))
	if err != nil {
		return MailReview{}, err
	}
	return r.withActions(c), nil
}

// MailReviewTokens re-derives the action tokens of the acting Person's
// open, unexpired review of one of their candidates, for their own
// authenticated Console or CLI. Nothing is stored; a restored or
// re-keyed database derives tokens that match nothing.
func (db *DB) MailReviewTokens(v *MailExcerptVault, actor, candidate string, at time.Time) (MailReview, map[string]string, error) {
	r, err := db.MailReviewFor(actor, candidate)
	if err != nil {
		return MailReview{}, nil, err
	}
	if r.State != MailReviewOpen || !at.Before(r.ExpiresAt) || v == nil || len(v.tokenKey) == 0 {
		return r, map[string]string{}, nil
	}
	tokens := map[string]string{}
	for _, action := range r.Actions {
		tok := v.reviewToken(r.ID, action)
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM mail_review_tokens WHERE token_hash = ? AND review = ? AND action = ?`,
			mailTokenHash(tok), r.ID, action).Scan(&n); err != nil {
			return MailReview{}, nil, err
		}
		if n == 1 {
			tokens[action] = tok
		}
	}
	return r, tokens, nil
}

// MailReviewClaim is one attempt to use a review.
type MailReviewClaim struct {
	// Actor is the AUTHENTICATED acting handle. It must resolve to the
	// active Person who owns the candidate.
	Actor string
	// Token, or Candidate+Revision+Action, names what is approved. A
	// token may also name its candidate and revision; they must agree.
	Token     string
	Candidate string
	Revision  int
	Action    string
	// Fields are Edit's new event fields.
	Fields *MailCandidateFields
	// Via is where the click came from: "console", "home", "cli", "web"
	// or "chat:<binding id>". A chat click must arrive on the binding
	// the review was delivered to.
	Via string
	// Group: the message came from a room. Always refused.
	Group bool
	// Grant is the live authorization, evaluated by the caller just
	// before the claim. Required for everything but Dismiss.
	Grant *BackgroundGrant
	At    time.Time
}

// MailReviewClaimed is what a successful claim did.
type MailReviewClaimed struct {
	Review    MailReview
	Action    string
	Candidate MailCandidate
	// Effect is the calendar write Add/Apply recorded, in state
	// dispatching; zero for Edit and Dismiss.
	Effect MailEffect
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMailReviewRefused, fmt.Sprintf(format, args...))
}

// ResolveMailReview finds, without using it, the review a claim names:
// the Person-scoped review, its candidate, and — for a token — the
// action the token is for. Every miss that is not the caller's is
// ErrMailCandidateNotFound.
func (db *DB) ResolveMailReview(actor, token, candidate string) (MailReview, MailCandidate, string, error) {
	person, err := db.mailViewer(actor)
	if err != nil {
		return MailReview{}, MailCandidate{}, "", err
	}
	var r MailReview
	action := ""
	if token != "" {
		var id string
		err := db.sql.QueryRow(`SELECT review, action FROM mail_review_tokens WHERE token_hash = ?`,
			mailTokenHash(token)).Scan(&id, &action)
		if errors.Is(err, sql.ErrNoRows) {
			return MailReview{}, MailCandidate{}, "", refuse("that is not a review token")
		}
		if err != nil {
			return MailReview{}, MailCandidate{}, "", err
		}
		if r, err = scanMailReview(db.sql.QueryRow(mailReviewColumns+` WHERE id = ?`, id)); err != nil {
			return MailReview{}, MailCandidate{}, "", err
		}
		if r.PersonID != person || (candidate != "" && candidate != r.Candidate) {
			return MailReview{}, MailCandidate{}, "", ErrMailCandidateNotFound
		}
	} else {
		if r, err = db.MailReviewFor(actor, candidate); err != nil {
			return MailReview{}, MailCandidate{}, "", err
		}
	}
	c, err := db.GetMailCandidate(actor, r.Candidate)
	if err != nil {
		return MailReview{}, MailCandidate{}, "", err
	}
	return r.withActions(c), c, action, nil
}

// ClaimMailReview uses one action of a review, once. It is the only
// path from a candidate to a calendar effect: Add and Apply record the
// effect (dispatching) in the same transaction that closes the review
// and moves the candidate, so a duplicate claim finds the review used
// and the effect already in flight.
func (db *DB) ClaimMailReview(c MailReviewClaim) (MailReviewClaimed, error) {
	if c.Group {
		return MailReviewClaimed{}, refuse("a reply in a group approves nothing")
	}
	person, err := db.mailViewer(c.Actor)
	if err != nil {
		return MailReviewClaimed{}, err
	}
	if c.Action == MailActionAdd || c.Action == MailActionApply || c.Action == MailActionEdit {
		if err := db.gate(GateDispatch); err != nil {
			return MailReviewClaimed{}, refuse("%v", err)
		}
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return MailReviewClaimed{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var reviewID, action string
	if c.Token != "" {
		err := tx.QueryRow(`SELECT review, action FROM mail_review_tokens WHERE token_hash = ?`,
			mailTokenHash(c.Token)).Scan(&reviewID, &action)
		if errors.Is(err, sql.ErrNoRows) {
			return MailReviewClaimed{}, refuse("that is not a review token")
		}
		if err != nil {
			return MailReviewClaimed{}, err
		}
		if c.Action != "" && c.Action != action {
			return MailReviewClaimed{}, refuse("that token is for %s, not %s", action, c.Action)
		}
	} else {
		if c.Candidate == "" || c.Revision <= 0 || c.Action == "" {
			return MailReviewClaimed{}, refuse("name the exact candidate, revision and action")
		}
		cand, err := mailCandidateTx(tx, c.Candidate)
		if err != nil || cand.PersonID != person {
			return MailReviewClaimed{}, ErrMailCandidateNotFound
		}
		err = tx.QueryRow(`SELECT id FROM mail_reviews WHERE candidate = ? AND person_id = ? AND state = 'open'`,
			c.Candidate, person).Scan(&reviewID)
		if errors.Is(err, sql.ErrNoRows) {
			if cand.Revision != c.Revision {
				return MailReviewClaimed{}, ErrMailCandidateStale
			}
			return MailReviewClaimed{}, refuse("there is no open review of it: it was already answered, replaced or has expired")
		}
		if err != nil {
			return MailReviewClaimed{}, err
		}
		action = c.Action
	}
	r, err := scanMailReview(tx.QueryRow(mailReviewColumns+` WHERE id = ?`, reviewID))
	if err != nil {
		return MailReviewClaimed{}, err
	}
	if r.PersonID != person || (c.Candidate != "" && c.Candidate != r.Candidate) {
		return MailReviewClaimed{}, ErrMailCandidateNotFound
	}
	if c.Revision != 0 && c.Revision != r.Revision {
		return MailReviewClaimed{}, ErrMailCandidateStale
	}
	closeReview := func(state, reason string) error {
		if _, err := tx.Exec(`UPDATE mail_reviews SET state = ?, state_reason = ?, closed_at = ? WHERE id = ? AND state = 'open'`,
			state, reason, mailTS(c.At), r.ID); err != nil {
			return err
		}
		return tx.Commit()
	}
	switch r.State {
	case MailReviewUsed:
		return MailReviewClaimed{}, refuse("that review was already answered")
	case MailReviewRevoked:
		return MailReviewClaimed{}, refuse("that review was withdrawn (%s)", r.StateReason)
	case MailReviewExpired:
		return MailReviewClaimed{}, refuse("that review has expired")
	}
	if !c.At.Before(r.ExpiresAt) {
		if err := closeReview(MailReviewExpired, "expired before it was answered"); err != nil {
			return MailReviewClaimed{}, err
		}
		return MailReviewClaimed{}, refuse("that review has expired")
	}
	cand, err := mailCandidateTx(tx, r.Candidate)
	if err != nil || cand.PersonID != person {
		return MailReviewClaimed{}, ErrMailCandidateNotFound
	}
	if cand.Revision != r.Revision || MailFieldsDigest(r.Kind, r.TargetCalendar, r.EventUID, cand.Fields) != r.FieldsDigest {
		if err := closeReview(MailReviewRevoked, "the candidate changed after it was offered"); err != nil {
			return MailReviewClaimed{}, err
		}
		return MailReviewClaimed{}, ErrMailCandidateStale
	}
	if !slices.Contains(mailReviewActions(r.Kind, cand), action) {
		return MailReviewClaimed{}, refuse("%s is not offered for this review", action)
	}
	var hasToken int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM mail_review_tokens WHERE review = ? AND action = ?`, r.ID, action).Scan(&hasToken); err != nil {
		return MailReviewClaimed{}, err
	}
	if hasToken != 1 {
		return MailReviewClaimed{}, refuse("%s is not offered for this review", action)
	}
	if strings.HasPrefix(c.Via, "chat:") && c.Via != r.Route {
		return MailReviewClaimed{}, refuse("a reply is honoured only in the conversation the review was sent to")
	}
	if action != MailActionDismiss {
		g := c.Grant
		if g == nil {
			return MailReviewClaimed{}, refuse("the mailbox's authorization could not be confirmed")
		}
		owner, err := resolvePersonID(tx, g.Person)
		if err != nil || owner != person || g.AuthorizationDigest != r.AuthorizationDigest ||
			g.SubscriptionVersion != r.SubscriptionVersion {
			if err := closeReview(MailReviewRevoked, "the authority it was issued under changed"); err != nil {
				return MailReviewClaimed{}, err
			}
			return MailReviewClaimed{}, refuse("the authority this review was issued under has changed")
		}
		if r.Kind == MailReviewCreate && g.TargetCalendar != r.TargetCalendar {
			if err := closeReview(MailReviewRevoked, "the target calendar changed"); err != nil {
				return MailReviewClaimed{}, err
			}
			return MailReviewClaimed{}, refuse("the target calendar has changed")
		}
	}
	var inflight int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM mail_candidate_effects WHERE candidate = ? AND state = 'dispatching'`,
		cand.ID).Scan(&inflight); err != nil {
		return MailReviewClaimed{}, err
	}
	if inflight > 0 {
		return MailReviewClaimed{}, refuse("a calendar write for it is already in flight")
	}

	var edited MailCandidateFields
	if action == MailActionEdit {
		if c.Fields == nil {
			return MailReviewClaimed{}, refuse("an edit carries the event's new fields")
		}
		edited = *c.Fields
		edited.Title = strings.TrimSpace(edited.Title)
		if err := edited.validate(); err != nil {
			return MailReviewClaimed{}, err
		}
	}

	// Close the review: from here, this click is the one that counted.
	res, err := tx.Exec(`UPDATE mail_reviews SET state = 'used', used_action = ?, used_by = ?, used_via = ?, closed_at = ?
		WHERE id = ? AND state = 'open'`, action, c.Actor, BoundMailText(c.Via, 64), mailTS(c.At), r.ID)
	if err != nil {
		return MailReviewClaimed{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return MailReviewClaimed{}, refuse("that review was already answered")
	}
	out := MailReviewClaimed{Action: action}
	switch {
	case action == MailActionEdit:
		if _, err := reviseMailCandidateTx(tx, cand, edited, c.Actor, c.At); err != nil {
			return MailReviewClaimed{}, err
		}
	case action == MailActionDismiss:
		switch r.Kind {
		case MailReviewCancel:
			// Keep the event: the pending cancel is settled, not applied.
			if _, err := tx.Exec(`UPDATE mail_candidates SET update_pending = '', updated_at = ? WHERE id = ?`,
				mailTS(c.At), cand.ID); err != nil {
				return MailReviewClaimed{}, err
			}
			if err := mailAudit(tx, cand.ID, cand.Revision, "keep", cand.State, cand.State, c.Actor, "cancel dismissed", c.At); err != nil {
				return MailReviewClaimed{}, err
			}
		default:
			if err := resolveMailCandidateTx(tx, cand, MailCandidateDismissed, "dismissed by its Person", c.Actor, c.At); err != nil {
				return MailReviewClaimed{}, err
			}
			if r.Kind == MailReviewUpdate && cand.Supersedes != "" {
				if err := settlePriorUpdateTx(tx, cand.Supersedes, "update dismissed", c.Actor, c.At); err != nil {
					return MailReviewClaimed{}, err
				}
			}
		}
	default: // add, apply
		uid := r.EventUID
		if r.Kind == MailReviewCreate {
			uid = MailEventUID(cand.ID)
		}
		if r.Kind != MailReviewCancel {
			res, err := tx.Exec(`UPDATE mail_candidates SET state = 'accepted', state_reason = '', updated_at = ?
				WHERE id = ? AND revision = ? AND state = 'proposed'`, mailTS(c.At), cand.ID, cand.Revision)
			if err != nil {
				return MailReviewClaimed{}, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return MailReviewClaimed{}, ErrMailCandidateStale
			}
			if err := mailAudit(tx, cand.ID, cand.Revision, "approve", cand.State, MailCandidateAccepted, c.Actor, r.Kind, c.At); err != nil {
				return MailReviewClaimed{}, err
			}
		} else if err := mailAudit(tx, cand.ID, cand.Revision, "approve", cand.State, cand.State, c.Actor, r.Kind, c.At); err != nil {
			return MailReviewClaimed{}, err
		}
		res, err := tx.Exec(`INSERT INTO mail_candidate_effects (candidate, revision, review, kind, state, calendar, uid,
			fields_digest, actor, started_at) VALUES (?, ?, ?, ?, 'dispatching', ?, ?, ?, ?, ?)`,
			cand.ID, cand.Revision, r.ID, r.Kind, r.TargetCalendar, uid, r.FieldsDigest, c.Actor, mailTS(c.At))
		if err != nil {
			return MailReviewClaimed{}, fmt.Errorf("runtimedb: recording the calendar effect: %w", err)
		}
		seq, _ := res.LastInsertId()
		if out.Effect, err = mailEffectTx(tx, seq); err != nil {
			return MailReviewClaimed{}, err
		}
	}
	if out.Review, err = scanMailReview(tx.QueryRow(mailReviewColumns+` WHERE id = ?`, r.ID)); err != nil {
		return MailReviewClaimed{}, err
	}
	if out.Candidate, err = mailCandidateTx(tx, cand.ID); err != nil {
		return MailReviewClaimed{}, err
	}
	if err := tx.Commit(); err != nil {
		return MailReviewClaimed{}, err
	}
	return out, nil
}

// settlePriorUpdateTx clears the update_pending of the created
// candidate a correction supersedes.
func settlePriorUpdateTx(tx *sql.Tx, prior, reason, actor string, at time.Time) error {
	p, err := mailCandidateTx(tx, prior)
	if err != nil {
		if errors.Is(err, ErrMailCandidateNotFound) {
			return nil
		}
		return err
	}
	if p.UpdatePending == MailUpdateNone {
		return nil
	}
	if _, err := tx.Exec(`UPDATE mail_candidates SET update_pending = '', updated_at = ? WHERE id = ?`, mailTS(at), p.ID); err != nil {
		return err
	}
	return mailAudit(tx, p.ID, p.Revision, "update_settled", p.State, p.State, actor, reason, at)
}

// RevokeMailReviews closes every open review of a candidate.
func (db *DB) RevokeMailReviews(candidate, reason string, at time.Time) error {
	_, err := db.sql.Exec(`UPDATE mail_reviews SET state = 'revoked', state_reason = ?, closed_at = ?
		WHERE candidate = ? AND state = 'open'`, BoundMailText(reason, 200), mailTS(at), candidate)
	return err
}

// ExpireMailReviews closes open reviews that ran out, and those whose
// candidate is no longer in a state the review can act on (resolved,
// purged by a revocation, superseded). It returns how many it closed.
func (db *DB) ExpireMailReviews(at time.Time) (int, error) {
	stamp := mailTS(at)
	res, err := db.sql.Exec(`UPDATE mail_reviews SET state = 'expired', state_reason = 'not answered in time', closed_at = ?
		WHERE state = 'open' AND expires_at <= ?`, stamp, stamp)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = db.sql.Exec(`UPDATE mail_reviews SET state = 'revoked', state_reason = 'the candidate moved on', closed_at = ?
		WHERE state = 'open' AND candidate IN (SELECT c.id FROM mail_candidates c WHERE c.id = mail_reviews.candidate AND (
			c.revision <> mail_reviews.revision OR
			(mail_reviews.kind IN ('create', 'update') AND c.state NOT IN ('proposed', 'needs_details')) OR
			(mail_reviews.kind = 'cancel' AND (c.state <> 'created' OR c.update_pending <> 'cancel'))))`, stamp)
	if err != nil {
		return 0, err
	}
	m, _ := res.RowsAffected()
	return int(n + m), nil
}

// ---------------------------------------------------------------------
// Calendar effects.

// MailEffect is one calendar write a review authorized, and what became
// of it: the effect evidence of a created, updated or cancelled event.
type MailEffect struct {
	Seq          int64     `json:"seq"`
	Candidate    string    `json:"candidate"`
	Revision     int       `json:"revision"`
	Review       string    `json:"review"`
	Kind         string    `json:"kind"`
	State        string    `json:"state"`
	Calendar     string    `json:"calendar"`
	UID          string    `json:"uid"`
	Href         string    `json:"href,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	FieldsDigest string    `json:"fieldsDigest"`
	Actor        string    `json:"actor"`
	Attempts     int       `json:"attempts"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt,omitzero"`
	Detail       string    `json:"detail,omitempty"`
}

const mailEffectColumns = `SELECT seq, candidate, revision, review, kind, state, calendar, uid, href, etag,
	fields_digest, actor, attempts, started_at, finished_at, detail FROM mail_candidate_effects`

func scanMailEffect(row rowScanner) (MailEffect, error) {
	var e MailEffect
	var started, finished string
	err := row.Scan(&e.Seq, &e.Candidate, &e.Revision, &e.Review, &e.Kind, &e.State, &e.Calendar, &e.UID, &e.Href,
		&e.ETag, &e.FieldsDigest, &e.Actor, &e.Attempts, &started, &finished, &e.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return MailEffect{}, ErrNotFound
	}
	if err != nil {
		return MailEffect{}, err
	}
	if e.StartedAt, err = parseMailTS(started); err != nil {
		return MailEffect{}, err
	}
	if e.FinishedAt, err = parseMailTS(finished); err != nil {
		return MailEffect{}, err
	}
	return e, nil
}

func mailEffectTx(tx *sql.Tx, seq int64) (MailEffect, error) {
	return scanMailEffect(tx.QueryRow(mailEffectColumns+` WHERE seq = ?`, seq))
}

// GetMailEffect returns one effect. Trusted in-process callers only.
func (db *DB) GetMailEffect(seq int64) (MailEffect, error) {
	return scanMailEffect(db.sql.QueryRow(mailEffectColumns+` WHERE seq = ?`, seq))
}

// NoteMailEffectAttempt counts one write attempt.
func (db *DB) NoteMailEffectAttempt(seq int64) error {
	_, err := db.sql.Exec(`UPDATE mail_candidate_effects SET attempts = attempts + 1 WHERE seq = ?`, seq)
	return err
}

// InflightMailEffects returns effects still dispatching that started at
// or before cutoff — what the reconciler settles after a crash.
func (db *DB) InflightMailEffects(cutoff time.Time) ([]MailEffect, error) {
	rows, err := db.sql.Query(mailEffectColumns+` WHERE state = 'dispatching' AND started_at <= ? ORDER BY seq`, mailTS(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailEffect
	for rows.Next() {
		e, err := scanMailEffect(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MailCandidateEvent returns the settled effect that put a candidate's
// event on the calendar — where it is and under which UID — or ok=false.
func (db *DB) MailCandidateEvent(candidate string) (MailEffect, bool, error) {
	e, err := scanMailEffect(db.sql.QueryRow(mailEffectColumns+` WHERE candidate = ? AND kind IN ('create', 'update')
		AND state IN ('confirmed', 'reconciled') ORDER BY seq DESC LIMIT 1`, candidate))
	if errors.Is(err, ErrNotFound) {
		return MailEffect{}, false, nil
	}
	if err != nil {
		return MailEffect{}, false, err
	}
	return e, true, nil
}

// ListMailEffects is the acting Person's effect evidence for one of
// their candidates.
func (db *DB) ListMailEffects(actor, candidate string) ([]MailEffect, error) {
	c, err := db.GetMailCandidate(actor, candidate)
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(mailEffectColumns+` WHERE candidate = ? ORDER BY seq`, c.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MailEffect{}
	for rows.Next() {
		e, err := scanMailEffect(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SettleMailEffect records that the calendar holds the effect: state is
// MailEffectConfirmed (the calendar answered) or MailEffectReconciled
// (an indeterminate write was found by its UID). It moves the candidate
// on in the same transaction — created (and its excerpt purged), or for
// a cancel, cancelled — and is idempotent on an effect already settled.
func (db *DB) SettleMailEffect(seq int64, state, href, etag, detail string, at time.Time) (MailCandidate, error) {
	if state != MailEffectConfirmed && state != MailEffectReconciled {
		return MailCandidate{}, fmt.Errorf("runtimedb: %q is not a settled effect state", state)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return MailCandidate{}, err
	}
	defer func() { _ = tx.Rollback() }()
	e, err := mailEffectTx(tx, seq)
	if err != nil {
		return MailCandidate{}, err
	}
	if e.State != MailEffectDispatching {
		c, err := mailCandidateTx(tx, e.Candidate)
		return c, err
	}
	if _, err := tx.Exec(`UPDATE mail_candidate_effects SET state = ?, href = ?, etag = ?, detail = ?, finished_at = ?
		WHERE seq = ? AND state = 'dispatching'`, state, BoundMailText(href, 1024), BoundMailText(etag, 256),
		BoundMailText(detail, 200), mailTS(at), seq); err != nil {
		return MailCandidate{}, err
	}
	c, err := mailCandidateTx(tx, e.Candidate)
	if err != nil {
		return MailCandidate{}, err
	}
	switch e.Kind {
	case MailReviewCreate, MailReviewUpdate:
		if c.State == MailCandidateAccepted && c.Revision == e.Revision {
			reason := "added to " + e.Calendar
			if e.Kind == MailReviewUpdate {
				reason = "applied to the event on " + e.Calendar
			}
			if err := resolveMailCandidateTx(tx, c, MailCandidateCreated, reason, e.Actor, at); err != nil {
				return MailCandidate{}, err
			}
		} else if err := mailAudit(tx, c.ID, e.Revision, "effect_settled", c.State, c.State, e.Actor, e.Kind, at); err != nil {
			return MailCandidate{}, err
		}
		if e.Kind == MailReviewUpdate && c.Supersedes != "" {
			if err := settlePriorUpdateTx(tx, c.Supersedes, "update applied", e.Actor, at); err != nil {
				return MailCandidate{}, err
			}
		}
	case MailReviewCancel:
		if _, err := tx.Exec(`UPDATE mail_candidates SET update_pending = '' WHERE id = ?`, c.ID); err != nil {
			return MailCandidate{}, err
		}
		if c.State == MailCandidateCreated {
			if err := resolveMailCandidateTx(tx, c, MailCandidateCancelled, "removed from "+e.Calendar+" at its Person's approval", e.Actor, at); err != nil {
				return MailCandidate{}, err
			}
		}
	}
	out, err := mailCandidateTx(tx, e.Candidate)
	if err != nil {
		return MailCandidate{}, err
	}
	return out, tx.Commit()
}

// ReturnMailEffect records that an effect did not happen: state is
// MailEffectReturned (found absent after an indeterminate write, or a
// new conflict appeared) or MailEffectFailed (the calendar refused).
// An accepted candidate goes back to proposed for a fresh review; a
// cancel stays pending.
func (db *DB) ReturnMailEffect(seq int64, state, detail string, at time.Time) (MailCandidate, error) {
	if state != MailEffectReturned && state != MailEffectFailed {
		return MailCandidate{}, fmt.Errorf("runtimedb: %q is not a returned effect state", state)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return MailCandidate{}, err
	}
	defer func() { _ = tx.Rollback() }()
	e, err := mailEffectTx(tx, seq)
	if err != nil {
		return MailCandidate{}, err
	}
	if e.State != MailEffectDispatching {
		c, err := mailCandidateTx(tx, e.Candidate)
		return c, err
	}
	if _, err := tx.Exec(`UPDATE mail_candidate_effects SET state = ?, detail = ?, finished_at = ? WHERE seq = ? AND state = 'dispatching'`,
		state, BoundMailText(detail, 200), mailTS(at), seq); err != nil {
		return MailCandidate{}, err
	}
	c, err := mailCandidateTx(tx, e.Candidate)
	if err != nil {
		return MailCandidate{}, err
	}
	if e.Kind != MailReviewCancel && c.State == MailCandidateAccepted && c.Revision == e.Revision {
		if _, err := tx.Exec(`UPDATE mail_candidates SET state = 'proposed', state_reason = ?, updated_at = ? WHERE id = ?`,
			BoundMailText(detail, 200), mailTS(at), c.ID); err != nil {
			return MailCandidate{}, err
		}
		if err := mailAudit(tx, c.ID, c.Revision, "returned", MailCandidateAccepted, MailCandidateProposed, e.Actor, detail, at); err != nil {
			return MailCandidate{}, err
		}
	}
	out, err := mailCandidateTx(tx, e.Candidate)
	if err != nil {
		return MailCandidate{}, err
	}
	return out, tx.Commit()
}

// MailCandidateInternal reads a candidate for trusted in-process code
// (the review engine and its reconciler), never for an API route.
func (db *DB) MailCandidateInternal(id string) (MailCandidate, error) {
	return scanMailCandidate(db.sql.QueryRow(mailCandidateColumns+` WHERE c.id = ?`, id))
}

// ---------------------------------------------------------------------
// Private delivery.

// DirectChatRoute finds the one-to-one chat Connection a Person can be
// reached on: a live binding whose identity is that Person, who is an
// active member, with a known direct conversation that is not a room.
// A binding to preferAgent (the monitor's own Agent) wins. ok=false
// means there is none, and nothing is sent by chat.
func (db *DB) DirectChatRoute(personID, preferAgent string) (ChatBinding, bool, error) {
	bindings, err := db.ListChatBindings(false)
	if err != nil {
		return ChatBinding{}, false, err
	}
	var found ChatBinding
	ok := false
	for _, b := range bindings {
		if b.Revoked() || b.Conversation == "" {
			continue
		}
		id, err := personIDForRead(db.sql, b.Identity)
		if err != nil || id == "" || id != personID {
			continue
		}
		m, err := db.GetButlerMember(b.Identity)
		if err != nil || !m.Active() {
			continue
		}
		if db.IsChatRoom(b.Connection, b.Conversation) {
			continue
		}
		if !ok || (b.Butler == preferAgent && found.Butler != preferAgent) {
			found, ok = b, true
		}
	}
	return found, ok, nil
}

// purgeMailReviewsOnRestore: no review issued before a snapshot is
// usable after it (its tokens are derived from a key the recovery set
// does not carry anyway).
func purgeMailReviewsOnRestore(db *DB) error {
	_, err := db.sql.Exec(`UPDATE mail_reviews SET state = 'revoked', state_reason = 'restored from a backup',
		closed_at = ? WHERE state = 'open'`, mailTS(time.Now()))
	return err
}
