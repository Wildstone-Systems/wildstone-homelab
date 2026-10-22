package api

// Epic #326 S08: crash at every durable boundary of the mailbox-to-
// calendar path. Each case kills the work at one boundary — the adapter
// before it answers, Keystone before it records an answer, the batch
// half-recorded, the review not yet handed over, the lease moving
// mid-read, the approval claimed but not sent, the event written but
// not settled, the write's answer lost — then lets the system recover
// on its own. Afterwards no committed message was skipped, and nothing
// was duplicated: one processing record per message, one candidate per
// event, one open review per candidate, at most one event per candidate.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/caldav"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// samBatch is three messages from the school, in sam's filter: two
// invitations read deterministically and a prose note read by her
// local model, between them so a model outage stops the batch halfway.
func (m *mailE2E) samBatch(t *testing.T) {
	t.Helper()
	to := "sam@example.internal"
	m.provider.Deliver(to, "INBOX", m.schoolNight(to))
	fair := m.schoolDay.AddDate(0, 0, 2)
	sentence := fair.Format("Monday, January 2, 2006") + " from 3:00 to 5:00 p.m."
	m.model.replies[sentence] = `{"isEvent":true,"title":"Book fair","date":"` + fair.Format("2006-01-02") +
		`","startTime":"15:00","endTime":"17:00","durationMinutes":0,"allDay":false,"timezone":"","location":"Library",` +
		`"notes":"","confidence":0.9,"missing":[],"ambiguous":[],"evidence":"` + sentence + `","isCorrection":false}`
	m.provider.Deliver(to, "INBOX", letter("book-fair", "Library <library@school.example.internal>", to, "Book fair",
		"The book fair is "+sentence+" in the library.", samTail, "", false))
	day := m.schoolDay.AddDate(0, 0, 4).Format("20060102")
	m.provider.Deliver(to, "INBOX", letter("picture-day", "Front Office <"+schoolSender+">", to, "Invitation: Picture day",
		"Picture day is coming.", samTail,
		"BEGIN:VCALENDAR\r\nPRODID:-//School//Office//EN\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n"+
			"UID:picture-day@school.example.internal\r\nDTSTAMP:20260901T120000Z\r\n"+
			"DTSTART;TZID=America/New_York:"+day+"T090000\r\nDTEND;TZID=America/New_York:"+day+"T100000\r\n"+
			"SUMMARY:Picture day\r\nLOCATION:School hall\r\nEND:VEVENT\r\nEND:VCALENDAR", false))
}

