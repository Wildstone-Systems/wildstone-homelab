package api

// The household portal's door (#197, #198).
//
// Keystone already admits a person by forwarded identity once, for the
// Console (ADR-W032). This is the same mechanism for a different kind
// of caller, and the differences are the entire content of this file:
//
//	Console                        Portal
//	one group, console_users       one group PER SURFACE, declared by
//	                               that surface's own package
//	admin role, every verb         no role and no verb at all
//	the operator's screens         two household surfaces, and nothing
//	                               else on this listener
//
// THE IDENTITY THIS PRODUCES HOLDS NO RBAC VERB, and that is the point
// rather than an omission. A household member reaching Keystone is a
// caller on the same listener that serves /api/v1/resources, and a
// principal role carrying `read` would have handed them the whole
// control plane's state the moment their browser guessed a URL. So a
// portal request is authenticated here, authorised by group membership
// here, and dispatched only to the handlers registered through
// s.portal — which is a closed list, four endpoints per surface, none
// of which can be reached with anything but a member identity.
//
// Everything else is ADR-W032's reasoning, unchanged: the peer must be
// the trusted ingress, because a header anyone can set is not an
// authentication; membership IS the permission, so removing somebody
// from the group revokes them on their next request; and no principal
// row is created, because a persisted row is a second place to revoke
// and a second place to forget.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// portalSurface is one household surface Keystone serves, named by the
// capability whose package declares its Route and its group.
//
// The capability is the key to everything else: the group that grants
// it is the LdapGroup that capability owns, and the surface is
// available at all only while that capability is installed. Neither is
// a constant in Go — an uninstalled capability must not leave an
// endpoint answering, and a group named in two places is a group that
// drifts.
type portalSurface struct {
	capability string
	// title is what a refusal calls this surface, so a household member
	// who is not in the group is told which thing they cannot reach.
	title string
}

var (
	surfaceFiles     = portalSurface{capability: "filedrop", title: "Files"}
	surfaceAssistant = portalSurface{capability: "assistant", title: "Assistant"}
	// The household's own front page. Its capability is `dashboard` —
	// the same capability the Homepage profile installs — because it IS
	// the dashboard, done by Wildstone instead of by a container.
	//
	// That sharing is what makes the surface appear and disappear
	// correctly with no extra rule. Only the wildstone profile declares
	// an LdapGroup, so portalGroup finds one exactly when this profile
	// is the installed one; a household running the Homepage profile has
	// no group for `dashboard`, and these routes are 404 rather than a
	// second front door answering beside the first.
	surfaceHome = portalSurface{capability: "dashboard", title: "Home"}
)

var (
	// errPortalNotInstalled is "nothing declares this surface", which is
	// a 404: an uninstalled capability should look absent, not forbidden.
	errPortalNotInstalled = errors.New("not installed")
	// errPortalDenied covers every other refusal — untrusted peer, no
	// forwarded user, not in the group. The CALLER is told one thing for
	// all three, because the differences between them are exactly what a
	// prober wants.
	//
	// The OPERATOR is told which, in the log, and that is not a
	// contradiction: the first thing this door did on a live household
	// was refuse the person it was installed for, and the audit row said
	// only "403, anonymous". Three conditions and one indistinguishable
	// refusal is right at the boundary and useless behind it. Every
	// refusal below wraps this with the reason.
	errPortalDenied = errors.New("denied")
)

// portalGroup is the directory group that grants a surface, read from
// the LdapGroup its own capability declares.
//
// Derived rather than constant so that the group Keystone enforces and
// the group the identity capability creates are the same string by
// construction. A household that renames it in the package renames it
// here; a household that has not installed the capability has no group,
// and the surface is absent rather than open.
func portalGroup(desired []resource.Resource, capability string) (string, bool) {
	for _, r := range desired {
		if r.Kind != resource.KindLdapGroup || r.LdapGroup == nil {
			continue
		}
		if strings.TrimPrefix(r.Metadata.Owner, "capability/") != capability {
			continue
		}
		if name := strings.TrimSpace(r.LdapGroup.Name); name != "" {
			return name, true
		}
	}
	return "", false
}

