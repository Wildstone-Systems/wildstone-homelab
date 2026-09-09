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
//
// Butler names the specialist this session is running as (ADR-W031
// A4/A9). When it is set, the Butler's own narrowings are consulted
// first and the PROFILE comes from the Butler's record rather than from
// the query — so a session physically cannot ask about a wider ceiling
// than the Butler it belongs to.
//
// It is a field on the resolver rather than a member of the query for
// the same reason. One Authority is built per session, bound to one
// Butler; there is no call shape in which a running session could name
// a different one, which is a structural answer rather than a check
// somebody has to remember to write.
//
// Empty means no Butler scope — the pre-W031 profile-level resolution,
// which is what a deployment with no Butlers still runs.
type Authority struct {
	DB     *runtimedb.DB
	Butler string
}

// AuthorizeTool resolves one dispatch.
func (a *Authority) AuthorizeTool(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	query := runtimedb.ToolQuery{
		Profile:       q.Profile,
		Tool:          q.Tool,
		Structural:    q.Structural,
		GrantRequired: q.GrantRequired,
		TaskID:        q.TaskID,
		RoutineName:   q.RoutineName,
	}
	var (
		res runtimedb.ToolAuthority
		err error
	)
	if a.Butler != "" {
		res, err = a.DB.ResolveButlerTool(a.Butler, query)
	} else {
		res, err = a.DB.ResolveToolAuthority(query)
	}
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
