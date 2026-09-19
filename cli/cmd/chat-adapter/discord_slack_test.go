package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Discord: the gateway says Hello, the bot identifies with its token and
// the intents it needs, and what arrives as MESSAGE_CREATE becomes
// Incoming — except its own messages and other bots'. A message in a
// server channel is a group message.
func TestDiscordGatewayDeliversMessages(t *testing.T) {
	identified := make(chan map[string]any, 1)
	gw := wsServer(t, func(p *wsPeer) {
		p.text(`{"op":10,"d":{"heartbeat_interval":45000}}`)
		_, body, ok := p.read()
		if !ok {
			return
		}
		var f map[string]any
		json.Unmarshal([]byte(body), &f)
		identified <- f
		p.text(`{"op":0,"s":1,"t":"READY","d":{"user":{"id":"B1"}}}`)
		p.text(`{"op":0,"s":2,"t":"MESSAGE_CREATE","d":{"id":"m1","channel_id":"dm1","content":"hello","author":{"id":"U1","username":"mika","global_name":"Mika"}}}`)
		p.text(`{"op":0,"s":3,"t":"MESSAGE_CREATE","d":{"id":"m2","channel_id":"dm1","content":"my own reply","author":{"id":"B1","username":"joan"}}}`)
		p.text(`{"op":0,"s":4,"t":"MESSAGE_CREATE","d":{"id":"m3","channel_id":"c9","content":"beep","author":{"id":"X","username":"other","bot":true}}}`)
		p.text(`{"op":0,"s":5,"t":"MESSAGE_CREATE","d":{"id":"m4","channel_id":"c9","guild_id":"g1","content":"hi all","author":{"id":"U2","username":"sam"}}}`)
		p.read() // hold the connection until the client goes
	})
	tr, _ := newDiscord("tok", 3*time.Second)
	d := tr.(*discord)
	d.gateway = gw
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []Incoming
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		msgs, err := d.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, msgs...)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want mika's DM and sam's group message only", got)
	}
	if got[0].Account != "U1" || got[0].Conversation != "dm1" || got[0].Group || got[0].DisplayName != "Mika" || got[0].Text != "hello" {
		t.Errorf("the DM = %+v", got[0])
	}
	if !got[1].Group || got[1].Account != "U2" {
		t.Errorf("a server-channel message = %+v, want a group message", got[1])
	}
	f := <-identified
	data := f["d"].(map[string]any)
	if f["op"].(float64) != 2 || data["token"] != "tok" || int(data["intents"].(float64))&(1<<15) == 0 {
		t.Errorf("identify = %v, want op 2 with the token and the message-content intent", f)
	}
}

func TestDiscordSendsAndIdentifiesOverREST(t *testing.T) {
	var mu sync.Mutex
	var sent map[string]string
	var auth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/users/@me":
			w.Write([]byte(`{"id":"B1","username":"joan_bot","global_name":"Joan"}`))
		case "/channels/dm1/messages":
			json.NewDecoder(r.Body).Decode(&sent)
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	tr, _ := newDiscord("tok", time.Second)
	d := tr.(*discord)
	d.api = api.URL
	handle, name, err := d.Identify(context.Background())
	if err != nil || handle != "joan_bot" || name != "Joan" {
		t.Fatalf("identify = %q %q %v", handle, name, err)
	}
	if err := d.Send(context.Background(), "dm1", strings.Repeat("é", 2500)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bot tok" {
		t.Errorf("authorization = %q", auth)
	}
	if n := len([]rune(sent["content"])); n != maxDiscordBody {
		t.Errorf("a long reply was sent as %d characters, want it cut to %d", n, maxDiscordBody)
	}
}

// Slack: Socket Mode opens with the app token, every envelope is
// acknowledged, and only a person's plain message becomes Incoming.
func TestSlackSocketModeDeliversAndAcknowledges(t *testing.T) {
	acks := make(chan string, 8)
	socket := wsServer(t, func(p *wsPeer) {
		p.text(`{"type":"hello"}`)
		for i, ev := range []string{
			`{"type":"message","channel":"D1","channel_type":"im","user":"U1","text":"hello","ts":"1.1"}`,
			`{"type":"message","subtype":"message_changed","channel":"D1","channel_type":"im","user":"U1","text":"edit","ts":"1.2"}`,
			`{"type":"message","channel":"D1","channel_type":"im","bot_id":"BX","user":"U9","text":"bot","ts":"1.3"}`,
			`{"type":"message","channel":"C1","channel_type":"channel","user":"U2","text":"hi all","ts":"1.4"}`,
		} {
			p.text(`{"type":"events_api","envelope_id":"e` + string(rune('0'+i)) + `","payload":{"event":` + ev + `}}`)
		}
		for {
			_, body, ok := p.read()
			if !ok {
				return
			}
			var a map[string]string
			json.Unmarshal([]byte(body), &a)
			acks <- a["envelope_id"]
		}
	})
	var openedWith string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps.connections.open" {
			openedWith = r.Header.Get("Authorization")
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": socket})
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	tr, _ := newSlack("xoxb-bot", "xapp-app", 3*time.Second)
	s := tr.(*slack)
	s.api = api.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []Incoming
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		msgs, err := s.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, msgs...)
	}
	if len(got) != 2 || got[0].Account != "U1" || got[0].Group || got[0].Conversation != "D1" || !got[1].Group {
		t.Fatalf("got %+v, want U1's DM and U2's channel message", got)
	}
	if openedWith != "Bearer xapp-app" {
		t.Errorf("the socket was opened with %q, want the app-level token", openedWith)
	}
	seen := map[string]bool{}
	for len(seen) < 4 {
		select {
		case id := <-acks:
			seen[id] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("acknowledged only %v; every envelope must be, or Slack sends it again", seen)
		}
	}
}

func TestSlackSendsWithTheBotToken(t *testing.T) {
	var got struct{ auth, channel, text string }
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.URL.Path {
		case "/chat.postMessage":
			got.auth, got.channel, got.text = r.Header.Get("Authorization"), r.PostForm.Get("channel"), r.PostForm.Get("text")
			w.Write([]byte(`{"ok":true}`))
		case "/auth.test":
			w.Write([]byte(`{"ok":true,"user":"joan","user_id":"UB"}`))
		}
	}))
	defer api.Close()
	tr, _ := newSlack("xoxb-bot", "xapp-app", time.Second)
	s := tr.(*slack)
	s.api = api.URL
	if err := s.Send(context.Background(), "D1", "hi"); err != nil {
		t.Fatal(err)
	}
	if got.auth != "Bearer xoxb-bot" || got.channel != "D1" || got.text != "hi" {
		t.Errorf("postMessage = %+v", got)
	}
	if h, _, err := s.Identify(context.Background()); err != nil || h != "joan" {
		t.Errorf("identify = %q %v", h, err)
	}
}

func TestSlackAndDiscordNeedTheirTokens(t *testing.T) {
	if _, err := newSlack("xoxb", "", time.Second); err == nil {
		t.Error("Slack without an app-level token was accepted")
	}
	if _, err := newDiscord("", time.Second); err == nil {
		t.Error("Discord without a bot token was accepted")
	}
}
