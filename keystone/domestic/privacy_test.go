package domestic

import (
	"context"
	"errors"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedContextCannotLeaveThroughAnAction(t *testing.T) {
	db, err := runtimedb.Open(migrated.Into(t, filepath.Join(t.TempDir(), "state.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.RecordButlerMember("sam", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err = db.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	seq, err := db.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: "fact", Owner: "sam", Author: "sam", Content: "jasmine", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "sam", SourceKind: "conversation", SourceID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetMemoryPrivacy("sam", seq, runtimedb.PrivacyPolicy{TrustedOnly: true}); err != nil {
		t.Fatal(err)
	}
	ctx, _, err := db.BeginPrivacyTurn(context.Background(), runtimedb.ModelLocal, runtimedb.MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	runtimedb.TrackMemoryPrivacy(ctx, seq, nil)
	executor := &fakeExecutor{name: "notifications.notify"}
	g := gateway(t, executor)
	audits := 0
	g.Audit = func(_ Call, _ Result, err error) {
		if errors.Is(err, runtimedb.ErrPrivacy) {
			audits++
		}
	}
	if _, err = g.Dispatch(ctx, call("notifications.notify", "domestic")); !errors.Is(err, runtimedb.ErrPrivacy) {
		t.Fatalf("delivery allowed: %v", err)
	}
	if audits != 1 {
		t.Fatalf("privacy refusal was not audited exactly once: %d", audits)
	}
	if len(executor.calls) != 0 {
		t.Fatal("protected content reached executor")
	}
	if _, err = g.Dispatch(context.Background(), call("notifications.notify", "domestic")); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 {
		t.Fatal("ordinary family notification blocked")
	}
}

func changingPrivacy(t *testing.T) (context.Context, func() error) {
	t.Helper()
	db, err := runtimedb.Open(migrated.Into(t, filepath.Join(t.TempDir(), "privacy.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = db.RecordButlerMember("sam", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err = db.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	seq, err := db.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: "fact", Owner: "sam", Author: "sam", Content: "jasmine", Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "sam", SourceKind: "conversation", SourceID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, err := db.BeginPrivacyTurn(context.Background(), runtimedb.ModelLocal, runtimedb.MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = runtimedb.TrackMemoryPrivacy(ctx, seq, nil); err != nil {
		t.Fatal(err)
	}
	return ctx, func() error { return db.SetMemoryPrivacy("sam", seq, runtimedb.PrivacyPolicy{TrustedOnly: true}) }
}
func TestNotifyRechecksPrivacyAfterPreparationAndRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "preparation", true: "redirect"}[redirect], func(t *testing.T) {
			ctx, restrict := changingPrivacy(t)
			delivered := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { delivered++; w.Write([]byte(`{}`)) }))
			defer target.Close()
			n := &Notifier{Authority: permittedExecution{}, BaseURL: target.URL}
			params := map[string]string{"topic": "household", "message": "jasmine"}
			if redirect {
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := restrict(); err != nil {
						t.Error(err)
					}
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				}))
				defer source.Close()
				n.BaseURL = source.URL
			} else {
				n.ReplyURL = "https://home.test/reply"
				n.MintReply = func(context.Context) (string, error) { return "fixture", restrict() }
				params["replies"] = "yes,no"
			}
			_, err := n.Execute(ctx, notifyCall(params))
			if err == nil || delivered != 0 {
				t.Fatalf("protected publish escaped: delivered=%d err=%v", delivered, err)
			}
		})
	}
}
func TestCalendarPrivacyChangesDuringLookup(t *testing.T) {
	ctx, restrict := changingPrivacy(t)
	f := newFakeCalendar(t, vevent("dinner", "Dinner", "20260918T233000Z", "20260919T010000Z"))
	original := f.Config.Handler
	f.Close()
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		original.ServeHTTP(w, r)
		if r.Method == "REPORT" {
			if err := restrict(); err != nil {
				t.Error(err)
			}
		}
	}))
	defer f.Close()
	calendar := calendarUnder(f)
	var err error
	for _, executor := range calendar.Executors() {
		if executor.Name() == "calendar."+CalToolUpdate {
			_, err = executor.Execute(ctx, Call{Tool: executor.Name(), Tier: TierDomestic, Params: map[string]string{"title": "dinner", "date": "2026-09-18", "start": "19:00"}})
		}
	}
	if err == nil {
		t.Fatal("privacy change not checked")
	}
	for _, request := range f.requests {
		if request.Method == http.MethodPut {
			t.Fatal("calendar write escaped")
		}
	}
}
func TestFiledropPrivacyChangesDuringPreparation(t *testing.T) {
	ctx, restrict := changingPrivacy(t)
	root := t.TempDir()
	drop(t, root, "Shared", "original.txt", 5, 0)
	fd := filedropAt(t, root)
	fd.Settings = func() (string, int64, error) { return root, 0, restrict() }
	var err error
	for _, executor := range fd.Executors() {
		if executor.Name() == "filedrop."+FileToolRename {
			_, err = executor.Execute(ctx, fileCall(FileToolRename, map[string]string{"destination": "shared", "name": "original.txt", "new_name": "jasmine.txt"}))
		}
	}
	if err == nil {
		t.Fatal("privacy change not checked")
	}
	if _, err = os.Stat(filepath.Join(root, "Shared", "original.txt")); err != nil {
		t.Fatal("file changed despite privacy refusal")
	}
}

func TestCalendarReadsCheckOutboundPrivacy(t *testing.T) {
	for _, tool := range []string{CalToolAgenda, CalToolSearch, CalToolAvailability} {
		t.Run(tool, func(t *testing.T) {
			ctx, restrict := changingPrivacy(t)
			f := newFakeCalendar(t, vevent("dinner", "Dinner", "20260918T233000Z", "20260919T010000Z"))
			calendar := calendarUnder(f)
			var executor Executor
			for _, candidate := range calendar.Executors() {
				if candidate.Name() == "calendar."+tool {
					executor = candidate
				}
			}
			params := map[string]string{"query": "dinner", "date": "2026-09-18", "start": "2026-09-18T18:00", "end": "2026-09-18T19:00"}
			if _, err := executor.Execute(ctx, Call{Tool: executor.Name(), Tier: TierRead, Params: params}); err != nil {
				t.Fatalf("ordinary read: %v", err)
			}
			before := len(f.requests)
			if before == 0 {
				t.Fatal("positive read made no request")
			}
			if err := restrict(); err != nil {
				t.Fatal(err)
			}
			_, err := executor.Execute(ctx, Call{Tool: executor.Name(), Tier: TierRead, Params: params})
			if err == nil || len(f.requests) != before {
				t.Fatalf("protected read escaped: requests %d -> %d err=%v", before, len(f.requests), err)
			}
		})
	}
}
