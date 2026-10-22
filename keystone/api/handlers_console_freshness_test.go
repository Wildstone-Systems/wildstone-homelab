package api

// Console sign-in freshness (ADR-W033 A4.1, #205).
//
// consoleForwardAuth's other tests present a fresh session through
// h.forwarded. These build their requests by hand, because what they
// test is the session itself: absent, stale, forged, somebody else's,
// or honestly earned through the sign-in round trip.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/consoleauth/fakeidp"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// consoleRequest is a request as the proxy would forward it for user,
// in the Console group, carrying cookie when one is given.
func (h *harness) consoleRequest(method, path, user string, cookies ...*http.Cookie) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set(TrustedIngressHeader, user)
	req.Header.Set(ConsoleGroupsHeader, ConsoleGroup)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// consoleClient is the client id the packaged Console declares.
const consoleClient = "wildstone-console"

func sessionCookie(id string) *http.Cookie {
	return &http.Cookie{Name: consoleSessionCookie, Value: id}
}

// freshnessHarness is a household with an active Owner (mika) and an
// active Member (sandy), behind a trusted proxy.
func freshnessHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.s.SetTrustedIngress([]string{"127.0.0.1", "::1"})
	for member, role := range map[string]string{"mika": runtimedb.RoleOwner, "sandy": runtimedb.RoleMember} {
		if err := h.s.DB.RecordButlerMember(member, role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(member, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// TestMissingFreshnessAssertionFailsClosed: the proxy's word on who is
// asking is not enough to enter the Console. With no Console session the
// request is refused — and told where to sign in, not handed a bare 401.
func TestMissingFreshnessAssertionFailsClosed(t *testing.T) {
	h := freshnessHarness(t)
	resp := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "mika")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an Owner with no Console session got %d", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["signIn"] != consoleSigninPath {
		t.Errorf("refusal = %v, want a pointer to %s", body, consoleSigninPath)
	}
	// A session id nobody issued is the same as none.
	if res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "mika", sessionCookie("made-up")); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an invented session id got %d", res.StatusCode)
	}
}

// TestFreshnessAssertionFromUntrustedPeerIsIgnored (mandatory negative
// case): nothing a caller sends can supply freshness. Not a header
// claiming a sign-in time — Keystone reads none — and not even a real
// session, when the request does not come through the proxy.
func TestFreshnessAssertionFromUntrustedPeerIsIgnored(t *testing.T) {
	h := freshnessHarness(t)
	id := h.s.StartConsoleSession("mika", time.Now())

	// Forged freshness headers, from the trusted peer, with no session.
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/whoami", nil)
	req.Header.Set(TrustedIngressHeader, "mika")
	req.Header.Set(ConsoleGroupsHeader, ConsoleGroup)
	for _, hdr := range []string{"Remote-Auth-Time", "X-Auth-Time", "Auth-Time", "X-Authelia-Auth-Time"} {
		req.Header.Set(hdr, time.Now().Format(time.RFC3339))
	}
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged freshness headers were accepted: %v %v", res.StatusCode, err)
	}

	// A genuine session, but the request did not come through the proxy.
	h.s.SetTrustedIngress([]string{"192.0.2.1"})
	if res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "mika", sessionCookie(id)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session presented from an untrusted peer got %d", res.StatusCode)
	}
	// And the untrusted peer is not even told where to sign in.
	if res := h.consoleRequest(http.MethodGet, "/api/v1/signin", "mika"); res.StatusCode == http.StatusSeeOther {
		t.Fatal("an untrusted peer was offered a sign-in")
	}
	if res := h.consoleRequest(http.MethodGet, consoleSigninPath, "mika"); res.StatusCode != http.StatusForbidden {
		t.Errorf("sign-in from an untrusted peer got %d", res.StatusCode)
	}
}

