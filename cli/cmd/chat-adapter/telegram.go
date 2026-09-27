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
	"errors"
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
	// botID and botUsername are who this bot is, learned from getMe,
	// so a group message can be recognised as addressed to it.
	botID       int64
	botUsername string
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
		// A recording comes as one of these three, with no text
		// (ADR-W042): a voice note (Ogg/Opus), an audio file, or a round
		// video note, whose sound track is what is transcribed. Caption
		// is the text an audio file may carry, which is where a group
		// @mention would be.
		Voice     *telegramFile `json:"voice"`
		Audio     *telegramFile `json:"audio"`
		VideoNote *telegramFile `json:"video_note"`
		Caption   string        `json:"caption"`
		// ReplyTo is the message this one answers, if any: replying to
		// the bot addresses it in a group without an @mention.
		ReplyTo *struct {
			From struct {
				ID int64 `json:"id"`
			} `json:"from"`
		} `json:"reply_to_message"`
		From struct {
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

// telegramFile is the part of a Voice, Audio or VideoNote the adapter
// uses: how to fetch it, and what Telegram says about its size.
type telegramFile struct {
	FileID   string  `json:"file_id"`
	Duration float64 `json:"duration"`
	MimeType string  `json:"mime_type"`
	FileSize int64   `json:"file_size"`
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
			ID        int64  `json:"id"`
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
	t.botID, t.botUsername = envelope.Result.ID, envelope.Result.Username
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
		if u.Message == nil {
			continue
		}
		voice := t.voiceIn(u.Message.Voice, u.Message.Audio, u.Message.VideoNote)
		raw := u.Message.Text
		if voice != nil {
			// A recording has no text; an audio file's caption is the
			// only place a group @mention could be.
			raw = u.Message.Caption
		} else if strings.TrimSpace(raw) == "" {
			continue // a photo, a sticker, a join event: nothing to relay
		}
		group := u.Message.Chat.Type != "private"
		text, addressed := raw, !group
		if group {
			// Until registration has learned who this bot is, nothing in
			// a group is addressed to it: silence is the safe answer.
			// A voice note cannot @mention anyone, so in a group it is
			// addressed only by replying to the bot.
			text, addressed = t.addressedIn(raw)
			if u.Message.ReplyTo != nil && t.botID != 0 && u.Message.ReplyTo.From.ID == t.botID {
				addressed = true
			}
		}
		if voice != nil {
			text = ""
		}
		out = append(out, Incoming{
			Account:      strconv.FormatInt(u.Message.From.ID, 10),
			Conversation: strconv.FormatInt(u.Message.Chat.ID, 10),
			Group:        group,
			Addressed:    addressed,
			Text:         text,
			MessageID:    strconv.FormatInt(u.Message.MessageID, 10),
			DisplayName:  displayName(u.Message.From.Username, u.Message.From.FirstName),
			Voice:        voice,
		})
	}
	return out, nil
}

// voiceIn turns whichever recording a message carries into a VoiceNote,
// or nil. Telegram's own labels are used when it sends them; a voice
// note is always Ogg/Opus and a video note always MP4, so those have a
// sensible default when it does not.
func (t *telegram) voiceIn(voice, audio, videoNote *telegramFile) *VoiceNote {
	var f *telegramFile
	var fallback string
	switch {
	case voice != nil:
		f, fallback = voice, "audio/ogg"
	case videoNote != nil:
		f, fallback = videoNote, "video/mp4"
	case audio != nil:
		f, fallback = audio, "audio/mpeg"
	default:
		return nil
	}
	if f.FileID == "" {
		return nil
	}
	mimeType := f.MimeType
	if mimeType == "" {
		mimeType = fallback
	}
	id := f.FileID
	return &VoiceNote{
		MIME: mimeType, Seconds: f.Duration, Size: f.FileSize,
		Fetch: func(ctx context.Context, limit int64) ([]byte, error) {
			return t.fetchFile(ctx, id, limit)
		},
	}
}

// fetchFile downloads one file by id: getFile names its path, and the
// bytes are at /file/bot<token>/<path>. The Bot API hands out files up
// to 20 MB; limit is smaller, and is enforced on the bytes actually read
// as well as on what Telegram reports.
func (t *telegram) fetchFile(ctx context.Context, fileID string, limit int64) ([]byte, error) {
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			FilePath string `json:"file_path"`
			FileSize int64  `json:"file_size"`
		} `json:"result"`
	}
	if err := t.get(ctx, "getFile?"+url.Values{"file_id": {fileID}}.Encode(), &envelope); err != nil {
		return nil, err
	}
	if !envelope.OK {
		if strings.Contains(strings.ToLower(envelope.Description), "too big") {
			return nil, errVoiceTooLarge
		}
		return nil, fmt.Errorf("telegram getFile: %s", envelope.Description)
	}
	if envelope.Result.FileSize > limit {
		return nil, errVoiceTooLarge
	}
	if envelope.Result.FilePath == "" {
		return nil, fmt.Errorf("telegram getFile returned no path")
	}
	base := t.base
	if base == "" {
		base = "https://api.telegram.org"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/file/bot%s/%s", strings.TrimRight(base, "/"), t.token, envelope.Result.FilePath), nil)
	if err != nil {
		return nil, errors.New("telegram: building the file request")
	}
	res, err := t.client().Do(req)
	if err != nil {
		return nil, withoutURL("telegram file download", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram file download: %s", res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, withoutURL("telegram file download", err)
	}
	if int64(len(data)) > limit {
		return nil, errVoiceTooLarge
	}
	return data, nil
}

// withoutURL drops the request URL from a transport error. Every
// Telegram URL carries the bot token in its path, and net/http quotes
// the URL in the errors it returns — so logging one as-is would write
// the token into the container log.
func withoutURL(what string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %s: %w", what, ue.Op, ue.Err)
	}
	return fmt.Errorf("%s: %w", what, err)
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
	// nil and empty mean different things, and the difference decides
	// whether a stale menu is cleared.
	//
	// nil is "we were not told" — a Keystone too old to send the field,
	// or a response we could not read. Publishing then would wipe a
	// working menu on a transient failure.
	//
	// An empty-but-not-nil slice is Keystone saying this surface offers
	// nothing, and it has to reach the vendor: setMyCommands with an
	// empty list is the only way a command REMOVED from Keystone leaves
	// the menu. Skipping it would leave a bot advertising commands it
	// no longer answers.
	if commands == nil {
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
		return withoutURL("telegram", err)
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

// addressedIn reports whether a group message mentions this bot
// (@username, any case) and returns the text without the mention.
func (t *telegram) addressedIn(text string) (string, bool) {
	if t.botUsername == "" {
		return text, false
	}
	return stripMention(text, "@"+t.botUsername)
}
