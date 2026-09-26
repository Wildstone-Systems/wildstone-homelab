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
// Everything else is ADR-W032's reasoning: the peer must be the trusted
// ingress, because a header anyone can set is not an authentication;
// and no principal row is created, because a persisted row is a second
// place to revoke and a second place to forget.
//
// ONE PART OF THAT REASONING WAS WRONG, and it is worth stating plainly
// because this door was built on it. It said membership IS the
// permission, "so removing somebody from the group revokes them on
// their next request". Nothing in this system can remove anybody from a
// group: the identity capability's provisioning job is additive by
// construction and says so — it "never deletes a user and never removes
// a membership". So the group could only ever GRANT. Offboarding a
// household member left their dashboard, their files and their
// assistant exactly where they were.
//
// So there is a fourth condition, and it is the same one main added to
// the Console door for the same reason: a person the household has a
// record of is held to that record's lifecycle. What differs is the
// bar. The Console requires an active OWNER; this surface requires only
// that the member is still a member.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
func (s *Server) portalMember(r *http.Request, surface portalSurface) (string, string, []resource.Resource, error) {
	if s.State == nil {
		return "", "", nil, errPortalNotInstalled
	}
	// THE HEAD IS READ BEFORE THE STATE, NOT AFTER, and the order is the
	// whole safety of it. A write computed from this read commits only
	// if the head is still this one (see applyAuthored). Reading the
	// head second would mean a commit landing in between made a STALE
	// read look current — the silent overwrite this is here to stop.
	// Read first, and the same race only ever makes a CURRENT read look
	// stale, which costs somebody a retry and loses nobody's work.
	at, err := s.State.Head()
	if err != nil {
		return "", "", nil, fmt.Errorf("reading statefile head: %w", err)
	}
	desired, err := s.State.Load()
	if err != nil {
		return "", "", nil, fmt.Errorf("reading desired state: %w", err)
	}
	group, installed := portalGroup(desired, surface.capability)
	if !installed {
		return "", "", nil, errPortalNotInstalled
	}
	// The peer test first, so that an attempt from anywhere but the
	// proxy is refused before any header is read at all.
	if !s.peerIsTrustedIngress(r) {
		return "", "", nil, fmt.Errorf("%w: the request did not arrive from the identity proxy (peer %s, trusted %v)",
			errPortalDenied, remoteHost(r), s.TrustedIngressAddrs())
	}
	member := strings.TrimSpace(r.Header.Get(TrustedIngressHeader))
	if member == "" {
		return "", "", nil, fmt.Errorf("%w: the proxy forwarded no %s header",
			errPortalDenied, TrustedIngressHeader)
	}
	// CANONICALISED HERE, ONCE, and this is the layer that has to do it.
	// The door admits the household's one Sandy however she signed in
	// (ADR-W032 A4), and then hands whatever spelling the proxy asserted
	// to every member-scoped query underneath: her Butler roster, her
	// memory, her notices, her conversations, her events. Those compare
	// a stored name, so a capitalisation the door forgave left her
	// authenticated and her own household empty — a state worse than a
	// refusal, because it looks like an answer.
	//
	// Adding a case-insensitive predicate to each of those queries is
	// the same fix written once per query and forgotten by the next one.
	// The name is resolved to the household's own spelling at the door
	// instead, so everything below sees the name the records are under.
	canonical, ok := s.forwardedUserIsStillAMember(member)
	if !ok {
		// Named in the log like every other refusal, because "they were
		// offboarded last week" is the one explanation an operator will
		// otherwise spend an afternoon not finding.
		return member, "", nil, fmt.Errorf("%w: %s is not an active member of this household",
			errPortalDenied, member)
	}
	member = canonical
	forwarded := strings.TrimSpace(r.Header.Get(ConsoleGroupsHeader))
	if !hasGroup(forwarded, group) {
		// The member is returned WITH the refusal so the audit row names
		// the person who was turned away rather than "anonymous", and
		// the groups they arrived with are in the message: a household
		// member added to a group after they signed in arrives with the
		// list their session was built from, and this line is the
		// difference between seeing that and guessing at it.
		return member, "", nil, fmt.Errorf("%w: %s is in [%s], which does not include %s",
			errPortalDenied, member, forwarded, group)
	}
	return member, at, desired, nil
}

// basedOnKey carries the Statefile head a portal request's desired
// state was read against.
type basedOnKey struct{}

