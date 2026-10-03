package runtimedb

import (
	"context"
	"errors"
	"fmt"
)

var ErrResearchContext = errors.New("context_missing: research needs trusted task provenance")
var ErrResearchPrivacy = errors.New("privacy_denied: protected sources cannot reach public research")

// CheckResearchEgress authorizes public upstream disclosure, independently of
// model location and the configured worker's network address. Call immediately
// before every search/fetch adapter invocation, including retries and resumes.
// Query redaction and model-supplied privacy claims cannot alter this graph.
func CheckResearchEgress(ctx context.Context) error {
	p, _ := ctx.Value(privacyContextKey{}).(*PrivacyTurn)
	if p == nil {
		return ErrResearchContext
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.task == 0 || p.db == nil {
		return ErrResearchContext
	}
	if p.failure != nil || ctx.Err() != nil {
		return ErrResearchPrivacy
	}
	if err := p.includeTaskSources(); err != nil {
		return ErrResearchPrivacy
	}
	tx, err := p.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return ErrResearchPrivacy
	}
	defer tx.Rollback()
	// Reject a stale turn rather than silently dropping a newly linked session.
	if err = workspaceInputs(tx, p); err != nil {
		return ErrResearchPrivacy
	}
	if p.session != 0 {
		for memory := range p.used {
			if _, err = tx.Exec(`INSERT OR IGNORE INTO session_memory_sources(session,memory) VALUES(?,?)`, p.session, memory); err != nil {
				return ErrResearchPrivacy
			}
		}
	}
	deny := func() error {
		// Retain inputs even when disclosure is refused. A subsequent turn
		// must not forget a protected read merely because this call failed.
		_ = tx.Commit()
		return ErrResearchPrivacy
	}
	var reader workspaceSourceReader
	if err = tx.QueryRow(`SELECT person.member,task.butler,task.agent_class,task.surface
 FROM butler_tasks task JOIN butler_members person ON person.person_id=task.owner_person_id
 JOIN butlers agent ON agent.name=task.butler
 WHERE task.seq=? AND task.kind='conversation'
 AND agent.agent_class=task.agent_class AND agent.surface=task.surface`, p.task).Scan(&reader.person, &reader.agent, &reader.class, &reader.surface); err != nil {
		return deny()
	}
	objects := []string{fmt.Sprintf("t:%d", p.task)}
	if p.session != 0 {
		objects = append(objects, fmt.Sprintf("s:%d", p.session))
	}
	for _, object := range objects {
		policy, err := contentPrivacy(tx, object)
		if err != nil || policy.LocalOnly || policy.TrustedOnly {
			return deny()
		}
		// Current source permissions and class/surface ceilings apply even to
		// unrestricted memory and artifacts retained in earlier task history.
		if err = p.db.checkWorkspaceSources(tx, reader, p.destination, object); err != nil {
			return deny()
		}
	}
	// Commit provenance before dispatch; a failed commit discloses nothing.
	if err = tx.Commit(); err != nil {
		return ErrResearchPrivacy
	}
	return nil
}
