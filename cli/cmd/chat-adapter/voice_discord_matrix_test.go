package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// discordWith feeds MESSAGE_CREATE payloads straight into the driver, as
// the gateway would, and returns what it made of them.
func discordWith(t *testing.T, cdn *httptest.Server, payloads ...string) []Incoming {
	t.Helper()
	d := &discord{token: "tok", events: make(chan Incoming, 16), botID: "B1"}
	if cdn != nil {
		d.http = cdn.Client()
		d.cdn = func(u *url.URL) bool { return strings.HasPrefix(u.String(), cdn.URL) }
	}
	for _, p := range payloads {
		d.dispatch(context.Background(), discordFrame{T: "MESSAGE_CREATE", D: []byte(p)})
	}
	var out []Incoming
	for {
		select {
		case in := <-d.events:
			out = append(out, in)
		default:
			return out
		}
	}
}

func TestDiscordReadsVoiceMessages(t *testing.T) {
	var downloads atomic.Int32
	var gotAuth string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, "OggS opus bytes")
	}))
	defer cdn.Close()
	att := func(extra string) string {
		return `"attachments":[{"url":"` + cdn.URL + `/attachments/1/voice-message.ogg?ex=sig","content_type":"audio/ogg","size":9000,"duration_secs":4.2` + extra + `}]`
	}
	got := discordWith(t, cdn,
		// A voice message in a DM.
		`{"id":"v1","channel_id":"dm1","content":"","flags":8192,`+att("")+`,"author":{"id":"U1","username":"mika"}}`,
		// In a server channel, not addressed to anyone.
		`{"id":"v2","channel_id":"c9","guild_id":"g1","content":"","flags":8192,`+att("")+`,"author":{"id":"U2","username":"sam"}}`,
		// In a server channel, replying to the bot.
		`{"id":"v3","channel_id":"c9","guild_id":"g1","content":"","flags":8192,`+att("")+`,"referenced_message":{"author":{"id":"B1"}},"author":{"id":"U2","username":"sam"}}`,
		// An image is still not something to relay.
		`{"id":"i1","channel_id":"dm1","content":"","attachments":[{"url":"`+cdn.URL+`/a.png","content_type":"image/png"}],"author":{"id":"U1","username":"mika"}}`,
	)
	if len(got) != 3 {
		t.Fatalf("got %d messages, want the three recordings: %+v", len(got), got)
	}
	if downloads.Load() != 0 {
		t.Fatal("the driver downloaded audio; fetching is the adapter's decision, after its checks")
	}
	dm := got[0]
	if dm.Voice == nil || dm.Text != "" || dm.Group || !dm.Addressed || dm.MessageID != "v1" {
		t.Fatalf("voice DM = %+v", dm)
	}
	if dm.Voice.MIME != "audio/ogg" || dm.Voice.Seconds != 4.2 || dm.Voice.Size != 9000 {
		t.Fatalf("voice metadata = %+v", dm.Voice)
	}
	if got[1].Addressed {
		t.Error("a server-channel voice message nobody addressed was marked addressed")
	}
	if !got[2].Addressed {
		t.Error("a server-channel voice message replying to the bot was not addressed")
	}
	audio, err := dm.Voice.Fetch(context.Background(), 1<<20)
	if err != nil || string(audio) != "OggS opus bytes" {
		t.Fatalf("Fetch = %q, %v", audio, err)
	}
	if gotAuth != "" {
		t.Error("the bot token was sent to the CDN")
	}
	if _, err := dm.Voice.Fetch(context.Background(), 5); !errors.Is(err, errVoiceTooLarge) {
		t.Fatalf("over the limit: err = %v, want errVoiceTooLarge", err)
	}
}

