package runtimedb

// Proactive communication and delivery state (ADR-W030 A10/A10.1, P15).
//
// Two pairs of things this file keeps apart, and most of it exists for
// that reason.
//
// Delivered is not acknowledged, and neither is answered. A message
// handed to Telegram is delivered; one a person saw is acknowledged; one
// they replied to is a third fact again. Collapsing them produces a
// Butler that treats "the notification was sent" as "they know", which
// is how a household gets blamed for ignoring something they never saw.
//
// Expired is not answered. A completed task whose notice expires stays
// completed: the notice is marked undelivered and shown as such, and it
// does not quietly become an approval the task is waiting on. Silence is
// never approval, and neither is a timeout.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoticeNotAnswerable is returned when a reply cannot authorize
// anything: the notice asked nothing, it already expired, it was already
// answered, or the reply does not resolve to a yes or a no.
var ErrNoticeNotAnswerable = errors.New("runtimedb: this reply authorizes nothing")

// MemberNoticePreferences is one person's override of the household
// policy.
type MemberNoticePreferences struct {
	Member         string `json:"member"`
	QuietStartMin  int    `json:"quietStartMin"`
	QuietEndMin    int    `json:"quietEndMin"`
	Timezone       string `json:"timezone,omitempty"`
	UrgentOverride bool   `json:"urgentOverride"`
	MinRelevance   int    `json:"minRelevance"`
}

// HasOwnQuietHours reports whether this person set a window of their
// own. Equal bounds mean they follow the household's, which is the right
// default: a household that agreed on quiet hours should not need every
// member to restate them.
func (p MemberNoticePreferences) HasOwnQuietHours() bool {
	return p.QuietStartMin != p.QuietEndMin
}

// SetMemberNoticePreferences records one person's overrides.
func (db *DB) SetMemberNoticePreferences(p MemberNoticePreferences) error {
	if p.Member == "" {
		return fmt.Errorf("runtimedb: notice preferences need a member")
	}
	for name, v := range map[string]int{"quietStartMin": p.QuietStartMin, "quietEndMin": p.QuietEndMin} {
		if v < 0 || v > 1439 {
			return fmt.Errorf("runtimedb: %s = %d, must be minutes within a day", name, v)
		}
	}
	if p.MinRelevance < 0 || p.MinRelevance > 100 {
		return fmt.Errorf("runtimedb: minRelevance = %d, must be between 0 and 100", p.MinRelevance)
	}
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			return fmt.Errorf("runtimedb: %q is not a known timezone: %w", p.Timezone, err)
		}
	}
	_, err := db.sql.Exec(
		`INSERT INTO member_notice_preferences (member, quiet_start_min, quiet_end_min, timezone,
		     urgent_override, min_relevance, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(member) DO UPDATE SET
		   quiet_start_min = excluded.quiet_start_min, quiet_end_min = excluded.quiet_end_min,
		   timezone = excluded.timezone, urgent_override = excluded.urgent_override,
		   min_relevance = excluded.min_relevance, updated_at = excluded.updated_at`,
		p.Member, p.QuietStartMin, p.QuietEndMin, p.Timezone,
		boolInt(p.UrgentOverride), p.MinRelevance, now())
	if err != nil {
		return fmt.Errorf("runtimedb: recording notice preferences: %w", err)
	}
	return nil
}

// GetMemberNoticePreferences returns a person's overrides, or the
// defaults (no personal window, urgent allowed through, no relevance
// floor) when they have set none.
func (db *DB) GetMemberNoticePreferences(member string) (MemberNoticePreferences, error) {
	p := MemberNoticePreferences{Member: member, UrgentOverride: true}
	var urgent int
	err := db.sql.QueryRow(
		`SELECT quiet_start_min, quiet_end_min, timezone, urgent_override, min_relevance
		 FROM member_notice_preferences WHERE member = ?`, member).
		Scan(&p.QuietStartMin, &p.QuietEndMin, &p.Timezone, &urgent, &p.MinRelevance)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return MemberNoticePreferences{}, fmt.Errorf("runtimedb: reading notice preferences: %w", err)
	}
	p.UrgentOverride = urgent != 0
	return p, nil
}

// NoticeKind is an administrator's declaration about a class of notice.
type NoticeKind struct {
	Kind        string `json:"kind"`
	Urgent      bool   `json:"urgent"`
	Relevance   int    `json:"relevance"`
	Description string `json:"description,omitempty"`
}

