package mailmonitor

import (
	"context"
	"errors"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// CandidateHook is how a candidate reaches its Person's private review
// (S06). It is called only after the candidate is durable.
//
// Delivery is durable for the changes that need an answer: a
// runtimedb.MailChangeNew or runtimedb.MailChangeUpdateNeeded is handed
// over from UnannouncedMailCandidates and marked announced only when
// CandidateChanged returns nil, so a crash or an error re-offers it on
// the next run. Superseded and cancelled changes are advisory (a review
// token bound to a revision no longer proposed fails on its own) and are
// offered once.
//
// The candidate carries only minimal fields, never the message or its
// excerpt; a hook that needs the excerpt reads it Person-scoped through
// runtimedb.ReadMailCandidateExcerpt. A hook must deliver only to an
// authenticated one-to-one Connection for change.Candidate.PersonID.
type CandidateHook interface {
	CandidateChanged(ctx context.Context, change runtimedb.MailCandidateChange) error
}

// CandidateHookFunc adapts a function to CandidateHook.
type CandidateHookFunc func(ctx context.Context, change runtimedb.MailCandidateChange) error

// CandidateChanged calls f.
func (f CandidateHookFunc) CandidateChanged(ctx context.Context, change runtimedb.MailCandidateChange) error {
	return f(ctx, change)
}

// ErrNoReview is what NoopHook answers: nothing was delivered, so the
// candidate stays unannounced and the review hook wired later (S06)
// still receives it.
var ErrNoReview = errors.New("mailmonitor: no private review is wired")

// NoopHook sends nothing. It is the default until S06 wires the review:
// candidates are recorded and left unannounced, and nobody is asked.
type NoopHook struct{}

// CandidateChanged delivers nothing and says so.
func (NoopHook) CandidateChanged(context.Context, runtimedb.MailCandidateChange) error {
	return ErrNoReview
}
