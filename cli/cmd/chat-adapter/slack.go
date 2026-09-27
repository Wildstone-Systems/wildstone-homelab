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
	"errors"
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

	// files replaces the file-host check in tests, which serve files
	// from httptest rather than Slack.
	files func(*url.URL) bool
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
		// Files come with subtype file_share: an audio clip recorded in
		// Slack, or an audio file (ADR-W042).
		Files []slackFile `json:"files"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Type != "message" {
		return
	}
	s.mu.Lock()
	self := e.User != "" && e.User == s.botID
	s.mu.Unlock()
	if e.BotID != "" || self || e.User == "" {
		return // bots, and its own replies, are not people talking to it
	}
	var voice *VoiceNote
	switch e.Subtype {
	case "":
		if strings.TrimSpace(e.Text) == "" {
			return
		}
	case "file_share":
		// A shared file is relayed only when it is a recording; a photo
		// or a document is still nothing to relay.
		if voice = s.voiceIn(e.Files); voice == nil {
			return
		}
	default:
		return // edits, joins and the like
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
	if voice != nil {
		// A clip's caption may carry the mention; the recording is the
		// message.
		text = ""
	}
	in := Incoming{Account: e.User, Conversation: e.Channel, Group: group, Addressed: addressed,
		Text: text, MessageID: e.TS, DisplayName: e.User, Voice: voice}
	select {
	case s.events <- in:
	case <-ctx.Done():
	}
}

// slackFile is the part of a shared file the adapter reads to relay a
// recording.
type slackFile struct {
	Subtype    string `json:"subtype"` // slack_audio for a clip recorded in Slack
	Mimetype   string `json:"mimetype"`
	Size       int64  `json:"size"`
	DurationMS int64  `json:"duration_ms"`
	Download   string `json:"url_private_download"`
	Private    string `json:"url_private"`
}

// voiceIn picks the first recording among a message's files: a Slack
// audio clip (which Slack labels video/mp4 although it is sound only)
// or an audio file. A video clip or anything else is not one.
func (s *slack) voiceIn(files []slackFile) *VoiceNote {
	for _, f := range files {
		mt := strings.ToLower(f.Mimetype)
		if f.Subtype != "slack_audio" && !strings.HasPrefix(mt, "audio/") {
			continue
		}
		if mt == "" {
			mt = "audio/mp4"
		}
		link := f.Download
		if link == "" {
			link = f.Private
		}
		if link == "" {
			continue
		}
		return &VoiceNote{
			MIME: mt, Seconds: float64(f.DurationMS) / 1000, Size: f.Size,
			Fetch: func(ctx context.Context, limit int64) ([]byte, error) {
				return s.fetchFile(ctx, link, limit)
			},
		}
	}
	return nil
}

// slackFileHosts are where Slack serves private files. The bot token
// goes with the request, so it is sent to these hosts and no others.
var slackFileHosts = []string{"files.slack.com"}

func (s *slack) fileAllowed(u *url.URL) bool {
	if s.files != nil {
		return s.files(u)
	}
	if u.Scheme != "https" {
		return false
	}
	for _, h := range slackFileHosts {
		if strings.EqualFold(u.Hostname(), h) {
			return true
		}
	}
	return false
}

// errSlackFilesScope is what a download without the files:read scope
// gets: Slack answers 200 with its sign-in page rather than the file.
var errSlackFilesScope = errors.New("slack: the bot cannot download files; add the files:read scope to the Slack app and reinstall it")

// fetchFile downloads a private file with the bot token.
func (s *slack) fetchFile(ctx context.Context, link string, limit int64) ([]byte, error) {
	u, err := url.Parse(link)
	if err != nil || !s.fileAllowed(u) {
		return nil, errors.New("slack: a file URL that is not Slack's file host")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("slack: building the file request")
	}
	req.Header.Set("Authorization", "Bearer "+s.botToken)
	res, err := s.client().Do(req)
	if err != nil {
		return nil, withoutURL("slack file download", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack file download: %s", res.Status)
	}
	if strings.HasPrefix(strings.ToLower(res.Header.Get("Content-Type")), "text/html") {
		return nil, errSlackFilesScope
	}
	if res.ContentLength > limit {
		return nil, errVoiceTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, withoutURL("slack file download", err)
	}
	if int64(len(data)) > limit {
		return nil, errVoiceTooLarge
	}
	return data, nil
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
