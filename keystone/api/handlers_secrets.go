package api

// Secrets endpoints (ADR-W012 W2). Keystone is a pass-through here: a
// value transits the set endpoint once, is forwarded to the node's agent
// over the mTLS plane, and is never persisted, logged, or readable back
// (the protocol has no GetSecret arm; the audit chain records method and
// path, never bodies). Everything Keystone keeps is name-level.
//
// Rotation (ADR-W012 A5) is re-creation: regenerate the value on the
// node, then Stop+Remove every container in the Statefile whose env
// references the secret — the next reconcile pass observes them absent
// and re-creates them, materializing the new value. A restart would NOT
// pick it up (docker env is fixed at create), hence remove. Each
// rotation is recorded as a system/secrets plan record driven straight
// through the state machine (the backup-engine pattern; the reconcile
// loop skips this principal), with the calling admin as acting-for.
//
// SETTING A VALUE DOES THE SAME, and for the same reason. It did not,
// once: `wild secret set` wrote the value to the node and returned
// success while every container that reads it went on holding the
// previous one, because a container's environment is fixed when it is
// created. An operator set the Davis admin password, was told it
// worked, and still could not sign in. A new value nothing reads is not
// a new value — so set propagates exactly as rotate does, and the two
// differ only in where the value comes from.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// secretsPrincipal marks rotation plan records. The reconcile loop skips
// it (see keystone/reconcile/loop): rotations are executed here, not
// Statefile intents.
const secretsPrincipal = "system/secrets"

// apiSecretNameRegexp mirrors the agent's secretNameRegexp (the
// small-duplicated-constant pattern) so a bad name fails fast with a 400
// instead of a round trip.
var apiSecretNameRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (s *Server) secretsPreflight(r *http.Request) (node, name string, status int, err error) {
	if s.Agents == nil {
		return "", "", http.StatusServiceUnavailable, fmt.Errorf("agent plane not wired")
	}
	node = r.PathValue("node")
	if node == "" {
		return "", "", http.StatusBadRequest, fmt.Errorf("node required")
	}
	name = r.PathValue("name") // empty for the list route
	if name != "" && !apiSecretNameRegexp.MatchString(name) {
		return "", "", http.StatusBadRequest, fmt.Errorf("secret name must be alphanumeric-led [A-Za-z0-9._-], at most 64 chars")
	}
	return node, name, 0, nil
}

// opStatus maps an agent OpError to an HTTP status.
func opStatus(oe *wsv1.OpError) int {
	switch oe.GetCode() {
	case wsv1.ErrorCode_ERROR_CODE_INVALID:
		return http.StatusBadRequest
	case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}

func (s *Server) listNodeSecrets(r *http.Request, _ principal.Identity) (any, int, error) {
	node, _, status, err := s.secretsPreflight(r)
	if err != nil {
		return nil, status, err
	}
	res, err := s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_ListSecrets{
		ListSecrets: &wsv1.ListSecrets{},
	}})
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("node %s: %w", node, err)
	}
	if oe := res.GetError(); oe != nil {
		return nil, opStatus(oe), fmt.Errorf("node %s: %s", node, oe.GetMessage())
	}
	type info struct {
		Name    string `json:"name"`
		Version uint32 `json:"version"`
		Created int64  `json:"createdUnixSec,omitempty"`
		Updated int64  `json:"updatedUnixSec,omitempty"`
	}
	out := []info{}
	for _, si := range res.GetListSecrets().GetSecrets() {
		out = append(out, info{Name: si.GetName(), Version: si.GetVersion(),
			Created: si.GetCreatedUnixSec(), Updated: si.GetUpdatedUnixSec()})
	}
	return map[string]any{"node": node, "secrets": out}, http.StatusOK, nil
}

