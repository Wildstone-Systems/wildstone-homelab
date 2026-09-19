package runtimedb

// Selected-source retrieval, freshness, and compaction (ADR-W030 A3/A6/
// A9.1, P9).
//
// The implementation choice was made by measurement and is recorded in
// butler_index_measurements: SQLite FTS5 with bm25 for the keyword half,
// and an optional caller-supplied embedding for the vector half. Both
// local, no service, no second datastore, no new dependency — the
// pure-Go driver this database already uses ships FTS5.
//
// Three properties this file exists to hold:
//
//   Permission first. The filter is in the SQL that finds candidates,
//   not applied to results afterwards. A match on material the reader
//   may not see must never exist, not even briefly, because "briefly"
//   is how it reaches a model's context.
//
//   Freshness is not relevance. A chunk carries when the underlying
//   thing was OBSERVED, and a source marked volatile — a light, a lock,
//   a queue depth — is never answered from the index at all. It comes
//   back flagged for live verification, because the index knows what was
//   true and the question is what is true.
//
//   Deletion means gone. Removing a source removes its chunks, and the
//   FTS triggers remove the searchable text with them. A revocation that
//   leaves a searchable ghost has not revoked anything.

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Ranking strategies, recorded with each measurement so a comparison
// says what was compared.
const (
	StrategyKeyword = "keyword"
	StrategyVector  = "vector"
	StrategyHybrid  = "hybrid"
)

// Index job kinds and states.
const (
	IndexJobIndex   = "index"
	IndexJobCompact = "compact"
	IndexJobReindex = "reindex"

	IndexJobQueued  = "queued"
	IndexJobRunning = "running"
	IndexJobDone    = "done"
	IndexJobFailed  = "failed"
)

// BackgroundJobPriority is where indexing work starts. Lower is sooner,
// and interactive work can be queued in front without renumbering
// anything (ADR-W030 A9.1: local-first background processing runs at
// lower priority than a person waiting for an answer).
const BackgroundJobPriority = 100

// IndexDocument is one indexable chunk with the permissions of its
// source.
type IndexDocument struct {
	Seq             int64              `json:"seq"`
	SourceKind      string             `json:"sourceKind"`
	SourceID        string             `json:"sourceId"`
	MemorySeq       int64              `json:"memorySeq,omitempty"`
	Owner           string             `json:"owner"`
	Audience        ButlerTaskAudience `json:"audience"`
	PermissionLabel string             `json:"permissionLabel"`
	Content         string             `json:"content"`
	ObservedAt      time.Time          `json:"observedAt,omitzero"`
	IndexedAt       time.Time          `json:"indexedAt,omitzero"`
	// Butler scopes this chunk to one Butler (ADR-W031 A4). Empty is
	// the shared pool every Butler may read.
	Butler string `json:"butler,omitempty"`
	// Volatile marks a source whose current state cannot be answered
	// from an index. See MustVerifyLive on the result.
	Volatile bool `json:"volatile,omitempty"`
	// Embedding is optional. Nil means this deployment ranks by keyword
	// alone, which is a working configuration and not a degraded one.
	Embedding []float32 `json:"-"`
	Tokens    int       `json:"tokens,omitempty"`
}

