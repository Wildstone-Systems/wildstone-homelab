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
	if err != nil || p.task == 0 || seq != p.task || p.failure != nil || p.destination.Group {
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

// workspaceSourceReader carries trusted reader identity and the original
// class/surface ceiling, independent of whether a task has finished.
type workspaceSourceReader struct {
	person, agent string
	class         string
	surface       ButlerSurface
}

func (db *DB) checkWorkspaceSources(q privacyReader, reader workspaceSourceReader, destination MemoryDestination, object string) error {
	var person string
	if err := q.QueryRow(`SELECT person_id FROM butler_members WHERE member=?`, reader.person).Scan(&person); err != nil {
		return ErrWorkspaceDenied
	}
	var agent int64
	if err := q.QueryRow(`SELECT seq FROM butlers WHERE name=?`, reader.agent).Scan(&agent); err != nil {
		return ErrWorkspaceDenied
	}
	// The destination comes from the bound privacy turn, not the workspace's
	// ingress Connection or model-supplied arguments. Never use nil/internal
	// memory authority for content that is about to reach a model or Person.
	// Workspace authority always requires an explicit Agent grant, including
	// Console Owners; the legacy memory-only Console exemption is not a grant.
	destination.ConsoleOwner = false
	read := &MemoryReadContext{person: person, agent: agent, destination: destination}
	boundary, boundaryArgs := memoryBoundaryClause(person, reader.agent, read)
	var admitted bool
	if err := q.QueryRow(`SELECT 1 WHERE 1`+boundary, boundaryArgs...).Scan(&admitted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrWorkspaceDenied
		}
		return err
	}
	authority, args, err := db.memoryContentAuthorityClause("m", person, reader.agent, read)
	if err != nil {
		return err
	}
	query := privacySources + `SELECT EXISTS(SELECT 1 FROM sources s JOIN butler_memory m ON s.object='m:'||m.seq
 WHERE NOT COALESCE(` + authority + `,0))`
	values := append([]any{object}, args...)
	var denied bool
	if err := q.QueryRow(query, values...).Scan(&denied); err != nil {
		return err
	}
	if denied {
		return ErrWorkspaceDenied
	}
	err = q.QueryRow(privacySources+`SELECT EXISTS (
 SELECT 1 FROM sources s JOIN workspace_revisions r ON s.object='wr:'||r.id
 JOIN workspace_artifacts artifact ON artifact.id=r.artifact_id JOIN agent_workspaces w ON w.id=artifact.workspace_id
 JOIN butlers b ON b.seq=w.agent_id
 JOIN butler_tasks origin ON origin.seq=r.task_id
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
	return nil
}

func (db *DB) workspacePolicy(tx *sql.Tx, a *WorkspaceAccess, p *PrivacyTurn, object string) (PrivacyPolicy, error) {
	var reader workspaceSourceReader
	reader.person, reader.agent = a.person, a.agent
	if err := tx.QueryRow(`SELECT agent_class,surface FROM butler_tasks WHERE seq=?`, p.task).Scan(&reader.class, &reader.surface); err != nil {
		return PrivacyPolicy{}, err
	}
	if err := db.checkWorkspaceSources(tx, reader, p.destination, object); err != nil {
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
	var writable bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM butler_tasks task
 JOIN butlers agent ON agent.name=task.butler JOIN models model ON model.seq=agent.model
 LEFT JOIN butler_sessions session ON session.seq=task.session
 WHERE task.seq=? AND model.enabled=1 AND model.readiness='ready'
 AND (task.session IS NULL OR session.mode='full'))`, p.task).Scan(&writable)
	if err != nil {
		return err
	}
	if !writable {
		return ErrWorkspaceDenied
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
		// Editing consumes the base even without a separate read call. Persist
		// that input before revision creation so task/session continuations and
		// the revision's frozen input snapshot inherit the same restrictions.
		if _, err = tx.Exec(`INSERT OR IGNORE INTO workspace_task_sources(task,revision) VALUES(?,?)`, p.task, base); err != nil {
			return err
		}
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

// checkWorkspaceHistoryAuthority repeats source authority when retained output
// is resumed or sent elsewhere. Memory-only history keeps its existing policy;
// this slice adds no new grants or unattended-work authority.
func (db *DB) checkWorkspaceHistoryAuthority(object string, task, session int64, destination MemoryDestination) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = db.checkWorkspaceHistoryAuthorityIn(tx, object, task, session, destination); err != nil {
		return err
	}
	return tx.Commit()
}

// All lineage, identity, source admission and disclosure reads share a snapshot.
func (db *DB) checkWorkspaceHistoryAuthorityIn(q privacyReader, object string, task, session int64, destination MemoryDestination) error {
	var workspace bool
	if err := q.QueryRow(privacySources+`SELECT EXISTS(SELECT 1 FROM sources s JOIN workspace_revisions r ON s.object='wr:'||r.id)`, object).Scan(&workspace); err != nil {
		return err
	}
	if !workspace {
		return nil
	}
	var reader workspaceSourceReader
	var err error
	if task != 0 {
		err = q.QueryRow(`SELECT p.member,t.butler,t.agent_class,t.surface FROM butler_tasks t JOIN butler_members p ON p.person_id=t.owner_person_id WHERE t.seq=? AND t.kind='conversation' AND t.audience=?`, task, ButlerAudiencePrivate).Scan(&reader.person, &reader.agent, &reader.class, &reader.surface)
	} else if session != 0 {
		err = q.QueryRow(`SELECT p.member,b.name,b.agent_class,s.surface FROM butler_sessions s JOIN butler_members p ON p.person_id=s.person_id JOIN butlers b ON b.seq=s.agent_id WHERE s.seq=?`, session).Scan(&reader.person, &reader.agent, &reader.class, &reader.surface)
	} else {
		return ErrPrivacy
	}
	if err != nil {
		return ErrPrivacy
	}
	if err = db.checkWorkspaceSources(q, reader, destination, object); err != nil {
		return ErrPrivacy
	}
	return nil
}
