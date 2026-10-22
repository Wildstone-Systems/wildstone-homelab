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

	// cdn replaces the attachment-host check in tests, which serve
	// attachments from httptest rather than Discord's CDN.
	cdn func(*url.URL) bool
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
			// Flags carries IS_VOICE_MESSAGE; a voice message has one
			// audio attachment and no content (ADR-W042).
			Flags       int                 `json:"flags"`
			Attachments []discordAttachment `json:"attachments"`
			// ReferencedMessage is the message this one replies to: a
			// voice message cannot @mention, so in a server channel a
			// reply to the bot is how it is addressed.
			ReferencedMessage *struct {
				Author struct {
					ID string `json:"id"`
				} `json:"author"`
			} `json:"referenced_message"`
			Mentions []struct {
				ID string `json:"id"`
			} `json:"mentions"`
			Author struct {
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
		botID := d.botID
		d.mu.Unlock()
		self := m.Author.ID == botID
		// Its own replies, and other bots, are not people talking to it.
		if self || m.Author.Bot {
			return
		}
		voice := d.voiceIn(m.Flags, m.Attachments)
		if voice == nil && strings.TrimSpace(m.Content) == "" {
			return // an image, a sticker, an embed: nothing to relay
		}
		name := m.Author.GlobalName
		if name == "" {
			name = m.Author.Username
		}
		group := m.GuildID != ""
		text, addressed := m.Content, !group
		if group && botID != "" {
			mentioned := false
			for _, u := range m.Mentions {
				if u.ID == botID {
					mentioned = true
				}
			}
			for _, tag := range []string{"<@" + botID + ">", "<@!" + botID + ">"} {
				var hit bool
				if text, hit = stripMention(text, tag); hit {
					mentioned = true
				}
			}
			if m.ReferencedMessage != nil && m.ReferencedMessage.Author.ID == botID {
				mentioned = true
			}
			addressed = mentioned
		}
		if voice != nil {
			// The recording is the message; any caption is not relayed.
			text = ""
		}
		in := Incoming{Account: m.Author.ID, Conversation: m.ChannelID, Group: group, Addressed: addressed,
			Text: text, MessageID: m.ID, DisplayName: name, Voice: voice}
		select {
		case d.events <- in:
		case <-ctx.Done():
		}
	}
}

// discordAttachment is the part of a message attachment the adapter
// reads to relay a recording.
type discordAttachment struct {
	URL          string  `json:"url"`
	ContentType  string  `json:"content_type"`
	Size         int64   `json:"size"`
	DurationSecs float64 `json:"duration_secs"`
}

// discordVoiceFlag is IS_VOICE_MESSAGE on a message's flags.
const discordVoiceFlag = 1 << 13

// voiceIn turns a voice message — or a message that is only an audio
// file — into a VoiceNote, or nil. Only the first audio attachment is
// taken; a voice message has exactly one.
func (d *discord) voiceIn(flags int, attachments []discordAttachment) *VoiceNote {
	for _, a := range attachments {
		ct := strings.ToLower(a.ContentType)
		if !strings.HasPrefix(ct, "audio/") && !(flags&discordVoiceFlag != 0 && ct == "") {
			continue
		}
		if ct == "" {
			ct = "audio/ogg" // a voice message is Ogg/Opus
		}
		link := a.URL
		return &VoiceNote{
			MIME: ct, Seconds: a.DurationSecs, Size: a.Size,
			Fetch: func(ctx context.Context, limit int64) ([]byte, error) {
				return d.fetchAttachment(ctx, link, limit)
			},
		}
	}
	return nil
}

// discordCDNHosts are where Discord serves attachments. A URL anywhere
// else is not fetched: the adapter follows links Discord gave it, to
// Discord, and nowhere a crafted message might point.
var discordCDNHosts = []string{"cdn.discordapp.com", "media.discordapp.net"}

func (d *discord) cdnAllowed(u *url.URL) bool {
	if d.cdn != nil {
		return d.cdn(u)
	}
	if u.Scheme != "https" {
		return false
	}
	for _, h := range discordCDNHosts {
		if strings.EqualFold(u.Hostname(), h) {
			return true
		}
	}
	return false
}

// fetchAttachment downloads a recording from Discord's CDN. The URL is
// already signed, so no credential is sent: the bot token never leaves
// for a host that does not need it.
func (d *discord) fetchAttachment(ctx context.Context, link string, limit int64) ([]byte, error) {
	u, err := url.Parse(link)
	if err != nil || !d.cdnAllowed(u) {
		return nil, errors.New("discord: an attachment URL that is not Discord's CDN")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("discord: building the attachment request")
	}
	res, err := d.client().Do(req)
	if err != nil {
		// The signed URL is a credential of sorts; keep it out of logs.
		return nil, withoutURL("discord attachment download", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord attachment download: %s", res.Status)
	}
	if res.ContentLength > limit {
		return nil, errVoiceTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, withoutURL("discord attachment download", err)
	}
	if int64(len(data)) > limit {
		return nil, errVoiceTooLarge
	}
	return data, nil
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
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/Wildstone-Systems/wildstone-homelab, 1)")
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
