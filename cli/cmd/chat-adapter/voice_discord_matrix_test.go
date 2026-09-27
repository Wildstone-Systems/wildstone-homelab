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
