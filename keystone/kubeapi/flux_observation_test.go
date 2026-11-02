package kubeapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFluxObservationOnlyNamedGETAndRotatedIdentity(t *testing.T) {
	calls := 0
	token := "first"
	responseName := "example-app"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/namespaces/example-app/helmreleases/example-app") || r.URL.RawQuery != "" {
			t.Errorf("unexpected read: %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("token not re-read")
		}
		fmt.Fprintf(w, `{"metadata":{"name":%q,"namespace":"example-app","uid":"example-uid","resourceVersion":"2","generation":2},"spec":{},"status":{"observedGeneration":2}}`, responseName)
	}))
	defer server.Close()
	c, err := NewWithHTTP(server.URL, func() (string, error) { return token, nil }, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []string{"first", "rotated"} {
		token = next
		if _, err := c.ObserveFlux(context.Background(), "HelmRelease", "example-app", "example-app"); err != nil {
			t.Fatal(err)
		}
	}
	for _, request := range [][3]string{{"Secret", "example-app", "example-app"}, {"PodLogs", "example-app", "example-app"}, {"HelmRelease", "../other", "example-app"}, {"Deployment", "example-app", "example-app/log"}, {"Deployment", "example-app", ""}} {
		if _, err := c.ObserveFlux(context.Background(), request[0], request[1], request[2]); err == nil {
			t.Fatalf("accepted %v", request)
		}
	}
	if calls != 2 {
		t.Fatal("invalid input reached server", calls)
	}
	responseName = "unrelated"
	if _, err := c.ObserveFlux(context.Background(), "HelmRelease", "example-app", "example-app"); err == nil {
		t.Fatal("accepted unrelated object")
	}
}