// basedOnHead is the Statefile head this request's view of the house
// was read at. A change computed from that view commits only while the
// head is still this one, so two members editing the same commitment
// cannot both be told they succeeded.
//
// It is "" on a household whose Statefile has no commits yet, which is
// a real head and not a missing one: Head() reports an unborn branch
// that way, so "" must compare equal to "" rather than mean "skip the
// check".
func basedOnHead(r *http.Request) string {
	at, _ := r.Context().Value(basedOnKey{}).(string)
	return at
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
		member, at, desired, err := s.portalMember(r, surface)
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
		// A write has to have come from the portal's own pages. This is
		// AFTER the member is known so the audit row names the person
		// whose session was being ridden, which is the only way an
		// operator finds out it happened.
		if err := sameSiteWrite(r); err != nil {
			s.audit(r, member, http.StatusForbidden)
			s.Log.Info("portal refused", "surface", surface.capability,
				"path", r.URL.Path, "status", http.StatusForbidden, "reason", err.Error())
			s.writeError(w, http.StatusForbidden,
				"this request did not come from the household dashboard")
			return
		}
		// The head travels on the request rather than in the handler
		// signature: only the two authoring handlers need it, and every
		// other portal handler would otherwise carry a parameter it
		// ignores.
		r = r.WithContext(context.WithValue(r.Context(), basedOnKey{}, at))
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
	if !auditWorthy(r, status) {
		return
	}
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

// portalSurfaces is every household surface this file knows about, which
// is what lets a shared resource ask "is this person entitled to ANY of
// them" without naming one.
var portalSurfaces = []portalSurface{surfaceHome, surfaceFiles, surfaceAssistant, surfaceLists}

// portalAsset builds the chain for the portal's static design files —
// the stylesheet, the two fonts, and the app icon set.
//
// WHY THESE ARE GATED AT ALL. They carry no household data: an icon set
// and a font say nothing about who lives here. But Keystone's public
// listener is reachable on the LAN without going through the identity
// proxy, and "nothing on this listener answers to an unauthenticated
// caller except healthz" is a property worth keeping whole rather than
// worth one exception. The cost is one forward-auth check on files the
// browser then caches for a year.
//
// WHY ANY SURFACE RATHER THAN A NAMED ONE. The same stylesheet dresses
// the front page, the Files manager and the Assistant, and a household
// may have installed any subset of them. Gating on one would mean a
// household with filedrop but no dashboard got an unstyled Files page,
// and gating each surface's own copy would mean three routes serving
// identical bytes under three cache keys.
func (s *Server) portalAsset(h portalFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var member string
		var desired []resource.Resource
		var lastErr error
		for _, surface := range portalSurfaces {
			m, _, d, err := s.portalMember(r, surface)
			if err == nil {
				member, desired = m, d
				lastErr = nil
				break
			}
			// Remember a refusal that names a person over one that does
			// not: "not installed" from a surface this household never
			// had says less than "you are not in that group".
			if lastErr == nil || errors.Is(lastErr, errPortalNotInstalled) {
				lastErr = err
			}
			if m != "" {
				member = m
			}
		}
		if lastErr != nil {
			if !s.limiter.allow(anonBucket) {
				s.writeError(w, http.StatusTooManyRequests, "rate limited")
				return
			}
			status := http.StatusForbidden
			if errors.Is(lastErr, errPortalNotInstalled) {
				status = http.StatusNotFound
			}
			s.audit(r, member, status)
			s.Log.Info("portal asset refused", "path", r.URL.Path, "status", status, "reason", lastErr.Error())
			s.writeError(w, status, "not available to you")
			return
		}
		if !s.limiter.allow("portal:" + member) {
			s.audit(r, member, http.StatusTooManyRequests)
			s.writeError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
		// Deliberately NOT audited on success. Every page load fetches a
		// stylesheet, two fonts and a dozen icons; auditing them would
		// bury the rows that say what a person actually did under
		// fifteen that say their browser fetched a picture.
		body, status, err := h(w, r, member, desired)
		if err != nil {
			s.writeError(w, status, err.Error())
			return
		}
		if body != nil {
			s.writeJSON(w, status, body)
		}
	})
}

// polledPaths are the read-only projections a page refreshes on a
// timer. A closed set, and deliberately a short one.
var polledPaths = map[string]bool{
	"/api/v1/home/summary": true,
}

// auditWorthy reports whether one portal request earns an audit row.
//
// Everything does, except a SUCCESSFUL GET of a projection a page polls.
//
// The household's front page refreshes every twenty seconds so the
// machine card and the activity list are current rather than frozen at
// whenever somebody last pressed reload. A wall tablet left on all day
// is four thousand of those, nothing in this database prunes the audit
// table, and four thousand rows a day saying "a dashboard refreshed
// itself" would bury the rows that say a person answered a question or
// deleted a file at about a hundred to one.
//
// It is the same trade portalAsset already makes for the stylesheet and
// the icons. What is NOT traded away is everything that matters: every
// refusal is still recorded — somebody outside the group hammering this
// endpoint is exactly the case the log is for — and every write still
// is, because the set above is read-only projections and nothing else.
func auditWorthy(r *http.Request, status int) bool {
	if r.Method != http.MethodGet {
		return true
	}
	if status < 200 || status >= 300 {
		return true
	}
	return !polledPaths[r.URL.Path]
}

