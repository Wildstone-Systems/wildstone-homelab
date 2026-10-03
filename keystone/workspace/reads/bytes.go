package reads

import (
	"strconv"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/storage"
)

// ReadCommittedBytes verifies confined immutable revision bytes, with no model
// envelope or task requirement. Callers must obtain committed revision/workspace
// metadata through their current authority seam and recheck authority after I/O.
// This helper grants no human, Agent or artifact access by itself.
func ReadCommittedBytes(settings workspace.Settings, w runtimedb.Workspace, r runtimedb.WorkspaceRevision) ([]byte, error) {
	if w.ID == "" || w.PersonID == "" || w.AgentID <= 0 || r.ID == "" || r.ArtifactID == "" || r.TaskID <= 0 {
		return nil, workspace.ErrUnavailable
	}
	store, err := storage.New(settings, w.PersonID, strconv.FormatInt(w.AgentID, 10))
	if err != nil {
		return nil, err
	}
	return store.Read(storage.Object{ID: r.ID, SHA256: r.Hash, Size: r.SizeBytes})
}
