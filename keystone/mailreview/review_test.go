package mailreview

// Epic #326 S06 (#459): every way something other than the owning
// Person's exact, live, single-use choice might approve a calendar event
// fails closed — and the one that is such a choice creates exactly one
// event, records its evidence and purges the excerpt.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func noEvent(t *testing.T, e *env) {
	t.Helper()
	if got := e.dav.candidateObjects(); len(got) != 0 {
		t.Fatalf("an event was written: %v", got)
	}
	if n := e.dav.putCount(); n != 0 {
		t.Fatalf("%d PUTs reached the calendar", n)
	}
}

func refused(t *testing.T, err error, want error) {
	t.Helper()
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestAReviewIsPrivateExactAndNamesOnlyVisibleConflicts(t *testing.T) {
	e := newEnv(t)
	ny := newYork()
	// A clash sam can see (household), and one she cannot (bo-private).
	e.dav.seed("household", "parents-evening", "Parents evening", time.Date(2026, 10, 15, 18, 30, 0, 0, ny), time.Date(2026, 10, 15, 19, 0, 0, 0, ny))
	e.dav.seed("bo-private", "bo-secret", "Bo's private thing", time.Date(2026, 10, 15, 18, 0, 0, 0, ny), time.Date(2026, 10, 15, 20, 0, 0, 0, ny))
	c := e.record(t, "mfp1_a", draft(schoolNight()))

	r, tokens := e.tokens(t, c.ID)
	if r.Kind != runtimedb.MailReviewCreate || r.Revision != 1 || r.TargetCalendar != "sam-family" ||
		r.Route != "chat:"+itoa(e.binding.ID) || r.State != runtimedb.MailReviewOpen {
		t.Fatalf("review: %+v", r)
	}
	if want := []string{"add", "edit", "dismiss"}; strings.Join(r.Actions, ",") != strings.Join(want, ",") || len(tokens) != 3 {
		t.Fatalf("actions %v tokens %d", r.Actions, len(tokens))
	}
	// Expires at the earlier of 72 hours and the start.
	if !r.ExpiresAt.Equal(now.Add(72 * time.Hour)) {
		t.Fatalf("expires %v", r.ExpiresAt)
	}
	if r.Availability != runtimedb.MailAvailabilityBusy || len(r.Conflicts) != 1 || r.Conflicts[0].Title != "Parents evening" ||
		strings.Join(r.CheckedCalendars, ",") != "sam-family,household" {
		t.Fatalf("conflicts %+v checked %v (%s)", r.Conflicts, r.CheckedCalendars, r.Availability)
	}
	// The notice and the push go to sam alone, and say nothing of bo's.
	for _, who := range []string{"bo", "mika"} {
		if n, _ := e.db.ListNotices(50, runtimedb.NoticeFilter{Recipient: who}); len(n) != 0 {
			t.Fatalf("%s received a notice: %+v", who, n)
		}
	}
	notices, _ := e.db.ListNotices(50, runtimedb.NoticeFilter{Recipient: "sam"})
	if len(notices) != 1 || notices[0].Kind != NoticeKind || notices[0].Asks {
		t.Fatalf("sam's notices: %+v", notices)
	}
	body := notices[0].Question
	if !strings.Contains(body, "School family night") || !strings.Contains(body, "Parents evening") ||
		strings.Contains(body, "Bo's private") || strings.Contains(body, "mrt_") || strings.Contains(body, "gym. See you") {
		t.Fatalf("review text: %s", body)
	}
	if !strings.Contains(body, "/mail add "+c.ID+" 1") {
		t.Fatalf("review text does not name the exact candidate and revision: %s", body)
	}
	// Nothing is on the calendar until sam chooses.
	noEvent(t, e)
}

func TestAddCreatesExactlyOneEventRecordsEvidenceAndPurgesTheExcerpt(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	if ex, _ := e.db.ReadMailCandidateExcerpt(e.vault, "sam", c.ID, e.clock.Now()); ex == "" {
		t.Fatal("fixture has no excerpt")
	}
	_, tokens := e.tokens(t, c.ID)
	res, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]})
	if err != nil || res.Outcome != OutcomeAdded {
		t.Fatalf("add: %+v %v", res, err)
	}
	objs := e.dav.candidateObjects()
	if len(objs) != 1 || objs[0] != "/cal/sam-family/wildstone-candidate-"+c.ID+".ics" || e.dav.putCount() != 1 {
		t.Fatalf("objects %v puts %d", objs, e.dav.putCount())
	}
	ics := e.dav.object(objs[0])
	for _, want := range []string{"UID:wildstone-candidate-" + c.ID, "SUMMARY:School family night", "DTSTART:20261015T220000Z",
		"DTEND:20261015T233000Z", "LOCATION:School gym"} {
		if !strings.Contains(ics, want) {
			t.Fatalf("written event lacks %q:\n%s", want, ics)
		}
	}
	got := e.candidate(t, c.ID)
	if got.State != runtimedb.MailCandidateCreated {
		t.Fatalf("candidate %s", got.State)
	}
	effects, err := e.db.ListMailEffects("sam", c.ID)
	if err != nil || len(effects) != 1 || effects[0].State != runtimedb.MailEffectConfirmed ||
		effects[0].UID != "wildstone-candidate-"+c.ID || effects[0].Href == "" || effects[0].Actor != "sam" ||
		effects[0].Kind != runtimedb.MailReviewCreate || effects[0].Calendar != "sam-family" {
		t.Fatalf("effect evidence %+v %v", effects, err)
	}
	if ex, _ := e.db.ReadMailCandidateExcerpt(e.vault, "sam", c.ID, e.clock.Now()); ex != "" {
		t.Fatal("the excerpt outlived the created event")
	}
	// Replayed and duplicate approvals make nothing else.
	if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); !errors.Is(err, runtimedb.ErrMailReviewRefused) {
		t.Fatalf("replayed token: %v", err)
	}
	if _, err := e.act(ActRequest{Actor: "sam", Candidate: c.ID, Revision: 1, Action: "add"}); err == nil {
		t.Fatal("a duplicate click by id and revision was honoured")
	}
	if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["dismiss"]}); err == nil {
		t.Fatal("a sibling token of a used review was honoured")
	}
	if len(e.dav.candidateObjects()) != 1 || e.dav.putCount() != 1 {
		t.Fatal("a replay wrote again")
	}
}

