package api

// Console sign-in freshness (ADR-W033 A4, amended A4.1; #205).
//
// consoleForwardAuth knows WHO from the headers the trusted ingress
// forwards. This file is how it knows WHEN. Authelia's forward-auth has
// no header for the time somebody authenticated, so the Console is also
// an OpenID Connect client of Authelia: signing in to the Console is an
// authorization-code round trip whose ID token carries auth_time, signed
// with Authelia's key and verified here (keystone/consoleauth).
//
// The result is a Console session: a random id in a cookie, held only in
// memory, naming the account and when it authenticated. A forwarded
// request is a Console identity only when its session names the same
// account the ingress forwarded AND that authentication is inside the
// entry window. Everything else is refused as unauthenticated, with a
// pointer to the sign-in, so the Console can send the person round
// again rather than showing a blank refusal.
//
// A header is never enough. The session id is a capability issued by
// this process after verifying a signature; the forwarded headers only
// say whose it must be. A forged Remote-User from an untrusted peer is
// refused before the session is looked at, and a stolen session cookie
// presented under somebody else's forwarded name is refused because the
// names differ.
//
// Bearer tokens are untouched (A4.1): the CLI, the installer and host
// recovery authenticate with one, have no browser session to go stale,
// and are the host's own path (A7).

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/consoleauth"
	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

const (
	// consoleSessionCookie holds the session id. HttpOnly: no page script
	// ever needs it, and a Console page that could read it could hand it
	// to anything the page loads.
	consoleSessionCookie = "wst_console"
	// consoleSigninCookie binds a sign-in in progress to the browser that
	// started it, so a callback URL lifted from somebody else's sign-in
	// cannot be completed in another browser.
	consoleSigninCookie = "wst_console_signin"
	// consoleCapability owns the Console's client at Authelia
	// (packages/wildstone-console). Keystone reads the client id from
	// that declaration rather than keeping its own copy, so the two
	// halves of the sign-in cannot name different clients.
	consoleCapability = "capability/console"
	consoleClientID   = "wildstone-console"

	consoleSigninPath   = "/api/v1/console/signin"
	consoleCallbackPath = "/api/v1/console/callback"
)

// consoleAuth is the Server's Console sign-in state.
type consoleAuth struct {
	sessions consoleauth.Store

	mu       sync.Mutex
	provider *consoleauth.Provider
}

// consoleEntryWindow and consoleStepUpWindow are the configured windows,
// or the defaults (A4.1).
func (s *Server) consoleEntryWindow() time.Duration {
	if s.ConsoleEntryWindow > 0 {
		return s.ConsoleEntryWindow
	}
	return consoleauth.DefaultEntryWindow
}

