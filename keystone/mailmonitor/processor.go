// Package mailmonitor turns one Person's new mail into private, versioned
// calendar event candidates (epic #326 S05, #458; ADR-W030 A5.6).
//
// A scheduled run (S03) hands the Dispatcher a claimed run; it queues one
// bounded mail.changes read against the Person's connection, and when the
// adapter answers, the Processor reads each message:
//
//  1. deduplicate by the adapter's keyed fingerprint (delivery is
//     at-least-once: a replay or a UIDVALIDITY reset changes nothing);
//  2. an iCalendar part is read DETERMINISTICALLY and no model is asked;
//  3. otherwise, and only under the Person's authorized processing
//     location, a model fills a strict JSON object that Keystone
//     validates, with one bounded repair — a local-only source never
//     reaches a cloud route, on any path;
//  4. the result is recorded (runtimedb.RecordMailMessage), a correction
//     superseding what it corrects;
//  5. only then is the checkpoint committed and the review hook told.
//
// Mail is untrusted evidence throughout. It never chooses the target
// calendar, the audience, the state or an approval; the full body is
// never stored, logged or returned — only minimal fields and a short
// sealed excerpt while the candidate is unresolved.
package mailmonitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail/imapread/mimenorm"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/householdtz"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// MaxModelAttempts bounds how many runs retry a message whose model
// extraction failed transiently before it is recorded extraction_failed
// and the mailbox moves on.
const MaxModelAttempts = 3

// Message is one message of a mail.changes answer.
type Message struct {
	Ref         string            `json:"ref"`
	Fingerprint string            `json:"fingerprint"`
	Folder      string            `json:"folder"`
	Size        int64             `json:"size"`
	Content     *mimenorm.Message `json:"content"`
}

// Source is the run's authority, from runtimedb.AuthorizeBackgroundUse:
// which connection, at which epoch, for which Person, with which
// processing location and target calendar.
type Source struct {
	Connection         string
	Epoch              int64
	PersonID           string
	Agent              int64
	ProcessingLocation string
	TargetCalendar     string
	Run                int64
}

// SourceFromGrant builds a Source from a live grant.
func SourceFromGrant(g runtimedb.BackgroundGrant, personID string, run int64) Source {
	return Source{Connection: g.Connection, Epoch: g.LeaseEpoch, PersonID: personID, Agent: g.Agent,
		ProcessingLocation: g.ProcessingLocation, TargetCalendar: g.TargetCalendar, Run: run}
}

// ErrModelUnavailable is the batch error when a message could not be
// read because every eligible model route failed. The run must not
// commit its cursor; the next run retries that message.
var ErrModelUnavailable = errors.New("mailmonitor: model extraction is temporarily unavailable")

// Processor reads messages into candidates.
type Processor struct {
	DB *runtimedb.DB
	// Vault seals excerpts; nil keeps none.
	Vault *runtimedb.MailExcerptVault
	// Zone is the household's timezone; nil is UTC.
	Zone      *householdtz.Zone
	Extractor *Extractor
	// Hook receives durable candidate changes; nil is NoopHook.
	Hook CandidateHook
	Now  func() time.Time
	Log  *slog.Logger
	// Window and ExcerptWindow override the unresolved-candidate and
	// excerpt lifetimes.
	Window, ExcerptWindow time.Duration
}

func (p *Processor) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p *Processor) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Processor) hook() CandidateHook {
	if p.Hook != nil {
		return p.Hook
	}
	return NoopHook{}
}

// Outcome is one message's result, for tests and run health. It carries
// no message content.
type Outcome struct {
	Fingerprint string
	Duplicate   bool
	State       string
	Reason      string
	Source      string
	Candidates  []string
}

// ProcessBatch records every message, in order. It stops at the first
// transient model failure (ErrModelUnavailable) or fence; everything
// recorded before that stays recorded and dedupes on replay.
func (p *Processor) ProcessBatch(ctx context.Context, src Source, msgs []Message) ([]Outcome, error) {
	var out []Outcome
	var advisory []runtimedb.MailCandidateChange
	var stop error
	for _, m := range msgs {
		if err := ctx.Err(); err != nil {
			stop = err
			break
		}
		o, changes, err := p.processOne(ctx, src, m)
		if err != nil {
			stop = err
			if o.State != "" {
				out = append(out, o)
			}
			break
		}
		out = append(out, o)
		for _, c := range changes {
			if c.Kind == runtimedb.MailChangeSuperseded || c.Kind == runtimedb.MailChangeCancelled {
				advisory = append(advisory, c)
			}
		}
	}
	p.announce(ctx, src.Connection, advisory)
	return out, stop
}

