package runtimedb

// Epic #326 S08 at the store: work a lease move fenced is withdrawn, not
// left claimed; offboarding erases a mailbox monitor's private state
// only after the fence, and a restore of an older snapshot erases it
// again before anyone can read it; a backup copy carries no mail answer
// and no excerpt; and a restored copy is quarantined before anything
// else touches it.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFencedWorkIsWithdrawnNotLeftClaimed(t *testing.T) {
	f := newCandidateFixture(t)
	if err := f.db.BindAdapterPrincipal("adapter-sam", "sam-mail", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AcquireCoordinator("fence-test"); err != nil {
		t.Fatal(err)
	}
	queue := func() int64 {
		t.Helper()
		seq, err := f.db.EnqueueIntegrationWork("sam-mail", IntegrationWork{Kind: WorkQuery, Capability: "mail.changes"}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	claimed := queue()
	if w, ok, err := f.db.ClaimIntegrationWork("adapter-sam"); err != nil || !ok || w.Seq != claimed {
		t.Fatalf("claim: %+v %v %v", w, ok, err)
	}
	waiting := queue()
	if _, err := f.db.BumpConnectionEpoch("sam-mail"); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{claimed, waiting} {
		w, err := f.db.GetIntegrationWork(seq)
		if err != nil || w.State != WorkCancelled || w.Error != FencedWorkReason {
			t.Fatalf("work %d after the fence = %s %q %v", seq, w.State, w.Error, err)
		}
	}
	// The fenced adapter's late answer is still refused, and lands nothing.
	if err := f.db.ReportIntegrationResult("adapter-sam", claimed, ResultOK, `{"messages":[]}`, time.Time{}, ""); !errors.Is(err, ErrAttemptFenced) {
		t.Fatalf("late answer = %v, want fenced", err)
	}

	// A path that moves the epoch without withdrawing (any future one) is
	// covered by the sweep and by the next claim.
	orphan := queue()
	if _, _, err := f.db.ClaimIntegrationWork("adapter-sam"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.sql.Exec(`UPDATE integration_connections SET lease_epoch = lease_epoch + 1 WHERE name = 'sam-mail'`); err != nil {
		t.Fatal(err)
	}
	if n, err := f.db.ExpireIntegrationWork(); err != nil || n != 1 {
		t.Fatalf("sweep withdrew %d, %v", n, err)
	}
	if w, _ := f.db.GetIntegrationWork(orphan); w.State != WorkCancelled {
		t.Fatalf("orphaned claimed work = %s", w.State)
	}
}

// mailRows counts every row of a connection's private mail state.
func mailRows(t *testing.T, db *DB, connection string) map[string]int {
	t.Helper()
	of := `(SELECT id FROM mail_candidates WHERE connection = (SELECT seq FROM integration_connections WHERE name = ?))`
	out := map[string]int{}
	for name, q := range map[string]string{
		"candidates":  `SELECT COUNT(*) FROM mail_candidates WHERE id IN ` + of,
		"revisions":   `SELECT COUNT(*) FROM mail_candidate_revisions WHERE candidate IN ` + of,
		"reviews":     `SELECT COUNT(*) FROM mail_reviews WHERE candidate IN ` + of,
		"tokens":      `SELECT COUNT(*) FROM mail_review_tokens WHERE review IN (SELECT id FROM mail_reviews WHERE candidate IN ` + of + `)`,
		"excerpts":    `SELECT COUNT(*) FROM mail_candidate_excerpts WHERE candidate IN ` + of,
		"audit":       `SELECT COUNT(*) FROM mail_candidate_audit WHERE candidate IN ` + of,
		"messages":    `SELECT COUNT(*) FROM mail_monitor_messages WHERE connection = (SELECT seq FROM integration_connections WHERE name = ?)`,
		"checkpoints": `SELECT COUNT(*) FROM agent_subscription_checkpoints WHERE source = ?`,
	} {
		out[name] = count(t, db, q, connection)
	}
	return out
}

func TestOffboardingErasesMailStateOnlyAfterTheFence(t *testing.T) {
	f := newCandidateFixture(t)
	c := onlyNew(t, f.record(t, "mfp1_school", draft(schoolNight())))
	f.issue(t, c.ID)
	sub := f.sub(t)
	if _, err := f.db.sql.Exec(`INSERT INTO agent_subscription_checkpoints (subscription, source, version, cursor_json, committed_at, committed_by_run)
		VALUES (?, 'sam-mail', 1, '{"folders":[{"folder":"INBOX","uidValidity":7,"lastUid":42}]}', ?, 1)`,
		sub, mailNow.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	before := mailRows(t, f.db, "sam-mail")
	for _, k := range []string{"candidates", "reviews", "tokens", "excerpts", "messages", "checkpoints"} {
		if before[k] == 0 {
			t.Fatalf("fixture has no %s: %v", k, before)
		}
	}
	// While sam's authorization is live, nothing is erased: the fence
	// comes first.
	if _, err := f.db.EraseMailConnectionState("sam-mail", "mika", "tidying up", mailNow); !errors.Is(err, ErrMailConnectionLive) {
		t.Fatalf("erasing a live monitor = %v", err)
	}
	snapshot := filepath.Join(t.TempDir(), "before.db")
	if err := f.db.BackupTo(snapshot); err != nil {
		t.Fatal(err)
	}

	res, err := f.db.OffboardMember("sam", "mika", "mika")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stores["mail-state-erased"] == 0 {
		t.Fatalf("offboarding reported no mail state erased: %v", res.Stores)
	}
	for k, n := range mailRows(t, f.db, "sam-mail") {
		if n != 0 {
			t.Errorf("%d %s survived offboarding", n, k)
		}
	}
	conn, _ := f.db.GetConnection("sam-mail")
	if conn.LeaseEpoch <= f.epoch {
		t.Fatal("offboarding erased without fencing")
	}
	var disposition string
	if err := f.db.sql.QueryRow(`SELECT disposition FROM offboarding_resource_dispositions
		WHERE resource_kind = 'mail-monitor' AND resource_ref = 'sam-mail'`).Scan(&disposition); err != nil || disposition != "deleted" {
		t.Fatalf("disposition %q %v", disposition, err)
	}
	var journal [][3]string
	rows, err := f.db.sql.Query(`SELECT subject_kind, subject_id, action FROM deletion_journal`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var e [3]string
		rows.Scan(&e[0], &e[1], &e[2])
		journal = append(journal, e)
	}
	rows.Close()

	// A snapshot from before the offboarding comes back. It is
	// quarantined, then the journal (current, from the newer source) is
	// replayed: sam's mailbox state is gone again before release.
	restored, err := RestoreBackup(snapshot, filepath.Join(t.TempDir(), "restored.db"), "restored from before sam left", mailNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.Close() })
	if err := restored.CheckQuarantine(); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("restored copy not quarantined: %v", err)
	}
	if n := mailRows(t, restored, "sam-mail")["candidates"]; n == 0 {
		t.Fatal("the snapshot had no candidate to replay over")
	}
	if n := count(t, restored, `SELECT COUNT(*) FROM mail_reviews WHERE state = 'open'`); n != 0 {
		t.Fatal("a review restored open")
	}
	for _, e := range journal {
		if _, err := restored.ExecForTest(`INSERT INTO deletion_journal (subject_kind, subject_id, action, actor, at)
			VALUES (?, ?, ?, 'mika', ?)`, e[0], e[1], e[2], time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if err := restored.AssertJournalCurrent("mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.ReconcileRestore(); err != nil {
		t.Fatal(err)
	}
	for k, n := range mailRows(t, restored, "sam-mail") {
		if n != 0 {
			t.Errorf("%d %s came back with the restore", n, k)
		}
	}
	if m, _ := restored.GetButlerMember("sam"); m.Active() {
		t.Fatal("sam was re-admitted by the restore")
	}
}

func TestBackupCopyHoldsNoMailAnswerOrExcerpt(t *testing.T) {
	f := newCandidateFixture(t)
	onlyNew(t, f.record(t, "mfp1_school", draft(schoolNight())))
	seq, err := f.db.EnqueueIntegrationWork("sam-mail", IntegrationWork{Kind: WorkQuery, Capability: "mail.changes"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// An answer the collector has not processed yet: done, not scrubbed.
	const body = "REPLACED-full-body-of-a-private-letter"
	if _, err := f.db.sql.Exec(`UPDATE integration_work SET state = 'done', result_json = ? WHERE seq = ?`,
		`{"messages":[{"content":{"text":"`+body+`"}}]}`, seq); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "keystone.db")
	if err := f.db.BackupTo(dest); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(body)) {
		t.Fatal("the backup copy holds a mail answer")
	}
	copyDB, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	if n := count(t, copyDB, `SELECT COUNT(*) FROM mail_candidate_excerpts`); n != 0 {
		t.Fatalf("the backup copy holds %d sealed excerpts", n)
	}
	if n := count(t, copyDB, `SELECT COUNT(*) FROM mail_candidates`); n != 1 {
		t.Fatalf("the backup copy lost the candidate itself (%d)", n)
	}
	// The live database is untouched: the collector still gets its answer.
	if w, _ := f.db.GetIntegrationWork(seq); !strings.Contains(w.ResultJSON, body) {
		t.Fatal("backing up scrubbed the live queue")
	}
	// The excerpt key is not in the database, and no path into a backup
	// names it: it lives beside the database file.
	if strings.HasPrefix(f.key, filepath.Dir(dest)) {
		t.Fatal("fixture error: the key sits in the backup directory")
	}
}
