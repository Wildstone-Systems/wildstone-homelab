// Package mailreview is the exact private review of mail event
// candidates, and the at-most-once calendar effect an approval
// authorizes (epic #326 S06, #459; ADR-W030 A5.7, A8.1).
//
// The monitor (keystone/mailmonitor) records a candidate and tells this
// engine through its CandidateHook. The engine then:
//
//  1. evaluates the mailbox's authority LIVE (the Person-authorized,
//     Owner-approved intersection) and resolves the owning Person;
//  2. checks availability with calendar.check_availability over the
//     calendars the authorization permits — the target calendar and the
//     Person's chosen conflict calendars — and only those that Person
//     can read now, so it names only conflicts they may see;
//  3. opens a review of that exact revision (runtimedb.IssueMailReview)
//     and delivers it privately: an in-app notice for that Person, and a
//     push on their authenticated one-to-one chat Connection when there
//     is one. Never a room, never another Person, never the Owner.
//
// An action (Act) comes back through the Person-scoped API — Console,
// Homepage, CLI — or the deterministic /mail chat command, which no
// model sees. Add re-evaluates authority, the Person's standing and
// their write access to the target calendar, claims the review (one
// transaction closes it, moves the candidate to accepted and records
// the effect as dispatching), rechecks availability — a new conflict
// sends it back to review with a fresh token — and then writes one
// event under the deterministic UID wildstone-candidate-<id> with
// If-None-Match: *. A timeout, a lost response, or a crash is settled
// by looking that UID up before anything is retried, so at most one
// event ever exists. The mailbox credential is never involved: calendar
// writes use the Person's current calendar access.
package mailreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/caldav"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/domestic"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// Calendar is the exact, Person-scoped calendar path. *domestic.Calendar
// implements it; every call checks the acting Person's current access.
type Calendar interface {
	CheckWritable(ctx context.Context, member, id string) error
	Availability(ctx context.Context, member, id string, start, end time.Time) ([]caldav.Event, string, error)
	EventByUID(ctx context.Context, member, id, uid string) (*caldav.Event, string, error)
	CreateExact(ctx context.Context, member, id string, w caldav.EventWrite) (string, string, error)
	UpdateExact(ctx context.Context, member, id string, w caldav.EventWrite) (string, string, error)
	DeleteExact(ctx context.Context, member, id, uid string) error
}

var _ Calendar = (*domestic.Calendar)(nil)

// Outcomes of an action.
const (
	OutcomeAdded     = "added"
	OutcomeUpdated   = "updated"
	OutcomeCancelled = "cancelled"
	OutcomeDismissed = "dismissed"
	OutcomeKept      = "kept"
	OutcomeRevised   = "revised"
	// OutcomeReturned: a new conflict appeared (or the calendar could
	// not be rechecked, or an interrupted write was found absent). The
	// candidate is proposed again under a fresh review.
	OutcomeReturned = "returned_to_review"
	// OutcomePending: the write's result is not known yet and the
	// calendar could not be asked. The effect stays dispatching and the
	// reconciler settles it by UID; nothing is retried blind.
	OutcomePending = "pending_reconciliation"
)

// ErrCalendarRefused: the calendar refused the write (access lost, or
// the credential may not write there). Nothing was written.
var ErrCalendarRefused = errors.New("mailreview: the calendar refused the write")

