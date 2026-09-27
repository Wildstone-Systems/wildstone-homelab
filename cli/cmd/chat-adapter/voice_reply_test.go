package main

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// voiceTransport is a fakeTransport that can send voice messages.
type voiceTransport struct {
	fakeTransport
	vmu    sync.Mutex
	voices []sentVoice
}

type sentVoice struct {
	conversation, mimeType string
	audio                  []byte
}

func (v *voiceTransport) SendVoice(_ context.Context, conversation string, audio []byte, mimeType string) error {
	v.vmu.Lock()
	defer v.vmu.Unlock()
	v.voices = append(v.voices, sentVoice{conversation, mimeType, audio})
	return nil
}

func (v *voiceTransport) sentVoices() []sentVoice {
	v.vmu.Lock()
	defer v.vmu.Unlock()
	return append([]sentVoice(nil), v.voices...)
}

// A reply marked to speak arrives as text first, is acked, and is then
// sent as a voice message; a text-only reply never asks for audio.
func TestAdapterSendsTheSpokenReplyAfterTheText(t *testing.T) {
	ks := &fakeKeystone{
		pending: []outboundMessage{
			{Seq: 7, Target: "chat-1", Body: "🎙️ I heard: “lights?”\n\nThe lights are off.", Speak: true},
			{Seq: 8, Target: "chat-1", Body: "typed answer"},
		},
		speech: map[int64][]byte{7: []byte("ID3 mp3")},
	}
	tr := &voiceTransport{}
	a, done := newTestAdapter(t, ks, tr)
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runUntil(ctx, a, func() bool {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		return len(ks.acked) == 2 && len(tr.sentVoices()) == 1
	})

	if sent := tr.sentMessages(); len(sent) != 2 {
		t.Fatalf("text sent = %+v", sent)
	}
	voices := tr.sentVoices()
	if len(voices) != 1 || voices[0].conversation != "chat-1" || string(voices[0].audio) != "ID3 mp3" || voices[0].mimeType != "audio/mpeg" {
		t.Fatalf("voices = %+v", voices)
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.spoken) != 1 || ks.spoken[0] != 7 {
		t.Fatalf("asked for audio of %v, want only the reply marked to speak", ks.spoken)
	}
}

// Nothing to speak, or a transport with no voice: text only, and no
// error.
func TestAdapterWithoutSpeechSendsTextOnly(t *testing.T) {
	ks := &fakeKeystone{pending: []outboundMessage{{Seq: 3, Target: "chat-1", Body: "hi", Speak: true}}}
	tr := &voiceTransport{}
	a, done := newTestAdapter(t, ks, tr)
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runUntil(ctx, a, func() bool {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		return len(ks.spoken) == 1
	})
	if len(tr.sentMessages()) != 1 || len(tr.sentVoices()) != 0 {
		t.Fatalf("text = %+v, voices = %+v", tr.sentMessages(), tr.sentVoices())
	}
}

// readMultipart returns a multipart request's fields and its one file.
func readMultipart(t *testing.T, r *http.Request) (map[string]string, string, string, []byte) {
	t.Helper()
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	fields := map[string]string{}
	var fileField, fileType string
	var file []byte
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(p)
		if p.FileName() != "" {
			fileField, fileType, file = p.FormName(), p.Header.Get("Content-Type"), data
		} else {
			fields[p.FormName()] = string(data)
		}
	}
	return fields, fileField, fileType, file
}

func TestTelegramSendsAVoiceMessage(t *testing.T) {
	var fields map[string]string
	var field, ftype string
	var file []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botbot-token/sendVoice" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fields, field, ftype, file = readMultipart(t, r)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	defer srv.Close()
	tg := &telegram{token: "bot-token", base: srv.URL, timeout: time.Second, http: srv.Client()}
	if err := tg.SendVoice(context.Background(), "12345", []byte("ID3"), "audio/mpeg"); err != nil {
		t.Fatal(err)
	}
	if fields["chat_id"] != "12345" || field != "voice" || ftype != "audio/mpeg" || string(file) != "ID3" {
		t.Fatalf("fields = %v, file %s %s %q", fields, field, ftype, file)
	}
}

