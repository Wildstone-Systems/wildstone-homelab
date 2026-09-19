package api

// P6's evidence at the API surface (ADR-W030): the same admitted adult
// hands a conversation between bots with authority recomputed, and
// unlinked, disabled and household-only identities fail closed.

import (
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestButlerAdmissionAndHandoffThroughTheAPI(t *testing.T) {
	h := newHarness(t)

	// The admin token acts as "root-admin", which is who the harness
	// mints it for.
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
	var started struct {
		ConversationID string `json:"conversationId"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/conversations",
		map[string]any{"subject": "photos"}, &started,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("start conversation = %d", res.StatusCode)
	}

	// A handoff to the operator bot works for an operator-profile member.
	var ctx struct {
		Turns    []runtimedb.ButlerTurn `json:"turns"`
		Withheld int                    `json:"withheld"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost,
		"/api/v1/butler/conversations/"+started.ConversationID+"/handoff",
		map[string]any{"from": "family", "to": "admin"}, &ctx,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("handoff = %d", res.StatusCode)
	}

	// Somebody else's conversation answers 404, not 403: a 403 confirms
	// it exists.
	if res := h.do(runtimedb.RoleOperator, http.MethodPost,
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
	var off struct {
		Active      bool `json:"active"`
		PausedTasks int  `json:"pausedTasks"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/members/root-admin/offboard", nil, &off); res.StatusCode != http.StatusOK {
		t.Fatalf("offboard = %d", res.StatusCode)
	}
	if off.Active || off.PausedTasks != 1 {
		t.Fatalf("offboarding = %+v", off)
	}
	task, _ := h.s.DB.GetButlerTask(taskID)
	if task.State != runtimedb.ButlerTaskPausedOffboarded {
		t.Fatalf("task after offboarding = %q", task.State)
	}
	// And nothing more can be started.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/conversations",
		map[string]any{"subject": "again"}, nil,
	); res.StatusCode != http.StatusForbidden {
		t.Fatalf("an offboarded member started a conversation: %d", res.StatusCode)
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
