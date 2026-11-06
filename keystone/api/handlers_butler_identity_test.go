package api

// P6's evidence at the API surface (ADR-W030): the same admitted adult
// hands a conversation between bots with authority recomputed, and
// unlinked, disabled and household-only identities fail closed.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerlive"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerstore"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestButlerAdmissionAndHandoffThroughTheAPI(t *testing.T) {
	h := newHarness(t)

	// A machine administrator may establish the first Person record, but
	// it cannot perform a Person-authored operation on that Person's behalf.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/members",
		map[string]any{"member": "root-admin", "role": "owner"}, nil,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("record member = %d", res.StatusCode)
	}
	// Recorded is not admitted: a conversation is refused until someone
	// decides.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/conversations",
		map[string]any{"subject": "photos"}, nil,
	); res.StatusCode != http.StatusForbidden {
		t.Fatalf("an unadmitted member started a conversation: %d", res.StatusCode)
	}

	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/members/root-admin/admit", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("admit = %d", res.StatusCode)
	}
	rootAdmin, err := h.s.Principals.Mint("root-admin", "root-admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		ConversationID string `json:"conversationId"`
	}
	if res := h.doToken(rootAdmin, http.MethodPost, "/api/v1/butler/conversations",
		map[string]any{"subject": "photos"}, &started,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("start conversation = %d", res.StatusCode)
	}

	// A handoff to the operator bot works for an operator-profile member.
	var ctx struct {
		Turns    []runtimedb.ButlerTurn `json:"turns"`
		Withheld int                    `json:"withheld"`
	}
	if res := h.doToken(rootAdmin, http.MethodPost,
		"/api/v1/butler/conversations/"+started.ConversationID+"/handoff",
		map[string]any{"from": "family", "to": "admin"}, &ctx,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("handoff = %d", res.StatusCode)
	}

	// Somebody else's conversation answers 404, not 403: a 403 confirms
	// it exists.
	other := h.console(runtimedb.RoleOperator, "op")
	if res := h.doToken(other, http.MethodPost,
		"/api/v1/butler/conversations/"+started.ConversationID+"/handoff",
		map[string]any{"from": "family", "to": "admin"}, nil,
	); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another member's handoff = %d, want 404", res.StatusCode)
	}

	// A second Owner, so offboarding root-admin below is refused for no
	// reason other than the one this test is about: the Last Owner
	// invariant will not let a household lose its only one (§2.5).
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/members",
		map[string]any{"member": "dana", "role": "owner"}, nil,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("record second owner = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/members/dana/admit", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("admit second owner = %d", res.StatusCode)
	}

	// Offboarding denies immediately and pauses unfinished personal work.
	taskID, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "root-admin", Owner: "root-admin",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: "operator",
		Audience:  runtimedb.ButlerAudiencePrivate,
		Objective: "keep looking", CompletionCriteria: "answered",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.s.SetTrustedIngress([]string{"127.0.0.1", "::1"})
	session := h.s.StartConsoleSession("dana", time.Now())
	res := h.consoleRequest(http.MethodPost, "/api/v1/butler/members/root-admin/offboard", "dana", sessionCookie(session))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("offboard = %d", res.StatusCode)
	}
	if member, _ := h.s.DB.GetButlerMember("root-admin"); member.Active() {
		t.Fatal("offboarding kept the Person active")
	}
	task, _ := h.s.DB.GetButlerTask(taskID)
	if task.State != runtimedb.ButlerTaskPausedOffboarded {
		t.Fatalf("task after offboarding = %q", task.State)
	}
	// And nothing more can be started.
	if res := h.doToken(rootAdmin, http.MethodPost, "/api/v1/butler/conversations",
		map[string]any{"subject": "again"}, nil,
	); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an offboarded member started a conversation: %d, want 401", res.StatusCode)
	}
}

// The write half of admission needs manage. The Butler's own role does
// not have it, so there is no request the Butler can make that admits
// anyone — including itself.
func TestButlerCannotAdmitAnyone(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		path string
		body any
	}{
		{"/api/v1/butler/members", map[string]any{"member": "x", "role": "owner"}},
		{"/api/v1/butler/members/x/admit", nil},
		{"/api/v1/butler/members/x/links", map[string]any{"linkType": "api-principal", "externalId": "x"}},
	} {
		res := h.do(runtimedb.RoleButler, http.MethodPost, tc.path, tc.body, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("butler POST %s = %d, want 403", tc.path, res.StatusCode)
		}
	}
}

