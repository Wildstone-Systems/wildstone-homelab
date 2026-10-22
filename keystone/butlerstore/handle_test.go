package butlerstore

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// handledTask opens a conversation task snapshotted to run as a real
// Butler, the way butlerlive.Serve opens one, and returns its id.
func handledTask(t *testing.T, db *runtimedb.DB, butlerName string) string {
	t.Helper()
	if _, err := db.GetButler(butlerName); err != nil {
		if err := db.UpsertModel(runtimedb.Model{
			Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
			Readiness: runtimedb.ModelReady, Enabled: true,
		}, "root-admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateButler(runtimedb.Butler{
			Name: butlerName, Directive: "look after the house", Surface: runtimedb.ButlerSurfaceAdmin,
			AgentClass: runtimedb.AgentClassOwner, Model: "kitchen-box",
		}, "mika"); err != nil {
			t.Fatalf("CreateButler(%s): %v", butlerName, err)
		}
	}
	id, err := db.InsertButlerTask(runtimedb.ButlerTask{
		Kind: runtimedb.ButlerTaskConversation, Author: "sam", Owner: "sam",
		Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner,
		Audience: runtimedb.ButlerAudienceShared, OriginConnection: "console",
		Objective: "answer sam", Retention: runtimedb.ButlerRetentionConversation,
	})
	if err != nil {
		t.Fatalf("InsertButlerTask: %v", err)
	}
	if err := db.SetTaskButler(id, butlerName); err != nil {
		t.Fatalf("SetTaskButler: %v", err)
	}
	return id
}

func secretOf(h agentapi.Handle) string {
	_, secret, _ := strings.Cut(string(h), ".")
	return secret
}

func TestHandleIssueAndResolveRoundTrip(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "handle.db"))
	id := handledTask(t, db, "alfred")

	h, err := l.IssueHandle(id)
	if err != nil {
		t.Fatalf("IssueHandle: %v", err)
	}
	if h.TaskID() != id || secretOf(h) == "" {
		t.Fatalf("handle %q does not have the <taskID>.<secret> shape", h.String())
	}
	hc, err := l.ResolveHandle(h)
	if err != nil {
		t.Fatalf("ResolveHandle: %v", err)
	}
	if hc.Task.ID != id || hc.Butler.Name != "alfred" || hc.Butler.Seq == 0 {
		t.Fatalf("resolved to task %q as %q (seq %d); want %s as alfred", hc.Task.ID, hc.Butler.Name, hc.Butler.Seq, id)
	}
	if hc.Person != "sam" || hc.OriginConnection != "console" ||
		hc.Audience != runtimedb.ButlerAudienceShared || hc.Surface != runtimedb.ButlerSurfaceAdmin ||
		hc.AgentClass != runtimedb.AgentClassOwner {
		t.Fatalf("resolved context = %+v; the admission facts should be the task's own", hc)
	}

	// Two issues never agree, and only the latest one resolves.
	again, err := l.IssueHandle(id)
	if err != nil {
		t.Fatalf("IssueHandle again: %v", err)
	}
	if again == h {
		t.Fatal("re-issuing produced the same handle")
	}
	if _, err := l.ResolveHandle(h); !errors.Is(err, ErrHandleRefused) {
		t.Fatalf("the superseded handle still resolves: %v", err)
	}
	if _, err := l.ResolveHandle(again); err != nil {
		t.Fatalf("the live handle was refused: %v", err)
	}
}

func TestHandleWithTheWrongSecretIsRefused(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "handle.db"))
	id := handledTask(t, db, "alfred")
	h, err := l.IssueHandle(id)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]agentapi.Handle{
		"wrong secret":     agentapi.Handle(id + ".not-the-secret"),
		"truncated secret": agentapi.Handle(id + "." + secretOf(h)[:10]),
		"other task":       agentapi.Handle("bt-999." + secretOf(h)),
		"no secret":        agentapi.Handle(id + "."),
		"no dot":           agentapi.Handle(id),
		"empty":            "",
	} {
		_, err := l.ResolveHandle(bad)
		if !errors.Is(err, ErrHandleRefused) {
			t.Errorf("%s: err = %v; want ErrHandleRefused", name, err)
		}
		if err != nil && strings.Contains(err.Error(), secretOf(h)) {
			t.Errorf("%s: the refusal carries the live secret: %v", name, err)
		}
	}
}