func (p *Processor) processOne(ctx context.Context, src Source, m Message) (Outcome, []runtimedb.MailCandidateChange, error) {
	o := Outcome{Fingerprint: m.Fingerprint}
	state, attempts, seen, err := p.DB.MailMessageState(src.Connection, m.Fingerprint)
	if err != nil {
		return o, nil, err
	}
	if seen && state != runtimedb.MailMessageRetrying {
		o.Duplicate, o.State = true, state
		return o, nil, nil
	}
	rec := runtimedb.MailMessageRecord{
		Connection: src.Connection, Epoch: src.Epoch, PersonID: src.PersonID, Fingerprint: m.Fingerprint,
		SourceRef: runtimedb.BoundMailText(m.Ref, 512), Run: src.Run, TargetCalendar: src.TargetCalendar,
		At: p.now(), Window: p.Window, ExcerptWindow: p.ExcerptWindow,
	}
	transient := p.read(ctx, src, m, attempts, &rec)
	res, err := p.DB.RecordMailMessage(p.Vault, rec)
	if err != nil {
		return o, nil, err
	}
	o.Duplicate, o.State, o.Reason, o.Source = res.Duplicate, res.State, res.Reason, rec.Source
	for _, c := range res.Changes {
		if c.Kind == runtimedb.MailChangeNew {
			o.Candidates = append(o.Candidates, c.Candidate.ID)
		}
	}
	if transient {
		return o, res.Changes, ErrModelUnavailable
	}
	return o, res.Changes, nil
}

// read fills rec's state or drafts. It reports transient=true when the
// message must be retried by a later run.
func (p *Processor) read(ctx context.Context, src Source, m Message, attempts int, rec *runtimedb.MailMessageRecord) (transient bool) {
	c := m.Content
	skip := func(reason string) bool {
		rec.State, rec.Reason = runtimedb.MailMessageSkipped, reason
		return false
	}
	if c == nil {
		return skip("no_content")
	}
	switch c.Outcome {
	case mimenorm.Oversized:
		return skip("oversized")
	case mimenorm.TooDeep:
		return skip("too_deep")
	case mimenorm.Malformed:
		if len(c.Calendars) == 0 && strings.TrimSpace(c.Text) == "" {
			return skip("malformed")
		}
	}
	household := p.Zone.Or(time.UTC)
	keys := messageKeys(src.Connection, c)
	now := p.now()

	if len(c.Calendars) > 0 {
		rec.Source = runtimedb.MailSourceICS
		ics := extractICS(c.Calendars, household)
		if len(ics.events) == 0 {
			rec.State, rec.Reason = ics.state, ics.reason
			return false
		}
		for _, ev := range ics.events {
			d := keys.draft()
			d.Source = runtimedb.MailSourceICS
			d.UIDKey = key("uid", src.Connection, ev.uid)
			d.ICSSequence = ev.sequence
			d.Cancel = ev.cancel
			d.Fields, d.Excerpt = ev.fields, ev.excerpt
			if !d.Cancel && past(d.Fields, now) {
				continue
			}
			rec.Drafts = append(rec.Drafts, d)
		}
		if len(rec.Drafts) == 0 {
			rec.State, rec.Reason = runtimedb.MailMessageNoEvent, "past_event"
		}
		return false
	}

	// Prose. A message with no date or time anywhere is not an
	// invitation, and no model needs to read it to say so.
	subject := c.Headers.Subject
	if strings.TrimSpace(c.Text) == "" {
		rec.State, rec.Reason = runtimedb.MailMessageNoEvent, "no_text"
		return false
	}
	if !dateEvidence.MatchString(subject+"\n"+c.Text) && !timeEvidence.MatchString(subject+"\n"+c.Text) {
		rec.State, rec.Reason = runtimedb.MailMessageNoEvent, "no_date_evidence"
		return false
	}
	rec.Source = runtimedb.MailSourceModel
	in := ExtractInput{Agent: src.Agent, Processing: src.ProcessingLocation, Subject: subject,
		Sent: c.Headers.Date, Text: c.Text, Household: household}
	res := p.Extractor.Extract(ctx, in)
	switch res.Status {
	case ExtractNoModel:
		return skip(res.Reason)
	case ExtractFailed:
		rec.State, rec.Reason = runtimedb.MailMessageExtractionFailed, res.Reason
		return false
	case ExtractUnavailable:
		if attempts+1 >= MaxModelAttempts {
			rec.State, rec.Reason = runtimedb.MailMessageExtractionFailed, res.Reason
			return false
		}
		rec.State, rec.Reason = runtimedb.MailMessageRetrying, res.Reason
		return true
	}
	ev, isEvent := resolveModel(res.Output, in)
	if !isEvent {
		rec.State, rec.Reason = runtimedb.MailMessageNoEvent, ""
		return false
	}
	if past(ev.fields, now) {
		rec.State, rec.Reason = runtimedb.MailMessageNoEvent, "past_event"
		return false
	}
	d := keys.draft()
	d.Source = runtimedb.MailSourceModel
	d.Fields, d.Excerpt = ev.fields, ev.excerpt
	d.Correction = ev.correction || correctionMarker.MatchString(subject)
	rec.Drafts = append(rec.Drafts, d)
	return false
}

