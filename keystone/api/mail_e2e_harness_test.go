package api

// The end-to-end harness for epic #326's proofs (S08, #461): a real
// Keystone (this package's HTTP server and runtime database), the real
// scheduled-subscription engine and mailbox monitor, the real private
// review engine and exact calendar path, and — for each placement — the
// real wild-mail-adapter configuration (keystone/adapter/mail/mailadapter)
// built from the very container environment an approved render Plan put
// in desired state, its secret references resolved from the fake node's
// secret store the credential intake wrote to, running adapter.Runner
// with mail.Upstream over verified TLS to imapfake. The calendar is
// caldavfake, the CalDAV server S06's tests use.
//
// Every setup step goes through the public API in the order the product
// takes it: an Owner renders the placement, the Person enters their
// credential, the Person files the subscription, an Owner approves it,
// the Person authorizes it, an Owner approves the render Plan, and the
// placement is marked ready.

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/caldav/caldavfake"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/imapread/imapfake"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/mailadapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/automation"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"github.com/chickenrat17/wildstone-homelab/keystone/mailmonitor"
	"github.com/chickenrat17/wildstone-homelab/keystone/mailreview"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/sharedcalendar"
)

// The scenario's dates follow the real clock (the API reads tokens and
// excerpts at time.Now): the school night is eight days out, 18:00–19:30
// in New York; bo's club dinner is thirteen days out, 19:00–21:00.
func e2eDays(base time.Time, days int) time.Time {
	ny, _ := time.LoadLocation("America/New_York")
	d := base.In(ny).AddDate(0, 0, days)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, ny)
}

// Each Person's app password, the credential the capture checks hunt.
var e2ePasswords = map[string]string{
	"sam": "REPLACED-sam-app-password-7d1f0c",
	"bo":  "REPLACED-bo-app-password-93ae42",
}

type e2eClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *e2eClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *e2eClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recordingModel is the monitor Agent's local model. It records every
// request so the capture checks can read exactly what a model was sent.
type recordingModel struct {
	mu      sync.Mutex
	replies map[string]string // body marker -> structured reply
	down    int               // fail this many calls
	reqs    []provider.ChatRequest
}

func (m *recordingModel) Name() string { return "kitchen-box" }

func (m *recordingModel) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req)
	if m.down > 0 {
		m.down--
		return nil, fmt.Errorf("model host unreachable")
	}
	var text string
	for _, msg := range req.Messages {
		text += msg.Text
	}
	for marker, reply := range m.replies {
		if strings.Contains(text, marker) {
			return &provider.ChatResponse{Text: reply}, nil
		}
	}
	return &provider.ChatResponse{Text: `{"isEvent":false,"title":"","date":"","startTime":"","endTime":"","durationMinutes":0,` +
		`"allDay":false,"timezone":"","location":"","notes":"","confidence":0.1,"missing":[],"ambiguous":[],"evidence":"","isCorrection":false}`}, nil
}

func (m *recordingModel) requests() []provider.ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]provider.ChatRequest(nil), m.reqs...)
}

// flakyHook is the review hook with a crash a test can schedule: the
// next n deliveries fail as if the process died before the review was
// handed over.
type flakyHook struct {
	mu   sync.Mutex
	next mailmonitor.CandidateHook
	fail int
}

func (h *flakyHook) CandidateChanged(ctx context.Context, ch runtimedb.MailCandidateChange) error {
	h.mu.Lock()
	if h.fail > 0 {
		h.fail--
		h.mu.Unlock()
		return fmt.Errorf("crashed before the review was delivered")
	}
	h.mu.Unlock()
	return h.next.CandidateChanged(ctx, ch)
}

// e2eAdapter is one running placement of one connection.
type e2eAdapter struct {
	connection, node string
	cfg              mailadapter.Config
	runner           *adapter.Runner
}

