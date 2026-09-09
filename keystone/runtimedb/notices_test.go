package runtimedb

// P15's evidence (ADR-W030): quiet hours, audience and deduplication
// hold across restart; ambiguous or expired replies authorize nothing;
// an expired notice cannot become approval or reopen a completed task.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func household() ButlerSettings {
	s := DefaultButlerSettings()
	s.QuietStartMin, s.QuietEndMin = 22*60, 7*60
	return s
}

// The clocks these tests raise notices on, anchored to TOMORROW rather
// than to a fixed date.
//
// A notice's quiet-hours decision uses the time it was raised — which
// these supply — but its EXPIRY is compared against the real clock,
// because that is a database read rather than a caller's opinion about
// the time. A literal date therefore made a notice raised at "midday"
// expire six hours later on that date, so every test answering one
// passed before 18:00 UTC and failed after it. This suite was green all
// morning and red at teatime, on the same commit.
//
// Anchoring to tomorrow keeps the expiry ahead of the real clock
// whenever this runs, while leaving the TIME OF DAY — which is the only
// thing the quiet-hours logic reads — exactly as it was.
func atHour(h int) time.Time {
	d := time.Now().UTC().AddDate(0, 0, 1)
	return time.Date(d.Year(), d.Month(), d.Day(), h, 0, 0, 0, time.UTC)
}

func night() time.Time  { return atHour(3) }
func midday() time.Time { return atHour(12) }

// A person's own window overrides the household's; without one they
// follow the household.
func TestPerPersonQuietHoursOverride(t *testing.T) {
	db := butlerTestDB(t)

	// Sam follows the household: 3am is quiet.
	held, err := db.RaiseNotice(NoticeRequest{
		Kind: "task-stalled", Recipient: "sam", DedupeKey: "n1",
	}, household(), night())
	if err != nil {
		t.Fatal(err)
	}
	if held.HeldUntil.IsZero() {
		t.Fatal("a 3am notice went straight out under household quiet hours")
	}

	// Mika works nights: quiet from 10am to 6pm, so 3am is fine and
	// midday is not.
	if err := db.SetMemberNoticePreferences(MemberNoticePreferences{
		Member: "mika", QuietStartMin: 10 * 60, QuietEndMin: 18 * 60, UrgentOverride: true,
	}); err != nil {
		t.Fatal(err)
	}
	night, err := db.RaiseNotice(NoticeRequest{
		Kind: "task-stalled", Recipient: "mika", DedupeKey: "n2",
	}, household(), night())
	if err != nil {
		t.Fatal(err)
	}
	if !night.HeldUntil.IsZero() {
		t.Fatal("a personal window did not override the household's")
	}
	day, err := db.RaiseNotice(NoticeRequest{
		Kind: "task-stalled", Recipient: "mika", DedupeKey: "n3",
	}, household(), midday())
	if err != nil {
		t.Fatal(err)
	}
	if day.HeldUntil.IsZero() {
		t.Fatal("a midday notice ignored the night worker's own quiet hours")
	}
}

