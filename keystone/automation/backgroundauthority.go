package automation

// The real BackgroundAuthority (epic #326 integration of S02 into S03):
// the dispatch gate's question — may this exact subscription version use
// this Person's Personal source unattended? — answered by runtimedb's
// live intersection evaluator, and then held to the gate's own view of
// the run so the two can never quietly disagree.
//
// runtimedb.AuthorizeBackgroundUse checks the Person's authorization
// against the Owner's approval. What it cannot know is what the GATE
// is about to dispatch: which Agent the claim names, the connection
// epoch the claim will be fenced under, and the canonical Person the
// run will be authored by. A grant for any other Agent, epoch or Person
// is refused here, so a grant is never stretched over a run it was not
// evaluated for.

import (
	"context"
	"errors"
	"fmt"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// PersonalBackgroundAuthority is the BackgroundAuthority cmd/keystone
// wires. A subscription with no source connection has no Personal source
// to authorize and is refused, as DenyAllBackgroundAuthority always
// refused it: nothing scheduled runs that the Person-authorized,
// Owner-approved intersection does not cover.
type PersonalBackgroundAuthority struct {
	DB *runtimedb.DB
}

// AuthorizeBackgroundUse evaluates use live. nil allows.
func (a PersonalBackgroundAuthority) AuthorizeBackgroundUse(ctx context.Context, use BackgroundUse) error {
	deny := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrBackgroundUseDenied, fmt.Sprintf(format, args...))
	}
	if a.DB == nil {
		return deny("no evaluator is wired")
	}
	if use.Connection == "" {
		return deny("subscription %d names no Personal source to authorize", use.Subscription)
	}
	grant, err := a.DB.AuthorizeBackgroundUse(ctx, use.Connection, use.Subscription, use.SubscriptionVersion)
	if err != nil {
		if errors.Is(err, runtimedb.ErrBackgroundUseDenied) {
			return fmt.Errorf("%w: %w", ErrBackgroundUseDenied, err)
		}
		return err
	}
	if grant.Subscription != use.Subscription || grant.SubscriptionVersion != use.SubscriptionVersion {
		return deny("the grant is for subscription %d version %d, not %d version %d",
			grant.Subscription, grant.SubscriptionVersion, use.Subscription, use.SubscriptionVersion)
	}
	if grant.Agent != use.Butler {
		return deny("the Person authorized Agent %d, not Agent %d", grant.Agent, use.Butler)
	}
	if use.ConnectionEpoch != 0 && grant.LeaseEpoch != use.ConnectionEpoch {
		return deny("the connection's epoch moved (%d, now %d)", use.ConnectionEpoch, grant.LeaseEpoch)
	}
	if use.PersonID == "" {
		return deny("the subscription is not bound to the Person who owns its source")
	}
	member, err := a.DB.GetButlerMember(grant.Person)
	if err != nil || member.PersonID == "" || member.PersonID != use.PersonID {
		return deny("the grant was given by somebody other than the Person this subscription runs for")
	}
	return nil
}
