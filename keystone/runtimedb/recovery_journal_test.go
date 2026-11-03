package runtimedb

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	journalSnapshotAt = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	journalFencedAt   = time.Date(2099, 11, 3, 11, 0, 0, 0, time.UTC)
	journalVerifiedAt = time.Date(2099, 11, 3, 12, 0, 0, 0, time.UTC)
)

type recoveryJournalFixture struct {
	live         *DB
	snapshotPath string
	restoredPath string
	privateKey   ed25519.PrivateKey
	publicKey    ed25519.PublicKey
}

func newRecoveryJournalFixture(t *testing.T) *recoveryJournalFixture {
	t.Helper()
	live, err := Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	return &recoveryJournalFixture{live: live, snapshotPath: filepath.Join(root, "snapshot.db"),
		restoredPath: filepath.Join(root, "restored.db"), privateKey: priv, publicKey: pub}
}

func (f *recoveryJournalFixture) snapshot(t *testing.T) {
	t.Helper()
	if err := f.live.BackupTo(f.snapshotPath); err != nil {
		t.Fatal(err)
	}
}

func (f *recoveryJournalFixture) evidence(t *testing.T) []byte {
	t.Helper()
	body, err := f.live.BuildRecoveryJournalEvidence(f.snapshotPath, "keystone-primary",
		"offline recovery journal volume", journalSnapshotAt, journalFencedAt, journalFencedAt.Add(time.Minute), f.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (f *recoveryJournalFixture) restore(t *testing.T) *DB {
	t.Helper()
	db, err := RestoreBackup(f.snapshotPath, f.restoredPath, "synthetic recovery journal test", journalSnapshotAt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func resignEvidence(t *testing.T, body []byte, privateKey ed25519.PrivateKey, change func(*RecoveryJournalEvidence)) []byte {
	t.Helper()
	var evidence RecoveryJournalEvidence
	if err := json.Unmarshal(body, &evidence); err != nil {
		t.Fatal(err)
	}
	change(&evidence)
	evidence.Signature = ""
	payload, err := journalSigningPayload(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidence.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	out, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertStillQuarantined(t *testing.T, db *DB) RestoreState {
	t.Helper()
	state, err := db.GetRestoreState()
	if err != nil || !state.Quarantined || !state.ReleasedAt.IsZero() {
		t.Fatalf("restore state = %+v, %v; quarantine must remain active", state, err)
	}
	return state
}

func TestRecoveryJournalReconciliationCompleteWithoutPostSnapshotChanges(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	f.snapshot(t)
	restored := f.restore(t)
	result, err := restored.ReconcileRestoreEvidence(f.evidence(t), f.publicKey, "mika", journalVerifiedAt)
	if err != nil || result.HeadSequence != 0 || result.AlreadyVerified {
		t.Fatalf("result = %+v, %v", result, err)
	}
	state := assertStillQuarantined(t, restored)
	if !state.JournalCurrent || state.JournalEvidenceID == "" || !state.JournalVerifiedAt.Equal(journalVerifiedAt) {
		t.Fatalf("verification was not retained: %+v", state)
	}
}

func TestRecoveryJournalReplaysCredentialRevokedAfterSnapshot(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	if err := f.live.CreatePrincipal("restored-cli", RoleOperator, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.live.InsertToken("restored-token", "restored-cli", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	f.snapshot(t)
	if err := f.live.Journal("link", apiTokensJournalPrefix+"restored-cli", "revoked", "mika", "post-snapshot revocation"); err != nil {
		t.Fatal(err)
	}
	restored := f.restore(t)
	if n, _ := restored.LiveTokensFor("restored-cli"); n != 1 {
		t.Fatalf("precondition: live tokens = %d", n)
	}
	if _, err := restored.ReconcileRestoreEvidence(f.evidence(t), f.publicKey, "mika", journalVerifiedAt); err != nil {
		t.Fatal(err)
	}
	if n, _ := restored.LiveTokensFor("restored-cli"); n != 0 {
		t.Fatalf("revoked credential was restored: live tokens = %d", n)
	}
	assertStillQuarantined(t, restored)
}

func TestRecoveryJournalReplaysMemberRemovedAfterSnapshot(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	if err := f.live.RecordButlerMember("sam", RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.live.AdmitButlerMember("sam", "mika"); err != nil {
		t.Fatal(err)
	}
	f.snapshot(t)
	if err := f.live.Journal("member", "sam", "offboarded", "mika", "post-snapshot removal"); err != nil {
		t.Fatal(err)
	}
	restored := f.restore(t)
	if _, err := restored.ReconcileRestoreEvidence(f.evidence(t), f.publicKey, "mika", journalVerifiedAt); err != nil {
		t.Fatal(err)
	}
	member, err := restored.GetButlerMember("sam")
	if err != nil || member.Active() {
		t.Fatalf("removed member was restored: %+v, %v", member, err)
	}
	assertStillQuarantined(t, restored)
}

func TestRecoveryJournalRefusesIncompleteGapTamperMismatchAndStaleness(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	f.snapshot(t)
	if err := f.live.Journal("link", apiTokensJournalPrefix+"one", "revoked", "mika", "one"); err != nil {
		t.Fatal(err)
	}
	if err := f.live.Journal("link", apiTokensJournalPrefix+"two", "revoked", "mika", "two"); err != nil {
		t.Fatal(err)
	}
	good := f.evidence(t)
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"missing entry", resignEvidence(t, good, f.privateKey, func(e *RecoveryJournalEvidence) { e.Entries = e.Entries[:1] }), "bounds"},
		{"sequence gap", resignEvidence(t, good, f.privateKey, func(e *RecoveryJournalEvidence) { e.Entries[1].Sequence = 3 }), "head integrity"},
		{"tampered", func() []byte {
			b := append([]byte(nil), good...)
			return []byte(strings.Replace(string(b), "api-tokens:one", "api-tokens:evil", 1))
		}(), "signature"},
		{"snapshot mismatch", resignEvidence(t, good, f.privateKey, func(e *RecoveryJournalEvidence) { e.SnapshotSHA256 = strings.Repeat("0", 64) }), "does not bind"},
		{"outdated coverage", resignEvidence(t, good, f.privateKey, func(e *RecoveryJournalEvidence) { e.CoverageThrough = e.SourceFencedAt.Add(-time.Second) }), "stale"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restored, err := RestoreBackup(f.snapshotPath, filepath.Join(t.TempDir(), "restored.db"), tc.name, journalSnapshotAt)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if _, err := restored.ReconcileRestoreEvidence(tc.body, f.publicKey, "mika", journalVerifiedAt); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			state := assertStillQuarantined(t, restored)
			if state.JournalCurrent || state.JournalEvidenceID != "" {
				t.Fatalf("refused evidence changed state: %+v", state)
			}
		})
	}
}

func TestRecoveryJournalReconciliationIsIdempotentAndPersistsAcrossRestart(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	f.snapshot(t)
	body := f.evidence(t)
	restored := f.restore(t)
	first, err := restored.ReconcileRestoreEvidence(body, f.publicKey, "mika", journalVerifiedAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restored.ReconcileRestoreEvidence(body, f.publicKey, "mika", journalVerifiedAt.Add(time.Minute))
	if err != nil || !second.AlreadyVerified || second.EvidenceID != first.EvidenceID {
		t.Fatalf("repeat = %+v, %v; first=%+v", second, err, first)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state := assertStillQuarantined(t, reopened)
	if state.JournalEvidenceID != first.EvidenceID || !state.JournalVerifiedAt.Equal(journalVerifiedAt) {
		t.Fatalf("restart lost reconciliation: %+v", state)
	}
}

func TestRecoveryJournalRetryAfterReplayCheckpointInterruption(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	for _, member := range []struct{ name, role string }{{"mika", RoleOwner}, {"sam", RoleMember}} {
		if err := f.live.RecordButlerMember(member.name, member.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := f.live.AdmitButlerMember(member.name, "mika"); err != nil {
			t.Fatal(err)
		}
	}
	vault, err := f.live.OpenSecretVault(filepath.Join(t.TempDir(), "household.key"))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Create("sam", SecretInput{Scope: "personal", Label: "Recovery fixture", Value: "not-production-secret"})
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot(t)
	if err := f.live.Journal("member", "sam", "offboarded", "mika", "post-snapshot removal"); err != nil {
		t.Fatal(err)
	}
	body := f.evidence(t)
	_, _, evidenceID, err := decodeRecoveryJournalEvidence(body)
	if err != nil {
		t.Fatal(err)
	}
	restored := f.restore(t)
	interrupted := errors.New("synthetic process interruption after replay")
	restored.recoveryJournalTestHook = func() error { return interrupted }
	if _, err := restored.ReconcileRestoreEvidence(body, f.publicKey, "mika", journalVerifiedAt); !errors.Is(err, interrupted) {
		t.Fatalf("interrupted reconciliation error = %v", err)
	}
	state := assertStillQuarantined(t, restored)
	if state.JournalCurrent || state.JournalEvidenceID != "" || state.ReplayedAt.IsZero() {
		t.Fatalf("interrupted verification was partially accepted: %+v", state)
	}
	var auditCount, generatedCount int
	if err := restored.sql.QueryRow(`SELECT COUNT(*) FROM recovery_journal_reconciliations WHERE evidence_id=?`, evidenceID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := restored.sql.QueryRow(`SELECT COUNT(*) FROM deletion_journal WHERE recovery_evidence_id=?`, evidenceID).Scan(&generatedCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 0 || generatedCount != 1 {
		t.Fatalf("after interruption audit=%d generated suffix=%d, want 0 and 1", auditCount, generatedCount)
	}
	member, err := restored.GetButlerMember("sam")
	if err != nil || member.Active() {
		t.Fatalf("authority withdrawal did not commit before interruption: %+v, %v", member, err)
	}
	generation := member.Generation
	var secretState string
	if err := restored.sql.QueryRow(`SELECT state FROM household_secrets WHERE id=?`, secret.ID).Scan(&secretState); err != nil || secretState != "quarantine" {
		t.Fatalf("personal secret state = %q, %v", secretState, err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.ReconcileRestoreEvidence(body, f.publicKey, "mika", journalVerifiedAt.Add(time.Minute))
	if err != nil || result.AlreadyVerified {
		t.Fatalf("retry result = %+v, %v", result, err)
	}
	state = assertStillQuarantined(t, reopened)
	if !state.JournalCurrent || state.JournalEvidenceID != evidenceID {
		t.Fatalf("retry did not complete verification: %+v", state)
	}
	member, err = reopened.GetButlerMember("sam")
	if err != nil || member.Active() || member.Generation != generation {
		t.Fatalf("retry duplicated or restored authority effects: %+v, %v", member, err)
	}
	if err := reopened.sql.QueryRow(`SELECT COUNT(*) FROM deletion_journal WHERE recovery_evidence_id=?`, evidenceID).Scan(&generatedCount); err != nil || generatedCount != 1 {
		t.Fatalf("retry duplicated replay journal effects: count=%d, %v", generatedCount, err)
	}
}

func TestRecoveryJournalRejectsAuditCachedInNewRestore(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	if err := f.live.CreatePrincipal("restored-cli", RoleOperator, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.live.InsertToken("restored-token", "restored-cli", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	f.snapshot(t)
	body := f.evidence(t)
	first := f.restore(t)
	if _, err := first.ReconcileRestoreEvidence(body, f.publicKey, "mika", journalVerifiedAt); err != nil {
		t.Fatal(err)
	}
	firstState := assertStillQuarantined(t, first)
	verifiedBackup := filepath.Join(t.TempDir(), "verified-backup.db")
	if err := first.BackupTo(verifiedBackup); err != nil {
		t.Fatal(err)
	}
	second, err := RestoreBackup(verifiedBackup, filepath.Join(t.TempDir(), "new-restore.db"), "new recovery generation", journalSnapshotAt)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	before := assertStillQuarantined(t, second)
	if before.RestoreGeneration == firstState.RestoreGeneration || before.JournalCurrent || before.JournalEvidenceID != "" {
		t.Fatalf("new restore inherited verification projection: first=%+v second=%+v", firstState, before)
	}
	var audits int
	if err := second.sql.QueryRow(`SELECT COUNT(*) FROM recovery_journal_reconciliations`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("precondition: copied audit count=%d, %v", audits, err)
	}
	beforeTokens, err := second.LiveTokensFor("restored-cli")
	if err != nil || beforeTokens != 1 {
		t.Fatalf("precondition: live tokens=%d, %v", beforeTokens, err)
	}
	if _, err := second.ReconcileRestoreEvidence(body, f.publicKey, "mika", journalVerifiedAt.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "does not bind") {
		t.Fatalf("old evidence error = %v", err)
	}
	after := assertStillQuarantined(t, second)
	if after != before {
		t.Fatalf("stale evidence changed current recovery state: before=%+v after=%+v", before, after)
	}
	afterTokens, err := second.LiveTokensFor("restored-cli")
	if err != nil || afterTokens != beforeTokens {
		t.Fatalf("stale evidence changed authorization: before=%d after=%d err=%v", beforeTokens, afterTokens, err)
	}
}

func TestRecoveryJournalCachedAuditRequiresCurrentRestoreProjection(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	f.snapshot(t)
	body := f.evidence(t)
	restored := f.restore(t)
	if _, err := restored.ReconcileRestoreEvidence(body, f.publicKey, "mika", journalVerifiedAt); err != nil {
		t.Fatal(err)
	}
	verified := assertStillQuarantined(t, restored)
	// Entering a new generation with the same snapshot identity isolates the
	// cache check from digest/timestamp rejection. The immutable old audit is
	// still present, but its projection was correctly cleared.
	if err := restored.enterRestoreQuarantine("same artifact restored again", verified.SnapshotAt, verified.SnapshotSHA256); err != nil {
		t.Fatal(err)
	}
	before := assertStillQuarantined(t, restored)
	if before.RestoreGeneration == verified.RestoreGeneration {
		t.Fatal("new restore reused the previous generation")
	}
	if _, err := restored.ReconcileRestoreEvidence(body, f.publicKey, "mika", journalVerifiedAt.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "cached verification") {
		t.Fatalf("stale cached audit error = %v", err)
	}
	after := assertStillQuarantined(t, restored)
	if after != before || after.JournalCurrent || after.JournalEvidenceID != "" || !after.ReplayedAt.IsZero() {
		t.Fatalf("stale cached audit changed current restore: before=%+v after=%+v", before, after)
	}
}

func TestRecoveryJournalRejectsUntrustedCustodian(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	f.snapshot(t)
	restored := f.restore(t)
	untrusted, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.ReconcileRestoreEvidence(f.evidence(t), untrusted, "mika", journalVerifiedAt); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("untrusted custodian error = %v", err)
	}
	state := assertStillQuarantined(t, restored)
	if state.JournalCurrent || state.JournalEvidenceID != "" {
		t.Fatalf("untrusted evidence changed verification state: %+v", state)
	}
}

func TestRecoveryJournalRejectsUnprovenLocalSuffix(t *testing.T) {
	f := newRecoveryJournalFixture(t)
	f.snapshot(t)
	restored := f.restore(t)
	if _, err := restored.sql.Exec(`INSERT INTO deletion_journal(subject_kind,subject_id,action,at,actor,detail)
		VALUES('member','divergent-member','offboarded',?,'mika','not produced by recovery replay')`, timeText(journalSnapshotAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.ReconcileRestoreEvidence(f.evidence(t), f.publicKey, "mika", journalVerifiedAt); err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("unproven local suffix error = %v", err)
	}
	state := assertStillQuarantined(t, restored)
	if state.JournalCurrent || state.JournalEvidenceID != "" || !state.ReplayedAt.IsZero() {
		t.Fatalf("divergent suffix changed recovery state: %+v", state)
	}
}

func TestRecoveryJournalResumeMigrationPreservesPriorEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-resume.db")
	db, err := openThrough(t, path, "migrations/0115_recovery_journal_evidence.sql")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.sql.Exec(`INSERT INTO restore_state(id,quarantined,entered_at,reason,snapshot_at,snapshot_sha256)
		VALUES(1,1,'t','migration fixture','t','snapshot')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO deletion_journal(subject_kind,subject_id,action,at,actor)
		VALUES('member','sam','offboarded','t','mika')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO recovery_journal_reconciliations
		(evidence_id,snapshot_sha256,snapshot_at,journal_source,custody,base_seq,head_seq,head_sha256,
		 coverage_through,source_fenced_at,signer_fingerprint,verified_at,verified_by,applied_count,evidence_json)
		VALUES('evidence','snapshot','t','source','custody',0,1,'head','t','t','signer','t','mika',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	var journalEvidence, stateGeneration, auditGeneration string
	if err := db.sql.QueryRow(`SELECT recovery_evidence_id FROM deletion_journal WHERE seq=1`).Scan(&journalEvidence); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT restore_generation FROM restore_state WHERE id=1`).Scan(&stateGeneration); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT restore_generation FROM recovery_journal_reconciliations WHERE evidence_id='evidence'`).Scan(&auditGeneration); err != nil {
		t.Fatal(err)
	}
	if journalEvidence != "" || stateGeneration != "" || auditGeneration != "" {
		t.Fatalf("migration invented provenance: journal=%q state=%q audit=%q", journalEvidence, stateGeneration, auditGeneration)
	}
}
