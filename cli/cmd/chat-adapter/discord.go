package main

// Discord, through its gateway (ADR-W033 B7.1).
//
// Discord has no long-poll for a bot's messages: they arrive over the
// gateway, a WebSocket the bot keeps open and heartbeats. So this
// transport runs the gateway in the background and hands Receive what
// it collected, which keeps the adapter's loop the same for every
// platform. Replies go out over the REST API.
//
// The Owner does one thing in Discord's developer portal beyond making
// the bot: switch on the Message Content intent. Without it the gateway
// delivers messages with their text removed, and the bot would answer
// nothing. The Console's connection form says so.
//
// Resuming a dropped gateway session is not implemented: a reconnect
// identifies afresh. Messages sent in the seconds between are not
// replayed by Discord, and the person sees no reply and asks again —
// the same failure a restart of the adapter already has.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	discordAPI = "https://discord.com/api/v10"
	// The gateway intents this bot asks for: messages in servers (so a
	// message in a group channel is seen and refused, B9), direct
	// messages, and message content.
	discordIntents = 1<<9 | 1<<12 | 1<<15
	// Discord's limit on one message, in characters.
	maxDiscordBody = 2000
)

type discord struct {
	token   string
	api     string // overridable for tests
	gateway string // overridable for tests; asked of the API otherwise
	timeout time.Duration
	http    *http.Client

	once   sync.Once
	events chan Incoming
	errs   chan error

	mu    sync.Mutex
	botID string
}

func newDiscord(token string, pollWait time.Duration) (Transport, error) {
	if token == "" {
		return nil, fmt.Errorf("DISCORD_BOT_TOKEN is not set: %w", errNotConfigured)
	}
	if pollWait <= 0 {
		pollWait = 25 * time.Second
	}
	return &discord{token: token, api: discordAPI, timeout: pollWait,
		events: make(chan Incoming, 256), errs: make(chan error, 1)}, nil
}

func (d *discord) Name() string { return "discord" }

func (d *discord) client() *http.Client {
	if d.http != nil {
		return d.http
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Receive returns what the gateway delivered, waiting up to the poll
// window for the first message.
func (d *discord) Receive(ctx context.Context) ([]Incoming, error) {
	d.once.Do(func() { go d.gatewayLoop(ctx) })
	select {
	case m := <-d.events:
		out := []Incoming{m}
		for {
			select {
			case m := <-d.events:
				out = append(out, m)
			default:
				return out, nil
			}
		}
	case err := <-d.errs:
		return nil, err
	case <-time.After(d.timeout):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *discord) gatewayLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := d.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			select {
			case d.errs <- fmt.Errorf("discord gateway: %w", err):
			default:
			}
		}
		// A session that lasted is not a failure to back off from.
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		backoff = sleepBackoff(ctx, backoff)
	}
}

type discordFrame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

// session runs one gateway connection until it ends.
func (d *discord) session(ctx context.Context) error {
	gw := d.gateway
	if gw == "" {
		var out struct {
			URL string `json:"url"`
		}
		if err := d.call(ctx, http.MethodGet, "/gateway/bot", nil, &out); err != nil {
			return err
		}
		gw = out.URL
	}
	conn, err := wsDial(gw+"?v=10&encoding=json", 30*time.Second)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-sctx.Done(); conn.Close() }()

	var hello struct {
		Interval int64 `json:"heartbeat_interval"`
	}
	raw, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	var f discordFrame
	if err := json.Unmarshal(raw, &f); err != nil || f.Op != 10 || json.Unmarshal(f.D, &hello) != nil || hello.Interval <= 0 {
		return fmt.Errorf("expected Hello, got %.100s", raw)
	}
	identify, _ := json.Marshal(map[string]any{"op": 2, "d": map[string]any{
		"token": d.token, "intents": discordIntents,
		"properties": map[string]string{"os": "linux", "browser": "wildstone", "device": "wildstone"},
	}})
	if err := conn.WriteText(identify); err != nil {
		return err
	}

	var seqMu sync.Mutex
	var seq *int64
	beat := func() error {
		seqMu.Lock()
		b, _ := json.Marshal(map[string]any{"op": 1, "d": seq})
		seqMu.Unlock()
		return conn.WriteText(b)
	}
	go func() {
		t := time.NewTicker(time.Duration(hello.Interval) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				if beat() != nil {
					cancel()
					return
				}
			}
		}
	}()

	for {
		raw, err := conn.ReadMessage()
		if err != nil {
			if sctx.Err() != nil && ctx.Err() == nil {
				return fmt.Errorf("connection lost")
			}
			return err
		}
		var f discordFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		if f.S != nil {
			seqMu.Lock()
			seq = f.S
			seqMu.Unlock()
		}
		switch f.Op {
		case 0:
			d.dispatch(ctx, f)
		case 1:
			if err := beat(); err != nil {
				return err
			}
		case 7:
			return nil // Discord asks for a reconnect
		case 9:
			return fmt.Errorf("session invalidated (is the bot token right, and the Message Content intent on?)")
		}
	}
}

func (d *discord) dispatch(ctx context.Context, f discordFrame) {
	switch f.T {
	case "READY":
		var r struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		if json.Unmarshal(f.D, &r) == nil {
			d.mu.Lock()
			d.botID = r.User.ID
			d.mu.Unlock()
		}
	case "MESSAGE_CREATE":
		var m struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
			GuildID   string `json:"guild_id"`
			Content   string `json:"content"`
			Author    struct {
				ID         string `json:"id"`
				Username   string `json:"username"`
				GlobalName string `json:"global_name"`
				Bot        bool   `json:"bot"`
			} `json:"author"`
		}
		if json.Unmarshal(f.D, &m) != nil {
			return
		}
		d.mu.Lock()
		self := m.Author.ID == d.botID
		d.mu.Unlock()
		// Its own replies, and other bots, are not people talking to it.
		if self || m.Author.Bot || strings.TrimSpace(m.Content) == "" {
			return
		}
		name := m.Author.GlobalName
		if name == "" {
			name = m.Author.Username
		}
		in := Incoming{Account: m.Author.ID, Conversation: m.ChannelID, Group: m.GuildID != "",
			Text: m.Content, MessageID: m.ID, DisplayName: name}
		select {
		case d.events <- in:
		case <-ctx.Done():
		}
	}
}

func (d *discord) Send(ctx context.Context, conversation, body string) error {
	if utf8.RuneCountInString(body) > maxDiscordBody {
		body = string([]rune(body)[:maxDiscordBody])
	}
	return d.call(ctx, http.MethodPost, "/channels/"+conversation+"/messages", map[string]string{"content": body}, nil)
}

func (d *discord) Identify(ctx context.Context) (string, string, error) {
	var me struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
	}
	if err := d.call(ctx, http.MethodGet, "/users/@me", nil, &me); err != nil {
		return "", "", err
	}
	d.mu.Lock()
	d.botID = me.ID
	d.mu.Unlock()
	name := me.GlobalName
	if name == "" {
		name = me.Username
	}
	return me.Username, name, nil
}

// Advertise does nothing on Discord. Its command menu is slash commands,
// which arrive as interactions rather than messages and need handling
// this adapter does not have yet; typed commands still work as text.
func (d *discord) Advertise(context.Context, []Command) error { return nil }

func (d *discord) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+d.token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/chickenrat17/wildstone-homelab, 1)")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("discord %s %s: %s %.200s", method, path, resp.Status, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
