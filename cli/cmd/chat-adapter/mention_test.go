package main

import (
	"context"
	"encoding/json"
	"testing"
)

// A mention is found in any case and removed, so the Agent reads the
// question and not its own name.
func TestStripMention(t *testing.T) {
	for _, c := range []struct {
		text, mention, want string
		hit                 bool
	}{
		{"@Joan_Bot what's for dinner?", "@joan_bot", "what's for dinner?", true},
		{"hey @joan_bot, lights off", "@joan_bot", "hey , lights off", true},
		{"what's for dinner?", "@joan_bot", "what's for dinner?", false},
		{"<@123> ping", "<@123>", "ping", true},
		{"héllo @joan_bot ça va", "@joan_bot", "héllo ça va", true},
	} {
		got, hit := stripMention(c.text, c.mention)
		if got != c.want || hit != c.hit {
			t.Errorf("stripMention(%q, %q) = %q %v, want %q %v", c.text, c.mention, got, hit, c.want, c.hit)
		}
	}
}

// Telegram: in a group, a message is for the bot when it names the bot
// or replies to it; in a direct chat, always.
func TestTelegramKnowsWhatIsAddressedToIt(t *testing.T) {
	tg := &telegram{botID: 42, botUsername: "joan_bot"}
	if _, ok := tg.addressedIn("dinner at 6?"); ok {
		t.Error("an unaddressed group message counted as addressed")
	}
	if text, ok := tg.addressedIn("@joan_bot dinner at 6?"); !ok || text != "dinner at 6?" {
		t.Errorf("mention = %q %v", text, ok)
	}
	unknown := &telegram{}
	if _, ok := unknown.addressedIn("@joan_bot hi"); ok {
		t.Error("a bot that does not know its own name took a mention as addressed")
	}
}

// Discord: a server message is addressed only when it mentions the bot,
// and the mention tag is removed.
func TestDiscordKnowsWhatIsAddressedToIt(t *testing.T) {
	d := &discord{events: make(chan Incoming, 4), botID: "B1"}
	send := func(content string, mentions ...string) Incoming {
		ms := []map[string]string{}
		for _, m := range mentions {
			ms = append(ms, map[string]string{"id": m})
		}
		raw, _ := json.Marshal(map[string]any{"id": "m", "channel_id": "c", "guild_id": "g", "content": content,
			"mentions": ms, "author": map[string]any{"id": "U1", "username": "sam"}})
		d.dispatch(context.Background(), discordFrame{T: "MESSAGE_CREATE", D: raw})
		return <-d.events
	}
	if in := send("anyone around?"); in.Addressed {
		t.Error("an unmentioned server message counted as addressed")
	}
	if in := send("<@B1> lights off", "B1"); !in.Addressed || in.Text != "lights off" {
		t.Errorf("mentioned = %+v", in)
	}
}

// Slack: a channel message is addressed only with <@bot>.
func TestSlackKnowsWhatIsAddressedToIt(t *testing.T) {
	s := &slack{events: make(chan Incoming, 4), botID: "UB"}
	ev := func(text string) Incoming {
		raw, _ := json.Marshal(map[string]string{"type": "message", "channel": "C1", "channel_type": "channel", "user": "U2", "text": text, "ts": "1"})
		s.event(context.Background(), raw)
		return <-s.events
	}
	if in := ev("standup?"); in.Addressed {
		t.Error("an unmentioned channel message counted as addressed")
	}
	if in := ev("<@UB> what's on today"); !in.Addressed || in.Text != "what's on today" {
		t.Errorf("mentioned = %+v", in)
	}
}

// Matrix: a group-room message is addressed by m.mentions, a pill link,
// or the bot's user id in the text; a direct room is always addressed.
func TestMatrixKnowsWhatIsAddressedToIt(t *testing.T) {
	const sync = `{"next_batch":"s2","rooms":{"join":{
	  "!kitchen:example.org":{"summary":{"m.joined_member_count":4},"timeline":{"events":[
	    {"type":"m.room.message","event_id":"$a","sender":"@sam:example.org","content":{"msgtype":"m.text","body":"bins?"}},
	    {"type":"m.room.message","event_id":"$b","sender":"@sam:example.org","content":{"msgtype":"m.text","body":"Butler: bins?",
	      "m.mentions":{"user_ids":["@butler:example.org"]}}},
	    {"type":"m.room.message","event_id":"$c","sender":"@sam:example.org","content":{"msgtype":"m.text","body":"@butler:example.org bins?"}}
	  ]}}}}}`
	srv, _ := matrixServer(t, sync)
	tr, err := newMatrix(srv.URL, "tok", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := tr.Receive(ctx); err != nil { // takes the cursor
		t.Fatal(err)
	}
	got, err := tr.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Incoming{}
	for _, in := range got {
		by[in.MessageID] = in
	}
	if by["$a"].Addressed || !by["$b"].Addressed || !by["$c"].Addressed || by["$c"].Text != "bins?" {
		t.Errorf("addressed = %+v", by)
	}
}
