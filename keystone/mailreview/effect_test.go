package mailreview

// At most one event, whatever the network or the process does: a write
// that fails before it is stored, one stored and answered too late, one
// stored whose connection drops, a crash between the approval and the
// write, a crash after the write, and a UID already holding the event.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func onlyOneEvent(t *testing.T, e *env, id string) {
	t.Helper()
	objs := e.dav.CandidateObjects()
	if len(objs) != 1 || objs[0] != "/cal/sam-family/wildstone-candidate-"+id+".ics" {
		t.Fatalf("calendar holds %v, want exactly the one event", objs)
	}
	if got := e.candidate(t, id); got.State != runtimedb.MailCandidateCreated {
		t.Fatalf("candidate %s, want created", got.State)
	}
	if ex, _ := e.db.ReadMailCandidateExcerpt(e.vault, "sam", id, e.clock.Now()); ex != "" {
		t.Fatal("excerpt survived")
	}
}

func TestIndeterminateWritesAreReconciledByUID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		faults  []string
		effect  string
		maxPuts int
	}{
		// The first PUT never stored anything: the lookup finds nothing,
		// and only then is it sent once more.
		{"timeout before send", []string{"fail-before"}, runtimedb.MailEffectConfirmed, 2},
		// Stored, then answered after the client gave up.
		{"timeout after send", []string{"hang-after"}, runtimedb.MailEffectReconciled, 1},
		// Stored, then the response was lost with the connection.
		{"response lost", []string{"drop-after"}, runtimedb.MailEffectReconciled, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			c := e.record(t, "mfp1_a", draft(schoolNight()))
			_, tokens := e.tokens(t, c.ID)
			e.dav.Schedule(tc.faults...)
			res, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]})
			if err != nil || res.Outcome != OutcomeAdded || res.Effect == nil || res.Effect.State != tc.effect {
				t.Fatalf("add: %+v %v", res, err)
			}
			onlyOneEvent(t, e, c.ID)
			if n := e.dav.PutCount(); n > tc.maxPuts {
				t.Fatalf("%d PUTs, want at most %d", n, tc.maxPuts)
			}
			// A duplicate approval afterwards still makes nothing.
			if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); err == nil {
				t.Fatal("duplicate approval honoured")
			}
			onlyOneEvent(t, e, c.ID)
		})
	}
}

func TestAnUnanswerableLookupLeavesTheEffectForTheReconciler(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, tokens := e.tokens(t, c.ID)
	// Stored and answered too late, and then the calendar will not say
	// whether it has it.
	e.dav.Schedule("hang-after")
	e.dav.FailGets(1)
	res, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]})
	if err != nil || res.Outcome != OutcomePending {
		t.Fatalf("add: %+v %v", res, err)
	}
	if got := e.candidate(t, c.ID); got.State != runtimedb.MailCandidateAccepted {
		t.Fatalf("candidate %s while the write is unsettled", got.State)
	}
	// Nothing retries it blind; nothing edits it meanwhile.
	edit := schoolNight()
	if _, err := e.act(ActRequest{Actor: "sam", Candidate: c.ID, Revision: 1, Action: "edit", Fields: &edit}); err == nil {
		t.Fatal("an edit was accepted while the write is in flight")
	}
	// Too soon for the reconciler: it leaves a live write alone.
	if err := e.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.candidate(t, c.ID); got.State != runtimedb.MailCandidateAccepted {
		t.Fatalf("reconciled too early: %s", got.State)
	}
	e.clock.advance(3 * time.Minute)
	if err := e.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	onlyOneEvent(t, e, c.ID)
	if e.dav.PutCount() != 1 {
		t.Fatalf("%d PUTs", e.dav.PutCount())
	}
	effects, _ := e.db.ListMailEffects("sam", c.ID)
	if len(effects) != 1 || effects[0].State != runtimedb.MailEffectReconciled {
		t.Fatalf("effects %+v", effects)
	}
}