// forwardedUserIsStillAMember holds a forwarded name to the household's
// own record of that person, when there is one.
//
// THE DIRECTORY GROUP CAN ONLY GRANT. Wildstone never removes a
// membership — the provisioning job is additive by construction — so a
// group is a record of who was once let in, not of who may be here now.
// Whether somebody is still a member is the household's own answer, and
// this is where it is asked.
//
// NO RECORD MEANS THE GROUP DECIDES, exactly as before. Requiring a
// Person record would lock out every household that has not created
// them yet, including this one's, and a door that refuses everybody is
// not a safer door. It closes the hole it can close without a lockout
// it cannot undo.
//
// MATCHED CASE-INSENSITIVELY, which is load-bearing rather than tidy.
// Handles are compared exactly everywhere else, so "Sandy" and "sandy"
// are two records (ADR-W032 A4, still open). An exact lookup would miss
// the record whenever the proxy asserted a different capitalisation,
// fall into the permissive branch, and admit somebody the household had
// just offboarded. The permissive branch is the one an attacker aims
// for, so nothing reaching it may depend on spelling.
//
// Looser matching is only ever used to REFUSE. It cannot grant: no
// match at all still leaves the group deciding.
func (s *Server) forwardedUserIsStillAMember(user string) (string, bool) {
	if s.DB == nil {
		return "", false
	}
	members, err := s.DB.ListButlerMembers()
	if err != nil {
		// A lookup that failed is not a lookup that passed.
		return "", false
	}
	canonical := ""
	for _, m := range members {
		stored := strings.TrimSpace(m.Member)
		if !strings.EqualFold(stored, strings.TrimSpace(user)) {
			continue
		}
		// EVERY match must qualify. With two records differing only by
		// case there is no telling which one the proxy meant, and an
		// authorization check that guesses is not a check.
		if !m.Active() {
			return "", false
		}
		if canonical == "" {
			canonical = stored
		} else if canonical != stored {
			// The household has this person under two spellings, which
			// its own rule says cannot happen. Named in the log because
			// half their records will be under each, and nothing else
			// in the system will ever say so.
			s.Log.Warn("household member stored under two spellings",
				"asserted", user, "using", canonical, "also", stored)
		}
	}
	// Unlike the Console, no ROLE is required. This surface is for
	// household members, and the group already says which of them may
	// reach it; holding them to `owner` here would refuse the family the
	// dashboard was built for.
	//
	// A member with no record at all keeps the spelling the proxy
	// asserted: there is nothing to canonicalise against, and inventing
	// one would be this layer deciding who somebody is.
	if canonical == "" {
		canonical = strings.TrimSpace(user)
	}
	return canonical, true
}

// errPortalCrossSite is a write that did not come from the portal's own
// pages.
var errPortalCrossSite = errors.New("cross-site write")

// sameSiteWrite refuses a state-changing request that a page other than
// the portal's own caused a browser to send.
//
// THE DOOR ABOVE IS NOT ENOUGH ON ITS OWN, and the reason is the shape
// of the session rather than a defect in the check. Authelia's cookie
// is scoped to the whole SessionDomain (packages/identity), so every
// sibling service under it shares the household's signed-in state. A
// page on any one of them — a compromised container, an application
// with a script injection, a service that renders somebody else's
// markup — can make the browser POST here, and the browser will attach
// that cookie because the request is same-SITE even though it is
// cross-ORIGIN. The proxy then authenticates a real member and forwards
// a real name. Every check in portalMember passes. The write happens.
//
// CORS does not stop it: it withholds the RESPONSE, and these are
// operations whose damage is done by arriving — a delete, a forget, a
// cleared banner. The page's own `form-action 'none'` does not stop it
// either: that constrains markup WE serve, and the markup is theirs.
//
// So the request itself has to carry proof of where it came from, and a
// browser supplies two such proofs that a cross-origin page cannot
// forge — both are forbidden header names, settable by the browser and
// never by script:
//
//   - Sec-Fetch-Site, which says same-origin in as many words. Sent by
//     every browser this household can sign in from.
//   - Origin, which every browser sends on a write. Its host must be
//     the host this request was addressed to.
//
// A request carrying NEITHER is refused rather than admitted. A modern
// browser always sends at least one, so the only callers that header
// says nothing about are scripted ones — and a script that has got this
// far is either the operator (who has the admin API and does not need
// this door) or somebody replaying a stolen session, which is the case
// this exists for.
//
// Reads are exempt because they change nothing, and because the two
// framed widgets are GETs from a dashboard that may be served from
// another origin entirely. Their own writes are unaffected: a fetch
// from inside an iframe carries the IFRAME's origin, which is this one.
func sameSiteWrite(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); site != "" {
		if site == "same-origin" {
			return nil
		}
		return fmt.Errorf("%w: the browser reported Sec-Fetch-Site: %s", errPortalCrossSite, site)
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return fmt.Errorf("%w: the write carried neither Sec-Fetch-Site nor Origin", errPortalCrossSite)
	}
	// Compared by HOST rather than by whole URL: the scheme the browser
	// used is the scheme the ingress terminated, and this handler is
	// behind it on plain HTTP. The host is the part a cross-origin page
	// cannot match without already being us.
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: Origin %q is not a URL", errPortalCrossSite, origin)
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return fmt.Errorf("%w: Origin %s is not %s", errPortalCrossSite, u.Host, r.Host)
	}
	return nil
}