// IndexDocumentInto stores one chunk, refusing anything without the
// permission metadata retrieval needs.
func (db *DB) IndexDocumentInto(d IndexDocument) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	if d.Content == "" || d.SourceKind == "" || d.SourceID == "" {
		return 0, fmt.Errorf("runtimedb: an indexed chunk needs content and provenance")
	}
	if d.Owner == "" || d.PermissionLabel == "" {
		return 0, fmt.Errorf("runtimedb: an indexed chunk needs an owner and a permission label — retrieval filters on them before it ranks (ADR-W030 A3)")
	}
	// See validateMemory: named only, with the foreign key as backstop.
	if d.Audience == "" {
		return 0, fmt.Errorf("runtimedb: an indexed chunk needs an audience — who it may be disclosed to (design §10 layer 3)")
	}
	var memory any
	if d.MemorySeq != 0 {
		memory = d.MemorySeq
	}
	// The owner mirrors butler_memory's: a person, or the household as
	// NULL.
	owner, err := ownerPersonID(db.sql, d.Owner)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: indexing document: the owner %w", err)
	}
	res, err := db.sql.Exec(
		`INSERT INTO butler_index (source_kind, source_id, memory, owner_person_id, audience,
		     permission_label, content, observed_at, indexed_at, volatile, embedding, tokens, butler)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.SourceKind, d.SourceID, memory, owner, string(d.Audience), d.PermissionLabel,
		d.Content, timeText(d.ObservedAt), now(), boolInt(d.Volatile),
		encodeEmbedding(d.Embedding), d.Tokens, d.Butler)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: indexing document: %w", err)
	}
	return res.LastInsertId()
}

// encodeEmbedding packs a float32 vector little-endian. nil stays NULL,
// which is how "this deployment has no local embedder" is stored rather
// than as a zero vector that would rank against everything equally.
func encodeEmbedding(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func decodeEmbedding(buf []byte) []float32 {
	if len(buf) < 4 {
		return nil
	}
	out := make([]float32, len(buf)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return out
}

// cosine is the vector half's similarity. Vectors of different lengths
// score zero rather than panicking: a deployment that changed embedding
// models has a mixed index, and the right answer there is "this one does
// not help", not a crash.
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// IndexQuery is a retrieval. Reader is mandatory and is applied in SQL:
// there is no way to ask this index a question that is not on someone's
// behalf.
type IndexQuery struct {
	Reader string
	Text   string
	// Embedding turns a keyword search into a hybrid one. Nil is fine.
	Embedding []float32
	Limit     int
	// MaxAge, when set, drops chunks whose observation is older than
	// this — the caller's freshness requirement, which the index cannot
	// guess.
	MaxAge time.Duration
	// Butler is the scope, ANDed onto the reader's permission and never
	// replacing it (ADR-W031 A4). Empty is the shared pool alone, so an
	// omission fails closed; AllButlers is the Console holder's view.
	Butler string
}

// IndexHit is one result.
type IndexHit struct {
	IndexDocument
	// Score is the combined rank: bm25 alone, or bm25 blended with
	// cosine when the query carried an embedding.
	Score float64 `json:"score"`
	// MustVerifyLive is set for a volatile source. The content is what
	// was true when it was indexed, and the caller must read the app
	// before answering with it (ADR-W030 A6).
	MustVerifyLive bool `json:"mustVerifyLive,omitempty"`
	// Age is how old the observation is, so a caller can say "as of an
	// hour ago" rather than implying now.
	Age time.Duration `json:"age,omitempty"`
}

// SearchIndex retrieves chunks this reader may see.
//
// The permission clause is part of the candidate query, not a filter
// over its results. That ordering is the whole guarantee: at no point
// does a row the reader may not see exist in this process on their
// behalf.
//
// The forgotten/superseded exclusion is a BACKSTOP, not the mechanism.
// ForgetMemory and CorrectMemory delete the chunks in their own
// transaction; this clause is what holds if some other writer misses a
// removal, because a correction that lives on as a search hit is the
// same failure as never having corrected it.
func (db *DB) SearchIndex(q IndexQuery) ([]IndexHit, error) {
	if err := db.gate(GateRetrieval); err != nil {
		return nil, err
	}
	if q.Reader == "" {
		return nil, fmt.Errorf("runtimedb: index retrieval needs a reader (ADR-W030 A3)")
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 20
	}
	// Over-fetch when a rerank will follow, so the vector half has
	// something to reorder; without it the keyword rank is the answer.
	fetch := q.Limit
	if len(q.Embedding) > 0 {
		fetch = q.Limit * 4
	}

	scope, scopeArgs := scopeClause("i.butler", q.Butler)
	// An unknown reader owns nothing and matches only shared material,
	// the same as memory retrieval.
	reader, err := personIDForRead(db.sql, q.Reader)
	if err != nil {
		return nil, err
	}
	audClause, audArgs, err := db.visibleAudienceClause("i.audience", "i.owner_person_id", reader)
	if err != nil {
		return nil, err
	}
	query := `SELECT i.seq, i.source_kind, i.source_id, COALESCE(i.memory, 0),
		        COALESCE(io.member, 'household'), i.audience,
		        i.permission_label, i.content, i.observed_at, i.indexed_at, i.volatile,
		        i.embedding, i.tokens, bm25(butler_index_fts)
		 FROM butler_index_fts
		 JOIN butler_index i ON i.seq = butler_index_fts.rowid
		 LEFT JOIN butler_members io ON io.person_id = i.owner_person_id
		 WHERE butler_index_fts MATCH ?
		   AND ` + audClause + `
		   AND NOT EXISTS (
		       SELECT 1 FROM butler_memory m WHERE m.seq = i.memory
		         AND (m.forgotten_at <> '' OR m.superseded_by IS NOT NULL))` +
		scope + `
		 ORDER BY bm25(butler_index_fts)
		 LIMIT ?`
	args := append([]any{ftsQuery(q.Text)}, audArgs...)
	args = append(args, scopeArgs...)
	args = append(args, fetch)
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: searching index: %w", err)
	}
	defer rows.Close()

	var hits []IndexHit
	nowT := time.Now()
	for rows.Next() {
		var h IndexHit
		var audience, observed, indexed string
		var volatile int
		var embedding []byte
		var bm25 float64
		if err := rows.Scan(&h.Seq, &h.SourceKind, &h.SourceID, &h.MemorySeq, &h.Owner,
			&audience, &h.PermissionLabel, &h.Content, &observed, &indexed, &volatile,
			&embedding, &h.Tokens, &bm25); err != nil {
			return nil, err
		}
		h.Audience = ButlerTaskAudience(audience)
		h.Volatile = volatile != 0
		h.MustVerifyLive = h.Volatile
		for _, f := range []struct {
			text string
			into *time.Time
		}{{observed, &h.ObservedAt}, {indexed, &h.IndexedAt}} {
			if f.text == "" {
				continue
			}
			parsed, err := time.Parse(time.RFC3339Nano, f.text)
			if err != nil {
				return nil, fmt.Errorf("runtimedb: bad index timestamp %q: %w", f.text, err)
			}
			*f.into = parsed
		}
		if !h.ObservedAt.IsZero() {
			h.Age = nowT.Sub(h.ObservedAt)
			if q.MaxAge > 0 && h.Age > q.MaxAge {
				continue
			}
		}
		// bm25 returns lower-is-better; invert so every score in this
		// package reads the same way.
		h.Score = -bm25
		if len(q.Embedding) > 0 {
			// Hybrid: keep the keyword rank as the candidate filter and
			// let similarity reorder within it. Blending rather than
			// replacing is what stops a confident-but-wrong embedding
			// burying an exact term match.
			h.Score += 2 * cosine(q.Embedding, decodeEmbedding(embedding))
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return hits, nil
}

// ftsQuery turns a person's words into an FTS5 expression.
//
// Every term is quoted, which is not cosmetic: an apostrophe or a bare
// "AND" in a household question is FTS5 syntax, and a query that fails
// to parse would look to a caller exactly like a question with no
// answer.
func ftsQuery(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		// Match nothing rather than everything. An empty question
		// returning the whole index is the worst possible default for
		// something that feeds a model.
		return `""`
	}
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// RemoveIndexForSource deletes every chunk from one source. The FTS
// triggers remove the searchable text with them, so a revoked source
// leaves no ghost to match on.
func (db *DB) RemoveIndexForSource(sourceKind, sourceID string) (int, error) {
	res, err := db.sql.Exec(
		`DELETE FROM butler_index WHERE source_kind = ? AND source_id = ?`, sourceKind, sourceID)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: removing index for source: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RemoveIndexForMemory deletes the chunks derived from one memory
// record — what forgetting a record must also do.
func (db *DB) RemoveIndexForMemory(memorySeq int64) (int, error) {
	res, err := db.sql.Exec(`DELETE FROM butler_index WHERE memory = ?`, memorySeq)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: removing index for memory: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PruneForgottenFromIndex removes every chunk whose memory record has
// been forgotten or superseded — the sweep that keeps a correction from
// living on as a searchable answer.
func (db *DB) PruneForgottenFromIndex() (int, error) {
	res, err := db.sql.Exec(
		`DELETE FROM butler_index WHERE memory IN (
		     SELECT seq FROM butler_memory WHERE forgotten_at <> '' OR superseded_by IS NOT NULL)`)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: pruning forgotten index entries: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// CompactIndex prunes what should no longer be searchable and rebuilds
// the FTS structures.
//
// It returns what it removed, because compaction that reports nothing is
// compaction nobody can tell ran.
func (db *DB) CompactIndex(maxAge time.Duration) (pruned, aged int, err error) {
	pruned, err = db.PruneForgottenFromIndex()
	if err != nil {
		return 0, 0, err
	}
	if maxAge > 0 {
		// Volatile chunks age out fastest: they were never answers, only
		// hints about where to look.
		cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
		res, err := db.sql.Exec(
			`DELETE FROM butler_index WHERE observed_at <> '' AND observed_at < ?`, cutoff)
		if err != nil {
			return pruned, 0, fmt.Errorf("runtimedb: aging out index entries: %w", err)
		}
		n, _ := res.RowsAffected()
		aged = int(n)
	}
	if _, err := db.sql.Exec(
		`INSERT INTO butler_index_fts (butler_index_fts) VALUES ('optimize')`); err != nil {
		return pruned, aged, fmt.Errorf("runtimedb: optimizing the index: %w", err)
	}
	return pruned, aged, nil
}

// IndexMeasurement is one recorded fixture run: how the chosen strategy
// actually performed, rather than how it was expected to.
type IndexMeasurement struct {
	Seq        int64     `json:"seq"`
	At         time.Time `json:"at"`
	Fixture    string    `json:"fixture"`
	Strategy   string    `json:"strategy"`
	Queries    int       `json:"queries"`
	Hits       int       `json:"hits"`
	RecallPct  int       `json:"recallPct"`
	P95Micros  int       `json:"p95Micros"`
	IndexBytes int       `json:"indexBytes"`
	Note       string    `json:"note,omitempty"`
}

// RecordIndexMeasurement stores a fixture run's numbers.
func (db *DB) RecordIndexMeasurement(m IndexMeasurement) error {
	switch m.Strategy {
	case StrategyKeyword, StrategyVector, StrategyHybrid:
	default:
		return fmt.Errorf("runtimedb: measurement strategy must be keyword|vector|hybrid, got %q", m.Strategy)
	}
	if m.Fixture == "" {
		return fmt.Errorf("runtimedb: a measurement needs the fixture it ran against")
	}
	_, err := db.sql.Exec(
		`INSERT INTO butler_index_measurements (at, fixture, strategy, queries, hits, recall_pct,
		     p95_micros, index_bytes, note)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now(), m.Fixture, m.Strategy, m.Queries, m.Hits, m.RecallPct, m.P95Micros,
		m.IndexBytes, m.Note)
	if err != nil {
		return fmt.Errorf("runtimedb: recording index measurement: %w", err)
	}
	return nil
}

