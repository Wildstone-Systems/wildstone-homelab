package main

// Voice replies (ADR-W042 part two): when a person spoke, Keystone may
// mark the reply to be read aloud too. The adapter delivers the text as
// always, then asks Keystone for the audio — Keystone makes it, and
// chooses the engine by the Agent's privacy — and sends it as a voice
// message. The adapter only carries bytes: it never talks to a speech
// engine and never decides where words may go.
//
// The audio is MP3 from every engine, and every vendor here plays it:
// Telegram shows it as a voice message, Discord, Slack and Matrix play
// it inline. Nothing is transcoded, and nothing is written to disk.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// VoiceSender is a Transport that can send a voice message. Every
// shipped transport is one; a transport that is not simply answers in
// text.
type VoiceSender interface {
	// SendVoice delivers one recording to a conversation. mimeType is
	// always audio/mpeg today.
	SendVoice(ctx context.Context, conversation string, audio []byte, mimeType string) error
}

const (
	// maxReplyAudio bounds the audio the adapter accepts from Keystone:
	// a spoken reply is capped at about a minute and a half of speech,
	// well under 2 MB of MP3.
	maxReplyAudio = 8 << 20
	// voiceReplyTimeout bounds fetching and sending one voice reply. A
	// CPU engine takes seconds; the text has already arrived, so a voice
	// that takes longer than this is dropped rather than waited for.
	voiceReplyTimeout = 3 * time.Minute
	replyFileName     = "reply.mp3"
)

// errNoSpeech means Keystone has nothing to read aloud for a message:
// the reply is text only, or its voice was already taken.
var errNoSpeech = errors.New("no spoken reply for this message")

// speech asks Keystone for one delivered reply read aloud.
func (k *keystoneClient) speech(ctx context.Context, transport string, seq int64) ([]byte, string, error) {
	path := fmt.Sprintf("/api/v1/chat/outbound/%d/speech?transport=%s", seq, url.QueryEscape(transport))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(k.Base, "/")+path, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+k.Token)
	res, err := k.client().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, "", errNoSpeech
	}
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return nil, "", fmt.Errorf("POST %s: %s: %s", path, res.Status, strings.TrimSpace(string(msg)))
	}
	audio, err := io.ReadAll(io.LimitReader(res.Body, maxReplyAudio+1))
	if err != nil {
		return nil, "", err
	}
	if len(audio) == 0 || len(audio) > maxReplyAudio {
		return nil, "", fmt.Errorf("spoken reply is empty or too large (%d bytes)", len(audio))
	}
	mt := res.Header.Get("Content-Type")
	if mt != "audio/mpeg" {
		return nil, "", fmt.Errorf("spoken reply is %q, not audio/mpeg", mt)
	}
	return audio, mt, nil
}

// voiceQueue bounds the voice replies waiting for the worker. A
// household sends a handful at a time; past this the engine is clearly
// stuck, and a reply whose text has arrived loses only its voice.
const voiceQueue = 16

// voiceJob is one delivered reply whose voice is still to be sent.
type voiceJob struct {
	seq          int64
	conversation string
}

// queueVoice hands a voice reply to the worker without waiting. When
// the queue is full the voice is dropped, never the delivery loop held.
func (a *adapter) queueVoice(q chan<- voiceJob, j voiceJob) {
	select {
	case q <- j:
	default:
		a.log.Warn("voice reply dropped: too many waiting; the text was sent", "seq", j.seq)
	}
}

// voiceWorker sends voice replies one at a time, in order, apart from
// the text delivery loop.
func (a *adapter) voiceWorker(ctx context.Context, q <-chan voiceJob) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-q:
			a.deliverVoice(ctx, j.seq, j.conversation)
		}
	}
}

// deliverVoice sends a delivered reply's voice message, when Keystone
// has one. Any failure is logged and dropped: the person already has
// the answer in text.
func (a *adapter) deliverVoice(ctx context.Context, seq int64, conversation string) {
	vs, ok := a.transport.(VoiceSender)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, voiceReplyTimeout)
	defer cancel()
	audio, mt, err := a.keystone.speech(ctx, a.transport.Name(), seq)
	if errors.Is(err, errNoSpeech) {
		return
	}
	if err != nil {
		a.log.Warn("voice reply not made; the text was sent", "seq", seq, "err", err)
		return
	}
	if err := vs.SendVoice(ctx, conversation, audio, mt); err != nil {
		a.log.Warn("voice reply not sent; the text was", "seq", seq, "err", err)
	}
}

