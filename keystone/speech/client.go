package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// Systran/faster-whisper-small or gpt-4o-mini-transcribe, or for a
	// Client that speaks, speaches-ai/Kokoro-82M-v1.0-ONNX or
	// gpt-4o-mini-tts.
	Model string
	// Voice is the voice a speaking Client reads with, e.g. af_heart
	// (Kokoro) or coral (OpenAI). Unused for transcription.
	Voice string
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
	// state and detail are what the last attempt to fetch or use the
	// model found, for the Console to show (#408).
	state  ModelState
	detail string
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
	if resp.StatusCode/100 == 2 {
		c.setState(ModelReady, "")
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
		return fmt.Errorf("%w: %w", errUnreachable, err)
	}
	defer resp.Body.Close()
	// 200 downloaded now, 201 already there.
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("speech: downloading %s: engine answered %d: %s", c.Model, resp.StatusCode, engineMessage(resp.Body))
	}
	return nil
}

// errUnreachable is a download that could not reach the engine at all:
// the container is still starting, or is not there.
var errUnreachable = errors.New("speech: local engine unreachable")

// errDownloading reports that a model download is already running for
// this engine, started by somebody else.
var errDownloading = errors.New("speech: the model is already downloading")

// download runs EnsureModel unless one is already running for this
// Client, in which case it returns errDownloading at once. Every path
// that fetches the model — the startup Warm and the one a 404 starts —
// goes through here, so a model that is gigabytes is never requested
// twice at the same time.
func (c *Client) download(ctx context.Context) error {
	c.mu.Lock()
	if c.warming {
		c.mu.Unlock()
		return errDownloading
	}
	c.warming = true
	c.state, c.detail = ModelDownloading, ""
	c.mu.Unlock()
	err := c.EnsureModel(ctx)
	c.mu.Lock()
	c.warming = false
	switch {
	case err == nil:
		c.state, c.detail = ModelReady, ""
	case ctx.Err() != nil && c.state == ModelDownloading:
		// Stopped rather than failed: Keystone is shutting down or the
		// engine was replaced. Nothing is known either way.
		c.state, c.detail = ModelStarting, ""
	case errors.Is(err, errUnreachable):
		c.state, c.detail = ModelStarting, "the speech engine is not answering yet"
	default:
		c.state, c.detail = ModelFailed, err.Error()
	}
	c.mu.Unlock()
	return err
}

// modelState reports what is known about the model. A Client nobody
// has asked yet is starting: its engine may not be up.
func (c *Client) modelState() (ModelState, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == "" {
		return ModelStarting, ""
	}
	return c.state, c.detail
}

func (c *Client) setState(st ModelState, detail string) {
	c.mu.Lock()
	c.state, c.detail = st, detail
	c.mu.Unlock()
}

// startWarming fetches the model in the background, unless a download
// is already running.
func (c *Client) startWarming() {
	c.mu.Lock()
	busy := c.warming
	c.mu.Unlock()
	if busy {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
		defer cancel()
		_ = c.download(ctx) // the next Transcribe finds out either way
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