func TestHandleIsRefusedUntilIssued(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "handle.db"))
	id := handledTask(t, db, "alfred")
	// A task that was never issued a handle refuses anything at all,
	// including the empty digest's own preimage.
	if _, err := l.ResolveHandle(agentapi.Handle(id + ".anything")); !errors.Is(err, ErrHandleRefused) {
		t.Fatalf("an unissued task accepted a handle: %v", err)
	}
	if _, err := l.ResolveHandle(agentapi.Handle(id + ".")); !errors.Is(err, ErrHandleRefused) {
		t.Fatalf("an unissued task accepted an empty secret: %v", err)
	}
}

func TestHandleForAFinishedTaskIsRefused(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "handle.db"))
	id := handledTask(t, db, "alfred")
	h, err := l.IssueHandle(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionButlerTask(id, runtimedb.ButlerTaskCancelled, "stopped", "sam"); err != nil {
		t.Fatalf("cancelling: %v", err)
	}
	if _, err := l.ResolveHandle(h); !errors.Is(err, ErrHandleRefused) {
		t.Fatalf("a cancelled task's handle still resolves: %v", err)
	}
	// Nor can a finished task be issued a fresh one.
	if _, err := l.IssueHandle(id); err == nil {
		t.Fatal("a cancelled task was issued a handle")
	}
}

func TestFencingATaskWithdrawsItsHandle(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "handle.db"))
	id := handledTask(t, db, "alfred")
	h, err := l.IssueHandle(id)
	if err != nil {
		t.Fatal(err)
	}
	// A stalled model call fences attempts and keeps the bearer: the
	// same runtime retries under the same authority.
	if _, err := l.FenceAttempts(id); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ResolveHandle(h); err != nil {
		t.Fatalf("fencing attempts alone withdrew the handle: %v", err)
	}
	// Revocation fences the task: the bearer goes with it.
	before, _ := db.GetButlerTask(id)
	epoch, err := l.FenceTask(id)
	if err != nil {
		t.Fatalf("FenceTask: %v", err)
	}
	if epoch != before.LeaseEpoch+1 {
		t.Fatalf("epoch = %d after fencing from %d", epoch, before.LeaseEpoch)
	}
	if _, err := l.ResolveHandle(h); !errors.Is(err, ErrHandleRefused) {
		t.Fatalf("a fenced task's handle still resolves: %v", err)
	}
	// The task itself is still open; a resume is issued a new one.
	if _, err := l.IssueHandle(id); err != nil {
		t.Fatalf("a fenced task could not be re-issued: %v", err)
	}
}

func TestHandleForARetiredButlerIsRefused(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "handle.db"))
	id := handledTask(t, db, "alfred")
	h, err := l.IssueHandle(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RetireButler("alfred", "mika"); err != nil {
		t.Fatalf("RetireButler: %v", err)
	}
	if _, err := l.ResolveHandle(h); !errors.Is(err, ErrHandleRefused) {
		t.Fatalf("a retired butler's task handle still resolves: %v", err)
	}
}

// The secret never appears where a handle is printed: not through
// String, not through fmt's verbs, not in any refusal.
func TestHandleNeverPrintsItsSecret(t *testing.T) {
	db, l := testLedger(t, filepath.Join(t.TempDir(), "handle.db"))
	id := handledTask(t, db, "alfred")
	h, err := l.IssueHandle(id)
	if err != nil {
		t.Fatal(err)
	}
	secret := secretOf(h)
	for name, rendered := range map[string]string{
		"String": h.String(),
		"%s":     fmt.Sprintf("%s", h),
		"%v":     fmt.Sprintf("%v", h),
		"%+v":    fmt.Sprintf("%+v", h),
		"Sprint": fmt.Sprint(h),
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("%s rendered the secret: %q", name, rendered)
		}
		if !strings.Contains(rendered, id) {
			t.Errorf("%s lost the public task id: %q", name, rendered)
		}
	}
	// Refusals about this handle do not carry it either.
	if err := db.TransitionButlerTask(id, runtimedb.ButlerTaskCancelled, "stopped", "sam"); err != nil {
		t.Fatal(err)
	}
	_, rerr := l.ResolveHandle(h)
	if rerr == nil || strings.Contains(rerr.Error(), secret) {
		t.Fatalf("the refusal for a cancelled task leaked the secret: %v", rerr)
	}
}
