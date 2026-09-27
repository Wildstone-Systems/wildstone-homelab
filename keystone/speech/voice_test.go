package speech

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A speaking Client sends the OpenAI speech shape, asks for MP3, and
// returns the bytes it got.
func TestSpeakSendsTheOpenAIShape(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, "ID3 mp3 bytes")
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1", Model: "gpt-4o-mini-tts", Voice: "coral", Where: Cloud,
		Key: func() string { return "sk-test" }}
	audio, err := c.Speak(context.Background(), "hello there")
	if err != nil || string(audio) != "ID3 mp3 bytes" {
		t.Fatalf("Speak = %q, %v", audio, err)
	}
	if got["model"] != "gpt-4o-mini-tts" || got["voice"] != "coral" || got["input"] != "hello there" || got["response_format"] != "mp3" {
		t.Errorf("body = %v", got)
	}
	if auth != "Bearer sk-test" {
		t.Errorf("authorization = %q", auth)
	}
}

func TestSpeakWithAMissingKeyIsUnavailable(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1/v1", Where: Cloud, Key: func() string { return "" }}
	if _, err := c.Speak(context.Background(), "hi"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// Speaches answers 404 for a voice model it has not downloaded: the
// Client starts the download and says "warming".
func TestSpeakDownloadsAMissingVoice(t *testing.T) {
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/audio/speech":
			http.Error(w, `{"detail":"Model is not installed locally"}`, http.StatusNotFound)
		case r.URL.Path == "/v1/models/speaches-ai/Kokoro-82M-v1.0-ONNX" && r.Method == http.MethodPost:
			downloads.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1", Model: "speaches-ai/Kokoro-82M-v1.0-ONNX", Voice: "af_heart",
		Where: Local, DownloadsModels: true}
	if _, err := c.Speak(context.Background(), "hi"); !errors.Is(err, ErrWarming) {
		t.Fatalf("err = %v, want ErrWarming", err)
	}
	for i := 0; i < 200 && downloads.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if downloads.Load() != 1 {
		t.Fatalf("downloads = %d, want 1", downloads.Load())
	}
}

type voiceStub struct {
	where Processing
	err   error
	calls atomic.Int32
	heard string
}

func (s *voiceStub) Processing() Processing { return s.where }
func (s *voiceStub) Speak(_ context.Context, text string) ([]byte, error) {
	s.calls.Add(1)
	s.heard = text
	if s.err != nil {
		return nil, s.err
	}
	return []byte("mp3:" + string(s.where)), nil
}

// The same privacy matrix as transcription (AC12): a reply that may not
// leave the house is read by the local voice or not at all.
func TestSpeakPrivacyMatrix(t *testing.T) {
	for _, tc := range []struct {
		mode       Mode
		allowCloud bool
		local      bool
		want       Processing
		wantErr    error
	}{
		{ModeOff, true, true, "", ErrOff},
		{ModeLocal, false, true, Local, nil},
		{ModeLocal, true, false, "", ErrUnavailable},
		{ModeCloud, false, true, "", ErrUnavailable},
		{ModeCloud, true, true, Cloud, nil},
		{ModeLocalThenCloud, false, false, "", ErrUnavailable},
		{ModeLocalThenCloud, true, false, Cloud, nil},
		{ModeLocalThenCloud, true, true, Local, nil},
	} {
		local, cloud := &voiceStub{where: Local}, &voiceStub{where: Cloud}
		r := &Router{ReplyMode: tc.mode, CloudVoice: cloud}
		if tc.local {
			r.LocalVoice = local
		}
		got, err := r.Speak(context.Background(), tc.allowCloud, "The lights are off.")
		if !errors.Is(err, tc.wantErr) || got.Processing != tc.want {
			t.Errorf("%s allowCloud=%v local=%v: got %q, %v; want %q, %v", tc.mode, tc.allowCloud, tc.local, got.Processing, err, tc.want, tc.wantErr)
		}
		if !tc.allowCloud && cloud.calls.Load() != 0 {
			t.Errorf("%s: a local-only reply reached the cloud voice", tc.mode)
		}
		if err == nil && got.MIME != SpokenMIME {
			t.Errorf("MIME = %q", got.MIME)
		}
		if r.CanSpeak(tc.allowCloud) != (tc.wantErr == nil) {
			t.Errorf("%s allowCloud=%v: CanSpeak disagrees with Speak", tc.mode, tc.allowCloud)
		}
	}
}

// A cloud voice put in the local slot by mistake is still cloud.
func TestSpeakTrustsProcessingNotTheSlot(t *testing.T) {
	cloud := &voiceStub{where: Cloud}
	r := &Router{ReplyMode: ModeLocal, LocalVoice: cloud}
	if _, err := r.Speak(context.Background(), false, "hi"); !errors.Is(err, ErrUnavailable) || cloud.calls.Load() != 0 {
		t.Fatalf("err = %v, calls = %d", err, cloud.calls.Load())
	}
}

func TestSpeakFallsBackToTheCloudOnlyWhenAllowed(t *testing.T) {
	local, cloud := &voiceStub{where: Local, err: errors.New("down")}, &voiceStub{where: Cloud}
	r := &Router{ReplyMode: ModeLocalThenCloud, LocalVoice: local, CloudVoice: cloud}
	got, err := r.Speak(context.Background(), true, "hi")
	if err != nil || got.Processing != Cloud {
		t.Fatalf("allowed: %v %v", got.Processing, err)
	}
	cloud.calls.Store(0)
	if _, err := r.Speak(context.Background(), false, "hi"); err == nil || cloud.calls.Load() != 0 {
		t.Fatalf("not allowed: err = %v, cloud calls = %d", err, cloud.calls.Load())
	}
}

func TestSpeakReadsTheSpeakableText(t *testing.T) {
	v := &voiceStub{where: Local}
	r := &Router{ReplyMode: ModeLocal, LocalVoice: v}
	if _, err := r.Speak(context.Background(), false, "**Done.** See https://example.com/x"); err != nil {
		t.Fatal(err)
	}
	if v.heard != "Done. See a link." {
		t.Fatalf("engine heard %q", v.heard)
	}
	if _, err := r.Speak(context.Background(), false, "```\ncode only\n```"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Speak(context.Background(), false, " \n---\n "); !errors.Is(err, ErrEmpty) {
		t.Fatalf("nothing to say: err = %v", err)
	}
}

func TestSpeakableText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Plain answer.", "Plain answer."},
		{"## Today\n- milk\n- eggs\n1. bread", "Today. milk. eggs. bread."},
		{"Use `wild status` to check.", "Use wild status to check."},
		{"See [the docs](https://example.com/docs) or https://x.io/a?b=c.", "See the docs or a link."},
		{"Run this:\n```sh\nrm -rf /\n```\nThen wait.", "Run this: (There's code in the message.) Then wait."},
		{"| Room | Temp |\n|---|---|\n| Kitchen | 21 |", "Room, Temp. Kitchen, 21."},
		{"> quoted **bold** ~~gone~~", "quoted bold gone."},
	} {
		if got := SpeakableText(tc.in); got != tc.want {
			t.Errorf("SpeakableText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSpeakableTextClipsAtASentence(t *testing.T) {
	long := strings.Repeat("This is one sentence of a long answer. ", 60)
	got := SpeakableText(long)
	if n := len([]rune(got)); n > MaxSpokenChars {
		t.Fatalf("clipped to %d characters, over %d", n, MaxSpokenChars)
	}
	if !strings.HasSuffix(got, "answer. The rest is in the message.") {
		t.Fatalf("clipped text ends %q", got[len(got)-60:])
	}
	noStops := strings.Repeat("word ", 400)
	if got := SpeakableText(noStops); !strings.HasSuffix(got, "word… The rest is in the message.") {
		t.Fatalf("no sentence end: ends %q", got[len(got)-40:])
	}
}

// Warm fetches the voice's model as well when replies are read locally.
func TestWarmDownloadsTheVoiceModel(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	r := &Router{
		Mode:       ModeLocal,
		Local:      &Client{BaseURL: srv.URL + "/v1", Model: "Systran/faster-whisper-small", Where: Local, DownloadsModels: true},
		ReplyMode:  ModeLocal,
		LocalVoice: &Client{BaseURL: srv.URL + "/v1", Model: "speaches-ai/Kokoro-82M-v1.0-ONNX", Where: Local, DownloadsModels: true},
	}
	r.Warm(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	want := []string{"/v1/models/Systran/faster-whisper-small", "/v1/models/speaches-ai/Kokoro-82M-v1.0-ONNX"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("downloads = %v, want %v", got, want)
	}
}
