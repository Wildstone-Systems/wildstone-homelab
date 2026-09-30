package runtimedb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

var ErrWorkspaceContext = errors.New("context_missing: workspace operation needs trusted task provenance")

// WorkspaceAccess is bound by Keystone's authenticated task-handle adapter.
// It is not a wire type; callers cannot fill identity fields from tool args.
// Binding is not admission: every use repeats current authority in a transaction.
type WorkspaceAccess struct {
	db                              *DB
	person, agent, task, connection string
}

func (db *DB) BindWorkspaceAccess(person, agent, task, connection string) *WorkspaceAccess {
	return &WorkspaceAccess{db: db, person: person, agent: agent, task: task, connection: connection}
}

// workspaceTurn requires the full originating privacy turn, not a context-free
// model string. Caller holds its mutex until the metadata transaction finishes.
func (db *DB) workspaceTurn(ctx context.Context, a *WorkspaceAccess) (*PrivacyTurn, error) {
	p, _ := ctx.Value(privacyContextKey{}).(*PrivacyTurn)
	if p == nil || a == nil || a.db != db || p.db != db {
		return nil, ErrWorkspaceContext
	}
	p.mu.Lock()
	seq, err := parseButlerTaskID(a.task)
	if err != nil || p.task == 0 || seq != p.task || p.failure != nil {
		p.mu.Unlock()
		return nil, ErrWorkspaceContext
	}
	return p, nil
}

// workspaceInputs persists in-flight memory inputs only in the same transaction
// as an admitted read/revision. Failed checks roll back both evidence and output.
func workspaceInputs(tx *sql.Tx, p *PrivacyTurn) error {
	var session int64
	if err := tx.QueryRow(`SELECT COALESCE(session,0) FROM butler_tasks WHERE seq=?`, p.task).Scan(&session); err != nil {
		return err
	}
	if session != p.session {
		return ErrWorkspaceContext
	}
	for memory := range p.used {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO task_memory_sources(task,memory) VALUES(?,?)`, p.task, memory); err != nil {
			return err
		}
	}
	return nil
}

// checkWorkspaceSources rechecks inherited audience/scope as well as labels.
// Sharing and external source adapters have separate slices; an unknown source
// kind never becomes an implicitly public source here.
func (db *DB) checkWorkspaceSources(tx *sql.Tx, a *WorkspaceAccess, object string) error {
	var person string
	if err := tx.QueryRow(`SELECT person_id FROM butler_members WHERE member=?`, a.person).Scan(&person); err != nil {
		return ErrWorkspaceDenied
	}
	visible, args, err := db.visibleAudienceClause("m.audience", "m.owner_person_id", person)
	if err != nil {
		return err
	}
	constraints, constraintArgs, err := db.memoryConstraintClause("m", person)
	if err != nil {
		return err
	}
	scope, scopeArgs := memoryScopeClause("m", a.agent, person)
	query := privacySources + `SELECT EXISTS(SELECT 1 FROM sources s JOIN butler_memory m ON s.object='m:'||m.seq
 WHERE NOT COALESCE(m.forgotten_at='' AND ` + visible + ` AND ` + constraints + scope + `,0))`
	values := []any{object}
	values = append(values, args...)
	values = append(values, constraintArgs...)
	values = append(values, scopeArgs...)
	var denied bool
	if err = tx.QueryRow(query, values...).Scan(&denied); err != nil {
		return err
	}
	if denied {
		return ErrWorkspaceDenied
	}
	err = tx.QueryRow(privacySources+`SELECT EXISTS (
 SELECT 1 FROM sources s JOIN workspace_revisions r ON s.object='wr:'||r.id
 JOIN workspace_artifacts artifact ON artifact.id=r.artifact_id JOIN agent_workspaces w ON w.id=artifact.workspace_id
 JOIN butlers b ON b.seq=w.agent_id
 WHERE w.person_id<>? OR b.name<>? OR w.visibility<>'private'
 UNION ALL SELECT 1 FROM sources s JOIN workspace_provenance edge ON s.object='wr:'||edge.revision_id
 WHERE edge.source_revision IS NULL OR edge.audience NOT IN ('private','aud_owner')
 )`, object, person, a.agent).Scan(&denied)
	if err != nil {
		return err
	}
	if denied {
		return ErrWorkspaceDenied
	}
	return nil
}

func (db *DB) workspacePolicy(tx *sql.Tx, a *WorkspaceAccess, p *PrivacyTurn, object string) (PrivacyPolicy, error) {
	if err := db.checkWorkspaceSources(tx, a, object); err != nil {
		return PrivacyPolicy{}, err
	}
	policy, err := contentPrivacy(tx, object)
	if err != nil {
		return PrivacyPolicy{}, err
	}
	return policy, policy.Check(p.model, p.destination)
}

// ReadWorkspaceRevision returns retained committed metadata only after current
// authority and privacy checks. Callers must use this before revealing a path,
// snippet or bytes, and repeat at each page/cache/retry boundary. Durable source
// identity commits before the return; a failed commit discloses nothing.
func (db *DB) ReadWorkspaceRevision(ctx context.Context, a *WorkspaceAccess, workspace, artifact, revision string) (WorkspaceRevision, error) {
	p, err := db.workspaceTurn(ctx, a)
	if err != nil {
		return WorkspaceRevision{}, err
	}
	defer p.mu.Unlock()
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceRevision{}, err
	}
	defer tx.Rollback()
	if err = checkWorkspaceTask(tx, a.person, a.agent, a.task, a.connection, workspace); err != nil {
		return WorkspaceRevision{}, err
	}
	if err = workspaceInputs(tx, p); err != nil {
		return WorkspaceRevision{}, err
	}
	var r WorkspaceRevision
	err = tx.QueryRow(`SELECT r.id,r.artifact_id,r.task_id,r.hash,r.size_bytes,r.media_type,r.privacy,r.trusted_only,r.created_at
 FROM workspace_revisions r JOIN workspace_artifacts artifact ON artifact.id=r.artifact_id
 JOIN workspace_publications publication ON publication.revision_id=r.id AND publication.state='committed'
 WHERE artifact.workspace_id=? AND artifact.id=? AND r.id=? AND artifact.state IN ('committed','trashed')`, workspace, artifact, revision).Scan(&r.ID, &r.ArtifactID, &r.TaskID, &r.Hash, &r.SizeBytes, &r.MediaType, &r.Privacy, &r.TrustedOnly, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceRevision{}, ErrWorkspaceDenied
	}
	if err != nil {
		return WorkspaceRevision{}, err
	}
	if _, err = db.workspacePolicy(tx, a, p, fmt.Sprintf("t:%d", p.task)); err != nil {
		return WorkspaceRevision{}, err
	}
	if _, err = db.workspacePolicy(tx, a, p, "wr:"+revision); err != nil {
		return WorkspaceRevision{}, err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO workspace_task_sources(task,revision) VALUES(?,?)`, p.task, revision); err != nil {
		return WorkspaceRevision{}, err
	}
	if err = tx.Commit(); err != nil {
		return WorkspaceRevision{}, err
	}
	return r, nil
}

