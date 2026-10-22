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
	"sync"

	"google.golang.org/protobuf/proto"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
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

// verifyAndDispatch checks the ADR-W003 W1 per-operation signature and the
// per-session replay guard before executing an operation.
//
//   - If pinnedPub is non-nil (a post-W1 identity), the op must verify or
//     the agent returns ERROR_CODE_VERIFICATION and logs a Warn — dispatch
//     is never called, so the operation has no side effect.
//   - If pinnedPub is nil (pre-W1 identity, no key ever pinned at join),
//     operations are accepted unsigned; the once-per-session warning for
//     that state is logged by the caller when the session starts.
//   - A correctly-signed op whose op_id was already processed in this
//     session is refused with ERROR_CODE_FORBIDDEN without dispatching —
//     even a valid signature does not license executing the same op twice
//     (a MITM can replay the identical captured bytes verbatim). guard may
//     be nil (unit tests calling this directly), which disables the check.
//
// The replay check runs AFTER signature verification so only legitimately-
// signed op_ids ever enter the guard's set — an unverified op can't pollute
// it. No config flag disables verification when a key is pinned — see
// ADR-W003 W1.
func (a *Agent) verifyAndDispatch(ctx context.Context, opID string, op *wsv1.Operation, sig []byte, pinnedPub ed25519.PublicKey, guard *replayGuard) *wsv1.OpResult {
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
	if guard != nil && opID != "" && !guard.admit(opID) {
		a.log.Warn("rejecting replayed operation", "op_id", opID)
		return &wsv1.OpResult{
			OpId: opID,
			Result: &wsv1.OpResult_Error{Error: opErr(
				wsv1.ErrorCode_ERROR_CODE_FORBIDDEN, "op_id already processed this session (replay rejected)",
			)},
		}
	}
	return a.dispatch(ctx, opID, op)
}

// maxSeenOpIDs bounds one session's replay set. A gateway dispatches
// operations far below this rate at homelab scale, so in practice the set
// never evicts within a session; the cap is purely a memory backstop against
// a pathologically long-lived session.
const maxSeenOpIDs = 8192

// replayGuard is a bounded, session-scoped set of op_ids already accepted by
// verifyAndDispatch, defending against an in-session replay (ADR-W003 W1): a
// transport MITM can re-inject a validly-signed KeystoneMessage byte-for-byte
// — the signature still verifies, so signature checking alone does not stop
// it. A fresh guard is created per Session (see agent.go's session loop), so
// the set is naturally scoped to one connection's lifetime and discarded when
// the session ends — this both bounds memory and means an op_id from a
// different gateway generation (a new op-<gen>-<seq> namespace after a
// gateway restart) is never falsely rejected.
//
// Within one session the set is additionally capped at maxSeenOpIDs entries
// with FIFO eviction so a very long session cannot grow it without bound.
// Eviction only reopens replay for op_ids older than the last maxSeenOpIDs
// accepted — acceptable given op_ids are monotonic within a gateway
// generation (a replayed capture is re-injected close in time to the
// original) and the homelab scale. Full replay defense that survives a
// reconnect would need a durable, expiry-based nonce carried in the op —
// i.e. a new proto field — which is out of scope for this change.
type replayGuard struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string // insertion order, for FIFO eviction
}

func newReplayGuard() *replayGuard {
	return &replayGuard{seen: make(map[string]struct{})}
}

// admit records opID and returns true if it is new (admitted for dispatch),
// or false if opID was already accepted in this session (a replay to refuse).
func (g *replayGuard) admit(opID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dup := g.seen[opID]; dup {
		return false
	}
	g.seen[opID] = struct{}{}
	g.order = append(g.order, opID)
	if len(g.order) > maxSeenOpIDs {
		evict := g.order[0]
		g.order = g.order[1:]
		delete(g.seen, evict)
	}
	return true
}
