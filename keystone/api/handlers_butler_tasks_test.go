package api

// P5's evidence (ADR-W030): a failed or resumed task is explainable from
// request through delivery without secrets, unauthorized source content
// or private chain-of-thought, and one reviewed failure becomes a
// repeatable evaluation.

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// seedTracedTask writes a task whose trace covers a full run: request,
// model call, tool dispatch, a denial, a wait, and a completion.
func seedTracedTask(t *testing.T, h *harness, owner string, audience runtimedb.ButlerTaskAudience) string {
	t.Helper()
	id, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: owner, Owner: owner,
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: "operator", Audience: audience,
		ObjectiveKind: runtimedb.ObjectiveInformational,
		Objective:     "why is photos unhealthy?", CompletionCriteria: "root cause named",
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	step, err := h.s.DB.AppendButlerTaskStep(id, runtimedb.ButlerTaskStep{
		Kind: runtimedb.ButlerStepTool, Name: "get_health",
		ParamsJSON: `{"node":"n2","container":"immich-server"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := h.s.DB.StartButlerAttempt(step)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.FinishButlerAttempt(attempt.Seq, runtimedb.ButlerAttemptSucceeded, `{"ok":false}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.SetButlerStepState(step, runtimedb.ButlerStepVerified); err != nil {
		t.Fatal(err)
	}
	for _, e := range []runtimedb.ButlerTraceEvent{
		{Type: "slice.started", ModelEndpoint: "ollama/llama3", ManifestVersion: "m7",
			PromptVersion: "p3", PolicyVersion: "pol2"},
		{Type: "tool.dispatched", Step: step, Attempt: attempt.Seq, SourceIDs: []string{"get_health"}},
		{Type: "tool.result", Step: step, Attempt: attempt.Seq, PayloadJSON: `{"bytes":42}`},
		{Type: "denial", PayloadJSON: `{"reason":"tool not granted"}`},
		{Type: "wait.opened", PayloadJSON: `{"reason":"approval"}`},
		{Type: "outcome", PayloadJSON: `{"evidence":2}`},
	} {
		e.PermissionLabel = "operator"
		e.Audience = audience
		e.Redacted = true
		if err := h.s.DB.AppendButlerTraceEvent(id, e); err != nil {
			t.Fatalf("AppendButlerTraceEvent: %v", err)
		}
	}
	if err := h.s.DB.TransitionButlerTask(id, runtimedb.ButlerTaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.CompleteButlerTask(id, []runtimedb.ButlerTaskEvidence{{
		Kind: runtimedb.ButlerEvidenceToolResult, Description: "get_health reported immich-server down",
		SourceID: "get_health", PermissionLabel: "operator", Audience: audience,
		PayloadJSON: `{"secret":"do-not-leak"}`,
	}}, "immich-server is down on n2", "butler"); err != nil {
		t.Fatal(err)
	}
	return id
}

// The timeline explains a run end to end and carries no source content.
func TestTaskTimelineExplainsARunWithoutContent(t *testing.T) {
	h := newHarness(t)
	h.person("root-admin")
	id := seedTracedTask(t, h, "root-admin", runtimedb.ButlerAudiencePrivate)

	var body struct {
		Task     runtimedb.ButlerTask `json:"task"`
		Timeline []struct {
			Type            string         `json:"type"`
			ManifestVersion string         `json:"manifestVersion"`
			Sources         []string       `json:"sources"`
			Detail          map[string]any `json:"detail"`
		} `json:"timeline"`
		Evidence []runtimedb.ButlerTaskEvidence `json:"evidence"`
		Withheld int                            `json:"withheld"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/tasks/"+id, nil, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET task = %d", res.StatusCode)
	}
	seen := map[string]bool{}
	for _, e := range body.Timeline {
		seen[e.Type] = true
	}
	// Request through delivery: what it was offered, what it ran, what
	// refused it, what it waited on, and how it ended.
	for _, want := range []string{"slice.started", "tool.dispatched", "tool.result",
		"denial", "wait.opened", "outcome"} {
		if !seen[want] {
			t.Fatalf("timeline has no %q entry: %v", want, seen)
		}
	}
	if body.Withheld != 0 {
		t.Fatalf("the owner was shown %d withheld entries", body.Withheld)
	}
	if body.Task.State != runtimedb.ButlerTaskCompleted {
		t.Fatalf("task state = %q", body.Task.State)
	}
	// Evidence says what was observed and where — never the reading.
	if len(body.Evidence) != 1 {
		t.Fatalf("evidence = %+v", body.Evidence)
	}
	if body.Evidence[0].PayloadJSON != "" {
		t.Fatalf("evidence returned its payload: %q", body.Evidence[0].PayloadJSON)
	}
	whole := h.rawBody(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/tasks/"+id)
	if strings.Contains(whole, "do-not-leak") {
		t.Fatalf("the timeline leaked evidence payload content:\n%s", whole)
	}
	// And no entry carries a chain-of-thought field, because none is
	// ever requested or stored.
	for _, bad := range []string{"reasoning", "chainOfThought", "thoughts"} {
		if strings.Contains(whole, bad) {
			t.Fatalf("the timeline carries a %q field", bad)
		}
	}
}

// Someone else's private task is not listed, and asking for it by id
// answers exactly as a task that does not exist would.
func TestPrivateTasksAreInvisibleToOthers(t *testing.T) {
	h := newHarness(t)
	h.person("sam")
	id := seedTracedTask(t, h, "sam", runtimedb.ButlerAudiencePrivate)

	var list struct {
		Tasks []runtimedb.ButlerTask `json:"tasks"`
	}
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/butler/tasks", nil, &list)
	for _, task := range list.Tasks {
		if task.ID == id {
			t.Fatal("another member's task was listed to an operator")
		}
	}
	res := h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/butler/tasks/"+id, nil, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("reading another member's task = %d, want 404 (a 403 would confirm it exists)", res.StatusCode)
	}
	// The admin, who is responsible for the deployment, can.
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/tasks/"+id, nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("admin reading a member's task = %d", res.StatusCode)
	}
}

// A reader who can see a shared task but not its private rows is told
// how many are withheld, rather than shown an incomplete story as if it
// were complete.
func TestWithheldRowsAreCountedNotHidden(t *testing.T) {
	h := newHarness(t)
	h.person("sam")
	id, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: "operator",
		Audience:  runtimedb.ButlerAudienceShared,
		Objective: "shared work", CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, aud := range []runtimedb.ButlerTaskAudience{
		runtimedb.ButlerAudienceShared, runtimedb.ButlerAudiencePrivate, runtimedb.ButlerAudiencePrivate,
	} {
		if err := h.s.DB.AppendButlerTraceEvent(id, runtimedb.ButlerTraceEvent{
			Type: "tool.result", PermissionLabel: "operator", Audience: aud, Redacted: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The operator is neither owner nor author, so it sees the shared
	// task but not its private rows.
	var body struct {
		Timeline []map[string]any `json:"timeline"`
		Withheld int              `json:"withheld"`
	}
	res := h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/butler/tasks/"+id, nil, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET shared task = %d", res.StatusCode)
	}
	if len(body.Timeline) != 1 || body.Withheld != 2 {
		t.Fatalf("timeline = %d entries, withheld = %d; want 1 and 2", len(body.Timeline), body.Withheld)
	}
}

// A trace row whose audience the reader does not hold is withheld, even
// when that audience is one nobody has seen before (#208).
//
// The old check asked "is it the private audience", which was the whole
// list of narrow audiences while there were two. Slice 7 made the
// vocabulary open: a household may name its own, and a filter that
// listed the narrow ones would show every one of them to a reader who
// is in none. The question is therefore "is it household-wide", and
// anything else is withheld.
func TestAnAudienceTheFilterDoesNotRecogniseIsWithheld(t *testing.T) {
	h := newHarness(t)
	h.person("sam")
	id, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: "operator",
		Audience:  runtimedb.ButlerAudienceShared,
		Objective: "shared work", CompletionCriteria: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	adults, err := h.s.DB.CreateAudienceForGroups("Adults only", adultsGroupID(t, h))
	if err != nil {
		t.Fatal(err)
	}
	for _, aud := range []runtimedb.ButlerTaskAudience{
		runtimedb.ButlerAudienceShared,
		runtimedb.ButlerTaskAudience(adults.ID),
	} {
		if err := h.s.DB.AppendButlerTraceEvent(id, runtimedb.ButlerTraceEvent{
			Type: "tool.result", PermissionLabel: "operator", Audience: aud, Redacted: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var body struct {
		Timeline []map[string]any `json:"timeline"`
		Withheld int              `json:"withheld"`
	}
	res := h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/butler/tasks/"+id, nil, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET shared task = %d", res.StatusCode)
	}
	if len(body.Timeline) != 1 || body.Withheld != 1 {
		t.Fatalf("timeline = %d entries, withheld = %d; want 1 and 1 — an audience "+
			"the filter does not recognise must be withheld, not shown",
			len(body.Timeline), body.Withheld)
	}
}

// adultsGroupID is the slice-6 Adults group, by seed key.
func adultsGroupID(t *testing.T, h *harness) string {
	t.Helper()
	g, err := h.s.DB.GroupBySeedKey(runtimedb.SeedAdults)
	if err != nil {
		t.Fatalf("GroupBySeedKey(adults): %v", err)
	}
	return g.ID
}

// A reviewed failure becomes a version-pinned evaluation that records
// the shape of what happened and none of its content.
func TestReviewedFailureBecomesAnEvaluation(t *testing.T) {
	h := newHarness(t)
	h.person("root-admin")
	id := seedTracedTask(t, h, "root-admin", runtimedb.ButlerAudiencePrivate)

	// First the human judgement.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+id+"/feedback",
		map[string]any{"label": "stale-read", "note": "it answered from a reading taken an hour earlier"}, nil,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("feedback = %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+id+"/feedback",
		map[string]any{"label": "it felt wrong"}, nil,
	); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a free-text label = %d, want 400", res.StatusCode)
	}

	// Then the evaluation built from it.
	var made struct {
		Name string `json:"name"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+id+"/evaluation",
		map[string]any{
			"name":             "photos-stale-health-read",
			"expected":         "it re-reads immich-server's health rather than answering from the earlier reading",
			"summary":          "answered from a stale reading",
			"evaluatorVersion": "ev1",
		}, &made)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export = %d", res.StatusCode)
	}

	var list struct {
		Evaluations []runtimedb.ButlerEvaluation `json:"evaluations"`
	}
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/evaluations", nil, &list)
	if len(list.Evaluations) != 1 {
		t.Fatalf("evaluations = %+v", list.Evaluations)
	}
	ev := list.Evaluations[0]
	// Version-pinned, so a later run can be compared against this one.
	if ev.ManifestVersion != "m7" || ev.PromptVersion != "p3" || ev.PolicyVersion != "pol2" {
		t.Fatalf("evaluation is not version-pinned: %+v", ev)
	}
	if ev.ModelEndpoint != "ollama/llama3" || ev.EvaluatorVersion != "ev1" {
		t.Fatalf("evaluation = %+v", ev)
	}
	// Sanitized: the shape of the call, not its arguments.
	if !strings.Contains(ev.FixtureJSON, `"get_health"`) {
		t.Fatalf("fixture lost the tool call: %s", ev.FixtureJSON)
	}
	if !strings.Contains(ev.FixtureJSON, `"node"`) || !strings.Contains(ev.FixtureJSON, `"container"`) {
		t.Fatalf("fixture lost the argument shape: %s", ev.FixtureJSON)
	}
	for _, leak := range []string{"n2", "immich-server", "do-not-leak"} {
		if strings.Contains(ev.FixtureJSON, leak) {
			t.Fatalf("fixture carries the argument VALUE %q: %s", leak, ev.FixtureJSON)
		}
	}
	// An expectation is required: a fixture the model writes for itself
	// checks nothing.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+id+"/evaluation",
		map[string]any{"name": "no-expectation", "expected": "  "}, nil,
	); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an evaluation with no expectation = %d, want 400", res.StatusCode)
	}
	// And names are unique, so a report naming one check means one check.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+id+"/evaluation",
		map[string]any{"name": "photos-stale-health-read", "expected": "something else"}, nil,
	); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a duplicate evaluation name = %d, want 400", res.StatusCode)
	}
}