func TestTelegramVoiceErrorsNeverCarryTheToken(t *testing.T) {
	tg := &telegram{token: "secret-bot-token", base: "http://127.0.0.1:1", timeout: time.Second}
	err := tg.SendVoice(context.Background(), "1", []byte("x"), "audio/mpeg")
	if err == nil || strings.Contains(err.Error(), "secret-bot-token") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscordSendsTheReplyAsAnAttachment(t *testing.T) {
	var fields map[string]string
	var field string
	var file []byte
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/channels/C1/messages" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		fields, field, _, file = readMultipart(t, r)
		w.Write([]byte(`{"id":"1"}`)) //nolint:errcheck
	}))
	defer srv.Close()
	d := &discord{api: srv.URL, token: "tok", http: srv.Client()}
	if err := d.SendVoice(context.Background(), "C1", []byte("ID3"), "audio/mpeg"); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Attachments []struct {
			ID       int    `json:"id"`
			Filename string `json:"filename"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(fields["payload_json"]), &payload); err != nil || len(payload.Attachments) != 1 || payload.Attachments[0].Filename != "reply.mp3" {
		t.Fatalf("payload_json = %q (%v)", fields["payload_json"], err)
	}
	if field != "files[0]" || string(file) != "ID3" || auth != "Bot tok" {
		t.Fatalf("file field %q = %q, auth %q", field, file, auth)
	}
}

// Slack's external upload: an upload URL on its file host, the bytes
// there (without the bot token), then the file shared into the channel.
func TestSlackUploadsTheReplyAndSharesIt(t *testing.T) {
	var steps []string
	var uploaded []byte
	var uploadAuth, completeFiles, completeChannel string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files.getUploadURLExternal":
			r.ParseForm() //nolint:errcheck
			steps = append(steps, "url:"+r.Form.Get("filename")+":"+r.Form.Get("length"))
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "upload_url": srv.URL + "/upload/abc", "file_id": "F1"}) //nolint:errcheck
		case "/upload/abc":
			steps = append(steps, "upload")
			uploadAuth = r.Header.Get("Authorization")
			uploaded, _ = io.ReadAll(r.Body)
		case "/files.completeUploadExternal":
			r.ParseForm() //nolint:errcheck
			steps = append(steps, "complete")
			completeFiles, completeChannel = r.Form.Get("files"), r.Form.Get("channel_id")
			w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	s := &slack{api: srv.URL, botToken: "xoxb", http: srv.Client(), files: func(u *url.URL) bool { return u.Host == host }}
	if err := s.SendVoice(context.Background(), "D1", []byte("ID3mp3"), "audio/mpeg"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(steps, ",") != "url:reply.mp3:6,upload,complete" {
		t.Fatalf("steps = %v", steps)
	}
	if string(uploaded) != "ID3mp3" || uploadAuth != "" {
		t.Fatalf("uploaded %q with auth %q; the bot token must not go to the upload URL", uploaded, uploadAuth)
	}
	if completeChannel != "D1" || !strings.Contains(completeFiles, `"id":"F1"`) {
		t.Fatalf("complete files=%s channel=%s", completeFiles, completeChannel)
	}
}

func TestSlackVoiceRefusesAnUploadURLElsewhere(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/files.getUploadURLExternal" {
			t.Errorf("followed %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "upload_url": "https://evil.example/upload", "file_id": "F1"}) //nolint:errcheck
	}))
	defer srv.Close()
	s := &slack{api: srv.URL, botToken: "xoxb", http: srv.Client()}
	if err := s.SendVoice(context.Background(), "D1", []byte("x"), "audio/mpeg"); err == nil {
		t.Fatal("an upload URL off Slack's file host was followed")
	}
}

func TestSlackVoiceNamesTheMissingScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"error":"missing_scope"}`)) //nolint:errcheck
	}))
	defer srv.Close()
	s := &slack{api: srv.URL, botToken: "xoxb", http: srv.Client()}
	err := s.SendVoice(context.Background(), "D1", []byte("x"), "audio/mpeg")
	if err == nil || !strings.Contains(err.Error(), "files:write") {
		t.Fatalf("err = %v, want it to name files:write", err)
	}
}

func TestMatrixUploadsThenSendsAnAudioEvent(t *testing.T) {
	var uploaded []byte
	var uploadType string
	var event map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/_matrix/media/v3/upload":
			uploadType = r.Header.Get("Content-Type")
			uploaded, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"content_uri":"mxc://hs.example/abc"}`)) //nolint:errcheck
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:hs/send/m.room.message/"):
			json.NewDecoder(r.Body).Decode(&event) //nolint:errcheck
			w.Write([]byte(`{"event_id":"$e"}`))   //nolint:errcheck
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	m := &matrix{homeserver: srv.URL, token: "tok", timeout: time.Second, http: srv.Client()}
	if err := m.SendVoice(context.Background(), "!room:hs", []byte("ID3"), "audio/mpeg"); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "ID3" || uploadType != "audio/mpeg" {
		t.Fatalf("uploaded %q as %q", uploaded, uploadType)
	}
	if event["msgtype"] != "m.audio" || event["url"] != "mxc://hs.example/abc" {
		t.Fatalf("event = %v", event)
	}
	if _, ok := event["org.matrix.msc3245.voice"]; !ok {
		t.Error("the event is not marked as a voice message")
	}
}
