package runtimedb

// Signed recovery-journal evidence is the narrow bridge between a restored
// SQLite snapshot and deletion/revocation history retained outside it.  The
// SQLite journal remains the authority ledger; this envelope makes its exact
// prefix, terminal sequence and source cutoff independently portable and
// tamper evident.  It is intentionally not a general event system.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const RecoveryJournalEvidenceFormat = "wildstone-recovery-journal-v1"

var journalGenesis = sha256.Sum256([]byte("wildstone-recovery-journal-v1\n"))

type RecoveryJournalEntry struct {
	Sequence    int64  `json:"sequence"`
	SubjectKind string `json:"subjectKind"`
	SubjectID   string `json:"subjectId"`
	Action      string `json:"action"`
	At          string `json:"at"`
	Actor       string `json:"actor"`
	Detail      string `json:"detail"`
}

type RecoveryJournalEvidence struct {
	Format            string                 `json:"format"`
	SnapshotSHA256    string                 `json:"snapshotSha256"`
	SnapshotAt        time.Time              `json:"snapshotAt"`
	JournalSource     string                 `json:"journalSource"`
	Custody           string                 `json:"custody"`
	BaseSequence      int64                  `json:"baseSequence"`
	BaseSHA256        string                 `json:"baseSha256"`
	HeadSequence      int64                  `json:"headSequence"`
	HeadSHA256        string                 `json:"headSha256"`
	CoverageThrough   time.Time              `json:"coverageThrough"`
	SourceFencedAt    time.Time              `json:"sourceFencedAt"`
	GeneratedAt       time.Time              `json:"generatedAt"`
	Final             bool                   `json:"final"`
	SignerFingerprint string                 `json:"signerFingerprint"`
	Entries           []RecoveryJournalEntry `json:"entries"`
	Signature         string                 `json:"signature,omitempty"`
}

type RecoveryJournalResult struct {
	EvidenceID      string `json:"evidenceId"`
	Applied         int    `json:"applied"`
	HeadSequence    int64  `json:"headSequence"`
	HeadSHA256      string `json:"headSha256"`
	AlreadyVerified bool   `json:"alreadyVerified"`
}

func journalSigningPayload(e RecoveryJournalEvidence) ([]byte, error) {
	e.Signature = ""
	return json.Marshal(e)
}

func signingFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

func chainJournal(entries []RecoveryJournalEntry) (string, error) {
	previous := journalGenesis[:]
	for i, entry := range entries {
		if entry.Sequence != int64(i+1) {
			return "", fmt.Errorf("recovery journal: sequence gap at %d (found %d)", i+1, entry.Sequence)
		}
		body, err := json.Marshal(entry)
		if err != nil {
			return "", err
		}
		h := sha256.New()
		_, _ = h.Write(previous)
		_, _ = h.Write(body)
		previous = h.Sum(nil)
	}
	return hex.EncodeToString(previous), nil
}

