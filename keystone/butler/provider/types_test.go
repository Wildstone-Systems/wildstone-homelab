package provider

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestMain overrides the package-wide retrySleep hook to an instant no-op
// for the whole test binary, so every provider test (including ones that
// don't explicitly exercise retry/backoff, like the KeyNeverLeaksInError
// tests) runs without sleeping for real wall-clock time when the HTTP
// fake returns a retryable status. Tests that need to exercise the real
// production sleep behavior call realRetrySleep directly instead of going
// through the (here-overridden) retrySleep var.
func TestMain(m *testing.M) {
	retrySleep = func(ctx context.Context, d time.Duration) error {
		return ctx.Err()
	}
	os.Exit(m.Run())
}

func TestHTTPClientOrDefault_NilGetsTimeout(t *testing.T) {
	c := httpClientOrDefault(nil)
	if c == nil {
		t.Fatal("httpClientOrDefault(nil) = nil")
	}
	if c.Timeout != defaultHTTPClientTimeout {
		t.Errorf("Timeout = %v, want %v", c.Timeout, defaultHTTPClientTimeout)
	}
}

func TestHTTPClientOrDefault_InjectedClientPreserved(t *testing.T) {
	injected := &http.Client{Timeout: 5 * time.Second}
	c := httpClientOrDefault(injected)
	if c != injected {
		t.Fatalf("httpClientOrDefault did not preserve the injected client seam")
	}
}

func TestIsRetryableStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusOK, false},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{599, true},
		{600, false},
	}
	for _, c := range cases {
		if got := isRetryableStatus(c.status); got != c.want {
			t.Errorf("isRetryableStatus(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}

func TestParseRetryAfter_Seconds(t *testing.T) {
	d, ok := parseRetryAfter("5")
	if !ok {
		t.Fatal("parseRetryAfter(\"5\") ok = false")
	}
	if d != 5*time.Second {
		t.Errorf("parseRetryAfter(\"5\") = %v, want 5s", d)
	}
}

func TestParseRetryAfter_NegativeSecondsClampsToZero(t *testing.T) {
	d, ok := parseRetryAfter("-3")
	if !ok {
		t.Fatal("parseRetryAfter(\"-3\") ok = false")
	}
	if d != 0 {
		t.Errorf("parseRetryAfter(\"-3\") = %v, want 0", d)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	d, ok := parseRetryAfter(future)
	if !ok {
		t.Fatal("parseRetryAfter(future date) ok = false")
	}
	if d <= 0 || d > 10*time.Second {
		t.Errorf("parseRetryAfter(future date) = %v, want (0, 10s]", d)
	}
}

func TestParseRetryAfter_EmptyOrGarbage(t *testing.T) {
	if _, ok := parseRetryAfter(""); ok {
		t.Error("parseRetryAfter(\"\") ok = true, want false")
	}
	if _, ok := parseRetryAfter("not-a-valid-value"); ok {
		t.Error("parseRetryAfter(garbage) ok = true, want false")
	}
}

func TestRetryBackoff_HonorsRetryAfter(t *testing.T) {
	d := retryBackoff(1, "3")
	if d != 3*time.Second {
		t.Errorf("retryBackoff with Retry-After = %v, want 3s", d)
	}
}

func TestRetryBackoff_FallsBackToExponential(t *testing.T) {
	d1 := retryBackoff(1, "")
	d2 := retryBackoff(2, "")
	if d1 <= 0 {
		t.Errorf("retryBackoff(1, \"\") = %v, want > 0", d1)
	}
	if d2 <= d1 {
		t.Errorf("retryBackoff(2, \"\") = %v, want > retryBackoff(1, \"\") = %v", d2, d1)
	}
}

func TestRetryBackoff_Capped(t *testing.T) {
	d := retryBackoff(20, "")
	if d > 2*time.Second {
		t.Errorf("retryBackoff(20, \"\") = %v, want capped at 2s", d)
	}
}

// withInstantRetrySleep overrides the package-level retrySleep hook for the
// duration of a test so retry tests never sleep for real wall-clock time,
// restoring the original hook on cleanup.
func withInstantRetrySleep(t *testing.T, fn func(ctx context.Context, d time.Duration) error) {
	t.Helper()
	orig := retrySleep
	retrySleep = fn
	t.Cleanup(func() { retrySleep = orig })
}

func TestRealRetrySleep_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := realRetrySleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("realRetrySleep with d=0 on canceled ctx = %v, want context.Canceled", err)
	}
	if err := realRetrySleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("realRetrySleep with d=1h on canceled ctx = %v, want context.Canceled (select picks the already-closed Done() channel, not a real 1h wait)", err)
	}
}

func TestRealRetrySleep_ZeroDelayNoCancellation(t *testing.T) {
	if err := realRetrySleep(context.Background(), 0); err != nil {
		t.Errorf("realRetrySleep(ctx, 0) with live ctx = %v, want nil", err)
	}
}
