package runtimedb

// Owned memory records (ADR-W030 A3/A3.1, P8).
//
// What the Butler remembers is owned data with provenance, not a blob it
// consults. Four properties hold, and each is enforced here rather than
// described:
//
//   Poisoned input cannot create authority. There is no kind, column or
//   function in this file through which a memory record could grant
//   anything. A hostile calendar entry can make the Butler believe
//   something false; it cannot make the Butler ALLOWED to do something,
//   because the authority resolver (butlertools.go) does not read this
//   table and has no reason to.
//
//   Private material cannot become shared through a derivative. Derive
//   takes the STRICTEST audience of its sources, and the schema refuses
//   an update that loosens one afterwards.
//
//   A correction is a new record, not an edit. The old one is
//   superseded, so "what did it think last week, and why" stays
//   answerable.
//
//   Forgetting sticks. Marking a record forgotten stops retrieval;
//   suppressing its source stops the next extraction pass learning it
//   again from the same place.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Memory kinds. Closed, and deliberately containing nothing that could
// be mistaken for a permission.
const (
	MemoryFact       = "fact"
	MemoryPreference = "preference"
	MemoryInference  = "inference"
	MemoryProcedure  = "procedure"
)

// Confidence separates a stated fact from a guess. An inference
// presented with the confidence of a fact is how a household ends up
// arguing with its own house.
const (
	ConfidenceStated    = "stated"
	ConfidenceInferred  = "inferred"
	ConfidenceUncertain = "uncertain"
)

// Digest change kinds.
const (
	MemoryRecorded  = "recorded"
	MemoryCorrected = "corrected"
	MemoryForgotten = "forgotten"
)

// ErrSourceSuppressed reports that this content was forgotten from this
// source and must not be learned from it again.
var ErrSourceSuppressed = errors.New("runtimedb: this was forgotten from this source and will not be relearned from it")

// MemoryRecord is one thing the Butler remembers.
type MemoryRecord struct {
	Seq             int64              `json:"seq"`
	Kind            string             `json:"kind"`
	Subject         string             `json:"subject,omitempty"`
	Content         string             `json:"content"`
	Audience        ButlerTaskAudience `json:"audience"`
	PermissionLabel string             `json:"permissionLabel"`
	Owner           string             `json:"owner"`
	SourceKind      string             `json:"sourceKind"`
	SourceID        string             `json:"sourceId"`
	ObservedAt      time.Time          `json:"observedAt,omitzero"`
	Confidence      string             `json:"confidence"`
	SafetyRelevant  bool               `json:"safetyRelevant,omitempty"`
	CreatedAt       time.Time          `json:"createdAt"`
	Author          string             `json:"author"`
	SupersededBy    int64              `json:"supersededBy,omitempty"`
	ForgottenAt     time.Time          `json:"forgottenAt,omitzero"`
	ForgottenBy     string             `json:"forgottenBy,omitempty"`
	EnvelopeVersion int                `json:"envelopeVersion"`
}

// Eligible reports whether this record may be retrieved: not corrected
// away, not forgotten.
func (m MemoryRecord) Eligible() bool { return m.SupersededBy == 0 && m.ForgottenAt.IsZero() }

// contentKey normalizes content for source suppression. Case and
// surrounding whitespace should not let the same claim back in through
// the same door.
func contentKey(content string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(content), " "))))
	return hex.EncodeToString(sum[:8])
}

