package domestic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNtfy captures one publish.
type fakeNtfy struct {
	*httptest.Server
	got    ntfyMessage
	auth   string
	status int
}

func newFakeNtfy(t *testing.T) *fakeNtfy {
	t.Helper()
	f := &fakeNtfy{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &f.got) //nolint:errcheck
		f.auth = r.Header.Get("Authorization")
		w.WriteHeader(f.status)
		w.Write([]byte(`{"id":"x"}`)) //nolint:errcheck
	}))
	t.Cleanup(f.Close)
	return f
}

func notifyCall(params map[string]string) Call {
	return Call{
		Tool: "notifications.notify", Tier: "domestic", Params: params,
		Rule: "mail-check", Subject: "mika", CorrelationID: "ha-arrived-1",
		TriggerType: "person.arrived_home",
	}
}

func TestNotifyPublishes(t *testing.T) {
	f := newFakeNtfy(t)
	n := &Notifier{Authority: permittedExecution{}, BaseURL: f.URL, Token: "KNOWN-FAKE-ntfy-token"}

	res, err := n.Execute(context.Background(), notifyCall(map[string]string{
		"topic": "household-mika", "message": "Did you get the mail?",
		"title": "Wildstone", "priority": "4", "tags": "house, envelope",
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.got.Topic != "household-mika" || f.got.Message != "Did you get the mail?" ||
		f.got.Title != "Wildstone" || f.got.Priority != 4 || len(f.got.Tags) != 2 {
		t.Fatalf("published %+v", f.got)
	}
	if len(f.got.Actions) != 0 {
		t.Fatalf("unrequested buttons: %+v", f.got.Actions)
	}
	if f.auth != "Bearer KNOWN-FAKE-ntfy-token" {
		t.Fatalf("auth = %q", f.auth)
	}
	if res.Summary != "notified household-mika" {
		t.Fatalf("summary = %q", res.Summary)
	}
}

func TestNotifyReplyButtonsCloseTheLoop(t *testing.T) {
	f := newFakeNtfy(t)
	minted := 0
	n := &Notifier{Authority: permittedExecution{},
		BaseURL:  f.URL,
		ReplyURL: "https://wildstone.example/api/v1/household/events",
		MintReply: func(context.Context) (string, error) {
			minted++
			return "wst_KNOWNFAKEreplytoken", nil
		},
	}

	res, err := n.Execute(context.Background(), notifyCall(map[string]string{
		"topic": "household-mika", "message": "Did you get the mail?", "replies": "Yes, No",
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if minted != 1 {
		t.Fatalf("minted %d tokens for one notification", minted)
	}
	if len(f.got.Actions) != 2 || res.Summary != "asked household-mika (Yes/No)" {
		t.Fatalf("actions = %+v, summary = %q", f.got.Actions, res.Summary)
	}
	for i, want := range []string{"Yes", "No"} {
		a := f.got.Actions[i]
		if a.Action != "http" || a.Label != want || a.Method != http.MethodPost ||
			a.URL != "https://wildstone.example/api/v1/household/events" {
			t.Fatalf("action %d = %+v", i, a)
		}
		if a.Headers["Authorization"] != "Bearer wst_KNOWNFAKEreplytoken" {
			t.Fatalf("action %d auth = %q", i, a.Headers["Authorization"])
		}
		// The button posts a household envelope the intake accepts as
		// written — that is the whole loop.
		var env struct {
			EventID       string            `json:"eventId"`
			Type          string            `json:"type"`
			Subject       string            `json:"subject"`
			Entity        string            `json:"entity"`
			CorrelationID string            `json:"correlationId"`
			Payload       map[string]string `json:"payload"`
		}
		if err := json.Unmarshal([]byte(a.Body), &env); err != nil {
			t.Fatalf("action %d body: %v", i, err)
		}
		if env.Type != AnsweredEventType || env.Subject != "mika" || env.Entity != "mail-check" ||
			env.CorrelationID != "ha-arrived-1" || env.Payload["answer"] != want {
			t.Fatalf("action %d envelope = %+v", i, env)
		}
		if !strings.HasPrefix(env.EventID, "reply:ha-arrived-1:") {
			t.Fatalf("action %d event id = %q", i, env.EventID)
		}
	}
	// Two taps of the same button are one answer; two different buttons
	// are two answers, in order.
	if f.got.Actions[0].Body == f.got.Actions[1].Body {
		t.Fatal("both buttons carry the same event id")
	}
}

func TestNotifyRefusals(t *testing.T) {
	f := newFakeNtfy(t)
	for name, tc := range map[string]struct {
		n      *Notifier
		params map[string]string
		want   string
	}{
		"no base url":   {&Notifier{Authority: permittedExecution{}}, map[string]string{"topic": "t", "message": "m"}, "no ntfy base URL"},
		"no topic":      {&Notifier{Authority: permittedExecution{}, BaseURL: f.URL}, map[string]string{"message": "m"}, `"topic" is required`},
		"blank message": {&Notifier{Authority: permittedExecution{}, BaseURL: f.URL}, map[string]string{"topic": "t", "message": "  "}, `"message" is required`},
		"bad priority":  {&Notifier{Authority: permittedExecution{}, BaseURL: f.URL}, map[string]string{"topic": "t", "message": "m", "priority": "9"}, `"priority" must be 1..5`},
		// A question with nowhere to send its answer is refused, not
		// quietly downgraded to a statement.
		"replies with no reply url": {&Notifier{Authority: permittedExecution{}, BaseURL: f.URL}, map[string]string{"topic": "t", "message": "m", "replies": "Yes,No"}, "no household reply URL is configured"},
		"too many replies": {
			&Notifier{Authority: permittedExecution{}, BaseURL: f.URL, ReplyURL: "https://x/y", MintReply: func(context.Context) (string, error) { return "t", nil }},
			map[string]string{"topic": "t", "message": "m", "replies": "a,b,c,d"}, "at most 3",
		},
	} {
		_, err := tc.n.Execute(context.Background(), notifyCall(tc.params))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}

	// A non-2xx from ntfy is the executor's failure, with the server's
	// own words carried through.
	f.status = http.StatusInsufficientStorage
	n := &Notifier{Authority: permittedExecution{}, BaseURL: f.URL}
	if _, err := n.Execute(context.Background(), notifyCall(map[string]string{"topic": "t", "message": "m"})); err == nil ||
		!strings.Contains(err.Error(), "507") {
		t.Fatalf("ntfy failure = %v", err)
	}
}
