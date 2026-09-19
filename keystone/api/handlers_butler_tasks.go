package api

// The task timeline, feedback, evaluations and debug capture
// (ADR-W030 A11, work package P5).
//
// The Console could already expand one turn's redacted tool calls. That
// answers "what did it read to say this" and not the questions a failed
// or resumed task raises: what was it trying to do, what did it
// establish before it stopped, what refused it, what is it waiting for,
// and what finally justified calling it done.
//
// This is that timeline, and it is permission-filtered on the way out.
// Every trace and evidence row carries the label and audience P1 made
// mandatory; here is where those get used. Three rules:
//
//   - A reader sees a task if they own it, authored it, or hold admin.
//     Someone else's private work is not listed, not summarised, and
//     not confirmable by asking for its id.
//   - Within a visible task, a row is shown when its audience is shared,
//     or when the reader is the owner or author, or when the reader
//     holds admin. A row whose label the reader cannot hold is withheld
//     and COUNTED, so the timeline says "3 entries are not shown to you"
//     rather than quietly presenting an incomplete story as complete.
//   - Payloads are structural — counts, flags, tool names, versions.
//     The runner never writes source content into a trace row, so the
//     timeline cannot leak one; a test asserts it rather than a comment
//     promising it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// actingMember is who a request is FOR: the acting identity where a
// token carries one, the principal otherwise.
func actingMember(id principal.Identity) string {
	if id.ActingFor != "" {
		return id.ActingFor
	}
	return id.Name
}

// canSeeTask reports whether id may read this task at all.
//
// Shared work is visible to admitted adults — that is what shared means,
// and A2 keeps the household's shared record shared. Private work is
// visible to the person it belongs to and to the administrator
// responsible for the deployment, and to nobody else.
//
// Since slice 7 an audience may also be a set — "adults only" — and
// such a task falls through to the private branch below, so it reaches
// its owner and the administrator and nobody else. That is narrower
// than the audience says and deliberately so: widening it means
// resolving the caller's handle to a person_id and asking the
// resolver, which is WHO MAY READ rather than what the record says,
// and #208 is explicit that this slice changes the vocabulary and not
// the authorization model. Nothing creates a set-audience task today;
// the memory and secret slices that will are where it belongs.
func canSeeTask(task runtimedb.ButlerTask, id principal.Identity) bool {
	if task.Audience == runtimedb.ButlerAudienceShared {
		return true
	}
	return canSeePrivateRows(task, id)
}

// canSeePrivateRows reports whether id may see this task's private
// entries as well as its shared ones.
//
// Note that a SHARED task can still carry private rows — a step that
// read one person's calendar inside work the household asked for — so
// this is a separate question from canSeeTask, not a stricter phrasing
// of it.
func canSeePrivateRows(task runtimedb.ButlerTask, id principal.Identity) bool {
	who := actingMember(id)
	return id.Role == runtimedb.RoleAdmin || task.Owner == who || task.Author == who
}

// listButlerTasks lists the tasks this reader may see, newest first.
func (s *Server) listButlerTasks(r *http.Request, id principal.Identity) (any, int, error) {
	all, err := s.DB.ListButlerTasks(200, "")
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := make([]runtimedb.ButlerTask, 0, len(all))
	for _, t := range all {
		if canSeeTask(t, id) {
			out = append(out, t)
		}
	}
	return map[string]any{"tasks": out}, http.StatusOK, nil
}

// timelineEntry is one row of the rendered timeline.
type timelineEntry struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Step    int64     `json:"step,omitempty"`
	Attempt int64     `json:"attempt,omitempty"`
	Actor   string    `json:"actor,omitempty"`
	// Sources names WHICH sources were used, never what they said.
	Sources         []string       `json:"sources,omitempty"`
	ModelEndpoint   string         `json:"modelEndpoint,omitempty"`
	ManifestVersion string         `json:"manifestVersion,omitempty"`
	PromptVersion   string         `json:"promptVersion,omitempty"`
	PolicyVersion   string         `json:"policyVersion,omitempty"`
	Detail          map[string]any `json:"detail,omitempty"`
	TraceSeq        int64          `json:"traceSeq"`
}

