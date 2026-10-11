package domestic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

type fakeSource struct {
	events []runtimedb.Event
}

func (s *fakeSource) LatestEventSeq() (int64, error) {
	if len(s.events) == 0 {
		return 0, nil
	}
	return s.events[len(s.events)-1].Seq, nil
}

func (s *fakeSource) EventsAfter(after int64, limit int) ([]runtimedb.Event, error) {
	var out []runtimedb.Event
	for _, e := range s.events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestForwarderPublishesMatchingOnly(t *testing.T) {
	f := newFakeNtfy(t)
	src := &fakeSource{events: []runtimedb.Event{
		{Seq: 1, Type: "backup.started", Message: "x"},
		{Seq: 2, Type: "report.morning", Message: "All quiet."},
	}}
	fw := &EventForwarder{Source: src, Topic: "wild", Notifier: &Notifier{Authority: permittedExecution{}, BaseURL: f.URL, Token: "KNOWN-FAKE-ntfy-token"}}
	if cur := fw.Poll(context.Background(), 0); cur != 2 {
		t.Fatalf("cursor = %d", cur)
	}
	if f.got.Topic != "wild" || f.got.Message != "All quiet." || f.got.Priority != 0 {
		t.Fatalf("morning published %+v", f.got)
	}
	if f.auth != "Bearer KNOWN-FAKE-ntfy-token" {
		t.Fatalf("auth = %q", f.auth)
	}

	f.got = ntfyMessage{}
	src.events = append(src.events,
		runtimedb.Event{Seq: 3, Type: "backup.completed", Message: "ok"},
		runtimedb.Event{Seq: 4, Type: "backup.failed", Message: "photos", Attrs: map[string]string{"kind": "drill", "detail": "SECRET-ERR"}})
	if cur := fw.Poll(context.Background(), 2); cur != 4 {
		t.Fatalf("cursor = %d", cur)
	}
	if f.got.Priority != 4 || f.got.Title != "Backup drill failed" || f.got.Message != "photos" {
		t.Fatalf("failure published %+v", f.got)
	}
}

func TestForwarderIgnoresNonMatching(t *testing.T) {
	f := newFakeNtfy(t)
	src := &fakeSource{events: []runtimedb.Event{{Seq: 1, Type: "backup.completed"}}}
	fw := &EventForwarder{Source: src, Topic: "wild", Notifier: &Notifier{Authority: permittedExecution{}, BaseURL: f.URL}}
	fw.Poll(context.Background(), 0)
	if f.got.Topic != "" {
		t.Fatalf("published %+v", f.got)
	}
}

func TestForwarderDisabledWithoutConfig(t *testing.T) {
	src := &fakeSource{}
	if (&EventForwarder{Source: src, Topic: "wild", Notifier: &Notifier{}}).Enabled() {
		t.Fatal("enabled with no ntfy URL")
	}
	if (&EventForwarder{Source: src, Notifier: &Notifier{BaseURL: "http://x"}}).Enabled() {
		t.Fatal("enabled with no topic")
	}
	if !(&EventForwarder{Source: src, Topic: "wild", Notifier: &Notifier{BaseURL: "http://x"}}).Enabled() {
		t.Fatal("not enabled when fully configured")
	}
}

func TestForwarderFailureDoesNotBlockLaterEvents(t *testing.T) {
	f := newFakeNtfy(t)
	f.status = 500
	src := &fakeSource{events: []runtimedb.Event{
		{Seq: 1, Type: "backup.failed", Message: "a"},
		{Seq: 2, Type: "backup.failed", Message: "b"},
	}}
	fw := &EventForwarder{Source: src, Topic: "wild", Notifier: &Notifier{Authority: permittedExecution{}, BaseURL: f.URL}}
	if cur := fw.Poll(context.Background(), 0); cur != 2 {
		t.Fatalf("cursor = %d, want 2", cur)
	}
	if f.got.Message != "b" {
		t.Fatalf("later event not attempted: %+v", f.got)
	}
}

type errSource struct{ fakeSource }

func (errSource) EventsAfter(int64, int) ([]runtimedb.Event, error) { return nil, errors.New("db") }

func TestForwarderReadErrorKeepsCursor(t *testing.T) {
	fw := &EventForwarder{Source: &errSource{}, Topic: "wild", Notifier: &Notifier{BaseURL: "http://x"}}
	if cur := fw.Poll(context.Background(), 7); cur != 7 {
		t.Fatalf("cursor = %d", cur)
	}
}

type lockedSource struct {
	mu sync.Mutex
	fakeSource
}

func (s *lockedSource) add(e runtimedb.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *lockedSource) EventsAfter(after int64, limit int) ([]runtimedb.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeSource.EventsAfter(after, limit)
}

func (s *lockedSource) LatestEventSeq() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeSource.LatestEventSeq()
}

func TestForwarderRunDoesNotReplayHistory(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m ntfyMessage
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &m) //nolint:errcheck
		got <- m.Message
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	src := &lockedSource{}
	src.add(runtimedb.Event{Seq: 1, Type: "backup.failed", Message: "old"})
	fw := &EventForwarder{Source: src, Topic: "wild", Interval: 5 * time.Millisecond,
		Notifier: &Notifier{Authority: permittedExecution{}, BaseURL: srv.URL}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fw.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	src.add(runtimedb.Event{Seq: 2, Type: "backup.failed", Message: "new"})
	select {
	case m := <-got:
		if m != "new" {
			t.Fatalf("first delivery = %q, want only the post-start event", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("new event never delivered")
	}
	cancel()
	<-done
}
