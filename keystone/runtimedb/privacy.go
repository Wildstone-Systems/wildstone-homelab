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

func (db *DB) contentPrivacy(object string) (PrivacyPolicy, error) {
	var p PrivacyPolicy
	err := db.sql.QueryRow(`WITH RECURSIVE sources(object) AS (
 SELECT ? UNION SELECT e.parent FROM privacy_edges e JOIN sources s ON e.child=s.object
 ) SELECT COALESCE(MAX(p.local_only),0),COALESCE(MAX(p.trusted_only),0)
 FROM sources s LEFT JOIN content_privacy p ON p.object=s.object`, object).Scan(&p.LocalOnly, &p.TrustedOnly)
	return p, err
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
	if p.session != 0 {
		policy, err := p.db.contentPrivacy(fmt.Sprintf("s:%d", p.session))
		if err != nil {
			return err
		}
		if err = policy.Check(p.model, p.destination); err != nil {
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
	if p.session != 0 {
		policy, err := p.db.contentPrivacy(fmt.Sprintf("s:%d", p.session))
		if err != nil {
			return err
		}
		if err = policy.Check(p.model, d); err != nil {
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
	p, err := db.contentPrivacy(fmt.Sprintf("s:%d", s.Seq))
	if err != nil {
		return err
	}
	return p.Check(model, d)
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