// CreateWorkspaceRevisionForTask is the authority/privacy seam for S05's
// publication coordinator. It creates metadata only; it grants no filesystem
// access or quota, and does not publish bytes. The coordinator still owns
// expected-revision, quota and execution fences at prepare/commit.
// Source policy and the base edge are captured atomically with the revision.
func (db *DB) CreateWorkspaceRevisionForTask(ctx context.Context, a *WorkspaceAccess, workspace, base string, r WorkspaceRevision) error {
	p, err := db.workspaceTurn(ctx, a)
	if err != nil {
		return err
	}
	defer p.mu.Unlock()
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkWorkspaceTask(tx, a.person, a.agent, a.task, a.connection, workspace); err != nil {
		return err
	}
	if r.TaskID != 0 && r.TaskID != p.task {
		return ErrWorkspaceContext
	}
	r.TaskID = p.task
	if err = workspaceInputs(tx, p); err != nil {
		return err
	}
	var current sql.NullString
	if err = tx.QueryRow(`SELECT current_revision FROM workspace_artifacts WHERE workspace_id=? AND id=? AND state IN ('pending','committed')`, workspace, r.ArtifactID).Scan(&current); err != nil {
		return ErrWorkspaceDenied
	}
	if current.String != base {
		return errors.New("conflict: revision changed")
	}
	policy, err := db.workspacePolicy(tx, a, p, fmt.Sprintf("t:%d", p.task))
	if err != nil {
		return err
	}
	if base != "" {
		inherited, err := db.workspacePolicy(tx, a, p, "wr:"+base)
		if err != nil {
			return err
		}
		policy.LocalOnly = policy.LocalOnly || inherited.LocalOnly
		policy.TrustedOnly = policy.TrustedOnly || inherited.TrustedOnly
	}
	if r.Privacy == "" {
		r.Privacy = "normal"
	}
	if policy.LocalOnly {
		r.Privacy = "local-only"
	}
	r.TrustedOnly = r.TrustedOnly || policy.TrustedOnly
	_, err = tx.Exec(`INSERT INTO workspace_revisions(id,artifact_id,task_id,hash,size_bytes,media_type,privacy,trusted_only,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, r.ID, r.ArtifactID, r.TaskID, r.Hash, r.SizeBytes, r.MediaType, r.Privacy, r.TrustedOnly, now())
	if err != nil {
		return err
	}
	if base != "" {
		sum := sha256.Sum256([]byte(r.ID))
		_, err = tx.Exec(`INSERT INTO workspace_provenance(id,revision_id,source_revision,privacy,trusted_only,audience,created_at) VALUES(?,?,?,?,?,'private',?)`, fmt.Sprintf("base-%x", sum), r.ID, base, r.Privacy, r.TrustedOnly, now())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
