package api

// Epic #326's acceptance proofs, end to end (S08, #461). See
// mail_e2e_harness_test.go for what is real and what is faked.
//
//   - The school-night scenario for two admitted adults on one provider,
//     separate accounts, different filters and target calendars: mail →
//     adapter → scheduled dispatch → candidate → private review → Add →
//     one event, with evidence, the excerpt gone, and replays of the
//     message and of the approval creating nothing.
//   - The cross-Person denial matrix: message read, candidate, notice,
//     approval, calendar and health/status.
//   - Capture: no credential and no full message body in any API
//     response, log line, the runtime database (task traces, events,
//     audit), a model request, or a backup copy; no sender or subject
//     in a log line or the database.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestSchoolNightEndToEndForTwoIsolatedAccounts(t *testing.T) {
	m := newMailE2E(t, "sam", "bo")
	m.model.replies[m.dinnerSentence()] = m.awardsReply()
	ny, _ := time.LoadLocation("America/New_York")
	// bo's private evening overlaps sam's school night; sam may not read
	// bo-private, so it must never reach her review.
	m.dav.Seed("bo-private", "bo-thing", "Bo's private appointment",
		m.schoolDay.Add(17*time.Hour), m.schoolDay.Add(21*time.Hour))

	// First cadence: each monitor commits a baseline and reads nothing
	// that was already in the mailbox.
	m.cycle(t)
	for _, conn := range []string{"sam-mail", "bo-mail"} {
		runs, _ := m.s.DB.ListSubscriptionRuns(m.subs[conn], 5)
		if len(runs) != 1 || runs[0].State != runtimedb.SubscriptionRunSucceeded {
			t.Fatalf("%s baseline run: %+v", conn, runs)
		}
	}

	// The school writes to both; the club writes to bo.
	sam, bo := "sam@example.internal", "bo@example.internal"
	m.provider.Deliver(sam, "INBOX", m.schoolNight(sam))
	m.provider.Deliver(bo, "INBOX", m.schoolNight(bo))
	m.provider.Deliver(bo, "INBOX", m.awardsDinner(bo))
	m.cycle(t)

	// sam: exactly one candidate, from the invitation, in the household
	// zone, on her own target calendar, offered privately.
	samList := m.candidates(t, "sam")
	if len(samList) != 1 {
		t.Fatalf("sam has %d candidates, want 1", len(samList))
	}
	samC := samList[0]
	wantStart := m.schoolDay.Add(18 * time.Hour)
	if samC.Fields.Title != "School family night" || !samC.Fields.Start.Equal(wantStart) ||
		samC.TargetCalendar != "sam-family" || samC.State != runtimedb.MailCandidateProposed {
		t.Fatalf("sam's candidate: %+v", samC)
	}
	if samC.Fields.Start.In(ny).Hour() != 18 {
		t.Fatalf("not resolved in the household zone: %s", samC.Fields.Start)
	}
	samView, code, _ := m.candidate(t, "sam", samC.ID)
	if code != http.StatusOK || samView.Tokens["add"] == "" || samView.Review == nil {
		t.Fatalf("sam's review: %d %+v", code, samView)
	}
	for _, cf := range samView.Review.Conflicts {
		if cf.Calendar == "bo-private" || strings.Contains(cf.Title, "Bo") {
			t.Fatalf("sam's review names a conflict she may not see: %+v", cf)
		}
	}
	if samNotices, _ := m.s.DB.ListNotices(10, runtimedb.NoticeFilter{Recipient: "sam"}); len(samNotices) != 1 {
		t.Fatalf("sam has %d notices, want her one review", len(samNotices))
	}

	// bo: the school's mail is outside his filter; the club's dinner,
	// read by his local model, is his one candidate, on his calendar.
	boList := m.candidates(t, "bo")
	if len(boList) != 1 || boList[0].Fields.Title != "Club awards dinner" || boList[0].TargetCalendar != "bo-club" {
		t.Fatalf("bo's candidates: %+v", boList)
	}
	boC := boList[0]

	// Nothing is on any calendar until a Person approves.
	if ev := m.events(); len(ev) != 0 {
		t.Fatalf("events before approval: %v", ev)
	}

	// sam approves the exact revision.
	var added struct {
		Outcome string `json:"outcome"`
	}
	if code, body := m.home("sam", http.MethodPost, "/api/v1/home/mail/act", map[string]string{"token": samView.Tokens["add"]}, &added); code != http.StatusOK || added.Outcome != "added" {
		t.Fatalf("sam adds = %d %s", code, body)
	}
	ev := m.events()
	if len(ev) != 1 || len(ev["sam-family"]) != 1 || ev["sam-family"][0] != runtimedb.MailEventUID(samC.ID)+".ics" {
		t.Fatalf("events after sam's approval: %v", ev)
	}
	// Evidence, and the excerpt is gone.
	after, _, _ := m.candidate(t, "sam", samC.ID)
	if after.Candidate.State != runtimedb.MailCandidateCreated || after.Excerpt != "" {
		t.Fatalf("after Add: %s, excerpt %q", after.Candidate.State, after.Excerpt)
	}
	effects, _ := m.s.DB.ListMailEffects("sam", samC.ID)
	if len(effects) != 1 || effects[0].State != runtimedb.MailEffectConfirmed || effects[0].Calendar != "sam-family" ||
		effects[0].UID != runtimedb.MailEventUID(samC.ID) || effects[0].ETag == "" {
		t.Fatalf("effect evidence: %+v", effects)
	}

	// Replaying the message: the provider renumbers the folder, the
	// monitor reads everything again under new UIDs, and nothing new
	// is asked or created.
	m.provider.ResetUIDValidity(sam, "INBOX")
	m.provider.ResetUIDValidity(bo, "INBOX")
	m.cycle(t)
	if got := m.candidates(t, "sam"); len(got) != 1 {
		t.Fatalf("a replayed message made sam %d candidates", len(got))
	}
	if got := m.candidates(t, "bo"); len(got) != 1 {
		t.Fatalf("a replayed message made bo %d candidates", len(got))
	}
	if n, _ := m.s.DB.ListNotices(10, runtimedb.NoticeFilter{Recipient: "sam"}); len(n) != 1 {
		t.Fatalf("a replayed message re-asked sam (%d notices)", len(n))
	}
	// Replaying the approval: the token, then the exact revision.
	if code, _ := m.home("sam", http.MethodPost, "/api/v1/home/mail/act", map[string]string{"token": samView.Tokens["add"]}, nil); code == http.StatusOK {
		t.Fatal("a replayed approval token was honoured")
	}
	if code, _ := m.home("sam", http.MethodPost, "/api/v1/home/mail/candidates/"+samC.ID+"/act",
		map[string]any{"revision": samC.Revision, "action": "add"}, nil); code == http.StatusOK {
		t.Fatal("a replayed approval by revision was honoured")
	}
	if ev := m.events(); len(ev) != 1 || len(ev["sam-family"]) != 1 {
		t.Fatalf("events after the replays: %v", ev)
	}
	if puts := m.dav.PutCount(); puts != 1 {
		t.Fatalf("%d calendar writes for one approval", puts)
	}

	// bo approves his own, independently, onto his own calendar.
	boView, _, _ := m.candidate(t, "bo", boC.ID)
	if code, body := m.home("bo", http.MethodPost, "/api/v1/home/mail/act", map[string]string{"token": boView.Tokens["add"]}, nil); code != http.StatusOK {
		t.Fatalf("bo adds = %d %s", code, body)
	}
	if ev := m.events(); len(ev["bo-club"]) != 1 || len(ev["sam-family"]) != 1 || len(ev) != 2 {
		t.Fatalf("events after bo's approval: %v", ev)
	}

	t.Run("denial matrix", func(t *testing.T) { m.denialMatrix(t, samC, samView) })
	t.Run("capture", func(t *testing.T) { m.captureChecks(t) })

	if v := m.provider.Violations(); len(v) > 0 {
		t.Fatalf("mutating IMAP commands reached the provider: %v", v)
	}
}

