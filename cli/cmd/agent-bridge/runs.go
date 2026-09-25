package main

import (
	"sync"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

// runRegistry maps a Hermes run id to the Wildstone handle that started
// it, for the MCP server's tools/list and tools/call to recover which
// task a call belongs to (mcp.go's handleFor).
//
// It exists because of a real limitation, not a choice: ADR-W040 §6
// recommends the task handle travel as "an explicit MCP argument", and
// its fallback is a static per-server identity_header. Hermes's own MCP
// client config (docs/design/hermes-capability-package-and-execution-
// profiles.md §6; the hermes.md research brief §2) makes identity_header
// a value resolved once, from the environment, when Hermes connects to
// the server — not a per-call or per-run value a long-lived connection
// could vary. One Wildstone MCP server therefore cannot be told, through
// any documented Hermes config key, which of several concurrent or
// sequential runs a given tools/call belongs to.
//
// The chosen fallback (H3 brief, design doc §6 "open point"): the bridge
// remembers the handle it used to start each Hermes run, keyed by the
// run id Hermes's own POST /v1/runs returned, and expects Hermes to name
// that run on its own MCP calls via X-Hermes-Run-Id. Whether Hermes
// attaches such a header to its outbound MCP requests today is
// undocumented upstream — TODO(spike S3) confirms or corrects this
// against a real release. Until then, mcp.go also accepts an explicit
// X-Wildstone-Handle header (the ADR's own preferred shape) from any
// runtime that can be configured to send one directly, and prefers it
// over the run-id lookup when both are present.
type runRegistry struct {
	mu      sync.RWMutex
	handles map[string]agentapi.Handle
}

func newRunRegistry() *runRegistry {
	return &runRegistry{handles: map[string]agentapi.Handle{}}
}

// set records which handle started runID. Called once, right after
// hermesClient.StartRun returns the run id.
func (r *runRegistry) set(runID string, h agentapi.Handle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handles[runID] = h
}

// get looks up the handle for runID.
func (r *runRegistry) get(runID string) (agentapi.Handle, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handles[runID]
	return h, ok
}

// delete forgets runID once its run has ended, so the map does not grow
// without bound over the bridge's lifetime.
func (r *runRegistry) delete(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.handles, runID)
}

// solePending reports the one handle currently in flight, when there is
// exactly one. mcp.go falls back to it as a last resort: the delivery
// loop processes one delivery at a time (delivery.go's own doc comment),
// so in the packaged shape — one Hermes container per Agent, one run at
// a time (the managed config's own max_concurrent_runs: 1) — there is
// structurally never more than one live run to be ambiguous about. It
// answers false rather than guessing whenever that invariant does not
// hold (zero runs, or more than one), so a deployment that raises
// max_concurrent_runs loses this fallback rather than silently
// misattributing a call.
func (r *runRegistry) solePending() (agentapi.Handle, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.handles) != 1 {
		return "", false
	}
	for _, h := range r.handles {
		return h, true
	}
	return "", false // unreachable
}
