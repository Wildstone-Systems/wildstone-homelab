package api

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/consoleauth/fakeidp"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func externalEnrollmentHarness(t *testing.T) (*harness, runtimedb.ExternalClientRegistration, string, *fakeidp.IdP) {
	t.Helper()
	h := freshnessHarness(t)
	for _, person := range []string{"mika", "sandy"} {
		if err := h.s.DB.LinkSurfaceIdentity(runtimedb.LinkTrustedIngress, person, person, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	seedConsoleSignIn(t, h)
	client := resource.Resource{Kind: resource.KindOidcClient,
		Metadata: resource.Metadata{Name: externalEnrollmentClientID, Owner: consoleCapability, Node: "n1"},
		OidcClient: &resource.OidcClientSpec{ClientId: externalEnrollmentClientID,
			ClientName: "Wildstone external Agent enrollment", Public: true, RequirePkce: true,
			ConsentMode: "explicit", RedirectUris: []string{"https://console.home.test" + externalEnrollmentCallback},
			Scopes: []string{"openid", "profile"}, IdTokenClaims: []string{"preferred_username"}, AuthPolicy: "one_factor"}}
	if err := h.s.State.Put(client); err != nil {
		t.Fatal(err)
	}
	if err := h.s.State.Commit("test", "seed", "external enrollment client"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.UpsertModel(runtimedb.Model{Name: "external-model", Kind: runtimedb.ModelCloud,
		Provider: "test", ProviderModel: "synthetic", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	agent, err := h.s.DB.CreateButler(runtimedb.Butler{Name: "sandy-external", Surface: runtimedb.ButlerSurfaceFamily,
		AgentClass: runtimedb.AgentClassMember, Model: "external-model"}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.GrantButlerAccess("sandy-external", "sandy", "root-admin"); err != nil {
		t.Fatal(err)
	}
	connectionSeq, err := h.s.DB.AddButlerAddress("sandy-external", "matrix", "sandy-agent", "direct", "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	at, err := h.s.DB.ChatConnectionBySeq(connectionSeq)
	if err != nil {
		t.Fatal(err)
	}
	const claimHash = "synthetic-enrollment-claim-hash"
	if err := h.s.DB.InsertChatClaimCode(claimHash, runtimedb.ChatClaimCode{Connection: connectionSeq,
		Identity: "sandy", CreatedBy: "root-admin", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.RedeemChatClaimCode(claimHash, at, "sandy-account", "Sandy", "direct"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.SetButlerRuntime(agent, runtimedb.ButlerRuntime{Type: agentapi.RuntimeHermes,
		Delivery: agentapi.DeliveryPull, ProcessingClass: agentapi.ProcessingCloud}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	const runtimePrincipal = "runtime-sandy-external"
	if err := h.s.DB.CreatePrincipal(runtimePrincipal, runtimedb.RoleButler, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.BindRuntimePrincipal(runtimePrincipal, agent); err != nil {
		t.Fatal(err)
	}
	staticToken, err := h.s.Principals.Mint(runtimePrincipal, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	sandy, err := h.s.DB.GetButlerMember("sandy")
	if err != nil {
		t.Fatal(err)
	}
	registration, err := h.s.DB.CreatePendingExternalRegistration(runtimedb.CreatePendingExternalRegistration{
		PersonID: sandy.PersonID, Butler: agent, Connection: connectionSeq, Issuer: "https://auth.home.test",
		ClientID: "synthetic-hermes-client", RuntimePrincipal: runtimePrincipal, ConfiguredBy: "root-admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	idp := fakeidp.New(t, "https://auth.home.test", externalEnrollmentClientID)
	idp.User, idp.AuthTime = "sandy", time.Now().Add(-time.Minute)
	h.s.EnrollmentSignInHTTP = idp.Client()
	return h, registration, staticToken, idp
}

func enrollmentRequest(t *testing.T, h *harness, method, path, user string, form url.Values, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TrustedIngressHeader, user)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func enrollmentStartRequest(t *testing.T, h *harness, registrationID, user string) (*http.Response, *http.Cookie) {
	t.Helper()
	response := enrollmentRequest(t, h, http.MethodGet,
		"/api/v1/external-enrollment/"+registrationID+"/start", user, nil)
	var browser *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == externalEnrollmentCookie {
			browser = cookie
		}
	}
	return response, browser
}

func enrollmentCallbackRequest(t *testing.T, h *harness, idp *fakeidp.IdP, start *http.Response, user string, browser *http.Cookie) *http.Response {
	t.Helper()
	code, state := idp.Authorize(t, start.Header.Get("Location"))
	return enrollmentRequest(t, h, http.MethodGet, externalEnrollmentCallback+"?"+
		url.Values{"code": {code}, "state": {state}}.Encode(), user, nil, browser)
}

var confirmationValue = regexp.MustCompile(`name="confirmation" value="([^"]+)"`)

func TestExternalEnrollmentRequiresBothIdentitiesAndExplicitConfirmation(t *testing.T) {
	h, registration, staticToken, idp := externalEnrollmentHarness(t)

	// A copied selector is not a credential: another valid household Person
	// cannot even issue the registration-bound proof.
	if response, _ := enrollmentStartRequest(t, h, registration.ID, "mika"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("copied start link for another Person = %d", response.StatusCode)
	}

	start, browser := enrollmentStartRequest(t, h, registration.ID, "sandy")
	if start.StatusCode != http.StatusSeeOther || browser == nil || !browser.HttpOnly || !browser.Secure ||
		browser.SameSite != http.SameSiteLaxMode || start.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("start = %d cookie=%+v cache=%q", start.StatusCode, browser, start.Header.Get("Cache-Control"))
	}
	callback := enrollmentCallbackRequest(t, h, idp, start, "sandy", browser)
	body, _ := io.ReadAll(callback.Body)
	callback.Body.Close()
	if callback.StatusCode != http.StatusOK || !strings.Contains(string(body), "sandy-external") ||
		!strings.Contains(string(body), "synthetic-hermes-client") || !strings.Contains(string(body), "retires every static runtime token") {
		t.Fatalf("confirmation = %d %s", callback.StatusCode, body)
	}
	if strings.Contains(string(body), "0f5a-uuid") {
		t.Fatal("verified subject appeared in browser confirmation")
	}
	stored, err := h.s.DB.ExternalClientRegistration(registration.ID)
	if err != nil || stored.State != runtimedb.ExternalRegistrationPendingConsent {
		t.Fatalf("callback activated registration: %+v, %v", stored, err)
	}
	match := confirmationValue.FindSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("confirmation form has no bounded value: %s", body)
	}
	if missing := enrollmentRequest(t, h, http.MethodPost, externalEnrollmentConfirm, "sandy",
		url.Values{}, browser); missing.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing confirmation = %d", missing.StatusCode)
	}
	confirm := enrollmentRequest(t, h, http.MethodPost, externalEnrollmentConfirm, "sandy",
		url.Values{"confirmation": {string(match[1])}}, browser)
	confirm.Body.Close()
	if confirm.StatusCode != http.StatusOK {
		t.Fatalf("confirmation POST = %d", confirm.StatusCode)
	}
	stored, err = h.s.DB.ExternalClientRegistration(registration.ID)
	if err != nil || stored.State != runtimedb.ExternalRegistrationActive || stored.Subject != "0f5a-uuid" {
		t.Fatalf("active registration = %+v, %v", stored, err)
	}
	if _, err := h.s.Principals.Authenticate(staticToken); !errors.Is(err, principal.ErrUnauthenticated) {
		t.Fatalf("retired static token authenticated: %v", err)
	}
	if replay := enrollmentRequest(t, h, http.MethodPost, externalEnrollmentConfirm, "sandy",
		url.Values{"confirmation": {string(match[1])}}, browser); replay.StatusCode != http.StatusBadRequest {
		t.Fatalf("confirmation replay = %d", replay.StatusCode)
	}
}

func TestExternalEnrollmentRefusesCrossBrowserAccountSwitchAndStaleFacts(t *testing.T) {
	t.Run("cross browser callback", func(t *testing.T) {
		h, registration, _, idp := externalEnrollmentHarness(t)
		start, _ := enrollmentStartRequest(t, h, registration.ID, "sandy")
		if callback := enrollmentCallbackRequest(t, h, idp, start, "sandy", &http.Cookie{Name: externalEnrollmentCookie, Value: "other"}); callback.StatusCode != http.StatusBadRequest {
			t.Fatalf("cross-browser callback = %d", callback.StatusCode)
		}
		stored, _ := h.s.DB.ExternalClientRegistration(registration.ID)
		if stored.State != runtimedb.ExternalRegistrationPendingConsent {
			t.Fatal("cross-browser callback changed registration")
		}
	})

	t.Run("account switch confirmation", func(t *testing.T) {
		h, registration, _, idp := externalEnrollmentHarness(t)
		start, browser := enrollmentStartRequest(t, h, registration.ID, "sandy")
		callback := enrollmentCallbackRequest(t, h, idp, start, "sandy", browser)
		body, _ := io.ReadAll(callback.Body)
		callback.Body.Close()
		match := confirmationValue.FindSubmatch(body)
		response := enrollmentRequest(t, h, http.MethodPost, externalEnrollmentConfirm, "mika",
			url.Values{"confirmation": {string(match[1])}}, browser)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("account-switched confirmation = %d", response.StatusCode)
		}
		stored, _ := h.s.DB.ExternalClientRegistration(registration.ID)
		if stored.State != runtimedb.ExternalRegistrationPendingConsent {
			t.Fatal("account-switched confirmation changed registration")
		}
	})

	t.Run("cross browser confirmation does not consume CSRF state", func(t *testing.T) {
		h, registration, _, idp := externalEnrollmentHarness(t)
		start, browser := enrollmentStartRequest(t, h, registration.ID, "sandy")
		callback := enrollmentCallbackRequest(t, h, idp, start, "sandy", browser)
		body, _ := io.ReadAll(callback.Body)
		callback.Body.Close()
		match := confirmationValue.FindSubmatch(body)
		wrong := enrollmentRequest(t, h, http.MethodPost, externalEnrollmentConfirm, "sandy",
			url.Values{"confirmation": {string(match[1])}}, &http.Cookie{Name: externalEnrollmentCookie, Value: "other"})
		if wrong.StatusCode != http.StatusBadRequest {
			t.Fatalf("cross-browser confirmation = %d", wrong.StatusCode)
		}
		correct := enrollmentRequest(t, h, http.MethodPost, externalEnrollmentConfirm, "sandy",
			url.Values{"confirmation": {string(match[1])}}, browser)
		if correct.StatusCode != http.StatusOK {
			t.Fatalf("valid confirmation after cross-browser refusal = %d", correct.StatusCode)
		}
	})

	t.Run("authority changed after callback", func(t *testing.T) {
		h, registration, _, idp := externalEnrollmentHarness(t)
		start, browser := enrollmentStartRequest(t, h, registration.ID, "sandy")
		callback := enrollmentCallbackRequest(t, h, idp, start, "sandy", browser)
		body, _ := io.ReadAll(callback.Body)
		callback.Body.Close()
		match := confirmationValue.FindSubmatch(body)
		if err := h.s.DB.RevokeButlerAccess("sandy-external", "sandy", "root-admin"); err != nil {
			t.Fatal(err)
		}
		response := enrollmentRequest(t, h, http.MethodPost, externalEnrollmentConfirm, "sandy",
			url.Values{"confirmation": {string(match[1])}}, browser)
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("stale-authority confirmation = %d", response.StatusCode)
		}
		stored, _ := h.s.DB.ExternalClientRegistration(registration.ID)
		if stored.State != runtimedb.ExternalRegistrationPendingConsent {
			t.Fatal("stale-authority confirmation changed registration")
		}
	})

	t.Run("fallback disclosure changed after callback", func(t *testing.T) {
		h, registration, _, idp := externalEnrollmentHarness(t)
		start, browser := enrollmentStartRequest(t, h, registration.ID, "sandy")
		callback := enrollmentCallbackRequest(t, h, idp, start, "sandy", browser)
		body, _ := io.ReadAll(callback.Body)
		callback.Body.Close()
		if !strings.Contains(string(body), "Cross-connection fallback</dt><dd>Disabled") {
			t.Fatalf("confirmation omitted fallback consequence: %s", body)
		}
		match := confirmationValue.FindSubmatch(body)
		if _, err := h.s.DB.ExecForTest(`UPDATE connection_disclosure SET accepts_fallback=1 WHERE connection=?`, registration.Connection); err != nil {
			t.Fatal(err)
		}
		response := enrollmentRequest(t, h, http.MethodPost, externalEnrollmentConfirm, "sandy",
			url.Values{"confirmation": {string(match[1])}}, browser)
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("stale-fallback confirmation = %d", response.StatusCode)
		}
		stored, _ := h.s.DB.ExternalClientRegistration(registration.ID)
		if stored.State != runtimedb.ExternalRegistrationPendingConsent {
			t.Fatal("stale-fallback confirmation changed registration")
		}
	})
}

func TestEnrollmentAndConsoleSelectDistinctPublicClients(t *testing.T) {
	desired := []resource.Resource{
		{Kind: resource.KindRoute, Metadata: resource.Metadata{Name: "auth", Owner: "capability/identity"},
			Route: &resource.RouteSpec{Host: "auth.home.test", Auth: "portal"}},
		{Kind: resource.KindRoute, Metadata: resource.Metadata{Name: "console", Owner: consoleCapability},
			Route: &resource.RouteSpec{Host: "console.home.test", Auth: "authelia_oidc"}},
		{Kind: resource.KindOidcClient, Metadata: resource.Metadata{Name: consoleClient, Owner: consoleCapability},
			OidcClient: &resource.OidcClientSpec{ClientId: consoleClient, Public: true}},
		{Kind: resource.KindOidcClient, Metadata: resource.Metadata{Name: externalEnrollmentClientID, Owner: consoleCapability},
			OidcClient: &resource.OidcClientSpec{ClientId: externalEnrollmentClientID, Public: true}},
	}
	if id, _, _, ok := consoleSignInConfig(desired); !ok || id != consoleClient {
		t.Fatalf("Console selected client %q", id)
	}
	if id, issuer, callback, ok := externalEnrollmentSignInConfig(desired); !ok || id != externalEnrollmentClientID ||
		issuer != "https://auth.home.test" || callback != "https://console.home.test"+externalEnrollmentCallback {
		t.Fatalf("enrollment config = %q %q %q %v", id, issuer, callback, ok)
	}
}