// mailE2E is the whole household.
type mailE2E struct {
	*credentialHarness
	provider *imapfake.Server
	roots    *x509.CertPool
	dav      *caldavfake.Server
	clock    *e2eClock
	model    *recordingModel
	hook     *flakyHook
	sched    *automation.SubscriptionEngine
	review   *mailreview.Engine
	proc     *mailmonitor.Processor
	vault    *runtimedb.MailExcerptVault

	collectMu sync.Mutex
	collect   []func()

	accessMu sync.Mutex
	access   map[string]map[string]string

	schoolDay, dinnerDay time.Time

	adapters  map[string]*e2eAdapter // connection -> the lease holder's adapter
	subs      map[string]int64       // connection -> subscription
	responses []string               // every API body, for the capture checks
}

func (m *mailE2E) record(body string) string {
	m.responses = append(m.responses, body)
	return body
}

// home calls the Homepage API as a Person and keeps the body.
func (m *mailE2E) home(user, method, path string, body, out any) (int, string) {
	m.t.Helper()
	code, raw := m.homeCall(user, method, path, body, out)
	return code, m.record(raw)
}

// owner calls the Console API as the Owner and keeps the body.
func (m *mailE2E) owner(method, path string, body, out any) (int, string) {
	m.t.Helper()
	code, raw := m.consoleCall(m.mika, method, path, body, out)
	return code, m.record(raw)
}

// e2eWatch is how each Person sets up their monitor: separate accounts,
// different filters, different target calendars.
var e2eWatch = map[string]map[string]any{
	"sam": {
		"targetCalendar": "sam-family", "processingLocation": "local", "cadence": "15m",
		"scope":             map[string]any{"folders": []string{"INBOX"}, "senders": []string{"school.example.internal"}},
		"conflictCalendars": []string{"household"},
	},
	"bo": {
		"targetCalendar": "bo-club", "processingLocation": "local", "cadence": "15m",
		"scope":             map[string]any{"folders": []string{"INBOX"}, "senders": []string{"club.example.internal"}},
		"conflictCalendars": []string{"household", "bo-private"},
	},
}