// ListIndexMeasurements returns recorded runs, newest first.
func (db *DB) ListIndexMeasurements() ([]IndexMeasurement, error) {
	rows, err := db.sql.Query(
		`SELECT seq, at, fixture, strategy, queries, hits, recall_pct, p95_micros, index_bytes, note
		 FROM butler_index_measurements ORDER BY seq DESC`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing index measurements: %w", err)
	}
	defer rows.Close()
	var out []IndexMeasurement
	for rows.Next() {
		var m IndexMeasurement
		var at string
		if err := rows.Scan(&m.Seq, &at, &m.Fixture, &m.Strategy, &m.Queries, &m.Hits,
			&m.RecallPct, &m.P95Micros, &m.IndexBytes, &m.Note); err != nil {
			return nil, err
		}
		if m.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// IndexJob is one piece of background retrieval work.
type IndexJob struct {
	Seq       int64     `json:"seq"`
	Kind      string    `json:"kind"`
	Payload   string    `json:"payload,omitempty"`
	State     string    `json:"state"`
	Priority  int       `json:"priority"`
	CreatedAt time.Time `json:"createdAt"`
	Error     string    `json:"error,omitempty"`
}

// QueueIndexJob adds background work. It defaults to background
// priority, so indexing never competes with a person waiting for an
// answer (ADR-W030 A9.1).
func (db *DB) QueueIndexJob(kind, payload string, priority int) (int64, error) {
	switch kind {
	case IndexJobIndex, IndexJobCompact, IndexJobReindex:
	default:
		return 0, fmt.Errorf("runtimedb: index job kind must be index|compact|reindex, got %q", kind)
	}
	if priority <= 0 {
		priority = BackgroundJobPriority
	}
	if payload == "" {
		payload = "{}"
	}
	res, err := db.sql.Exec(
		`INSERT INTO butler_index_jobs (kind, payload, state, priority, created_at)
		 VALUES (?, ?, ?, ?, ?)`, kind, payload, IndexJobQueued, priority, now())
	if err != nil {
		return 0, fmt.Errorf("runtimedb: queuing index job: %w", err)
	}
	return res.LastInsertId()
}

// ClaimIndexJob takes the highest-priority queued job, or ok=false when
// there is nothing to do.
func (db *DB) ClaimIndexJob() (IndexJob, bool, error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return IndexJob{}, false, fmt.Errorf("runtimedb: claiming index job: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var j IndexJob
	var created string
	err = tx.QueryRow(
		`SELECT seq, kind, payload, state, priority, created_at, error FROM butler_index_jobs
		 WHERE state = ? ORDER BY priority, seq LIMIT 1`, IndexJobQueued).
		Scan(&j.Seq, &j.Kind, &j.Payload, &j.State, &j.Priority, &created, &j.Error)
	if err != nil {
		// No rows is the normal idle case, not a failure.
		return IndexJob{}, false, nil
	}
	if _, err := tx.Exec(
		`UPDATE butler_index_jobs SET state = ?, started_at = ? WHERE seq = ?`,
		IndexJobRunning, now(), j.Seq); err != nil {
		return IndexJob{}, false, fmt.Errorf("runtimedb: claiming index job: %w", err)
	}
	if j.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return IndexJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return IndexJob{}, false, fmt.Errorf("runtimedb: claiming index job: %w", err)
	}
	j.State = IndexJobRunning
	return j, true, nil
}

// FinishIndexJob records a job's outcome.
func (db *DB) FinishIndexJob(seq int64, failure string) error {
	state := IndexJobDone
	if failure != "" {
		state = IndexJobFailed
	}
	_, err := db.sql.Exec(
		`UPDATE butler_index_jobs SET state = ?, finished_at = ?, error = ? WHERE seq = ?`,
		state, now(), failure, seq)
	if err != nil {
		return fmt.Errorf("runtimedb: finishing index job: %w", err)
	}
	return nil
}
