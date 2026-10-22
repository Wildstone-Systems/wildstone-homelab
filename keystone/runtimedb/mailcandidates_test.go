package runtimedb

// Epic #326 S05's evidence at the store: processed mail dedupes by keyed
// fingerprint, candidates are versioned and Person-private, a correction
// supersedes what it corrects (or marks a created event for an update
// proposal), the excerpt is sealed, expires and never survives a
// resolution, a revocation or a restore, and no path other than the
// owning Person's reads anything.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var mailNow = time.Date(2026, 10, 5, 13, 15, 0, 0, time.UTC)

type candidateFixture struct {
	*mailFixture
	vault  *MailExcerptVault
	key    string
	person string
	epoch  int64
}

func newCandidateFixture(t *testing.T) *candidateFixture {
	t.Helper()
	f := newMailFixture(t)
	f.authorize(t)
	key := filepath.Join(t.TempDir(), "mail-excerpts.key")
	v, err := f.db.OpenMailExcerptVault(key)
	if err != nil {
		t.Fatal(err)
	}
	sam, err := f.db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := f.db.GetConnection("sam-mail")
	if err != nil {
		t.Fatal(err)
	}
	return &candidateFixture{mailFixture: f, vault: v, key: key, person: sam.PersonID, epoch: conn.LeaseEpoch}
}

func schoolNight() MailCandidateFields {
	ny, _ := time.LoadLocation("America/New_York")
	return MailCandidateFields{
		Title: "School family night", Location: "School gym", TZ: "America/New_York",
		Start: time.Date(2026, 10, 15, 18, 0, 0, 0, ny), End: time.Date(2026, 10, 15, 19, 30, 0, 0, ny),
		Confidence: 0.9, Missing: []string{}, Ambiguous: []string{},
	}
}

func (f *candidateFixture) record(t *testing.T, fp string, d ...MailCandidateDraft) MailRecordResult {
	t.Helper()
	res, err := f.db.RecordMailMessage(f.vault, MailMessageRecord{
		Connection: "sam-mail", Epoch: f.epoch, PersonID: f.person, Fingerprint: fp, SourceRef: "ref-" + fp,
		TargetCalendar: "sam-family", State: MailMessageNoEvent, Drafts: d, At: mailNow, Source: MailSourceModel,
	})
	if err != nil {
		t.Fatalf("RecordMailMessage(%s): %v", fp, err)
	}
	return res
}

func draft(f MailCandidateFields) MailCandidateDraft {
	return MailCandidateDraft{Fields: f, Source: MailSourceModel, MessageKey: "m1", SenderKey: "s1",
		SubjectKey: "subj", Excerpt: "The school family night is Thursday, October 15, 2026 from 6:00 to 7:30 p.m."}
}

func onlyNew(t *testing.T, res MailRecordResult) MailCandidate {
	t.Helper()
	for _, c := range res.Changes {
		if c.Kind == MailChangeNew {
			return c.Candidate
		}
	}
	t.Fatalf("no new candidate in %+v", res)
	return MailCandidate{}
}

func TestMailMessageDedupesByFingerprint(t *testing.T) {
	f := newCandidateFixture(t)
	first := f.record(t, "mfp1_aaa", draft(schoolNight()))
	c := onlyNew(t, first)
	if first.State != MailMessageCandidate || c.State != MailCandidateProposed || c.Revision != 1 ||
		c.TargetCalendar != "sam-family" || c.PersonID != f.person || c.Connection != "sam-mail" {
		t.Fatalf("first record: %+v / %+v", first, c)
	}
	again := f.record(t, "mfp1_aaa", draft(schoolNight()))
	if !again.Duplicate || len(again.Changes) != 0 {
		t.Fatalf("replayed fingerprint wrote again: %+v", again)
	}
	// A different message announcing the same event is a duplicate event,
	// not a second question.
	resend := f.record(t, "mfp1_bbb", draft(schoolNight()))
	if resend.State != MailMessageDuplicateEvent || len(resend.Changes) != 0 {
		t.Fatalf("resend: %+v", resend)
	}
	list, err := f.db.ListMailCandidates("sam", MailCandidateFilter{})
	if err != nil || len(list) != 1 {
		t.Fatalf("sam's candidates = %d (%v), want 1", len(list), err)
	}
}

