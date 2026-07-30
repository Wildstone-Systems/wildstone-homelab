package agent

// Per-operation signature verification (ADR-W003 W1): a MITM on the
// transport, or a compromised load balancer in front of Keystone, must not
// be able to inject operations even inside a valid mTLS tunnel. Once an
// op-signing public key is pinned (see agent.go's loadOpSigningPub), every
// operation must carry a valid signature or the agent refuses it with
// ERROR_CODE_VERIFICATION — without invoking the handler, so a rejected op
// has no side effect.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"

	"google.golang.org/protobuf/proto"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// opSignDomainPrefix domain-separates the per-operation signature. Keep in
// sync with the .proto comment on KeystoneMessage.op_signature and with
// keystone/gateway's copy of this constant.
const opSignDomainPrefix = "wardstone-op-v1"

// opSignPayload builds the digest signed by Keystone and verified here:
// sha256("wardstone-op-v1" || op_id || deterministic-marshal(op)).
func opSignPayload(opID string, op *wsv1.Operation) ([]byte, error) {
	opBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(op)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(opSignDomainPrefix))
	h.Write([]byte(opID))
	h.Write(opBytes)
	return h.Sum(nil), nil
}

// verifyAndDispatch checks the ADR-W003 W1 per-operation signature before
// executing an operation.
//
//   - If pinnedPub is non-nil (a post-W1 identity), the op must verify or
//     the agent returns ERROR_CODE_VERIFICATION and logs a Warn — dispatch
//     is never called, so the operation has no side effect.
//   - If pinnedPub is nil (pre-W1 identity, no key ever pinned at join),
//     operations are accepted unsigned; the once-per-session warning for
//     that state is logged by the caller when the session starts.
//
// No config flag disables verification when a key is pinned — see ADR-W003
// W1.
func (a *Agent) verifyAndDispatch(ctx context.Context, opID string, op *wsv1.Operation, sig []byte, pinnedPub ed25519.PublicKey) *wsv1.OpResult {
	if pinnedPub != nil {
		payload, err := opSignPayload(opID, op)
		if err != nil || !ed25519.Verify(pinnedPub, payload, sig) {
			a.log.Warn("operation signature verification failed", "op_id", opID)
			return &wsv1.OpResult{
				OpId: opID,
				Result: &wsv1.OpResult_Error{Error: opErr(
					wsv1.ErrorCode_ERROR_CODE_VERIFICATION, "operation signature verification failed",
				)},
			}
		}
	}
	return a.dispatch(ctx, opID, op)
}
