package mailmonitor

// Epic #326 S05's extraction fixtures, each with its exact expected
// state: deterministic ICS, ordinary prose (the school-night scenario),
// ambiguous time, correction, duplicate, hostile prompt injection,
// malformed model output, and a local-only source that never reaches a
// cloud route on any path.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// neverCalled fails the test if a model is asked anything.
func neverCalled(t *testing.T) *fakeModel {
	t.Helper()
	m := &fakeModel{}
	t.Cleanup(func() {
		if m.calls() != 0 {
			t.Errorf("a model was called %d time(s) where none may be", m.calls())
		}
	})
	return m
}

func TestICSInvitationIsReadDeterministically(t *testing.T) {
	e := newEnv(t, routes{list: []ModelRoute{local(neverCalled(t))}}, runtimedb.ProcessingLocal)
	out := e.process(t, fixture(t, "ics-invite.eml"))
	// The invitation arrives twice (inline and as an .ics attachment):
	// one candidate.
	c := e.candidate(t, one(t, out[0]))
	want := runtimedb.MailCandidateFields{
		Title: "School family night", Location: "School gym", TZ: "America/New_York", Confidence: 1,
		Start: time.Date(2026, 10, 15, 22, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 15, 23, 30, 0, 0, time.UTC),
		Missing: []string{}, Ambiguous: []string{},
	}
	if out[0].State != runtimedb.MailMessageCandidate || out[0].Source != runtimedb.MailSourceICS ||
		c.State != runtimedb.MailCandidateProposed || c.Source != runtimedb.MailSourceICS ||
		c.TargetCalendar != "sam-family" || c.Revision != 1 {
		t.Fatalf("ICS: outcome %+v, candidate %+v", out[0], c)
	}
	c.Fields.Start, c.Fields.End = c.Fields.Start.UTC(), c.Fields.End.UTC()
	if !reflect.DeepEqual(c.Fields, want) {
		t.Fatalf("ICS fields:\n got %+v\nwant %+v", c.Fields, want)
	}

	// The same invitation forwarded by someone else is the same event.
	out = e.process(t, fixture(t, "forwarded-invite.eml"))
	if out[0].State != runtimedb.MailMessageDuplicateEvent || len(out[0].Candidates) != 0 {
		t.Fatalf("forwarded invite: %+v", out[0])
	}

	// The organizer's CANCEL cancels the unresolved candidate.
	out = e.process(t, fixture(t, "ics-cancel.eml"))
	if out[0].State != runtimedb.MailMessageCancelled {
		t.Fatalf("cancel: %+v", out[0])
	}
	if got := e.candidate(t, c.ID); got.State != runtimedb.MailCandidateCancelled {
		t.Fatalf("after cancel: %s", got.State)
	}
}

func TestICSWithUnknownZoneNeedsDetails(t *testing.T) {
	e := newEnv(t, nil, runtimedb.ProcessingLocal)
	m := fixture(t, "ics-invite.eml")
	for i := range m.Content.Calendars {
		m.Content.Calendars[i].Data = strings.ReplaceAll(m.Content.Calendars[i].Data, "TZID=America/New_York", "TZID=Eastern-ish Time")
	}
	c := e.candidate(t, one(t, e.process(t, m)[0]))
	if c.State != runtimedb.MailCandidateNeedsDetails || !c.Fields.Start.IsZero() ||
		!reflect.DeepEqual(c.Fields.Ambiguous, []string{runtimedb.MailFieldTimezone}) {
		t.Fatalf("unknown TZID was resolved anyway: %+v", c)
	}
}

