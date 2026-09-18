package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// seedMemory records one memory record owned by owner.
//
// A household-owned record is authored by a person, not by the
// household: since 0040 owner is the scoped column that takes the
// household and author is the person column that does not. So a shared
// record here is stated by the member these fixtures already have.
func seedMemory(t *testing.T, h *harness, content, owner string, audience runtimedb.ButlerTaskAudience) int64 {
	t.Helper()
	author := owner
	if owner == runtimedb.HouseholdOwner {
		author = "mika"
	}
	h.member(author)
	seq, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{
		Kind: runtimedb.MemoryFact, Subject: owner, Content: content, Audience: audience,
		PermissionLabel: owner, Owner: owner, Author: author,
		SourceKind: "conversation", SourceID: "bs-" + content[:1],
	})
	if err != nil {
		t.Fatalf("RecordMemory: %v", err)
	}
	return seq
}

// Any admitted adult may correct household-shared memory; nobody else's
// private record is even confirmable.
func TestMemoryChangesFollowOwnership(t *testing.T) {
	h := newHarness(t)
	shared := seedMemory(t, h, "dinner is at seven", "household", runtimedb.ButlerAudienceShared)
	private := seedMemory(t, h, "sam's therapy is on Tuesdays", "sam", runtimedb.ButlerAudiencePrivate)

	// Equal adult standing: the operator corrects shared memory without
	// asking whoever recorded it.
	if res := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/butler/memory/correct",
		map[string]any{"seq": shared, "content": "dinner is at half six", "confidence": "stated"}, nil,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("correcting shared memory = %d", res.StatusCode)
	}
	// Somebody else's private record answers 404 — a 403 would confirm
	// it exists.
	if res := h.do(runtimedb.RoleOperator, http.MethodPost, "/api/v1/butler/memory/forget",
		map[string]any{"seq": private}, nil,
	); res.StatusCode != http.StatusNotFound {
		t.Fatalf("forgetting another member's private record = %d, want 404", res.StatusCode)
	}
	// The administrator can, being responsible for the deployment.
	var forgotten struct {
		Note string `json:"note"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/memory/forget",
		map[string]any{"seq": private}, &forgotten,
	); res.StatusCode != http.StatusOK {
		t.Fatalf("admin forgetting a private record = %d", res.StatusCode)
	}
	if !strings.Contains(forgotten.Note, "will not teach it again") {
		t.Fatalf("forgetting did not explain what it does: %q", forgotten.Note)
	}
}

// Retrieval and export are scoped to the caller, and the caller is the
// acting identity — never a member named in the request.
func TestMemoryReadsAreScopedToTheCaller(t *testing.T) {
	h := newHarness(t)
	seedMemory(t, h, "dinner is at seven", "household", runtimedb.ButlerAudienceShared)
	seedMemory(t, h, "sam's therapy is on Tuesdays", "sam", runtimedb.ButlerAudiencePrivate)

	var got struct {
		Records []runtimedb.MemoryRecord `json:"records"`
	}
	h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/butler/memory", nil, &got)
	for _, r := range got.Records {
		if strings.Contains(r.Content, "therapy") {
			t.Fatalf("retrieval returned another member's private record: %+v", r)
		}
	}
	if len(got.Records) != 1 {
		t.Fatalf("retrieval = %d records, want the shared one", len(got.Records))
	}

	body := h.rawBody(runtimedb.RoleOperator, http.MethodGet, "/api/v1/butler/memory/export")
	if strings.Contains(body, "therapy") {
		t.Fatalf("an export carried another member's private record:\n%s", body)
	}
	if !strings.Contains(body, "dinner is at seven") {
		t.Fatalf("an export dropped material the caller may see:\n%s", body)
	}
}

// The digest shows shared changes and never private ones.
func TestMemoryDigestNeverCarriesPrivateChanges(t *testing.T) {
	h := newHarness(t)
	h.member("mika")
	private := seedMemory(t, h, "mika is job hunting", "mika", runtimedb.ButlerAudiencePrivate)
	if err := h.s.DB.ForgetMemory(private, "mika"); err != nil {
		t.Fatal(err)
	}
	seedMemory(t, h, "bin day is Wednesday", "household", runtimedb.ButlerAudienceShared)

	body := h.rawBody(runtimedb.RoleOperator, http.MethodGet, "/api/v1/butler/memory/digest")
	if strings.Contains(body, "job hunting") {
		t.Fatalf("the digest leaked a private change:\n%s", body)
	}
	if !strings.Contains(body, "bin day") {
		t.Fatalf("the digest dropped a shared change:\n%s", body)
	}
}

// Found by Codex: both memory mutation routes need only VerbRead, which
// the Butler SERVICE principal holds, and mayChangeMemory waved through
// every shared record. So a Butler token could correct or forget the
// household's shared memory directly rather than proposing it.
//
// A1's "any admitted adult" is a statement about people. The Butler is
// not one.
func TestAServicePrincipalCannotRewriteHouseholdMemory(t *testing.T) {
	h := newHarness(t)
	h.member("mika")
	seq, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{
		Kind: runtimedb.MemoryFact, Subject: "household",
		Content:  "the spare key is under the mat",
		Audience: runtimedb.ButlerAudienceShared, PermissionLabel: "household",
		Owner: "household", Author: "mika", SourceKind: "conversation", SourceID: "m-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The Butler holds read, so it reaches the handler — and is refused
	// there.
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"/api/v1/butler/memory/forget", map[string]any{"seq": seq}},
		{"/api/v1/butler/memory/correct", map[string]any{
			"seq": seq, "content": "the spare key is with the neighbour"}},
	} {
		res := h.do(runtimedb.RoleButler, http.MethodPost, c.path, c.body, nil)
		if res.StatusCode == http.StatusOK {
			t.Fatalf("%s: the butler rewrote household memory itself", c.path)
		}
	}
	// It really is untouched.
	m, err := h.s.DB.GetMemory(seq)
	if err != nil {
		t.Fatal(err)
	}
	if !m.ForgottenAt.IsZero() || m.SupersededBy != 0 {
		t.Fatalf("the record was changed by a service principal: %+v", m)
	}

	// An admin still can — the fix must not lock out the people who are
	// meant to manage shared records.
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/butler/memory/forget",
		map[string]any{"seq": seq}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("an admin could not forget a shared record: %d", res.StatusCode)
	}
}

// Found by Codex: a routine could be created already enabled through a
// route that needs only propose — which the Butler holds — skipping the
// review its activation requires.
func TestARoutineCannotBeCreatedAlreadyRunning(t *testing.T) {
	h := newHarness(t)
	res := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/household/routines",
		map[string]any{
			"name": "self-starter", "ownership": "wildstone",
			"document": `{"name":"self-starter"}`, "state": "enabled",
		}, nil)
	if res.StatusCode == http.StatusOK {
		t.Fatal("a routine was created already enabled, skipping review")
	}
	if _, err := h.s.DB.GetHouseholdRoutine("self-starter"); err == nil {
		t.Fatal("the refused routine was created anyway")
	}

	// Creating it as a draft works, and it is a draft.
	if res := h.do(runtimedb.RoleButler, http.MethodPost, "/api/v1/household/routines",
		map[string]any{
			"name": "proper", "ownership": "wildstone", "document": `{"name":"proper"}`,
		}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("creating a draft routine = %d", res.StatusCode)
	}
	r, err := h.s.DB.GetHouseholdRoutine("proper")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != runtimedb.RoutineDraft || r.PerformsActions() {
		t.Fatalf("a new routine is %q and performsActions=%v", r.State, r.PerformsActions())
	}
}

// Found by Codex: after W031 A4 an empty MemoryQuery.Butler selects the
// shared pool alone, and this endpoint supplied none — so every record
// held by a named Butler vanished from the only screen that can correct
// or forget it. A regression introduced by the scoping work itself.
func TestTheMemoryViewShowsNamedButlerRecords(t *testing.T) {
	h := newHarness(t)
	h.member("mika")
	if err := h.s.DB.UpsertModel(runtimedb.Model{
		Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama",
		Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.CreateButler(runtimedb.Butler{
		Name: "health", Surface: runtimedb.ButlerSurfaceFamily,
		Profile: runtimedb.ProfileHousehold, Model: "kitchen-box",
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	held, err := h.s.DB.RecordButlerMemory("health", runtimedb.MemoryRecord{
		Kind: runtimedb.MemoryFact, Subject: "household", Content: "held by a butler",
		Audience: runtimedb.ButlerAudienceShared, PermissionLabel: "household",
		Owner: "household", Author: "mika", SourceKind: "conversation", SourceID: "v-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	var out struct {
		Records []runtimedb.MemoryRecord `json:"records"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butler/memory", nil, &out); res.StatusCode != http.StatusOK {
		t.Fatalf("retrieving memory = %d", res.StatusCode)
	}
	var found bool
	for _, r := range out.Records {
		if r.Seq == held {
			found = true
		}
	}
	if !found {
		t.Fatal("a record held by a named butler is invisible in the only view that can act on it")
	}

	// And it can still be narrowed to one Butler on request.
	var scoped struct {
		Records []runtimedb.MemoryRecord `json:"records"`
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet,
		"/api/v1/butler/memory?butler=health", nil, &scoped); res.StatusCode != http.StatusOK {
		t.Fatalf("scoped retrieval = %d", res.StatusCode)
	}
	if len(scoped.Records) != 1 || scoped.Records[0].Seq != held {
		t.Fatalf("scoping to one butler returned %+v", scoped.Records)
	}
}