// The adapter follows attachment links to Discord's CDN and nowhere else.
func TestDiscordFetchesOnlyFromItsCDN(t *testing.T) {
	d := &discord{}
	for _, link := range []string{
		"http://cdn.discordapp.com/attachments/1/a.ogg", // not https
		"https://evil.example/attachments/1/a.ogg",      // not Discord
		"https://cdn.discordapp.com.evil.example/a.ogg", // look-alike
		"https://127.0.0.1/attachments/1/a.ogg",         // the host itself
	} {
		if _, err := d.fetchAttachment(context.Background(), link, 1<<20); err == nil || errors.Is(err, errVoiceTooLarge) {
			t.Errorf("%s: fetched, want refused", link)
		}
	}
	u, _ := url.Parse("https://cdn.discordapp.com/attachments/1/a.ogg")
	if !d.cdnAllowed(u) {
		t.Error("Discord's own CDN was refused")
	}
}

// matrixWith serves a homeserver whose /sync returns events, and whose
// media endpoints serve media.
type fakeHomeserver struct {
	events     string
	media      string
	legacyOnly bool // an older homeserver without authenticated media
	gotAuth    atomic.Value
	paths      []string
}

func (f *fakeHomeserver) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/account/whoami"):
			io.WriteString(w, `{"user_id":"@joan:example.org"}`)
		case strings.HasSuffix(r.URL.Path, "/sync"):
			io.WriteString(w, `{"next_batch":"b2","rooms":{"join":{
			  "!dm:example.org":{"summary":{"m.joined_member_count":2},"timeline":{"events":[`+f.events+`]}},
			  "!room:example.org":{"summary":{"m.joined_member_count":5},"timeline":{"events":[
			    {"type":"m.room.message","event_id":"$g1","sender":"@sam:example.org","content":{"msgtype":"m.audio","body":"voice","url":"mxc://example.org/room1","info":{"mimetype":"audio/ogg","duration":3000}}},
			    {"type":"m.room.message","event_id":"$g2","sender":"@sam:example.org","content":{"msgtype":"m.audio","body":"voice","url":"mxc://example.org/room2","info":{"mimetype":"audio/ogg"},"m.mentions":{"user_ids":["@joan:example.org"]}}}
			  ]}}}}}`)
		case strings.Contains(r.URL.Path, "/media/"):
			f.paths = append(f.paths, r.URL.Path)
			f.gotAuth.Store(r.Header.Get("Authorization"))
			if f.legacyOnly && strings.HasPrefix(r.URL.Path, "/_matrix/client/v1/media/") {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"errcode":"M_UNRECOGNIZED","error":"Unrecognized request"}`)
				return
			}
			io.WriteString(w, f.media)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func receiveMatrix(t *testing.T, hs *fakeHomeserver) []Incoming {
	t.Helper()
	srv := hs.server(t)
	m := &matrix{homeserver: srv.URL, token: "syt_test", timeout: time.Second, http: srv.Client(), since: "b1"}
	msgs, err := m.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestMatrixReadsVoiceMessages(t *testing.T) {
	hs := &fakeHomeserver{media: "OggS opus bytes", events: `
	  {"type":"m.room.message","event_id":"$v1","sender":"@mika:example.org","content":{"msgtype":"m.audio","body":"Voice message","url":"mxc://example.org/abc123","info":{"mimetype":"audio/ogg","size":9000,"duration":4200},"org.matrix.msc3245.voice":{}}},
	  {"type":"m.room.message","event_id":"$i1","sender":"@mika:example.org","content":{"msgtype":"m.image","body":"cat.png","url":"mxc://example.org/cat"}},
	  {"type":"m.room.message","event_id":"$e1","sender":"@mika:example.org","content":{"msgtype":"m.audio","body":"secret.ogg","file":{"url":"mxc://example.org/enc"}}}`}
	msgs := receiveMatrix(t, hs)

	byID := map[string]Incoming{}
	for _, m := range msgs {
		byID[m.MessageID] = m
	}
	if _, ok := byID["$i1"]; ok {
		t.Error("an image was relayed")
	}
	if _, ok := byID["$e1"]; ok {
		t.Error("an encrypted attachment the bot cannot read was relayed")
	}
	dm, ok := byID["$v1"]
	if !ok || dm.Voice == nil || dm.Text != "" || dm.Group || !dm.Addressed {
		t.Fatalf("voice DM = %+v", dm)
	}
	if dm.Voice.MIME != "audio/ogg" || dm.Voice.Seconds != 4.2 || dm.Voice.Size != 9000 {
		t.Fatalf("voice metadata = %+v", dm.Voice)
	}
	if byID["$g1"].Addressed {
		t.Error("a group voice message nobody addressed was marked addressed")
	}
	if !byID["$g2"].Addressed {
		t.Error("a group voice message mentioning the bot was not addressed")
	}
	if len(hs.paths) != 0 {
		t.Fatal("Receive downloaded audio; fetching is the adapter's decision")
	}

	audio, err := dm.Voice.Fetch(context.Background(), 1<<20)
	if err != nil || string(audio) != "OggS opus bytes" {
		t.Fatalf("Fetch = %q, %v", audio, err)
	}
	if hs.paths[0] != "/_matrix/client/v1/media/download/example.org/abc123" {
		t.Errorf("fetched %s, want the authenticated media endpoint", hs.paths[0])
	}
	if hs.gotAuth.Load() != "Bearer syt_test" {
		t.Error("the media request did not carry the bot's token")
	}
	if _, err := dm.Voice.Fetch(context.Background(), 5); !errors.Is(err, errVoiceTooLarge) {
		t.Fatalf("over the limit: err = %v, want errVoiceTooLarge", err)
	}
}

// An older homeserver without authenticated media is still served, from
// the legacy endpoint.
func TestMatrixFallsBackToLegacyMedia(t *testing.T) {
	hs := &fakeHomeserver{media: "OggS", legacyOnly: true, events: `
	  {"type":"m.room.message","event_id":"$v1","sender":"@mika:example.org","content":{"msgtype":"m.audio","body":"v","url":"mxc://example.org/abc123","info":{"mimetype":"audio/ogg"}}}`}
	msgs := receiveMatrix(t, hs)
	var v *VoiceNote
	for _, m := range msgs {
		if m.MessageID == "$v1" {
			v = m.Voice
		}
	}
	if v == nil {
		t.Fatal("no voice message")
	}
	audio, err := v.Fetch(context.Background(), 1<<20)
	if err != nil || string(audio) != "OggS" {
		t.Fatalf("Fetch = %q, %v", audio, err)
	}
	if last := hs.paths[len(hs.paths)-1]; last != "/_matrix/media/v3/download/example.org/abc123" {
		t.Errorf("fell back to %s, want the legacy download endpoint", last)
	}
}

func TestMatrixRefusesMalformedMediaURIs(t *testing.T) {
	for _, uri := range []string{"", "https://example.org/a", "mxc://example.org", "mxc:///abc", "mxc://example.org/a/b", "mxc://../x"} {
		if _, _, ok := parseMXC(uri); ok {
			t.Errorf("parseMXC(%q) accepted it", uri)
		}
	}
	if s, id, ok := parseMXC("mxc://example.org/abc123"); !ok || s != "example.org" || id != "abc123" {
		t.Errorf("parseMXC = %q %q %v", s, id, ok)
	}
}

// slackWith feeds message events straight into the driver, as Socket
// Mode would, and returns what it made of them.
func slackWith(t *testing.T, files *httptest.Server, events ...string) []Incoming {
	t.Helper()
	s := &slack{botToken: "xoxb-test", events: make(chan Incoming, 16), botID: "B1"}
	if files != nil {
		s.http = files.Client()
		s.files = func(u *url.URL) bool { return strings.HasPrefix(u.String(), files.URL) }
	}
	for _, e := range events {
		s.event(context.Background(), []byte(e))
	}
	var out []Incoming
	for {
		select {
		case in := <-s.events:
			out = append(out, in)
		default:
			return out
		}
	}
}

func TestSlackReadsAudioClips(t *testing.T) {
	var gotAuth string
	var downloads atomic.Int32
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "video/mp4")
		io.WriteString(w, "mp4 audio bytes")
	}))
	defer files.Close()
	clip := `"files":[{"subtype":"slack_audio","mimetype":"video/mp4","size":9000,"duration_ms":4200,"url_private_download":"` + files.URL + `/files-pri/T1-F1/download/audio_message.mp4"}]`
	got := slackWith(t, files,
		// A clip in a DM.
		`{"type":"message","subtype":"file_share","user":"U1","channel":"D1","channel_type":"im","ts":"1.1","text":"",`+clip+`}`,
		// In a channel, not addressed.
		`{"type":"message","subtype":"file_share","user":"U2","channel":"C1","channel_type":"channel","ts":"1.2","text":"",`+clip+`}`,
		// In a channel, with the bot mentioned in its caption.
		`{"type":"message","subtype":"file_share","user":"U2","channel":"C1","channel_type":"channel","ts":"1.3","text":"<@B1> listen",`+clip+`}`,
		// A shared photo is still nothing to relay.
		`{"type":"message","subtype":"file_share","user":"U1","channel":"D1","channel_type":"im","ts":"1.4","text":"","files":[{"mimetype":"image/png","url_private_download":"`+files.URL+`/a.png"}]}`,
		// An edit is still ignored.
		`{"type":"message","subtype":"message_changed","user":"U1","channel":"D1","channel_type":"im","ts":"1.5","text":"x"}`,
	)
	if len(got) != 3 {
		t.Fatalf("got %d messages, want the three clips: %+v", len(got), got)
	}
	if downloads.Load() != 0 {
		t.Fatal("the driver downloaded audio; fetching is the adapter's decision")
	}
	dm := got[0]
	if dm.Voice == nil || dm.Text != "" || dm.Group || !dm.Addressed || dm.MessageID != "1.1" {
		t.Fatalf("clip DM = %+v", dm)
	}
	// Slack labels its audio clips video/mp4; Keystone accepts that.
	if dm.Voice.MIME != "video/mp4" || dm.Voice.Seconds != 4.2 || dm.Voice.Size != 9000 {
		t.Fatalf("clip metadata = %+v", dm.Voice)
	}
	if got[1].Addressed {
		t.Error("a channel clip nobody addressed was marked addressed")
	}
	if !got[2].Addressed || got[2].Text != "" {
		t.Errorf("a channel clip mentioning the bot = %+v, want addressed with no text", got[2])
	}
	audio, err := dm.Voice.Fetch(context.Background(), 1<<20)
	if err != nil || string(audio) != "mp4 audio bytes" {
		t.Fatalf("Fetch = %q, %v", audio, err)
	}
	if gotAuth != "Bearer xoxb-test" {
		t.Errorf("Authorization = %q, want the bot token", gotAuth)
	}
	if _, err := dm.Voice.Fetch(context.Background(), 5); !errors.Is(err, errVoiceTooLarge) {
		t.Fatalf("over the limit: err = %v, want errVoiceTooLarge", err)
	}
}

// Without files:read Slack answers with its sign-in page, not the file.
// That is said plainly, rather than sent to Keystone as a recording.
func TestSlackSaysWhenTheFilesScopeIsMissing(t *testing.T) {
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<html>sign in</html>")
	}))
	defer files.Close()
	got := slackWith(t, files, `{"type":"message","subtype":"file_share","user":"U1","channel":"D1","channel_type":"im","ts":"1.1","files":[{"subtype":"slack_audio","mimetype":"video/mp4","url_private_download":"`+files.URL+`/f"}]}`)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if _, err := got[0].Voice.Fetch(context.Background(), 1<<20); !errors.Is(err, errSlackFilesScope) {
		t.Fatalf("err = %v, want errSlackFilesScope", err)
	}
}

// The bot token is sent to Slack's file host and nowhere else.
func TestSlackFetchesOnlyFromItsFileHost(t *testing.T) {
	s := &slack{botToken: "xoxb-test"}
	for _, link := range []string{
		"http://files.slack.com/files-pri/T1-F1/a.mp4",
		"https://evil.example/files-pri/T1-F1/a.mp4",
		"https://files.slack.com.evil.example/a.mp4",
	} {
		if _, err := s.fetchFile(context.Background(), link, 1<<20); err == nil || errors.Is(err, errVoiceTooLarge) {
			t.Errorf("%s: fetched, want refused", link)
		}
	}
	u, _ := url.Parse("https://files.slack.com/files-pri/T1-F1/a.mp4")
	if !s.fileAllowed(u) {
		t.Error("Slack's own file host was refused")
	}
}