// portalMember authenticates the household member behind one portal
// request, and returns the desired state it read on the way — every
// portal handler needs both, and loading it twice per request would be
// two answers to "what is installed" from one request.
func (s *Server) portalMember(r *http.Request, surface portalSurface) (string, []resource.Resource, error) {
	if s.State == nil {
		return "", nil, errPortalNotInstalled
	}
	desired, err := s.State.Load()
	if err != nil {
		return "", nil, fmt.Errorf("reading desired state: %w", err)
	}
	group, installed := portalGroup(desired, surface.capability)
	if !installed {
		return "", nil, errPortalNotInstalled
	}
	// The peer test first, so that an attempt from anywhere but the
	// proxy is refused before any header is read at all.
	if !s.peerIsTrustedIngress(r) {
		return "", nil, fmt.Errorf("%w: the request did not arrive from the identity proxy (peer %s, trusted %v)",
			errPortalDenied, remoteHost(r), s.TrustedIngress)
	}
	member := strings.TrimSpace(r.Header.Get(TrustedIngressHeader))
	if member == "" {
		return "", nil, fmt.Errorf("%w: the proxy forwarded no %s header",
			errPortalDenied, TrustedIngressHeader)
	}
	forwarded := strings.TrimSpace(r.Header.Get(ConsoleGroupsHeader))
	if !hasGroup(forwarded, group) {
		// The member is returned WITH the refusal so the audit row names
		// the person who was turned away rather than "anonymous", and
		// the groups they arrived with are in the message: a household
		// member added to a group after they signed in arrives with the
		// list their session was built from, and this line is the
		// difference between seeing that and guessing at it.
		return member, nil, fmt.Errorf("%w: %s is in [%s], which does not include %s",
			errPortalDenied, member, forwarded, group)
	}
	return member, desired, nil
}

// portalFunc is a portal handler: the authenticated member, the desired
// state their request was authorised against, and the usual (body,
// status, error).
//
// It takes desired state rather than reading it because authorisation
// already had to, and because a handler that re-read it could answer
// from a different install than the one that admitted the caller.
//
// It also takes the ResponseWriter, which the admin chain's handlers do
// not: two of these surfaces serve something that is not JSON — an HTML
// page and a file download — and a handler that returns a nil body with
// a 2xx has written the response itself.
type portalFunc func(w http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error)

// portal builds the middleware chain for one portal endpoint.
//
// It is deliberately NOT s.handle. That chain authenticates a
// principal, checks an RBAC verb and rate-limits per principal; this
// one authenticates a household member, checks a directory group, and
// rate-limits them all through the anonymous bucket, because a
// household member is not a principal and must never become one by
// being handed a role.
//
// Every request is audited, allowed or refused, under the member's own
// directory name. #197 asks for "a structured audit record for the
// write" and this is it: the same audit log that records every
// operator action, with the person who dropped the file in it.
func (s *Server) portal(surface portalSurface, h portalFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, desired, err := s.portalMember(r, surface)
		if err != nil {
			status := http.StatusForbidden
			msg := fmt.Sprintf("%s is not available to you", surface.title)
			switch {
			case errors.Is(err, errPortalNotInstalled):
				status, msg = http.StatusNotFound, fmt.Sprintf("%s is not installed", surface.title)
			case errors.Is(err, errPortalDenied):
			default:
				status, msg = http.StatusInternalServerError, err.Error()
			}
			if !s.limiter.allow(anonBucket) {
				s.writeError(w, http.StatusTooManyRequests, "rate limited")
				return
			}
			s.audit(r, member, status)
			// The reason goes to the operator's log, never to the
			// caller: msg is the same sentence for all three.
			s.Log.Info("portal refused", "surface", surface.capability,
				"path", r.URL.Path, "status", status, "reason", err.Error())
			s.writeError(w, status, msg)
			return
		}
		if !s.limiter.allow("portal:" + member) {
			s.audit(r, member, http.StatusTooManyRequests)
			s.writeError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
		body, status, err := h(w, r, member, desired)
		s.audit(r, member, status)
		if err != nil {
			s.writeError(w, status, err.Error())
			return
		}
		// A nil body with a 2xx means the handler has already written
		// the response itself — a file download, or an HTML page. The
		// admin chain cannot have that case, because every one of its
		// handlers returns JSON.
		if body == nil {
			return
		}
		s.writeJSON(w, status, body)
	})
}

// audit records one portal request against the member who made it.
//
// A failure to audit is logged and does not fail the request: the
// household member is holding a phone, and a dashboard that stops
// accepting photos because a log write failed is a worse outcome than
// an upload with no row. That is the same trade the admin chain makes.
func (s *Server) audit(r *http.Request, member string, status int) {
	if member == "" {
		member = "anonymous"
	}
	if err := s.DB.AppendAudit(runtimedb.AuditRecord{
		Principal: "portal",
		ActingFor: member,
		Method:    r.Method,
		Path:      r.URL.Path,
		Status:    status,
		Remote:    remoteHost(r),
	}); err != nil {
		s.Log.Error("audit append failed", "err", err)
	}
}

// portalPrincipal is the name every portal-originated record is filed
// under, with the member in ActingFor.
//
// A name of its own rather than the member's, because the member is not
// a principal: nothing minted them a credential, and a record that
// claimed they were one would make "who did this" and "what were they
// allowed to do" disagree. It is the same split the Butler's own
// records use (butlerPrincipal, acting for the person who asked).
const portalPrincipal = "portal"
