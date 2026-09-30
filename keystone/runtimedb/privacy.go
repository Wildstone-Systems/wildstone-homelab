package runtimedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

type PrivacyPolicy struct {
	LocalOnly   bool `json:"localOnly"`
	TrustedOnly bool `json:"trustedOnly"`
}

var ErrPrivacy = errors.New("privacy restriction: this content needs a local model or a trusted household screen; change the destination or model to continue")

// privacySources includes durable workspace reads in the same graph as memory.
// UNION (not UNION ALL) terminates cycles without dropping restrictions.
const privacySources = `WITH RECURSIVE edges(child,parent) AS (
 SELECT child,parent FROM privacy_edges UNION SELECT child,parent FROM workspace_privacy_edges
), sources(object) AS (SELECT ? UNION SELECT e.parent FROM edges e JOIN sources s ON e.child=s.object) `

type privacyReader interface{ QueryRow(string, ...any) *sql.Row }

func contentPrivacy(q privacyReader, object string) (PrivacyPolicy, error) {
	var p PrivacyPolicy
	err := q.QueryRow(privacySources+`SELECT COALESCE(MAX(p.local_only),0),COALESCE(MAX(p.trusted_only),0)
 FROM sources s LEFT JOIN (SELECT * FROM content_privacy UNION ALL SELECT * FROM workspace_content_privacy) p ON p.object=s.object`, object).Scan(&p.LocalOnly, &p.TrustedOnly)
	return p, err
}
func (db *DB) contentPrivacy(object string) (PrivacyPolicy, error) {
	return contentPrivacy(db.sql, object)
}
func (p PrivacyPolicy) Check(model string, d MemoryDestination) error {
	if p.LocalOnly && model != ModelLocal {
		return ErrPrivacy
	}
	if p.TrustedOnly && (!d.BuiltIn || d.ConnectionRef != 0 || d.Group) {
		return ErrPrivacy
	}
	return nil
}
func (db *DB) MemoryPrivacy(seq int64) (PrivacyPolicy, error) {
	return db.contentPrivacy(fmt.Sprintf("m:%d", seq))
}
func (db *DB) CheckMemoryPrivacy(seq int64, model string, d MemoryDestination) error {
	p, err := db.MemoryPrivacy(seq)
	if err != nil {
		return err
	}
	return p.Check(model, d)
}

// PrivacyPatch updates only explicit flags; inherited effective values must
// never be copied onto a child by changing an unrelated control.
type PrivacyPatch struct {
	LocalOnly   *bool `json:"localOnly,omitempty"`
	TrustedOnly *bool `json:"trustedOnly,omitempty"`
}

func (db *DB) SetMemoryPrivacy(actor string, seq int64, p PrivacyPolicy) error {
	return db.PatchMemoryPrivacy(actor, seq, PrivacyPatch{&p.LocalOnly, &p.TrustedOnly})
}
func (db *DB) PatchMemoryPrivacy(actor string, seq int64, p PrivacyPatch) error {
	if p.LocalOnly == nil && p.TrustedOnly == nil {
		return fmt.Errorf("choose a privacy setting to change")
	}
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	person, err := activeMemoryActor(tx, actor, false)
	if err != nil {
		return err
	}
	var role string
	if err = tx.QueryRow("SELECT role FROM butler_members WHERE person_id=?", person).Scan(&role); err != nil {
		return err
	}
	var scope string
	var recordOwner sql.NullString
	if err = tx.QueryRow("SELECT scope,owner_person_id FROM butler_memory WHERE seq=?", seq).Scan(&scope, &recordOwner); err != nil {
		return ErrNotFound
	}
	if (scope == MemoryScopeHousehold && role != "owner") || (scope != MemoryScopeHousehold && recordOwner.String != person) {
		return ErrNotFound
	}
	_, err = tx.Exec(`INSERT INTO content_privacy(object,local_only,trusted_only) VALUES(?,COALESCE(?,0),COALESCE(?,0))
 ON CONFLICT(object) DO UPDATE SET local_only=COALESCE(?,content_privacy.local_only),trusted_only=COALESCE(?,content_privacy.trusted_only)`, fmt.Sprintf("m:%d", seq), p.LocalOnly, p.TrustedOnly, p.LocalOnly, p.TrustedOnly)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) checkReadPrivacy(seq int64, read *MemoryReadContext) error {
	if read == nil {
		return nil
	}
	var model string
	if err := db.sql.QueryRow("SELECT m.kind FROM butlers b JOIN models m ON m.seq=b.model WHERE b.seq=?", read.agent).Scan(&model); err != nil {
		return err
	}
	return db.CheckMemoryPrivacy(seq, model, read.destination)
}