func TestMailCandidatesAreVisibleOnlyToTheirPerson(t *testing.T) {
	f := newCandidateFixture(t)
	c := onlyNew(t, f.record(t, "mfp1_aaa", draft(schoolNight())))
	if _, err := f.db.GetMailCandidate("sam", c.ID); err != nil {
		t.Fatalf("sam cannot read her own candidate: %v", err)
	}
	// bo is another Member, mika an Owner, hobson the very Agent that
	// runs the monitor, root-admin a machine principal.
	for _, who := range []string{"bo", "mika", "hobson", "root-admin", "", "sam-mail"} {
		if _, err := f.db.GetMailCandidate(who, c.ID); !errors.Is(err, ErrMailCandidateNotFound) {
			t.Errorf("%q read sam's candidate: %v", who, err)
		}
		if list, err := f.db.ListMailCandidates(who, MailCandidateFilter{}); err != nil || len(list) != 0 {
			t.Errorf("%q listed %d of sam's candidates (%v)", who, len(list), err)
		}
		if _, err := f.db.GetMailCandidateRevision(who, c.ID, 1); !errors.Is(err, ErrMailCandidateNotFound) {
			t.Errorf("%q read a revision: %v", who, err)
		}
		if s, err := f.db.ReadMailCandidateExcerpt(f.vault, who, c.ID, mailNow); s != "" || !errors.Is(err, ErrMailCandidateNotFound) {
			t.Errorf("%q read the excerpt: %q %v", who, s, err)
		}
		if _, err := f.db.ReviseMailCandidate(who, c.ID, 1, schoolNight(), mailNow); !errors.Is(err, ErrMailCandidateNotFound) {
			t.Errorf("%q edited: %v", who, err)
		}
		for _, to := range []string{MailCandidateAccepted, MailCandidateDismissed} {
			if _, err := f.db.TransitionMailCandidate(who, c.ID, 1, to, "", mailNow); !errors.Is(err, ErrMailCandidateNotFound) {
				t.Errorf("%q moved it to %s: %v", who, to, err)
			}
		}
		if err := f.db.ClearMailCandidateUpdate(who, c.ID, mailNow); !errors.Is(err, ErrMailCandidateNotFound) {
			t.Errorf("%q cleared an update: %v", who, err)
		}
	}
	// Nothing anyone tried changed it.
	if got, _ := f.db.GetMailCandidate("sam", c.ID); got.State != MailCandidateProposed || got.Revision != 1 {
		t.Fatalf("after denied attempts: %s rev %d", got.State, got.Revision)
	}
	// A disabled Person reads nothing either, not even her own.
	if err := f.db.DisableButlerMember("sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.GetMailCandidate("sam", c.ID); !errors.Is(err, ErrMailCandidateNotFound) {
		t.Fatalf("disabled sam read a candidate: %v", err)
	}
}

func TestMailRecordIsFencedByEpochAndOwner(t *testing.T) {
	f := newCandidateFixture(t)
	bo, _ := f.db.GetButlerMember("bo")
	for name, rec := range map[string]MailMessageRecord{
		"stale epoch":  {Epoch: f.epoch - 1, PersonID: f.person},
		"other Person": {Epoch: f.epoch, PersonID: bo.PersonID},
	} {
		rec.Connection, rec.Fingerprint, rec.At, rec.TargetCalendar = "sam-mail", "mfp1_x", mailNow, "sam-family"
		rec.Drafts = []MailCandidateDraft{draft(schoolNight())}
		if _, err := f.db.RecordMailMessage(f.vault, rec); !errors.Is(err, ErrMailConnectionFenced) {
			t.Errorf("%s: %v, want fenced", name, err)
		}
	}
	if state, _, seen, _ := f.db.MailMessageState("sam-mail", "mfp1_x"); seen {
		t.Fatalf("a fenced record left state %q", state)
	}
}

