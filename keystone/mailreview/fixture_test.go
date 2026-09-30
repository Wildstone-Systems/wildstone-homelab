package mailreview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// now is a week before the school-night scenario (Thursday 15 October
// 2026, 18:00–19:30 in New York).
var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func newYork() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return l
}

// fakeDAV is an in-process CalDAV server with several collections
// (/cal/<id>/), real conditional-write semantics, and faults a test can
// schedule: a PUT that fails before it is stored, one that is stored
// and then answered too late (the client times out), one that is
// stored and then has its connection dropped, and GETs that fail.
type fakeDAV struct {
	*httptest.Server
	mu      sync.Mutex
	objects map[string]string
	etag    map[string]int
	// PUT faults, consumed one per PUT: "", "fail-before", "hang-after",
	// "drop-after", "forbidden".
	putFaults []string
	// getFaults fails this many GETs with 503.
	getFaults int
	puts      int
	deletes   int
}

func newFakeDAV(t *testing.T) *fakeDAV {
	t.Helper()
	f := &fakeDAV{objects: map[string]string{}, etag: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDAV) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	switch r.Method {
	case "REPORT":
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
		var paths []string
		for p := range f.objects {
			if strings.HasPrefix(p, r.URL.Path) {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(&b, `<response><href>%s</href><propstat><prop><getetag>"%d"</getetag><C:calendar-data>%s</C:calendar-data></prop></propstat></response>`,
				p, f.etag[p], xmlEscape(f.objects[p]))
		}
		b.WriteString(`</multistatus>`)
		f.mu.Unlock()
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(b.String()))
	case http.MethodGet:
		if f.getFaults > 0 {
			f.getFaults--
			f.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		obj, ok := f.objects[r.URL.Path]
		tag := f.etag[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, tag))
		_, _ = w.Write([]byte(obj))
	case http.MethodPut:
		f.puts++
		fault := ""
		if len(f.putFaults) > 0 {
			fault, f.putFaults = f.putFaults[0], f.putFaults[1:]
		}
		switch fault {
		case "fail-before":
			f.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case "forbidden":
			f.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, exists := f.objects[r.URL.Path]
		if r.Header.Get("If-None-Match") == "*" && exists {
			f.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if m := r.Header.Get("If-Match"); m != "" && (!exists || m != fmt.Sprintf(`"%d"`, f.etag[r.URL.Path])) {
			f.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		f.objects[r.URL.Path] = string(body)
		f.etag[r.URL.Path]++
		tag := f.etag[r.URL.Path]
		f.mu.Unlock()
		switch fault {
		case "hang-after":
			time.Sleep(600 * time.Millisecond)
		case "drop-after":
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, tag))
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		f.deletes++
		_, ok := f.objects[r.URL.Path]
		delete(f.objects, r.URL.Path)
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		f.mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// seed puts a plain event on a collection.
func (f *fakeDAV) seed(calendar, uid, summary string, start, end time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := "/cal/" + calendar + "/" + uid + ".ics"
	f.objects[p] = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nSUMMARY:" + summary +
		"\r\nDTSTART:" + start.UTC().Format("20060102T150405Z") + "\r\nDTEND:" + end.UTC().Format("20060102T150405Z") +
		"\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	f.etag[p]++
}

// candidateObjects lists the events the review engine wrote.
func (f *fakeDAV) candidateObjects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.objects {
		if strings.Contains(p, "wildstone-candidate-") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeDAV) object(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[p]
}

func (f *fakeDAV) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

func (f *fakeDAV) schedule(faults ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putFaults = append(f.putFaults, faults...)
}

func (f *fakeDAV) failGets(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getFaults = n
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type allowExecution struct{}

func (allowExecution) CheckHouseholdExecution() error { return nil }

// env: Owner mika approves; Members sam and bo. sam owns sam-mail and
// authorizes Agent hobson to watch it, target calendar sam-family,
// conflicts also checked on household and bo-private. sam can write
// sam-family and read household; she cannot read bo-private. sam has a
// one-to-one Telegram binding to hobson.
type env struct {
	db      *runtimedb.DB
	vault   *runtimedb.MailExcerptVault
	dav     *fakeDAV
	cal     *domestic.Calendar
	eng     *Engine
	clock   *clock
	sub     int64
	agent   int64
	binding runtimedb.ChatBinding

	accessMu sync.Mutex
	access   map[string]map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.UpsertModel(runtimedb.Model{Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama",
		ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"))
	for _, m := range []struct{ name, role string }{{"mika", runtimedb.RoleOwner}, {"sam", runtimedb.RoleMember}, {"bo", runtimedb.RoleMember}} {
		must(db.RecordButlerMember(m.name, m.role, "", ""))
		must(db.AdmitButlerMember(m.name, "root-admin"))
	}
	agent, err := db.CreateButler(runtimedb.Butler{Name: "hobson", Directive: "watch the post",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box"}, "root-admin")
	must(err)
	must(db.GrantButlerAccess("hobson", "sam", "root-admin"))
	_, err = db.CreateConnection(runtimedb.IntegrationConnection{Name: "sam-mail", Adapter: "wild-mail-adapter",
		Ownership: runtimedb.OwnershipPersonal, Owner: "sam", CreatedBy: "sam"})
	must(err)
	must(db.AddPlacement("sam-mail", "n1", runtimedb.PlacementPrimary, 0, "root-admin"))
	must(db.MarkPlacementPrepared("sam-mail", "n1", true, true, true))
	_, _, err = db.PlaceConnection("sam-mail", time.Hour)
	must(err)
	must(db.SetConnectionExecutable("sam-mail", "mail-roundcube@1.1.0/wild-mail-adapter", "root-admin"))
	staged, err := db.StagePersonalCredential("sam-mail", "", "sam@example.internal", "sam", []string{"n1"})
	must(err)
	must(db.MarkCredentialCopy(staged.Seq, "n1", true, 1))
	_, _, err = db.ActivatePersonalCredential(staged.Seq, "sam")
	must(err)
	req := runtimedb.BackgroundAuthorizationRequest{Connection: "sam-mail",
		Scope: runtimedb.BackgroundScope{Folders: []string{"INBOX"}, ConflictCalendars: []string{"household", "bo-private"}},
		ProcessingLocation: runtimedb.ProcessingLocal, Agent: agent, TargetCalendar: "sam-family"}
	preview, err := db.PrepareBackgroundAuthorization("sam", req)
	must(err)
	match, _ := json.Marshal(runtimedb.BackgroundSubscriptionMatch("sam-mail", preview.Digest))
	sub, err := db.ProposeAgentSubscription(runtimedb.AgentSubscriptionSpec{Butler: agent,
		TriggerKind: runtimedb.SubscriptionTriggerSchedule, MatchKind: "mail.changes", MatchJSON: string(match),
		Audience: runtimedb.AudienceOwner, Capabilities: []string{"mail.changes"}, SourceConnection: "sam-mail",
		Cadence: 15 * time.Minute})
	must(err)
	req.Subscription = sub
	_, err = db.AuthorizeBackgroundUseAsPerson("sam", req)
	must(err)
	must(db.ApproveAgentSubscription(sub, "mika"))
	_, err = db.AcquireCoordinator("mail-test")
	must(err)
	vault, err := db.OpenMailExcerptVault(filepath.Join(t.TempDir(), "mail-excerpts.key"))
	must(err)

	// sam's one-to-one Telegram conversation with hobson.
	_, err = db.AddButlerAddress("hobson", "telegram", "hobson-bot", "", "mika")
	must(err)
	at, err := db.ChatConnectionAt("telegram", "hobson-bot")
	must(err)
	must(db.InsertChatClaimCode("hash-sam", runtimedb.ChatClaimCode{Connection: at.Seq, Identity: "sam",
		CreatedBy: "root-admin", ExpiresAt: time.Now().Add(time.Hour)}))
	binding, err := db.RedeemChatClaimCode("hash-sam", at, "tg-sam", "sam", "dm-sam")
	must(err)

	e := &env{db: db, vault: vault, dav: newFakeDAV(t), clock: &clock{t: now}, sub: sub, agent: agent, binding: binding,
		access: map[string]map[string]string{
			"sam":   {"sam-family": "rw", "household": "r"},
			"bo":    {"household": "rw", "bo-private": "rw"},
			"mika": {"household": "rw"},
		}}
	e.cal = &domestic.Calendar{Authority: allowExecution{}, Location: newYork(), Now: e.clock.Now,
		Resolve: func(member, id string, write bool) (*caldav.Upstream, string, error) {
			e.accessMu.Lock()
			grant := e.access[member][id]
			e.accessMu.Unlock()
			if grant == "" || (write && grant != "rw") {
				return nil, "", fmt.Errorf("%s may not %s calendar %s", member, map[bool]string{true: "write", false: "read"}[write], id)
			}
			return &caldav.Upstream{CollectionURL: e.dav.URL + "/cal/" + id + "/", Writable: true, Location: newYork(),
				Now: e.clock.Now, HTTP: &http.Client{Timeout: 300 * time.Millisecond}}, id, nil
		}}
	e.eng = &Engine{DB: db, Vault: vault, Calendar: e.cal, Now: e.clock.Now}
	return e
}

func (e *env) setAccess(member, calendar, grant string) {
	e.accessMu.Lock()
	defer e.accessMu.Unlock()
	if grant == "" {
		delete(e.access[member], calendar)
		return
	}
	e.access[member][calendar] = grant
}

func schoolNight() runtimedb.MailCandidateFields {
	ny := newYork()
	return runtimedb.MailCandidateFields{
		Title: "School family night", Location: "School gym", TZ: "America/New_York",
		Start: time.Date(2026, 10, 15, 18, 0, 0, 0, ny), End: time.Date(2026, 10, 15, 19, 30, 0, 0, ny),
		Confidence: 0.9, Missing: []string{}, Ambiguous: []string{},
	}
}

// record files one message's candidate the way the monitor does and
// hands its changes to the engine, as the monitor's hook would.
func (e *env) record(t *testing.T, fp string, d runtimedb.MailCandidateDraft) runtimedb.MailCandidate {
	t.Helper()
	sam, err := e.db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.db.GetConnection("sam-mail")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.db.RecordMailMessage(e.vault, runtimedb.MailMessageRecord{
		Connection: "sam-mail", Epoch: conn.LeaseEpoch, PersonID: sam.PersonID, Fingerprint: fp, SourceRef: "ref-" + fp,
		TargetCalendar: "sam-family", State: runtimedb.MailMessageNoEvent, Drafts: []runtimedb.MailCandidateDraft{d},
		At: e.clock.Now(), Source: runtimedb.MailSourceModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out runtimedb.MailCandidate
	for _, ch := range res.Changes {
		if err := e.eng.CandidateChanged(context.Background(), ch); err != nil {
			t.Fatalf("hook(%s): %v", ch.Kind, err)
		}
		if ch.Kind == runtimedb.MailChangeNew {
			out = ch.Candidate
		}
	}
	if out.ID == "" {
		t.Fatalf("no new candidate from %s: %+v", fp, res)
	}
	return out
}

func draft(f runtimedb.MailCandidateFields) runtimedb.MailCandidateDraft {
	return runtimedb.MailCandidateDraft{Fields: f, Source: runtimedb.MailSourceModel, MessageKey: "m1", SenderKey: "s1",
		SubjectKey: "subj", Excerpt: "The school family night is Thursday, October 15, 2026 from 6:00 to 7:30 p.m."}
}

// tokens returns sam's live tokens for a candidate.
func (e *env) tokens(t *testing.T, id string) (runtimedb.MailReview, map[string]string) {
	t.Helper()
	r, tok, err := e.db.MailReviewTokens(e.vault, "sam", id, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r, tok
}

func (e *env) act(req ActRequest) (Result, error) {
	if req.Via == "" {
		req.Via = "console"
	}
	return e.eng.Act(context.Background(), req)
}

func (e *env) candidate(t *testing.T, id string) runtimedb.MailCandidate {
	t.Helper()
	c, err := e.db.MailCandidateInternal(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
