package main

// Slack, through Socket Mode (ADR-W033 B7.1).
//
// Socket Mode is how a Slack app receives events without a public URL
// for Slack to call — which a homelab behind a tailnet does not have.
// It takes two tokens, which is why the connection form asks Slack for
// two: the app-level token (xapp-…) opens the socket, and the bot token
// (xoxb-…) sends replies and says who the bot is.
//
// Every event envelope must be acknowledged, or Slack delivers it
// again. Messages are acknowledged as they are received, before they are
// relayed: a message Keystone refuses is refused, not retried by Slack.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	slackAPI = "https://slack.com/api"
	// Slack truncates a message's text well past this; 4000 keeps one
	// reply one message.
	maxSlackBody = 4000
)

type slack struct {
	botToken string
	appToken string
	api      string // overridable for tests
	timeout  time.Duration
	http     *http.Client

	once   sync.Once
	events chan Incoming
	errs   chan error

	mu    sync.Mutex
	botID string
}

func newSlack(botToken, appToken string, pollWait time.Duration) (Transport, error) {
	if botToken == "" {
		return nil, fmt.Errorf("SLACK_BOT_TOKEN is not set: %w", errNotConfigured)
	}
	if appToken == "" {
		return nil, fmt.Errorf("SLACK_APP_TOKEN is not set (Socket Mode needs an app-level token): %w", errNotConfigured)
	}
	if pollWait <= 0 {
		pollWait = 25 * time.Second
	}
	return &slack{botToken: botToken, appToken: appToken, api: slackAPI, timeout: pollWait,
		events: make(chan Incoming, 256), errs: make(chan error, 1)}, nil
}

func (s *slack) Name() string { return "slack" }

func (s *slack) client() *http.Client {
	if s.http != nil {
		return s.http
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *slack) Receive(ctx context.Context) ([]Incoming, error) {
	s.once.Do(func() { go s.socketLoop(ctx) })
	select {
	case m := <-s.events:
		out := []Incoming{m}
		for {
			select {
			case m := <-s.events:
				out = append(out, m)
			default:
				return out, nil
			}
		}
	case err := <-s.errs:
		return nil, err
	case <-time.After(s.timeout):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *slack) socketLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			select {
			case s.errs <- fmt.Errorf("slack socket mode: %w", err):
			default:
			}
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		backoff = sleepBackoff(ctx, backoff)
	}
}

// session runs one Socket Mode connection until Slack ends it. Slack
// rotates these connections routinely ("disconnect" envelopes), so a
// clean end is not an error.
func (s *slack) session(ctx context.Context) error {
	var open struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := s.call(ctx, s.appToken, "apps.connections.open", nil, &open); err != nil {
		return err
	}
	if !open.OK {
		return fmt.Errorf("apps.connections.open: %s (is Socket Mode on, and the app token's scope connections:write?)", open.Error)
	}
	conn, err := wsDial(open.URL, 30*time.Second)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-sctx.Done(); conn.Close() }()

	for {
		raw, err := conn.ReadMessage()
		if err != nil {
			if err == errWSClosed {
				return nil
			}
			return err
		}
		var env struct {
			Type       string `json:"type"`
			EnvelopeID string `json:"envelope_id"`
			Payload    struct {
				Event json.RawMessage `json:"event"`
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		if env.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err := conn.WriteText(ack); err != nil {
				return err
			}
		}
		switch env.Type {
		case "disconnect":
			return nil
		case "events_api":
			s.event(ctx, env.Payload.Event)
		}
	}
}

func (s *slack) event(ctx context.Context, raw json.RawMessage) {
	var e struct {
		Type        string `json:"type"`
		Subtype     string `json:"subtype"`
		User        string `json:"user"`
		BotID       string `json:"bot_id"`
		Text        string `json:"text"`
		Channel     string `json:"channel"`
		ChannelType string `json:"channel_type"`
		TS          string `json:"ts"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Type != "message" {
		return
	}
	s.mu.Lock()
	self := e.User != "" && e.User == s.botID
	s.mu.Unlock()
	// Edits, joins and the like carry a subtype; bots carry a bot_id.
	// Neither is a person talking to it.
	if e.Subtype != "" || e.BotID != "" || self || e.User == "" || strings.TrimSpace(e.Text) == "" {
		return
	}
	group := e.ChannelType != "im"
	text, addressed := e.Text, !group
	if group {
		s.mu.Lock()
		botID := s.botID
		s.mu.Unlock()
		if botID == "" {
			addressed = false
		} else {
			text, addressed = stripMention(e.Text, "<@"+botID+">")
		}
	}
	in := Incoming{Account: e.User, Conversation: e.Channel, Group: group, Addressed: addressed,
		Text: text, MessageID: e.TS, DisplayName: e.User}
	select {
	case s.events <- in:
	case <-ctx.Done():
	}
}

func (s *slack) Send(ctx context.Context, conversation, body string) error {
	if utf8.RuneCountInString(body) > maxSlackBody {
		body = string([]rune(body)[:maxSlackBody])
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := s.call(ctx, s.botToken, "chat.postMessage", map[string]string{"channel": conversation, "text": body}, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack chat.postMessage: %s", out.Error)
	}
	return nil
}

func (s *slack) Identify(ctx context.Context) (string, string, error) {
	var me struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		User   string `json:"user"`
		UserID string `json:"user_id"`
		Team   string `json:"team"`
	}
	if err := s.call(ctx, s.botToken, "auth.test", nil, &me); err != nil {
		return "", "", err
	}
	if !me.OK {
		return "", "", fmt.Errorf("slack auth.test: %s", me.Error)
	}
	s.mu.Lock()
	s.botID = me.UserID
	s.mu.Unlock()
	return me.User, me.User, nil
}

// Advertise does nothing on Slack: its slash commands are configured in
// the app's settings and delivered as a separate envelope type. Typed
// commands still work as text.
func (s *slack) Advertise(context.Context, []Command) error { return nil }

// call is a Slack Web API method, form-encoded, with one of the tokens.
func (s *slack) call(ctx context.Context, token, method string, form map[string]string, out any) error {
	vals := url.Values{}
	for k, v := range form {
		vals.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+"/"+method, strings.NewReader(vals.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack %s: %s %.200s", method, resp.Status, raw)
	}
	return json.Unmarshal(raw, out)
}