func TestMailExcerptIsSealedAndShortLived(t *testing.T) {
	f := newCandidateFixture(t)
	c := onlyNew(t, f.record(t, "mfp1_aaa", draft(schoolNight())))
	var sealed []byte
	if err := f.db.sql.QueryRow(`SELECT sealed FROM mail_candidate_excerpts WHERE candidate = ?`, c.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("family night")) {
		t.Fatal("the excerpt is stored in the clear")
	}
	got, err := f.db.ReadMailCandidateExcerpt(f.vault, "sam", c.ID, mailNow)
	if err != nil || got != draft(schoolNight()).Excerpt {
		t.Fatalf("sam's excerpt = %q, %v", got, err)
	}
	// A vault opened under a different key (a restored or re-keyed data
	// directory) cannot read it and discards it.
	if _, err := f.db.OpenMailExcerptVault(filepath.Join(t.TempDir(), "other.key")); err != nil {
		t.Fatal(err)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM mail_candidate_excerpts`); n != 0 {
		t.Fatalf("%d excerpts survived a new key", n)
	}
	// And a corrupt key file is refused rather than silently replaced.
	bad := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(bad, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.OpenMailExcerptVault(bad); err == nil {
		t.Fatal("a malformed excerpt key was accepted")
	}
}

func count(t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMailCandidateLifecycle(t *testing.T) {
	f := newCandidateFixture(t)
	vague := schoolNight()
	vague.Start, vague.End, vague.TZ = time.Time{}, time.Time{}, ""
	vague.Missing = []string{MailFieldStart}
	nd := onlyNew(t, f.record(t, "mfp1_vague", func() MailCandidateDraft {
		d := draft(vague)
		d.SubjectKey = "other"
		return d
	}()))
	if nd.State != MailCandidateNeedsDetails {
		t.Fatalf("missing start: state %s, want needs_details", nd.State)
	}
	if _, err := f.db.TransitionMailCandidate("sam", nd.ID, 1, MailCandidateAccepted, "", mailNow); !errors.Is(err, ErrMailCandidateStale) {
		t.Fatalf("accepting needs_details: %v", err)
	}
	// Edit supplies the time: a new revision, now proposed.
	edited, err := f.db.ReviseMailCandidate("sam", nd.ID, 1, schoolNight(), mailNow)
	if err != nil || edited.Revision != 2 || edited.State != MailCandidateProposed {
		t.Fatalf("revise: %+v %v", edited, err)
	}
	if _, err := f.db.ReviseMailCandidate("sam", nd.ID, 1, schoolNight(), mailNow); !errors.Is(err, ErrMailCandidateStale) {
		t.Fatalf("editing a stale revision: %v", err)
	}
	if old, err := f.db.GetMailCandidateRevision("sam", nd.ID, 1); err != nil || !old.Start.IsZero() {
		t.Fatalf("revision 1 changed: %+v %v", old, err)
	}
	if _, err := f.db.TransitionMailCandidate("sam", nd.ID, 1, MailCandidateAccepted, "", mailNow); !errors.Is(err, ErrMailCandidateStale) {
		t.Fatalf("accepting a superseded revision: %v", err)
	}
	acc, err := f.db.TransitionMailCandidate("sam", nd.ID, 2, MailCandidateAccepted, "approved", mailNow)
	if err != nil || acc.State != MailCandidateAccepted {
		t.Fatalf("accept: %+v %v", acc, err)
	}
	created, err := f.db.TransitionMailCandidate("sam", nd.ID, 2, MailCandidateCreated, "event written", mailNow)
	if err != nil || created.State != MailCandidateCreated || created.ResolvedAt.IsZero() {
		t.Fatalf("create: %+v %v", created, err)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM mail_candidate_excerpts WHERE candidate = ?`, nd.ID); n != 0 {
		t.Fatal("a created candidate kept its excerpt")
	}

	// Dismiss keeps only the fingerprint.
	other := schoolNight()
	other.Title = "Bake sale"
	d := onlyNew(t, f.record(t, "mfp1_bake", draft(other)))
	if _, err := f.db.TransitionMailCandidate("sam", d.ID, 1, MailCandidateDismissed, "", mailNow); err != nil {
		t.Fatal(err)
	}
	got, _ := f.db.GetMailCandidate("sam", d.ID)
	if got.State != MailCandidateDismissed || got.Fields.Title != "" || got.Fields.Location != "" || got.Fingerprint != "mfp1_bake" {
		t.Fatalf("dismissed kept %+v", got)
	}
	// Dismissed means "do not ask again", including for a reminder.
	if res := f.record(t, "mfp1_bake_reminder", draft(other)); res.State != MailMessageDuplicateEvent || len(res.Changes) != 0 {
		t.Fatalf("a reminder of a dismissed event asked again: %+v", res)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM mail_candidate_audit WHERE candidate = ?`, d.ID); n != 2 {
		t.Fatalf("audit rows = %d, want create + resolve", n)
	}
}

func TestMailCorrectionSupersedesOrMarksAnUpdate(t *testing.T) {
	f := newCandidateFixture(t)
	orig := onlyNew(t, f.record(t, "mfp1_orig", draft(schoolNight())))

	moved := schoolNight()
	moved.Start = moved.Start.Add(30 * time.Minute)
	moved.End = moved.End.Add(30 * time.Minute)
	corr := draft(moved)
	corr.Correction, corr.MessageKey, corr.ThreadKeys = true, "m2", []string{"m1"}
	res := f.record(t, "mfp1_corr", corr)
	next := onlyNew(t, res)
	prior, _ := f.db.GetMailCandidate("sam", orig.ID)
	if prior.State != MailCandidateSuperseded || prior.SupersededBy != next.ID || next.Supersedes != orig.ID {
		t.Fatalf("correction: prior %s by %q, next supersedes %q", prior.State, prior.SupersededBy, next.Supersedes)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM mail_candidate_excerpts WHERE candidate = ?`, orig.ID); n != 0 {
		t.Fatal("a superseded candidate kept its excerpt")
	}

	// A stranger's "correction" in the same thread is not one.
	fake := draft(moved)
	fake.Fields.Start = fake.Fields.Start.Add(time.Hour)
	fake.Fields.End = fake.Fields.End.Add(time.Hour)
	fake.Correction, fake.SenderKey, fake.ThreadKeys = true, "stranger", []string{"m2"}
	f.record(t, "mfp1_fake", fake)
	if still, _ := f.db.GetMailCandidate("sam", next.ID); still.State != MailCandidateProposed {
		t.Fatalf("a stranger superseded sam's candidate: %s", still.State)
	}

	// Once the event is created, a correction marks it for an update
	// proposal and never edits it.
	if _, err := f.db.TransitionMailCandidate("sam", next.ID, 1, MailCandidateAccepted, "", mailNow); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.TransitionMailCandidate("sam", next.ID, 1, MailCandidateCreated, "", mailNow); err != nil {
		t.Fatal(err)
	}
	again := schoolNight()
	again.Location = "Auditorium"
	corr2 := draft(again)
	corr2.Correction, corr2.MessageKey, corr2.ThreadKeys = true, "m3", []string{"m2"}
	res = f.record(t, "mfp1_corr2", corr2)
	var marked bool
	for _, c := range res.Changes {
		if c.Kind == MailChangeUpdateNeeded && c.Candidate.ID == next.ID && c.Candidate.UpdatePending == MailUpdateEvent {
			marked = true
		}
	}
	created, _ := f.db.GetMailCandidate("sam", next.ID)
	if !marked || created.State != MailCandidateCreated || created.UpdatePending != MailUpdateEvent ||
		!created.Fields.Start.Equal(moved.Start) {
		t.Fatalf("correction of a created event: %+v (changes %+v)", created, res.Changes)
	}
	pending, _ := f.db.UnannouncedMailCandidates("sam-mail", 0)
	var offered bool
	for _, c := range pending {
		if c.ID == next.ID {
			offered = true
		}
	}
	if !offered {
		t.Fatal("the update-needed candidate is not offered to the review hook")
	}
}

