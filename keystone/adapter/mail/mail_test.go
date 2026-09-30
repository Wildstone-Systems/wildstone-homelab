package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/integration"
)

// fakeReader counts what it was asked, so a test can prove a refused
// request never reached the mailbox.
type fakeReader struct {
	calls []string
	probe error
}

func (f *fakeReader) Probe(context.Context) (Probe, error) {
	f.calls = append(f.calls, "probe")
	return Probe{Folders: []FolderStatus{{Folder: "INBOX", Available: true}}}, f.probe
}

func (f *fakeReader) Changes(_ context.Context, req ChangesRequest) (adapter.Answer, error) {
	folder := req.Folder
	if folder == "" {
		folder = "all"
	}
	f.calls = append(f.calls, fmt.Sprintf("changes %s %d", folder, req.Limit))
	return adapter.Answer{Data: map[string]any{"messages": []any{}}, Empty: true}, nil
}

func (f *fakeReader) Message(_ context.Context, req MessageRequest) (adapter.Answer, error) {
	f.calls = append(f.calls, "message "+req.Ref)
	return adapter.Answer{Data: map[string]any{"ref": req.Ref}}, nil
}

func (f *fakeReader) Search(_ context.Context, req SearchRequest) (adapter.Answer, error) {
	f.calls = append(f.calls, fmt.Sprintf("search %v %d", req.Folders, req.Limit))
	return adapter.Answer{Data: map[string]any{"messages": []any{}}, Empty: true}, nil
}

// The dispatch table is exactly the mail domain's declared queries: the
// adapter cannot answer something the vocabulary does not declare, and
// the vocabulary declares nothing the adapter does not answer.
func TestOperationsAreExactlyTheMailQueries(t *testing.T) {
	var declared []string
	for _, q := range integration.Mail.Queries {
		declared = append(declared, q.Name)
	}
	if strings.Join(declared, ",") != strings.Join(Operations, ",") {
		t.Fatalf("operations %v, vocabulary %v", Operations, declared)
	}
	if len(integration.Mail.Actions) != 0 {
		t.Fatal("the mail domain grew an action")
	}
}

