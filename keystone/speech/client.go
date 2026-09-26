package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Client speaks the OpenAI transcription API
// (POST {BaseURL}/audio/transcriptions), which both Speaches and OpenAI
// serve. One type, two configurations: the local engine is a Client
// with Processing Local and no key; the cloud one is a Client with
// Processing Cloud and OPENAI_API_KEY.
type Client struct {
	// BaseURL ends in /v1: http://127.0.0.1:18000/v1 (Speaches) or
	// https://api.openai.com/v1.
	BaseURL string
	// Model is the engine's own model id, e.g.
	// Systran/faster-whisper-small or gpt-4o-mini-transcribe.
	Model string
	// Key returns the API key, sent as a Bearer token. It is a function
	// because an Owner can enter or replace the OpenAI key in the Console
	// while Keystone runs. nil means the engine takes no key (Speaches);
	// a func returning "" means a key is needed and missing. The value
	// never appears in an error or a log line.
	Key func() string
	// Where is where the audio goes. It is declared by whoever built
	// the Client, never inferred from the URL.
	Where Processing
	// DownloadsModels says the engine is Speaches, which answers 404 for
	// a model it has not downloaded yet and downloads one on
	// POST {BaseURL}/models/{id}. OpenAI has no such step.
	DownloadsModels bool
	// HTTPClient defaults to a client with no timeout of its own: every
	// call carries a context deadline from the Router instead.
	HTTPClient *http.Client

	mu      sync.Mutex
	warming bool
}

// Processing implements Transcriber.
func (c *Client) Processing() Processing { return c.Where }

func (c *Client) http() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// errBody bounds how much of an engine's error response is read. The
// message is only ever for an operator's log, and a large one is not a
// message.
const errBody = 4 << 10

// Transcribe implements Transcriber.
func (c *Client) Transcribe(ctx context.Context, audio []byte, mimeType, language string) (string, error) {
	mt, err := NormalizeMIME(mimeType)
	if err != nil {
		return "", err
	}
	if err := CheckSize(len(audio), 0, 0); err != nil {
		return "", err
	}

	if c.Key != nil && c.Key() == "" {
		return "", ErrUnavailable
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fields := [][2]string{{"model", c.Model}, {"response_format", "json"}}
	if language = strings.TrimSpace(language); language != "" && language != "auto" {
		fields = append(fields, [2]string{"language", language})
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return "", fmt.Errorf("speech: build request: %w", err)
		}
	}
	part, err := w.CreatePart(fileHeader(fileName(mt), mt))
	if err != nil {
		return "", fmt.Errorf("speech: build request: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return "", fmt.Errorf("speech: build request: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("speech: build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/audio/transcriptions"), &body)
	if err != nil {
		return "", fmt.Errorf("speech: build request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	c.authorize(req)

	resp, err := c.http().Do(req)
	if err != nil {
		// A url.Error quotes the URL, which carries no secret here; the
		// key travels in a header and is never part of it.
		return "", fmt.Errorf("speech: %s engine unreachable: %w", c.Where, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound && c.DownloadsModels {
		// Speaches has not got the model yet. Start fetching it and say
		// so; the next message after the download finishes succeeds.
		c.startWarming()
		return "", ErrWarming
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("speech: %s engine answered %d: %s", c.Where, resp.StatusCode, engineMessage(resp.Body))
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("speech: %s engine: decode response: %w", c.Where, err)
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		return "", ErrEmpty
	}
	return text, nil
}

// EnsureModel asks Speaches to download the configured model, and
// returns once it is present. It is a no-op for an engine that does not
// download models. Downloads take minutes, so the caller's context
// should allow for that.
func (c *Client) EnsureModel(ctx context.Context) error {
	if !c.DownloadsModels {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/models/"+modelPath(c.Model)), nil)
	if err != nil {
		return fmt.Errorf("speech: build request: %w", err)
	}
	c.authorize(req)
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("speech: local engine unreachable: %w", err)
	}
	defer resp.Body.Close()
	// 200 downloaded now, 201 already there.
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("speech: downloading %s: engine answered %d: %s", c.Model, resp.StatusCode, engineMessage(resp.Body))
	}
	return nil
}

// startWarming fetches the model in the background, once at a time.
func (c *Client) startWarming() {
	c.mu.Lock()
	if c.warming {
		c.mu.Unlock()
		return
	}
	c.warming = true
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			c.warming = false
			c.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
		defer cancel()
		_ = c.EnsureModel(ctx) // the next Transcribe finds out either way
	}()
}

// modelPath escapes a model id segment by segment: a Hugging Face id
// is "owner/name", and Speaches routes the slash as part of the path.
func modelPath(id string) string {
	parts := strings.Split(id, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (c *Client) endpoint(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + path
}

func (c *Client) authorize(req *http.Request) {
	if c.Key == nil {
		return
	}
	if key := c.Key(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func fileHeader(name, mimeType string) map[string][]string {
	return map[string][]string{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename=%q`, name)},
		"Content-Type":        {mimeType},
	}
}

// engineMessage extracts a short, printable reason from an error body:
// OpenAI's {"error":{"message"}} or Speaches' {"detail"}, else the
// first line of whatever came back.
func engineMessage(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, errBody))
	var shaped struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	if json.Unmarshal(raw, &shaped) == nil {
		if shaped.Error.Message != "" {
			return clip(shaped.Error.Message)
		}
		if s, ok := shaped.Detail.(string); ok && s != "" {
			return clip(s)
		}
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	if line == "" {
		return "no detail"
	}
	return clip(line)
}

func clip(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