// exactlyOnce is the invariant every case ends on.
func (m *mailE2E) exactlyOnce(t *testing.T, messages, candidates int) []runtimedb.MailCandidate {
	t.Helper()
	db := m.s.DB
	if n := countRows(t, m, `SELECT COUNT(*) FROM mail_monitor_messages WHERE connection = (SELECT seq FROM integration_connections WHERE name = 'sam-mail') AND state <> 'retrying'`); n != messages {
		t.Fatalf("%d messages recorded as processed, want %d: a committed message was skipped or left retrying", n, messages)
	}
	list, err := db.ListMailCandidates("sam", runtimedb.MailCandidateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != candidates {
		t.Fatalf("%d candidates, want %d", len(list), candidates)
	}
	titles := map[string]bool{}
	for _, c := range list {
		if titles[c.Fields.Title] {
			t.Fatalf("duplicate candidate for %q", c.Fields.Title)
		}
		titles[c.Fields.Title] = true
		if n := countRows(t, m, `SELECT COUNT(*) FROM mail_reviews WHERE candidate = ? AND state = 'open'`, c.ID); n > 1 {
			t.Fatalf("%d open reviews for %s", n, c.Fields.Title)
		}
	}
	for cal, objs := range m.events() {
		seen := map[string]bool{}
		for _, o := range objs {
			if seen[o] {
				t.Fatalf("event %s twice on %s", o, cal)
			}
			seen[o] = true
		}
	}
	if dup := countRows(t, m, `SELECT COUNT(*) FROM (SELECT candidate FROM mail_candidate_effects
		WHERE state IN ('confirmed', 'reconciled') AND kind = 'create' GROUP BY candidate HAVING COUNT(*) > 1)`); dup != 0 {
		t.Fatalf("%d candidates have more than one settled create", dup)
	}
	return list
}

func (m *mailE2E) samPrincipal(node string) string {
	return capability.AdapterPrincipalName("mail", "sam-mail", node)
}

// settle runs a few more cadences, an hour apart, so any backoff after
// a failed run has passed and recovery has had every chance to happen.
func (m *mailE2E) settle(t *testing.T) {
	t.Helper()
	for i := 0; i < 4; i++ {
		m.clock.advance(time.Hour)
		m.cycle(t)
	}
}

func TestCrashAtEveryBoundarySkipsNothingAndDuplicatesNothing(t *testing.T) {
	fresh := func(t *testing.T) *mailE2E {
		t.Helper()
		m := newMailE2E(t, "sam")
		m.cycle(t) // baseline
		m.samBatch(t)
		return m
	}

	t.Run("adapter dies after claiming, before answering", func(t *testing.T) {
		m := fresh(t)
		m.tick(t)
		if _, ok, err := m.s.DB.ClaimIntegrationWork(m.samPrincipal("n1")); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		// Nobody answers; the collector gives up after the deadline.
		m.clock.advance(3 * time.Minute)
		m.runCollectors()
		if n := countRows(t, m, `SELECT COUNT(*) FROM mail_monitor_messages`); n != 0 {
			t.Fatalf("%d messages recorded from an unanswered read", n)
		}
		m.settle(t)
		m.exactlyOnce(t, 3, 3)
	})

	t.Run("Keystone dies after the answer, before recording it", func(t *testing.T) {
		m := fresh(t)
		m.tick(t)
		m.drainAdapter(t, m.adapters["sam-mail"])
		if n := m.dropCollectors(); n != 1 {
			t.Fatalf("%d runs in flight", n)
		}
		// The run's lease runs out; the next tick abandons it, and the
		// checkpoint never moved, so the batch is read again.
		m.clock.advance(11 * time.Minute)
		m.settle(t)
		m.exactlyOnce(t, 3, 3)
		if n := countRows(t, m, `SELECT COUNT(*) FROM agent_subscription_runs WHERE state = 'abandoned'`); n != 1 {
			t.Fatalf("%d abandoned runs, want the one Keystone died in", n)
		}
	})

	t.Run("the batch is half-recorded when the model goes down", func(t *testing.T) {
		m := fresh(t)
		m.model.down = 1
		m.cycle(t)
		// The invitation before the prose note was recorded; the run did
		// not commit its cursor.
		if n := countRows(t, m, `SELECT COUNT(*) FROM mail_candidates`); n != 1 {
			t.Fatalf("%d candidates after a half batch, want 1", n)
		}
		runs, _ := m.s.DB.ListSubscriptionRuns(m.subs["sam-mail"], 5)
		if len(runs) == 0 || runs[0].Outcome != runtimedb.RunOutcomeError {
			t.Fatalf("the half batch did not fail its run: %+v", runs)
		}
		m.settle(t)
		m.exactlyOnce(t, 3, 3)
	})

	t.Run("crash before the review is handed over", func(t *testing.T) {
		m := fresh(t)
		m.hook.fail = 3
		m.cycle(t)
		if n, _ := m.s.DB.ListNotices(10, runtimedb.NoticeFilter{Recipient: "sam"}); len(n) != 0 {
			t.Fatalf("%d reviews delivered through a failed hand-over", len(n))
		}
		m.settle(t)
		list := m.exactlyOnce(t, 3, 3)
		if n, _ := m.s.DB.ListNotices(10, runtimedb.NoticeFilter{Recipient: "sam"}); len(n) != len(list) {
			t.Fatalf("%d reviews for %d candidates after recovery", len(n), len(list))
		}
	})

	t.Run("the lease moves mid-read", func(t *testing.T) {
		m := fresh(t)
		m.tick(t)
		if _, ok, err := m.s.DB.ClaimIntegrationWork(m.samPrincipal("n1")); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		// n1 stops; the lease fails over to the prepared fallback. The
		// read n1 was holding is withdrawn at once, not left claimed.
		if err := m.s.DB.MarkPlacementPrepared("sam-mail", "n1", false, true, true); err != nil {
			t.Fatal(err)
		}
		if p, moved, err := m.s.DB.PlaceConnection("sam-mail", time.Hour); err != nil || !moved || p.Node != "n2" {
			t.Fatalf("failover: %+v %v %v", p, moved, err)
		}
		m.runCollectors()
		runs, _ := m.s.DB.ListSubscriptionRuns(m.subs["sam-mail"], 5)
		if len(runs) == 0 || runs[0].State == runtimedb.SubscriptionRunRunning || runs[0].Outcome == runtimedb.RunOutcomeOK {
			t.Fatalf("the fenced read's run: %+v", runs)
		}
		m.startAdapter(t, "sam-mail", "n2")
		m.settle(t)
		m.exactlyOnce(t, 3, 3)
	})

	t.Run("approval claimed, crash before the calendar write", func(t *testing.T) {
		m := fresh(t)
		m.settle(t)
		c := m.titled(t, "School family night")
		tokens := m.tokens(t, c.ID)
		m.claimOnly(t, c.ID, tokens["add"])
		m.clock.advance(3 * time.Minute)
		if err := m.review.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if ev := m.events(); len(ev) != 0 {
			t.Fatalf("an interrupted approval wrote: %v", ev)
		}
		again := m.tokens(t, c.ID)
		if again["add"] == "" || again["add"] == tokens["add"] {
			t.Fatal("the interrupted approval was not returned to review with a fresh token")
		}
		if code, body := m.home("sam", http.MethodPost, "/api/v1/home/mail/act", map[string]string{"token": again["add"]}, nil); code != http.StatusOK {
			t.Fatalf("re-approval = %d %s", code, body)
		}
		if ev := m.events(); len(ev["sam-family"]) != 1 {
			t.Fatalf("events: %v", ev)
		}
		m.exactlyOnce(t, 3, 3)
	})

	t.Run("event written, crash before it is settled", func(t *testing.T) {
		m := fresh(t)
		m.settle(t)
		c := m.titled(t, "School family night")
		claimed := m.claimOnly(t, c.ID, m.tokens(t, c.ID)["add"])
		f := claimed.Candidate.Fields
		if _, _, err := m.review.Calendar.CreateExact(context.Background(), "sam", "sam-family", reviewEventWrite(f, claimed.Effect.UID)); err != nil {
			t.Fatal(err)
		}
		m.clock.advance(3 * time.Minute)
		if err := m.review.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if m.dav.PutCount() != 1 {
			t.Fatalf("%d calendar writes: the reconciler resent", m.dav.PutCount())
		}
		if got, _ := m.s.DB.MailCandidateInternal(c.ID); got.State != runtimedb.MailCandidateCreated {
			t.Fatalf("after reconciling: %s", got.State)
		}
		m.exactlyOnce(t, 3, 3)
	})

	t.Run("the calendar's answer is lost after the write", func(t *testing.T) {
		m := fresh(t)
		m.settle(t)
		c := m.titled(t, "School family night")
		m.dav.Schedule("hang-after")
		var res struct {
			Outcome string `json:"outcome"`
			Effect  struct {
				State string `json:"state"`
			} `json:"effect"`
		}
		if code, body := m.home("sam", http.MethodPost, "/api/v1/home/mail/act", map[string]string{"token": m.tokens(t, c.ID)["add"]}, &res); code != http.StatusOK {
			t.Fatalf("add = %d %s", code, body)
		}
		if res.Effect.State != runtimedb.MailEffectReconciled || len(m.events()["sam-family"]) != 1 {
			t.Fatalf("after a lost answer: %+v %v", res, m.events())
		}
		m.exactlyOnce(t, 3, 3)
	})
}

func (m *mailE2E) titled(t *testing.T, title string) runtimedb.MailCandidate {
	t.Helper()
	for _, c := range m.candidates(t, "sam") {
		if c.Fields.Title == title {
			return c
		}
	}
	t.Fatalf("no candidate titled %q", title)
	return runtimedb.MailCandidate{}
}

func (m *mailE2E) tokens(t *testing.T, id string) map[string]string {
	t.Helper()
	view, code, body := m.candidate(t, "sam", id)
	if code != http.StatusOK {
		t.Fatalf("candidate %s = %d %s", id, code, body)
	}
	return view.Tokens
}

// claimOnly is a process that dies right after the approval claim: the
// effect is recorded dispatching and nothing is sent.
func (m *mailE2E) claimOnly(t *testing.T, id, token string) runtimedb.MailReviewClaimed {
	t.Helper()
	sub, _ := m.s.DB.GetAgentSubscription(m.subs["sam-mail"])
	g, err := m.s.DB.AuthorizeBackgroundUse(context.Background(), "sam-mail", sub.ID, sub.ApprovedVersion)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := m.s.DB.ClaimMailReview(runtimedb.MailReviewClaim{Actor: "sam", Token: token, Via: "home", Grant: &g, At: m.clock.Now()})
	if err != nil || claimed.Effect.State != runtimedb.MailEffectDispatching {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if !strings.HasPrefix(claimed.Effect.UID, "wildstone-candidate-") {
		t.Fatalf("effect UID %q", claimed.Effect.UID)
	}
	return claimed
}

// reviewEventWrite is the exact event the review engine writes for a
// timed candidate (mailreview.eventWrite), as the write that landed just
// before a crash.
func reviewEventWrite(f runtimedb.MailCandidateFields, uid string) caldav.EventWrite {
	title, location, notes := f.Title, f.Location, f.Notes
	start, end, _ := runtimedb.MailEventWindow(f)
	return caldav.EventWrite{UID: uid, Summary: &title, Location: &location, Description: &notes,
		Start: start.Format(time.RFC3339), End: end.Format(time.RFC3339)}
}