// past reports whether an event is already over. An event with no known
// time is never past: it needs details, not a verdict.
func past(f runtimedb.MailCandidateFields, now time.Time) bool {
	end := f.End
	if end.IsZero() {
		end = f.Start
	}
	return !end.IsZero() && !end.After(now)
}

// correlation is a message's connection-scoped header digests.
type correlation struct {
	message, sender, subject string
	thread                   []string
}

func messageKeys(connection string, c *mimenorm.Message) correlation {
	k := correlation{
		message: key("message-id", connection, strings.ToLower(c.Headers.MessageID)),
		subject: key("subject", connection, normalizeSubject(c.Headers.Subject)),
	}
	if len(c.Headers.From) > 0 {
		k.sender = key("sender", connection, strings.ToLower(c.Headers.From[0].Address))
	}
	refs := append([]string{c.Headers.InReplyTo}, c.Headers.References...)
	for _, r := range refs {
		if len(k.thread) >= 20 {
			break
		}
		if d := key("message-id", connection, strings.ToLower(r)); d != "" {
			k.thread = append(k.thread, d)
		}
	}
	return k
}

func (k correlation) draft() runtimedb.MailCandidateDraft {
	return runtimedb.MailCandidateDraft{MessageKey: k.message, SenderKey: k.sender, SubjectKey: k.subject,
		ThreadKeys: k.thread}
}

// announce hands durable changes to the hook: every unannounced
// candidate on the connection (new ones, and committed ones with an
// update pending), then the advisory ones. A hook error leaves the
// candidate unannounced for the next run.
func (p *Processor) announce(ctx context.Context, connection string, advisory []runtimedb.MailCandidateChange) {
	h := p.hook()
	pending, err := p.DB.UnannouncedMailCandidates(connection, 0)
	if err != nil {
		p.log().Warn("mail monitor: listing unannounced candidates failed", "connection", connection, "err", err)
		return
	}
	for _, c := range pending {
		kind := runtimedb.MailChangeNew
		if c.UpdatePending != runtimedb.MailUpdateNone {
			kind = runtimedb.MailChangeUpdateNeeded
		}
		if err := h.CandidateChanged(ctx, runtimedb.MailCandidateChange{Kind: kind, Candidate: c}); err != nil {
			if !errors.Is(err, ErrNoReview) {
				p.log().Warn("mail monitor: review hook refused a candidate", "candidate", c.ID, "err", err)
			}
			continue
		}
		if err := p.DB.MarkMailCandidateAnnounced(c.ID, p.now()); err != nil {
			p.log().Warn("mail monitor: marking a candidate announced failed", "candidate", c.ID, "err", err)
		}
	}
	for _, c := range advisory {
		if err := h.CandidateChanged(ctx, c); err != nil && !errors.Is(err, ErrNoReview) {
			p.log().Warn("mail monitor: review hook refused a change", "candidate", c.Candidate.ID, "kind", c.Kind, "err", err)
		}
	}
}

// String summarizes an outcome without content.
func (o Outcome) String() string {
	return fmt.Sprintf("%s:%s/%s", o.Fingerprint, o.State, o.Reason)
}
