package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
)

const (
	maxEmbeddingInputBytes    = 16 << 10
	maxEmbeddingResponseBytes = 1 << 20
	maxEmbeddingDimensions    = 8192
)

// OllamaEmbedder calls Ollama's native /api/embed endpoint. Host and Model
// come from the Agent's assigned route; Embed accepts no routing fields.
type OllamaEmbedder struct {
	Host       string
	Model      string
	HTTPClient *http.Client

	mu        sync.Mutex
	dimension int
}

func NewOllamaEmbedder(host, model string) *OllamaEmbedder {
	return &OllamaEmbedder{Host: host, Model: model}
}

type ollamaEmbedRequest struct {
	Model    string `json:"model"`
	Input    string `json:"input"`
	Truncate bool   `json:"truncate"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]*float64 `json:"embeddings"`
}

func (p *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultHTTPClientTimeout)
		defer cancel()
	}
	if text == "" || len(text) > maxEmbeddingInputBytes {
		return nil, ErrEmbeddingBounds
	}
	if p.Host == "" || p.Model == "" {
		return nil, ErrEmbedderNotConfigured
	}
	body, err := json.Marshal(ollamaEmbedRequest{Model: p.Model, Input: text, Truncate: false})
	if err != nil {
		return nil, fmt.Errorf("ollama embedder: request: %w", ErrEmbeddingMalformed)
	}

	status, response, err := doEmbeddingRequest(ctx, httpClientOrDefault(p.HTTPClient), func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Host+"/api/embed", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		return req, nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		if checked, ok := unwrapAttemptCheckError(err); ok {
			return nil, checked
		}
		if errors.Is(err, ErrEmbeddingBounds) {
			return nil, err
		}
		return nil, ErrEmbeddingUnavailable
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrEmbeddingUnavailable, status)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var decoded ollamaEmbedResponse
	dec := json.NewDecoder(bytes.NewReader(response))
	if err := dec.Decode(&decoded); err != nil {
		return nil, ErrEmbeddingMalformed
	}
	if err := requireJSONEOF(dec); err != nil {
		return nil, ErrEmbeddingMalformed
	}
	if len(decoded.Embeddings) != 1 {
		return nil, ErrEmbeddingMalformed
	}
	values := decoded.Embeddings[0]
	if len(values) == 0 || len(values) > maxEmbeddingDimensions {
		return nil, ErrEmbeddingBounds
	}
	vector := make([]float32, len(values))
	for i, value := range values {
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
			return nil, ErrEmbeddingMalformed
		}
		vector[i] = float32(*value)
		if math.IsInf(float64(vector[i]), 0) {
			return nil, ErrEmbeddingBounds
		}
	}
	if err := p.checkDimension(len(vector)); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return vector, nil
}

func (p *OllamaEmbedder) checkDimension(got int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dimension == 0 {
		p.dimension = got
		return nil
	}
	if p.dimension != got {
		return fmt.Errorf("%w: got %d, expected %d", ErrEmbeddingDimension, got, p.dimension)
	}
	return nil
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return ErrEmbeddingMalformed
	}
	return err
}

func doEmbeddingRequest(ctx context.Context, client *http.Client, newRequest func() (*http.Request, error)) (int, []byte, error) {
	client = guardedHTTPClient(client)
	for attempt := 1; attempt <= maxChatAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		req, err := newRequest()
		if err != nil {
			return 0, nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		limited := io.LimitReader(resp.Body, maxEmbeddingResponseBytes+1)
		body, readErr := io.ReadAll(limited)
		resp.Body.Close()
		if readErr != nil {
			return 0, nil, readErr
		}
		if len(body) > maxEmbeddingResponseBytes {
			return 0, nil, ErrEmbeddingBounds
		}
		if resp.StatusCode == http.StatusOK || !isRetryableStatus(resp.StatusCode) || attempt == maxChatAttempts {
			return resp.StatusCode, body, nil
		}
		if err := retrySleep(ctx, retryBackoff(attempt, resp.Header.Get("Retry-After"))); err != nil {
			return 0, nil, err
		}
	}
	return 0, nil, ErrEmbeddingUnavailable
}
