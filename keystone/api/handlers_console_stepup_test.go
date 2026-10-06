package api

// Step-up for consequential Console operations (ADR-W033 A4.1, #205).

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/consoleauth/fakeidp"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// consequentialRoutes is every operation A4.1 gates, as a request that
// reaches the gate. The gate runs before the handler reads anything, so
// the bodies only need to be well-formed enough for the ones that check
// inside the handler.
var consequentialRoutes = []struct{ method, path, body string }{
	{http.MethodPost, "/api/v1/principals", `{"name":"x","role":"admin"}`},
	{http.MethodPost, "/api/v1/principals/root-admin/disable", ``},
	{http.MethodPost, "/api/v1/principals/root-admin/tokens", `{}`},
	{http.MethodPost, "/api/v1/join-tokens", `{}`},
	{http.MethodPost, "/api/v1/nodes/n1/secrets/s1/rotate", `{}`},
	{http.MethodPost, "/api/v1/backups/p1/restore", `{}`},
	{http.MethodPost, "/api/v1/household/publishers/home-automation", `{}`},
	// Pairing mints a publisher token and stores a credential; unpairing
	// revokes them (ADR-W044 A6).
	{http.MethodPost, "/api/v1/pantry/pairing", `{}`},
	{http.MethodDelete, "/api/v1/pantry/pairing", ``},
	{http.MethodPost, "/api/v1/pantry/grocery-move", ``},
	{http.MethodPost, "/api/v1/butler/members/giulia/disable", ``},
	{http.MethodPost, "/api/v1/butler/members/giulia/offboard", ``},
	{http.MethodPost, "/api/v1/butler/members/giulia/offboarding", `{}`},
	{http.MethodPost, "/api/v1/restore/quarantine", `{}`},
	{http.MethodPost, "/api/v1/restore/reconcile", `{}`},
	{http.MethodDelete, "/api/v1/butlers/joan", ``},
	{http.MethodDelete, "/api/v1/butlers/joan?transferMemoryTo=sandy", ``},
	{http.MethodPut, "/api/v1/butlers/joan/memory-restrictions", `{"scope":"household","restricted":true}`},
	{http.MethodPut, "/api/v1/butlers/joan/memory-restrictions", `{"scope":"household","restricted":false}`},
	{http.MethodPost, "/api/v1/chat/adapters/chat-telegram-family", `{"node":"n1","butler":"joan"}`},
	// An Agent's runtime (ADR-W039 §3.7, §4.1): replacing it revokes a
	// credential, rotating mints one, revoking withdraws every one.
	{http.MethodPut, "/api/v1/butlers/joan/runtime", `{"type":"hermes"}`},
	{http.MethodPost, "/api/v1/butlers/joan/runtime/credential", `{}`},
	{http.MethodPost, "/api/v1/butlers/joan/runtime/revoke", ``},
	// Who a Connection admits (design §8.1; slice 11). Consequential in
	// both directions — narrowing withdraws a way the household reaches
	// an Agent, widening hands out access nobody had.
	{http.MethodPost, "/api/v1/connections/chat-telegram-family/admission", `{"admits":"owners"}`},
	// Removals, gated inside their handlers.
	{http.MethodPost, "/api/v1/services/media/access", `{"subjectKind":"person","subject":"giulia","effect":"restrict"}`},
	{http.MethodPost, "/api/v1/services/media/access", `{"subjectKind":"person","subject":"giulia","effect":""}`},
	{http.MethodPost, "/api/v1/household/person-groups/adults/members", `{"member":"giulia","in":false}`},
	// A group the household makes or deletes is a name every entitlement,
	// audience and secret grant can then use (ADR-W033 A4.1).
	{http.MethodPost, "/api/v1/household/person-groups", `{"name":"Neighbours"}`},
	{http.MethodDelete, "/api/v1/household/person-groups/adults", ``},
}

