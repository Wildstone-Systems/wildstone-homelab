package mailreview

// The epic's acceptance scenario, end to end against in-process fakes:
// the school's invitation arrives in sam's mailbox, the monitor records
// one candidate in the household zone, sam alone is privately asked
// with only the conflicts she may see, nothing is added until she
// approves that exact revision, availability is rechecked, one event
// lands on her authorized calendar with evidence, and the excerpt is
// gone. Replaying the message or the approval makes nothing else; bo
// cannot list, read, receive or approve any of it. The organizer's
// later cancellation becomes a proposal, and only sam's approval
// removes the event.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/imapread/mimenorm"
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"github.com/chickenrat17/wildstone-homelab/keystone/mailmonitor"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func mailFixture(t *testing.T, name string) mailmonitor.Message {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../testing/fixtures/mail", name))
	if err != nil {
		t.Fatal(err)
	}
	return mailmonitor.Message{Ref: "ref-" + name, Fingerprint: "mfp1_" + strings.TrimSuffix(name, ".eml"), Folder: "INBOX",
		Size: int64(len(raw)), Content: mimenorm.Normalize(raw, mimenorm.Limits{})}
}

func TestSchoolNightFromMailToOneCreatedEvent(t *testing.T) {
	e := newEnv(t)
	ny := newYork()
	// bo's private evening overlaps; sam cannot read bo-private, so it
	// must not appear in her review.
	e.dav.Seed("bo-private", "bo-thing", "Bo's private appointment", time.Date(2026, 10, 15, 17, 0, 0, 0, ny), time.Date(2026, 10, 15, 21, 0, 0, 0, ny))
	proc := &mailmonitor.Processor{DB: e.db, Vault: e.vault, Zone: householdtz.New(ny), Hook: e.eng, Now: e.clock.Now,
		Extractor: &mailmonitor.Extractor{}}
	g, err := e.eng.liveGrant(context.Background(), "sam-mail")
	if err != nil {
		t.Fatal(err)
	}
	sam, _ := e.db.GetButlerMember("sam")
	src := mailmonitor.SourceFromGrant(g, sam.PersonID, 1)
	process := func(name string) []mailmonitor.Outcome {
		t.Helper()
		out, err := proc.ProcessBatch(context.Background(), src, []mailmonitor.Message{mailFixture(t, name)})
		if err != nil {
			t.Fatalf("processing %s: %v", name, err)
		}
		return out
	}

	out := process("ics-invite.eml")
	if len(out) != 1 || len(out[0].Candidates) != 1 || out[0].Source != runtimedb.MailSourceICS {
		t.Fatalf("invite: %+v", out)
	}
	id := out[0].Candidates[0]
	c := e.candidate(t, id)
	if c.State != runtimedb.MailCandidateProposed || c.Fields.Title != "School family night" ||
		!c.Fields.Start.Equal(time.Date(2026, 10, 15, 18, 0, 0, 0, ny)) || c.TargetCalendar != "sam-family" || c.AnnouncedAt.IsZero() {
		t.Fatalf("candidate: %+v", c)
	}
	r, tokens := e.tokens(t, id)
	if r.Availability != runtimedb.MailAvailabilityFree || len(r.Conflicts) != 0 {
		t.Fatalf("sam was shown %s %+v", r.Availability, r.Conflicts)
	}
	notices, _ := e.db.ListNotices(10, runtimedb.NoticeFilter{Recipient: "sam"})
	if len(notices) != 1 || strings.Contains(notices[0].Question, "Bo's") {
		t.Fatalf("sam's review: %+v", notices)
	}
	// bo sees and can do nothing.
	if list, _ := e.db.ListMailCandidates("bo", runtimedb.MailCandidateFilter{}); len(list) != 0 {
		t.Fatal("bo lists sam's candidate")
	}
	if n, _ := e.db.ListNotices(10, runtimedb.NoticeFilter{Recipient: "bo"}); len(n) != 0 {
		t.Fatal("bo received sam's review")
	}
	if _, err := e.act(ActRequest{Actor: "bo", Token: tokens["add"]}); !errors.Is(err, runtimedb.ErrMailCandidateNotFound) {
		t.Fatalf("bo approving: %v", err)
	}
	noEvent(t, e)

	// sam approves the exact revision from her one-to-one chat.
	res, err := e.act(ActRequest{Actor: "sam", Candidate: id, Revision: 1, Action: "add", Via: "chat:" + itoa(e.binding.ID)})
	if err != nil || res.Outcome != OutcomeAdded {
		t.Fatalf("add: %+v %v", res, err)
	}
	onlyOneEvent(t, e, id)
	effects, _ := e.db.ListMailEffects("sam", id)
	if len(effects) != 1 || effects[0].State != runtimedb.MailEffectConfirmed || effects[0].Calendar != "sam-family" {
		t.Fatalf("evidence %+v", effects)
	}

	// Replays: the same message, and the same approval.
	if again := process("ics-invite.eml"); len(again) != 1 || !again[0].Duplicate {
		t.Fatalf("replayed message: %+v", again)
	}
	if _, err := e.act(ActRequest{Actor: "sam", Candidate: id, Revision: 1, Action: "add", Via: "chat:" + itoa(e.binding.ID)}); err == nil {
		t.Fatal("replayed approval honoured")
	}
	onlyOneEvent(t, e, id)

	// The organizer cancels. The event stays until sam says otherwise.
	e.clock.advance(24 * time.Hour)
	process("ics-cancel.eml")
	onlyOneEvent(t, e, id)
	cr, cancelTokens := e.tokens(t, id)
	if cr.Kind != runtimedb.MailReviewCancel || cancelTokens["apply"] == "" || cancelTokens["add"] != "" {
		t.Fatalf("cancel review %+v %v", cr, cancelTokens)
	}
	if _, err := e.act(ActRequest{Actor: "bo", Token: cancelTokens["apply"]}); !errors.Is(err, runtimedb.ErrMailCandidateNotFound) {
		t.Fatalf("bo removing sam's event: %v", err)
	}
	res, err = e.act(ActRequest{Actor: "sam", Token: cancelTokens["apply"]})
	if err != nil || res.Outcome != OutcomeCancelled {
		t.Fatalf("cancel: %+v %v", res, err)
	}
	if objs := e.dav.CandidateObjects(); len(objs) != 0 {
		t.Fatalf("the cancelled event is still there: %v", objs)
	}
	if got := e.candidate(t, id); got.State != runtimedb.MailCandidateCancelled || got.UpdatePending != "" {
		t.Fatalf("after cancel: %s %q", got.State, got.UpdatePending)
	}
}
