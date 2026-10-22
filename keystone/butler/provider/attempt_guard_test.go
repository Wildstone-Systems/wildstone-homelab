package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHTTPRedirectRechecksAttemptPolicy(t *testing.T) {
	var blocked atomic.Bool
	var calls atomic.Int32
	refused := errors.New("runtime policy changed")
	ctx := WithAttemptCheck(context.Background(), func() error {
		if blocked.Load() {
			return refused
		}
		return nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		blocked.Store(true)
		w.Header().Set("Location", "/next")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	p := NewOpenAIProvider(server.URL, "")
	_, err := p.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Text: "canary"}}})
	if !errors.Is(err, refused) || calls.Load() != 1 {
		t.Fatalf("redirect bypassed attempt policy: calls=%d error=%v", calls.Load(), err)
	}
}