// stepUpHarness adds giulia, a second active Owner, so the lifecycle
// operations are not refused by the Last Owner invariant instead.
func stepUpHarness(t *testing.T) *harness {
	t.Helper()
	h := freshnessHarness(t)
	if err := h.s.DB.RecordButlerMember("giulia", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("giulia", "root-admin"); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) sessionRequest(method, path, body, user string, session *http.Cookie) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set(TrustedIngressHeader, user)
	req.Header.Set(ConsoleGroupsHeader, ConsoleGroup)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(session)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

// TestConsequentialOperationWithStaleAuthIsRejected: an hour-old sign-in
// is good enough to be in the Console and not good enough for any of
// these. Each is refused with a step-up challenge, not a bare denial,
// and none of them happened.
func TestConsequentialOperationWithStaleAuthIsRejected(t *testing.T) {
	h := stepUpHarness(t)
	session := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-time.Hour)))
	for _, rt := range consequentialRoutes {
		code, out := h.sessionRequest(rt.method, rt.path, rt.body, "mika", session)
		if code != http.StatusForbidden || out["stepUp"] != consoleSigninPath+"?stepup=1" {
			t.Errorf("%s %s with a stale sign-in = %d %v, want 403 with a step-up", rt.method, rt.path, code, out)
		}
	}
	if m, err := h.s.DB.ButlerMembersByHandleFold("giulia"); err != nil || len(m) != 1 || !m[0].Active() {
		t.Errorf("a refused offboarding still changed giulia: %+v %v", m, err)
	}
}

// TestConsequentialOperationAfterStepUpSucceeds: refused, sent round the
// step-up sign-in, back — and the same operation now goes through.
func TestConsequentialOperationAfterStepUpSucceeds(t *testing.T) {
	h := stepUpHarness(t)
	seedConsoleSignIn(t, h)
	idp := fakeidp.New(t, "https://auth.home.test", consoleClient)
	idp.User = "mika"
	h.s.ConsoleSignInHTTP = idp.Client()

	stale := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-time.Hour)))
	code, out := h.sessionRequest(http.MethodPost, "/api/v1/butler/members/giulia/disable", "", "mika", stale)
	if code != http.StatusForbidden {
		t.Fatalf("stale disable = %d", code)
	}
	idp.AuthTime = time.Now()
	cb, asked := signIn(t, h, idp, "mika", strings.TrimPrefix(out["stepUp"].(string), consoleSigninPath)+"&return=/household")
	if asked.Get("prompt") != "login" || cb.Header.Get("Location") != "/household" {
		t.Fatalf("step-up asked %v and returned to %q", asked, cb.Header.Get("Location"))
	}
	if code, out := h.sessionRequest(http.MethodPost, "/api/v1/butler/members/giulia/disable", "", "mika", sessionFrom(cb)); code != http.StatusOK {
		t.Fatalf("disable after step-up = %d %v", code, out)
	}
}

// TestOrdinaryOperationDoesNotChallengeInsideTheWindow: the gate is for
// the short list, not for everything. With the same hour-old sign-in,
// reading, admitting, granting and adding go straight through — a
// Console that challenged everything would be unusable.
func TestOrdinaryOperationDoesNotChallengeInsideTheWindow(t *testing.T) {
	h := stepUpHarness(t)
	if err := h.s.DB.RecordButlerMember("sandy2", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	session := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-time.Hour)))
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/whoami", ""},
		{http.MethodGet, "/api/v1/butlers", ""},
		{http.MethodPost, "/api/v1/butler/members/sandy2/admit", ""},
		{http.MethodPost, "/api/v1/services/media/access", `{"subjectKind":"person","subject":"giulia","effect":"allow"}`},
		{http.MethodPost, "/api/v1/household/person-groups/adults/members", `{"member":"giulia","in":true}`},
	} {
		if code, out := h.sessionRequest(rt.method, rt.path, rt.body, "mika", session); out["stepUp"] != nil || code == http.StatusUnauthorized {
			t.Errorf("%s %s challenged an ordinary operation: %d %v", rt.method, rt.path, code, out)
		}
	}
}

// Bearer tokens are exempt (A4.1): the CLI, the installer and host
// recovery have no sign-in to be stale.
func TestABearerTokenIsNotAskedToStepUp(t *testing.T) {
	h := stepUpHarness(t)
	if code := h.bearerRequest(t, http.MethodPost, "/api/v1/butler/members/giulia/disable"); code != http.StatusOK {
		t.Fatalf("a token was refused a consequential operation: %d", code)
	}
}

// Every gated route answers with a step-up, never a 401: a person
// already inside the entry window is not signed out by trying one.
func TestAStepUpRefusalDoesNotSignThePersonOut(t *testing.T) {
	h := stepUpHarness(t)
	session := sessionCookie(h.s.StartConsoleSession("mika", time.Now().Add(-time.Hour)))
	h.sessionRequest(http.MethodDelete, "/api/v1/butlers/joan", "", "mika", session)
	if code, _ := h.sessionRequest(http.MethodGet, "/api/v1/whoami", "", "mika", session); code != http.StatusOK {
		t.Errorf("after a step-up refusal the session is gone: %d", code)
	}
}
