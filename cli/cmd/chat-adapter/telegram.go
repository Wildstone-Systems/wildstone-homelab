package main

// The Telegram driver (ADR-W026 G4).
//
// Telegram's Bot API is a polling API: the bot asks for updates and
// Telegram answers. That is the property that makes this shape safe —
// the adapter dials OUT, so the household gains a chat interface with no
// inbound port, no Route, and nothing new at the edge (ADR-W026 A2).
//
// Everything Telegram-specific is in this file. The rest of the program
// knows about Incoming and Transport and nothing else.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type telegram struct {
	token   string
	base    string // overridable for tests; the real API otherwise
	timeout time.Duration
	http    *http.Client

	// offset is Telegram's own cursor: updates are acknowledged by
	// asking for the next one.
	offset int64
	// started is false until the first poll has taken a cursor.
	//
	// Telegram holds undelivered updates for 24 hours and hands the whole
	// backlog to the next getUpdates that carries no offset, so an
	// adapter restarted after an outage would answer a day of questions
	// at once. Keystone's dedupe key would not save it: a queued reply
	// expires, so the same message answered again is a new message. The
	// first poll therefore takes the cursor and relays nothing — the same
	// rule the Matrix driver follows for the same reason.
	started bool
}

func newTelegram(token string, pollWait time.Duration) (Transport, error) {
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is not set: %w", errNotConfigured)
	}
	if pollWait <= 0 {
		pollWait = 25 * time.Second
	}
	return &telegram{token: token, timeout: pollWait}, nil
}

func (t *telegram) Name() string { return "telegram" }

func (t *telegram) client() *http.Client {
	if t.http != nil {
		return t.http
	}
	return &http.Client{Timeout: t.timeout + 30*time.Second}
}

func (t *telegram) endpoint(method string) string {
	base := t.base
	if base == "" {
		base = "https://api.telegram.org"
	}
	return fmt.Sprintf("%s/bot%s/%s", strings.TrimRight(base, "/"), t.token, method)
}

// telegramUpdate is the slice of Telegram's update object this adapter
// uses. Everything else it sends is ignored on purpose: a field this
// program does not read is a field it cannot be confused by.
type telegramUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		From      struct {
			ID        int64  `json:"id"`
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"` // private | group | supergroup | channel
		} `json:"chat"`
	} `json:"message"`
}

// Identify asks Telegram who this bot is. The username is what a
// t.me/<username>?start=<code> invite link is built from, and it is not
// derivable from anything else the deployment holds: the bot token is a
// write-only secret to Keystone.
func (t *telegram) Identify(ctx context.Context) (string, string, error) {
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"result"`
	}
	if err := t.get(ctx, "getMe", &envelope); err != nil {
		return "", "", err
	}
	if !envelope.OK {
		return "", "", fmt.Errorf("telegram getMe: %s", envelope.Description)
	}
	if envelope.Result.Username == "" {
		return "", "", fmt.Errorf("telegram getMe returned no username")
	}
	return envelope.Result.Username, envelope.Result.FirstName, nil
}

func (t *telegram) Receive(ctx context.Context) ([]Incoming, error) {
	q := url.Values{
		"timeout":         {strconv.Itoa(int(t.timeout.Seconds()))},
		"allowed_updates": {`["message"]`},
	}
	switch {
	case t.offset > 0:
		q.Set("offset", strconv.FormatInt(t.offset, 10))
	case !t.started:
		// offset=-1 asks for the LAST update only, which is how a
		// Telegram client says "tell me where the stream is, not what I
		// missed". timeout=0 so startup does not block for a poll
		// interval before the adapter is live.
		q.Set("offset", "-1")
		q.Set("timeout", "0")
	}
	var envelope struct {
		OK          bool             `json:"ok"`
		Description string           `json:"description"`
		Result      []telegramUpdate `json:"result"`
	}
	if err := t.get(ctx, "getUpdates?"+q.Encode(), &envelope); err != nil {
		return nil, err
	}
	if !envelope.OK {
		return nil, fmt.Errorf("telegram getUpdates: %s", envelope.Description)
	}
	// The first poll establishes the cursor and relays nothing: whatever
	// it returned is backlog by definition.
	first := !t.started
	t.started = true

	var out []Incoming
	for _, u := range envelope.Result {
		if u.UpdateID >= t.offset {
			t.offset = u.UpdateID + 1
		}
		if first {
			continue
		}
		if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
			continue // a photo, a sticker, a join event: nothing to relay
		}
		out = append(out, Incoming{
			Account:      strconv.FormatInt(u.Message.From.ID, 10),
			Conversation: strconv.FormatInt(u.Message.Chat.ID, 10),
			Group:        u.Message.Chat.Type != "private",
			Text:         u.Message.Text,
			MessageID:    strconv.FormatInt(u.Message.MessageID, 10),
			DisplayName:  displayName(u.Message.From.Username, u.Message.From.FirstName),
		})
	}
	return out, nil
}

func displayName(username, firstName string) string {
	if username != "" {
		return "@" + username
	}
	return firstName
}

func (t *telegram) Send(ctx context.Context, conversation, body string) error {
	// Telegram's own limit is 4096 characters; the queue bounds the body
	// below that, and this is the belt to that braces. The cut is on a
	// rune boundary — half a rune is invalid UTF-8, which Telegram
	// rejects outright, so a naive slice would turn a long message into
	// no message.
	const maxTelegramBody = 4096
	if len(body) > maxTelegramBody {
		cut := maxTelegramBody
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut]
	}
	payload := url.Values{"chat_id": {conversation}, "text": {body}}
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := t.post(ctx, "sendMessage", payload, &envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return fmt.Errorf("telegram sendMessage: %s", envelope.Description)
	}
	return nil
}

// Advertise publishes the command menu with setMyCommands, which is
// what puts the tappable list beside the message box.
//
// It is called on every registration heartbeat rather than once at
// startup, so a command added to Keystone appears without redeploying
// anything. Telegram replaces the whole list each call, which is the
// behaviour we want: a command REMOVED from Keystone should disappear
// from the menu, and an incremental update could not express that.
//
// The scope is left at Telegram's default, which covers private chats
// and groups alike. A narrower group scope is possible and deliberately
// not used yet: /today already behaves differently in a room, and
// hiding it there would make the room's version undiscoverable rather
// than safer.
//
// One caveat worth knowing: setMyCommands is global per BOT TOKEN. Two
// adapters sharing one token would overwrite each other's menus. Each
// surface has its own bot, so that does not arise here.
func (t *telegram) Advertise(ctx context.Context, commands []Command) error {
	if len(commands) == 0 {
		return nil
	}
	encoded, err := json.Marshal(commands)
	if err != nil {
		return err
	}
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := t.post(ctx, "setMyCommands", url.Values{"commands": {string(encoded)}}, &envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return fmt.Errorf("telegram setMyCommands: %s", envelope.Description)
	}
	return nil
}

func (t *telegram) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint(path), nil)
	if err != nil {
		return err
	}
	return t.do(req, out)
}

func (t *telegram) post(ctx context.Context, method string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint(method),
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return t.do(req, out)
}

func (t *telegram) do(req *http.Request, out any) error {
	res, err := t.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 && len(data) == 0 {
		return fmt.Errorf("telegram: %s", res.Status)
	}
	return json.Unmarshal(data, out)
}
