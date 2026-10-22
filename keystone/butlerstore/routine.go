package butlerstore

import (
	"strconv"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// RoutineResourceRules supplies §10's resource layer for exact, named
// resources. A missing vault or destination fails closed. Tool arguments
// cannot choose the routine Plan ID; the executor binds it on the query.
func RoutineResourceRules(db *runtimedb.DB, vault *runtimedb.SecretVault, destination runtimedb.MemoryDestination) butler.ResourceRules {
	return butler.ResourceRules{
		"secret": func(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
			if db == nil || vault == nil || q.Resource.ID == "" {
				return butler.ToolDecision{}, nil
			}
			p, err := db.BindRoutinePrincipal(q.RoutinePlanID)
			if err != nil || vault.CheckRoutineUse(q.Resource.ID, p, q.Tool) != nil {
				return butler.ToolDecision{}, nil
			}
			return butler.ToolDecision{Allowed: true}, nil
		},
		"memory": func(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
			if db == nil || q.Resource.ID == "" {
				return butler.ToolDecision{}, nil
			}
			id, err := strconv.ParseInt(q.Resource.ID, 10, 64)
			if err != nil {
				return butler.ToolDecision{}, nil
			}
			p, err := db.BindRoutinePrincipal(q.RoutinePlanID)
			if err != nil || db.CheckRoutineMemory(p, id, destination) != nil {
				return butler.ToolDecision{}, nil
			}
			return butler.ToolDecision{Allowed: true}, nil
		},
	}
}
