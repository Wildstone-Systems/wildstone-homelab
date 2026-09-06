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
)

type telegram struct {
	token   string
	base    string // overridable for tests; the real API otherwise
	timeout time.Duration
	http    *http.Client

	// offset is Telegram's own cursor: updates are acknowledged by
	// asking for the next one.
	offset int64
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

func (t *telegram) Receive(ctx context.Context) ([]Incoming, error) {
	q := url.Values{
		"timeout":         {strconv.Itoa(int(t.timeout.Seconds()))},
		"allowed_updates": {`["message"]`},
	}
	if t.offset > 0 {
		q.Set("offset", strconv.FormatInt(t.offset, 10))
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
	var out []Incoming
	for _, u := range envelope.Result {
		if u.UpdateID >= t.offset {
			t.offset = u.UpdateID + 1
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
	// below that, and this is the belt to that braces.
	const maxTelegramBody = 4096
	if len(body) > maxTelegramBody {
		body = body[:maxTelegramBody]
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