// Engine is the review and effect engine.
type Engine struct {
	DB    *runtimedb.DB
	Vault *runtimedb.MailExcerptVault
	// Calendar is the exact calendar path; nil means no calendar is
	// configured, and nothing can be offered for Add.
	Calendar Calendar
	Now      func() time.Time
	Log      *slog.Logger
	// SettleAfter is how long a dispatching effect is left to its own
	// request before the reconciler looks it up (default 2 minutes, far
	// beyond the calendar client's timeout).
	SettleAfter time.Duration

	// mu serializes effect work in this process: the reconciler never
	// looks up an effect a live Act is still writing.
	mu sync.Mutex
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// liveGrant evaluates the connection's authority now.
func (e *Engine) liveGrant(ctx context.Context, connection string) (runtimedb.BackgroundGrant, error) {
	a, err := e.DB.LiveBackgroundAuthorization(connection)
	if err != nil {
		return runtimedb.BackgroundGrant{}, fmt.Errorf("%w: no live authorization for %s", runtimedb.ErrBackgroundUseDenied, connection)
	}
	sub, err := e.DB.GetAgentSubscription(a.Subscription)
	if err != nil {
		return runtimedb.BackgroundGrant{}, fmt.Errorf("%w: its subscription is gone", runtimedb.ErrBackgroundUseDenied)
	}
	return e.DB.AuthorizeBackgroundUse(ctx, connection, a.Subscription, sub.ApprovedVersion)
}

// CandidateChanged implements mailmonitor.CandidateHook. A new
// candidate, or a created one with a pending cancel, gets a review;
// a candidate whose event already exists and was corrected is offered
// through its successor (the correction's own candidate), so there is
// nothing to send for it here. Superseded and cancelled candidates lose
// any open review.
func (e *Engine) CandidateChanged(ctx context.Context, ch runtimedb.MailCandidateChange) error {
	switch ch.Kind {
	case runtimedb.MailChangeSuperseded, runtimedb.MailChangeCancelled:
		return e.DB.RevokeMailReviews(ch.Candidate.ID, "the candidate was "+ch.Kind, e.now())
	case runtimedb.MailChangeNew, runtimedb.MailChangeUpdateNeeded:
		c, err := e.DB.MailCandidateInternal(ch.Candidate.ID)
		if err != nil {
			return err
		}
		if c.UpdatePending == runtimedb.MailUpdateEvent {
			return nil
		}
		if !c.Unresolved() && c.UpdatePending != runtimedb.MailUpdateCancel {
			return nil
		}
		_, err = e.Offer(ctx, c.ID)
		return err
	}
	return nil
}

// target works out what a review of c acts on: its kind, the calendar
// the effect lands on and, for an update or cancel, the existing
// event's UID.
func (e *Engine) target(c runtimedb.MailCandidate, g runtimedb.BackgroundGrant) (kind, calendar, uid string, err error) {
	switch {
	case c.State == runtimedb.MailCandidateCreated && c.UpdatePending == runtimedb.MailUpdateCancel:
		ev, ok, err := e.DB.MailCandidateEvent(c.ID)
		if err != nil {
			return "", "", "", err
		}
		if !ok {
			return runtimedb.MailReviewCancel, c.TargetCalendar, runtimedb.MailEventUID(c.ID), nil
		}
		return runtimedb.MailReviewCancel, ev.Calendar, ev.UID, nil
	case c.Supersedes != "":
		prior, err := e.DB.MailCandidateInternal(c.Supersedes)
		if err != nil || prior.PersonID != c.PersonID {
			break
		}
		switch prior.State {
		case runtimedb.MailCandidateCreated:
			ev, ok, err := e.DB.MailCandidateEvent(prior.ID)
			if err != nil {
				return "", "", "", err
			}
			if !ok {
				return runtimedb.MailReviewUpdate, prior.TargetCalendar, runtimedb.MailEventUID(prior.ID), nil
			}
			return runtimedb.MailReviewUpdate, ev.Calendar, ev.UID, nil
		case runtimedb.MailCandidateAccepted:
			// Its own write is still being settled; offer the correction
			// once it is.
			return "", "", "", fmt.Errorf("mailreview: %s is still being written; its correction waits", prior.ID)
		}
	}
	return runtimedb.MailReviewCreate, g.TargetCalendar, "", nil
}

// Offer opens (or re-opens) the private review of c's current revision
// and delivers it to its Person. It is idempotent in effect: a new
// review replaces any open one, whose tokens stop working.
func (e *Engine) Offer(ctx context.Context, id string) (runtimedb.MailReview, error) {
	at := e.now()
	c, err := e.DB.MailCandidateInternal(id)
	if err != nil {
		return runtimedb.MailReview{}, err
	}
	g, err := e.liveGrant(ctx, c.Connection)
	if err != nil {
		return runtimedb.MailReview{}, err
	}
	handle, err := e.DB.PersonHandle(c.PersonID)
	if err != nil || handle == "" {
		return runtimedb.MailReview{}, fmt.Errorf("mailreview: the candidate's Person has no handle")
	}
	kind, calendar, uid, err := e.target(c, g)
	if err != nil {
		return runtimedb.MailReview{}, err
	}
	availability, conflicts, checked := e.availability(ctx, handle, g, calendar, uid, kind, c)
	route := runtimedb.MailRouteNotice
	agent := ""
	if b, err := e.DB.GetButlerBySeq(g.Agent); err == nil {
		agent = b.Name
	}
	binding, direct, err := e.DB.DirectChatRoute(c.PersonID, agent)
	if err != nil {
		return runtimedb.MailReview{}, err
	}
	if direct {
		route = fmt.Sprintf("chat:%d", binding.ID)
	}
	review, _, err := e.DB.IssueMailReview(e.Vault, runtimedb.MailReviewIssue{
		Candidate: c.ID, Revision: c.Revision, Kind: kind, TargetCalendar: calendar, EventUID: uid,
		Grant: g, Route: route, Availability: availability, Conflicts: conflicts, Checked: checked, At: at,
	})
	if err != nil {
		return runtimedb.MailReview{}, err
	}
	if err := e.deliver(review, c, handle, binding, direct, at); err != nil {
		return review, err
	}
	return review, nil
}

// availability runs calendar.check_availability over the calendars the
// authorization permits that this Person can read now. A calendar they
// cannot read is skipped and never disclosed; the event a correction
// would update is not its own conflict.
func (e *Engine) availability(ctx context.Context, handle string, g runtimedb.BackgroundGrant, target, uid, kind string,
	c runtimedb.MailCandidate) (string, []runtimedb.MailConflict, []string) {
	if kind == runtimedb.MailReviewCancel || c.State == runtimedb.MailCandidateNeedsDetails || e.Calendar == nil {
		return runtimedb.MailAvailabilityNotChecked, nil, nil
	}
	start, end, ok := runtimedb.MailEventWindow(c.Fields)
	if !ok {
		return runtimedb.MailAvailabilityNotChecked, nil, nil
	}
	calendars := e.conflictCalendars(g, target)
	return e.check(ctx, handle, calendars, start, end, map[string]bool{uid: uid != "", runtimedb.MailEventUID(c.ID): true})
}

// maxCheckedCalendars bounds how many calendars one review reads.
const maxCheckedCalendars = 17

// conflictCalendars is the target calendar plus the calendars the
// authorization lets a review check: the Person's authorized scope, and
// the conflictCalendars the approved subscription's match names (S07
// files them there; the subscription's content is digested into the
// Person's authorization, so both are what the Person agreed to). Each
// is still read only if the Person can read it now.
func (e *Engine) conflictCalendars(g runtimedb.BackgroundGrant, target string) []string {
	out := []string{target}
	seen := map[string]bool{target: true}
	add := func(ids []string) {
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] && len(out) < maxCheckedCalendars {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	add(g.Scope.ConflictCalendars)
	if sub, err := e.DB.GetAgentSubscription(g.Subscription); err == nil {
		var match struct {
			ConflictCalendars []string `json:"conflictCalendars"`
		}
		if json.Unmarshal([]byte(sub.MatchJSON), &match) == nil {
			add(match.ConflictCalendars)
		}
	}
	return out
}

func (e *Engine) check(ctx context.Context, handle string, calendars []string, start, end time.Time,
	self map[string]bool) (string, []runtimedb.MailConflict, []string) {
	var conflicts []runtimedb.MailConflict
	checked := []string{}
	unavailable := false
	for _, id := range calendars {
		events, resolved, err := e.Calendar.Availability(ctx, handle, id, start, end)
		switch {
		case errors.Is(err, domestic.ErrCalendarAccess):
			continue
		case err != nil:
			unavailable = true
			continue
		}
		checked = append(checked, resolved)
		for _, ev := range events {
			if self[ev.UID] {
				continue
			}
			conflicts = append(conflicts, runtimedb.MailConflict{Calendar: resolved, Title: ev.Summary,
				Start: ev.Start, End: ev.End, AllDay: ev.AllDay})
		}
	}
	switch {
	case unavailable || len(checked) == 0:
		return runtimedb.MailAvailabilityUnavailable, conflicts, checked
	case len(conflicts) > 0:
		return runtimedb.MailAvailabilityBusy, conflicts, checked
	}
	return runtimedb.MailAvailabilityFree, conflicts, checked
}

// ActRequest is one typed action, authenticated as Actor.
type ActRequest struct {
	// Actor is the AUTHENTICATED acting handle — a Person's own session
	// or token, never a delegated identity, an Agent or a machine.
	Actor string
	// Via is where it came from: console, home, cli, web or
	// chat:<binding id>.
	Via   string
	Group bool
	// Token, or Candidate+Revision+Action.
	Token     string
	Candidate string
	Revision  int
	Action    string
	// Fields carries Edit's new event.
	Fields *runtimedb.MailCandidateFields
}

// Result is what an action did.
type Result struct {
	Outcome   string                  `json:"outcome"`
	Candidate runtimedb.MailCandidate `json:"candidate"`
	// Review is the fresh review an Edit or a returned Add opened.
	Review  *runtimedb.MailReview `json:"review,omitempty"`
	Effect  *runtimedb.MailEffect `json:"effect,omitempty"`
	Message string                `json:"message"`
}

// Act performs one typed action. Every refusal changes nothing (except
// closing a review that can no longer be used) and is an error:
// runtimedb.ErrMailCandidateNotFound for anything not the actor's,
// runtimedb.ErrMailCandidateStale for a revision or fields that moved,
// runtimedb.ErrMailReviewRefused for everything else.
func (e *Engine) Act(ctx context.Context, req ActRequest) (Result, error) {
	if req.Group {
		return Result{}, fmt.Errorf("%w: a reply in a group approves nothing", runtimedb.ErrMailReviewRefused)
	}
	review, cand, tokenAction, err := e.DB.ResolveMailReview(req.Actor, req.Token, req.Candidate)
	if err != nil {
		return Result{}, err
	}
	action := req.Action
	if tokenAction != "" {
		if action != "" && action != tokenAction {
			return Result{}, fmt.Errorf("%w: that token is for %s, not %s", runtimedb.ErrMailReviewRefused, tokenAction, action)
		}
		action = tokenAction
	}
	var grant *runtimedb.BackgroundGrant
	g, gerr := e.liveGrant(ctx, cand.Connection)
	if gerr == nil {
		grant = &g
	}
	if action == runtimedb.MailActionAdd || action == runtimedb.MailActionApply {
		// Authority first: the intersection, the Person's standing (the
		// claim re-resolves them as an active member), and write access
		// to the calendar the effect lands on.
		if gerr != nil {
			_ = e.DB.RevokeMailReviews(cand.ID, "the mailbox's authorization is no longer live", e.now())
			return Result{}, fmt.Errorf("%w: the mailbox's authorization is no longer live", runtimedb.ErrMailReviewRefused)
		}
		if e.Calendar == nil {
			return Result{}, fmt.Errorf("%w: no calendar is configured", runtimedb.ErrMailReviewRefused)
		}
		if err := e.Calendar.CheckWritable(ctx, req.Actor, review.TargetCalendar); err != nil {
			return Result{}, fmt.Errorf("%w: you can no longer write to %s", runtimedb.ErrMailReviewRefused, review.TargetCalendar)
		}
	}
	claimed, err := e.DB.ClaimMailReview(runtimedb.MailReviewClaim{
		Actor: req.Actor, Token: req.Token, Candidate: req.Candidate, Revision: req.Revision, Action: action,
		Fields: req.Fields, Via: req.Via, Group: req.Group, Grant: grant, At: e.now(),
	})
	if err != nil {
		return Result{}, err
	}
	switch claimed.Action {
	case runtimedb.MailActionEdit:
		out := Result{Outcome: OutcomeRevised, Candidate: claimed.Candidate, Message: "Saved as a new revision."}
		fresh, err := e.Offer(ctx, claimed.Candidate.ID)
		if err != nil {
			e.log().Warn("mail review: re-offering an edited candidate failed", "candidate", claimed.Candidate.ID, "err", err)
			out.Message = "Saved as a new revision; it could not be offered again yet."
		} else {
			out.Review = &fresh
		}
		out.Candidate, _ = e.DB.MailCandidateInternal(claimed.Candidate.ID)
		return out, nil
	case runtimedb.MailActionDismiss:
		if claimed.Review.Kind == runtimedb.MailReviewCancel {
			return Result{Outcome: OutcomeKept, Candidate: claimed.Candidate, Message: "The event stays on the calendar."}, nil
		}
		return Result{Outcome: OutcomeDismissed, Candidate: claimed.Candidate, Message: "Dismissed. You will not be asked about this message again."}, nil
	}
	return e.dispatch(ctx, claimed, req.Actor)
}

// dispatch performs the effect a claim recorded.
func (e *Engine) dispatch(ctx context.Context, claimed runtimedb.MailReviewClaimed, actor string) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	eff, c := claimed.Effect, claimed.Candidate
	if eff.Kind != runtimedb.MailReviewCancel {
		start, end, ok := runtimedb.MailEventWindow(c.Fields)
		if !ok {
			return e.returnToReview(ctx, eff, "the event's time is not complete")
		}
		calendars := claimed.Review.CheckedCalendars
		if len(calendars) == 0 {
			calendars = []string{eff.Calendar}
		}
		availability, conflicts, _ := e.check(ctx, actor, calendars, start, end,
			map[string]bool{eff.UID: true, runtimedb.MailEventUID(c.ID): true})
		if availability == runtimedb.MailAvailabilityUnavailable {
			return e.returnToReview(ctx, eff, "the calendar could not be rechecked")
		}
		if newConflict(claimed.Review.Conflicts, conflicts) {
			return e.returnToReview(ctx, eff, "a new conflict appeared since it was offered")
		}
	}
	return e.write(ctx, eff, c, actor)
}

func newConflict(known, now []runtimedb.MailConflict) bool {
	seen := map[string]bool{}
	for _, k := range known {
		seen[conflictKey(k)] = true
	}
	for _, n := range now {
		if !seen[conflictKey(n)] {
			return true
		}
	}
	return false
}

func conflictKey(c runtimedb.MailConflict) string {
	return c.Calendar + "\x00" + c.Title + "\x00" + c.Start.UTC().Format(time.RFC3339) + "\x00" + c.End.UTC().Format(time.RFC3339)
}

// returnToReview records the effect as not having happened and offers
// the candidate afresh.
func (e *Engine) returnToReview(ctx context.Context, eff runtimedb.MailEffect, why string) (Result, error) {
	c, err := e.DB.ReturnMailEffect(eff.Seq, runtimedb.MailEffectReturned, why, e.now())
	if err != nil {
		return Result{}, err
	}
	out := Result{Outcome: OutcomeReturned, Candidate: c, Message: "Not added: " + why + ". It is back for review."}
	fresh, err := e.Offer(ctx, c.ID)
	if err != nil {
		e.log().Warn("mail review: re-offering a returned candidate failed", "candidate", c.ID, "err", err)
	} else {
		out.Review = &fresh
		out.Candidate, _ = e.DB.MailCandidateInternal(c.ID)
	}
	return out, nil
}

// maxWriteAttempts bounds how often one Act sends the write. The second
// is only ever sent after a lookup found the first did not land.
const maxWriteAttempts = 2

// write sends the effect and settles it: confirmed when the calendar
// answers, reconciled when an indeterminate answer is resolved by
// looking the UID up, returned when it is found absent after an
// interruption, and left dispatching (for the reconciler) when the
// calendar cannot even be asked.
func (e *Engine) write(ctx context.Context, eff runtimedb.MailEffect, c runtimedb.MailCandidate, actor string) (Result, error) {
	w := eventWrite(c.Fields, eff.UID)
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		if err := e.DB.NoteMailEffectAttempt(eff.Seq); err != nil {
			return Result{}, err
		}
		var href, etag string
		var err error
		switch eff.Kind {
		case runtimedb.MailReviewCreate:
			href, etag, err = e.Calendar.CreateExact(ctx, actor, eff.Calendar, w)
		case runtimedb.MailReviewUpdate:
			href, etag, err = e.Calendar.UpdateExact(ctx, actor, eff.Calendar, w)
		case runtimedb.MailReviewCancel:
			err = e.Calendar.DeleteExact(ctx, actor, eff.Calendar, eff.UID)
		}
		if err == nil {
			return e.settle(eff, runtimedb.MailEffectConfirmed, href, etag, "")
		}
		if errors.Is(err, caldav.ErrWriteForbidden) || errors.Is(err, domestic.ErrCalendarAccess) {
			if _, rerr := e.DB.ReturnMailEffect(eff.Seq, runtimedb.MailEffectFailed, "the calendar refused the write", e.now()); rerr != nil {
				return Result{}, rerr
			}
			return Result{}, fmt.Errorf("%w: %v", ErrCalendarRefused, err)
		}
		// Indeterminate: a timeout, a lost response, an unavailable
		// calendar, or a conditional write refused because the UID is
		// already taken. The UID decides; nothing is resent until the
		// calendar has been asked.
		e.log().Info("mail review: indeterminate calendar write; reconciling by UID", "candidate", c.ID,
			"kind", eff.Kind, "attempt", attempt+1, "err", err)
		state, href, etag, lerr := e.lookup(ctx, actor, eff, c)
		switch {
		case lerr != nil:
			return Result{Outcome: OutcomePending, Candidate: c, Effect: &eff,
				Message: "The calendar did not answer; this will be checked before anything is tried again."}, nil
		case state == lookupDone:
			return e.settle(eff, runtimedb.MailEffectReconciled, href, etag, "found by its UID after an indeterminate write")
		case state == lookupDiffers:
			// Our UID holds an earlier approved version of this event:
			// make it exactly what was approved now, still one event.
			eff.Kind = runtimedb.MailReviewUpdate
		}
	}
	return Result{Outcome: OutcomePending, Candidate: c, Effect: &eff,
		Message: "The calendar did not confirm the write; it will be checked before anything is tried again."}, nil
}

