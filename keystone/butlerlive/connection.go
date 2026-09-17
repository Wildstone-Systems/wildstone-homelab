package butlerlive

// The connection ceiling, as a decorator around whatever resolver a
// session already has.
//
// A DECORATOR RATHER THAN A CHECK INSIDE THE RESOLVER, because that is
// what makes it a ceiling instead of a filter. The resolver underneath
// answers the question it always answered — is this tool registered,
// has an administrator disabled it, does this work hold a grant — and
// this wrapper can only ever turn a yes into a no. There is no argument
// it takes, no grant it consults and no configuration that reorders it,
// so "an operator capability is refused from the dashboard even when
// the request is hand-crafted, and even if an owner-capable assistant
// is exposed here by mistake" is true by the shape of the code rather
// than by everybody remembering to check.
//
// It is bound from the TURN, which carries the connection the request
// arrived through. The portal's ask handler sets it; the chat path and
// the Console leave it empty and nothing is wrapped.

import "github.com/chickenrat17/wildstone-homelab/keystone/butler"

// connectionCeiling refuses tools a named connection denies, ahead of
// the resolver it wraps.
type connectionCeiling struct {
	// permits is the connection's own rule. Injected rather than
	// imported by name so this package does not learn about the portal:
	// the caller that knows which connection a turn arrived through is
	// the caller that supplies it.
	permits func(tool string) (bool, string)
	// inner is the authority that would have decided. Never consulted
	// for a denied tool — the point of a ceiling is that what is
	// underneath it cannot raise it.
	inner butler.ToolAuthorizer
}

func (c connectionCeiling) AuthorizeTool(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	if ok, reason := c.permits(q.Tool); !ok {
		return butler.ToolDecision{
			Allowed: false,
			Reason:  reason,
			// NOT requestable. "Requestable" means a person could change
			// the answer by asking, and this answer is not about
			// permission — it is about which surface the request came
			// from. Offering to file a request for it would send a
			// household member to ask the owner for something no
			// approval can grant here.
			Requestable: false,
		}, nil
	}
	if c.inner == nil {
		// No resolver underneath means no administrative controls are
		// configured, which is a real state and not the same as
		// "everything is disabled" (see butler/authority.go). The
		// ceiling has already had its say; the rest behaves as an
		// unconfigured deployment does.
		return butler.ToolDecision{Allowed: q.Structural}, nil
	}
	return c.inner.AuthorizeTool(q)
}

// WithConnection wraps an authorizer in a connection's ceiling. A nil
// permits leaves the authorizer alone, so a turn that names no
// connection is unaffected.
func WithConnection(inner butler.ToolAuthorizer, permits func(string) (bool, string)) butler.ToolAuthorizer {
	if permits == nil {
		return inner
	}
	return connectionCeiling{permits: permits, inner: inner}
}