func newMailE2E(t *testing.T, people ...string) *mailE2E {
	t.Helper()
	c := newCredentialHarness(t)
	logs := c.logs
	installMailForTest(t, c)
	db := c.s.DB
	// The harness already holds the coordinator lease (execution
	// authority), as a running Keystone does.

	// The calendars: sam writes sam-family, bo writes bo-club and his
	// private one, everyone reads and writes the household's.
	reg := `{"calendars":[
	  {"id":"household","name":"Household","color":"#3b82f6","default":true,"readGroups":["homepage_users"],"writeGroups":["homepage_users"]},
	  {"id":"sam-family","name":"Sam's family","color":"#22c55e","readGroups":[],"writeGroups":[],"readMembers":["sam"],"writeMembers":["sam"]},
	  {"id":"bo-club","name":"Bo's club","color":"#f59e0b","readGroups":[],"writeGroups":[],"readMembers":["bo"],"writeMembers":["bo"]},
	  {"id":"bo-private","name":"Bo's private","color":"#ef4444","readGroups":[],"writeGroups":[],"readMembers":["bo"],"writeMembers":["bo"]}
	]}`
	if err := c.s.State.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindConfigFile,
		Metadata:   resource.Metadata{Name: sharedcalendar.ConfigName, Owner: "capability/calendar", Node: "n1"},
		ConfigFile: &resource.ConfigFileSpec{Filename: "calendars.json", Content: reg},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.s.State.Commit("test", "calendars", "calendar registry"); err != nil {
		t.Fatal(err)
	}

	serverTLS, roots, err := imapfake.Certificate("imap.example.internal")
	if err != nil {
		t.Fatal(err)
	}
	prov, err := imapfake.Start(imapfake.Options{TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { prov.Close() })
	dav := caldavfake.New()
	t.Cleanup(dav.Close)

	ny, _ := time.LoadLocation("America/New_York")
	base := time.Now().UTC().Truncate(time.Minute)
	m := &mailE2E{credentialHarness: c, provider: prov, roots: roots, dav: dav, clock: &e2eClock{t: base},
		schoolDay: e2eDays(base, 8), dinnerDay: e2eDays(base, 13),
		model:    &recordingModel{replies: map[string]string{}},
		adapters: map[string]*e2eAdapter{}, subs: map[string]int64{},
		access: map[string]map[string]string{
			"sam":  {"sam-family": "rw", "household": "rw"},
			"bo":   {"bo-club": "rw", "household": "rw", "bo-private": "rw"},
			"mika": {"household": "rw"},
		}}
	vault, err := db.OpenMailExcerptVault(t.TempDir() + "/mail-excerpts.key")
	if err != nil {
		t.Fatal(err)
	}
	m.vault = vault
	log := redact.Logger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	cal := &domestic.Calendar{Authority: db, Location: ny, Now: m.clock.Now,
		Resolve: func(member, id string, write bool) (*caldav.Upstream, string, error) {
			m.accessMu.Lock()
			grant := m.access[member][id]
			m.accessMu.Unlock()
			if grant == "" || (write && grant != "rw") {
				return nil, "", fmt.Errorf("%w: %s on %s", domestic.ErrCalendarAccess, member, id)
			}
			return &caldav.Upstream{CollectionURL: dav.URL + "/cal/" + id + "/", Writable: true, Location: ny,
				Now: m.clock.Now, HTTP: &http.Client{Timeout: 300 * time.Millisecond}}, id, nil
		}}
	m.review = &mailreview.Engine{DB: db, Vault: vault, Calendar: cal, Now: m.clock.Now, Log: log}
	c.s.MailReview = m.review
	m.hook = &flakyHook{next: m.review}
	m.proc = &mailmonitor.Processor{DB: db, Vault: vault, Zone: householdtz.New(ny), Hook: m.hook, Now: m.clock.Now, Log: log,
		Extractor: &mailmonitor.Extractor{Router: mailmonitor.ButlerModelRouter{DB: db,
			Build: func(runtimedb.Butler) (provider.Provider, error) { return m.model, nil }}}}
	m.sched = &automation.SubscriptionEngine{DB: db, Now: m.clock.Now, Log: log,
		Authority: automation.PersonalBackgroundAuthority{DB: db}}
	m.sched.Dispatchers = map[string]automation.ScheduledDispatcher{mailmonitor.MatchKind: mailmonitor.NewDispatcher(mailmonitor.Config{
		DB: db, Processor: m.proc, Runs: m.sched, Poll: time.Millisecond, Now: m.clock.Now, Log: log,
		Go: func(f func()) { m.collectMu.Lock(); m.collect = append(m.collect, f); m.collectMu.Unlock() },
	})}

	for _, p := range people {
		m.enable(t, p)
	}
	return m
}

// enable walks one Person's monitor through the product's setup order.
func (m *mailE2E) enable(t *testing.T, person string) {
	t.Helper()
	db := m.s.DB
	conn := person + "-mail"
	if _, err := db.GetConnection(conn); err != nil {
		if _, err := db.CreateConnection(runtimedb.IntegrationConnection{Name: conn, Adapter: "wild-mail-adapter",
			Ownership: runtimedb.OwnershipPersonal, Owner: person, CreatedBy: "root-admin"}); err != nil {
			t.Fatal(err)
		}
		prepare(t, m.harness, conn, "n1", runtimedb.PlacementPrimary, 0, true)
		prepare(t, m.harness, conn, "n2", runtimedb.PlacementFallback, 1, true)
		for _, n := range []string{"n1", "n2"} {
			m.nodes.mu.Lock()
			m.nodes.containers[n][capability.AdapterContainerName("mail", conn, n)] = true
			m.nodes.mu.Unlock()
		}
	}
	login := person + "@example.internal"
	m.provider.AddUser(login, e2ePasswords[person])

	// 1. The Owner prepares the placement (records the executable, mints
	//    node-bound adapter tokens). Nothing is desired yet.
	if code, body := m.owner(http.MethodPost, "/api/v1/integrations/"+conn+"/adapter/render", nil, nil); code != http.StatusOK {
		t.Fatalf("%s render = %d %s", conn, code, body)
	}
	// 2. The Person enters their own credential.
	if code, body := m.home(person, http.MethodPost, "/api/v1/home/integrations/"+conn+"/credential",
		map[string]string{"login": login, "password": e2ePasswords[person]}, nil); code != http.StatusOK {
		t.Fatalf("%s credential = %d %s", person, code, body)
	}
	// 3. The Person files the subscription; 4. an Owner approves it.
	watch := map[string]any{"agent": m.agent}
	for k, v := range e2eWatch[person] {
		watch[k] = v
	}
	var filed struct {
		Plan         struct{ ID string } `json:"plan"`
		Subscription int64               `json:"subscription"`
	}
	if code, body := m.home(person, http.MethodPost, "/api/v1/home/integrations/"+conn+"/subscription", watch, &filed); code != http.StatusOK {
		t.Fatalf("%s subscription = %d %s", person, code, body)
	}
	m.approve(t, filed.Plan.ID)
	setup := m.homeSetup(t, person)
	var sub int64
	for _, mon := range setup.Monitors {
		if mon.Connection.Name == conn && len(mon.Subscriptions) == 1 {
			sub = mon.Subscriptions[0].ID
		}
	}
	if sub == 0 {
		t.Fatalf("%s has no approved subscription: %+v", person, setup.Monitors)
	}
	m.subs[conn] = sub
	// 5. The Person authorizes that approved subscription.
	auth := map[string]any{"agent": m.agent, "subscription": sub}
	for _, k := range []string{"targetCalendar", "processingLocation", "scope"} {
		auth[k] = e2eWatch[person][k]
	}
	if code, body := m.home(person, http.MethodPost, "/api/v1/home/integrations/"+conn+"/authorization", auth, nil); code != http.StatusOK {
		t.Fatalf("%s authorize = %d %s", person, code, body)
	}
	// 6. The Owner approves the render Plan the authorization filed.
	renders := proposedRenders(t, m.credentialHarness, conn)
	if len(renders) != 1 {
		t.Fatalf("%s: %d render plans", conn, len(renders))
	}
	m.approve(t, renders[0].ID)
	// 7. The primary is marked ready and takes the lease.
	if code, body := m.owner(http.MethodPost, "/api/v1/integrations/"+conn+"/placement/ready",
		map[string]any{"node": "n1", "containerRunning": true, "secretProvisioned": true, "readinessVerified": true}, nil); code != http.StatusOK {
		t.Fatalf("%s ready = %d %s", conn, code, body)
	}
	m.startAdapter(t, conn, "n1")
}

// startAdapter builds the adapter exactly as the program does, from the
// rendered container's environment on node, and makes it the one that
// runs for the connection.
func (m *mailE2E) startAdapter(t *testing.T, conn, node string) *e2eAdapter {
	t.Helper()
	running := containersByNode(adapterResources(t, m.credentialHarness, conn))
	ct, ok := running[node]
	if !ok {
		t.Fatalf("no rendered adapter for %s on %s", conn, node)
	}
	host, port, _ := net.SplitHostPort(m.provider.Addr())
	getenv := func(k string) string {
		switch k {
		case capability.AdapterEnvKeystoneAPI:
			return m.srv.URL // the rendered address is a documentation IP
		case capability.AdapterEnvParamPrefix + "IMAP_HOST":
			return host
		case capability.AdapterEnvParamPrefix + "IMAP_PORT":
			return port
		}
		v := ct.Container.Env[k]
		if name, ok := strings.CutPrefix(v, "secret/"); ok {
			// Wardstone materializes a secret reference from the node's
			// own store: here, what the intake and the render put there.
			value, held := m.nodes.holds(node, name)
			if !held {
				return ""
			}
			return value
		}
		return v
	}
	cfg, err := mailadapter.LoadConfig(getenv)
	if err != nil {
		t.Fatalf("%s on %s: the rendered environment does not load: %v", conn, node, err)
	}
	cfg.RootCAs = m.roots
	a := &e2eAdapter{connection: conn, node: node, cfg: cfg, runner: &adapter.Runner{
		Client:   &adapter.Client{BaseURL: cfg.KeystoneAPI, Token: cfg.Token},
		Upstream: &mail.Upstream{Settings: cfg.Settings, Reader: mailadapter.ReaderFor(cfg)},
		PollWait: 20 * time.Millisecond,
		Log:      func(format string, args ...any) { m.s.Log.Info(fmt.Sprintf(format, args...)) },
	}}
	if _, ok := a.runner.Upstream.(*mail.Upstream).Reader.(mail.NotConfigured); ok {
		t.Fatalf("%s on %s: the adapter is not configured from its rendered environment", conn, node)
	}
	m.adapters[conn] = a
	return a
}

// drainAdapter answers everything queued for one adapter.
func (m *mailE2E) drainAdapter(t *testing.T, a *e2eAdapter) int {
	t.Helper()
	n := 0
	for {
		did, err := a.runner.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("%s adapter on %s: %v", a.connection, a.node, err)
		}
		if !did {
			return n
		}
		n++
	}
}