// PrivacyTurn tracks the inputs used by one turn, including recalled content.
// It is trusted runtime context, never a model argument.
type PrivacyTurn struct {
	db          *DB
	model       string
	destination MemoryDestination
	session     int64
	task        int64
	mu          sync.Mutex
	used        map[int64]bool
	failure     error
}
type privacyContextKey struct{}

func (db *DB) BeginPrivacyTurn(ctx context.Context, model string, d MemoryDestination, session string) (context.Context, *PrivacyTurn, error) {
	p := &PrivacyTurn{db: db, model: model, destination: d, used: map[int64]bool{}}
	if session != "" {
		s, err := db.GetButlerSession(session)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return ctx, nil, err
		}
		p.session = s.Seq
	}
	return context.WithValue(ctx, privacyContextKey{}, p), p, nil
}
func TrackMemoryPrivacy(ctx context.Context, seq int64, err error) error {
	p, _ := ctx.Value(privacyContextKey{}).(*PrivacyTurn)
	if p == nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.failure = err
		return err
	}
	if seq > 0 {
		p.used[seq] = true
	}
	return nil
}
func (p *PrivacyTurn) Check() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.includeTaskSources(); err != nil {
		return err
	}
	// A response may already have been recorded when this check denies it.
	// Keep known provenance even after policy changes or a latched read failure.
	if p.task != 0 {
		for seq := range p.used {
			if _, err := p.db.sql.Exec("INSERT OR IGNORE INTO task_memory_sources(task,memory) VALUES(?,?)", p.task, seq); err != nil {
				return err
			}
		}
	}
	// Persist before inference: a later turn must retain the provenance even
	// if this request is interrupted after the provider receives its inputs.
	if p.session != 0 {
		for seq := range p.used {
			if _, err := p.db.sql.Exec("INSERT OR IGNORE INTO session_memory_sources(session,memory) VALUES(?,?)", p.session, seq); err != nil {
				return err
			}
		}
	}
	if p.failure != nil {
		return p.failure
	}
	if p.task != 0 {
		policy, err := p.db.contentPrivacy(fmt.Sprintf("t:%d", p.task))
		if err != nil {
			return err
		}
		if err = policy.Check(p.model, p.destination); err != nil {
			return err
		}
		if err = p.db.checkWorkspaceHistoryAuthority(fmt.Sprintf("t:%d", p.task), p.task, 0, p.destination); err != nil {
			return err
		}
	}
	if p.session != 0 {
		policy, err := p.db.contentPrivacy(fmt.Sprintf("s:%d", p.session))
		if err != nil {
			return err
		}
		if err = policy.Check(p.model, p.destination); err != nil {
			return err
		}
		if err = p.db.checkWorkspaceHistoryAuthority(fmt.Sprintf("s:%d", p.session), p.task, p.session, p.destination); err != nil {
			return err
		}
	}
	for seq := range p.used {
		if err := p.db.CheckMemoryPrivacy(seq, p.model, p.destination); err != nil {
			return err
		}
	}
	return nil
}

