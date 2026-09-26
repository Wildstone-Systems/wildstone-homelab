package speech

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEngine is an httptest stand-in for Speaches or OpenAI. It records
// what it was sent so a test can check the wire shape.
type fakeEngine struct {
	mu        sync.Mutex
	status    int
	text      string
	installed bool // Speaches: false answers 404 until /models is POSTed
	delay     time.Duration

	gotModel, gotLanguage, gotFile, gotType, gotAuth string
	gotAudio                                         []byte
	transcribes, downloads                           atomic.Int32
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/models/"):
		f.downloads.Add(1)
		f.mu.Lock()
		f.installed = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/audio/transcriptions":
		f.transcribes.Add(1)
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(MaxAudioBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.gotModel, f.gotLanguage = r.FormValue("model"), r.FormValue("language")
		file, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.gotFile, f.gotType = hdr.Filename, hdr.Header.Get("Content-Type")
		f.gotAudio, _ = io.ReadAll(file)
		if !f.installed {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"detail":"Model is not installed locally."}`)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
			return
		}
		io.WriteString(w, `{"text":`+quote(f.text)+`}`)
	default:
		http.NotFound(w, r)
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func newEngine(t *testing.T, f *fakeEngine) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

var voice = []byte("OggS fake opus bytes")

func TestClientSendsTheOpenAIShape(t *testing.T) {
	f := &fakeEngine{installed: true, text: "  buy milk  "}
	c := &Client{BaseURL: newEngine(t, f), Model: "gpt-4o-mini-transcribe", Key: func() string { return "sk-test" }, Where: Cloud}

	got, err := c.Transcribe(context.Background(), voice, "audio/ogg; codecs=opus", "en")
	if err != nil {
		t.Fatal(err)
	}
	if got != "buy milk" {
		t.Errorf("text = %q, want trimmed %q", got, "buy milk")
	}
	if f.gotModel != "gpt-4o-mini-transcribe" || f.gotLanguage != "en" {
		t.Errorf("model/language = %q/%q", f.gotModel, f.gotLanguage)
	}
	// OpenAI infers the format from the file name, so the extension is
	// part of the contract.
	if f.gotFile != "voice.ogg" || f.gotType != "audio/ogg" {
		t.Errorf("file = %q (%s), want voice.ogg (audio/ogg)", f.gotFile, f.gotType)
	}
	if string(f.gotAudio) != string(voice) {
		t.Error("audio bytes were not sent unchanged")
	}
	if f.gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", f.gotAuth)
	}
}

func TestClientOmitsAutoLanguageAndEmptyKey(t *testing.T) {
	f := &fakeEngine{installed: true, text: "hi"}
	c := &Client{BaseURL: newEngine(t, f), Model: "m", Where: Local}
	if _, err := c.Transcribe(context.Background(), voice, "audio/webm;codecs=opus", "auto"); err != nil {
		t.Fatal(err)
	}
	if f.gotLanguage != "" || f.gotAuth != "" {
		t.Errorf("language %q / auth %q should both be absent", f.gotLanguage, f.gotAuth)
	}
	if f.gotFile != "voice.webm" {
		t.Errorf("file = %q", f.gotFile)
	}
}

func TestClientErrorsNeverCarryTheKey(t *testing.T) {
	f := &fakeEngine{installed: true, status: http.StatusTooManyRequests}
	c := &Client{BaseURL: newEngine(t, f), Model: "m", Key: func() string { return "sk-very-secret" }, Where: Cloud}
	_, err := c.Transcribe(context.Background(), voice, "audio/ogg", "")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "sk-very-secret") {
		t.Fatalf("error leaks the key: %v", err)
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("error should name the status and the engine's reason: %v", err)
	}
}

func TestClientWithAMissingKeyIsUnavailable(t *testing.T) {
	f := &fakeEngine{installed: true, text: "x"}
	c := &Client{BaseURL: newEngine(t, f), Model: "m", Key: func() string { return "" }, Where: Cloud}
	if _, err := c.Transcribe(context.Background(), voice, "audio/ogg", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if f.transcribes.Load() != 0 {
		t.Error("a request went out without the key it needs")
	}
}

func TestClientEmptyTranscript(t *testing.T) {
	f := &fakeEngine{installed: true, text: "   "}
	c := &Client{BaseURL: newEngine(t, f), Model: "m", Where: Local}
	if _, err := c.Transcribe(context.Background(), voice, "audio/ogg", ""); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestClientDownloadsAMissingModel(t *testing.T) {
	f := &fakeEngine{text: "hello"}
	c := &Client{BaseURL: newEngine(t, f), Model: "Systran/faster-whisper-small", Where: Local, DownloadsModels: true}

	if _, err := c.Transcribe(context.Background(), voice, "audio/ogg", ""); !errors.Is(err, ErrWarming) {
		t.Fatalf("first call err = %v, want ErrWarming", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.downloads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.downloads.Load() == 0 {
		t.Fatal("a 404 should start the model download")
	}
	// Wait for the background download to finish before asking again.
	for time.Now().Before(deadline) {
		c.mu.Lock()
		w := c.warming
		c.mu.Unlock()
		if !w {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := c.Transcribe(context.Background(), voice, "audio/ogg", "")
	if err != nil || got != "hello" {
		t.Fatalf("after download: %q, %v", got, err)
	}
}

func TestEnsureModelKeepsTheSlashInTheID(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1/", Model: "Systran/faster-whisper-small", DownloadsModels: true}
	if err := c.EnsureModel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/models/Systran/faster-whisper-small" {
		t.Errorf("path = %q", path)
	}
}

func TestNormalizeMIME(t *testing.T) {
	for in, want := range map[string]string{
		"audio/ogg":              "audio/ogg",
		"audio/webm;codecs=opus": "audio/webm",
		"Audio/MP4":              "audio/mp4",
		"video/mp4":              "video/mp4", // Slack audio clips
	} {
		got, err := NormalizeMIME(in)
		if err != nil || got != want {
			t.Errorf("NormalizeMIME(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "image/png", "application/octet-stream", "text/plain", "not a type"} {
		if _, err := NormalizeMIME(bad); !errors.Is(err, ErrUnsupported) {
			t.Errorf("NormalizeMIME(%q) err = %v, want ErrUnsupported", bad, err)
		}
	}
}

func TestCheckSize(t *testing.T) {
	cases := []struct {
		size    int
		seconds float64
		max     int
		want    error
	}{
		{1000, 30, 0, nil},
		{1000, 0, 0, nil}, // no reported duration: size is the only bound
		{0, 5, 0, ErrEmpty},
		{MaxAudioBytes + 1, 5, 0, ErrTooLarge},
		{1000, 121, 0, ErrTooLarge},
		{1000, 45, 30, ErrTooLarge},
	}
	for _, c := range cases {
		if err := CheckSize(c.size, c.seconds, c.max); !errors.Is(err, c.want) && !(err == nil && c.want == nil) {
			t.Errorf("CheckSize(%d, %v, %d) = %v, want %v", c.size, c.seconds, c.max, err, c.want)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeOff, "off": ModeOff, "LOCAL": ModeLocal, "local-then-cloud": ModeLocalThenCloud, "cloud": ModeCloud} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("whisper"); err == nil {
		t.Error("an unknown mode must be refused")
	}
}

// stub is a Transcriber the Router tests drive directly.
type stub struct {
	where Processing
	text  string
	err   error
	calls atomic.Int32
}

func (s *stub) Processing() Processing { return s.where }
func (s *stub) Transcribe(context.Context, []byte, string, string) (string, error) {
	s.calls.Add(1)
	return s.text, s.err
}

// The privacy matrix (ADR-W042 §2.2 step 4, AC12): which engine each
// mode uses, with and without permission to leave the house.
func TestRouterPrivacyMatrix(t *testing.T) {
	cases := []struct {
		mode       Mode
		allowCloud bool
		local      bool // a local engine is configured
		want       Processing
		wantErr    error
	}{
		{ModeOff, true, true, "", ErrOff},
		{ModeLocal, true, true, Local, nil},
		{ModeLocal, false, true, Local, nil},
		{ModeLocal, true, false, "", ErrUnavailable}, // never the cloud, even when allowed
		{ModeCloud, true, true, Cloud, nil},
		{ModeCloud, false, true, "", ErrUnavailable}, // local-only turn, cloud-only house
		{ModeLocalThenCloud, true, true, Local, nil},
		{ModeLocalThenCloud, true, false, Cloud, nil},
		{ModeLocalThenCloud, false, false, "", ErrUnavailable},
	}
	for _, c := range cases {
		local := &stub{where: Local, text: "from local"}
		cloud := &stub{where: Cloud, text: "from cloud"}
		r := &Router{Mode: c.mode, Cloud: cloud}
		if c.local {
			r.Local = local
		}
		res, err := r.Transcribe(context.Background(), c.allowCloud, voice, "audio/ogg", 3)
		if !errors.Is(err, c.wantErr) || res.Processing != c.want {
			t.Errorf("%s allowCloud=%v local=%v: got %q, %v; want %q, %v", c.mode, c.allowCloud, c.local, res.Processing, err, c.want, c.wantErr)
		}
		if !c.allowCloud && cloud.calls.Load() > 0 {
			t.Errorf("%s: a local-only turn reached the cloud engine", c.mode)
		}
	}
}

func TestRouterFallsBackOnlyWhenAllowed(t *testing.T) {
	broken := &stub{where: Local, err: errors.New("connection refused")}
	cloud := &stub{where: Cloud, text: "from cloud"}
	r := &Router{Mode: ModeLocalThenCloud, Local: broken, Cloud: cloud}

	res, err := r.Transcribe(context.Background(), true, voice, "audio/ogg", 3)
	if err != nil || res.Processing != Cloud {
		t.Fatalf("allowed fallback: %+v, %v", res, err)
	}
	if _, err := r.Transcribe(context.Background(), false, voice, "audio/ogg", 3); err == nil {
		t.Fatal("a local-only turn must fail when the local engine does, not fall back")
	}
	if cloud.calls.Load() != 1 {
		t.Errorf("cloud calls = %d, want 1", cloud.calls.Load())
	}
}

func TestRouterDoesNotSendSilenceToTheCloud(t *testing.T) {
	local := &stub{where: Local, err: ErrEmpty}
	cloud := &stub{where: Cloud, text: "x"}
	r := &Router{Mode: ModeLocalThenCloud, Local: local, Cloud: cloud}
	if _, err := r.Transcribe(context.Background(), true, voice, "audio/ogg", 3); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
	if cloud.calls.Load() != 0 {
		t.Error("an empty recording was sent to the cloud for a second opinion")
	}
}

// A cloud engine put in the Local slot is still cloud: the Router asks
// the engine, not the slot.
func TestRouterTrustsProcessingNotTheSlot(t *testing.T) {
	mislabelled := &stub{where: Cloud, text: "x"}
	r := &Router{Mode: ModeLocal, Local: mislabelled}
	if _, err := r.Transcribe(context.Background(), false, voice, "audio/ogg", 3); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if mislabelled.calls.Load() != 0 {
		t.Error("a local-only turn reached a cloud engine configured as local")
	}
}

func TestRouterRefusesBeforeCallingAnEngine(t *testing.T) {
	local := &stub{where: Local, text: "x"}
	r := &Router{Mode: ModeLocal, Local: local, MaxSeconds: 60}
	for name, call := range map[string]func() error{
		"too long":    func() error { _, e := r.Transcribe(context.Background(), false, voice, "audio/ogg", 61); return e },
		"unsupported": func() error { _, e := r.Transcribe(context.Background(), false, voice, "image/png", 3); return e },
		"empty":       func() error { _, e := r.Transcribe(context.Background(), false, nil, "audio/ogg", 3); return e },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if local.calls.Load() != 0 {
		t.Error("an engine was called for a recording that should have been refused first")
	}
}

func TestRouterTimesOut(t *testing.T) {
	f := &fakeEngine{installed: true, text: "late", delay: 2 * time.Second}
	r := &Router{Mode: ModeLocal, Local: &Client{BaseURL: newEngine(t, f), Model: "m", Where: Local}, Timeout: 50 * time.Millisecond}
	start := time.Now()
	if _, err := r.Transcribe(context.Background(), false, voice, "audio/ogg", 3); err == nil {
		t.Fatal("want a timeout error")
	}
	if time.Since(start) > time.Second {
		t.Error("the timeout was not applied")
	}
}

func TestRouterRunsOneAtATime(t *testing.T) {
	var inFlight, peak atomic.Int32
	slow := transcriberFunc(func() (string, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return "ok", nil
	})
	r := &Router{Mode: ModeLocal, Local: slow}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Transcribe(context.Background(), false, voice, "audio/ogg", 3); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Errorf("peak concurrency = %d, want 1", peak.Load())
	}
}

type transcriberFunc func() (string, error)

func (f transcriberFunc) Processing() Processing { return Local }
func (f transcriberFunc) Transcribe(context.Context, []byte, string, string) (string, error) {
	return f()
}

func TestRouterEnabledAndAvailable(t *testing.T) {
	var nilRouter *Router
	if nilRouter.Enabled() || nilRouter.Available(true) {
		t.Error("a nil Router is not enabled")
	}
	r := &Router{Mode: ModeCloud, Cloud: &stub{where: Cloud}}
	if !r.Enabled() || !r.Available(true) || r.Available(false) {
		t.Error("cloud-only: enabled, available only when cloud is allowed")
	}
	r = &Router{Mode: ModeLocal}
	if r.Enabled() {
		t.Error("local mode with no local engine is not enabled")
	}
}

func TestWarmDownloadsTheModel(t *testing.T) {
	f := &fakeEngine{}
	r := &Router{Mode: ModeLocal, Local: &Client{BaseURL: newEngine(t, f), Model: "Systran/faster-whisper-small", Where: Local, DownloadsModels: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.Warm(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if f.downloads.Load() != 1 {
		t.Fatalf("downloads = %d, want 1", f.downloads.Load())
	}
}