func readRecoveryJournal(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) ([]RecoveryJournalEntry, error) {
	rows, err := q.Query(`SELECT seq,subject_kind,subject_id,action,at,actor,detail FROM deletion_journal ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []RecoveryJournalEntry
	for rows.Next() {
		var entry RecoveryJournalEntry
		if err := rows.Scan(&entry.Sequence, &entry.SubjectKind, &entry.SubjectID, &entry.Action, &entry.At, &entry.Actor, &entry.Detail); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BuildRecoveryJournalEvidence creates a terminal evidence envelope from a
// fenced source database and an exact snapshot artifact.  The caller is
// responsible for fencing the source and placing the returned bytes in
// independent custody; neither condition can be proved by the snapshot.
func (db *DB) BuildRecoveryJournalEvidence(snapshotPath, source, custody string, snapshotAt, sourceFencedAt, generatedAt time.Time, privateKey ed25519.PrivateKey) ([]byte, error) {
	if source == "" || custody == "" {
		return nil, fmt.Errorf("recovery journal: source and independent custody description are required")
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("recovery journal: invalid signing key")
	}
	if snapshotAt.IsZero() || sourceFencedAt.Before(snapshotAt) || generatedAt.Before(sourceFencedAt) {
		return nil, fmt.Errorf("recovery journal: snapshot, fencing and generation times are inconsistent")
	}
	snapshotSHA, err := sha256File(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("recovery journal: hashing snapshot: %w", err)
	}
	snapshotDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", snapshotPath))
	if err != nil {
		return nil, fmt.Errorf("recovery journal: opening snapshot: %w", err)
	}
	defer snapshotDB.Close()
	base, err := readRecoveryJournal(snapshotDB)
	if err != nil {
		return nil, fmt.Errorf("recovery journal: reading snapshot prefix: %w", err)
	}
	entries, err := readRecoveryJournal(db.sql)
	if err != nil {
		return nil, fmt.Errorf("recovery journal: reading source: %w", err)
	}
	if len(entries) < len(base) {
		return nil, fmt.Errorf("recovery journal: source is behind the snapshot")
	}
	for i := range base {
		if base[i] != entries[i] {
			return nil, fmt.Errorf("recovery journal: source diverges from snapshot at sequence %d", i+1)
		}
	}
	baseHash, err := chainJournal(base)
	if err != nil {
		return nil, err
	}
	headHash, err := chainJournal(entries)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		at, err := time.Parse(time.RFC3339Nano, entry.At)
		if err != nil || at.After(sourceFencedAt) {
			return nil, fmt.Errorf("recovery journal: entry %d is outside the terminal coverage", entry.Sequence)
		}
	}
	pub := privateKey.Public().(ed25519.PublicKey)
	evidence := RecoveryJournalEvidence{
		Format: RecoveryJournalEvidenceFormat, SnapshotSHA256: snapshotSHA, SnapshotAt: snapshotAt.UTC(),
		JournalSource: source, Custody: custody, BaseSequence: int64(len(base)), BaseSHA256: baseHash,
		HeadSequence: int64(len(entries)), HeadSHA256: headHash, CoverageThrough: sourceFencedAt.UTC(),
		SourceFencedAt: sourceFencedAt.UTC(), GeneratedAt: generatedAt.UTC(), Final: true,
		SignerFingerprint: signingFingerprint(pub), Entries: entries,
	}
	payload, _ := journalSigningPayload(evidence)
	evidence.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return json.Marshal(evidence)
}

func decodeRecoveryJournalEvidence(body []byte) (RecoveryJournalEvidence, []byte, string, error) {
	var evidence RecoveryJournalEvidence
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&evidence); err != nil {
		return evidence, nil, "", fmt.Errorf("recovery journal: decoding evidence: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return evidence, nil, "", fmt.Errorf("recovery journal: evidence has trailing data")
	}
	canonical, err := json.Marshal(evidence)
	if err != nil {
		return evidence, nil, "", err
	}
	id := sha256.Sum256(canonical)
	payload, err := journalSigningPayload(evidence)
	return evidence, payload, hex.EncodeToString(id[:]), err
}

// ReconcileRestoreEvidence verifies independent, signed evidence, imports any
// post-snapshot entries, replays the journal, and retains immutable audit
// evidence.  It never releases restore quarantine.
func (db *DB) ReconcileRestoreEvidence(body []byte, trustedKey ed25519.PublicKey, verifiedBy string, verifiedAt time.Time) (RecoveryJournalResult, error) {
	var result RecoveryJournalResult
	if verifiedBy == "" || len(trustedKey) != ed25519.PublicKeySize {
		return result, fmt.Errorf("recovery journal: a trusted signer and current Owner are required")
	}
	evidence, payload, evidenceID, err := decodeRecoveryJournalEvidence(body)
	if err != nil {
		return result, err
	}
	result = RecoveryJournalResult{EvidenceID: evidenceID, HeadSequence: evidence.HeadSequence, HeadSHA256: evidence.HeadSHA256}
	var priorApplied int
	err = db.sql.QueryRow(`SELECT applied_count FROM recovery_journal_reconciliations WHERE evidence_id=?`, evidenceID).Scan(&priorApplied)
	if err == nil {
		result.Applied, result.AlreadyVerified = priorApplied, true
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	signature, err := base64.StdEncoding.DecodeString(evidence.Signature)
	if err != nil || !ed25519.Verify(trustedKey, payload, signature) {
		return result, fmt.Errorf("recovery journal: signature verification failed")
	}
	if evidence.Format != RecoveryJournalEvidenceFormat || evidence.JournalSource == "" || evidence.Custody == "" || !evidence.Final {
		return result, fmt.Errorf("recovery journal: evidence is incomplete or not terminal")
	}
	if evidence.SignerFingerprint != signingFingerprint(trustedKey) {
		return result, fmt.Errorf("recovery journal: signer fingerprint does not match the trusted installation identity")
	}
	state, err := db.GetRestoreState()
	if err != nil || !state.Quarantined {
		return result, fmt.Errorf("recovery journal: database is not in restore quarantine")
	}
	if state.SnapshotSHA256 == "" || evidence.SnapshotSHA256 != state.SnapshotSHA256 || !evidence.SnapshotAt.Equal(state.SnapshotAt) {
		return result, fmt.Errorf("recovery journal: evidence does not bind to this restored snapshot")
	}
	if evidence.SourceFencedAt.Before(evidence.SnapshotAt) || evidence.CoverageThrough.Before(evidence.SourceFencedAt) || evidence.GeneratedAt.Before(evidence.CoverageThrough) {
		return result, fmt.Errorf("recovery journal: stale or inconsistent coverage")
	}
	if evidence.GeneratedAt.After(verifiedAt.Add(5 * time.Minute)) {
		return result, fmt.Errorf("recovery journal: generation time is in the future")
	}
	if evidence.HeadSequence != int64(len(evidence.Entries)) || evidence.BaseSequence < 0 || evidence.BaseSequence > evidence.HeadSequence {
		return result, fmt.Errorf("recovery journal: sequence bounds are inconsistent")
	}
	headHash, err := chainJournal(evidence.Entries)
	if err != nil || headHash != evidence.HeadSHA256 {
		return result, fmt.Errorf("recovery journal: head integrity check failed: %v", err)
	}
	baseHash, err := chainJournal(evidence.Entries[:evidence.BaseSequence])
	if err != nil || baseHash != evidence.BaseSHA256 {
		return result, fmt.Errorf("recovery journal: snapshot-prefix integrity check failed")
	}
	for _, entry := range evidence.Entries {
		at, err := time.Parse(time.RFC3339Nano, entry.At)
		if err != nil || at.After(evidence.CoverageThrough) {
			return result, fmt.Errorf("recovery journal: entry %d falls outside verified coverage", entry.Sequence)
		}
	}
	local, err := readRecoveryJournal(db.sql)
	if err != nil {
		return result, err
	}
	if int64(len(local)) < evidence.BaseSequence {
		return result, fmt.Errorf("recovery journal: restored snapshot is missing its declared journal prefix")
	}
	if int64(len(local)) > evidence.HeadSequence {
		return result, fmt.Errorf("recovery journal: local journal extends beyond unrecorded evidence")
	}
	for i := int64(0); i < int64(len(local)); i++ {
		if local[i] != evidence.Entries[i] {
			return result, fmt.Errorf("recovery journal: local journal conflicts at sequence %d", i+1)
		}
	}
	for i := int64(len(local)); i < evidence.HeadSequence; i++ {
		e := evidence.Entries[i]
		if _, err := db.sql.Exec(`INSERT INTO deletion_journal(seq,subject_kind,subject_id,action,at,actor,detail) VALUES(?,?,?,?,?,?,?)`,
			e.Sequence, e.SubjectKind, e.SubjectID, e.Action, e.At, e.Actor, e.Detail); err != nil {
			return result, fmt.Errorf("recovery journal: importing sequence %d: %w", e.Sequence, err)
		}
	}
	applied, err := db.ReconcileRestore()
	if err != nil {
		return result, err
	}
	canonical, _ := json.Marshal(evidence)
	tx, err := db.sql.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO recovery_journal_reconciliations
		(evidence_id,snapshot_sha256,snapshot_at,journal_source,custody,base_seq,head_seq,head_sha256,coverage_through,source_fenced_at,signer_fingerprint,verified_at,verified_by,applied_count,evidence_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, evidenceID, evidence.SnapshotSHA256, timeText(evidence.SnapshotAt),
		evidence.JournalSource, evidence.Custody, evidence.BaseSequence, evidence.HeadSequence, evidence.HeadSHA256,
		timeText(evidence.CoverageThrough), timeText(evidence.SourceFencedAt), evidence.SignerFingerprint,
		timeText(verifiedAt), verifiedBy, applied, string(canonical))
	if err == nil {
		_, err = tx.Exec(`UPDATE restore_state SET journal_current=1,journal_asserted_by=?,journal_evidence_id=?,
			journal_source=?,journal_head_seq=?,journal_head_sha256=?,journal_coverage_through=?,journal_verified_at=?,journal_verified_by=? WHERE id=1 AND quarantined=1`,
			verifiedBy, evidenceID, evidence.JournalSource, evidence.HeadSequence, evidence.HeadSHA256,
			timeText(evidence.CoverageThrough), timeText(verifiedAt), verifiedBy)
	}
	if err != nil {
		return result, fmt.Errorf("recovery journal: recording verification: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.Applied = applied
	return result, nil
}