func (e *Engine) settle(eff runtimedb.MailEffect, state, href, etag, detail string) (Result, error) {
	c, err := e.DB.SettleMailEffect(eff.Seq, state, href, etag, detail, e.now())
	if err != nil {
		return Result{}, err
	}
	settled, _ := e.DB.GetMailEffect(eff.Seq)
	out := Result{Candidate: c, Effect: &settled}
	switch eff.Kind {
	case runtimedb.MailReviewCancel:
		out.Outcome, out.Message = OutcomeCancelled, "Removed from "+eff.Calendar+"."
	case runtimedb.MailReviewUpdate:
		out.Outcome, out.Message = OutcomeUpdated, "Updated on "+eff.Calendar+"."
		if settled.Kind == runtimedb.MailReviewCreate {
			out.Outcome, out.Message = OutcomeAdded, "Added to "+eff.Calendar+"."
		}
	default:
		out.Outcome, out.Message = OutcomeAdded, "Added to "+eff.Calendar+"."
	}
	return out, nil
}

type lookupState int

const (
	lookupNotDone lookupState = iota
	lookupDone
	lookupDiffers
)

// lookup asks the calendar whether the effect happened: for a create or
// update, whether the UID holds exactly the approved event; for a
// cancel, whether it is gone.
func (e *Engine) lookup(ctx context.Context, actor string, eff runtimedb.MailEffect, c runtimedb.MailCandidate) (lookupState, string, string, error) {
	ev, etag, err := e.Calendar.EventByUID(ctx, actor, eff.Calendar, eff.UID)
	if err != nil {
		return lookupNotDone, "", "", err
	}
	if eff.Kind == runtimedb.MailReviewCancel {
		if ev == nil {
			return lookupDone, "", "", nil
		}
		return lookupNotDone, "", "", nil
	}
	if ev == nil {
		return lookupNotDone, "", "", nil
	}
	if sameEvent(*ev, c.Fields) {
		return lookupDone, ev.Href, etag, nil
	}
	return lookupDiffers, ev.Href, etag, nil
}