// A session is somebody's. Presented under another forwarded name it is
// nothing — so a cookie lifted from one Owner's browser does not make
// another account an administrator.
func TestASessionDoesNotTransferToAnotherAccount(t *testing.T) {
	h := freshnessHarness(t)
	for _, extra := range []string{"giulia"} {
		if err := h.s.DB.RecordButlerMember(extra, runtimedb.RoleOwner, "", ""); err != nil {
			t.Fatal(err)
		}
		h.s.DB.AdmitButlerMember(extra, "root-admin")
	}
	id := h.s.StartConsoleSession("mika", time.Now())
	if res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "giulia", sessionCookie(id)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("mika's session admitted giulia: %d", res.StatusCode)
	}
	// Capitalisation is the same account, as everywhere in Console auth.
	if res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "Mika", sessionCookie(id)); res.StatusCode != http.StatusOK {
		t.Errorf("the same account in other capitals was refused: %d", res.StatusCode)
	}
}

// The entry window (12 hours by default) is enforced by Keystone, not by
// how long Authelia's family session lasts.
func TestAStaleConsoleSessionIsRefused(t *testing.T) {
	h := freshnessHarness(t)
	now := time.Now()
	h.s.Now = func() time.Time { return now }
	id := h.s.StartConsoleSession("mika", now.Add(-11*time.Hour))
	if res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "mika", sessionCookie(id)); res.StatusCode != http.StatusOK {
		t.Fatalf("an 11-hour-old sign-in was refused: %d", res.StatusCode)
	}
	h.s.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "mika", sessionCookie(id)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a 13-hour-old sign-in still reached the Console: %d", res.StatusCode)
	}
	// The window is configurable.
	h.s.ConsoleEntryWindow = 24 * time.Hour
	if res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "mika", sessionCookie(id)); res.StatusCode != http.StatusOK {
		t.Errorf("a configured 24-hour window did not apply: %d", res.StatusCode)
	}
}

// TestFreshnessDoesNotSubstituteForOwnerRole: freshness never widens
// layer 1. A Member who has just signed in is still not an Owner.
func TestFreshnessDoesNotSubstituteForOwnerRole(t *testing.T) {
	h := freshnessHarness(t)
	id := h.s.StartConsoleSession("sandy", time.Now())
	res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "sandy", sessionCookie(id))
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a freshly signed-in Member reached the Console: %d", res.StatusCode)
	}
	// Nor are they sent to sign in again: that would bring them straight
	// back here, forever.
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	if _, ok := body["signIn"]; ok {
		t.Errorf("a signed-in Member was told to sign in again: %v", body)
	}
}

// Disabling a Person ends their Console sessions, not only their use.
func TestDisablingAPersonEndsTheirConsoleSessions(t *testing.T) {
	h := freshnessHarness(t)
	if err := h.s.DB.RecordButlerMember("giulia", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	h.s.DB.AdmitButlerMember("giulia", "root-admin")
	id := h.s.StartConsoleSession("giulia", time.Now())
	if res := h.bearerRequest(t, http.MethodPost, "/api/v1/butler/members/giulia/disable"); res != http.StatusOK {
		t.Fatalf("disable: %d", res)
	}
	if _, ok := h.s.console.sessions.Lookup(id); ok {
		t.Error("the disabled Owner's Console session still exists")
	}
}

func (h *harness) bearerRequest(t *testing.T, method, path string) int {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+h.tokens[runtimedb.RoleAdmin])
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// seedConsoleSignIn declares what a deployment with the Console behind
// Authelia has: the identity provider's login route, the Console route,
// and the Console's public client.
func seedConsoleSignIn(t *testing.T, h *harness) {
	t.Helper()
	for _, r := range []resource.Resource{
		{Kind: resource.KindRoute, Metadata: resource.Metadata{Name: "auth", Owner: "capability/identity", Node: "n1"},
			Route: &resource.RouteSpec{Host: "auth.home.test", Upstream: "container/authelia:9091", Auth: "portal", Classification: "family_public"}},
		{Kind: resource.KindRoute, Metadata: resource.Metadata{Name: "wildstone-console", Owner: consoleCapability, Node: "n1"},
			Route: &resource.RouteSpec{Host: "console.home.test", Upstream: "127.0.0.1:9441", Auth: "authelia_oidc", Classification: "admin_tailscale"}},
		{Kind: resource.KindOidcClient, Metadata: resource.Metadata{Name: consoleClient, Owner: consoleCapability, Node: "n1"},
			OidcClient: &resource.OidcClientSpec{ClientId: consoleClient, ClientName: "Wildstone Console",
				RedirectUris: []string{"https://console.home.test/api/v1/console/callback"}, Scopes: []string{"openid", "profile"},
				AuthPolicy: "one_factor", Public: true, RequirePkce: true, ConsentMode: "implicit",
				IdTokenClaims: []string{"preferred_username"}}},
	} {
		if err := h.s.State.Put(r); err != nil {
			t.Fatalf("seed %s: %v", r.Metadata.Name, err)
		}
	}
	if err := h.s.State.Commit("test", "seed", "console sign-in"); err != nil {
		t.Fatal(err)
	}
}

// signIn runs the whole round trip for user and returns the response to
// the callback and the sign-in cookie it used.
func signIn(t *testing.T, h *harness, idp *fakeidp.IdP, user, query string) (*http.Response, url.Values) {
	t.Helper()
	res := h.consoleRequest(http.MethodGet, consoleSigninPath+query, user)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in: %d", res.StatusCode)
	}
	var browser *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == consoleSigninCookie {
			browser = c
		}
	}
	if browser == nil || !browser.HttpOnly || !browser.Secure {
		t.Fatalf("sign-in cookie = %+v", browser)
	}
	code, state := idp.Authorize(t, res.Header.Get("Location"))
	cb := h.consoleRequest(http.MethodGet, consoleCallbackPath+"?"+url.Values{"code": {code}, "state": {state}}.Encode(),
		user, &http.Cookie{Name: consoleSigninCookie, Value: browser.Value})
	return cb, idp.LastAuthorize
}

