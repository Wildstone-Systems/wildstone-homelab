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
	PromotedTo      int64              `json:"promotedTo,omitempty"`
	Scope           string             `json:"scope"`
	Seq             int64              `json:"seq"`
	Kind            string             `json:"kind"`
	Subject         string             `json:"subject,omitempty"`
	Content         string             `json:"content"`
	Audience        ButlerTaskAudience `json:"audience"`
	PermissionLabel string             `json:"permissionLabel"`
	Owner           string             `json:"owner"`
	// Butler holds Agent-local memory. For Personal/Household records it is
	// creation provenance, not an access boundary (ADR-W033 section 11).
	Butler          string    `json:"butler,omitempty"`
	SourceKind      string    `json:"sourceKind"`
	SourceID        string    `json:"sourceId"`
	ObservedAt      time.Time `json:"observedAt,omitzero"`
	Confidence      string    `json:"confidence"`
	SafetyRelevant  bool      `json:"safetyRelevant,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	Author          string    `json:"author"`
	SupersededBy    int64     `json:"supersededBy,omitempty"`
	ForgottenAt     time.Time `json:"forgottenAt,omitzero"`
	ForgottenBy     string    `json:"forgottenBy,omitempty"`
	EnvelopeVersion int       `json:"envelopeVersion"`
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
	switch m.Scope {
	case MemoryScopePersonal:
		if m.Owner == HouseholdOwner {
			return fmt.Errorf("personal memory requires a Person")
		}
	case MemoryScopeAgentLocal:
		if m.Butler == "" {
			return fmt.Errorf("Agent-local memory requires an Agent")
		}
	case MemoryScopeHousehold:
	default:
		return fmt.Errorf("invalid durable memory scope %q", m.Scope)
	}
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
	// Non-empty, and no further. The closed set of two words moved into
	// the `audiences` table in slice 7 precisely so a household could
	// name its own — "adults only" was the capability the two words
	// could not express — and a list written here would be a second,
	// shorter answer to which audiences exist. The foreign key on
	// butler_memory.audience is the backstop, and it is the right place
	// for one: it refuses an audience that does not exist no matter
	// which code path wrote the row.
	if m.Audience == "" {
		return fmt.Errorf("runtimedb: a memory record needs an audience — who it may be disclosed to (ADR-W030 A2, design §10 layer 3)")
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
	defaultMemoryScope(&m)
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
	var suppressed int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM butler_memory_suppressed_sources WHERE source_kind=? AND source_id=? AND content_key=?`, m.SourceKind, m.SourceID, contentKey(m.Content)).Scan(&suppressed); err != nil {
		return 0, err
	}
	if suppressed > 0 {
		return 0, ErrSourceSuppressed
	}
	// Owner and author are stored as identities. The owner may be the
	// household, which is NULL; the author is always a person, because
	// a record nobody stated is not a record. Both resolve inside this
	// transaction so a rename landing mid-write cannot split them.
	owner, err := ownerPersonID(tx, m.Owner)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: recording memory: the owner %w", err)
	}
	author, err := resolvePersonID(tx, m.Author)
	if err != nil {
		return 0, fmt.Errorf("runtimedb: recording memory: the author %w", err)
	}
	res, err := tx.Exec(
		`INSERT INTO butler_memory (kind, subject, content, audience, permission_label, owner_person_id,
		     source_kind, source_id, observed_at, confidence, safety_relevant, created_at, author_person_id,
		     envelope_version, butler, scope)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Kind, m.Subject, m.Content, string(m.Audience), m.PermissionLabel, owner,
		m.SourceKind, m.SourceID, timeText(m.ObservedAt), m.Confidence,
		boolInt(m.SafetyRelevant), now(), author, TaskEnvelopeVersion, m.Butler, m.Scope)
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
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: deriving memory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The audiences of every source, for the intersection below. The
	// owner audience short-circuits it: it is already the narrowest
	// thing an audience can be, and it travels with an owner, which no
	// other kind does.
	var audiences []string
	anyOwnerAudience := false
	label := m.PermissionLabel
	owner := ""
	butler := ""
	for _, src := range sources {
		s, err := scanMemory(tx.QueryRow(memoryColumns+` WHERE r.seq=?`, src))
		if err != nil {
			return 0, fmt.Errorf("runtimedb: deriving from memory %d: %w", src, err)
		}
		// Only Agent-local sources impose an Agent boundary. Personal and
		// Household sources retain their independent audience constraints.
		if !s.Eligible() {
			return 0, fmt.Errorf("cannot derive from inactive memory")
		}
		if s.Scope == MemoryScopeAgentLocal {
			if butler == "" {
				butler = s.Butler
			} else if s.Butler != butler {
				return 0, fmt.Errorf(
					"runtimedb: refusing to derive from material held by two different butlers (%s and %s) — there is no scope the result could correctly have (ADR-W031 A4)",
					butler, s.Butler)
			}
		}
		audiences = append(audiences, string(s.Audience))
		if s.Audience != ButlerAudiencePrivate && s.Scope != MemoryScopePersonal {
			continue
		}
		anyOwnerAudience = true
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
	// The narrowest audience among the sources. An owner audience wins
	// outright — one person is as narrow as disclosure gets — and
	// otherwise the sets are compared, which is where "Adults and a
	// household record makes an Adults derivative" comes from.
	if anyOwnerAudience {
		m.Audience = ButlerAudiencePrivate
	} else {
		strictest, err := db.StrictestAudience(audiences)
		if err != nil {
			return 0, err
		}
		m.Audience = ButlerTaskAudience(strictest)
	}
	if owner != "" {
		m.Owner = owner
	}
	m.PermissionLabel = label
	if butler != "" {
		m.Butler, m.Scope = butler, MemoryScopeAgentLocal
	}
	defaultMemoryScope(&m)
	if anyOwnerAudience && m.Scope == MemoryScopeHousehold {
		m.Scope = MemoryScopePersonal
	}
	if err := validateMemory(m); err != nil {
		return 0, err
	}
	if m.Confidence == "" {
		m.Confidence = ConfidenceInferred
	}

	seq, err := insertMemoryTx(tx, m)
	if err != nil {
		return 0, err
	}
	for _, src := range sources {
		if err := inheritMemoryConstraints(tx, seq, src); err != nil {
			return 0, err
		}
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
	// Read the source in the same snapshot as the replacement. Retirement
	// can transfer its holding Agent, and a stale read could recreate local
	// memory under the retired Agent after that transfer commits.
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("runtimedb: correcting memory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	old, err := scanMemory(tx.QueryRow(memoryColumns+` WHERE r.seq=?`, seq))
	if err != nil {
		return 0, err
	}
	if !old.Eligible() {
		return 0, fmt.Errorf("runtimedb: memory %d was already corrected or forgotten", seq)
	}
	// A correction cannot widen the audience of what it corrects. That
	// would make "actually, it is X" a way to publish a private record.
	//
	// While there were two words this was one comparison: private beat
	// shared. With sets it is the same subset test the derivation rule
	// uses, so the two cannot disagree — and it CLAMPS rather than
	// refuses, because a correction naming a wider audience is a caller
	// who did not think about audiences, not one trying to publish.
	// Incomparable audiences are refused, as they are everywhere else:
	// there is no audience the correction could correctly have.
	if replacement.Audience == "" {
		replacement.Audience = old.Audience
	}
	if old.Audience == ButlerAudiencePrivate || replacement.Audience == ButlerAudiencePrivate {
		// The owner audience resolves against the row, not against
		// itself, and is already the narrowest thing there is — so it
		// short-circuits here exactly as it does in DeriveMemory.
		replacement.Audience = ButlerAudiencePrivate
	} else if replacement.Audience != old.Audience {
		narrowest, err := db.StrictestAudience(
			[]string{string(old.Audience), string(replacement.Audience)})
		if err != nil {
			return 0, err
		}
		replacement.Audience = ButlerTaskAudience(narrowest)
	}
	if replacement.PermissionLabel == "" {
		replacement.PermissionLabel = old.PermissionLabel
	}
	if replacement.Owner == "" {
		replacement.Owner = old.Owner
	}
	// A correction stays in the scope it corrects. Without this
	// "actually, it is X" moves a scoped record into the shared pool,
	// which is the same publication A4 forbids, arriving through the
	// edit path rather than the read path.
	replacement.Butler = old.Butler
	replacement.Scope = old.Scope
	replacement.Owner = old.Owner
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

	newSeq, err := insertMemoryTx(tx, replacement)
	if err != nil {
		return 0, err
	}
	if err := inheritMemoryConstraints(tx, newSeq, seq); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE butler_memory SET superseded_by = ? WHERE seq = ?`, newSeq, seq); err != nil {
		return 0, fmt.Errorf("runtimedb: correcting memory: %w", err)
	}
	// Everything derived from the corrected record is invalidated: a
	// summary of something that turned out to be wrong is also wrong.
	if _, err := tx.Exec(
		`UPDATE butler_memory SET forgotten_at = ?, forgotten_by = ?
		 WHERE seq IN (WITH RECURSIVE descendants(seq) AS (
 SELECT derivative FROM butler_memory_derivations WHERE source = ?
 UNION SELECT d.derivative FROM butler_memory_derivations d JOIN descendants a ON d.source=a.seq
) SELECT seq FROM descendants)
		   AND forgotten_at = ''`, now(), actor, seq); err != nil {
		return 0, fmt.Errorf("runtimedb: invalidating derivatives: %w", err)
	}
	// The old text stops being searchable now, not at the next
	// compaction. A correction that leaves the wrong answer as a search
	// hit has not corrected anything.
	if _, err := tx.Exec(
		`DELETE FROM butler_index WHERE memory = ?
		    OR memory IN (WITH RECURSIVE descendants(seq) AS (
 SELECT derivative FROM butler_memory_derivations WHERE source = ?
 UNION SELECT d.derivative FROM butler_memory_derivations d JOIN descendants a ON d.source=a.seq
) SELECT seq FROM descendants)`,
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
		 WHERE seq IN (WITH RECURSIVE descendants(seq) AS (
 SELECT derivative FROM butler_memory_derivations WHERE source = ?
 UNION SELECT d.derivative FROM butler_memory_derivations d JOIN descendants a ON d.source=a.seq
) SELECT seq FROM descendants)
		   AND forgotten_at = ''`, stamp, actor, seq); err != nil {
		return fmt.Errorf("runtimedb: invalidating derivatives: %w", err)
	}
	// And the searchable copies go in the SAME transaction. Leaving
	// them for the next compaction would mean "I asked you to forget
	// that" holds until a sweep runs — the record stops being
	// retrievable while its text is still a search hit.
	if _, err := tx.Exec(
		`DELETE FROM butler_index WHERE memory = ?
		    OR memory IN (WITH RECURSIVE descendants(seq) AS (
 SELECT derivative FROM butler_memory_derivations WHERE source = ?
 UNION SELECT d.derivative FROM butler_memory_derivations d JOIN descendants a ON d.source=a.seq
) SELECT seq FROM descendants)`,
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
	if err := suppressMemoryDescendants(tx, seq, actor); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO deletion_journal(subject_kind,subject_id,action,at,actor,detail)
 SELECT 'memory',?,'forgotten',?,?,'' WHERE NOT EXISTS
 (SELECT 1 FROM deletion_journal WHERE subject_kind='memory' AND subject_id=? AND action='forgotten')`, fmt.Sprint(seq), stamp, actor, fmt.Sprint(seq)); err != nil {
		return err
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
	return scanMemory(db.sql.QueryRow(memoryColumns+` WHERE r.seq = ?`, seq))
}

