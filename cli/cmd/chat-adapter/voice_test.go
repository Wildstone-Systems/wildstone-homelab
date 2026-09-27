package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A recorded getUpdates batch with every kind of recording the driver
// reads, in a direct chat and in a group.
const telegramVoiceUpdates = `{"ok":true,"result":[
 {"update_id":201,"message":{"message_id":20,
   "voice":{"file_id":"VOICE1","duration":4,"mime_type":"audio/ogg","file_size":9000},
   "from":{"id":12345,"username":"sam"},"chat":{"id":12345,"type":"private"}}},
 {"update_id":202,"message":{"message_id":21,
   "video_note":{"file_id":"ROUND1","duration":7,"length":240,"file_size":50000},
   "from":{"id":12345,"username":"sam"},"chat":{"id":12345,"type":"private"}}},
 {"update_id":203,"message":{"message_id":22,
   "voice":{"file_id":"ROOMCHAT","duration":3},
   "from":{"id":777,"first_name":"Mika"},"chat":{"id":-99,"type":"supergroup"}}},
 {"update_id":204,"message":{"message_id":23,
   "voice":{"file_id":"ROOMREPLY","duration":3},
   "reply_to_message":{"from":{"id":4242}},
   "from":{"id":777,"first_name":"Mika"},"chat":{"id":-99,"type":"supergroup"}}},
 {"update_id":205,"message":{"message_id":24,
   "audio":{"file_id":"SONG1","duration":30,"mime_type":"audio/mpeg"},"caption":"@HouseBot what is this?",
   "from":{"id":777,"first_name":"Mika"},"chat":{"id":-99,"type":"supergroup"}}}
]}`

func voiceTelegram(t *testing.T, fileBody string) (*telegram, *atomic.Int32) {
	t.Helper()
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			io.WriteString(w, telegramVoiceUpdates)
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			id := r.URL.Query().Get("file_id")
			io.WriteString(w, `{"ok":true,"result":{"file_id":"`+id+`","file_path":"voice/`+id+`.oga"}}`)
		case strings.HasPrefix(r.URL.Path, "/file/botsecret-token/voice/"):
			downloads.Add(1)
			io.WriteString(w, fileBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	tg := &telegram{token: "secret-token", base: srv.URL, timeout: time.Second, http: srv.Client(),
		botID: 4242, botUsername: "HouseBot", started: true}
	return tg, &downloads
}

func TestTelegramReadsRecordings(t *testing.T) {
	tg, downloads := voiceTelegram(t, "OggS opus bytes")
	msgs, err := tg.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("msgs = %d, want all five recordings", len(msgs))
	}
	if downloads.Load() != 0 {
		t.Fatal("Receive downloaded audio; fetching is the adapter's decision, after its checks")
	}

	voice := msgs[0]
	if voice.Voice == nil || voice.Text != "" || voice.Group || !voice.Addressed {
		t.Fatalf("voice note mapped as %+v", voice)
	}
	if voice.Voice.MIME != "audio/ogg" || voice.Voice.Seconds != 4 || voice.Voice.Size != 9000 {
		t.Fatalf("voice note metadata = %+v", voice.Voice)
	}
	audio, err := voice.Voice.Fetch(context.Background(), 1<<20)
	if err != nil || string(audio) != "OggS opus bytes" {
		t.Fatalf("Fetch = %q, %v", audio, err)
	}

	if round := msgs[1].Voice; round == nil || round.MIME != "video/mp4" {
		t.Fatalf("video note = %+v, want MP4 by default", round)
	}
	// In a group a voice note cannot mention the bot, so it is addressed
	// only as a reply to it.
	if msgs[2].Addressed {
		t.Fatal("a group voice note nobody addressed to the bot was marked addressed")
	}
	if !msgs[3].Addressed {
		t.Fatal("a group voice note replying to the bot was not addressed")
	}
	// An audio file's caption can carry the mention; the caption is not
	// relayed as text, the recording is the message.
	if !msgs[4].Addressed || msgs[4].Text != "" || msgs[4].Voice.MIME != "audio/mpeg" {
		t.Fatalf("captioned audio mapped as %+v", msgs[4])
	}
}

func TestTelegramFetchRefusesLargeFiles(t *testing.T) {
	tg, _ := voiceTelegram(t, strings.Repeat("x", 100))
	// More bytes than the limit, whatever Telegram reported.
	if _, err := tg.fetchFile(context.Background(), "VOICE1", 50); !errors.Is(err, errVoiceTooLarge) {
		t.Fatalf("err = %v, want errVoiceTooLarge", err)
	}

	for name, body := range map[string]string{
		"reported size": `{"ok":true,"result":{"file_path":"voice/a.oga","file_size":999999999}}`,
		"refused":       `{"ok":false,"description":"Bad Request: file is too big"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		tg := &telegram{token: "t", base: srv.URL, timeout: time.Second, http: srv.Client()}
		if _, err := tg.fetchFile(context.Background(), "x", 1<<20); !errors.Is(err, errVoiceTooLarge) {
			t.Errorf("%s: err = %v, want errVoiceTooLarge", name, err)
		}
		srv.Close()
	}
}

// Every Telegram URL carries the bot token, and net/http quotes the URL
// in its errors. None of that may reach an error the adapter logs.
func TestTelegramErrorsDoNotCarryTheToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listening: every request fails in net/http
	tg := &telegram{token: "123456:SECRET-bot-token", base: base, timeout: time.Second, started: true,
		http: &http.Client{Timeout: time.Second}}

	_, recvErr := tg.Receive(context.Background())
	_, fetchErr := tg.fetchFile(context.Background(), "x", 10)
	for name, err := range map[string]error{"getUpdates": recvErr, "getFile": fetchErr} {
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s error carries the bot token: %v", name, err)
		}
	}
}

// voiceKeystone is a fake Keystone that also answers the voice door.
type voiceKeystone struct {
	fakeKeystone
	vmu    sync.Mutex
	voices []receivedVoice
}

type receivedVoice struct {
	meta  keystoneVoice
	audio []byte
	mime  string
}

func (k *voiceKeystone) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", k.fakeKeystone.handler())
	mux.HandleFunc("POST /api/v1/chat/voice", func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var got receivedVoice
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(p)
			switch p.FormName() {
			case "message":
				json.Unmarshal(data, &got.meta) //nolint:errcheck // test fake
			case "audio":
				got.audio, got.mime = data, p.Header.Get("Content-Type")
			}
		}
		k.vmu.Lock()
		k.voices = append(k.voices, got)
		k.vmu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"accepted":true,"bound":true,"queued":1}`)
	})
	return mux
}