// getButlerTask returns one task with its permission-filtered timeline.
func (s *Server) getButlerTask(r *http.Request, id principal.Identity) (any, int, error) {
	taskID := r.PathValue("id")
	task, err := s.DB.GetButlerTask(taskID)
	if err != nil {
		// A task the reader may not see and a task that does not exist
		// answer identically: otherwise the 403 confirms it exists.
		return nil, http.StatusNotFound, fmt.Errorf("no such task")
	}
	if !canSeeTask(task, id) {
		return nil, http.StatusNotFound, fmt.Errorf("no such task")
	}
	private := canSeePrivateRows(task, id)

	trace, err := s.DB.ListButlerTrace(taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	entries := make([]timelineEntry, 0, len(trace))
	withheld := 0
	for _, e := range trace {
		// Asked as "is it household-wide", not "is it the owner
		// audience": slice 7 made the vocabulary open, so the list of
		// narrow audiences is open too and an unrecognised one must be
		// withheld rather than shown. A screen that listed the narrow
		// ones would disclose every audience a household invented.
		if e.Audience != runtimedb.ButlerAudienceShared && !private {
			withheld++
			continue
		}
		entry := timelineEntry{
			At: e.At, Type: e.Type, Step: e.Step, Attempt: e.Attempt, Actor: e.Actor,
			Sources: e.SourceIDs, ModelEndpoint: e.ModelEndpoint,
			ManifestVersion: e.ManifestVersion, PromptVersion: e.PromptVersion,
			PolicyVersion: e.PolicyVersion, TraceSeq: e.Seq,
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(e.PayloadJSON), &detail); err == nil && len(detail) > 0 {
			entry.Detail = detail
		}
		entries = append(entries, entry)
	}

	steps, err := s.DB.ListButlerTaskSteps(taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	waits, err := s.DB.ListButlerTaskWaits(taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	allEvidence, err := s.DB.ListButlerTaskEvidence(taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	evidence := make([]runtimedb.ButlerTaskEvidence, 0, len(allEvidence))
	for _, ev := range allEvidence {
		if ev.Audience != runtimedb.ButlerAudienceShared && !private {
			withheld++
			continue
		}
		// The payload is not returned at all. Evidence says WHAT was
		// observed and where; the reading itself belongs to its source
		// and is read from there, under the reader's own permissions.
		ev.PayloadJSON = ""
		evidence = append(evidence, ev)
	}
	transitions, err := s.DB.ListButlerTaskTransitions(taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	feedback, err := s.DB.ListButlerFeedback(taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	debugOn, err := s.DB.DebugCaptureActive(taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	return map[string]any{
		"task":        task,
		"timeline":    entries,
		"steps":       steps,
		"waits":       waits,
		"evidence":    evidence,
		"transitions": transitions,
		"feedback":    feedback,
		"withheld":    withheld,
		"debugActive": debugOn,
	}, http.StatusOK, nil
}

// addButlerTaskFeedback records one human label.
func (s *Server) addButlerTaskFeedback(r *http.Request, id principal.Identity) (any, int, error) {
	taskID := r.PathValue("id")
	task, err := s.DB.GetButlerTask(taskID)
	if err != nil || !canSeeTask(task, id) {
		return nil, http.StatusNotFound, fmt.Errorf("no such task")
	}
	var req struct {
		Label    string `json:"label"`
		Note     string `json:"note"`
		TraceSeq int64  `json:"traceSeq"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	who := actingMember(id)
	if err := s.DB.AddButlerFeedback(taskID, runtimedb.ButlerFeedback{
		Author: who, Label: runtimedb.ButlerFeedbackLabel(req.Label),
		Note: req.Note, TraceSeq: req.TraceSeq,
	}); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"recorded": true}, http.StatusOK, nil
}

// exportButlerEvaluation turns one reviewed task into a version-pinned
// regression fixture.
//
// The fixture is BUILT, not copied. It carries the objective, the
// ordered tool calls by name, the KEYS of each call's arguments, the
// outcome and the versions in force — and no argument values, no tool
// results, and no answer text. That is enough to replay the shape of a
// failure and detect the original behaviour, and not enough to leak what
// the household was doing at the time.
//
// The person supplies what SHOULD have happened. A fixture whose
// expectation was written by the model that failed it checks nothing.
func (s *Server) exportButlerEvaluation(r *http.Request, id principal.Identity) (any, int, error) {
	taskID := r.PathValue("id")
	task, err := s.DB.GetButlerTask(taskID)
	if err != nil || !canSeeTask(task, id) {
		return nil, http.StatusNotFound, fmt.Errorf("no such task")
	}
	var req struct {
		Name             string `json:"name"`
		Expected         string `json:"expected"`
		Summary          string `json:"summary"`
		EvaluatorVersion string `json:"evaluatorVersion"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	if strings.TrimSpace(req.Expected) == "" {
		return nil, http.StatusBadRequest,
			fmt.Errorf("expected: say what should have happened — an evaluation with no expectation checks nothing")
	}

	steps, err := s.DB.ListButlerTaskSteps(taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	trace, err := s.DB.ListButlerTrace(taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	type fixtureStep struct {
		Ordinal  int      `json:"ordinal"`
		Kind     string   `json:"kind"`
		Name     string   `json:"name"`
		State    string   `json:"state"`
		ArgNames []string `json:"argNames,omitempty"`
	}
	fixtureSteps := make([]fixtureStep, 0, len(steps))
	for _, st := range steps {
		fixtureSteps = append(fixtureSteps, fixtureStep{
			Ordinal: st.Ordinal, Kind: string(st.Kind), Name: st.Name,
			State: string(st.State), ArgNames: argNames(st.ParamsJSON),
		})
	}
	var prompt, policy, manifest, endpoint string
	for _, e := range trace {
		// The last recorded version of each wins: a run whose manifest
		// changed mid-task is pinned to what it finished under.
		if e.PromptVersion != "" {
			prompt = e.PromptVersion
		}
		if e.PolicyVersion != "" {
			policy = e.PolicyVersion
		}
		if e.ManifestVersion != "" {
			manifest = e.ManifestVersion
		}
		if e.ModelEndpoint != "" {
			endpoint = e.ModelEndpoint
		}
	}
	fixture, err := json.Marshal(map[string]any{
		"objective": task.Objective,
		"surface":   string(task.Surface),
		"profile":   task.Profile,
		"steps":     fixtureSteps,
		"outcome":   string(task.State),
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	who := actingMember(id)
	seq, err := s.DB.SaveButlerEvaluation(runtimedb.ButlerEvaluation{
		Author: who, Name: req.Name, SourceTaskID: taskID,
		Summary: req.Summary, Expected: req.Expected,
		PromptVersion: prompt, PolicyVersion: policy, ManifestVersion: manifest,
		ModelEndpoint: endpoint, EvaluatorVersion: req.EvaluatorVersion,
		FixtureJSON: string(fixture),
	})
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"seq": seq, "name": req.Name}, http.StatusOK, nil
}

// argNames returns the KEYS of a step's recorded arguments, sorted. The
// keys say which shape of call was made; the values are what the
// household was actually doing, and they do not travel into a fixture.
func argNames(paramsJSON string) []string {
	var params map[string]any
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		return nil
	}
	out := make([]string, 0, len(params))
	for k := range params {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// listButlerEvaluations returns the stored regression fixtures.
func (s *Server) listButlerEvaluations(_ *http.Request, _ principal.Identity) (any, int, error) {
	evals, err := s.DB.ListButlerEvaluations(100)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"evaluations": evals}, http.StatusOK, nil
}

// enableButlerDebugCapture turns on bounded extra capture for one task.
func (s *Server) enableButlerDebugCapture(r *http.Request, id principal.Identity) (any, int, error) {
	taskID := r.PathValue("id")
	task, err := s.DB.GetButlerTask(taskID)
	if err != nil || !canSeeTask(task, id) {
		return nil, http.StatusNotFound, fmt.Errorf("no such task")
	}
	var req struct {
		Reason  string `json:"reason"`
		Minutes int    `json:"minutes"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	if req.Minutes <= 0 {
		req.Minutes = 60
	}
	who := actingMember(id)
	seq, err := s.DB.EnableButlerDebugCapture(taskID, who, req.Reason,
		time.Duration(req.Minutes)*time.Minute)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"seq": seq}, http.StatusOK, nil
}

// listButlerDebugCaptures is the "visibly enabled" half of A11: extra
// capture that nobody can see is extra capture nobody turns off.
func (s *Server) listButlerDebugCaptures(_ *http.Request, _ principal.Identity) (any, int, error) {
	captures, err := s.DB.ListActiveDebugCaptures()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"captures": captures}, http.StatusOK, nil
}

// cancelButlerTask stops a task that is not going to finish.
//
// It exists because nothing reaps a wait whose cause has gone. A task
// that stopped on "model-unavailable" waits for a model that answers
// again; when the reason was a bug rather than an outage, the model
// comes back and the task does not, because nothing is watching for
// that. Three of them accumulated on a live deployment in one morning.
//
// Cancelling is not deleting. The task, its steps and its evidence stay
// readable — they are the record of work that was attempted, and a
// household asking later what happened is owed it. What changes is that
// it stops being pending work.
//
// Manage, because it decides that work will not happen.
func (s *Server) cancelButlerTask(r *http.Request, id principal.Identity) (any, int, error) {
	taskID := r.PathValue("id")
	var req struct {
		Reason string `json:"reason"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		// A cancelled task with no reason is a mystery in the audit
		// trail six months later.
		return nil, http.StatusBadRequest, fmt.Errorf("cancelling a task needs a reason")
	}
	if err := s.DB.TransitionButlerTask(taskID, runtimedb.ButlerTaskCancelled, reason, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no task %q", taskID)
		}
		// A finished task cannot be cancelled, and the store says so.
		return nil, http.StatusConflict, err
	}
	return map[string]any{"cancelled": taskID, "reason": reason}, http.StatusOK, nil
}