// Owner and author come back as the handles they carry now, so a
// caller above this package still sees names. The owner join is a LEFT
// one and its absence is the household: NULL is how 0043 stores a
// record the household owns rather than any one person.
const memoryColumns = `SELECT r.seq, r.kind, r.subject, r.content, r.audience, r.permission_label,
	 COALESCE(o.member, 'household'),
	 r.source_kind, r.source_id, r.observed_at, r.confidence, r.safety_relevant, r.created_at,
	 a.member,
	 COALESCE(r.superseded_by, 0), r.forgotten_at, r.forgotten_by, r.envelope_version, r.butler, r.scope,
 COALESCE((SELECT memory FROM memory_scope_events WHERE source_memory=r.seq AND kind='promotion' ORDER BY seq LIMIT 1),0)
	 FROM butler_memory r
	 LEFT JOIN butler_members o ON o.person_id = r.owner_person_id
	 JOIN butler_members a ON a.person_id = r.author_person_id`

func scanMemory(row rowScanner) (MemoryRecord, error) {
	var m MemoryRecord
	var audience, observed, created, forgotten string
	var safety int
	err := row.Scan(&m.Seq, &m.Kind, &m.Subject, &m.Content, &audience, &m.PermissionLabel,
		&m.Owner, &m.SourceKind, &m.SourceID, &observed, &m.Confidence, &safety, &created,
		&m.Author, &m.SupersededBy, &forgotten, &m.ForgottenBy, &m.EnvelopeVersion, &m.Butler, &m.Scope, &m.PromotedTo)
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
	// Butler identifies the requesting Agent. Agent-local records must
	// match it; Personal and Household records cross Agents, subject to
	// ownership, audience and per-Agent restrictions (ADR-W031 A4).
	// Empty excludes Agent-local records.
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
	// A reader who is nobody on record owns nothing and sees the shared
	// material only, which is what the free-text owner match did before
	// 0043 for a principal or an unknown name.
	reader, err := personIDForRead(db.sql, q.Reader)
	if err != nil {
		return nil, err
	}
	clause, audArgs, err := db.visibleAudienceClause("r.audience", "r.owner_person_id", reader)
	if err != nil {
		return nil, err
	}
	inherited, inheritedArgs, err := db.memoryConstraintClause("r", reader)
	if err != nil {
		return nil, err
	}
	clause += " AND " + inherited
	audArgs = append(audArgs, inheritedArgs...)
	where := ` WHERE r.forgotten_at = '' AND r.superseded_by IS NULL AND ` + clause
	args := append([]any{}, audArgs...)
	// The Butler's scope is ANDed onto the reader's, in SQL, before any
	// content loads. Both have to hold: this person may see it AND this
	// Butler holds it.
	if clause, scopeArgs := memoryScopeClause("r", q.Butler, reader); clause != "" {
		where += clause
		args = append(args, scopeArgs...)
	}
	if q.Subject != "" {
		where += " AND r.subject = ?"
		args = append(args, q.Subject)
	}
	if q.Kind != "" {
		where += " AND r.kind = ?"
		args = append(args, q.Kind)
	}
	args = append(args, q.Limit)
	rows, err := db.sql.Query(memoryColumns+where+` ORDER BY r.seq DESC LIMIT ?`, args...)
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
	readerID, err := personIDForRead(db.sql, reader)
	if err != nil {
		return nil, err
	}
	// An export is the same disclosure question as retrieval, so it asks
	// it the same way. It differs only in also handing back the reader's
	// OWN forgotten and superseded records, which are theirs to take.
	audClause, audArgs, err := db.visibleAudienceClause("r.audience", "r.owner_person_id", readerID)
	if err != nil {
		return nil, err
	}
	inherited, inheritedArgs, err := db.memoryConstraintClause("r", readerID)
	if err != nil {
		return nil, err
	}
	args := append([]any{readerID}, audArgs...)
	args = append(args, readerID)
	args = append(args, inheritedArgs...)
	rows, err := db.sql.Query(
		memoryColumns+` WHERE (r.owner_person_id = ?
		     OR (`+audClause+` AND (r.scope <> 'personal' OR r.owner_person_id = ?) AND r.forgotten_at = '' AND r.superseded_by IS NULL)) AND `+inherited+`
		   ORDER BY r.seq`, args...)
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
	// Re-check queued entries, including those written before scope migration.
	// A household digest has no single Person/Agent authorization context.
	where := ` WHERE d.delivered_at = '' AND r.scope = 'household'
 AND r.audience = ? AND NOT EXISTS (
 SELECT 1 FROM memory_constraints mc WHERE mc.memory=r.seq AND mc.audience <> ?)`
	if immediateOnly {
		where += ` AND d.immediate = 1`
	}
	rows, err := db.sql.Query(
		`SELECT d.seq, d.memory, d.change, d.summary, d.actor, d.at, d.immediate FROM butler_memory_digest d JOIN butler_memory r ON r.seq=d.memory`+
			where+` ORDER BY d.seq`, ButlerAudienceShared, ButlerAudienceShared)
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
