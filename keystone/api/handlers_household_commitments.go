package api

// Managing commitments from the Console.
//
// THE CONSOLE COULD READ THEM AND NOTHING ELSE. An Owner looking at the
// commitments page could see a duplicate somebody had made and had no
// way to remove it: every write lived on the household surface, behind
// a portal session and membership of homepage_users. An Owner is not
// necessarily in that group, and "sign in to the family dashboard to
// delete a typo" is not an answer.
//
// ONE IMPLEMENTATION, TWO DOORS. These are adapters, not a second
// authoring path. Each resolves the acting person and the desired
// state the portal chain is handed for free, then calls the very
// function the dashboard calls. handlers_portal_authoring.go keeps the
// structural guarantee it documents — a request body becomes typed
// fields and never a Kind — and it keeps it for this door too, because
// this door does not parse anything of its own.
//
// WHAT THE VERBS MEAN HERE. Reading is VerbRead, as it was. Writing is
// VerbManage: the Console is the operator surface, and an Owner acting
// on somebody else's commitment is an administrative act rather than
// the household member's own. The dashboard's rule — a Member may add
// their own without an Owner approving a Plan (ADR-W024, and the long
// comment in handlers_portal_authoring.go) — is unchanged and still
// lives there.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// consoleCommitmentCall runs a portal authoring handler as the Console.
//
// The portal chain hands a handler the member and the desired state
// because it has already resolved both. Here they are resolved once,
// in one place, so no individual route can get it subtly wrong.
func (s *Server) consoleCommitmentCall(
	fn func(http.ResponseWriter, *http.Request, string, []resource.Resource) (any, int, error),
) func(*http.Request, principal.Identity) (any, int, error) {
	return func(r *http.Request, id principal.Identity) (any, int, error) {
		member := actingMember(id)
		if member == "" {
			// Every commitment record names who did it. A write with
			// nobody behind it would leave a ledger row attributed to
			// nothing, which is worse than a refusal.
			return nil, http.StatusForbidden,
				fmt.Errorf("this action records who took it, and this credential names nobody")
		}
		if s.State == nil {
			return nil, http.StatusServiceUnavailable, fmt.Errorf("this is not ready yet")
		}
		// The revision and the state in one read, as the portal chain
		// reads them: a write commits only while the accepted revision
		// is still this one. Without it the supersede-on-drift check
		// sees no basis and refuses every write with a 409.
		at, desired, err := s.State.Snapshot()
		if err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("reading desired state: %w", err)
		}
		// The portal handlers resolve their own `who` from the request,
		// and a Console request carries no forwarded household groups.
		// adminActor tells s.who that this caller is the operator
		// surface; it is a context value set only here.
		ctx := withAdminActor(context.WithValue(r.Context(), basedOnKey{}, at))
		return fn(nil, r.WithContext(ctx), member, desired)
	}
}

// adminActorKey marks a request as coming from the operator surface.
//
// A context value rather than a parameter because the portal handlers
// resolve their own `who` from the request, and threading a flag
// through every one of them would be a change to the surface that has
// nothing wrong with it. Unexported key, unexported setter, set in one
// function that is already behind VerbManage: there is no request a
// caller can shape that turns this on.
type adminActorKey struct{}

func withAdminActor(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminActorKey{}, true)
}

func isAdminActor(r *http.Request) bool {
	if r == nil {
		return false
	}
	on, _ := r.Context().Value(adminActorKey{}).(bool)
	return on
}