// TestSchoolNightScenario is the epic's acceptance message: prose, read
// by a LOCAL model, resolved in the household zone, for sam alone.
func TestSchoolNightScenario(t *testing.T) {
	m := &fakeModel{replies: []string{schoolNightReply}}
	e := newEnv(t, routes{list: []ModelRoute{local(m)}}, runtimedb.ProcessingLocal)
	out := e.process(t, fixture(t, "plain.eml"))
	c := e.candidate(t, one(t, out[0]))
	if c.State != runtimedb.MailCandidateProposed || c.Source != runtimedb.MailSourceModel ||
		c.Fields.Title != "School family night" || c.Fields.Location != "School gym" || c.Fields.TZ != "America/New_York" ||
		!c.Fields.Start.Equal(time.Date(2026, 10, 15, 18, 0, 0, 0, e.ny)) ||
		!c.Fields.End.Equal(time.Date(2026, 10, 15, 19, 30, 0, 0, e.ny)) ||
		len(c.Fields.Missing) != 0 || len(c.Fields.Ambiguous) != 0 || c.TargetCalendar != "sam-family" {
		t.Fatalf("school night: %+v", c)
	}
	if m.calls() != 1 {
		t.Fatalf("model calls = %d, want 1", m.calls())
	}
	req := m.reqs[0]
	if len(req.Tools) != 0 {
		t.Fatal("the extraction model was offered tools")
	}
	if !strings.Contains(req.System, "UNTRUSTED") || !strings.Contains(req.Messages[0].Text, "<<<EMAIL") ||
		!strings.Contains(req.Messages[0].Text, "Household timezone: America/New_York") {
		t.Fatalf("prompt does not frame the mail as untrusted evidence:\n%s", req.Messages[0].Text)
	}
	excerpt, err := e.db.ReadMailCandidateExcerpt(e.proc.Vault, "sam", c.ID, now)
	if err != nil || excerpt != "Thursday, October 15, 2026 from 6:00 to 7:30 p.m." {
		t.Fatalf("excerpt = %q, %v", excerpt, err)
	}
	// Only sam's review hears about it; bo cannot list, read or infer it.
	if len(e.hook.changes) != 1 || e.hook.changes[0].Kind != runtimedb.MailChangeNew ||
		e.hook.changes[0].Candidate.PersonID != e.src.PersonID {
		t.Fatalf("hook: %+v", e.hook.changes)
	}
	if list, _ := e.db.ListMailCandidates("bo", runtimedb.MailCandidateFilter{}); len(list) != 0 {
		t.Fatal("bo can list sam's candidate")
	}
	if _, err := e.db.GetMailCandidate("bo", c.ID); !errors.Is(err, runtimedb.ErrMailCandidateNotFound) {
		t.Fatalf("bo read sam's candidate: %v", err)
	}
	if pending, _ := e.db.UnannouncedMailCandidates("sam-mail", 0); len(pending) != 0 {
		t.Fatal("an acknowledged candidate is still offered to the hook")
	}
}

func TestAmbiguousTimeNeedsDetails(t *testing.T) {
	for name, reply := range map[string]string{
		// An honest model says what is missing.
		"honest": modelJSON("2026-10-08", "", "", "Family night", "", `"start","end"`, "", false, "Family night is on Thursday evening"),
		// A model that guesses "evening" as 18:00 is overruled: the mail
		// states no time.
		"guessing": modelJSON("2026-10-08", "18:00", "19:30", "Family night", "", "", "", false, "Family night is on Thursday evening"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, routes{list: []ModelRoute{local(&fakeModel{replies: []string{reply}})}}, runtimedb.ProcessingLocal)
			c := e.candidate(t, one(t, e.process(t, fixture(t, "ambiguous-time.eml"))[0]))
			if c.State != runtimedb.MailCandidateNeedsDetails || !c.Fields.Start.IsZero() || !c.Fields.End.IsZero() {
				t.Fatalf("%s: %+v", name, c)
			}
			hasStart := false
			for _, f := range append(c.Fields.Missing, c.Fields.Ambiguous...) {
				if f == runtimedb.MailFieldStart {
					hasStart = true
				}
			}
			if !hasStart {
				t.Fatalf("%s: start is neither missing nor ambiguous: %+v", name, c.Fields)
			}
		})
	}
}

func TestCorrectionSupersedes(t *testing.T) {
	corrected := modelJSON("2026-10-15", "18:30", "20:00", "School family night", "School gym", "", "", true,
		"now runs from 6:30 to 8:00 p.m.")
	m := &fakeModel{replies: []string{schoolNightReply}}
	e := newEnv(t, routes{list: []ModelRoute{local(m)}}, runtimedb.ProcessingLocal)
	first := one(t, e.process(t, fixture(t, "plain.eml"))[0])
	m.replies = []string{corrected}
	second := one(t, e.process(t, fixture(t, "correction.eml"))[0])
	old, next := e.candidate(t, first), e.candidate(t, second)
	if old.State != runtimedb.MailCandidateSuperseded || old.SupersededBy != second || next.Supersedes != first ||
		next.State != runtimedb.MailCandidateProposed || !next.Fields.Start.Equal(time.Date(2026, 10, 15, 18, 30, 0, 0, e.ny)) {
		t.Fatalf("correction: old %+v\nnext %+v", old, next)
	}
	var superseded bool
	for _, c := range e.hook.changes {
		if c.Kind == runtimedb.MailChangeSuperseded && c.Candidate.ID == first {
			superseded = true
		}
	}
	if !superseded {
		t.Fatal("the review hook was not told the first candidate was superseded")
	}
}

