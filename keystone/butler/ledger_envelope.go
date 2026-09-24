package butler

import "time"

// The tool step's ledger envelope, exported for the Agent Gateway
// (ADR-W039 Phase 2a).
//
// The runner writes a tool step's result in one versioned shape
// (toolStepResult) and reads it back to serve a verified read on a
// resume. The gateway now writes the same rows on the runner's behalf
// (ADR-W039 §3.4 step 7), and two writers of one envelope must share
// the definition rather than each keep a copy that agrees until the
// day it does not. These three names are that shared definition; the
// runner's own copies stay where they are and delegate nothing, so
// this file changes no behaviour.

// ToolStepResult is the envelope a tool step's result is stored in.
// Fingerprint travels with the result rather than being recomputed
// from the step's params, so a change to the fingerprint scheme cannot
// silently start matching old rows to new questions.
type ToolStepResult = toolStepResult

// CallFingerprint identifies a tool call by name and arguments, so a
// resume can tell "the model asked for this again" from "the model
// asked for something similar". Argument order does not change it.
func CallFingerprint(name string, input map[string]any) string {
	return callFingerprint(name, input)
}

// VerifiedResults reads a task's ledger and returns, per fingerprint,
// the recorded result of every tool step that reached verified within
// ttl of now. A ttl <= 0 reuses nothing.
func VerifiedResults(steps []LedgerStep, now time.Time, ttl time.Duration) map[string]string {
	return verifiedResults(steps, now, ttl)
}
