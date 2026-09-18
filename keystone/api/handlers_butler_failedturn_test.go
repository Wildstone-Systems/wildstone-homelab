package api

// A failed turn keeps the work it already did.
//
// A provider can fail after tool calls have completed, and some of those
// calls change the house. If the durable task is not attached to a
// conversation before the error goes back, the next message opens a
// fresh task and runs those calls again — which is how a door gets
// unlocked twice.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func failedTurnTask(t *testing.T, h *harness) string {
	t.Helper()
	id, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: runtimedb.ButlerSurfaceFamily, Profile: runtimedb.ProfileHousehold,
		Audience:  runtimedb.ButlerAudiencePrivate,
		Objective: "unlock the back door", CompletionCriteria: "done",
		Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The task a failed turn was running is linked to a session, so the
// next turn resumes it instead of starting over.
func TestAFailedTurnKeepsItsDurableTask(t *testing.T) {
	h := newHarness(t)
	taskID := failedTurnTask(t, h)

	out := h.s.preserveFailedTurn(
		butlerlive.Turn{Member: "sam", Text: "unlock the back door"},
		nil,
		butlerlive.Answer{
			TaskID:   taskID,
			Butler:   "alfred",
			Surface:  runtimedb.ButlerSurfaceFamily,
			Provider: "scripted",
			Calls: []butler.ToolCallRecord{
				{Name: "unlock_door", Result: "unlocked"},
			},
		})

	ask, ok := out.(askResponse)
	if !ok || ask.SessionID == "" {
		t.Fatalf("a failed turn returned no resumable session: %#v", out)
	}
	// The task now belongs to that conversation, which is what
	// ButlerTaskForSession needs in order to resume rather than restart.
	got, err := h.s.DB.ButlerTaskForSession(ask.SessionID)
	if err != nil {
		t.Fatalf("the task was not linked to the session: %v", err)
	}
	if got.ID != taskID {
		t.Fatalf("session resolved to task %s, want %s", got.ID, taskID)
	}
	// The question is on the record; the Butler never answered, so no
	// assistant turn was invented for it.
	var sess runtimedb.ButlerSession
	if res := h.do(runtimedb.RoleAdmin, "GET", "/api/v1/butler/sessions/"+ask.SessionID, nil, &sess); res.StatusCode != 200 {
		t.Fatalf("get session: %d", res.StatusCode)
	}
	if len(sess.Turns) != 1 || sess.Turns[0].Role != runtimedb.ButlerRoleUser {
		t.Fatalf("turns = %+v, want only the question", sess.Turns)
	}
	// And what DID run before the failure is answerable afterwards.
	if sess.TranscriptJSON == "" {
		t.Error("the completed tool call was not recorded")
	}
}

// A turn that failed before any work started has nothing to keep, and
// must not manufacture an empty conversation for it.
func TestATurnThatFailedBeforeAnyWorkKeepsNothing(t *testing.T) {
	h := newHarness(t)
	if out := h.s.preserveFailedTurn(
		butlerlive.Turn{Member: "sam", Text: "hello"},
		nil,
		butlerlive.Answer{}, // no TaskID: refused before a task existed
	); out != nil {
		t.Fatalf("a turn with no task produced %#v", out)
	}
}

