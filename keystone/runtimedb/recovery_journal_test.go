package runtimedb

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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