func validateMemory(m MemoryRecord) error {
	switch m.Kind {
	case MemoryFact, MemoryPreference, MemoryInference, MemoryProcedure:
	default:
		return fmt.Errorf("runtimedb: memory kind must be fact|preference|inference|procedure, got %q", m.Kind)
	}
	switch m.Confidence {
	case "", ConfidenceStated, ConfidenceInferred, ConfidenceUncertain:
	default:
		return fmt.Errorf("runtimedb: memory confidence must be stated|inferred|uncertain, got %q", m.Confidence)
	}
	if m.Content == "" {
		return fmt.Errorf("runtimedb: a memory record needs content")
	}
	if m.Owner == "" {
		return fmt.Errorf("runtimedb: a memory record needs an owner — someone who may correct and forget it (ADR-W030 A3)")
	}
	if m.PermissionLabel == "" {
		return fmt.Errorf("runtimedb: a memory record needs a permission label (ADR-W030 A11)")
	}
	if m.Audience != ButlerAudiencePrivate && m.Audience != ButlerAudienceShared {
		return fmt.Errorf("runtimedb: a memory record needs audience private|shared, got %q (ADR-W030 A2)", m.Audience)
	}
	if m.SourceKind == "" || m.SourceID == "" {
		return fmt.Errorf("runtimedb: a memory record needs provenance: which kind of source, and which one (ADR-W030 A3)")
	}
	if m.Author == "" {
		return fmt.Errorf("runtimedb: a memory record needs an author")
	}
	return nil
}

// RecordMemory stores one record, refusing anything whose source was
// suppressed for this content.
//
// Content reaching here has already been through the gateway's redaction
// boundary — extraction runs over post-Dispatch material, exactly as the
// session transcript does — so this function's job is provenance and
// permission, not sanitization.
func (db *DB) RecordMemory(m MemoryRecord) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	if err := validateMemory(m); err != nil {
		return 0, err
	}
	if m.Confidence == "" {
		m.Confidence = ConfidenceStated
	}
	var suppressed int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM butler_memory_suppressed_sources
		 WHERE source_kind = ? AND source_id = ? AND content_key = ?`,
		m.SourceKind, m.SourceID, contentKey(m.Content)).Scan(&suppressed); err != nil {
		return 0, fmt.Errorf("runtimedb: checking suppressed sources: %w", err)
	}
	if suppressed > 0 {
		return 0, ErrSourceSuppressed
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: recording memory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seq, err := insertMemoryTx(tx, m)
	if err != nil {
		return 0, err
	}
	// Shared memory changes are other adults' business; private ones are
	// not, and a digest that leaked them would make correcting a private
	// record a disclosure (A1).
	if m.Audience == ButlerAudienceShared {
		if err := digestTx(tx, seq, MemoryRecorded,
			fmt.Sprintf("learned: %s", m.Content), m.Author, false); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: recording memory: %w", err)
	}
	return seq, nil
}

func insertMemoryTx(tx *sql.Tx, m MemoryRecord) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO butler_memory (kind, subject, content, audience, permission_label, owner,
		     source_kind, source_id, observed_at, confidence, safety_relevant, created_at, author,
		     envelope_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Kind, m.Subject, m.Content, string(m.Audience), m.PermissionLabel, m.Owner,
		m.SourceKind, m.SourceID, timeText(m.ObservedAt), m.Confidence,
		boolInt(m.SafetyRelevant), now(), m.Author, TaskEnvelopeVersion)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: inserting memory: %w", err)
	}
	return res.LastInsertId()
}

// digestTx records a change to SHARED memory for the next digest.
func digestTx(tx *sql.Tx, memory int64, change, summary, actor string, immediate bool) error {
	if _, err := tx.Exec(
		`INSERT INTO butler_memory_digest (memory, change, summary, actor, at, immediate)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		memory, change, summary, actor, now(), boolInt(immediate)); err != nil {
		return fmt.Errorf("runtimedb: recording memory digest entry: %w", err)
	}
	return nil
}