func TestNobodyButTheOwningPersonCanAct(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, tokens := e.tokens(t, c.ID)
	// bo is another Member, mika the Owner, hobson the Agent that runs
	// the monitor, root-admin a machine principal.
	for _, who := range []string{"bo", "mika", "hobson", "root-admin", "sam-mail", ""} {
		if _, err := e.act(ActRequest{Actor: who, Token: tokens["add"]}); !errors.Is(err, runtimedb.ErrMailCandidateNotFound) {
			t.Errorf("%q with sam's token: %v", who, err)
		}
		if _, err := e.act(ActRequest{Actor: who, Candidate: c.ID, Revision: 1, Action: "add"}); !errors.Is(err, runtimedb.ErrMailCandidateNotFound) {
			t.Errorf("%q by id: %v", who, err)
		}
		if _, _, err := e.db.MailReviewTokens(e.vault, who, c.ID, e.clock.Now()); err == nil {
			t.Errorf("%q read sam's tokens", who)
		}
	}
	noEvent(t, e)
	// sam's own token still works: nothing above spent it.
	if res, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); err != nil || res.Outcome != OutcomeAdded {
		t.Fatalf("sam after the refusals: %+v %v", res, err)
	}
}

func TestFreeTextSilenceAcknowledgementAndGroupApproveNothing(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	notices, _ := e.db.ListNotices(50, runtimedb.NoticeFilter{Recipient: "sam"})
	if len(notices) != 1 {
		t.Fatalf("notices %+v", notices)
	}
	// A free-text "yes" against the notice answers nothing.
	for _, yes := range []string{"yes", "ok", "add it", "approve"} {
		if _, err := e.db.AnswerNotice(notices[0].Seq, yes, "sam"); !errors.Is(err, runtimedb.ErrNoticeNotAnswerable) {
			t.Fatalf("%q answered the review: %v", yes, err)
		}
	}
	// Delivery and acknowledgement are not approval.
	if err := e.db.MarkNoticeDelivered(notices[0].Seq, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.db.AcknowledgeNotice(notices[0].Seq); err != nil {
		t.Fatal(err)
	}
	// A reply in a group approves nothing, even sam's, even with the
	// right id and revision.
	if _, err := e.act(ActRequest{Actor: "sam", Candidate: c.ID, Revision: 1, Action: "add", Group: true, Via: "chat:" + itoa(e.binding.ID)}); !errors.Is(err, runtimedb.ErrMailReviewRefused) {
		t.Fatalf("group reply: %v", err)
	}
	// A reply in some other conversation is not the route it was sent on.
	if _, err := e.act(ActRequest{Actor: "sam", Candidate: c.ID, Revision: 1, Action: "add", Via: "chat:999999"}); !errors.Is(err, runtimedb.ErrMailReviewRefused) {
		t.Fatalf("other conversation: %v", err)
	}
	// Silence: the review expires, and nothing was added.
	e.clock.advance(73 * time.Hour)
	if err := e.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.db.MailReviewFor("sam", c.ID); r.State != runtimedb.MailReviewExpired {
		t.Fatalf("after silence the review is %s", r.State)
	}
	if got := e.candidate(t, c.ID); got.State != runtimedb.MailCandidateProposed {
		t.Fatalf("after silence the candidate is %s", got.State)
	}
	noEvent(t, e)
}

func TestExpiredTokensFailClosed(t *testing.T) {
	e := newEnv(t)
	// An event starting in 10 hours: its review expires at its start.
	soon := schoolNight()
	soon.Start, soon.End = now.Add(10*time.Hour), now.Add(11*time.Hour)
	c := e.record(t, "mfp1_soon", draft(soon))
	r, tokens := e.tokens(t, c.ID)
	if !r.ExpiresAt.Equal(soon.Start) {
		t.Fatalf("review of an event 10h away expires %v, want its start", r.ExpiresAt)
	}
	e.clock.advance(10 * time.Hour)
	if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); !errors.Is(err, runtimedb.ErrMailReviewRefused) {
		t.Fatalf("expired token: %v", err)
	}
	if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); !errors.Is(err, runtimedb.ErrMailReviewRefused) {
		t.Fatalf("expired token, twice: %v", err)
	}
	// And the 72-hour bound.
	e2 := newEnv(t)
	c2 := e2.record(t, "mfp1_b", draft(schoolNight()))
	_, tokens2 := e2.tokens(t, c2.ID)
	e2.clock.advance(72 * time.Hour)
	if _, err := e2.act(ActRequest{Actor: "sam", Token: tokens2["add"]}); !errors.Is(err, runtimedb.ErrMailReviewRefused) {
		t.Fatalf("72h-old token: %v", err)
	}
	noEvent(t, e)
	noEvent(t, e2)
}

