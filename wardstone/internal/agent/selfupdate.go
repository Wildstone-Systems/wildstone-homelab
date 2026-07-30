package agent

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// selfUpdate implements the A/B happy path: verify integrity + signature,
// stage next to the live binary, keep the previous binary as the rollback
// artifact, promote atomically. Re-exec + health-gate + auto-revert are
// the systemd-watchdog half of the design and land in E1.
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
	// Keep exactly one rollback generation.
	os.Remove(prev)
	if err := os.Rename(current, prev); err != nil && !os.IsNotExist(err) {
		os.Remove(next)
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "demote current: %v", err)
	}
	if err := os.Rename(next, current); err != nil {
		// Try to restore the previous binary before reporting failure.
		os.Rename(prev, current)
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "promote: %v", err)
	}
	a.log.Info("self-update staged and promoted", "version", op.GetVersion(), "path", current)
	return &wsv1.OpResult_SelfUpdate{SelfUpdate: &wsv1.SelfUpdateResult{
		StagedPath:   current,
		PreviousPath: prev,
		Promoted:     true,
	}}, nil
}