// setSecret stores an operator-supplied value (body {"value": "..."}) or,
// with an empty/absent value, generates one on the node (the value then
// never crosses any wire at all).
func (s *Server) setSecret(r *http.Request, id principal.Identity) (any, int, error) {
	node, name, status, err := s.secretsPreflight(r)
	if err != nil {
		return nil, status, err
	}
	var body struct {
		Value string `json:"value"`
	}
	if r.Body != nil {
		// A malformed body is a client error; an empty one means generate.
		if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil && !errors.Is(derr, io.EOF) {
			return nil, http.StatusBadRequest, fmt.Errorf("body: %v", derr)
		}
	}

	var op *wsv1.Operation
	event := "secret.generated"
	if body.Value != "" {
		op = &wsv1.Operation{Op: &wsv1.Operation_PutSecret{PutSecret: &wsv1.PutSecret{Name: name, Value: body.Value}}}
		event = "secret.set"
	} else {
		op = &wsv1.Operation{Op: &wsv1.Operation_EnsureSecret{EnsureSecret: &wsv1.EnsureSecret{Name: name}}}
	}
	res, err := s.Agents.Apply(r.Context(), node, op)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("node %s: %w", node, err)
	}
	if oe := res.GetError(); oe != nil {
		return nil, opStatus(oe), fmt.Errorf("node %s: %s", node, oe.GetMessage())
	}

	out := map[string]any{"node": node, "name": name}
	existed := false
	switch {
	case res.GetPutSecret() != nil:
		out["version"] = res.GetPutSecret().GetVersion()
	case res.GetEnsureSecret() != nil:
		out["version"] = res.GetEnsureSecret().GetVersion()
		existed = res.GetEnsureSecret().GetExisted()
		out["existed"] = existed
	}
	s.appendEvent(event, node, name, id.Name)

	// A new value nothing reads is not a new value. Docker fixes a
	// container's environment at create time, so a container already
	// holding the OLD value keeps serving it until it is re-created —
	// which is exactly what this file's own header says about rotation
	// ("A restart would NOT pick it up ... hence remove") and what
	// recreateDependents exists to do.
	//
	// Rotation called it and set did not, so `wild secret set` wrote a
	// password into the node's store and reported success while the
	// service went on checking the previous one. The value was right,
	// the report was right, and signing in still failed: the Davis admin
	// page could not be opened with the password an operator had just
	// set for it.
	//
	// Skipped when generating a value that already existed: `set` with
	// no body is the install-time "make sure this exists" call, and a
	// no-op there must not restart a running service.
	if body.Value != "" || !existed {
		if n, err := s.applyToDependents(r, node, name, id.Name, event); err != nil {
			// The value IS set; only the propagation failed. Say both,
			// because "try again" and "restart the container" are
			// different instructions.
			return nil, http.StatusBadGateway, fmt.Errorf(
				"secret/%s was set on %s, but re-creating the containers that read it failed: %w", name, node, err)
		} else if n > 0 {
			out["recreated"] = n
		}
	}
	return out, http.StatusOK, nil
}

// applyToDependents re-creates every container whose env references this
// secret, recorded as a plan for the same reason rotation is: it stops
// and removes running containers, which is a Medium-risk act an operator
// should be able to find afterwards.
func (s *Server) applyToDependents(r *http.Request, node, name, actor, event string) (int, error) {
	dependents, err := s.secretDependents(node, name)
	if err != nil {
		return 0, err
	}
	if len(dependents) == 0 {
		return 0, nil
	}
	summary := fmt.Sprintf("%s secret/%s on %s (%d dependent container(s) re-created)",
		strings.TrimPrefix(event, "secret."), name, node, len(dependents))
	stored, _ := json.Marshal(map[string]any{"summary": summary, "secret": name, "node": node, "dependents": dependents})
	planID, err := s.DB.InsertPlan(secretsPrincipal, actor, summary, "medium", string(stored))
	if err != nil {
		return 0, err
	}
	for _, st := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying} {
		if terr := s.DB.TransitionPlan(planID, st, actor); terr != nil {
			return 0, terr
		}
	}
	if err := s.recreateDependents(r, node, dependents); err != nil {
		if terr := s.DB.TransitionPlan(planID, runtimedb.PlanFailed, actor); terr != nil {
			s.Log.Error("secret plan transition", "plan", planID, "err", terr)
		}
		return 0, err
	}
	if terr := s.DB.TransitionPlan(planID, runtimedb.PlanApplied, actor); terr != nil {
		s.Log.Error("secret plan transition", "plan", planID, "err", terr)
	}
	return len(dependents), nil
}