func TestStaleRevisionsAndChangedFieldsFailClosed(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, first := e.tokens(t, c.ID)

	// Edit through the review: a new revision, a fresh review, and the
	// old tokens are dead.
	edited := schoolNight()
	edited.Title = "School family night (gym)"
	res, err := e.act(ActRequest{Actor: "sam", Token: first["edit"], Fields: &edited})
	if err != nil || res.Outcome != OutcomeRevised || res.Review == nil || res.Review.Revision != 2 {
		t.Fatalf("edit: %+v %v", res, err)
	}
	for _, a := range []string{"add", "dismiss", "edit"} {
		if _, err := e.act(ActRequest{Actor: "sam", Token: first[a], Fields: &edited}); err == nil {
			t.Fatalf("revision 1's %s token still works after an edit", a)
		}
	}
	if _, err := e.act(ActRequest{Actor: "sam", Candidate: c.ID, Revision: 1, Action: "add"}); !errors.Is(err, runtimedb.ErrMailCandidateStale) {
		t.Fatalf("stale revision by id: %v", err)
	}
	// The fields change under the open review (a revision made outside
	// it): the review's digest no longer matches and it closes.
	_, second := e.tokens(t, c.ID)
	moved := edited
	moved.Start = moved.Start.Add(time.Hour)
	moved.End = moved.End.Add(time.Hour)
	if _, err := e.db.ReviseMailCandidate("sam", c.ID, 2, moved, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.act(ActRequest{Actor: "sam", Token: second["add"]}); !errors.Is(err, runtimedb.ErrMailCandidateStale) {
		t.Fatalf("token for changed fields: %v", err)
	}
	noEvent(t, e)
}

func TestRevokedAuthorityAndCalendarAccessLossFailClosed(t *testing.T) {
	t.Run("revoked authorization", func(t *testing.T) {
		e := newEnv(t)
		c := e.record(t, "mfp1_a", draft(schoolNight()))
		_, tokens := e.tokens(t, c.ID)
		if err := e.db.RevokeBackgroundAuthorization("sam-mail", "sam", "stop watching"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); !errors.Is(err, runtimedb.ErrMailReviewRefused) {
			t.Fatalf("revoked: %v", err)
		}
		noEvent(t, e)
	})
	t.Run("subscription disabled", func(t *testing.T) {
		e := newEnv(t)
		c := e.record(t, "mfp1_a", draft(schoolNight()))
		_, tokens := e.tokens(t, c.ID)
		if err := e.db.SetAgentSubscriptionEnabled(e.sub, false, "mika"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); !errors.Is(err, runtimedb.ErrMailReviewRefused) {
			t.Fatalf("disabled subscription: %v", err)
		}
		noEvent(t, e)
	})
	t.Run("offboarded Person", func(t *testing.T) {
		e := newEnv(t)
		c := e.record(t, "mfp1_a", draft(schoolNight()))
		_, tokens := e.tokens(t, c.ID)
		if err := e.db.DisableButlerMember("sam", "mika"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); err == nil {
			t.Fatal("a disabled Person approved")
		}
		noEvent(t, e)
	})
	t.Run("calendar write access lost", func(t *testing.T) {
		e := newEnv(t)
		c := e.record(t, "mfp1_a", draft(schoolNight()))
		_, tokens := e.tokens(t, c.ID)
		e.setAccess("sam", "sam-family", "r")
		if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); !errors.Is(err, runtimedb.ErrMailReviewRefused) {
			t.Fatalf("read-only target: %v", err)
		}
		noEvent(t, e)
		if got := e.candidate(t, c.ID); got.State != runtimedb.MailCandidateProposed {
			t.Fatalf("candidate %s", got.State)
		}
	})
	t.Run("calendar refuses at write time", func(t *testing.T) {
		e := newEnv(t)
		c := e.record(t, "mfp1_a", draft(schoolNight()))
		_, tokens := e.tokens(t, c.ID)
		e.dav.schedule("forbidden")
		if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); !errors.Is(err, ErrCalendarRefused) {
			t.Fatalf("forbidden: %v", err)
		}
		if len(e.dav.candidateObjects()) != 0 {
			t.Fatal("an event exists after a refused write")
		}
		if got := e.candidate(t, c.ID); got.State != runtimedb.MailCandidateProposed {
			t.Fatalf("candidate %s after a refusal", got.State)
		}
	})
}