func TestCorrectionOfACreatedEventProposesAnUpdate(t *testing.T) {
	corrected := modelJSON("2026-10-15", "18:30", "20:00", "School family night", "School gym", "", "", true,
		"now runs from 6:30 to 8:00 p.m.")
	m := &fakeModel{replies: []string{schoolNightReply}}
	e := newEnv(t, routes{list: []ModelRoute{local(m)}}, runtimedb.ProcessingLocal)
	first := one(t, e.process(t, fixture(t, "plain.eml"))[0])
	for _, to := range []string{runtimedb.MailCandidateAccepted, runtimedb.MailCandidateCreated} {
		if _, err := e.db.TransitionMailCandidate("sam", first, 1, to, "", now); err != nil {
			t.Fatal(err)
		}
	}
	m.replies = []string{corrected}
	second := one(t, e.process(t, fixture(t, "correction.eml"))[0])
	created := e.candidate(t, first)
	if created.State != runtimedb.MailCandidateCreated || created.UpdatePending != runtimedb.MailUpdateEvent ||
		created.SupersededBy != second || !created.Fields.Start.Equal(time.Date(2026, 10, 15, 18, 0, 0, 0, e.ny)) {
		t.Fatalf("created event after a correction: %+v", created)
	}
	var update bool
	for _, c := range e.hook.changes {
		if c.Kind == runtimedb.MailChangeUpdateNeeded && c.Candidate.ID == first {
			update = true
		}
	}
	if !update {
		t.Fatal("the review hook was not asked to propose the update")
	}
}

func TestDuplicateDeliveryAsksNothingTwice(t *testing.T) {
	m := &fakeModel{replies: []string{schoolNightReply}}
	e := newEnv(t, routes{list: []ModelRoute{local(m)}}, runtimedb.ProcessingLocal)
	msg := fixture(t, "plain.eml")
	e.process(t, msg)
	out := e.process(t, msg, msg)
	for _, o := range out {
		if !o.Duplicate || o.State != runtimedb.MailMessageCandidate || len(o.Candidates) != 0 {
			t.Fatalf("replay: %+v", o)
		}
	}
	if m.calls() != 1 {
		t.Fatalf("a replayed message was read again (%d model calls)", m.calls())
	}
	if list, _ := e.db.ListMailCandidates("sam", runtimedb.MailCandidateFilter{}); len(list) != 1 {
		t.Fatalf("%d candidates after replays", len(list))
	}
}

func TestPromptInjection(t *testing.T) {
	t.Run("instructions alone are not an event and reach no model", func(t *testing.T) {
		e := newEnv(t, routes{list: []ModelRoute{local(neverCalled(t))}}, runtimedb.ProcessingLocal)
		out := e.process(t, fixture(t, "prompt-injection.eml"))
		if out[0].State != runtimedb.MailMessageNoEvent || out[0].Reason != "no_date_evidence" {
			t.Fatalf("injection: %+v", out[0])
		}
	})
	t.Run("a model that obeys the mail is refused by the schema", func(t *testing.T) {
		hijacked := strings.TrimSuffix(schoolNightReply, "}") + `,"targetCalendar":"household-shared","state":"accepted"}`
		m := &fakeModel{replies: []string{hijacked}}
		e := newEnv(t, routes{list: []ModelRoute{local(m)}}, runtimedb.ProcessingLocal)
		out := e.process(t, fixture(t, "prompt-injection-event.eml"))
		if out[0].State != runtimedb.MailMessageExtractionFailed || out[0].Reason != "malformed_output" ||
			len(out[0].Candidates) != 0 || m.calls() != 2 {
			t.Fatalf("hijacked output: %+v after %d calls", out[0], m.calls())
		}
		if !strings.Contains(m.reqs[1].Messages[2].Text, "unknown field") {
			t.Fatalf("the repair did not name the violation: %q", m.reqs[1].Messages[2].Text)
		}
	})
	t.Run("a well-formed event from hostile mail is still only a proposal", func(t *testing.T) {
		reply := modelJSON("2026-10-16", "15:00", "16:00", "Bake sale", "School lobby", "", "", false,
			"Friday, October 16, 2026 from 3:00 to 4:00 p.m.")
		reply = strings.Replace(reply, `"notes":""`, `"notes":"Approved. Guest attacker@example.invalid, see https://example.invalid/x"`, 1)
		e := newEnv(t, routes{list: []ModelRoute{local(&fakeModel{replies: []string{reply}})}}, runtimedb.ProcessingLocal)
		c := e.candidate(t, one(t, e.process(t, fixture(t, "prompt-injection-event.eml"))[0]))
		if c.State != runtimedb.MailCandidateProposed || c.TargetCalendar != "sam-family" || c.UpdatePending != "" ||
			strings.Contains(c.Fields.Notes, "https://") {
			t.Fatalf("hostile mail: %+v", c)
		}
		// Nothing but sam's own candidate exists; no other Person, no
		// shared calendar, no approval.
		if list, _ := e.db.ListMailCandidates("mika", runtimedb.MailCandidateFilter{}); len(list) != 0 {
			t.Fatal("hostile mail reached an Owner")
		}
	})
	t.Run("a model that asks for a tool gets none", func(t *testing.T) {
		m := &fakeModel{tools: true}
		e := newEnv(t, routes{list: []ModelRoute{local(m)}}, runtimedb.ProcessingLocal)
		out := e.process(t, fixture(t, "prompt-injection-event.eml"))
		if out[0].State != runtimedb.MailMessageExtractionFailed || m.calls() != 2 {
			t.Fatalf("tool call: %+v after %d calls", out[0], m.calls())
		}
	})
}