// A follow-up that fails after running tools keeps those calls too.
//
// recordTurn appends turns to an existing session and never touches the
// session-level fields, so a transcript written only there survives the
// first turn of a conversation and silently vanishes on every one after
// it — while the activity line still claims the calls were kept.
func TestAFailedFollowUpKeepsItsCompletedCalls(t *testing.T) {
	h := newHarness(t)

	// A first, successful-looking turn, so there is a session to append
	// to and a prior context pointing at it.
	first, err := h.s.DB.InsertButlerSession(runtimedb.ButlerSession{
		Principal: butlerPrincipal, ActingFor: "sam",
		Mode: runtimedb.ButlerModeFull, Surface: runtimedb.ButlerSurfaceFamily,
		Butler: "alfred", FinalText: "done",
		Turns: []runtimedb.ButlerTurn{
			{Role: runtimedb.ButlerRoleUser, Text: "lights on"},
			{Role: runtimedb.ButlerRoleAssistant, Text: "done"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	taskID := failedTurnTask(t, h)
	out := h.s.preserveFailedTurn(
		butlerlive.Turn{Member: "sam", Text: "now unlock the back door", SessionID: first},
		&turnContext{SessionID: first},
		butlerlive.Answer{
			TaskID: taskID, Butler: "alfred", Surface: runtimedb.ButlerSurfaceFamily,
			Calls: []butler.ToolCallRecord{{Name: "unlock_door", Result: "unlocked"}},
		})

	ask, ok := out.(askResponse)
	if !ok || ask.SessionID != first {
		t.Fatalf("a failed follow-up did not append to its session: %#v", out)
	}
	var sess runtimedb.ButlerSession
	if res := h.do(runtimedb.RoleAdmin, "GET", "/api/v1/butler/sessions/"+first, nil, &sess); res.StatusCode != 200 {
		t.Fatalf("get session: %d", res.StatusCode)
	}
	if len(sess.Turns) != 3 {
		t.Fatalf("turns = %d, want 3 (the first pair plus the failed question)", len(sess.Turns))
	}
	last := sess.Turns[2]
	if last.Role != runtimedb.ButlerRoleUser {
		t.Fatalf("last turn = %s, want the question", last.Role)
	}
	// The completed call is on the record, on the turn that ran it.
	if !strings.Contains(last.TranscriptJSON, "unlock_door") {
		t.Errorf("the completed call was lost on a follow-up: %q", last.TranscriptJSON)
	}
}

// A failed request that has something to hand back still hands it back.
//
// handle writes only {"error": ...} when a handler errors, which would
// drop the session holding a preserved task — and the Console and API
// have no server-side thread pointer to recover it from.
func TestAnErrorResponseCarriesAPreservedSession(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		body any
		want map[string]string
	}{
		{
			name: "a plain failure is unchanged",
			body: nil,
			want: map[string]string{"error": "it broke"},
		},
		{
			name: "a preserved session rides along",
			body: askResponse{SessionID: "bs-7", Mode: "full"},
			// finalText has no omitempty, so it is legitimately present
			// and empty: the Butler produced no answer.
			want: map[string]string{"error": "it broke", "sessionId": "bs-7", "mode": "full", "finalText": ""},
		},
		{
			name: "the error wins over a field of the same name",
			body: map[string]any{"error": "something else", "sessionId": "bs-8"},
			want: map[string]string{"error": "it broke", "sessionId": "bs-8"},
		},
		{
			name: "a body that is not an object degrades to the error",
			body: []string{"not", "an", "object"},
			want: map[string]string{"error": "it broke"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.s.writeErrorBody(rec, http.StatusBadGateway, "it broke", tc.body)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d", rec.Code)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body is not an object: %s", rec.Body.String())
			}
			for k, want := range tc.want {
				if fmt.Sprint(got[k]) != want {
					t.Errorf("%s = %v, want %q (body %s)", k, got[k], want, rec.Body.String())
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("fields = %v, want exactly %v", got, tc.want)
			}
		})
	}
}

// And handle is actually wired to it.
//
// Testing writeErrorBody alone proves the merge and not the plumbing:
// handle could go on dropping the body and that test would still pass.
// This drives a real request through handle with a handler that returns
// a body AND an error, which is the shape a failed Butler turn now has.
func TestHandlePassesAPreservedBodyThroughOnError(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.s.handle(principal.VerbRead,
		func(*http.Request, principal.Identity) (any, int, error) {
			return askResponse{SessionID: "bs-9", Mode: "full"}, http.StatusBadGateway,
				fmt.Errorf("the provider gave up")
		}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.tokens[runtimedb.RoleAdmin])
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["error"] != "the provider gave up" {
		t.Errorf("error = %v", got["error"])
	}
	if got["sessionId"] != "bs-9" {
		t.Errorf("the preserved session did not survive handle: %v", got)
	}
}

// An accepted proposal survives the turn that failed around it.
//
// ResourceYAML lives on the tool call and not in the stored transcript,
// and the resume path reconstructs only the tool RESULT — so a proposal
// that is not submitted here reaches the next successful turn with
// nothing to submit, and the plan the person asked for disappears with
// nobody told.
func TestAFailedTurnStillSubmitsAnAcceptedProposal(t *testing.T) {
	h := newHarness(t)
	taskID := failedTurnTask(t, h)

	out := h.s.preserveFailedTurn(
		butlerlive.Turn{Member: "sam", Text: "expose grafana"},
		nil,
		butlerlive.Answer{
			TaskID: taskID, Butler: "alfred", Surface: runtimedb.ButlerSurfaceFamily,
			Calls: []butler.ToolCallRecord{{
				Name:         "propose_change",
				Result:       "accepted",
				ResourceYAML: containerYAML(t, "grafana", "ghcr.io/example/grafana@"+testDigest, nil),
			}},
		})

	ask, ok := out.(askResponse)
	if !ok {
		t.Fatalf("no response: %#v", out)
	}
	if len(ask.PlanIDs) != 1 {
		t.Fatalf("planIds = %v (errors %v), want one plan from the failed turn",
			ask.PlanIDs, ask.ProposalErrors)
	}
	// It is a real plan record, proposed and awaiting a human — a
	// proposal is not an action, which is why submitting one from a
	// failed turn is safe (ADR-W017 A3).
	var rec runtimedb.PlanRecord
	if res := h.do(runtimedb.RoleAdmin, "GET", "/api/v1/plans/"+ask.PlanIDs[0], nil, &rec); res.StatusCode != 200 {
		t.Fatalf("get plan: %d", res.StatusCode)
	}
	if rec.State != runtimedb.PlanProposed {
		t.Errorf("plan state = %s, want proposed", rec.State)
	}
	// And the turn names it, so the Console can show the proposal beside
	// the question that caused it.
	var sess runtimedb.ButlerSession
	if res := h.do(runtimedb.RoleAdmin, "GET", "/api/v1/butler/sessions/"+ask.SessionID, nil, &sess); res.StatusCode != 200 {
		t.Fatalf("get session: %d", res.StatusCode)
	}
	if len(sess.Turns) != 1 || len(sess.Turns[0].PlanIDs) != 1 {
		t.Fatalf("the failed turn does not name its proposal: %+v", sess.Turns)
	}
}