func (p *PrivacyTurn) SetTask(id string) error {
	t, err := p.db.GetButlerTask(id)
	if err != nil {
		return err
	}
	var session int64
	if t.SessionID != "" {
		history, err := p.db.GetButlerSession(t.SessionID)
		if err != nil {
			return err
		}
		session = history.Seq
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.task = t.Seq
	// A follow-up can use protected history without a fresh memory tool call.
	if p.session == 0 {
		p.session = session
	}
	return nil
}

// CheckPrivacyDelivery checks a separate output destination before a tool sends
// model-authored content outside the current conversation.
func CheckPrivacyDelivery(ctx context.Context, d MemoryDestination) error {
	p, _ := ctx.Value(privacyContextKey{}).(*PrivacyTurn)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != nil {
		return p.failure
	}
	if p.task != 0 {
		policy, err := p.db.contentPrivacy(fmt.Sprintf("t:%d", p.task))
		if err != nil {
			return err
		}
		if err = policy.Check(p.model, d); err != nil {
			return err
		}
		if err = p.db.checkWorkspaceHistoryAuthority(fmt.Sprintf("t:%d", p.task), p.task, p.session, d); err != nil {
			return err
		}
	}
	if p.session != 0 {
		policy, err := p.db.contentPrivacy(fmt.Sprintf("s:%d", p.session))
		if err != nil {
			return err
		}
		if err = policy.Check(p.model, d); err != nil {
			return err
		}
		if err = p.db.checkWorkspaceHistoryAuthority(fmt.Sprintf("s:%d", p.session), p.task, p.session, d); err != nil {
			return err
		}
	}
	if err := p.includeTaskSources(); err != nil {
		return err
	}
	for seq := range p.used {
		if err := p.db.CheckMemoryPrivacy(seq, p.model, d); err != nil {
			return err
		}
	}
	return nil
}
func (db *DB) CheckSessionPrivacy(id, model string, d MemoryDestination) error {
	s, err := db.GetButlerSession(id)
	if err != nil {
		return err
	}
	return db.checkHistoryPrivacy(fmt.Sprintf("s:%d", s.Seq), 0, s.Seq, model, d, false)
}

// CheckTaskPrivacy also covers first-turn workspace inputs before a session
// exists. Task-backed delivery must not infer an empty policy from no session.
func (db *DB) CheckTaskPrivacy(id, model string, d MemoryDestination) error {
	t, err := db.GetButlerTask(id)
	if err != nil {
		return err
	}
	return db.checkHistoryPrivacy(fmt.Sprintf("t:%d", t.Seq), t.Seq, 0, model, d, false)
}

// CheckTaskDeliveryPrivacy repeats the live binding gate within the same
// transaction as source authority and privacy. A separate earlier admission
// check cannot authorize a later, incompatible source-policy snapshot.
func (db *DB) CheckTaskDeliveryPrivacy(id, model string, d MemoryDestination) error {
	t, err := db.GetButlerTask(id)
	if err != nil {
		return err
	}
	return db.checkHistoryPrivacy(fmt.Sprintf("t:%d", t.Seq), t.Seq, 0, model, d, true)
}

func (db *DB) checkHistoryPrivacy(object string, task, session int64, model string, d MemoryDestination, delivery bool) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if delivery && d.ConnectionRef != 0 {
		var owner string
		if err = tx.QueryRow(`SELECT p.member FROM butler_tasks t JOIN butler_members p ON p.person_id=t.owner_person_id WHERE t.seq=?`, task).Scan(&owner); err != nil {
			return ErrPrivacy
		}
		// Re-read room discovery inside this transaction. An earlier gateway
		// lookup can become stale before this snapshot starts; source type
		// must not determine whether a known shared destination is refused.
		var room bool
		if err = tx.QueryRow(`SELECT EXISTS (
 SELECT 1 FROM chat_rooms r JOIN butler_addresses a ON a.seq=r.connection
 WHERE a.seq=? AND r.conversation=a.external_id)`, d.ConnectionRef).Scan(&room); err != nil || room || d.Group {
			return ErrPrivacy
		}
		admitted, err := hasLivePersonChatBinding(tx, d.ConnectionRef, owner)
		if err != nil || !admitted {
			return ErrPrivacy
		}
	}

	p, err := contentPrivacy(tx, object)
	if err != nil {
		return err
	}
	if err = p.Check(model, d); err != nil {
		return err
	}
	if err = db.checkWorkspaceHistoryAuthorityIn(tx, object, task, session, d); err != nil {
		return err
	}
	return tx.Commit()
}

// includeTaskSources runs under mu. A newly remembered fact is still in this
// turn's context even before the first session exists or a retrieval reads it.
func (p *PrivacyTurn) includeTaskSources() error {
	if p.task == 0 {
		return nil
	}
	rows, err := p.db.sql.Query("SELECT seq FROM butler_memory WHERE (source_kind IN ('conversation','group-conversation') AND source_id=?) OR seq IN (SELECT memory FROM task_memory_sources WHERE task=?)", fmt.Sprintf("bt-%d", p.task), p.task)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return err
		}
		p.used[seq] = true
	}
	return rows.Err()
}

// SetSession persists first-turn reads once the legacy session has an identity.
func (p *PrivacyTurn) SetSession(id string) error {
	session, err := p.db.GetButlerSession(id)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.session = session.Seq
	p.mu.Unlock()
	return p.Check()
}

// WithChatOutboundPrivacy preserves source identity until the adapter's poll.
func WithChatOutboundPrivacy(ctx context.Context, m ChatOutbound) (ChatOutbound, error) {
	p, _ := ctx.Value(privacyContextKey{}).(*PrivacyTurn)
	if p == nil {
		return m, nil
	}
	if err := CheckPrivacyDelivery(ctx, MemoryDestination{ConnectionRef: m.Connection}); err != nil {
		return m, err
	}
	if err := p.Check(); err != nil {
		return m, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	m.PrivacySession = p.session
	m.PrivacyTask = p.task
	for seq := range p.used {
		m.PrivacyMemories = append(m.PrivacyMemories, seq)
	}
	return m, nil
}
