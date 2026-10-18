package api

// Browser enrollment for the accepted ADR-W049 external-Agent profile.
//
// This is deliberately a relying-party ceremony, not an OAuth resource
// server. It authenticates the configured Person twice (trusted ingress and a
// fresh enrollment ID token), shows registration-specific consequences, and
// hands the final mutation to runtimedb's one activation transaction.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/consoleauth"
	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

const (
	externalEnrollmentClientID = "wildstone-external-enrollment"
	externalEnrollmentCallback = "/api/v1/external-enrollment/callback"
	externalEnrollmentConfirm  = "/api/v1/external-enrollment/confirm"
	externalEnrollmentCookie   = "wst_external_enrollment"
)

type enrollmentStart struct {
	RegistrationID string
	PersonID       string
	IngressUser    string
	Issuer         string
	Proof          string
	Browser        string
	Verifier       string
	Nonce          string
	ExpiresAt      time.Time
}

type enrollmentConfirmation struct {
	RegistrationID string
	PersonID       string
	IngressUser    string
	Issuer         string
	Subject        string
	Proof          string
	Browser        string
	Digest         string
	Summary        runtimedb.ExternalRegistrationConfirmation
	ExpiresAt      time.Time
}

type externalEnrollmentAuth struct {
	mu            sync.Mutex
	provider      *consoleauth.Provider
	starts        map[string]enrollmentStart
	confirmations map[string]enrollmentConfirmation
}

func (a *externalEnrollmentAuth) cleanup(now time.Time) {
	for key, value := range a.starts {
		if !now.Before(value.ExpiresAt) {
			delete(a.starts, key)
		}
	}
	for key, value := range a.confirmations {
		if !now.Before(value.ExpiresAt) {
			delete(a.confirmations, key)
		}
	}
}

func (a *externalEnrollmentAuth) begin(value enrollmentStart, now time.Time) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.starts == nil {
		a.starts = map[string]enrollmentStart{}
	}
	a.cleanup(now)
	if len(a.starts) >= 4096 {
		a.starts = map[string]enrollmentStart{}
	}
	state := consoleauth.Token()
	a.starts[state] = value
	return state
}

func (a *externalEnrollmentAuth) takeStart(state string, now time.Time) (enrollmentStart, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanup(now)
	value, ok := a.starts[state]
	delete(a.starts, state)
	return value, ok && now.Before(value.ExpiresAt)
}

func (a *externalEnrollmentAuth) beginConfirmation(value enrollmentConfirmation, now time.Time) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.confirmations == nil {
		a.confirmations = map[string]enrollmentConfirmation{}
	}
	a.cleanup(now)
	if len(a.confirmations) >= 4096 {
		a.confirmations = map[string]enrollmentConfirmation{}
	}
	token := consoleauth.Token()
	a.confirmations[token] = value
	return token
}

func (a *externalEnrollmentAuth) peekConfirmation(token string, now time.Time) (enrollmentConfirmation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanup(now)
	value, ok := a.confirmations[token]
	return value, ok && now.Before(value.ExpiresAt)
}

func (a *externalEnrollmentAuth) takeConfirmation(token string, now time.Time) (enrollmentConfirmation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanup(now)
	value, ok := a.confirmations[token]
	delete(a.confirmations, token)
	return value, ok && now.Before(value.ExpiresAt)
}

func externalEnrollmentSignInConfig(desired []resource.Resource) (clientID, issuer, redirect string, ok bool) {
	posture := ingressorigin.Of(desired)
	for _, res := range desired {
		switch {
		case res.Kind == resource.KindOidcClient && res.OidcClient != nil && res.OidcClient.Public &&
			res.Metadata.Owner == consoleCapability && res.OidcClient.ClientId == externalEnrollmentClientID:
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
	return clientID, issuer, strings.TrimRight(console, "/") + externalEnrollmentCallback, true
}

func (s *Server) externalEnrollmentProvider() (*consoleauth.Provider, error) {
	desired, err := s.State.Load()
	if err != nil {
		return nil, err
	}
	clientID, issuer, redirect, ok := externalEnrollmentSignInConfig(desired)
	if !ok {
		return nil, errors.New("external enrollment is not configured on this deployment")
	}
	s.externalEnrollment.mu.Lock()
	defer s.externalEnrollment.mu.Unlock()
	p := s.externalEnrollment.provider
	if p == nil || p.Issuer != issuer || p.ClientID != clientID || p.RedirectURL != redirect {
		p = &consoleauth.Provider{Issuer: issuer, ClientID: clientID, RedirectURL: redirect,
			HTTP: s.EnrollmentSignInHTTP, Now: s.Now}
		s.externalEnrollment.provider = p
	}
	return p, nil
}

func (s *Server) externalEnrollmentCaller(w http.ResponseWriter, r *http.Request) (string, runtimedb.ButlerMember, bool) {
	if !s.peerIsTrustedIngress(r) {
		if !s.limiter.allow(anonBucket) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return "", runtimedb.ButlerMember{}, false
		}
		s.externalEnrollmentPage(w, http.StatusForbidden, "Open enrollment through the household address.")
		return "", runtimedb.ButlerMember{}, false
	}
	user := strings.TrimSpace(r.Header.Get(TrustedIngressHeader))
	if user == "" {
		s.externalEnrollmentPage(w, http.StatusUnauthorized, "Sign in to the household first.")
		return "", runtimedb.ButlerMember{}, false
	}
	if !s.limiter.allow("external-enrollment:" + strings.ToLower(user)) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return "", runtimedb.ButlerMember{}, false
	}
	person, err := s.DB.ResolveSurfaceIdentity("trusted-ingress", user)
	if err != nil || !person.Admitted || !person.DisabledAt.IsZero() || !person.OffboardedAt.IsZero() {
		s.externalEnrollmentPage(w, http.StatusUnauthorized, "That household identity is not admitted for enrollment.")
		return "", runtimedb.ButlerMember{}, false
	}
	return user, person, true
}

