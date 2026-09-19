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
// first and the CLASS comes from the Butler's record rather than from
// the query — so a session physically cannot ask about a wider ceiling
// than the Butler it belongs to. The Butler's own row id is set there
// too, which is what scopes a grant to this Agent rather than to every
// Agent of its class.
//
// It is a field on the resolver rather than a member of the query for
// the same reason. One Authority is built per session, bound to one
// Butler; there is no call shape in which a running session could name
// a different one, which is a structural answer rather than a check
// somebody has to remember to write.
//
// Empty means no Butler scope — the pre-W031 class-level resolution,
// which is what a deployment with no Butlers still runs. Such a session
// reaches only the class-level grants of the pre-0058 shape, never
// another Agent's.
type Authority struct {
	DB     *runtimedb.DB
	Butler string
}

// AuthorizeTool resolves one dispatch.
func (a *Authority) AuthorizeTool(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	query := runtimedb.ToolQuery{
		AgentClass:    q.AgentClass,
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