// claimOnly performs the approval's transaction and "crashes" before
// the write is sent.
func claimOnly(t *testing.T, e *env, id, token string) runtimedb.MailReviewClaimed {
	t.Helper()
	g, err := e.eng.liveGrant(context.Background(), "sam-mail")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := e.db.ClaimMailReview(runtimedb.MailReviewClaim{Actor: "sam", Token: token, Via: "console", Grant: &g, At: e.clock.Now()})
	if err != nil || claimed.Effect.State != runtimedb.MailEffectDispatching {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	return claimed
}

func TestACrashBeforeTheWriteGoesBackToReview(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, tokens := e.tokens(t, c.ID)
	claimOnly(t, e, c.ID, tokens["add"])
	// The restarted process: the old token is spent, and a second click
	// cannot start a second write.
	if _, err := e.act(ActRequest{Actor: "sam", Candidate: c.ID, Revision: 1, Action: "add"}); err == nil {
		t.Fatal("a second approval raced the in-flight write")
	}
	e.clock.advance(3 * time.Minute)
	if err := e.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	noEvent(t, e)
	got := e.candidate(t, c.ID)
	r, fresh := e.tokens(t, c.ID)
	if got.State != runtimedb.MailCandidateProposed || r.State != runtimedb.MailReviewOpen || fresh["add"] == "" || fresh["add"] == tokens["add"] {
		t.Fatalf("after a crash before the write: candidate %s review %+v", got.State, r)
	}
	if res, err := e.act(ActRequest{Actor: "sam", Token: fresh["add"]}); err != nil || res.Outcome != OutcomeAdded {
		t.Fatalf("re-approval: %+v %v", res, err)
	}
	onlyOneEvent(t, e, c.ID)
}

func TestACrashAfterTheWriteIsSettledNotRepeated(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, tokens := e.tokens(t, c.ID)
	claimed := claimOnly(t, e, c.ID, tokens["add"])
	// The write landed; the process died before it heard.
	if _, _, err := e.cal.CreateExact(context.Background(), "sam", "sam-family", eventWrite(claimed.Candidate.Fields, claimed.Effect.UID)); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(3 * time.Minute)
	if err := e.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	onlyOneEvent(t, e, c.ID)
	if e.dav.PutCount() != 1 {
		t.Fatalf("%d PUTs: the reconciler wrote", e.dav.PutCount())
	}
}

func TestAnEventAlreadyAtTheUIDIsNotDuplicated(t *testing.T) {
	// A write from an earlier, interrupted approval landed late, after
	// the candidate went back to review. The next Add meets
	// If-None-Match and reconciles instead of making a second event.
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, tokens := e.tokens(t, c.ID)
	if _, _, err := e.cal.CreateExact(context.Background(), "sam", "sam-family", eventWrite(c.Fields, runtimedb.MailEventUID(c.ID))); err != nil {
		t.Fatal(err)
	}
	res, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]})
	if err != nil || res.Outcome != OutcomeAdded || res.Effect.State != runtimedb.MailEffectReconciled {
		t.Fatalf("add over an existing UID: %+v %v", res, err)
	}
	onlyOneEvent(t, e, c.ID)
}

func TestACorrectionIsAnExactUpdateProposalNeverASilentEdit(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, tokens := e.tokens(t, c.ID)
	if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); err != nil {
		t.Fatal(err)
	}
	path := "/cal/sam-family/wildstone-candidate-" + c.ID + ".ics"
	before := e.dav.Object(path)

	later := schoolNight()
	later.Start, later.End = later.Start.Add(30*time.Minute), later.End.Add(30*time.Minute)
	d := draft(later)
	d.Correction, d.MessageKey = true, "m2"
	next := e.record(t, "mfp1_b", d)
	if e.dav.Object(path) != before || e.dav.PutCount() != 1 {
		t.Fatal("a correction edited the event without approval")
	}
	r, fresh := e.tokens(t, next.ID)
	if r.Kind != runtimedb.MailReviewUpdate || r.EventUID != runtimedb.MailEventUID(c.ID) || fresh["apply"] == "" || fresh["add"] != "" {
		t.Fatalf("update review %+v tokens %v", r, fresh)
	}
	// The event is not its own conflict.
	if r.Availability != runtimedb.MailAvailabilityFree {
		t.Fatalf("update review availability %s %+v", r.Availability, r.Conflicts)
	}
	res, err := e.act(ActRequest{Actor: "sam", Token: fresh["apply"]})
	if err != nil || res.Outcome != OutcomeUpdated {
		t.Fatalf("apply: %+v %v", res, err)
	}
	objs := e.dav.CandidateObjects()
	if len(objs) != 1 || objs[0] != path {
		t.Fatalf("after the update the calendar holds %v", objs)
	}
	if ics := e.dav.Object(path); !strings.Contains(ics, "DTSTART:20261015T223000Z") {
		t.Fatalf("event not updated:\n%s", ics)
	}
	if prior := e.candidate(t, c.ID); prior.UpdatePending != runtimedb.MailUpdateNone {
		t.Fatalf("prior still pending %q", prior.UpdatePending)
	}
	if got := e.candidate(t, next.ID); got.State != runtimedb.MailCandidateCreated {
		t.Fatalf("correction candidate %s", got.State)
	}
}

// Epic #326 S08: a restored database is quarantined, and the reconciler
// neither looks at a calendar nor re-offers a review on a Person's behalf
// until the journal has been replayed and the quarantine released.
func TestTheReconcilerWaitsForTheQuarantineToLift(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, tokens := e.tokens(t, c.ID)
	claimOnly(t, e, c.ID, tokens["add"])
	if err := e.db.EnterRestoreQuarantine("restored from last night", now); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(3 * time.Minute)
	notices, _ := e.db.ListNotices(10, runtimedb.NoticeFilter{Recipient: "sam"})
	if err := e.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if effects, _ := e.db.ListMailEffects("sam", c.ID); len(effects) != 1 || effects[0].State != runtimedb.MailEffectDispatching {
		t.Fatalf("the reconciler settled an effect under quarantine: %+v", effects)
	}
	if after, _ := e.db.ListNotices(10, runtimedb.NoticeFilter{Recipient: "sam"}); len(after) != len(notices) {
		t.Fatal("the reconciler re-offered a review under quarantine")
	}
	if err := e.db.AssertJournalCurrent("mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ReconcileRestore(); err != nil {
		t.Fatal(err)
	}
	if err := e.db.ReleaseRestoreQuarantine("mika"); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if effects, _ := e.db.ListMailEffects("sam", c.ID); len(effects) != 1 || effects[0].State != runtimedb.MailEffectReturned {
		t.Fatalf("after release the interrupted write was not returned to review: %+v", effects)
	}
	noEvent(t, e)
}