func (s *Server) externalEnrollmentStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.ContentLength > 0 {
		s.externalEnrollmentPage(w, http.StatusBadRequest, "Enrollment start does not accept a request body.")
		return
	}
	user, person, ok := s.externalEnrollmentCaller(w, r)
	if !ok {
		return
	}
	registrationID := strings.TrimSpace(r.PathValue("registration"))
	registration, err := s.DB.ExternalClientRegistration(registrationID)
	if err != nil || registration.State != runtimedb.ExternalRegistrationPendingConsent || registration.PersonID != person.PersonID {
		s.externalEnrollmentPage(w, http.StatusNotFound, "That enrollment is unavailable.")
		return
	}
	p, err := s.externalEnrollmentProvider()
	if err != nil || p.Issuer != registration.Issuer {
		s.externalEnrollmentPage(w, http.StatusServiceUnavailable, "Enrollment cannot be verified on this deployment.")
		return
	}
	issued, err := s.DB.IssueExternalEnrollmentChallenge(runtimedb.IssueExternalEnrollmentChallenge{
		RegistrationID: registration.ID, PersonID: person.PersonID, IssuedBy: person.PersonID,
	})
	if err != nil {
		s.externalEnrollmentPage(w, http.StatusConflict, "That enrollment is no longer available.")
		return
	}
	verifier, browser, nonce := consoleauth.Token(), consoleauth.Token(), consoleauth.Token()
	state := s.externalEnrollment.begin(enrollmentStart{
		RegistrationID: registration.ID, PersonID: person.PersonID, IngressUser: user,
		Issuer: registration.Issuer, Proof: issued.Secret, Browser: browser,
		Verifier: verifier, Nonce: nonce, ExpiresAt: issued.Challenge.ExpiresAt,
	}, s.now())
	to, err := p.AuthURL(r.Context(), state, nonce, consoleauth.Challenge(verifier), s.consoleEntryWindow(), false)
	if err != nil {
		_ = s.DB.InvalidateExternalEnrollmentChallenges(registration.ID, person.PersonID, "enrollment provider unavailable")
		s.externalEnrollmentPage(w, http.StatusBadGateway, "The enrollment identity service could not be reached.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{Name: externalEnrollmentCookie, Value: browser,
		Path: "/api/v1/external-enrollment/", MaxAge: int(issued.Challenge.ExpiresAt.Sub(s.now()).Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) externalEnrollmentCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.ContentLength > 0 {
		s.externalEnrollmentPage(w, http.StatusBadRequest, "Enrollment callback does not accept a request body.")
		return
	}
	user, person, ok := s.externalEnrollmentCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		s.externalEnrollmentPage(w, http.StatusUnauthorized, "The identity provider did not complete enrollment.")
		return
	}
	pending, ok := s.externalEnrollment.takeStart(q.Get("state"), s.now())
	if !ok {
		s.externalEnrollmentPage(w, http.StatusBadRequest, "That enrollment has expired or was already used.")
		return
	}
	if c, err := r.Cookie(externalEnrollmentCookie); err != nil || c.Value != pending.Browser ||
		!strings.EqualFold(user, pending.IngressUser) || person.PersonID != pending.PersonID {
		s.externalEnrollmentPage(w, http.StatusBadRequest, "The browser or household account changed during enrollment.")
		return
	}
	p, err := s.externalEnrollmentProvider()
	if err != nil || p.Issuer != pending.Issuer {
		s.externalEnrollmentPage(w, http.StatusServiceUnavailable, "Enrollment cannot be verified on this deployment.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	raw, err := p.Exchange(ctx, q.Get("code"), pending.Verifier)
	if err != nil {
		s.externalEnrollmentPage(w, http.StatusBadGateway, "The identity provider did not confirm enrollment.")
		return
	}
	claims, err := p.Verify(ctx, raw, pending.Nonce)
	if err != nil || !consoleauth.Fresh(claims.AuthTime, s.now(), s.consoleEntryWindow()) {
		s.externalEnrollmentPage(w, http.StatusUnauthorized, "The enrollment identity could not be verified.")
		return
	}
	issuerPerson, err := s.DB.ResolveSurfaceIdentity("trusted-ingress", claims.User)
	if err != nil || issuerPerson.PersonID != pending.PersonID || person.PersonID != pending.PersonID {
		s.externalEnrollmentPage(w, http.StatusUnauthorized, "The enrollment identity does not match the configured Person.")
		return
	}
	summary, err := s.DB.ExternalRegistrationConfirmation(pending.RegistrationID, pending.PersonID)
	if err != nil {
		s.externalEnrollmentPage(w, http.StatusConflict, "The registration changed and must be reviewed again.")
		return
	}
	digest := externalEnrollmentDigest(summary)
	token := s.externalEnrollment.beginConfirmation(enrollmentConfirmation{
		RegistrationID: pending.RegistrationID, PersonID: pending.PersonID, IngressUser: user,
		Issuer: pending.Issuer, Subject: claims.Subject, Proof: pending.Proof, Browser: pending.Browser,
		Digest: digest, Summary: summary, ExpiresAt: pending.ExpiresAt,
	}, s.now())
	s.externalEnrollmentConfirmationPage(w, token, summary)
}

func (s *Server) externalEnrollmentConfirm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, person, ok := s.externalEnrollmentCaller(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil || len(r.PostForm) != 1 || len(r.PostForm["confirmation"]) != 1 {
		s.externalEnrollmentPage(w, http.StatusBadRequest, "The confirmation was invalid.")
		return
	}
	token := r.PostForm.Get("confirmation")
	pending, ok := s.externalEnrollment.peekConfirmation(token, s.now())
	if !ok {
		s.externalEnrollmentPage(w, http.StatusBadRequest, "That confirmation has expired or was already used.")
		return
	}
	// The synchronizer token and same-site browser cookie are the CSRF
	// evidence. Do not consume the state until both are present and match.
	cookie, cookieErr := r.Cookie(externalEnrollmentCookie)
	if cookieErr != nil || cookie.Value != pending.Browser {
		s.externalEnrollmentPage(w, http.StatusBadRequest, "That confirmation belongs to another browser.")
		return
	}
	pending, ok = s.externalEnrollment.takeConfirmation(token, s.now())
	if !ok {
		s.externalEnrollmentPage(w, http.StatusBadRequest, "That confirmation has expired or was already used.")
		return
	}
	if !strings.EqualFold(user, pending.IngressUser) || person.PersonID != pending.PersonID {
		s.externalEnrollmentPage(w, http.StatusUnauthorized, "The household account changed before confirmation.")
		return
	}
	current, err := s.DB.ExternalRegistrationConfirmation(pending.RegistrationID, pending.PersonID)
	if err != nil || externalEnrollmentDigest(current) != pending.Digest {
		s.externalEnrollmentPage(w, http.StatusConflict, "The registration changed and must be reviewed again.")
		return
	}
	_, err = s.DB.ActivateExternalClientRegistration(runtimedb.ActivateExternalRegistration{
		RegistrationID: pending.RegistrationID, PersonID: pending.PersonID,
		Issuer: pending.Issuer, Subject: pending.Subject, Proof: pending.Proof, ActivatedBy: person.PersonID,
	})
	if err != nil {
		s.externalEnrollmentPage(w, http.StatusConflict, "Enrollment could not be activated; no registration or credential was changed.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: externalEnrollmentCookie,
		Path: "/api/v1/external-enrollment/", MaxAge: -1, Secure: true, HttpOnly: true})
	s.externalEnrollmentPage(w, http.StatusOK, "Enrollment is active. Static runtime credentials for this Agent were retired.")
}

func externalEnrollmentDigest(summary runtimedb.ExternalRegistrationConfirmation) string {
	b, _ := json.Marshal(summary)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Server) externalEnrollmentConfirmationPage(w http.ResponseWriter, token string, summary runtimedb.ExternalRegistrationConfirmation) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Confirm external Agent enrollment</title>`+
		`<body><h1>Confirm external Agent enrollment</h1>`+
		`<dl><dt>Agent</dt><dd>%s</dd><dt>Connection</dt><dd>%s</dd>`+
		`<dt>External client</dt><dd>%s</dd><dt>Disclosure</dt><dd>%s</dd>`+
		`<dt>Processing</dt><dd>%s / %s</dd></dl>`+
		`<p>Confirming retires every static runtime token for this Agent.</p>`+
		`<form method="post" action="%s"><input type="hidden" name="confirmation" value="%s">`+
		`<button type="submit">Confirm enrollment</button></form></body>`,
		html.EscapeString(summary.Agent), html.EscapeString(summary.Connection),
		html.EscapeString(summary.ClientID), html.EscapeString(summary.Disclosure),
		html.EscapeString(summary.Processing), html.EscapeString(summary.Execution),
		externalEnrollmentConfirm, html.EscapeString(token))
}

func (s *Server) externalEnrollmentPage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>External Agent enrollment</title>`+
		`<body><h1>External Agent enrollment</h1><p>%s</p></body>`, html.EscapeString(message))
}
