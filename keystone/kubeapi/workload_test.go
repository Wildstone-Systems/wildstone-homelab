package kubeapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPodLogsReturnsActualBoundedSuffix(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("missing bearer token")
		}
		_, _ = w.Write([]byte("ABCDEFGHIJ"))
	}))
	defer s.Close()
	c, err := NewWithHTTP(s.URL, func() (string, error) { return "test-token", nil }, s.Client())
	if err != nil {
		t.Fatal(err)
	}
	logs, truncated, err := c.PodLogs(context.Background(), "pod-a", 10, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(logs) != "GHIJ" || !truncated {
		t.Fatalf("logs %q truncated=%v, want actual suffix", logs, truncated)
	}
}