func TestDismissKeepsOnlyTheFingerprint(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, tokens := e.tokens(t, c.ID)
	res, err := e.act(ActRequest{Actor: "sam", Token: tokens["dismiss"]})
	if err != nil || res.Outcome != OutcomeDismissed {
		t.Fatalf("dismiss: %+v %v", res, err)
	}
	got := e.candidate(t, c.ID)
	if got.State != runtimedb.MailCandidateDismissed || got.Fields.Title != "" || got.Fields.Location != "" ||
		got.Fields.Notes != "" || got.Fingerprint != "mfp1_a" {
		t.Fatalf("dismissed candidate keeps %+v", got)
	}
	if ex, _ := e.db.ReadMailCandidateExcerpt(e.vault, "sam", c.ID, e.clock.Now()); ex != "" {
		t.Fatal("a dismissed candidate kept its excerpt")
	}
	if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); err == nil {
		t.Fatal("add after dismiss")
	}
	noEvent(t, e)
}

func TestANewConflictReturnsToReviewWithAFreshToken(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	first, tokens := e.tokens(t, c.ID)
	if first.Availability != runtimedb.MailAvailabilityFree {
		t.Fatalf("availability %s", first.Availability)
	}
	ny := newYork()
	e.dav.seed("household", "late-meeting", "Late meeting", time.Date(2026, 10, 15, 19, 0, 0, 0, ny), time.Date(2026, 10, 15, 20, 0, 0, 0, ny))
	res, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]})
	if err != nil || res.Outcome != OutcomeReturned || res.Review == nil || res.Review.ID == first.ID ||
		len(res.Review.Conflicts) != 1 || res.Candidate.State != runtimedb.MailCandidateProposed {
		t.Fatalf("new conflict: %+v %v", res, err)
	}
	noEvent(t, e)
	if _, err := e.act(ActRequest{Actor: "sam", Token: tokens["add"]}); err == nil {
		t.Fatal("the pre-conflict token still works")
	}
	_, fresh := e.tokens(t, c.ID)
	if res, err := e.act(ActRequest{Actor: "sam", Token: fresh["add"]}); err != nil || res.Outcome != OutcomeAdded {
		t.Fatalf("add after seeing the conflict: %+v %v", res, err)
	}
	if len(e.dav.candidateObjects()) != 1 {
		t.Fatal("not exactly one event")
	}
}

