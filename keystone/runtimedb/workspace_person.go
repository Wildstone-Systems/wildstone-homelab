package runtimedb

// Person-facing artifact reads deliberately have no task/model dependency.
// Retirement preserves authorized human reads, but never preserves revoked
// Person or Agent access. Shared scope awaits explicit resource grants.
import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

type PersonWorkspaceEntry struct {
	Artifact WorkspaceArtifact
	Revision WorkspaceRevision
}

// PersonWorkspaceEntries returns a bounded page and a fingerprint of the
// currently authorized snapshot. Paths and revision metadata are disclosed only
// after provenance checks. The caller authenticates member and household;
// neither value comes from an artifact selector or pagination cursor.
func (db *DB) PersonWorkspaceEntries(ctx context.Context, member, agent string, household bool, artifact, revision, after string) (Workspace, []PersonWorkspaceEntry, string, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, nil, "", err
	}
	defer tx.Rollback()
	var w Workspace
	var reader workspaceSourceReader
	reader.person, reader.agent = member, agent
	err = tx.QueryRow(`SELECT w.id,w.person_id,w.agent_id,w.visibility,w.created_at,b.agent_class,b.surface
 FROM agent_workspaces w JOIN butler_members p ON p.person_id=w.person_id JOIN butlers b ON b.seq=w.agent_id
 WHERE p.member=? AND b.name=? AND w.visibility='private'
 AND p.admitted=1 AND p.disabled_at='' AND p.offboarded_at=''
 AND ((? AND b.agent_class='member' AND b.surface='family') OR (NOT ? AND p.role='owner'))
 AND (b.agent_class='member' OR (b.agent_class='owner' AND p.role='owner'))
 AND EXISTS(SELECT 1 FROM butler_access a WHERE a.person_id=p.person_id AND a.butler=b.seq AND a.revoked_at='')`, member, agent, household, household).Scan(&w.ID, &w.PersonID, &w.AgentID, &w.Visibility, &w.CreatedAt, &reader.class, &reader.surface)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrWorkspaceDenied
	}
	if err != nil {
		return Workspace{}, nil, "", err
	}
	query := `SELECT a.id,a.workspace_id,a.path,a.state,a.current_revision,a.trashed_at,a.created_at,
 r.id,r.artifact_id,r.task_id,r.hash,r.size_bytes,r.media_type,r.privacy,r.trusted_only,r.created_at
 FROM workspace_artifacts a JOIN workspace_revisions r ON r.artifact_id=a.id
 JOIN workspace_publications p ON p.revision_id=r.id AND p.state='committed'
 WHERE a.workspace_id=? AND a.state='committed'`
	args := []any{w.ID}
	if artifact == "" {
		query += ` AND r.id=a.current_revision`
	} else {
		query += ` AND a.id=?`
		args = append(args, artifact)
	}
	if revision != "" {
		query += ` AND r.id=?`
		args = append(args, revision)
	}
	query += ` ORDER BY a.id,r.id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return Workspace{}, nil, "", err
	}
	defer rows.Close()
	entries := make([]PersonWorkspaceEntry, 0, 21)
	fingerprint := sha256.New()
	for rows.Next() {
		var e PersonWorkspaceEntry
		a, r := &e.Artifact, &e.Revision
		if err = rows.Scan(&a.ID, &a.WorkspaceID, &a.Path, &a.State, &a.CurrentRevision, &a.TrashedAt, &a.CreatedAt, &r.ID, &r.ArtifactID, &r.TaskID, &r.Hash, &r.SizeBytes, &r.MediaType, &r.Privacy, &r.TrustedOnly, &r.CreatedAt); err != nil {
			return Workspace{}, nil, "", err
		}
		if err = db.checkPersonWorkspaceSources(tx, reader, w.PersonID, "wr:"+r.ID); errors.Is(err, ErrWorkspaceDenied) || errors.Is(err, ErrPrivacy) {
			continue
		}
		if err != nil {
			return Workspace{}, nil, "", err
		}
		encoded, err := json.Marshal(e)
		if err != nil {
			return Workspace{}, nil, "", err
		}
		fingerprint.Write(encoded)
		if a.ID+"\x00"+r.ID > after && len(entries) < 21 {
			entries = append(entries, e)
		}
	}
	if err = rows.Err(); err != nil {
		return Workspace{}, nil, "", err
	}
	if err = rows.Close(); err != nil {
		return Workspace{}, nil, "", err
	}
	if err = tx.Commit(); err != nil {
		return Workspace{}, nil, "", err
	}
	return w, entries, hex.EncodeToString(fingerprint.Sum(nil)), nil
}

func (db *DB) checkPersonWorkspaceSources(q privacyReader, reader workspaceSourceReader, person, object string) error {
	// This is a human trusted-screen read. Admission and the explicit Agent
	// grant were checked above, independently of runtime enabled/retired state.
	// Keep the existing memory audience/scope/derivation rules, without the
	// model boundary's active-Agent requirement or Console Owner exemption.
	clause, args, err := db.memoryContentAuthorityClause("m", person, reader.agent, nil)
	if err != nil {
		return err
	}
	var denied bool
	err = q.QueryRow(privacySources+`SELECT EXISTS(SELECT 1 FROM sources s JOIN butler_memory m ON s.object='m:'||m.seq WHERE NOT COALESCE(`+clause+`,0))`, append([]any{object}, args...)...).Scan(&denied)
	if err != nil {
		return err
	}
	if denied {
		return ErrWorkspaceDenied
	}
	err = q.QueryRow(privacySources+`SELECT EXISTS(
 SELECT 1 FROM sources s JOIN workspace_revisions r ON s.object='wr:'||r.id
 JOIN workspace_artifacts a ON a.id=r.artifact_id JOIN agent_workspaces w ON w.id=a.workspace_id
 JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks origin ON origin.seq=r.task_id
 WHERE w.person_id<>? OR b.name<>? OR w.visibility<>'private'
 OR origin.agent_class<>? OR origin.surface<>? OR b.agent_class<>? OR b.surface<>?
 UNION ALL SELECT 1 FROM sources s JOIN workspace_provenance edge ON s.object='wr:'||edge.revision_id
 WHERE edge.source_revision IS NULL OR edge.audience NOT IN ('private','aud_owner',?)
 )`, object, person, reader.agent, reader.class, reader.surface, reader.class, reader.surface, person).Scan(&denied)
	if err != nil {
		return err
	}
	if denied {
		return ErrWorkspaceDenied
	}
	policy, err := contentPrivacy(q, object)
	if err != nil {
		return err
	}
	return policy.Check(ModelLocal, MemoryDestination{BuiltIn: true})
}