// An urgent exception is administrator-declared and can be refused by
// the person it would wake.
func TestUrgentExceptionIsDeclaredNotClaimed(t *testing.T) {
	db := butlerTestDB(t)

	// An undeclared kind is not urgent, however it is named.
	held, err := db.RaiseNotice(NoticeRequest{
		Kind: "URGENT-please", Recipient: "sam", DedupeKey: "n1",
	}, household(), night())
	if err != nil {
		t.Fatal(err)
	}
	if held.HeldUntil.IsZero() {
		t.Fatal("a notice woke the house by calling itself urgent")
	}

	// An urgent kind has to say why it qualifies.
	if err := db.SetNoticeKind(NoticeKind{Kind: "smoke-alarm", Urgent: true}, "root-admin"); err == nil {
		t.Fatal("a kind was allowed to wake the household with no stated reason")
	}
	if err := db.SetNoticeKind(NoticeKind{
		Kind: "smoke-alarm", Urgent: true, Relevance: 100,
		Description: "a life-safety device reported an alarm",
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	through, err := db.RaiseNotice(NoticeRequest{
		Kind: "smoke-alarm", Recipient: "sam", DedupeKey: "n2",
	}, household(), night())
	if err != nil {
		t.Fatal(err)
	}
	if !through.HeldUntil.IsZero() {
		t.Fatal("a declared urgent kind was held through quiet hours")
	}

	// And a person can refuse even that.
	if err := db.SetMemberNoticePreferences(MemberNoticePreferences{
		Member: "sam", UrgentOverride: false,
	}); err != nil {
		t.Fatal(err)
	}
	refused, err := db.RaiseNotice(NoticeRequest{
		Kind: "smoke-alarm", Recipient: "sam", DedupeKey: "n3",
	}, household(), night())
	if err != nil {
		t.Fatal(err)
	}
	if refused.HeldUntil.IsZero() {
		t.Fatal("someone who asked not to be woken was woken")
	}
}

// Relevance drops what a person said they do not want, before anything
// else is considered.
func TestRelevanceThreshold(t *testing.T) {
	db := butlerTestDB(t)
	if err := db.SetNoticeKind(NoticeKind{Kind: "chatter", Relevance: 10}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMemberNoticePreferences(MemberNoticePreferences{
		Member: "sam", MinRelevance: 50, UrgentOverride: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.RaiseNotice(NoticeRequest{
		Kind: "chatter", Recipient: "sam", DedupeKey: "n1",
	}, household(), midday())
	if err != nil {
		t.Fatal(err)
	}
	if got.Sent {
		t.Fatal("a notice below the threshold was raised anyway")
	}
	if !strings.Contains(got.Reason, "threshold") {
		t.Fatalf("the drop does not say why: %q", got.Reason)
	}
	// Nothing was recorded, so it cannot be delivered later either.
	notices, _ := db.ListNotices("sam", 10)
	if len(notices) != 0 {
		t.Fatalf("a dropped notice was still recorded: %+v", notices)
	}
}

// An unassigned notice goes to the configured administrator.
func TestUnassignedNoticeRoutesToTheAdmin(t *testing.T) {
	db := butlerTestDB(t)
	got, err := db.RaiseNotice(NoticeRequest{
		Kind: "drift", DedupeKey: "n1", AdminFallback: "mika",
	}, household(), midday())
	if err != nil {
		t.Fatal(err)
	}
	notice, err := db.GetNotice(got.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if notice.Recipient != "mika" || !notice.RoutedToAdmin {
		t.Fatalf("notice = %+v", notice)
	}
	// With nobody to fall back to, it is refused rather than dropped
	// silently.
	if _, err := db.RaiseNotice(NoticeRequest{Kind: "drift", DedupeKey: "n2"},
		household(), midday()); err == nil {
		t.Fatal("a notice with nowhere to go was accepted")
	}
}

// Quiet hours, deduplication and delivery state survive a restart.
func TestNoticeStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keystone.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.RaiseNotice(NoticeRequest{
		Kind: "task-stalled", Recipient: "sam", DedupeKey: "task:bt-1",
	}, household(), night())
	if err != nil {
		t.Fatal(err)
	}
	if first.HeldUntil.IsZero() {
		t.Fatal("the notice was not held")
	}
	db.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	// Still held: the hold is state, not a timer in a process.
	due, err := reopened.DueNotices(night())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("a held notice was due after a restart: %+v", due)
	}
	// Still deduplicated.
	again, err := reopened.RaiseNotice(NoticeRequest{
		Kind: "task-stalled", Recipient: "sam", DedupeKey: "task:bt-1",
	}, household(), night())
	if err != nil {
		t.Fatal(err)
	}
	if again.Sent || again.Seq != first.Seq {
		t.Fatalf("a repeated notice was raised again after a restart: %+v", again)
	}
	// And due in the morning.
	morning := time.Date(2026, 9, 9, 8, 0, 0, 0, time.UTC)
	due, err = reopened.DueNotices(morning)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Seq != first.Seq {
		t.Fatalf("the held notice did not arrive in the morning: %+v", due)
	}
}

// Delivered, acknowledged and answered are three different facts.
func TestDeliveryAcknowledgementAndAnswerAreDistinct(t *testing.T) {
	db := butlerTestDB(t)
	got, err := db.RaiseNotice(NoticeRequest{
		Kind: "approval", Recipient: "sam", DedupeKey: "n1", Asks: true,
	}, household(), midday())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkNoticeDelivered(got.Seq, 0); err != nil {
		t.Fatal(err)
	}
	notice, _ := db.GetNotice(got.Seq)
	if notice.DeliveredAt.IsZero() {
		t.Fatal("delivery was not recorded")
	}
	if !notice.AcknowledgedAt.IsZero() || !notice.AnsweredAt.IsZero() {
		t.Fatal("delivering something recorded that a person had seen or answered it")
	}
	if err := db.AcknowledgeNotice(got.Seq); err != nil {
		t.Fatal(err)
	}
	notice, _ = db.GetNotice(got.Seq)
	if notice.AcknowledgedAt.IsZero() || !notice.AnsweredAt.IsZero() {
		t.Fatal("acknowledging something recorded an answer")
	}
	approved, err := db.AnswerNotice(got.Seq, "yes", "sam")
	if err != nil || !approved {
		t.Fatalf("AnswerNotice = %t, %v", approved, err)
	}
}

// Ambiguity is not consent, and an expired question authorizes nothing.
func TestAmbiguousAndExpiredRepliesAuthorizeNothing(t *testing.T) {
	db := butlerTestDB(t)

	t.Run("ambiguous", func(t *testing.T) {
		got, err := db.RaiseNotice(NoticeRequest{
			Kind: "approval", Recipient: "sam", DedupeKey: "a1", Asks: true,
		}, household(), midday())
		if err != nil {
			t.Fatal(err)
		}
		for _, reply := range []string{"maybe", "if you think so", "👍", "yes but not the second one", ""} {
			if _, err := db.AnswerNotice(got.Seq, reply, "sam"); !errors.Is(err, ErrNoticeNotAnswerable) {
				t.Fatalf("%q authorized something: %v", reply, err)
			}
		}
		// A clear one still works, so the refusal is about ambiguity
		// rather than about being unhelpful.
		if approved, err := db.AnswerNotice(got.Seq, "Yes.", "sam"); err != nil || !approved {
			t.Fatalf("a clear yes = %t, %v", approved, err)
		}
		// And once.
		if _, err := db.AnswerNotice(got.Seq, "no", "sam"); !errors.Is(err, ErrNoticeNotAnswerable) {
			t.Fatal("a question was answered twice")
		}
	})

	t.Run("a notice that asked nothing", func(t *testing.T) {
		got, err := db.RaiseNotice(NoticeRequest{
			Kind: "fyi", Recipient: "sam", DedupeKey: "a2",
		}, household(), midday())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.AnswerNotice(got.Seq, "yes", "sam"); !errors.Is(err, ErrNoticeNotAnswerable) {
			t.Fatalf("a reply to a statement authorized something: %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		got, err := db.RaiseNotice(NoticeRequest{
			Kind: "approval", Recipient: "sam", DedupeKey: "a3", Asks: true,
			TTL: time.Millisecond,
		}, household(), midday().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		n, err := db.ExpireNotices(midday())
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("expired %d notices, want 1", n)
		}
		notice, _ := db.GetNotice(got.Seq)
		if notice.UndeliveredAt.IsZero() {
			t.Fatal("an expired notice was not marked undelivered")
		}
		if _, err := db.AnswerNotice(got.Seq, "yes", "sam"); !errors.Is(err, ErrNoticeNotAnswerable) {
			t.Fatalf("an expired question was answered: %v", err)
		}
		// The schema refuses it too, for any writer.
		if _, err := db.ExecForTest(
			`UPDATE notices SET answered_at = 't' WHERE seq = ?`, got.Seq); err == nil {
			t.Fatal("an expired notice was answered by a direct write")
		}
	})
}

// A completed task whose notice expires stays completed. The notice is
// visible as undelivered and does not become an approval the task is
// waiting on.
func TestExpiredNoticeDoesNotReopenACompletedTask(t *testing.T) {
	db := butlerTestDB(t)
	taskID, err := db.InsertButlerTask(ButlerTask{
		Kind: ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: ButlerSurfaceFamily, Profile: ProfileHousehold,
		Audience: ButlerAudiencePrivate, Objective: "book the thing", CompletionCriteria: "booked",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionButlerTask(taskID, ButlerTaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteButlerTask(taskID, []ButlerTaskEvidence{{
		Kind: ButlerEvidenceDelivery, Description: "booked and confirmed",
		PermissionLabel: "household", Audience: ButlerAudiencePrivate,
	}}, "booked", "butler"); err != nil {
		t.Fatal(err)
	}
	got, err := db.RaiseNotice(NoticeRequest{
		Kind: "task-done", Recipient: "sam", DedupeKey: "task:" + taskID,
		CorrelationID: taskID, TTL: time.Millisecond,
	}, household(), midday().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExpireNotices(midday()); err != nil {
		t.Fatal(err)
	}

	// The task is still completed. Nothing about a missed notice
	// changes what actually happened.
	task, err := db.GetButlerTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != ButlerTaskCompleted {
		t.Fatalf("a missed notice changed the task to %q", task.State)
	}
	// And it is not waiting on anything.
	waits, _ := db.ListButlerTaskWaits(taskID)
	for _, w := range waits {
		if w.ResolvedAt.IsZero() {
			t.Fatalf("a completed task is waiting on %s", w.Reason)
		}
	}
	// The missed notice is visible, correlated to the task, so a person
	// can see they were never told.
	notice, _ := db.GetNotice(got.Seq)
	if notice.UndeliveredAt.IsZero() || notice.CorrelationID != taskID {
		t.Fatalf("notice = %+v", notice)
	}
	listed, _ := db.ListNotices("sam", 10)
	if len(listed) != 1 || listed[0].UndeliveredAt.IsZero() {
		t.Fatalf("the missed notice is not visible: %+v", listed)
	}
}