// rotateSecret implements ADR-W012 A5. The calling admin's explicit
// rotate request is the approval; it is recorded as a system/secrets
// plan record with the caller as acting-for and resolver, so the plan
// list shows who rotated what and how it went.
func (s *Server) rotateSecret(r *http.Request, id principal.Identity) (any, int, error) {
	node, name, status, err := s.secretsPreflight(r)
	if err != nil {
		return nil, status, err
	}

	dependents, err := s.secretDependents(node, name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	summary := fmt.Sprintf("rotate secret/%s on %s (%d dependent container(s) re-created)", name, node, len(dependents))
	stored, _ := json.Marshal(map[string]any{"summary": summary, "secret": name, "node": node, "dependents": dependents})
	planID, err := s.DB.InsertPlan(secretsPrincipal, id.Name, summary, "medium", string(stored))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	for _, st := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying} {
		if err := s.DB.TransitionPlan(planID, st, id.Name); err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}

	version, failure := s.executeRotation(r, node, name, dependents)
	if failure != nil {
		if terr := s.DB.TransitionPlan(planID, runtimedb.PlanFailed, id.Name); terr != nil {
			s.Log.Error("rotation plan transition", "plan", planID, "err", terr)
		}
		s.appendEvent("secret.rotate-failed", node, name, id.Name)
		return nil, http.StatusBadGateway, fmt.Errorf("%s (plan %s)", failure, planID)
	}
	if terr := s.DB.TransitionPlan(planID, runtimedb.PlanApplied, id.Name); terr != nil {
		s.Log.Error("rotation plan transition", "plan", planID, "err", terr)
	}
	s.appendEvent("secret.rotated", node, name, id.Name)
	if s.OnApproved != nil {
		s.OnApproved() // poke the loop: dependents re-create now, not next interval
	}
	return map[string]any{
		"node": node, "name": name, "version": version,
		"dependentsRemoved": dependents, "plan": planID,
	}, http.StatusOK, nil
}

// executeRotation regenerates the secret, then removes each dependent so
// the level-triggered loop re-creates it against the new value.
func (s *Server) executeRotation(r *http.Request, node, name string, dependents []string) (uint32, error) {
	res, err := s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_EnsureSecret{
		EnsureSecret: &wsv1.EnsureSecret{Name: name, Rotate: true},
	}})
	if err != nil {
		return 0, fmt.Errorf("node %s: %w", node, err)
	}
	if oe := res.GetError(); oe != nil {
		return 0, fmt.Errorf("node %s: %s", node, oe.GetMessage())
	}
	version := res.GetEnsureSecret().GetVersion()
	if err := s.recreateDependents(r, node, dependents); err != nil {
		return version, err
	}
	return version, nil
}

// recreateDependents stops and removes each dependent container so the
// level-triggered loop re-creates it against the secret's new value.
// Shared by secret rotation and publisher-token minting.
func (s *Server) recreateDependents(r *http.Request, node string, dependents []string) error {
	for _, c := range dependents {
		// Best-effort stop; the remove's own result is authoritative.
		s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_StopContainer{ //nolint:errcheck
			StopContainer: &wsv1.StopContainer{Name: c},
		}})
		rres, rerr := s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{
			RemoveContainer: &wsv1.RemoveContainer{Name: c, Force: true},
		}})
		if rerr != nil {
			return fmt.Errorf("removing dependent %s: %w", c, rerr)
		}
		if oe := rres.GetError(); oe != nil {
			return fmt.Errorf("removing dependent %s: %s", c, oe.GetMessage())
		}
	}
	return nil
}

// secretDependents lists the Statefile containers on node whose env
// references secret/<name> — sorted-stable via the Statefile's own load
// order.
func (s *Server) secretDependents(node, name string) ([]string, error) {
	desired, err := s.State.Load()
	if err != nil {
		return nil, fmt.Errorf("loading statefile: %w", err)
	}
	ref := "secret/" + name
	deps := []string{}
	for _, res := range desired {
		if res.Kind != resource.KindContainer || res.Metadata.Node != node {
			continue
		}
		for _, v := range res.Container.Env {
			if strings.TrimSpace(v) == ref {
				deps = append(deps, res.Metadata.Name)
				break
			}
		}
	}
	return deps, nil
}

func (s *Server) appendEvent(typ, node, name, who string) {
	if _, err := s.DB.AppendEvent(runtimedb.Event{
		Type: typ, Node: node, Name: name, Message: "secret/" + name,
		Attrs: map[string]string{"by": who},
	}); err != nil {
		s.Log.Error("secret event append failed", "type", typ, "err", err)
	}
}