func sessionFrom(res *http.Response) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == consoleSessionCookie && c.MaxAge > 0 {
			return c
		}
	}
	return nil
}

// The whole sign-in, honestly: the proxy forwards mika, Authelia signs
// a token for mika, Keystone verifies it and opens a session that
// reaches the Console and says when they authenticated.
func TestTheConsoleSignInRoundTrip(t *testing.T) {
	h := freshnessHarness(t)
	seedConsoleSignIn(t, h)
	idp := fakeidp.New(t, "https://auth.home.test", consoleClient)
	idp.User, idp.AuthTime = "mika", time.Now().Add(-5*time.Minute)
	h.s.ConsoleSignInHTTP = idp.Client()

	cb, asked := signIn(t, h, idp, "mika", "?return=/butlers")
	if cb.StatusCode != http.StatusSeeOther || cb.Header.Get("Location") != "/butlers" {
		t.Fatalf("callback = %d to %q", cb.StatusCode, cb.Header.Get("Location"))
	}
	if asked.Get("redirect_uri") != "https://console.home.test/api/v1/console/callback" || asked.Get("max_age") != "43200" {
		t.Errorf("authorization request = %v", asked)
	}
	sess := sessionFrom(cb)
	if sess == nil || !sess.HttpOnly || !sess.Secure {
		t.Fatalf("session cookie = %+v", sess)
	}
	res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "mika", sess)
	defer res.Body.Close()
	var who struct {
		Name     string     `json:"name"`
		AuthTime *time.Time `json:"authTime"`
	}
	json.NewDecoder(res.Body).Decode(&who)
	if res.StatusCode != http.StatusOK || who.AuthTime == nil || who.AuthTime.Unix() != idp.AuthTime.Unix() {
		t.Fatalf("whoami = %d %+v", res.StatusCode, who)
	}

	// Step-up asks Authelia to authenticate again.
	_, asked = signIn(t, h, idp, "mika", "?stepup=1")
	if asked.Get("prompt") != "login" {
		t.Errorf("step-up request = %v", asked)
	}
}