// tick runs the scheduler once and returns how many runs it dispatched.
func (m *mailE2E) tick(t *testing.T) automation.ScheduledTickOutcome {
	t.Helper()
	out, err := m.sched.TickScheduled(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return out
}

// runCollectors finishes every run whose read was answered.
func (m *mailE2E) runCollectors() {
	m.collectMu.Lock()
	fs := m.collect
	m.collect = nil
	m.collectMu.Unlock()
	for _, f := range fs {
		f()
	}
}

// dropCollectors is Keystone dying with reads outstanding: nothing it
// was about to record is recorded, and the runs are left in flight.
func (m *mailE2E) dropCollectors() int {
	m.collectMu.Lock()
	defer m.collectMu.Unlock()
	n := len(m.collect)
	m.collect = nil
	return n
}

// cycle is one cadence: tick, every adapter answers, every run finishes.
func (m *mailE2E) cycle(t *testing.T) {
	t.Helper()
	m.tick(t)
	for _, a := range m.adapters {
		m.drainAdapter(t, a)
	}
	m.runCollectors()
	m.clock.advance(16 * time.Minute)
}

// letter is a message from one sender. body goes first; tail follows
// enough filler that it lies beyond any excerpt and, when long is set,
// beyond what a model is ever shown.
func letter(id, from, to, subject, body, tail, ics string, long bool) []byte {
	filler := strings.Repeat("Other school news follows, none of it about dates. ", 12)
	if long {
		filler = strings.Repeat("A long newsletter paragraph about nothing in particular. ", 260)
	}
	text := body + "\r\n\r\n" + filler + "\r\n\r\n" + tail + "\r\n"
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: Wed, 07 Oct 2026 09:00:00 +0000\r\nMessage-ID: <%s@example.internal>\r\nMIME-Version: 1.0\r\n",
		from, to, subject, id)
	if ics == "" {
		fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n\r\n%s", text)
		return b.Bytes()
	}
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n"+
		"--b1\r\nContent-Type: text/calendar; charset=utf-8; method=REQUEST\r\n\r\n%s\r\n--b1--\r\n", text, ics)
	return b.Bytes()
}