func TestAPIPrincipalGenerationFencesOldSessionAndTask(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(answerTurns("first", "new generation"), runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "mika", "telegram", "synthetic-account")
	token := h.console(runtimedb.RoleOperator, "mika")
	apiPrincipal := fixturePersonPrincipal(h.nextPersonPrincipal)

	var first askResponse
	if res := h.doToken(token, http.MethodPost, "/api/v1/household/butler/ask",
		map[string]string{"message": "hello"}, &first); res.StatusCode != http.StatusOK {
		t.Fatalf("first ask = %d", res.StatusCode)
	}
	member, err := h.s.DB.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "mika", Owner: "mika",
		IdentityGeneration: member.Generation, Surface: runtimedb.ButlerSurfaceFamily,
		AgentClass: runtimedb.AgentClassMember, Audience: runtimedb.ButlerAudiencePrivate,
		OriginConnection: "console", Objective: "synthetic identity fence",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := h.s.DB.AcquireCoordinator("api-effect-fixture")
	if err != nil {
		t.Fatal(err)
	}
	ledger := butlerstore.New(h.s.DB, lease)
	ledger.DerivedAuthorityChecks()

	if err := h.s.DB.RevokeSurfaceLink(runtimedb.LinkAPIPrincipal, apiPrincipal, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if res := h.doToken(token, http.MethodPost, "/api/v1/household/butler/ask",
		map[string]string{"message": "stale"}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked API link dispatched = %d", res.StatusCode)
	}
	if err := h.s.DB.ButlerSessionActive(first.SessionID); !errors.Is(err, runtimedb.ErrSessionRevoked) {
		t.Fatalf("old session stayed active: %v", err)
	}
	if err := ledger.Authorize(taskID); !errors.Is(err, butlerstore.ErrAuthorityWithdrawn) {
		t.Fatalf("old task kept authority: %v", err)
	}

	if err := h.s.DB.AdmitButlerMember("mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.LinkSurfaceIdentity(runtimedb.LinkAPIPrincipal, apiPrincipal, "mika", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if res := h.doToken(token, http.MethodPost, "/api/v1/household/butler/ask",
		map[string]string{"message": "fresh"}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("current generation ask = %d", res.StatusCode)
	}
	if err := ledger.Authorize(taskID); !errors.Is(err, butlerstore.ErrAuthorityWithdrawn) {
		t.Fatalf("re-admission revived old task: %v", err)
	}
}

type revokingSurfaceButlers struct {
	db       *runtimedb.DB
	external string
}

func (b *revokingSurfaceButlers) Any() (bool, error) { return true, nil }
func (b *revokingSurfaceButlers) Sole(runtimedb.ButlerSurface, string, bool) (runtimedb.Butler, error) {
	return b.db.GetButler("sandy")
}
func (b *revokingSurfaceButlers) Serve(context.Context, butlerlive.Turn) (butlerlive.Answer, error) {
	if err := b.db.RevokeSurfaceLink(runtimedb.LinkAPIPrincipal, b.external, "root-admin"); err != nil {
		return butlerlive.Answer{}, err
	}
	return butlerlive.Answer{Butler: "sandy", Surface: runtimedb.ButlerSurfaceFamily,
		Provider: "synthetic", FinalText: "must not be published"}, nil
}

func TestGenerationIsRecheckedAfterModelBeforePublication(t *testing.T) {
	h := newHarness(t)
	h.bindAccount(t, "family", "sam", "telegram", "synthetic-account")
	token := h.console(runtimedb.RoleOperator, "sam")
	h.s.Butlers = &revokingSurfaceButlers{db: h.s.DB, external: "fixture-person-1"}
	if res := h.doToken(token, http.MethodPost, "/api/v1/household/butler/ask",
		map[string]string{"message": "revoke during model call"}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("late-revoked answer published = %d", res.StatusCode)
	}
	if sessions, err := h.s.DB.ListButlerSessions(10, runtimedb.ButlerSessionFilter{ActingFor: "sam"}); err != nil || len(sessions) != 0 {
		t.Fatalf("late-revoked turn persisted: %+v err=%v", sessions, err)
	}
}