func (k *voiceKeystone) received() []receivedVoice {
	k.vmu.Lock()
	defer k.vmu.Unlock()
	return append([]receivedVoice(nil), k.voices...)
}

func newVoiceAdapter(t *testing.T, ks *voiceKeystone, tr Transport) *adapter {
	t.Helper()
	srv := httptest.NewServer(ks.handler())
	t.Cleanup(srv.Close)
	return &adapter{
		keystone:  &keystoneClient{Base: srv.URL, Token: "wst-test", HTTP: srv.Client()},
		transport: tr, log: testLogger(), pollWait: 10 * time.Millisecond,
	}
}

type countingFetch struct {
	calls atomic.Int32
	audio []byte
	err   error
}

func (c *countingFetch) fetch(context.Context, int64) ([]byte, error) {
	c.calls.Add(1)
	return c.audio, c.err
}

func TestAdapterRelaysAVoiceNote(t *testing.T) {
	ks := &voiceKeystone{}
	f := &countingFetch{audio: []byte("OggS opus")}
	tr := &fakeTransport{incoming: [][]Incoming{{{
		Account: "12345", Conversation: "12345", MessageID: "20", DisplayName: "@sam", Addressed: true,
		Voice: &VoiceNote{MIME: "audio/ogg", Seconds: 4, Size: 9, Fetch: f.fetch},
	}}}}
	a := newVoiceAdapter(t, ks, tr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runUntil(ctx, a, func() bool { return len(ks.received()) > 0 })

	got := ks.received()
	if len(got) != 1 {
		t.Fatalf("voice uploads = %d, want 1", len(got))
	}
	v := got[0]
	if string(v.audio) != "OggS opus" || v.mime != "audio/ogg" {
		t.Fatalf("audio = %q (%s)", v.audio, v.mime)
	}
	if v.meta.Account != "12345" || v.meta.MessageID != "20" || v.meta.Transport != "faketransport" ||
		v.meta.AudioSeconds != 4 || v.meta.TooLarge || v.meta.Text != "" {
		t.Fatalf("message part = %+v", v.meta)
	}
	if len(ks.inbound) != 0 {
		t.Fatal("a voice note was also sent as text")
	}
}

func TestAdapterIgnoresUnaddressedGroupVoiceWithoutFetching(t *testing.T) {
	ks := &voiceKeystone{}
	f := &countingFetch{audio: []byte("x")}
	tr := &fakeTransport{incoming: [][]Incoming{{{
		Account: "777", Conversation: "-99", Group: true, Addressed: false, MessageID: "22",
		Voice: &VoiceNote{MIME: "audio/ogg", Seconds: 3, Fetch: f.fetch},
	}}}}
	a := newVoiceAdapter(t, ks, tr)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	runUntil(ctx, a, func() bool { return false })
	if f.calls.Load() != 0 || len(ks.received()) != 0 {
		t.Fatal("somebody else's voice note in a room was downloaded or relayed")
	}
}

// Too long by the vendor's own report: not downloaded, but still sent,
// marked, so a known person is told to send something shorter.
func TestAdapterSendsAnOversizedVoiceNoteUnfetched(t *testing.T) {
	for name, v := range map[string]*countingFetch{
		"reported too long":   {audio: []byte("x")},
		"found too big later": {err: errVoiceTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			ks := &voiceKeystone{}
			note := &VoiceNote{MIME: "audio/ogg", Seconds: 3, Fetch: v.fetch}
			if name == "reported too long" {
				note.Seconds = maxVoiceSeconds + 1
			}
			tr := &fakeTransport{incoming: [][]Incoming{{{
				Account: "12345", Conversation: "12345", Addressed: true, MessageID: "30", Voice: note,
			}}}}
			a := newVoiceAdapter(t, ks, tr)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			runUntil(ctx, a, func() bool { return len(ks.received()) > 0 })
			got := ks.received()
			if len(got) != 1 || !got[0].meta.TooLarge || len(got[0].audio) != 0 {
				t.Fatalf("uploads = %+v, want one marked tooLarge with no audio", got)
			}
			if name == "reported too long" && v.calls.Load() != 0 {
				t.Fatal("a recording reported too long was downloaded anyway")
			}
		})
	}
}