// denialMatrix: bo — and the Owner, and an Agent's credential — against
// every path to sam's mail: the message, the candidate, the notice, the
// approval, the calendar, and the monitor's health and status.
func (m *mailE2E) denialMatrix(t *testing.T, samC runtimedb.MailCandidate, samView e2eCandidate) {
	db := m.s.DB
	var boBodies []string
	bo := func(method, path string, body any) (int, string) {
		t.Helper()
		code, raw := m.home("bo", method, path, body, nil)
		boBodies = append(boBodies, raw)
		return code, raw
	}
	denied := func(what string, code int) {
		t.Helper()
		if code < 400 {
			t.Errorf("%s: %d, want a refusal", what, code)
		}
	}

	// Message read. bo's adapter is bound to bo-mail on its node: it
	// cannot claim sam's work, and a reference minted for sam's mailbox
	// is refused by bo's even though the provider, folder and UID exist.
	samWork, err := db.EnqueueIntegrationWork("sam-mail", runtimedb.IntegrationWork{Kind: runtimedb.WorkQuery,
		Capability: mail.CapMessage, RequestJSON: `{"ref":"` + samC.SourceRef + `"}`}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if did, err := m.adapters["bo-mail"].runner.RunOnce(context.Background()); err != nil || did {
		t.Fatalf("bo's adapter claimed sam's work: %v %v", did, err)
	}
	_ = db.CancelIntegrationWork(samWork, "test")
	crossRef, err := db.EnqueueIntegrationWork("bo-mail", runtimedb.IntegrationWork{Kind: runtimedb.WorkQuery,
		Capability: mail.CapMessage, RequestJSON: `{"ref":"` + samC.SourceRef + `"}`}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	m.drainAdapter(t, m.adapters["bo-mail"])
	if w, _ := db.GetIntegrationWork(crossRef); w.ResultType != runtimedb.ResultUnavailable || !strings.Contains(w.Error, mail.OutcomeBadRequest) {
		t.Fatalf("sam's reference through bo's mailbox = %s %q", w.ResultType, w.Error)
	} else if strings.Contains(w.ResultJSON, "family night") || strings.Contains(w.ResultJSON, samTail) {
		t.Fatal("a refused reference leaked content")
	}
	// Nor may bo (or any Person) queue reads at all.
	code, _ := bo(http.MethodPost, "/api/v1/integrations/sam-mail/work", map[string]any{"kind": "query", "capability": mail.CapMessage, "request": `{}`})
	denied("bo queueing a read of sam's mailbox", code)

	// Candidate: list, retrieve, revision.
	if list := m.candidates(t, "bo"); len(list) != 1 || list[0].ID == samC.ID {
		t.Fatalf("bo lists %+v", list)
	}
	code, _ = bo(http.MethodGet, "/api/v1/home/mail/candidates/"+samC.ID, nil)
	denied("bo retrieving sam's candidate", code)
	code, raw := m.owner(http.MethodGet, "/api/v1/mail/candidates/"+samC.ID, nil, nil)
	denied("the Owner retrieving sam's candidate", code)
	if code, raw = m.owner(http.MethodGet, "/api/v1/mail/candidates", nil, nil); strings.Contains(raw, samC.ID) {
		t.Fatal("the Owner lists sam's candidate")
	}
	res := m.do(runtimedb.RoleButler, http.MethodGet, "/api/v1/mail/candidates/"+samC.ID, nil, nil)
	denied("an Agent credential retrieving sam's candidate", res.StatusCode)

	// Notice: sam's review reached sam only.
	if n, _ := db.ListNotices(20, runtimedb.NoticeFilter{Recipient: "bo"}); len(n) != 1 {
		t.Fatalf("bo has %d notices; his own review is the only one", len(n))
	}
	if n, _ := db.ListNotices(20, runtimedb.NoticeFilter{Recipient: "mika"}); len(n) != 0 {
		t.Fatalf("the Owner received %d mail notices", len(n))
	}
	bo(http.MethodGet, "/api/v1/home/summary", nil)

	// Approval: sam's token, sam's exact revision, in bo's hands; the
	// Owner's; an Agent's. The candidate does not move.
	code, _ = bo(http.MethodPost, "/api/v1/home/mail/act", map[string]string{"token": samView.Tokens["dismiss"]})
	denied("bo using sam's token", code)
	code, _ = bo(http.MethodPost, "/api/v1/home/mail/candidates/"+samC.ID+"/act", map[string]any{"revision": samC.Revision, "action": "dismiss"})
	denied("bo acting on sam's revision", code)
	code, _ = m.owner(http.MethodPost, "/api/v1/mail/candidates/"+samC.ID+"/act", map[string]any{"revision": samC.Revision, "action": "dismiss"}, nil)
	denied("the Owner acting on sam's candidate", code)
	res = m.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/mail/act", map[string]string{"token": samView.Tokens["dismiss"]}, nil)
	denied("an Agent credential acting", res.StatusCode)
	if c, _ := db.MailCandidateInternal(samC.ID); c.State != runtimedb.MailCandidateCreated {
		t.Fatalf("sam's candidate moved to %s", c.State)
	}

	// Calendar: bo cannot point his monitor at sam's calendar, cannot
	// see it as a conflict, and bo's review named none of sam's events.
	watch := map[string]any{"agent": m.agent}
	for k, v := range e2eWatch["bo"] {
		watch[k] = v
	}
	watch["targetCalendar"] = "sam-family"
	code, _ = bo(http.MethodPost, "/api/v1/home/integrations/bo-mail/subscription", watch)
	denied("bo targeting sam's calendar", code)
	watch["targetCalendar"], watch["conflictCalendars"] = "bo-club", []string{"sam-family"}
	code, _ = bo(http.MethodPost, "/api/v1/home/integrations/bo-mail/subscription", watch)
	denied("bo checking sam's calendar for conflicts", code)
	if rv, err := db.MailReviewFor("bo", m.candidates(t, "bo")[0].ID); err == nil {
		for _, cf := range rv.Conflicts {
			if cf.Calendar == "sam-family" {
				t.Fatalf("bo's review named sam's event: %+v", cf)
			}
		}
	}

	// Health and status: bo's setup shows bo's monitor only; sam's
	// credential, authorization and subscription controls refuse him.
	_, raw = bo(http.MethodGet, "/api/v1/home/mail/setup", nil)
	if strings.Contains(raw, "sam-mail") || strings.Contains(raw, "sam@example.internal") {
		t.Fatalf("bo's setup shows sam's monitor: %s", raw)
	}
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/home/integrations/sam-mail/credential"},
		{http.MethodPost, "/api/v1/home/integrations/sam-mail/credential/revoke"},
		{http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization/revoke"},
	} {
		code, _ := bo(p.method, p.path, nil)
		denied("bo "+p.method+" "+p.path, code)
	}
	code, _ = bo(http.MethodPost, "/api/v1/home/agent-subscriptions/"+itoa(m.subs["sam-mail"])+"/control", map[string]string{"action": "pause"})
	denied("bo pausing sam's monitor", code)
	if _, err := db.LiveBackgroundAuthorization("sam-mail"); err != nil {
		t.Fatalf("bo's attempts changed sam's monitor: %v", err)
	}

	// Infer: nothing bo was answered names any part of sam's message or
	// candidate.
	for _, body := range boBodies {
		for _, secret := range []string{samC.ID, samC.Fingerprint, samC.SourceRef, "School family night", "family night", samTail,
			schoolSender, "sam@example.internal", e2ePasswords["sam"]} {
			if secret != "" && strings.Contains(body, secret) {
				t.Fatalf("bo was answered something of sam's (%q): %s", secret, body)
			}
		}
	}
}


// captureChecks hunts every credential and full body everywhere it
// could have leaked.
func (m *mailE2E) captureChecks(t *testing.T) {
	never := map[string]string{
		"sam's app password": e2ePasswords["sam"], "bo's app password": e2ePasswords["bo"],
		"sam's message tail": samTail, "bo's message tail": boTail,
	}
	// Fingerprint keys and adapter tokens never leave the nodes either.
	for _, a := range m.adapters {
		never[a.connection+" adapter token"] = a.cfg.Token
	}
	for _, conn := range []string{"sam-mail", "bo-mail"} {
		for _, n := range []string{"n1", "n2"} {
			if v, ok := m.nodes.holds(n, "pc."+conn+".fk.v1"); ok {
				never[conn+" fingerprint key"] = v
			}
		}
	}
	quiet := map[string]string{"school sender": schoolSender, "club sender": clubSender,
		"sam's subject": "Invitation: School family night", "bo's subject": "Awards dinner"}

	logs := m.logs.String()
	db := m.dbBytes(t)
	backup := filepath.Join(t.TempDir(), "keystone.db")
	if err := m.s.DB.BackupTo(backup); err != nil {
		t.Fatal(err)
	}
	backupBytes, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	var modelText bytes.Buffer
	for _, req := range m.model.requests() {
		if len(req.Tools) != 0 {
			t.Errorf("the extraction model was offered %d tools", len(req.Tools))
		}
		raw, _ := json.Marshal(req)
		modelText.Write(raw)
	}
	if modelText.Len() == 0 {
		t.Fatal("no model request was captured; bo's prose message should have reached his local model")
	}
	if !strings.Contains(modelText.String(), "awards dinner") {
		t.Fatal("the model request did not carry the bounded message text")
	}
	if strings.Contains(modelText.String(), "School family night") {
		t.Fatal("an invitation read deterministically still went to a model")
	}
	for what, v := range never {
		if v == "" {
			continue
		}
		for where, hay := range map[string]string{"an API response": strings.Join(m.responses, "\n"), "the log": logs,
			"the runtime database": string(db), "a model request": modelText.String(), "a backup copy": string(backupBytes)} {
			if strings.Contains(hay, v) {
				t.Errorf("%s appears in %s", what, where)
			}
		}
	}
	for what, v := range quiet {
		for where, hay := range map[string]string{"the log": logs, "the runtime database": string(db), "a backup copy": string(backupBytes)} {
			if strings.Contains(hay, v) {
				t.Errorf("%s appears in %s", what, where)
			}
		}
	}
	// The monitor opened no Agent task for any of this: there is no
	// task trace to hold mail.
	if n := countRows(t, m, `SELECT COUNT(*) FROM butler_tasks`); n != 0 {
		t.Errorf("the monitor opened %d tasks", n)
	}
	// The excerpt key is beside the database, never inside it.
	if bytes.Contains(db, []byte("mail-excerpts.key")) {
		t.Error("the runtime database names the excerpt key file")
	}
}

func countRows(t *testing.T, m *mailE2E, q string) int {
	t.Helper()
	n, err := m.s.DB.CountForTest(q)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