// SetNoticeKind declares whether a class of notice may claim the urgent
// exception, and how relevant it is.
//
// It is administrator-set rather than a flag on each message for a
// reason worth stating plainly: a sender that can mark its own message
// urgent will, and then quiet hours mean nothing. An urgent exception
// that everything can claim is not an exception.
func (db *DB) SetNoticeKind(k NoticeKind, by string) error {
	if k.Kind == "" {
		return fmt.Errorf("runtimedb: a notice kind needs a name")
	}
	if k.Relevance < 0 || k.Relevance > 100 {
		return fmt.Errorf("runtimedb: relevance = %d, must be between 0 and 100", k.Relevance)
	}
	if k.Urgent && k.Description == "" {
		return fmt.Errorf("runtimedb: a kind that may wake the household during quiet hours must say why it qualifies")
	}
	_, err := db.sql.Exec(
		`INSERT INTO notice_kinds (kind, urgent, relevance, description, updated_at, updated_by)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(kind) DO UPDATE SET urgent = excluded.urgent, relevance = excluded.relevance,
		   description = excluded.description, updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by`,
		k.Kind, boolInt(k.Urgent), k.Relevance, k.Description, now(), by)
	if err != nil {
		return fmt.Errorf("runtimedb: recording notice kind: %w", err)
	}
	return nil
}

// GetNoticeKind returns a kind's policy, or a non-urgent default.
func (db *DB) GetNoticeKind(kind string) (NoticeKind, error) {
	k := NoticeKind{Kind: kind, Relevance: 50}
	var urgent int
	err := db.sql.QueryRow(
		`SELECT urgent, relevance, description FROM notice_kinds WHERE kind = ?`, kind).
		Scan(&urgent, &k.Relevance, &k.Description)
	if errors.Is(err, sql.ErrNoRows) {
		// An undeclared kind is not urgent. Defaulting the other way
		// would make every new notice type able to wake the house.
		return k, nil
	}
	if err != nil {
		return NoticeKind{}, fmt.Errorf("runtimedb: reading notice kind: %w", err)
	}
	k.Urgent = urgent != 0
	return k, nil
}

// Notice is one proactive message and everything known about whether it
// landed.
type Notice struct {
	Seq            int64     `json:"seq"`
	Kind           string    `json:"kind"`
	Recipient      string    `json:"recipient"`
	RoutedToAdmin  bool      `json:"routedToAdmin,omitempty"`
	CorrelationID  string    `json:"correlationId,omitempty"`
	DedupeKey      string    `json:"dedupeKey"`
	Urgent         bool      `json:"urgent"`
	Relevance      int       `json:"relevance"`
	CreatedAt      time.Time `json:"createdAt"`
	HoldUntil      time.Time `json:"holdUntil,omitzero"`
	ExpiresAt      time.Time `json:"expiresAt"`
	DeliveredAt    time.Time `json:"deliveredAt,omitzero"`
	AcknowledgedAt time.Time `json:"acknowledgedAt,omitzero"`
	UndeliveredAt  time.Time `json:"undeliveredAt,omitzero"`
	Asks           bool      `json:"asks,omitempty"`
	AnsweredAt     time.Time `json:"answeredAt,omitzero"`
	Answer         string    `json:"answer,omitempty"`
	AnsweredBy     string    `json:"answeredBy,omitempty"`
	OutboundSeq    int64     `json:"outboundSeq,omitempty"`
}

// NoticeRequest is what a caller wants to say and to whom.
type NoticeRequest struct {
	Kind      string
	Recipient string
	// AdminFallback is the configured administrator, used when there is
	// no recipient. An unassigned notice defaults to them WITHOUT
	// granting access to whatever private thing prompted it, so the body
	// must be written for whoever ends up reading it (A10).
	AdminFallback string
	CorrelationID string
	DedupeKey     string
	Body          string
	// Asks marks a notice that poses a question. Only these can be
	// answered; a reply to anything else authorizes nothing.
	Asks bool
	TTL  time.Duration
}

