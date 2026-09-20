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

import (
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
)

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
		// No resolver underneath. On the ADMINISTRATIVE path that is a
		// real state and not the same as "everything is disabled" (see
		// butler/authority.go): the ceiling has already had its say,
		// and the rest behaves as an unconfigured deployment does.
		//
		// On the PERSON-FACING path it is a denial, for the reason the
		// seam gives: nobody has said this person may do this, and
		// §10.1 answers that with no. Repeated here rather than left to
		// the seam because this decorator answers in the seam's place
		// once it is wrapped, and a ceiling that fell open underneath
		// would be the permissive default surviving inside the one
		// component whose entire job is to narrow.
		if q.Caller == butler.CallerPersonFacing {
			return butler.ToolDecision{
				Allowed: false,
				Reason: fmt.Sprintf(
					"nothing on this deployment can say whether %s may use %s, so it was not run",
					q.Person, q.Tool),
			}, nil
		}
		return butler.ToolDecision{Allowed: q.Structural}, nil
	}
	return c.inner.AuthorizeTool(q)
}

// AdmitPerson forwards layer 1 to the resolver underneath.
//
// A decorator has to carry it for the reason it carries AdmitResource:
// once wrapped, this type IS the authorizer the seam sees, and a
// wrapper that did not implement it would make every person-facing call
// unanswerable the moment a connection ceiling was bound. Forwarding
// keeps the ceiling a ceiling — it still cannot turn a no into a yes.
func (c connectionCeiling) AdmitPerson(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	pa, ok := c.inner.(butler.PersonAuthorizer)
	if !ok {
		return butler.ToolDecision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"nothing on this deployment can say whether %s may use %s, so it was not run",
				q.Person, q.Tool),
		}, nil
	}
	return pa.AdmitPerson(q)
}

// AdmitResource forwards layer 2 to the resolver underneath.
//
// A decorator has to carry it: the seam asks its authorizer whether it
// can admit a resource, and a wrapper that did not implement the
// interface would make every resource unreachable the moment a
// connection ceiling was bound. Forwarding keeps the ceiling a ceiling
// — it still cannot turn a no into a yes, because a tool the connection
// refuses never reaches this far.
func (c connectionCeiling) AdmitResource(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	ra, ok := c.inner.(butler.ResourceAuthorizer)
	if !ok {
		return butler.ToolDecision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"%s reaches a %s, and nothing here decides who may reach one, so it was not run",
				q.Tool, q.Resource.Kind),
		}, nil
	}
	return ra.AdmitResource(q)
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
