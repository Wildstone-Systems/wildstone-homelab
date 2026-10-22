package api

// Step-up for consequential Console operations (ADR-W033 A4, amended
// A4.1; #205).
//
// Entering the Console needs a sign-in inside the entry window. The
// operations listed here need more: one inside the step-up window (15
// minutes by default). An Owner who signed in this morning can read,
// admit, add and configure all day. To offboard somebody, delete an
// Agent, restore a backup or mint a credential, they confirm their
// password first.
//
// A4.1 keeps the list short on purpose. A household reaches its homelab
// over a network it controls, so the gate is for what is destructive or
// hard to undo, not for every write. Minting a bearer credential is on
// the list because bearer tokens are exempt from the gate: an ungated
// mint would let a stale session escape it.
//
// The refusal is not a bare 403. It carries stepUp, the sign-in that
// asks Authelia to authenticate again (prompt=login), and the Console
// sends the person there. When they come back, the Console brings them
// to the same screen and they press the button again. Nothing is
// replayed automatically: a write re-sent after a redirect is a write
// the person did not see themselves make.
//
// Bearer tokens are exempt (A4.1): the CLI, the installer and host
// recovery have no browser session to go stale. An identity with no
// AuthTime is a bearer identity; one that arrived through the trusted
// ingress always has one, because consoleForwardAuth refuses it
// otherwise.

import (
	"fmt"
	"net/http"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/consoleauth"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
)

// errStepUp is the message a consequential operation is refused with
// when the sign-in behind it is too old.
var errStepUp = fmt.Errorf("confirm your password to do this: your last sign-in was more than a few minutes ago")

// stepUpRefusal is the refusal for an identity whose sign-in is too old
// for a consequential operation. A nil error means it may proceed.
func (s *Server) stepUpRefusal(id principal.Identity) (any, int, error) {
	if id.AuthTime == nil || consoleauth.Fresh(*id.AuthTime, s.now(), s.consoleStepUpWindow()) {
		return nil, 0, nil
	}
	return map[string]any{"stepUp": consoleSigninPath + "?stepup=1"}, http.StatusForbidden, errStepUp
}

// consequential gates a whole route on a fresh sign-in.
func (s *Server) consequential(h handlerFunc) handlerFunc {
	return func(r *http.Request, id principal.Identity) (any, int, error) {
		if body, code, err := s.stepUpRefusal(id); err != nil {
			return body, code, err
		}
		return h(r, id)
	}
}
