package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func seedMemory(t *testing.T, h *harness, content, owner string, audience runtimedb.ButlerTaskAudience) int64 {
	t.Helper()
	seq, err := h.s.DB.RecordMemory(runtimedb.MemoryRecord{
		Kind: runtimedb.MemoryFact, Subject: owner, Content: content, Audience: audience,
		PermissionLabel: owner, Owner: owner, Author: owner,
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