// The round trip refuses what it must: a token for somebody else, a
// callback from another browser, and a return address off this origin.
func TestTheConsoleSignInRefusesWhatItMust(t *testing.T) {
	h := freshnessHarness(t)
	seedConsoleSignIn(t, h)
	idp := fakeidp.New(t, "https://auth.home.test", consoleClient)
	idp.AuthTime = time.Now()
	h.s.ConsoleSignInHTTP = idp.Client()

	// Authelia signed in somebody other than the person the proxy forwards.
	idp.User = "sandy"
	if cb, _ := signIn(t, h, idp, "mika", ""); cb.StatusCode != http.StatusUnauthorized || sessionFrom(cb) != nil {
		t.Errorf("a token for sandy opened a session for mika: %d", cb.StatusCode)
	}

	// The callback, completed without the browser cookie that began it.
	idp.User = "mika"
	res := h.consoleRequest(http.MethodGet, consoleSigninPath, "mika")
	code, state := idp.Authorize(t, res.Header.Get("Location"))
	cb := h.consoleRequest(http.MethodGet, consoleCallbackPath+"?"+url.Values{"code": {code}, "state": {state}}.Encode(), "mika")
	if cb.StatusCode != http.StatusBadRequest || sessionFrom(cb) != nil {
		t.Errorf("a callback from another browser opened a session: %d", cb.StatusCode)
	}

	// An old authentication from the provider is not a fresh one.
	idp.AuthTime = time.Now().Add(-13 * time.Hour)
	if cb, _ := signIn(t, h, idp, "mika", ""); sessionFrom(cb) != nil {
		t.Error("a 13-hour-old authentication opened a session")
	}

	// A return address elsewhere lands on the Console's root.
	idp.AuthTime = time.Now()
	for _, bad := range []string{"https://evil.test/", "//evil.test/", "/\\evil.test"} {
		cb, _ := signIn(t, h, idp, "mika", "?return="+url.QueryEscape(bad))
		if loc := cb.Header.Get("Location"); loc != "/" {
			t.Errorf("return %q redirected to %q", bad, loc)
		}
	}
}

// Before wildstone-console registers its client there is nothing to sign
// in with, and the Console stays closed rather than falling back to the
// headers alone. The page says what to do about it.
func TestWithNoSignInClientTheConsoleStaysClosed(t *testing.T) {
	h := freshnessHarness(t)
	res := h.consoleRequest(http.MethodGet, consoleSigninPath, "mika")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("sign-in with no client = %d", res.StatusCode)
	}
	if res := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "mika"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the Console opened with no way to sign in: %d", res.StatusCode)
	}
}

// Keystone takes its client from the Console's own declaration and no
// other: not a confidential client (it holds no secret to use one), and
// not a same-named client some other package registered.
func TestTheConsoleUsesOnlyItsOwnPublicClient(t *testing.T) {
	base := func(owner string, public bool) []resource.Resource {
		return []resource.Resource{
			{Kind: resource.KindRoute, Metadata: resource.Metadata{Name: "auth", Owner: "capability/identity"},
				Route: &resource.RouteSpec{Host: "auth.home.test", Auth: "portal"}},
			{Kind: resource.KindRoute, Metadata: resource.Metadata{Name: "wildstone-console", Owner: consoleCapability},
				Route: &resource.RouteSpec{Host: "console.home.test", Auth: "authelia_oidc"}},
			{Kind: resource.KindOidcClient, Metadata: resource.Metadata{Name: consoleClient, Owner: owner},
				OidcClient: &resource.OidcClientSpec{ClientId: consoleClient, Public: public}},
		}
	}
	if id, iss, cb, ok := consoleSignInConfig(base(consoleCapability, true)); !ok || id != consoleClient ||
		iss != "https://auth.home.test" || cb != "https://console.home.test/api/v1/console/callback" {
		t.Fatalf("the packaged Console = %q %q %q %v", id, iss, cb, ok)
	}
	if _, _, _, ok := consoleSignInConfig(base(consoleCapability, false)); ok {
		t.Error("a confidential client was taken as the Console's")
	}
	if _, _, _, ok := consoleSignInConfig(base("capability/photos", true)); ok {
		t.Error("another package's client named wildstone-console was taken as the Console's")
	}
}

// Signing out ends the Console session and sends the browser on to the
// identity provider's logout — ending only the Console session would be
// undone, passwordless, on the next visit.
func TestSigningOutEndsTheSessionAndPointsAtLogout(t *testing.T) {
	h := freshnessHarness(t)
	seedConsoleSignIn(t, h)
	id := h.s.StartConsoleSession("mika", time.Now())
	res := h.consoleRequest(http.MethodPost, "/api/v1/console/signout", "mika", sessionCookie(id))
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusOK || out["signOut"] != "https://auth.home.test/logout" {
		t.Fatalf("sign out = %d %v", res.StatusCode, out)
	}
	if r := h.consoleRequest(http.MethodGet, "/api/v1/whoami", "mika", sessionCookie(id)); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("the session still works after signing out: %d", r.StatusCode)
	}
}