func (s *Server) consoleStepUpWindow() time.Duration {
	if s.ConsoleStepUpWindow > 0 {
		return s.ConsoleStepUpWindow
	}
	return consoleauth.DefaultStepUpWindow
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// consoleSession is the session a request presents, if any, whoever it
// names. Callers compare the name.
func (s *Server) consoleSession(r *http.Request) (consoleauth.Session, bool) {
	c, err := r.Cookie(consoleSessionCookie)
	if err != nil {
		return consoleauth.Session{}, false
	}
	return s.console.sessions.Lookup(c.Value)
}

// consoleSessionFor is the session a forwarded request presents for the
// account the ingress forwarded, when that authentication is still
// inside the entry window.
func (s *Server) consoleSessionFor(r *http.Request, user string) (consoleauth.Session, bool) {
	sess, ok := s.consoleSession(r)
	if !ok || !strings.EqualFold(sess.User, user) {
		return consoleauth.Session{}, false
	}
	if !consoleauth.Fresh(sess.AuthTime, s.now(), s.consoleEntryWindow()) {
		return consoleauth.Session{}, false
	}
	return sess, true
}

// StartConsoleSession records a Console sign-in and returns the session
// id. It is what the callback does after verifying a token, and it is
// exported for tests that need a signed-in browser without a provider.
func (s *Server) StartConsoleSession(user string, authTime time.Time) string {
	return s.console.sessions.Create(consoleauth.Session{User: user, AuthTime: authTime}, s.consoleEntryWindow())
}

// EndConsoleSessions signs a person out of every Console session they
// hold (A6: demotion invalidates active Console sessions). Every request
// rechecks the Person's role anyway; this makes the sessions stop
// existing as well as stop working.
func (s *Server) EndConsoleSessions(user string) {
	s.console.sessions.EndUser(user)
}

// consoleSigninHint is the body of a refusal a signed-in person can fix
// by signing in to the Console: where to go. Only offered on the trusted
// ingress path with a forwarded name — to anyone else there is nothing a
// sign-in would change.
//
// Returned as an untyped nil when there is no hint: writeErrorBody
// treats a typed nil map as a body and would decode "null" into one.
func (s *Server) consoleSigninHint(r *http.Request) any {
	if r.Header.Get("Authorization") != "" || !s.peerIsTrustedIngress(r) {
		return nil
	}
	user := strings.TrimSpace(r.Header.Get(TrustedIngressHeader))
	if user == "" {
		return nil
	}
	// Already signed in, freshly, and still refused: the refusal is about
	// who they are (a Member, a disabled Person), and sending them round
	// Authelia again would only bring them back to it.
	if _, ok := s.consoleSessionFor(r, user); ok {
		return nil
	}
	return map[string]any{"signIn": consoleSigninPath}
}

// consoleSignInConfig is the Console's client at Authelia as this
// deployment declares it: the issuer (where the identity package's own
// login route is served), and the callback (where the Console route is
// served). ok is false when either is missing — no identity provider,
// or a wildstone-console that predates its client.
//
// Derived from desired state rather than taken as flags, so installing
// or upgrading the packages is all it takes, and the addresses cannot
// disagree with what the proxy serves: the issuer is built the way the
// identity package builds Authelia's own authelia_url, from the served
// origin of its auth route.
func consoleSignInConfig(desired []resource.Resource) (clientID, issuer, redirect string, ok bool) {
	posture := ingressorigin.Of(desired)
	for _, res := range desired {
		switch {
		// Public only: the Console holds no secret, so a confidential
		// client under this capability is not one it could use.
		case res.Kind == resource.KindOidcClient && res.OidcClient != nil && res.OidcClient.Public &&
			res.Metadata.Owner == consoleCapability && res.OidcClient.ClientId == consoleClientID:
			clientID = res.OidcClient.ClientId
		case res.Kind == resource.KindRoute && res.Route != nil && res.Route.Auth == "portal" &&
			res.Metadata.Owner == "capability/identity" && strings.HasPrefix(res.Route.Host, "auth."):
			issuer = posture.OriginFor(res.Route.Host)
		}
	}
	console := consoleAddress(desired)
	if clientID == "" || issuer == "" || console == "" {
		return "", "", "", false
	}
	return clientID, issuer, strings.TrimRight(console, "/") + consoleCallbackPath, true
}

// consoleProvider is the relying party for the current configuration,
// rebuilt when the configuration changes and otherwise reused, so its
// discovery and key caches survive between sign-ins.
func (s *Server) consoleProvider() (*consoleauth.Provider, error) {
	desired, err := s.State.Load()
	if err != nil {
		return nil, err
	}
	clientID, issuer, redirect, ok := consoleSignInConfig(desired)
	if !ok {
		return nil, errConsoleSignInUnavailable
	}
	s.console.mu.Lock()
	defer s.console.mu.Unlock()
	p := s.console.provider
	if p == nil || p.Issuer != issuer || p.RedirectURL != redirect || p.ClientID != clientID {
		p = &consoleauth.Provider{Issuer: issuer, ClientID: clientID, RedirectURL: redirect,
			HTTP: s.ConsoleSignInHTTP, Now: s.Now}
		s.console.provider = p
	}
	return p, nil
}

var errConsoleSignInUnavailable = errors.New("the Console's sign-in is not set up on this deployment: " +
	"it needs the identity capability and a wildstone-console new enough to register its sign-in client " +
	"(re-propose wildstone-console; until then, administer the house from the host with `wild`)")

// consoleSignin starts a Console sign-in: GET /api/v1/console/signin
// ?return=<console path>[&stepup=1].
//
// Reached only through the trusted ingress, by somebody the proxy has
// already signed in. It sends them to Authelia with this sign-in's
// state, nonce and PKCE challenge; stepup asks Authelia to authenticate
// them again however recent their session (prompt=login).
func (s *Server) consoleSignin(w http.ResponseWriter, r *http.Request) {
	user, ok := s.consoleSigninCaller(w, r)
	if !ok {
		return
	}
	p, err := s.consoleProvider()
	if err != nil {
		s.consoleSigninPage(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	verifier := consoleauth.Token()
	browser := consoleauth.Token()
	nonce := consoleauth.Token()
	state := s.console.sessions.Begin(consoleauth.Pending{
		Nonce: nonce, Verifier: verifier, User: user, Browser: browser,
		Return: safeConsoleReturn(r.URL.Query().Get("return")),
	})
	stepUp := r.URL.Query().Get("stepup") == "1"
	to, err := p.AuthURL(r.Context(), state, nonce, consoleauth.Challenge(verifier), s.consoleEntryWindow(), stepUp)
	if err != nil {
		s.Log.Warn("console sign-in: provider unreachable", "err", err)
		s.consoleSigninPage(w, http.StatusBadGateway, "The sign-in service could not be reached. Try again in a moment.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: consoleSigninCookie, Value: browser, Path: "/api/v1/console/",
		MaxAge: int((10 * time.Minute).Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// consoleCallback finishes a sign-in: GET /api/v1/console/callback.
func (s *Server) consoleCallback(w http.ResponseWriter, r *http.Request) {
	user, ok := s.consoleSigninCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.consoleSigninPage(w, http.StatusUnauthorized, "The sign-in did not complete ("+e+").")
		return
	}
	pending, ok := s.console.sessions.Take(q.Get("state"))
	if !ok {
		s.consoleSigninPage(w, http.StatusBadRequest, "That sign-in has expired or was already used.")
		return
	}
	// The same browser that began it, and the same person the proxy
	// says is here now as then.
	if c, err := r.Cookie(consoleSigninCookie); err != nil || c.Value != pending.Browser {
		s.consoleSigninPage(w, http.StatusBadRequest, "That sign-in was started in another browser.")
		return
	}
	if !strings.EqualFold(pending.User, user) {
		s.consoleSigninPage(w, http.StatusBadRequest, "The account changed during sign-in.")
		return
	}
	p, err := s.consoleProvider()
	if err != nil {
		s.consoleSigninPage(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	raw, err := p.Exchange(ctx, q.Get("code"), pending.Verifier)
	if err != nil {
		s.Log.Warn("console sign-in: code exchange failed", "user", user, "err", err)
		s.consoleSigninPage(w, http.StatusBadGateway, "The sign-in service did not confirm the sign-in.")
		return
	}
	claims, err := p.Verify(ctx, raw, pending.Nonce)
	if err != nil {
		s.Log.Warn("console sign-in: token refused", "user", user, "err", err)
		s.consoleSigninPage(w, http.StatusUnauthorized, "The sign-in could not be verified.")
		return
	}
	// The token must be for the person the proxy forwarded. Without this
	// a token for one account could open a Console session under the
	// proxy's name for another.
	if !strings.EqualFold(claims.User, user) {
		s.Log.Warn("console sign-in: token names someone else", "forwarded", user, "token", claims.User)
		s.consoleSigninPage(w, http.StatusUnauthorized, "The sign-in was for a different account.")
		return
	}
	if !consoleauth.Fresh(claims.AuthTime, s.now(), s.consoleEntryWindow()) {
		s.consoleSigninPage(w, http.StatusUnauthorized, "The sign-in service reported an old sign-in. Try again.")
		return
	}
	if c, err := r.Cookie(consoleSessionCookie); err == nil {
		s.console.sessions.End(c.Value)
	}
	id := s.StartConsoleSession(user, claims.AuthTime)
	remaining := s.consoleEntryWindow() - s.now().Sub(claims.AuthTime)
	http.SetCookie(w, &http.Cookie{
		Name: consoleSessionCookie, Value: id, Path: "/",
		MaxAge: int(remaining.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{Name: consoleSigninCookie, Path: "/api/v1/console/", MaxAge: -1, Secure: true, HttpOnly: true})
	s.Log.Info("console sign-in", "user", user, "authenticated", claims.AuthTime.UTC().Format(time.RFC3339))
	http.Redirect(w, r, pending.Return, http.StatusSeeOther)
}

// consoleSignout ends this browser's Console session and answers with
// where to finish signing out: the identity provider's logout.
//
// Ending only the Console session would be a button that does nothing.
// The household session Authelia holds would sign the person straight
// back in on their next visit, without a password, because it is well
// inside the Console's window. So signing out of the Console is signing
// out, the same as the dashboard's button, which goes to the same place.
func (s *Server) consoleSignout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(consoleSessionCookie); err == nil {
		s.console.sessions.End(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: consoleSessionCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true})
	out := map[string]any{"ok": true}
	if desired, err := s.State.Load(); err == nil {
		if u := signOutURL(desired); u != "" {
			out["signOut"] = u
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

// consoleSigninCaller is the forwarded account a sign-in endpoint acts
// for. The sign-in endpoints are reachable only through the trusted
// ingress, which has already authenticated the person to Authelia; the
// Console session they produce adds when, never who.
func (s *Server) consoleSigninCaller(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.peerIsTrustedIngress(r) {
		if !s.limiter.allow(anonBucket) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return "", false
		}
		s.consoleSigninPage(w, http.StatusForbidden, "Sign in to the Console through the household address.")
		return "", false
	}
	user := strings.TrimSpace(r.Header.Get(TrustedIngressHeader))
	if user == "" {
		s.consoleSigninPage(w, http.StatusUnauthorized, "Sign in to the household first.")
		return "", false
	}
	// Limited per person, not in the shared anonymous bucket: the proxy
	// has already said who this is, and sharing that bucket would let a
	// flood of junk requests keep every Owner out of the Console.
	if !s.limiter.allow("console-signin:" + strings.ToLower(user)) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return "", false
	}
	return user, true
}

// safeConsoleReturn keeps a return path on this origin. Anything that is
// not a plain absolute path — a scheme, a host, "//elsewhere", a
// backslash some browsers read as a slash — goes to the Console's root.
func safeConsoleReturn(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}

// consoleSigninPage is a sign-in failure, as a page: the browser is
// mid-redirect and a JSON body would be a blank screen. It offers the
// one thing to do next rather than looping back automatically.
func (s *Server) consoleSigninPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Console sign-in</title>`+
		`<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">`+
		`<h1 style="font-size:1.25rem">Console sign-in</h1><p>%s</p>`+
		`<p><a href="%s">Try again</a></p></body>`, html.EscapeString(msg), consoleSigninPath)
}