// schoolNightICS is the school's invitation.
func (m *mailE2E) schoolNightICS() string {
	day := m.schoolDay.Format("20060102")
	return "BEGIN:VCALENDAR\r\nPRODID:-//School//Office//EN\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
		"UID:family-night@school.example.internal\r\nDTSTAMP:20260901T120000Z\r\n" +
		"DTSTART;TZID=America/New_York:" + day + "T180000\r\nDTEND;TZID=America/New_York:" + day + "T193000\r\n" +
		"SUMMARY:School family night\r\nLOCATION:School gym\r\n" +
		"ORGANIZER;CN=Front Office:mailto:office@school.example.internal\r\nEND:VEVENT\r\nEND:VCALENDAR"
}

// Tails: text that must never leave the provider — not in an excerpt, a
// model request, a response, a log, the database or a backup.
const (
	samTail = "REPLACED-sam-private-tail-4c1e: the combination to the side gate is on the fridge."
	boTail  = "REPLACED-bo-private-tail-b77a: membership renewal reference follows."
)

// Senders, which appear nowhere but at the provider either.
const (
	schoolSender = "office@school.example.internal"
	clubSender   = "secretary@club.example.internal"
)

// schoolNight is sam's invitation, from the school (inside sam's filter,
// outside bo's).
func (m *mailE2E) schoolNight(to string) []byte {
	return letter("family-night-"+to, "Front Office <"+schoolSender+">", to, "Invitation: School family night",
		"You are invited to School family night on "+m.schoolDay.Format("Monday, January 2")+" at 6pm in the school gym.",
		samTail, m.schoolNightICS(), false)
}

