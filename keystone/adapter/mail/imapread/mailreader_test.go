package imapread

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter"
	mailapi "github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail/imapread/imapfake"
)

// query runs one operation through the adapter's dispatcher and
// round-trips the answer's data through JSON, as Keystone receives it.
func query(t *testing.T, u *mailapi.Upstream, op, req string) (adapter.Answer, map[string]any, error) {
	t.Helper()
	a, err := u.Query(ctx(t), op, json.RawMessage(req))
	if err != nil {
		return a, nil, err
	}
	b, err := json.Marshal(a.Data)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s data is not a JSON object: %s", op, b)
	}
	return a, m, nil
}

// Every imapread outcome lands on the mail contract's result exactly as
// MailReader documents.
func TestMailReaderOutcomeMapping(t *testing.T) {
	h := newHarness(t, SecurityTLS)
	ada := "ada@example.internal"
	h.deliver(ada, 2, 0)
	u := upstream(h.reader(ada, Filters{}))

	// Baseline: empty, and the cursor to commit still arrives.
	a, data, err := query(t, u, mailapi.CapChanges, `{}`)
	if err != nil || !a.Empty || data["outcome"] != "empty" {
		t.Fatalf("baseline = %+v %v %v", a, data, err)
	}
	next, _ := json.Marshal(data["next"])
	if !strings.Contains(string(next), `"lastUid":2`) {
		t.Fatalf("baseline cursor = %s", next)
	}

	// New mail after the committed cursor: ok.
	h.deliver(ada, 1, 2)
	a, data, err = query(t, u, mailapi.CapChanges, `{"cursor":`+string(next)+`}`)
	if err != nil || a.Empty || data["outcome"] != "ok" || len(data["messages"].([]any)) != 1 {
		t.Fatalf("changes = %+v %v %v", a, data, err)
	}
	next, _ = json.Marshal(data["next"])
	ref := data["messages"].([]any)[0].(map[string]any)["ref"].(string)

	// UIDVALIDITY reset: ok (never unavailable), outcome cursor_reset,
	// carrying the restarted cursor.
	h.srv.ResetUIDValidity(ada, "INBOX")
	a, data, err = query(t, u, mailapi.CapChanges, `{"cursor":`+string(next)+`}`)
	if err != nil || a.Empty || data["outcome"] != mailapi.OutcomeCursorReset {
		t.Fatalf("reset = %+v %v %v", a, data, err)
	}
	reset, _ := json.Marshal(data["next"])
	if !strings.Contains(string(reset), `"lastUid":0`) {
		t.Fatalf("reset cursor = %s", reset)
	}

	// The old reference is stale: unavailable, outcome stale.
	_, _, err = query(t, u, mailapi.CapMessage, `{"ref":"`+ref+`"}`)
	if !errors.Is(err, adapter.ErrUnavailable) || !strings.Contains(err.Error(), ": stale: uidvalidity") {
		t.Fatalf("stale ref = %v", err)
	}

	// A folder outside the scope, in changes or search: bad_request.
	for _, tc := range []struct{ op, req string }{
		{mailapi.CapChanges, `{"folder":"Elsewhere"}`},
		{mailapi.CapSearch, `{"folders":["Elsewhere"]}`},
		{mailapi.CapMessage, `{"ref":"mref1.not-a-ref"}`},
	} {
		if _, _, err := query(t, u, tc.op, tc.req); !errors.Is(err, mailapi.ErrBadRequest) {
			t.Errorf("%s %s = %v", tc.op, tc.req, err)
		}
	}

	// Search with the default window, several senders, one matching.
	a, data, err = query(t, u, mailapi.CapSearch, `{"from":["nobody.example.internal","school.example.internal"],"since":"2026-09-01","before":"2026-11-01"}`)
	if err != nil || data["outcome"] != "ok" || len(data["results"].([]any)) != 3 {
		t.Fatalf("search = %+v %v %v", a, data, err)
	}
	a, data, err = query(t, u, mailapi.CapSearch, `{"from":["nobody.example.internal"],"since":"2026-09-01","before":"2026-11-01"}`)
	if err != nil || !a.Empty || data["outcome"] != "empty" {
		t.Fatalf("search, no match = %+v %v %v", a, data, err)
	}

	// Capabilities reports folders and the reader's bounds.
	_, data, err = query(t, u, mailapi.CapCapabilities, `{}`)
	if err != nil || data["readOnly"] != true || len(data["folders"].([]any)) != 1 {
		t.Fatalf("capabilities = %v %v", data, err)
	}
	if lim := data["limits"].(map[string]any); lim["messageBytes"].(float64) <= 0 {
		t.Fatalf("limits = %v", lim)
	}

	// A wrong password is auth_failed; a provider in maintenance is
	// provider_unavailable. Neither is empty.
	cfg := h.config(ada, Filters{})
	cfg.Password = "REPLACED-WRONG"
	wrong, _ := New(cfg)
	for _, op := range []string{mailapi.CapCapabilities, mailapi.CapChanges} {
		_, _, err := query(t, upstream(wrong), op, `{}`)
		if !errors.Is(err, mailapi.ErrAuthFailed) || errors.Is(err, mailapi.ErrNotConfigured) {
			t.Errorf("%s, wrong password = %v", op, err)
		}
	}
	h.srv.SetFaults(imapfake.Faults{LoginUnavailable: true})
	_, _, err = query(t, u, mailapi.CapChanges, `{}`)
	if !errors.Is(err, adapter.ErrUnavailable) || errors.Is(err, mailapi.ErrAuthFailed) ||
		!strings.Contains(err.Error(), mailapi.OutcomeProviderUnavailable) {
		t.Fatalf("maintenance = %v", err)
	}
	if strings.Contains(err.Error(), "REPLACED") {
		t.Fatal("an error carried the credential")
	}
}
