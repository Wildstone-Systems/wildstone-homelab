package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOllamaEmbedderUsesServerOwnedRouteAndPinsDimension(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body struct {
			Model    string `json:"model"`
			Input    string `json:"input"`
			Truncate bool   `json:"truncate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "embedding-model" || body.Input != "bounded text" || body.Truncate {
			t.Errorf("request = %+v", body)
		}
		if calls.Load() == 1 {
			fmt.Fprint(w, `{"embeddings":[[0.25,-0.5,1]]}`)
		} else {
			fmt.Fprint(w, `{"embeddings":[[0.25,-0.5]]}`)
		}
	}))
	defer server.Close()

	embedder := NewOllamaEmbedder(server.URL, "embedding-model")
	vector, err := embedder.Embed(context.Background(), "bounded text")
	if err != nil || len(vector) != 3 {
		t.Fatalf("first embedding = %v, %v", vector, err)
	}
	if _, err = embedder.Embed(context.Background(), "bounded text"); !errors.Is(err, ErrEmbeddingDimension) {
		t.Fatalf("dimension change = %v", err)
	}
}

func TestOllamaEmbedderBoundsAndRedactsFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{"malformed", `not-json`, ErrEmbeddingMalformed},
		{"empty", `{"embeddings":[[]]}`, ErrEmbeddingBounds},
		{"multiple", `{"embeddings":[[1],[2]]}`, ErrEmbeddingMalformed},
		{"null-coordinate", `{"embeddings":[[1,null,3]]}`, ErrEmbeddingMalformed},
		{"non-finite", `{"embeddings":[[1e999]]}`, ErrEmbeddingMalformed},
		{"float32-overflow", `{"embeddings":[[1e100]]}`, ErrEmbeddingBounds},
		{"trailing", `{"embeddings":[[1]]} {}`, ErrEmbeddingMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			_, err := NewOllamaEmbedder(server.URL, "model").Embed(context.Background(), "text")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	called := false
	p := &OllamaEmbedder{Host: "http://example.internal", Model: "model", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not call")
	})}}
	if _, err := p.Embed(context.Background(), strings.Repeat("x", maxEmbeddingInputBytes+1)); !errors.Is(err, ErrEmbeddingBounds) || called {
		t.Fatalf("oversized input: called=%v err=%v", called, err)
	}

	secret := "private-response-canary"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, secret)
	}))
	defer server.Close()
	_, err := NewOllamaEmbedder(server.URL, "model").Embed(context.Background(), "text")
	if !errors.Is(err, ErrEmbeddingUnavailable) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("unredacted provider failure: %v", err)
	}
}

func TestOllamaEmbedderRejectsOversizedResponseAndLateCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", maxEmbeddingResponseBytes+1))
	}))
	defer server.Close()
	if _, err := NewOllamaEmbedder(server.URL, "model").Embed(context.Background(), "text"); !errors.Is(err, ErrEmbeddingBounds) {
		t.Fatalf("oversized response = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"embeddings":[[1,2]]}`)),
			Header:     make(http.Header),
		}, nil
	})}
	p := &OllamaEmbedder{Host: "http://example.internal", Model: "model", HTTPClient: client}
	if _, err := p.Embed(ctx, "text"); !errors.Is(err, context.Canceled) {
		t.Fatalf("late canceled response = %v", err)
	}
}

func TestOllamaEmbedderDistinguishesTimeoutAndTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"timeout", context.DeadlineExceeded, context.DeadlineExceeded},
		{"canceled", context.Canceled, context.Canceled},
		{"transport", errors.New("private transport detail"), ErrEmbeddingUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, tc.err
			})}
			p := &OllamaEmbedder{Host: "http://example.internal", Model: "model", HTTPClient: client}
			_, err := p.Embed(context.Background(), "text")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "example.internal") || strings.Contains(err.Error(), "private transport detail") {
				t.Fatalf("transport detail leaked for %s: %v", tc.name, err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
