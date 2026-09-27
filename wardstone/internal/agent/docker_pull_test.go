package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowDocker is a Docker endpoint whose pull streams progress for a
// while before it ends, the way a large image does (#361), and whose
// inspect takes just as long to answer. finish is the last message.
func slowDocker(t *testing.T, took time.Duration, finish string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/images/create":
			w.WriteHeader(http.StatusOK)
			for end := time.Now().Add(took); time.Now().Before(end); {
				fmt.Fprintln(w, `{"status":"Downloading","progressDetail":{"current":1,"total":2},"id":"layer"}`)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			fmt.Fprintln(w, finish)
		case strings.HasPrefix(r.URL.Path, "/containers/"):
			select {
			case <-r.Context().Done():
			case <-time.After(took):
			}
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testDockerClient(t *testing.T, endpoint string, call, pull time.Duration) *dockerClient {
	t.Helper()
	dc, err := newDockerClient(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	dc.http.Timeout = call
	dc.pullTimeout = pull
	return dc
}

// A pull outlasts the bound on ordinary calls, and ordinary calls keep
// it: the simulated 2-minute timeout is 100ms and the pull streams for
// 400ms.
func TestPullIsNotBoundByTheCallTimeout(t *testing.T) {
	srv := slowDocker(t, 400*time.Millisecond, `{"status":"Status: Downloaded newer image"}`)
	dc := testDockerClient(t, srv.URL, 100*time.Millisecond, 10*time.Second)

	if err := dc.pull(context.Background(), "example/big"+testDigest, ""); err != nil {
		t.Fatalf("pull: %v", err)
	}
	_, err := dc.inspect(context.Background(), "anything")
	if err == nil || !strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("inspect err = %v, want the call timeout", err)
	}
}

// A pull has its own bound, and running out of it says so.
func TestPullStopsAtItsOwnDeadline(t *testing.T) {
	srv := slowDocker(t, 5*time.Second, `{"status":"done"}`)
	dc := testDockerClient(t, srv.URL, time.Minute, 150*time.Millisecond)

	err := dc.pull(context.Background(), "example/big"+testDigest, "")
	if err == nil || !strings.Contains(err.Error(), "pull did not finish within 150ms") {
		t.Fatalf("pull err = %v, want the pull deadline named", err)
	}
}

// The daemon reports a failure partway through a pull inside its 200
// progress stream; that is a failed pull, not a finished one.
func TestPullReportsAnErrorInTheProgressStream(t *testing.T) {
	srv := slowDocker(t, 30*time.Millisecond,
		`{"errorDetail":{"message":"write /var/lib/docker/tmp: no space left on device"},"error":"write /var/lib/docker/tmp: no space left on device"}`)
	dc := testDockerClient(t, srv.URL, time.Minute, time.Minute)

	err := dc.pull(context.Background(), "example/big"+testDigest, "")
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("pull err = %v, want the daemon's stream error", err)
	}
}