func TestMailCancellation(t *testing.T) {
	f := newCandidateFixture(t)
	inv := draft(schoolNight())
	inv.Source, inv.UIDKey, inv.ICSSequence = MailSourceICS, "uid-1", 0
	c := onlyNew(t, f.record(t, "mfp1_inv", inv))
	stranger := MailCandidateDraft{Source: MailSourceICS, UIDKey: "uid-1", Cancel: true, SenderKey: "stranger"}
	if res := f.record(t, "mfp1_x", stranger); res.State != MailMessageSkipped || res.Reason != "cancel_sender_mismatch" {
		t.Fatalf("stranger's cancel: %+v", res)
	}
	cancel := MailCandidateDraft{Source: MailSourceICS, UIDKey: "uid-1", ICSSequence: 1, Cancel: true, SenderKey: "s1"}
	if res := f.record(t, "mfp1_cancel", cancel); res.State != MailMessageCancelled {
		t.Fatalf("cancel: %+v", res)
	}
	if got, _ := f.db.GetMailCandidate("sam", c.ID); got.State != MailCandidateCancelled {
		t.Fatalf("after cancel: %s", got.State)
	}
}

func TestMailSweepExpiresRevokesAndScrubs(t *testing.T) {
	f := newCandidateFixture(t)
	c := onlyNew(t, f.record(t, "mfp1_aaa", draft(schoolNight())))
	// The candidate expires at its own start.
	if !c.ExpiresAt.Equal(schoolNight().Start) {
		t.Fatalf("expires %s, want the event start", c.ExpiresAt)
	}
	out, err := f.db.SweepMailMonitor(schoolNight().Start.Add(time.Minute), MailRetention{})
	if err != nil || out.Expired != 1 {
		t.Fatalf("sweep at start: %+v %v", out, err)
	}
	if got, _ := f.db.GetMailCandidate("sam", c.ID); got.State != MailCandidateExpired {
		t.Fatalf("after its start: %s", got.State)
	}

	// Revocation purges what is unresolved.
	other := schoolNight()
	other.Title = "Bake sale"
	d := onlyNew(t, f.record(t, "mfp1_bake", draft(other)))
	if err := f.db.RevokeBackgroundAuthorization("sam-mail", "sam", "stop watching"); err != nil {
		t.Fatal(err)
	}
	out, err = f.db.SweepMailMonitor(mailNow, MailRetention{})
	if err != nil || out.Revoked != 1 {
		t.Fatalf("sweep after revocation: %+v %v", out, err)
	}
	if got, _ := f.db.GetMailCandidate("sam", d.ID); got.State != MailCandidateExpired {
		t.Fatalf("after revocation: %s", got.State)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM mail_candidate_excerpts`); n != 0 {
		t.Fatalf("%d excerpts survived revocation", n)
	}

	// A mail read's answer is scrubbed from the work queue.
	seq, err := f.db.EnqueueIntegrationWork("sam-mail", IntegrationWork{Kind: WorkQuery, Capability: "mail.changes"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.sql.Exec(`UPDATE integration_work SET state = 'done', result_json = '{"messages":[{"content":{"text":"secret body"}}]}',
		finished_at = ? WHERE seq = ?`, mailNow.Add(-time.Hour).Format(time.RFC3339Nano), seq); err != nil {
		t.Fatal(err)
	}
	out, err = f.db.SweepMailMonitor(mailNow, MailRetention{})
	if err != nil || out.WorkScrubbed != 1 {
		t.Fatalf("scrub: %+v %v", out, err)
	}
	if w, _ := f.db.GetIntegrationWork(seq); w.ResultJSON != "{}" {
		t.Fatalf("mail answer survived: %s", w.ResultJSON)
	}

	// Resolved candidates go after the retention window; message state
	// stays and still dedupes.
	out, err = f.db.SweepMailMonitor(mailNow.Add(60*24*time.Hour), MailRetention{})
	if err != nil || out.CandidatesDeleted != 2 {
		t.Fatalf("retention: %+v %v", out, err)
	}
	if state, _, seen, err := f.db.MailMessageState("sam-mail", "mfp1_aaa"); !seen || state != MailMessageCandidate {
		t.Fatalf("dedupe state was lost with the candidate: %q %v %v", state, seen, err)
	}
}

func TestRestoreDropsMailExcerptsAndReadResults(t *testing.T) {
	f := newCandidateFixture(t)
	onlyNew(t, f.record(t, "mfp1_aaa", draft(schoolNight())))
	seq, err := f.db.EnqueueIntegrationWork("sam-mail", IntegrationWork{Kind: WorkQuery, Capability: "mail.changes"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.sql.Exec(`UPDATE integration_work SET result_json = '{"text":"secret body"}' WHERE seq = ?`, seq); err != nil {
		t.Fatal(err)
	}
	if err := purgeMailOnRestore(f.db); err != nil {
		t.Fatal(err)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM mail_candidate_excerpts`); n != 0 {
		t.Fatal("an excerpt survived restore")
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM integration_work WHERE result_json LIKE '%secret body%'`); n != 0 {
		t.Fatal("a mail answer survived restore")
	}
}
