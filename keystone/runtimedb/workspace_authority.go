package runtimedb

import "errors"

// ErrWorkspaceDenied deliberately does not distinguish missing from hidden
// resources. Neither resource selectors nor Owner administration grant access.
var ErrWorkspaceDenied = errors.New("permission_denied: workspace access refused")

// CheckWorkspaceTask is the private workspace resource rule (ADR-W047).
// Its identity arguments come from the authenticated task handle, never tool
// arguments. Recheck at each read, page, retry and execution fence. This is
// resource authority only; tool assignments and disclosure are separate gates.
// Shared resources remain refused until explicit sharing grants are implemented.
func (db *DB) CheckWorkspaceTask(person, agent, taskID, connection, workspaceID string) error {
	if db == nil || person == "" || agent == "" || taskID == "" || connection == "" || workspaceID == "" {
		return ErrWorkspaceDenied
	}
	seq, err := parseButlerTaskID(taskID)
	if err != nil {
		return ErrWorkspaceDenied
	}
	var allowed bool
	err = db.sql.QueryRow(`SELECT EXISTS (
 SELECT 1 FROM agent_workspaces w
 JOIN butler_members person ON person.person_id=w.person_id
 JOIN butlers agent ON agent.seq=w.agent_id
 JOIN butler_tasks task ON task.seq=? AND task.owner_person_id=person.person_id AND task.butler=agent.name
 WHERE w.id=? AND w.visibility='private' AND person.member=? AND agent.name=?
 AND person.admitted=1 AND person.disabled_at='' AND person.offboarded_at=''
 AND agent.enabled=1 AND agent.retired_at=''
 AND task.kind='conversation' AND task.state IN ('pending','running','waiting')
 AND task.audience=? AND task.origin_connection=?
 AND task.agent_class=agent.agent_class AND task.surface=agent.surface
 AND (agent.agent_class='member' OR (agent.agent_class='owner' AND person.role='owner'))
 AND EXISTS (SELECT 1 FROM butler_access access WHERE access.butler=agent.seq
             AND access.person_id=person.person_id AND access.revoked_at='')
 AND ((task.origin_connection_ref=0 AND
       ((task.origin_connection='console' AND person.role='owner') OR
        (task.origin_connection='household_portal' AND agent.agent_class='member' AND task.surface='family')))
 OR EXISTS (SELECT 1 FROM butler_addresses address
    JOIN connection_admission admission ON admission.connection=address.seq
    JOIN chat_bindings binding ON binding.connection=address.seq AND binding.person_id=person.person_id
    WHERE address.seq=task.origin_connection_ref AND address.butler=agent.seq
    AND address.revoked_at='' AND binding.revoked_at=''
    AND address.transport||':'||address.external_id=task.origin_connection
    AND admission.subject_kind='person'
    AND (admission.admits='household' OR (admission.admits='owners' AND person.role='owner'))))
 )`, seq, workspaceID, person, agent, ButlerAudiencePrivate, connection).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrWorkspaceDenied
	}
	return nil
}
