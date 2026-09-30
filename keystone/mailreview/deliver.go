package mailreview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// NoticeKind is the notice class of a mail review. It is not urgent: a
// review raised in quiet hours waits for the morning.
const NoticeKind = "mail.review"

// deliver sends the review to its Person, privately.
//
// Always as an in-app notice for that Person (Console and Homepage show
// only their own), which is NOT an asking notice: a "yes" typed against
// it answers nothing (runtimedb.AnswerNotice refuses it). And, when they
// have one, as a push on their authenticated one-to-one chat Connection
// — the route the review is bound to. Never a room, never a broadcast,
// never another Person or the Owner. The text carries no token: a chat
// reply names the exact candidate and revision, and the deterministic
// /mail command checks the same binding, expiry and single use.
func (e *Engine) deliver(r runtimedb.MailReview, c runtimedb.MailCandidate, handle string, b runtimedb.ChatBinding, direct bool, at time.Time) error {
	body := reviewText(r, c)
	settings, _, err := e.DB.GetButlerSettings()
	if err != nil {
		return err
	}
	ttl := r.ExpiresAt.Sub(at)
	decision, err := e.DB.RaiseNotice(runtimedb.NoticeRequest{
		Kind: NoticeKind, Recipient: handle, CorrelationID: c.ID,
		DedupeKey: "mail-review:" + r.ID, Body: body, TTL: ttl,
	}, settings, at)
	if err != nil {
		return err
	}
	if decision.Seq != 0 {
		if err := e.DB.SetMailReviewNotice(r.ID, decision.Seq); err != nil {
			return err
		}
	}
	if !direct {
		return nil
	}
	msg := runtimedb.ChatOutbound{
		Connection: b.Connection, Target: b.Conversation, BindingID: b.ID, Kind: "push", Body: body,
		CorrelationID: c.ID, DedupeKey: "mail-review:" + r.ID, ExpiresAt: r.ExpiresAt,
	}
	if settings.InQuietHours(at) {
		msg.HoldUntil = settings.QuietHoursEnd(at)
	}
	if _, err := e.DB.EnqueueChatOutbound(msg); err != nil && !errors.Is(err, runtimedb.ErrDuplicateOutbound) {
		return err
	}
	return nil
}

// reviewText is the review as its Person reads it. Minimal fields and
// the conflicts they may see; no excerpt, no token.
func reviewText(r runtimedb.MailReview, c runtimedb.MailCandidate) string {
	f := c.Fields
	loc := zone(f.TZ)
	var b strings.Builder
	switch r.Kind {
	case runtimedb.MailReviewCancel:
		b.WriteString("A message says this event on your calendar was cancelled:\n")
	case runtimedb.MailReviewUpdate:
		b.WriteString("A later message changes an event already on your calendar. Proposed:\n")
	default:
		b.WriteString("A possible event from your mail (only you can see this):\n")
	}
	title := f.Title
	if title == "" {
		title = "(no title)"
	}
	b.WriteString(title)
	b.WriteString("\n")
	if start, end, ok := runtimedb.MailEventWindow(f); ok {
		if f.AllDay {
			fmt.Fprintf(&b, "%s, all day", start.In(loc).Format("Mon 2 Jan 2006"))
		} else {
			fmt.Fprintf(&b, "%s–%s (%s)", start.In(loc).Format("Mon 2 Jan 2006 15:04"), end.In(loc).Format("15:04"), loc)
		}
	} else {
		fmt.Fprintf(&b, "Missing: %s", strings.Join(append(append([]string{}, f.Missing...), f.Ambiguous...), ", "))
	}
	if f.Location != "" {
		b.WriteString(" · " + f.Location)
	}
	fmt.Fprintf(&b, "\nCalendar: %s\n", r.TargetCalendar)
	switch r.Availability {
	case runtimedb.MailAvailabilityFree:
		b.WriteString("Nothing else on the calendars checked.\n")
	case runtimedb.MailAvailabilityBusy:
		fmt.Fprintf(&b, "Conflicts you can see (%d):\n", len(r.Conflicts))
		for _, x := range r.Conflicts {
			fmt.Fprintf(&b, "- [%s] %s %s\n", x.Calendar, x.Title, x.Start.In(loc).Format("Mon 15:04"))
		}
	case runtimedb.MailAvailabilityUnavailable:
		b.WriteString("The calendar could not be checked for conflicts just now.\n")
	}
	for _, a := range r.Actions {
		switch a {
		case runtimedb.MailActionAdd, runtimedb.MailActionApply, runtimedb.MailActionDismiss:
			fmt.Fprintf(&b, "Reply /mail %s %s %d to %s.\n", a, c.ID, r.Revision, actionVerb(r.Kind, a))
		case runtimedb.MailActionEdit:
			b.WriteString("Edit it in the household dashboard.\n")
		}
	}
	fmt.Fprintf(&b, "This expires %s. Nothing is added unless you choose it.", r.ExpiresAt.In(loc).Format("Mon 2 Jan 15:04"))
	return b.String()
}

func actionVerb(kind, action string) string {
	switch {
	case action == runtimedb.MailActionAdd:
		return "add it"
	case action == runtimedb.MailActionApply && kind == runtimedb.MailReviewCancel:
		return "remove it"
	case action == runtimedb.MailActionApply:
		return "apply the change"
	case kind == runtimedb.MailReviewCancel:
		return "keep it"
	case kind == runtimedb.MailReviewUpdate:
		return "keep the event as it is"
	}
	return "dismiss it"
}

// Reconcile closes reviews that ran out or moved on, and settles
// effects a crash, a restart or an unanswered lookup left dispatching.
// It never resends a create or an update: a write whose UID is absent
// after an interruption goes back to review (with a fresh token), and
// only a new approval sends it again — still under If-None-Match on the
// same UID, so it can never make a second event.
func (e *Engine) Reconcile(ctx context.Context) error {
	at := e.now()
	if _, err := e.DB.ExpireMailReviews(at); err != nil {
		return err
	}
	if e.Calendar == nil {
		return nil
	}
	settle := e.SettleAfter
	if settle <= 0 {
		settle = 2 * time.Minute
	}
	effects, err := e.DB.InflightMailEffects(at.Add(-settle))
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, eff := range effects {
		c, err := e.DB.MailCandidateInternal(eff.Candidate)
		if err != nil {
			continue
		}
		handle, err := e.DB.PersonHandle(c.PersonID)
		if err != nil || handle == "" {
			continue
		}
		state, href, etag, err := e.lookup(ctx, handle, eff, c)
		if err != nil {
			e.log().Info("mail review: an in-flight effect could not be looked up yet", "candidate", c.ID, "err", err)
			continue
		}
		switch state {
		case lookupDone:
			if _, err := e.settle(eff, runtimedb.MailEffectReconciled, href, etag, "found by its UID after an interruption"); err != nil {
				return err
			}
		default:
			if _, err := e.DB.ReturnMailEffect(eff.Seq, runtimedb.MailEffectReturned,
				"the write was interrupted and did not land", e.now()); err != nil {
				return err
			}
			if _, err := e.Offer(ctx, c.ID); err != nil {
				e.log().Warn("mail review: re-offering after an interrupted write failed", "candidate", c.ID, "err", err)
			}
		}
	}
	return nil
}

// RunReconciler reconciles every interval until ctx is done.
func RunReconciler(ctx context.Context, e *Engine, every time.Duration, log *slog.Logger) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := e.Reconcile(ctx); err != nil && log != nil {
			log.Warn("mail review reconcile", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