// dinnerSentence is the evidence bo's model quotes.
func (m *mailE2E) dinnerSentence() string {
	return m.dinnerDay.Format("Monday, January 2, 2006") + " from 7:00 to 9:00 p.m."
}

// awardsDinner is bo's club's prose announcement, read by the model. It
// is longer than a model is ever shown.
func (m *mailE2E) awardsDinner(to string) []byte {
	return letter("awards-"+to, "Club Secretary <"+clubSender+">", to, "Awards dinner",
		"The club awards dinner is "+m.dinnerSentence()+" at the club house.", boTail, "", true)
}

// awardsReply is what bo's local model reads from awardsDinner.
func (m *mailE2E) awardsReply() string {
	return `{"isEvent":true,"title":"Club awards dinner","date":"` + m.dinnerDay.Format("2006-01-02") + `","startTime":"19:00","endTime":"21:00",` +
		`"durationMinutes":0,"allDay":false,"timezone":"","location":"Club house","notes":"","confidence":0.9,"missing":[],` +
		`"ambiguous":[],"evidence":"` + m.dinnerSentence() + `","isCorrection":false}`
}

// e2eCandidate is the Person-scoped candidate view the API returns.
type e2eCandidate struct {
	Candidate struct {
		ID       string `json:"id"`
		Revision int    `json:"revision"`
		State    string `json:"state"`
		Fields   struct {
			Title string `json:"title"`
		} `json:"fields"`
		TargetCalendar string `json:"targetCalendar"`
	} `json:"candidate"`
	Tokens map[string]string `json:"tokens"`
	Review *struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		Conflicts []struct {
			Calendar string `json:"calendar"`
			Title    string `json:"title"`
		} `json:"conflicts"`
	} `json:"review"`
	Excerpt string `json:"excerpt"`
}

// candidates lists a Person's own candidates through the Homepage API.
func (m *mailE2E) candidates(t *testing.T, person string) []runtimedb.MailCandidate {
	t.Helper()
	var out struct {
		Candidates []runtimedb.MailCandidate `json:"candidates"`
	}
	if code, body := m.home(person, http.MethodGet, "/api/v1/home/mail/candidates", nil, &out); code != http.StatusOK {
		t.Fatalf("%s candidates = %d %s", person, code, body)
	}
	return out.Candidates
}

// candidate reads one through the Homepage API.
func (m *mailE2E) candidate(t *testing.T, person, id string) (e2eCandidate, int, string) {
	t.Helper()
	var out e2eCandidate
	code, body := m.home(person, http.MethodGet, "/api/v1/home/mail/candidates/"+id, nil, &out)
	return out, code, body
}

// events lists every event the review engine created, per calendar.
func (m *mailE2E) events() map[string][]string {
	out := map[string][]string{}
	for _, p := range m.dav.CandidateObjects() {
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) == 3 {
			out[parts[1]] = append(out[parts[1]], parts[2])
		}
	}
	return out
}

func (m *mailE2E) mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		m.t.Fatal(err)
	}
	return string(raw)
}