// NoticeDecision is what the policy decided, so a caller and a Console
// can both see why a message went out now, later, or not at all.
type NoticeDecision struct {
	Seq int64 `json:"seq"`
	// Sent is false when the notice was dropped for relevance or
	// deduplicated onto an existing one.
	Sent bool `json:"sent"`
	// HeldUntil is set when quiet hours deferred it.
	HeldUntil time.Time `json:"heldUntil,omitzero"`
	Reason    string    `json:"reason,omitempty"`
}

// RaiseNotice applies the whole policy and records the notice.
//
// Order: relevance, then deduplication, then quiet hours. Relevance
// first because a message a person does not want is not worth
// deduplicating or scheduling; deduplication before quiet hours so a
// repeated notice collapses onto the one already waiting for morning
// rather than queueing a second alongside it.
func (db *DB) RaiseNotice(req NoticeRequest, householdQuiet ButlerSettings, at time.Time) (NoticeDecision, error) {
	if err := db.gate(GateDelivery); err != nil {
		return NoticeDecision{}, err
	}
	if req.Kind == "" || req.DedupeKey == "" {
		return NoticeDecision{}, fmt.Errorf("runtimedb: a notice needs a kind and a dedupe key")
	}
	recipient := req.Recipient
	routedToAdmin := false
	if recipient == "" {
		if req.AdminFallback == "" {
			return NoticeDecision{}, fmt.Errorf("runtimedb: a notice with no recipient needs the configured administrator to fall back to (ADR-W030 A10)")
		}
		recipient, routedToAdmin = req.AdminFallback, true
	}
	kind, err := db.GetNoticeKind(req.Kind)
	if err != nil {
		return NoticeDecision{}, err
	}
	prefs, err := db.GetMemberNoticePreferences(recipient)
	if err != nil {
		return NoticeDecision{}, err
	}

	// 1. Relevance. Not worth deduplicating or scheduling something this
	//    person has said they do not want.
	if kind.Relevance < prefs.MinRelevance {
		return NoticeDecision{
			Reason: fmt.Sprintf("%s is below %s's threshold for what is worth interrupting them for",
				req.Kind, recipient),
		}, nil
	}

	// 2. Deduplication, before scheduling, so a repeat collapses onto
	//    the one already waiting rather than queueing beside it.
	var existing int64
	err = db.sql.QueryRow(`SELECT seq FROM notices WHERE dedupe_key = ?`, req.DedupeKey).Scan(&existing)
	if err == nil {
		return NoticeDecision{Seq: existing, Reason: "this has already been raised"}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return NoticeDecision{}, fmt.Errorf("runtimedb: checking for a duplicate notice: %w", err)
	}

	// 3. Quiet hours: the person's own window if they set one, the
	//    household's otherwise, and an urgent kind goes through when the
	//    person allows urgent through.
	var hold time.Time
	quiet, window := db.quietFor(prefs, householdQuiet, at)
	if quiet {
		if kind.Urgent && prefs.UrgentOverride {
			// Through it goes, which is what an urgent exception is for
			// — and why only an administrator can declare one.
		} else {
			hold = window
		}
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = DefaultChatOutboundTTL
	}
	res, err := db.sql.Exec(
		`INSERT INTO notices (kind, recipient, routed_to_admin, correlation_id, dedupe_key,
		     urgent, relevance, created_at, hold_until, expires_at, asks)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Kind, recipient, boolInt(routedToAdmin), req.CorrelationID, req.DedupeKey,
		boolInt(kind.Urgent), kind.Relevance, now(), timeText(hold),
		at.Add(ttl).UTC().Format(time.RFC3339Nano), boolInt(req.Asks))
	if err != nil {
		return NoticeDecision{}, fmt.Errorf("runtimedb: recording notice: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return NoticeDecision{}, err
	}
	decision := NoticeDecision{Seq: seq, Sent: true, HeldUntil: hold}
	if !hold.IsZero() {
		decision.Reason = "held until quiet hours end"
	}
	return decision, nil
}

// quietFor resolves whose quiet hours apply and when they end.
//
// A person's own window wins when they set one; otherwise the
// household's. That ordering is what makes "per-person overrides" mean
// override rather than "an additional window nobody asked for".
func (db *DB) quietFor(prefs MemberNoticePreferences, household ButlerSettings, at time.Time) (bool, time.Time) {
	if prefs.HasOwnQuietHours() {
		personal := household
		personal.QuietStartMin, personal.QuietEndMin = prefs.QuietStartMin, prefs.QuietEndMin
		if prefs.Timezone != "" {
			personal.Timezone = prefs.Timezone
		}
		if personal.InQuietHours(at) {
			return true, personal.QuietHoursEnd(at)
		}
		return false, time.Time{}
	}
	if household.InQuietHours(at) {
		return true, household.QuietHoursEnd(at)
	}
	return false, time.Time{}
}

// MarkNoticeDelivered records that an adapter handed it over.
func (db *DB) MarkNoticeDelivered(seq int64, outbound int64) error {
	var out any
	if outbound != 0 {
		out = outbound
	}
	_, err := db.sql.Exec(
		`UPDATE notices SET delivered_at = ?, outbound = ? WHERE seq = ? AND delivered_at = ''`,
		now(), out, seq)
	if err != nil {
		return fmt.Errorf("runtimedb: marking notice delivered: %w", err)
	}
	return nil
}

// AcknowledgeNotice records that a person saw it. Separate from
// delivery, because "we sent it" and "they know" are different claims
// and only one of them is about a person.
func (db *DB) AcknowledgeNotice(seq int64) error {
	res, err := db.sql.Exec(
		`UPDATE notices SET acknowledged_at = ? WHERE seq = ? AND acknowledged_at = ''`, now(), seq)
	if err != nil {
		return fmt.Errorf("runtimedb: acknowledging notice: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AcknowledgeNoticesFor clears every notice a member can clear, in one
// statement.
//
// ONE STATEMENT RATHER THAN A WINDOW AND A LOOP, because "Clear" is a
// promise about all of them. Reading a page of notices and
// acknowledging those left a member with more than a page still holding
// unread rows, a card still showing a count, and a button that had just
// told them it worked. A control that reports success and leaves the
// thing it was pressed for undone is worse than one that is slow.
//
// The recipient is a predicate, not a parameter the caller could aim
// somewhere else, so this keeps the property homeClearNotices was built
// around: there is no field in a request that can name another person's
// notice.
//
// An unanswered question is excluded here rather than in the caller, so
// the exclusion cannot be lost by a caller that forgets it.
// Acknowledging one would take the decision off the member's page while
// whatever waits on it still waits.
func (db *DB) AcknowledgeNoticesFor(recipient string) (int, error) {
	if recipient == "" {
		return 0, fmt.Errorf("runtimedb: clearing notices needs a recipient")
	}
	res, err := db.sql.Exec(
		`UPDATE notices SET acknowledged_at = ?
		  WHERE recipient = ? AND acknowledged_at = '' AND undelivered_at = ''
		    AND NOT (asks = 1 AND answered_at = '')`, now(), recipient)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: clearing notices: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ExpireNotices marks everything past its expiry as undelivered.
//
// It returns how many, because a household that never hears about
// missed notices has a Butler that silently stops talking to it.
func (db *DB) ExpireNotices(at time.Time) (int, error) {
	stamp := at.UTC().Format(time.RFC3339Nano)
	res, err := db.sql.Exec(
		`UPDATE notices SET undelivered_at = ?
		 WHERE undelivered_at = '' AND delivered_at = '' AND expires_at <= ?`, stamp, stamp)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: expiring notices: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// AnswerNotice records a human's reply to a question a notice asked.
//
// It refuses four things, and each refusal is one way a stale action
// gets authorized: a notice that asked nothing, one that already
// expired, one already answered, and a reply that does not resolve to a
// clear yes or no. Ambiguity is not consent, and neither is a timeout.
func (db *DB) AnswerNotice(seq int64, answer, by string) (approved bool, err error) {
	if by == "" {
		return false, fmt.Errorf("runtimedb: an answer needs who gave it")
	}
	decided, ok := resolveAnswer(answer)
	if !ok {
		return false, fmt.Errorf("%w: %q is neither a yes nor a no, and a guess is not consent",
			ErrNoticeNotAnswerable, answer)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("runtimedb: answering a notice: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var asks int
	var recipient, undelivered, answered, expires string
	err = tx.QueryRow(
		`SELECT recipient, asks, undelivered_at, answered_at, expires_at FROM notices WHERE seq = ?`, seq).
		Scan(&recipient, &asks, &undelivered, &answered, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("runtimedb: answering a notice: %w", err)
	}
	// Only the person it was ASKED of may answer it (A8: a proposal
	// names its intended approver, and consent is bound to that
	// identity).
	//
	// Without this, anyone who can reach the endpoint could walk the
	// sequence numbers, answer somebody else's question, record the
	// approval under their own name, and — because an answered notice
	// cannot be answered twice — permanently lock the person it was
	// meant for out of their own decision.
	//
	// A wrong answerer reads as not-found rather than forbidden, for
	// the same reason the review token does: "that is not yours"
	// confirms it exists and who it belongs to.
	if recipient != by {
		return false, ErrNotFound
	}
	switch {
	case asks == 0:
		return false, fmt.Errorf("%w: that notice did not ask anything", ErrNoticeNotAnswerable)
	case undelivered != "":
		return false, fmt.Errorf("%w: that notice expired without reaching anyone", ErrNoticeNotAnswerable)
	case answered != "":
		return false, fmt.Errorf("%w: that was already answered", ErrNoticeNotAnswerable)
	}
	if expiry, perr := time.Parse(time.RFC3339Nano, expires); perr == nil && time.Now().After(expiry) {
		return false, fmt.Errorf("%w: that question has expired", ErrNoticeNotAnswerable)
	}
	if _, err := tx.Exec(
		`UPDATE notices SET answered_at = ?, answer = ?, answered_by = ? WHERE seq = ?`,
		now(), answer, by, seq); err != nil {
		return false, fmt.Errorf("runtimedb: answering a notice: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("runtimedb: answering a notice: %w", err)
	}
	return decided, nil
}

// resolveAnswer reads a reply as a yes or a no, or refuses.
//
// The vocabulary is small and the refusal is the point. "Maybe later",
// "if you think so", an emoji, or a sentence that mentions both are all
// ambiguous, and ambiguity must not authorize an action — A10 says
// ambiguous or expired answers do not authorize stale actions, and this
// is where that is enforced rather than guessed at by a model.
func resolveAnswer(answer string) (approved, ok bool) {
	norm := strings.ToLower(strings.TrimSpace(answer))
	norm = strings.Trim(norm, ".!,;: ")
	yes := map[string]bool{"yes": true, "y": true, "yep": true, "yeah": true,
		"ok": true, "okay": true, "approve": true, "approved": true, "go ahead": true, "do it": true}
	no := map[string]bool{"no": true, "n": true, "nope": true, "don't": true, "dont": true,
		"stop": true, "cancel": true, "reject": true, "no thanks": true}
	switch {
	case yes[norm]:
		return true, true
	case no[norm]:
		return false, true
	}
	return false, false
}

// GetNotice returns one notice.
func (db *DB) GetNotice(seq int64) (Notice, error) {
	return scanNotice(db.sql.QueryRow(noticeColumns+` WHERE seq = ?`, seq))
}

const noticeColumns = `SELECT seq, kind, recipient, routed_to_admin, correlation_id, dedupe_key,
	 urgent, relevance, created_at, hold_until, expires_at, delivered_at, acknowledged_at,
	 undelivered_at, asks, answered_at, answer, answered_by, COALESCE(outbound, 0)
	 FROM notices`

func scanNotice(row rowScanner) (Notice, error) {
	var n Notice
	var admin, urgent, asks int
	var created, hold, expires, delivered, acked, undelivered, answered string
	err := row.Scan(&n.Seq, &n.Kind, &n.Recipient, &admin, &n.CorrelationID, &n.DedupeKey,
		&urgent, &n.Relevance, &created, &hold, &expires, &delivered, &acked, &undelivered,
		&asks, &answered, &n.Answer, &n.AnsweredBy, &n.OutboundSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return Notice{}, ErrNotFound
	}
	if err != nil {
		return Notice{}, fmt.Errorf("runtimedb: scanning notice: %w", err)
	}
	n.RoutedToAdmin, n.Urgent, n.Asks = admin != 0, urgent != 0, asks != 0
	for _, f := range []struct {
		text string
		into *time.Time
	}{
		{created, &n.CreatedAt}, {hold, &n.HoldUntil}, {expires, &n.ExpiresAt},
		{delivered, &n.DeliveredAt}, {acked, &n.AcknowledgedAt},
		{undelivered, &n.UndeliveredAt}, {answered, &n.AnsweredAt},
	} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return Notice{}, fmt.Errorf("runtimedb: bad notice timestamp %q: %w", f.text, err)
		}
		*f.into = parsed
	}
	return n, nil
}

// noticeClauses turns a filter into SQL, in ONE place.
//
// Shared by the listing and the count deliberately: a header that says
// "3 unread" and a list that shows them have to be answering the same
// question, and two copies of these predicates is the way they stop.
func noticeClauses(f NoticeFilter) ([]string, []any) {
	clauses, args := []string{}, []any{}
	if f.Recipient != "" {
		clauses = append(clauses, "recipient = ?")
		args = append(args, f.Recipient)
	}
	if f.Deliverable {
		clauses = append(clauses, "undelivered_at = ''")
	}
	if f.Unacknowledged {
		clauses = append(clauses, "acknowledged_at = ''")
	}
	if f.Pending {
		// A question still waiting on this person: it asks, nobody has
		// answered it, and it has not run out. expires_at stores
		// RFC3339Nano UTC, so the comparison is lexicographic — the
		// same trick HouseholdEventsSince relies on.
		clauses = append(clauses, "asks = 1", "answered_at = ''",
			"(expires_at = '' OR expires_at > ?)")
		at := f.At
		if at.IsZero() {
			at = time.Now()
		}
		args = append(args, at.UTC().Format(time.RFC3339Nano))
	}
	return clauses, args
}

// NoticeFilter narrows a notice listing to what a caller will keep.
// Every field is optional, and an empty filter lists everything.
//
// THE PREDICATES BELONG IN THE QUERY, and the reason is the one
// HouseholdEventFilter gives: a limit applied before a filter is not a
// filter. The "Needs you" card read the newest fifty notices and then
// looked for questions among them, so fifty ordinary notices were
// enough to hide a decision that was still waiting — the card said
// nothing needed the member while something did, which is the one
// failure that card exists to prevent.
type NoticeFilter struct {
	// Recipient is the member whose notices these are.
	Recipient string
	// Deliverable drops notices that never reached the member.
	Deliverable bool
	// Unacknowledged drops notices already cleared.
	Unacknowledged bool
	// Pending narrows to questions still waiting on an answer: it asks,
	// it is unanswered, and it has not expired as of At.
	Pending bool
	// At is the clock Pending's expiry is measured against. Zero means
	// now.
	At time.Time
	// UnreadFirst puts unacknowledged notices at the head of the
	// listing, BEFORE the limit is applied.
	//
	// A caller that sorts unread to the top after reading a window has
	// only reordered that window: forty read notices in front of an
	// unread one leave the unread one out of the answer entirely, and
	// sorting what came back cannot put it there. The database decides
	// what is in the window, so the card can only show what it is also
	// counting.
	UnreadFirst bool
}

// ListNotices returns recent notices for one member ("" for all),
// newest first.
func (db *DB) ListNotices(limit int, f NoticeFilter) ([]Notice, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	clauses, args := noticeClauses(f)
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	order := " ORDER BY seq DESC"
	if f.UnreadFirst {
		// Unread first, then newest. The expression is a boolean, and
		// DESC puts the true ones (1) at the head.
		order = " ORDER BY (acknowledged_at = '') DESC, seq DESC"
	}
	args = append(args, limit)
	rows, err := db.sql.Query(noticeColumns+where+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing notices: %w", err)
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountNotices is how many notices match, with no window at all.
//
// A count a card prints must not be the size of a page the card also
// read: "3 unread" that means "3 unread among the forty I looked at" is
// a number that goes quietly wrong exactly when it matters, in the
// household with a backlog. This asks the question the header actually
// asks.
func (db *DB) CountNotices(f NoticeFilter) (int, error) {
	clauses, args := noticeClauses(f)
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM notices`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("runtimedb: counting notices: %w", err)
	}
	return n, nil
}

// DueNotices returns notices whose hold has passed and which have not
// been delivered or expired — what an outbound pass picks up.
func (db *DB) DueNotices(at time.Time) ([]Notice, error) {
	stamp := at.UTC().Format(time.RFC3339Nano)
	rows, err := db.sql.Query(
		noticeColumns+` WHERE delivered_at = '' AND undelivered_at = ''
		   AND (hold_until = '' OR hold_until <= ?) AND expires_at > ? ORDER BY seq`, stamp, stamp)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading due notices: %w", err)
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
