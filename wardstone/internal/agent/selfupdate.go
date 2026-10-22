package agent

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// selfUpdate implements the full A/B happy path (ADR-W003 W5): verify
// integrity + signature, stage next to the live binary, write the
// update-state.json health-gate marker, promote atomically, and report
// success back to the caller. It deliberately does NOT re-exec itself —
// Keystone's Dispatch is blocked waiting on this op's OpResult, and
// re-exec'ing here would replace the process before that result is ever
// sent. Re-exec happens after the caller (the session send-path in
// agent.go) has successfully sent this SelfUpdateResult back — see the
// comment there for the ordering guarantee. See updatestate.go for the
// confirm/revert half of the health gate.
func (a *Agent) selfUpdate(op *wsv1.SelfUpdate) (*wsv1.OpResult_SelfUpdate, *wsv1.OpError) {
	if len(a.cfg.UpdatePublicKey) != ed25519.PublicKeySize {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "agent has no update public key configured")
	}
	sum := sha256.Sum256(op.GetBinary())
	if hex.EncodeToString(sum[:]) != op.GetSha256Hex() {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_VERIFICATION, "binary sha256 mismatch")
	}
	if !ed25519.Verify(a.cfg.UpdatePublicKey, sum[:], op.GetSignature()) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_VERIFICATION, "binary signature rejected")
	}

	current := a.cfg.BinaryPath
	next := current + "-next"
	prev := current + "-prev"

	if err := os.WriteFile(next, op.GetBinary(), 0o755); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "stage: %v", err)
	}

	// Write the health-gate marker BEFORE promoting (ADR-W003 W5): once the
	// live binary is replaced, a crash on the very next line must still
	// leave evidence for CheckUpdateState to find and revert on the next
	// start. Writing it after promotion would risk exactly that race.
	if err := writeUpdateState(a.cfg.DataDir, op.GetVersion()); err != nil {
		os.Remove(next)
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "%v", err)
	}

	// Keep exactly one rollback generation.
	os.Remove(prev)
	if err := os.Rename(current, prev); err != nil && !os.IsNotExist(err) {
		os.Remove(next)
		os.Remove(a.updateStatePath())
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "demote current: %v", err)
	}
	if err := os.Rename(next, current); err != nil {
		// Try to restore the previous binary before reporting failure.
		os.Rename(prev, current)
		os.Remove(a.updateStatePath())
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "promote: %v", err)
	}
	a.log.Info("self-update staged and promoted", "version", op.GetVersion(), "path", current)
	return &wsv1.OpResult_SelfUpdate{SelfUpdate: &wsv1.SelfUpdateResult{
		StagedPath:   current,
		PreviousPath: prev,
		Promoted:     true,
	}}, nil
}
