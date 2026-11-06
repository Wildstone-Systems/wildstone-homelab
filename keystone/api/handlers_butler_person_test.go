package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// A Console session and a separately authenticated API principal remain
// distinct principals, but both derive the same canonical Person before any
// Agent work or audit row is written.
func TestConsoleAndLinkedAPIPrincipalUseOneCanonicalActingPerson(t *testing.T) {
	h := newHarness(t)
	h.s.SetTrustedIngress([]string{"127.0.0.1", "::1"})
	h.owner("mika")
	provider := &scriptedProvider{turns: []*provider.ChatResponse{
		{Text: "console answer"},
		{Text: "api answer"},
	}}
	h.s.ButlerHost = &ButlerHost{
		Provider: provider,
		Gateway:  butler.NewToolGateway(h.s.State, nil),
		Mode:     runtimedb.ButlerModeFull,
	}

	// The credential's label casing is not the canonical stored handle.
	if err := h.s.Principals.Create("linked-client", runtimedb.RoleOperator, "MIKA"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.LinkSurfaceIdentity(runtimedb.LinkAPIPrincipal, "linked-client", "mika", "mika"); err != nil {
		t.Fatal(err)
	}
	linked, err := h.s.Principals.Mint("linked-client", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	session := sessionCookie(h.s.StartConsoleSession("mika", time.Now()))
	if code, _ := h.sessionRequest(http.MethodPost, "/api/v1/butler/ask", `{"message":"from Console"}`, "mika", session); code != http.StatusOK {
		t.Fatalf("Console ask = %d", code)
	}
	if res := h.doToken(linked, http.MethodPost, "/api/v1/butler/ask",
		map[string]string{"message": "from API"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("linked API ask = %d", res.StatusCode)
	}

	sessions, err := h.s.DB.ListButlerSessions(10, runtimedb.ButlerSessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
	for _, session := range sessions {
		if session.ActingFor != "mika" {
			t.Fatalf("session attribution = %+v", session)
		}
	}

	taskID, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner,
		Audience: runtimedb.ButlerAudiencePrivate, Objective: "check attribution",
		CompletionCriteria: "two labels",
	})
	if err != nil {
		t.Fatal(err)
	}
	feedbackPath := "/api/v1/butler/tasks/" + taskID + "/feedback"
	if code, _ := h.sessionRequest(http.MethodPost, feedbackPath,
		`{"label":"useful-completion","note":"Console"}`, "mika", session); code != http.StatusOK {
		t.Fatalf("Console feedback = %d", code)
	}
	if res := h.doToken(linked, http.MethodPost, feedbackPath,
		map[string]string{"label": "useful-completion", "note": "API"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("linked API feedback = %d", res.StatusCode)
	}
	feedback, err := h.s.DB.ListButlerFeedback(taskID)
	if err != nil || len(feedback) != 2 {
		t.Fatalf("feedback = %+v, err=%v", feedback, err)
	}
	for _, label := range feedback {
		if label.Author != "mika" {
			t.Fatalf("task feedback attribution = %+v", label)
		}
	}

	audit, err := h.s.DB.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range audit {
		if row.Path == "/api/v1/butler/ask" && row.Status == http.StatusOK {
			if row.ActingFor != "mika" {
				t.Fatalf("audit acting Person = %q for principal %q", row.ActingFor, row.Principal)
			}
			seen[row.Principal] = true
		}
	}
	if !seen["mika"] || !seen["linked-client"] {
		t.Fatalf("audit principals = %v, want Console and linked API identities", seen)
	}
}

// A machine credential has an authenticating principal but no Person. It may
// not borrow a same-named Person or reach inference, task creation, or session
// persistence through the interactive Agent routes.
func TestMachinePrincipalCannotImpersonateAnActingPerson(t *testing.T) {
	h := newHarness(t)
	h.person("op")
	provider := &scriptedProvider{turns: []*provider.ChatResponse{{Text: "must not run"}}}
	h.s.ButlerHost = &ButlerHost{
		Provider: provider,
		Gateway:  butler.NewToolGateway(h.s.State, nil),
		Mode:     runtimedb.ButlerModeFull,
	}

	if res := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/butler/ask",
		map[string]string{"message": "pretend I am op"}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("machine ask = %d, want 403", res.StatusCode)
	}
	if provider.i != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.i)
	}
	sessions, err := h.s.DB.ListButlerSessions(10, runtimedb.ButlerSessionFilter{})
	if err != nil || len(sessions) != 0 {
		t.Fatalf("machine ask left sessions = %+v, err=%v", sessions, err)
	}
	tasks, err := h.s.DB.ListButlerTasks(10, "")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("machine ask left tasks = %+v, err=%v", tasks, err)
	}
}

func TestUnknownOrAmbiguousActingPersonFailsAuthentication(t *testing.T) {
	h := newHarness(t)
	h.person("mika", "Mika")
	for name, actingFor := range map[string]string{
		"missing-person":   "nobody",
		"ambiguous-person": "MIKA",
	} {
		if err := h.s.Principals.Create(name, runtimedb.RoleOperator, actingFor); err != nil {
			t.Fatal(err)
		}
		token, err := h.s.Principals.Mint(name, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if res := h.doToken(token, http.MethodGet, "/api/v1/whoami", nil, nil); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s = %d, want 401", name, res.StatusCode)
		}
	}
}