func TestConcurrentDuplicateApprovalsCreateOneEvent(t *testing.T) {
	e := newEnv(t)
	c := e.record(t, "mfp1_a", draft(schoolNight()))
	_, tokens := e.tokens(t, c.ID)
	var wg sync.WaitGroup
	var mu sync.Mutex
	added, refusedN := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := ActRequest{Actor: "sam", Token: tokens["add"]}
			if i%2 == 1 {
				req = ActRequest{Actor: "sam", Candidate: c.ID, Revision: 1, Action: "add"}
			}
			res, err := e.act(req)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && res.Outcome == OutcomeAdded {
				added++
			} else {
				refusedN++
			}
		}(i)
	}
	wg.Wait()
	if added != 1 || refusedN != 7 || len(e.dav.candidateObjects()) != 1 || e.dav.putCount() != 1 {
		t.Fatalf("added %d refused %d objects %v puts %d", added, refusedN, e.dav.candidateObjects(), e.dav.putCount())
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestConflictCalendarsComeFromTheScopeAndTheApprovedMatch(t *testing.T) {
	e := newEnv(t)
	sub, err := e.db.ProposeAgentSubscription(runtimedb.AgentSubscriptionSpec{Butler: e.agent,
		TriggerKind: runtimedb.SubscriptionTriggerSchedule, MatchKind: "mail.changes",
		MatchJSON: `{"conflictCalendars":["school","household"," "]}`, Audience: runtimedb.AudienceOwner,
		Capabilities: []string{"mail.changes"}, SourceConnection: "sam-mail", Cadence: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	got := e.eng.conflictCalendars(runtimedb.BackgroundGrant{Subscription: sub,
		Scope: runtimedb.BackgroundScope{ConflictCalendars: []string{"household", "bo-private", "sam-family"}}}, "sam-family")
	if strings.Join(got, ",") != "sam-family,household,bo-private,school" {
		t.Fatalf("calendars checked: %v", got)
	}
}