// multipartFile builds a multipart body with the given fields and one
// file part.
func multipartFile(fields [][2]string, fileField, fileName, mimeType string, data []byte) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, "", err
		}
	}
	part, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name=%q; filename=%q`, fileField, fileName)},
		"Content-Type":        {mimeType},
	})
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

// SendVoice for Telegram is sendVoice, which shows MP3 as a voice
// message with a play button.
func (t *telegram) SendVoice(ctx context.Context, conversation string, audio []byte, mimeType string) error {
	body, ct, err := multipartFile([][2]string{{"chat_id", conversation}}, "voice", replyFileName, mimeType, audio)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint("sendVoice"), body)
	if err != nil {
		return withoutURL("telegram", err)
	}
	req.Header.Set("Content-Type", ct)
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := t.do(req, &envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return fmt.Errorf("telegram sendVoice: %s", envelope.Description)
	}
	return nil
}

// SendVoice for Discord posts the MP3 as an attachment, which Discord
// plays inline.
func (d *discord) SendVoice(ctx context.Context, conversation string, audio []byte, mimeType string) error {
	payload, _ := json.Marshal(map[string]any{
		"attachments": []map[string]any{{"id": 0, "filename": replyFileName, "description": "The reply, read aloud"}},
	})
	body, ct, err := multipartFile([][2]string{{"payload_json", string(payload)}}, "files[0]", replyFileName, mimeType, audio)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.api+"/channels/"+url.PathEscape(conversation)+"/messages", body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+d.token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/Wildstone-Systems/wildstone-homelab, 1)")
	req.Header.Set("Content-Type", ct)
	resp, err := d.client().Do(req)
	if err != nil {
		return withoutURL("discord", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("discord voice reply: %s %.200s", resp.Status, raw)
	}
	return nil
}

// SendVoice for Slack is the external upload flow: ask for an upload
// URL, send the bytes there, then share the file into the channel. It
// needs the files:write scope; without it Slack answers missing_scope,
// which is reported as such.
func (s *slack) SendVoice(ctx context.Context, conversation string, audio []byte, mimeType string) error {
	var slot struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := s.call(ctx, s.botToken, "files.getUploadURLExternal", map[string]string{
		"filename": replyFileName, "length": strconv.Itoa(len(audio)),
	}, &slot); err != nil {
		return err
	}
	if !slot.OK {
		return slackScopeError("files.getUploadURLExternal", slot.Error)
	}
	// The upload URL is Slack's own and pre-signed; the bot token is not
	// sent to it, and a URL anywhere but Slack's file host is not
	// followed.
	if u, err := url.Parse(slot.UploadURL); err != nil || !s.fileAllowed(u) {
		return fmt.Errorf("slack gave an upload address that is not files.slack.com")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slot.UploadURL, bytes.NewReader(audio))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mimeType)
	resp, err := s.client().Do(req)
	if err != nil {
		return withoutURL("slack upload", err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack upload: %s", resp.Status)
	}
	files, _ := json.Marshal([]map[string]string{{"id": slot.FileID, "title": "Reply, read aloud"}})
	var done struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := s.call(ctx, s.botToken, "files.completeUploadExternal", map[string]string{
		"files": string(files), "channel_id": conversation,
	}, &done); err != nil {
		return err
	}
	if !done.OK {
		return slackScopeError("files.completeUploadExternal", done.Error)
	}
	return nil
}

func slackScopeError(method, code string) error {
	if code == "missing_scope" {
		return fmt.Errorf("slack %s: missing_scope — add files:write to the Slack app and reinstall it to send voice replies", method)
	}
	return fmt.Errorf("slack %s: %s", method, code)
}

// SendVoice for Matrix uploads the MP3 to the bot's homeserver and
// sends it as an m.audio event, marked as a voice message for clients
// that show one.
func (m *matrix) SendVoice(ctx context.Context, conversation string, audio []byte, mimeType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.homeserver+"/_matrix/media/v3/upload?filename="+url.QueryEscape(replyFileName), bytes.NewReader(audio))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", mimeType)
	res, err := m.client().Do(req)
	if err != nil {
		return withoutURL("matrix media upload", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("matrix media upload: %s %.200s", res.Status, raw)
	}
	var up struct {
		ContentURI string `json:"content_uri"`
	}
	if err := json.Unmarshal(raw, &up); err != nil || !strings.HasPrefix(up.ContentURI, "mxc://") {
		return fmt.Errorf("matrix media upload: no content_uri in the answer")
	}
	txn := m.txn.Add(1)
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%d-%d",
		url.PathEscape(conversation), time.Now().UnixNano(), txn)
	payload := map[string]any{
		"msgtype": "m.audio",
		"body":    "Reply, read aloud",
		"url":     up.ContentURI,
		"info":    map[string]any{"mimetype": mimeType, "size": len(audio)},
		// Element and others show an m.audio carrying this as a voice
		// message rather than a file.
		"org.matrix.msc3245.voice": map[string]any{},
	}
	var out struct {
		EventID string `json:"event_id"`
	}
	return m.call(ctx, http.MethodPut, path, payload, &out)
}