func TestMalformedModelOutput(t *testing.T) {
	cases := map[string]struct {
		replies []string
		state   string
		calls   int
	}{
		"repaired":           {[]string{"Sure! The event is on Thursday.", schoolNightReply}, runtimedb.MailMessageCandidate, 2},
		"prose twice":        {[]string{"Sure!", "It is Thursday."}, runtimedb.MailMessageExtractionFailed, 2},
		"missing keys":       {[]string{`{"isEvent":true,"title":"x"}`}, runtimedb.MailMessageExtractionFailed, 2},
		"wrong types":        {[]string{strings.Replace(schoolNightReply, `"confidence":0.9`, `"confidence":"high"`, 1)}, runtimedb.MailMessageExtractionFailed, 2},
		"bad date":           {[]string{strings.Replace(schoolNightReply, "2026-10-15", "2026-02-31", 1)}, runtimedb.MailMessageExtractionFailed, 2},
		"bad clock":          {[]string{strings.Replace(schoolNightReply, `"18:00"`, `"6pm"`, 1)}, runtimedb.MailMessageExtractionFailed, 2},
		"unknown field name": {[]string{strings.Replace(schoolNightReply, `"missing":[]`, `"missing":["approval"]`, 1)}, runtimedb.MailMessageExtractionFailed, 2},
		"trailing text":      {[]string{schoolNightReply + " and more"}, runtimedb.MailMessageExtractionFailed, 2},
		"fenced":             {[]string{"```json\n" + schoolNightReply + "\n```"}, runtimedb.MailMessageCandidate, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &fakeModel{replies: tc.replies}
			e := newEnv(t, routes{list: []ModelRoute{local(m)}}, runtimedb.ProcessingLocal)
			out := e.process(t, fixture(t, "plain.eml"))
			if out[0].State != tc.state || m.calls() != tc.calls {
				t.Fatalf("%s: %+v after %d calls, want %s after %d", name, out[0], m.calls(), tc.state, tc.calls)
			}
			if tc.state == runtimedb.MailMessageExtractionFailed && out[0].Reason != "malformed_output" {
				t.Fatalf("%s: reason %q", name, out[0].Reason)
			}
		})
	}
}