// DeriveMemory records a record built FROM other records — a summary, a
// learned procedure, a rolled-up preference.
//
// Its audience is the strictest of its sources, always, and the caller
// cannot ask for otherwise. This is the rule that stops private material
// becoming shared by being summarised: a derivative of anything private
// is private, however much shared material it also drew on.
func (db *DB) DeriveMemory(m MemoryRecord, sources []int64) (int64, error) {
	if err := db.gate(GateIngestion); err != nil {
		return 0, err
	}
	if len(sources) == 0 {
		return 0, fmt.Errorf("runtimedb: a derivative needs the records it was derived from")
	}
	strictest := ButlerAudienceShared
	label := m.PermissionLabel
	owner := ""
	for _, src := range sources {
		s, err := db.GetMemory(src)
		if err != nil {
			return 0, fmt.Errorf("runtimedb: deriving from memory %d: %w", src, err)
		}
		if s.Audience != ButlerAudiencePrivate {
			continue
		}
		strictest = ButlerAudiencePrivate
		// The OWNER travels with the audience, and this is the half that
		// actually closes the leak. "Private, owned by the household" is
		// not private at all: retrieval treats an owner match as
		// permission, so every adult would read it. A derivative of
		// someone's private material belongs to that person.
		if owner != "" && owner != s.Owner {
			// Two people's private material in one summary has no
			// correct owner and therefore no correct audience. Refusing
			// is the only answer that does not leak one of them to the
			// other.
			return 0, fmt.Errorf(
				"runtimedb: refusing to derive one record from the private material of both %s and %s — it would have no correct audience (ADR-W030 A2/A3)",
				owner, s.Owner)
		}
		owner = s.Owner
		// The label travels too: a derivative readable under a broader
		// label than one of its sources is the same leak by another
		// route.
		label = s.PermissionLabel
	}
	m.Audience = strictest
	if owner != "" {
		m.Owner = owner
	}
	m.PermissionLabel = label
	if err := validateMemory(m); err != nil {
		return 0, err
	}
	if m.Confidence == "" {
		m.Confidence = ConfidenceInferred
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: deriving memory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seq, err := insertMemoryTx(tx, m)
	if err != nil {
		return 0, err
	}
	for _, src := range sources {
		if _, err := tx.Exec(
			`INSERT INTO butler_memory_derivations (derivative, source) VALUES (?, ?)`,
			seq, src); err != nil {
			return 0, fmt.Errorf("runtimedb: recording derivation: %w", err)
		}
	}
	if m.Audience == ButlerAudienceShared {
		if err := digestTx(tx, seq, MemoryRecorded, "learned: "+m.Content, m.Author, false); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: deriving memory: %w", err)
	}
	return seq, nil
}

// CorrectMemory supersedes a record with a new one. The old row stays,
// so "what did it think last week, and why" remains answerable.
//
// Any admitted adult may correct household-SHARED memory without the
// original author's approval (A1); a private record is its owner's, and
// the caller enforces that above this layer where the acting member is
// known.
func (db *DB) CorrectMemory(seq int64, replacement MemoryRecord, actor string) (int64, error) {
	if actor == "" {
		return 0, fmt.Errorf("runtimedb: a correction needs the person making it")
	}
	old, err := db.GetMemory(seq)
	if err != nil {
		return 0, err
	}
	if !old.Eligible() {
		return 0, fmt.Errorf("runtimedb: memory %d was already corrected or forgotten", seq)
	}
	// A correction cannot widen the audience of what it corrects. That
	// would make "actually, it is X" a way to publish a private record.
	if old.Audience == ButlerAudiencePrivate {
		replacement.Audience = ButlerAudiencePrivate
	}
	if replacement.Audience == "" {
		replacement.Audience = old.Audience
	}
	if replacement.PermissionLabel == "" {
		replacement.PermissionLabel = old.PermissionLabel
	}
	if replacement.Owner == "" {
		replacement.Owner = old.Owner
	}
	if replacement.Kind == "" {
		replacement.Kind = old.Kind
	}
	if replacement.Subject == "" {
		replacement.Subject = old.Subject
	}
	if replacement.Author == "" {
		replacement.Author = actor
	}
	if replacement.SourceKind == "" {
		replacement.SourceKind, replacement.SourceID = old.SourceKind, old.SourceID
	}
	if err := validateMemory(replacement); err != nil {
		return 0, err
	}
	if replacement.Confidence == "" {
		replacement.Confidence = ConfidenceStated
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: correcting memory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	newSeq, err := insertMemoryTx(tx, replacement)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE butler_memory SET superseded_by = ? WHERE seq = ?`, newSeq, seq); err != nil {
		return 0, fmt.Errorf("runtimedb: correcting memory: %w", err)
	}
	// Everything derived from the corrected record is invalidated: a
	// summary of something that turned out to be wrong is also wrong.
	if _, err := tx.Exec(
		`UPDATE butler_memory SET forgotten_at = ?, forgotten_by = ?
		 WHERE seq IN (SELECT derivative FROM butler_memory_derivations WHERE source = ?)
		   AND forgotten_at = ''`, now(), actor, seq); err != nil {
		return 0, fmt.Errorf("runtimedb: invalidating derivatives: %w", err)
	}
	// The old text stops being searchable now, not at the next
	// compaction. A correction that leaves the wrong answer as a search
	// hit has not corrected anything.
	if _, err := tx.Exec(
		`DELETE FROM butler_index WHERE memory = ?
		    OR memory IN (SELECT derivative FROM butler_memory_derivations WHERE source = ?)`,
		seq, seq); err != nil {
		return 0, fmt.Errorf("runtimedb: removing corrected material from the index: %w", err)
	}
	if old.Audience == ButlerAudienceShared {
		if err := digestTx(tx, newSeq, MemoryCorrected,
			fmt.Sprintf("corrected %q to %q", old.Content, replacement.Content), actor,
			old.SafetyRelevant); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("runtimedb: correcting memory: %w", err)
	}
	return newSeq, nil
}

// ForgetMemory marks a record forgotten and suppresses its source for
// that content.
//
// Both halves are needed. The marker stops retrieval; the suppression
// stops the next extraction pass reading the same conversation and
// cheerfully learning it again — which is how "I asked you to forget
// that" becomes something a household says three times.
func (db *DB) ForgetMemory(seq int64, actor string) error {
	if actor == "" {
		return fmt.Errorf("runtimedb: forgetting needs the person asking")
	}
	m, err := db.GetMemory(seq)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: forgetting memory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stamp := now()
	if _, err := tx.Exec(
		`UPDATE butler_memory SET forgotten_at = ?, forgotten_by = ? WHERE seq = ? AND forgotten_at = ''`,
		stamp, actor, seq); err != nil {
		return fmt.Errorf("runtimedb: forgetting memory: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO butler_memory_suppressed_sources (source_kind, source_id, content_key, suppressed_at, suppressed_by)
		 VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		m.SourceKind, m.SourceID, contentKey(m.Content), stamp, actor); err != nil {
		return fmt.Errorf("runtimedb: suppressing source: %w", err)
	}
	// Derivatives go with it, for the same reason they go with a
	// correction.
	if _, err := tx.Exec(
		`UPDATE butler_memory SET forgotten_at = ?, forgotten_by = ?
		 WHERE seq IN (SELECT derivative FROM butler_memory_derivations WHERE source = ?)
		   AND forgotten_at = ''`, stamp, actor, seq); err != nil {
		return fmt.Errorf("runtimedb: invalidating derivatives: %w", err)
	}
	// And the searchable copies go in the SAME transaction. Leaving
	// them for the next compaction would mean "I asked you to forget
	// that" holds until a sweep runs — the record stops being
	// retrievable while its text is still a search hit.
	if _, err := tx.Exec(
		`DELETE FROM butler_index WHERE memory = ?
		    OR memory IN (SELECT derivative FROM butler_memory_derivations WHERE source = ?)`,
		seq, seq); err != nil {
		return fmt.Errorf("runtimedb: removing forgotten material from the index: %w", err)
	}
	if m.Audience == ButlerAudienceShared {
		// A safety-relevant removal is immediate; everything else waits
		// for the next digest (A1).
		if err := digestTx(tx, seq, MemoryForgotten,
			fmt.Sprintf("forgot: %s", m.Content), actor, m.SafetyRelevant); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtimedb: forgetting memory: %w", err)
	}
	return nil
}

// InvalidateMemoryFromSource forgets everything learned from one source
// — what a deleted conversation or a revoked connection does (A3.1).
func (db *DB) InvalidateMemoryFromSource(sourceKind, sourceID, actor string) (int, error) {
	if actor == "" {
		return 0, fmt.Errorf("runtimedb: invalidating a source needs who did it")
	}
	rows, err := db.sql.Query(
		`SELECT seq FROM butler_memory WHERE source_kind = ? AND source_id = ? AND forgotten_at = ''`,
		sourceKind, sourceID)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: reading memory from source: %w", err)
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return 0, err
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, seq := range seqs {
		if err := db.ForgetMemory(seq, actor); err != nil {
			return 0, err
		}
	}
	return len(seqs), nil
}

// GetMemory returns one record, forgotten or not.
func (db *DB) GetMemory(seq int64) (MemoryRecord, error) {
	return scanMemory(db.sql.QueryRow(memoryColumns+` WHERE seq = ?`, seq))
}

const memoryColumns = `SELECT seq, kind, subject, content, audience, permission_label, owner,
	 source_kind, source_id, observed_at, confidence, safety_relevant, created_at, author,
	 COALESCE(superseded_by, 0), forgotten_at, forgotten_by, envelope_version
	 FROM butler_memory`

func scanMemory(row rowScanner) (MemoryRecord, error) {
	var m MemoryRecord
	var audience, observed, created, forgotten string
	var safety int
	err := row.Scan(&m.Seq, &m.Kind, &m.Subject, &m.Content, &audience, &m.PermissionLabel,
		&m.Owner, &m.SourceKind, &m.SourceID, &observed, &m.Confidence, &safety, &created,
		&m.Author, &m.SupersededBy, &forgotten, &m.ForgottenBy, &m.EnvelopeVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return MemoryRecord{}, ErrNotFound
	}
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("runtimedb: scanning memory: %w", err)
	}
	m.Audience = ButlerTaskAudience(audience)
	m.SafetyRelevant = safety != 0
	for _, f := range []struct {
		text string
		into *time.Time
	}{{observed, &m.ObservedAt}, {created, &m.CreatedAt}, {forgotten, &m.ForgottenAt}} {
		if f.text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, f.text)
		if err != nil {
			return MemoryRecord{}, fmt.Errorf("runtimedb: bad memory timestamp %q: %w", f.text, err)
		}
		*f.into = parsed
	}
	return m, nil
}

// MemoryQuery narrows a retrieval. Permission comes FIRST: the reader
// and their audience are applied in SQL, before any content is loaded,
// so nothing a reader may not see is ever in memory on their behalf.
type MemoryQuery struct {
	// Reader is the member retrieving. Their own private records and
	// household-shared ones are eligible; nobody else's private ones
	// are.
	Reader string
	// Subject narrows to records about one person or thing.
	Subject string
	Kind    string
	Limit   int
	// Butler is the scope: which Butler is asking. It narrows ON TOP OF
	// the reader's permission and never instead of it — a Butler is not
	// a way to read somebody else's private material (ADR-W031 A4).
	//
	// Empty means the shared pool alone, so a caller that forgets to
	// name a Butler sees household facts and nothing specialised.
	// AllButlers is the Console holder's view (A10).
	Butler string
}

// RetrieveMemory returns eligible records for a reader, newest first.
func (db *DB) RetrieveMemory(q MemoryQuery) ([]MemoryRecord, error) {
	if err := db.gate(GateRetrieval); err != nil {
		return nil, err
	}
	if q.Reader == "" {
		return nil, fmt.Errorf("runtimedb: memory retrieval needs a reader (ADR-W030 A2)")
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	where := ` WHERE forgotten_at = '' AND superseded_by IS NULL
	           AND (audience = 'shared' OR owner = ?)`
	args := []any{q.Reader}
	// The Butler's scope is ANDed onto the reader's, in SQL, before any
	// content loads. Both have to hold: this person may see it AND this
	// Butler holds it.
	if clause, scopeArgs := scopeClause("butler", q.Butler); clause != "" {
		where += clause
		args = append(args, scopeArgs...)
	}
	if q.Subject != "" {
		where += " AND subject = ?"
		args = append(args, q.Subject)
	}
	if q.Kind != "" {
		where += " AND kind = ?"
		args = append(args, q.Kind)
	}
	args = append(args, q.Limit)
	rows, err := db.sql.Query(memoryColumns+where+` ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: retrieving memory: %w", err)
	}
	defer rows.Close()
	var out []MemoryRecord
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ExportMemoryFor is a person's personal export (A3): everything they
// may see, including their own forgotten records, which are theirs to
// have a copy of.
//
// It is a READ governed by current permissions and never a bypass:
// material the reader could not retrieve is material the export does not
// contain (A10).
func (db *DB) ExportMemoryFor(reader string) ([]MemoryRecord, error) {
	// An export is a read governed by current permissions and never a
	// bypass of quarantine (ADR-W030 A10).
	if err := db.gate(GateRetrieval); err != nil {
		return nil, err
	}
	if reader == "" {
		return nil, fmt.Errorf("runtimedb: an export needs the person it is for")
	}
	// Their OWN forgotten records are theirs to have a copy of. Other
	// people's are not: a shared record that was forgotten or corrected
	// is not something an export may hand back in full, or "forget
	// that" would mean "until somebody exports".
	rows, err := db.sql.Query(
		memoryColumns+` WHERE owner = ?
		     OR (audience = 'shared' AND forgotten_at = '' AND superseded_by IS NULL)
		   ORDER BY seq`, reader)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: exporting memory: %w", err)
	}
	defer rows.Close()
	var out []MemoryRecord
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemoryDigestEntry is one change to shared memory other adults should
// hear about.
type MemoryDigestEntry struct {
	Seq       int64     `json:"seq"`
	Memory    int64     `json:"memory"`
	Change    string    `json:"change"`
	Summary   string    `json:"summary"`
	Actor     string    `json:"actor"`
	At        time.Time `json:"at"`
	Immediate bool      `json:"immediate"`
}

// PendingMemoryDigest returns undelivered digest entries, oldest first.
// immediateOnly returns just the safety-relevant removals, which go out
// now rather than with the next digest (A1).
func (db *DB) PendingMemoryDigest(immediateOnly bool) ([]MemoryDigestEntry, error) {
	where := ` WHERE delivered_at = ''`
	if immediateOnly {
		where += ` AND immediate = 1`
	}
	rows, err := db.sql.Query(
		`SELECT seq, memory, change, summary, actor, at, immediate FROM butler_memory_digest` +
			where + ` ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading memory digest: %w", err)
	}
	defer rows.Close()
	var out []MemoryDigestEntry
	for rows.Next() {
		var e MemoryDigestEntry
		var at string
		var immediate int
		if err := rows.Scan(&e.Seq, &e.Memory, &e.Change, &e.Summary, &e.Actor, &at, &immediate); err != nil {
			return nil, err
		}
		e.Immediate = immediate != 0
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkMemoryDigestDelivered records that entries have gone out.
func (db *DB) MarkMemoryDigestDelivered(seqs []int64) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("runtimedb: marking digest delivered: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stamp := now()
	for _, seq := range seqs {
		if _, err := tx.Exec(
			`UPDATE butler_memory_digest SET delivered_at = ? WHERE seq = ? AND delivered_at = ''`,
			stamp, seq); err != nil {
			return fmt.Errorf("runtimedb: marking digest delivered: %w", err)
		}
	}
	return tx.Commit()
}