// Structurally read-only: every mutating name, as a query or an action,
// is refused without the reader being asked anything.
func TestMutatingOperationsAreRefused(t *testing.T) {
	f := &fakeReader{}
	u := &Upstream{Reader: f}
	for _, op := range []string{"mail.send", "mail.reply", "mail.draft", "mail.move", "mail.archive",
		"mail.delete", "mail.mark_read", "mail.flag", "mail.rule", "mail.expunge", "calendar.create_event"} {
		if _, err := u.Query(context.Background(), op, nil); !errors.Is(err, adapter.ErrNoSuchCapability) {
			t.Errorf("Query(%s) = %v", op, err)
		}
		if _, err := u.Act(context.Background(), op, nil); !errors.Is(err, adapter.ErrNoSuchCapability) {
			t.Errorf("Act(%s) = %v", op, err)
		}
	}
	for _, op := range Operations {
		if _, err := u.Act(context.Background(), op, nil); !errors.Is(err, adapter.ErrNoSuchCapability) {
			t.Errorf("Act(%s) = %v; reads are not actions either", op, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("the reader was asked %v", f.calls)
	}
}

// Without a reader or a credential every operation is an explicit
// not_configured unavailability — never an empty mailbox.
func TestNotConfiguredIsUnavailableNeverEmpty(t *testing.T) {
	u := &Upstream{}
	requests := map[string]string{
		CapCapabilities: `{}`, CapChanges: `{}`, CapMessage: `{"ref":"r1"}`, CapSearch: `{}`,
	}
	for _, op := range Operations {
		answer, err := u.Query(context.Background(), op, json.RawMessage(requests[op]))
		if !errors.Is(err, ErrNotConfigured) || !errors.Is(err, adapter.ErrUnavailable) {
			t.Errorf("%s: err = %v, want not_configured", op, err)
		}
		if answer.Empty || answer.Data != nil {
			t.Errorf("%s answered %+v", op, answer)
		}
		if !strings.Contains(err.Error(), OutcomeNotConfigured) {
			t.Errorf("%s: %q does not name the outcome", op, err)
		}
	}
	caps, err := u.Capabilities(context.Background())
	if err != nil || len(caps) != len(Operations) {
		t.Fatalf("Capabilities = %+v, %v", caps, err)
	}
	for _, c := range caps {
		if c.Available || c.Kind != "query" {
			t.Errorf("capability %+v reported available with no mailbox", c)
		}
	}
}

func TestRequestsAreBoundedBeforeTheReaderSeesThem(t *testing.T) {
	for _, tc := range []struct{ op, req string }{
		{CapChanges, `{"limit":51}`},
		{CapChanges, `{"limit":-1}`},
		{CapChanges, `{"folder":"IN\u0000BOX"}`},
		{CapChanges, `{"folder":"INBOX","store":"+FLAGS \\Seen"}`},
		{CapChanges, `{} {}`},
		{CapMessage, `{}`},
		{CapMessage, `{"ref":"` + strings.Repeat("x", MaxRefLen+1) + `"}`},
		{CapSearch, `{"since":"yesterday"}`},
		{CapSearch, `{"since":"2026-10-02","before":"2026-10-01"}`},
		{CapSearch, `{"since":"2025-01-01","before":"2026-10-01"}`},
		// The cursor is bounded too: named folders, no duplicates, no
		// more than the scope could hold, and nothing but its fields.
		{CapChanges, `{"cursor":{"folders":[{"folder":"INBOX"},{"folder":"INBOX"}]}}`},
		{CapChanges, `{"cursor":{"folders":[{"folder":""}]}}`},
		{CapChanges, `{"cursor":{"folders":[` + strings.Repeat(`{"folder":"a"},`, MaxFolders) + `{"folder":"b"}]}}`},
		{CapChanges, `{"cursor":{"uidValidity":1,"uid":2}}`},
		{CapSearch, `{"folders":[` + strings.Repeat(`"a",`, MaxFolders) + `"a"]}`},
		{CapSearch, `{"subject":"` + strings.Repeat("s", MaxSubjectLen+1) + `"}`},
		{CapCapabilities, `{"folder":"INBOX"}`},
		{CapChanges, `{"pad":"` + strings.Repeat("x", MaxRequestBytes) + `"}`},
	} {
		f := &fakeReader{}
		_, err := (&Upstream{Reader: f}).Query(context.Background(), tc.op, json.RawMessage(tc.req))
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s %.60s: err = %v, want bad_request", tc.op, tc.req, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s %.60s reached the reader: %v", tc.op, tc.req, f.calls)
		}
	}
}

func TestValidRequestsReachTheReaderNormalized(t *testing.T) {
	f := &fakeReader{}
	u := &Upstream{Reader: f}
	ctx := context.Background()
	if a, err := u.Query(ctx, CapChanges, nil); err != nil || !a.Empty {
		t.Fatalf("changes = %+v, %v", a, err)
	}
	if _, err := u.Query(ctx, CapSearch, json.RawMessage(`{"from":["school.example.internal"],"since":"2026-10-01","limit":5}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Query(ctx, CapMessage, json.RawMessage(`{"ref":"r-1"}`)); err != nil {
		t.Fatal(err)
	}
	a, err := u.Query(ctx, CapCapabilities, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if data := a.Data.(map[string]any); data["readOnly"] != true {
		t.Fatalf("capabilities = %+v", data)
	}
	want := "changes all 50|search [] 5|message r-1|probe"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("reader calls = %s, want %s", got, want)
	}
}

func TestAuthFailedIsDistinctFromNotConfigured(t *testing.T) {
	u := &Upstream{Reader: &fakeReader{probe: ErrAuthFailed}}
	_, err := u.Query(context.Background(), CapCapabilities, nil)
	if !errors.Is(err, ErrAuthFailed) || errors.Is(err, ErrNotConfigured) || !errors.Is(err, adapter.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestCredentialsNeverPrint(t *testing.T) {
	c := Credentials{Username: "ada@example.internal", Password: "REPLACED-app-password"}
	enc, _ := json.Marshal(c)
	for _, s := range []string{c.String(), fmt.Sprint(c), fmt.Sprintf("%v %+v", c, c), string(enc)} {
		if strings.Contains(s, c.Password) {
			t.Fatalf("password printed: %s", s)
		}
	}
	if !c.Present() || (Credentials{Username: "ada"}).Present() {
		t.Fatal("Present")
	}
}