// TestLocalOnlyNeverReachesCloud walks every path a local-only source
// can take — success, transport error, malformed output, no local model,
// router failure, retries exhausted — with a cloud route available the
// whole time, and proves it is never called.
func TestLocalOnlyNeverReachesCloud(t *testing.T) {
	type path struct {
		local     *fakeModel
		routerErr error
		state     string
		reason    string
		runs      int
	}
	paths := map[string]path{
		"local answers":   {local: &fakeModel{replies: []string{schoolNightReply}}, state: runtimedb.MailMessageCandidate, runs: 1},
		"local malformed": {local: &fakeModel{replies: []string{"nope"}}, state: runtimedb.MailMessageExtractionFailed, reason: "malformed_output", runs: 1},
		"no local model":  {state: runtimedb.MailMessageSkipped, reason: "no_local_model", runs: 1},
		"local errors, retried": {local: &fakeModel{err: errors.New("connection refused")}, state: runtimedb.MailMessageExtractionFailed,
			reason: "model_unavailable", runs: MaxModelAttempts},
		"router fails": {routerErr: errors.New("db locked"), state: runtimedb.MailMessageExtractionFailed,
			reason: "model_routes_unreadable", runs: MaxModelAttempts},
	}
	for name, p := range paths {
		t.Run(name, func(t *testing.T) {
			hosted := neverCalled(t)
			list := []ModelRoute{cloud(hosted)} // cloud FIRST in the Agent's chain
			if p.local != nil {
				list = append(list, local(p.local))
			}
			e := newEnv(t, routes{list: list, err: p.routerErr}, runtimedb.ProcessingLocal)
			msg := fixture(t, "plain.eml")
			var out []Outcome
			for run := 1; run <= p.runs; run++ {
				e.src.Run = int64(run)
				got, err := e.proc.ProcessBatch(context.Background(), e.src, []Message{msg})
				if run < p.runs {
					if !errors.Is(err, ErrModelUnavailable) || got[0].State != runtimedb.MailMessageRetrying {
						t.Fatalf("run %d: %+v %v, want a retry that commits nothing", run, got, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("final run: %v", err)
				}
				out = got
			}
			if out[0].State != p.state || out[0].Reason != p.reason {
				t.Fatalf("%s: %+v, want %s/%s", name, out[0], p.state, p.reason)
			}
		})
	}

	t.Run("a cloud-authorized source may fall back", func(t *testing.T) {
		hosted := &fakeModel{replies: []string{schoolNightReply}}
		broken := &fakeModel{err: errors.New("connection refused")}
		e := newEnv(t, routes{list: []ModelRoute{cloud(hosted), local(broken)}}, runtimedb.ProcessingCloud)
		out := e.process(t, fixture(t, "plain.eml"))
		if out[0].State != runtimedb.MailMessageCandidate || broken.calls() != 1 || hosted.calls() != 1 {
			t.Fatalf("cloud fallback: %+v (local %d, cloud %d)", out[0], broken.calls(), hosted.calls())
		}
	})
}

// TestButlerRouterClassifiesHostedProvidersAsCloud: a model record that
// says "local" but names a hosted provider is cloud, so a local-only
// source cannot reach it through a mislabelled route.
func TestButlerRouterClassifiesHostedProvidersAsCloud(t *testing.T) {
	e := newEnv(t, nil, runtimedb.ProcessingLocal)
	for _, m := range []runtimedb.Model{
		{Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true},
		{Name: "mislabelled", Kind: runtimedb.ModelLocal, Provider: "anthropic", ProviderModel: "big", Readiness: runtimedb.ModelReady, Enabled: true},
	} {
		if err := e.db.UpsertModel(m, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	hosted := neverCalled(t)
	build := func(runtimedb.Butler) (provider.Provider, error) { return hosted, nil }
	for model, want := range map[string]string{"kitchen-box": runtimedb.ModelLocal, "mislabelled": runtimedb.ModelCloud} {
		seq, err := e.db.CreateButler(runtimedb.Butler{Name: "agent-" + model, Surface: runtimedb.ButlerSurfaceFamily,
			AgentClass: runtimedb.AgentClassMember, Model: model}, "mika")
		if err != nil {
			t.Fatal(err)
		}
		got, err := ButlerModelRouter{DB: e.db, Build: build}.ExtractionRoutes(context.Background(), seq)
		if err != nil || len(got) != 1 || got[0].Location != want {
			t.Fatalf("%s: %+v %v, want %s", model, got, err, want)
		}
		if model == "mislabelled" {
			e.src.Agent = seq
			e.proc.Extractor.Router = ButlerModelRouter{DB: e.db, Build: build}
			out := e.process(t, fixture(t, "plain.eml"))
			if out[0].State != runtimedb.MailMessageSkipped || out[0].Reason != "no_local_model" {
				t.Fatalf("mislabelled hosted route: %+v", out[0])
			}
		}
	}
}

func TestUnreadableMessagesAreSkippedWithAReason(t *testing.T) {
	e := newEnv(t, routes{list: []ModelRoute{local(neverCalled(t))}}, runtimedb.ProcessingLocal)
	for name, reason := range map[string]string{
		"nested-deep.eml":           "too_deep",
		"many-parts.eml":            "oversized",
		"malformed-no-boundary.eml": "malformed",
	} {
		out := e.process(t, fixture(t, name))
		if out[0].State != runtimedb.MailMessageSkipped || out[0].Reason != reason {
			t.Errorf("%s: %+v, want skipped/%s", name, out[0], reason)
		}
	}
	past := fixture(t, "ics-invite.eml")
	e.proc.Now = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }
	if out := e.process(t, past); out[0].State != runtimedb.MailMessageNoEvent || out[0].Reason != "past_event" {
		t.Fatalf("an event already over: %+v", out[0])
	}
}
