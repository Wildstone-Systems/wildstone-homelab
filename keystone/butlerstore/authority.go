package butlerstore

// The effective-authority resolver, bound to the runtime database
// (ADR-W030 A7/A8, P7).
//
// butler defines the seam and holds no database; this implements it.
// The ordering that makes the guarantees true lives in the store's
// resolver, not here — this is the adapter that lets the Butler ask.

import (
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// Authority implements butler.ToolAuthorizer.
type Authority struct{ DB *runtimedb.DB }

// AuthorizeTool resolves one dispatch.
func (a *Authority) AuthorizeTool(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	res, err := a.DB.ResolveToolAuthority(runtimedb.ToolQuery{
		Profile:       q.Profile,
		Tool:          q.Tool,
		Structural:    q.Structural,
		GrantRequired: q.GrantRequired,
		TaskID:        q.TaskID,
		RoutineName:   q.RoutineName,
	})
	if err != nil {
		return butler.ToolDecision{}, err
	}
	return butler.ToolDecision{
		Allowed:     res.Allowed,
		Reason:      res.Reason,
		Requestable: res.Requestable,
		GrantSeq:    res.GrantSeq,
	}, nil
}

var _ butler.ToolAuthorizer = (*Authority)(nil)