// sameEvent: the event at our UID is the approved one.
func sameEvent(ev caldav.Event, f runtimedb.MailCandidateFields) bool {
	start, end, ok := runtimedb.MailEventWindow(f)
	if !ok || strings.TrimSpace(ev.Summary) != strings.TrimSpace(f.Title) || ev.AllDay != f.AllDay ||
		strings.TrimSpace(ev.Location) != strings.TrimSpace(f.Location) {
		return false
	}
	if f.AllDay {
		loc := zone(f.TZ)
		return ev.Start.Format("2006-01-02") == start.In(loc).Format("2006-01-02") &&
			(ev.End.IsZero() || ev.End.Format("2006-01-02") == end.In(loc).Format("2006-01-02"))
	}
	return ev.Start.Equal(start) && (ev.End.IsZero() || ev.End.Equal(end))
}

func zone(name string) *time.Location {
	if name != "" {
		if l, err := time.LoadLocation(name); err == nil {
			return l
		}
	}
	return time.UTC
}

// eventWrite is exactly the approved event, under uid.
func eventWrite(f runtimedb.MailCandidateFields, uid string) caldav.EventWrite {
	title, location, notes := f.Title, f.Location, f.Notes
	w := caldav.EventWrite{UID: uid, Summary: &title, AllDay: f.AllDay, Location: &location, Description: &notes}
	start, end, _ := runtimedb.MailEventWindow(f)
	if f.AllDay {
		loc := zone(f.TZ)
		w.Start, w.End = start.In(loc).Format("2006-01-02"), end.In(loc).Format("2006-01-02")
	} else {
		w.Start, w.End = start.Format(time.RFC3339), end.Format(time.RFC3339)
	}
	return w
}
