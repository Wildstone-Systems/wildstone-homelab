package agentgateway

// MemoryReadContextForTask (ADR-W039 §3.1 principle 4, Phase 4, C11):
// an executor that needs a secret-use context — today, exactly one,
// cmd/keystone/calendar_secret.go's calendar.get_agenda binding — used
// to get one pre-computed and stuffed into ctx by whoever built the
// household surface's closure (cmd/keystone/main.go's own Gateways
// wiring), from the Turn that closure happened to be built with. That
// meant the SAME fact this whole family exists to fix elsewhere: a
// call carrying authority nobody derived from what it is actually
// running under.
//
// This is the fix, gateway-side rather than executor-side, because
// deriving it needs the runtimedb and butlerlive.Turn.MemoryDestination
// shape neither keystone/cmd nor a domestic executor should have to
// reconstruct by hand: given only the task id an operation.Identity
// already carries, read the task's own admission back and rebuild
// exactly the MemoryReadContext its own turn would have bound —
// person, agent and destination alike.
import (
	"fmt"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// MemoryReadContextForTask resolves the memory read context — and so
// the secret-use context, which is the same binding — for the task
// named by taskID, from the task's own admission alone.
func MemoryReadContextForTask(db *runtimedb.DB, taskID string) (*runtimedb.MemoryReadContext, error) {
	if taskID == "" {
		return nil, fmt.Errorf("agentgateway: resolving a secret-use context needs a task id")
	}
	task, err := db.GetButlerTask(taskID)
	if err != nil {
		return nil, err
	}
	agentName, err := db.TaskButler(taskID)
	if err != nil {
		return nil, err
	}
	return db.BindMemoryRead(task.Owner, agentName, memoryDestinationForTask(task)), nil
}

// memoryDestinationForTask reconstructs the trusted destination consistently
// for task-bound reads and queued delivery; no delivery payload supplies it.
func memoryDestinationForTask(task runtimedb.ButlerTask) runtimedb.MemoryDestination {
	turn := butlerlive.Turn{
		Member:     task.Owner,
		Group:      task.Audience == runtimedb.ButlerAudienceShared,
		AsOwner:    task.AsOwner,
		Connection: butlerlive.ConnectionScope{Name: task.OriginConnection, Ref: task.OriginConnectionRef, Conversation: task.OriginConversation},
	}
	return turn.MemoryDestination()
}
