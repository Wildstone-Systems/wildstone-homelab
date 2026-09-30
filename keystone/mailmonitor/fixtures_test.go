package mailmonitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail/imapread/mimenorm"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/householdtz"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

const corpus = "../../testing/fixtures/mail"

// now is the week before the school-night scenario.
var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func newYork(t *testing.T) *time.Location {
	t.Helper()
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// fixture reads one corpus message through the same normalizer the
// adapter uses, and wraps it as the adapter's answer would.
func fixture(t *testing.T, name string) Message {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpus, name))
	if err != nil {
		t.Fatal(err)
	}
	return Message{Ref: "ref-" + name, Fingerprint: "mfp1_" + strings.TrimSuffix(name, ".eml"), Folder: "INBOX",
		Size: int64(len(raw)), Content: mimenorm.Normalize(raw, mimenorm.Limits{})}
}

// fakeModel is a scripted provider. Every request is kept so a test can
// prove what a model was (and was not) shown.
type fakeModel struct {
	mu      sync.Mutex
	replies []string
	err     error
	tools   bool
	reqs    []provider.ChatRequest
}

func (m *fakeModel) Name() string { return "fake" }

func (m *fakeModel) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req)
	if m.err != nil {
		return nil, m.err
	}
	if m.tools {
		return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "calendar.create_event"}}}, nil
	}
	if len(m.replies) == 0 {
		return nil, errors.New("fake model: no scripted reply")
	}
	r := m.replies[0]
	if len(m.replies) > 1 {
		m.replies = m.replies[1:]
	}
	return &provider.ChatResponse{Text: r}, nil
}

func (m *fakeModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reqs)
}

// routes is a fixed ModelRouter.
type routes struct {
	list []ModelRoute
	err  error
}

func (r routes) ExtractionRoutes(context.Context, int64) ([]ModelRoute, error) { return r.list, r.err }

func local(m *fakeModel) ModelRoute {
	return ModelRoute{Name: "kitchen-box", Location: runtimedb.ModelLocal, Model: "llama", Provider: m}
}

func cloud(m *fakeModel) ModelRoute {
	return ModelRoute{Name: "hosted", Location: runtimedb.ModelCloud, Model: "big", Provider: m}
}

// hookLog records what the review hook was handed.
type hookLog struct {
	mu      sync.Mutex
	changes []runtimedb.MailCandidateChange
}

func (h *hookLog) CandidateChanged(_ context.Context, c runtimedb.MailCandidateChange) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes = append(h.changes, c)
	return nil
}

type env struct {
	db   *runtimedb.DB
	src  Source
	proc *Processor
	hook *hookLog
	ny   *time.Location
}

// newEnv: Owner mika, Members sam and bo, sam's Personal connection
// sam-mail. The household is in New York.
func newEnv(t *testing.T, router ModelRouter, processing string) *env {
	t.Helper()
	db, err := runtimedb.Open(filepath.Join(t.TempDir(), "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, m := range []struct{ name, role string }{{"mika", runtimedb.RoleOwner}, {"sam", runtimedb.RoleMember}, {"bo", runtimedb.RoleMember}} {
		if err := db.RecordButlerMember(m.name, m.role, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AdmitButlerMember(m.name, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CreateConnection(runtimedb.IntegrationConnection{
		Name: "sam-mail", Adapter: "wild-mail-adapter", Ownership: runtimedb.OwnershipPersonal, Owner: "sam", CreatedBy: "sam",
	}); err != nil {
		t.Fatal(err)
	}
	sam, _ := db.GetButlerMember("sam")
	conn, _ := db.GetConnection("sam-mail")
	vault, err := db.OpenMailExcerptVault(filepath.Join(t.TempDir(), "mail-excerpts.key"))
	if err != nil {
		t.Fatal(err)
	}
	ny := newYork(t)
	h := &hookLog{}
	return &env{
		db:  db,
		src: Source{Connection: "sam-mail", Epoch: conn.LeaseEpoch, PersonID: sam.PersonID, Agent: 1, ProcessingLocation: processing, TargetCalendar: "sam-family", Run: 1},
		proc: &Processor{DB: db, Vault: vault, Zone: householdtz.New(ny), Extractor: &Extractor{Router: router},
			Hook: h, Now: func() time.Time { return now }},
		hook: h,
		ny:   ny,
	}
}

func (e *env) process(t *testing.T, msgs ...Message) []Outcome {
	t.Helper()
	out, err := e.proc.ProcessBatch(context.Background(), e.src, msgs)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	return out
}

func (e *env) candidate(t *testing.T, id string) runtimedb.MailCandidate {
	t.Helper()
	c, err := e.db.GetMailCandidate("sam", id)
	if err != nil {
		t.Fatalf("GetMailCandidate(%s): %v", id, err)
	}
	return c
}

func one(t *testing.T, o Outcome) string {
	t.Helper()
	if len(o.Candidates) != 1 {
		t.Fatalf("%s produced %d candidates, want 1", o, len(o.Candidates))
	}
	return o.Candidates[0]
}

// modelJSON is a valid structured reply with the given fields.
func modelJSON(date, start, end, title, location string, missing, ambiguous string, correction bool, evidence string) string {
	c := "false"
	if correction {
		c = "true"
	}
	return `{"isEvent":true,"title":"` + title + `","date":"` + date + `","startTime":"` + start + `","endTime":"` + end +
		`","durationMinutes":0,"allDay":false,"timezone":"","location":"` + location + `","notes":"","confidence":0.9,` +
		`"missing":[` + missing + `],"ambiguous":[` + ambiguous + `],"evidence":"` + evidence + `","isCorrection":` + c + `}`
}

// schoolNightReply is what a local model correctly reads from plain.eml.
var schoolNightReply = modelJSON("2026-10-15", "18:00", "19:30", "School family night", "School gym", "", "", false,
	"Thursday, October 15, 2026 from 6:00 to 7:30 p.m.")