// Debug capture is explicit, scoped, bounded, and visible.
func TestDebugCaptureIsBoundedAndVisible(t *testing.T) {
	h := newHarness(t)
	h.person("root-admin")
	id := seedTracedTask(t, h, "root-admin", runtimedb.ButlerAudiencePrivate)

	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+id+"/debug",
		map[string]any{"reason": "intermittent stale read", "minutes": 30}, nil,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("enable debug = %d", res.StatusCode)
	}
	// No reason means no capture: "why is this on" must have an answer.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+id+"/debug",
		map[string]any{"minutes": 30}, nil,
	); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("debug capture with no reason = %d, want 400", res.StatusCode)
	}
	// There is no "until further notice".
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/tasks/"+id+"/debug",
		map[string]any{"reason": "forever please", "minutes": 60 * 24 * 30}, nil,
	); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unbounded debug window = %d, want 400", res.StatusCode)
	}

	var captures struct {
		Captures []runtimedb.ButlerDebugCapture `json:"captures"`
	}
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/debug", nil, &captures)
	if len(captures.Captures) != 1 {
		t.Fatalf("active captures = %+v", captures.Captures)
	}
	if captures.Captures[0].EnabledBy == "" || captures.Captures[0].Reason == "" {
		t.Fatalf("a capture with no visible owner or reason: %+v", captures.Captures[0])
	}
	if captures.Captures[0].TaskID != id {
		t.Fatalf("capture scope = %q, want the one task", captures.Captures[0].TaskID)
	}
}

// rawBody returns a response body as text, for the leak checks that have
// to look at everything the endpoint returns rather than at the fields a
// struct happens to name.
func (h *harness) rawBody(role runtimedb.Role, method, path string) string {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, nil)
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.tokens[role])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("reading body: %v", err)
	}
	return string(body)
}
