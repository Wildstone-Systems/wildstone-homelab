package speech

// Spoken replies (ADR-W042 part two): the Agent's answer read aloud.
//
// The same two engines as transcription, the other way round: Speaches
// serves Kokoro at /v1/audio/speech on the house's own hardware, and
// OpenAI serves the same API from the cloud. The same privacy rule
// decides between them, because the text of an answer is as private as
// the question that produced it: an Agent that keeps what it is told
// in the house has its answers read aloud in the house, or not at all.
//
// Every engine is asked for MP3. Speaches cannot produce Opus, and MP3
// is the one format every place a reply goes can play: Telegram shows
// it as a voice message, Discord, Slack and Matrix play it inline, and
// every browser plays it. Wildstone still never transcodes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Speaker turns text into speech.
type Speaker interface {
	// Processing is where the text goes. The Router refuses a Cloud
	// speaker for a reply that may not leave the house.
	Processing() Processing
	// Speak returns the text read aloud, as MP3.
	Speak(ctx context.Context, text string) ([]byte, error)
}

const (
	// SpokenMIME is the one format a spoken reply comes in.
	SpokenMIME = "audio/mpeg"
	// MaxSpokenChars bounds what is read aloud: about a minute and a
	// half of speech, a few seconds to synthesise on a modern CPU. A
	// longer answer is read up to a sentence end and says the rest is in
	// the message; a reply is a thing to hear while doing something
	// else, not an audiobook.
	MaxSpokenChars = 1200
	// maxSpokenBytes bounds one engine response. 1200 characters of MP3
	// speech is well under 2 MB.
	maxSpokenBytes = 8 << 20
	// DefaultReplyTimeout bounds one synthesis.
	DefaultReplyTimeout = 2 * time.Minute
)

// Spoken is one reply read aloud, and where it was made.
type Spoken struct {
	Audio      []byte
	MIME       string
	Processing Processing
}

// Speak implements Speaker for a Client whose Model is a text-to-speech
// model and whose Voice names one of its voices.
func (c *Client) Speak(ctx context.Context, text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return nil, ErrEmpty
	}
	if c.Key != nil && c.Key() == "" {
		return nil, ErrUnavailable
	}
	body, err := json.Marshal(map[string]any{
		"model": c.Model, "input": text, "voice": c.Voice, "response_format": "mp3",
	})
	if err != nil {
		return nil, fmt.Errorf("speech: build request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/audio/speech"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("speech: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("speech: %s voice engine unreachable: %w", c.Where, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && c.DownloadsModels {
		c.startWarming()
		return nil, ErrWarming
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("speech: %s voice engine answered %d: %s", c.Where, resp.StatusCode, engineMessage(resp.Body))
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, maxSpokenBytes+1))
	if err != nil {
		return nil, fmt.Errorf("speech: %s voice engine: reading audio: %w", c.Where, err)
	}
	if len(audio) > maxSpokenBytes {
		return nil, ErrTooLarge
	}
	if len(audio) == 0 {
		return nil, ErrEmpty
	}
	return audio, nil
}

// CanSpeak reports whether a reply with this permission could be read
// aloud. A caller uses it to decide whether to offer a speaker button,
// and whether to ask for a voice reply at all.
func (r *Router) CanSpeak(allowCloud bool) bool {
	return r != nil && r.ReplyMode != ModeOff && len(r.voices(allowCloud)) > 0
}

// SpeaksAtAll reports whether spoken replies are on and any engine is
// configured, whatever a given Agent may use.
func (r *Router) SpeaksAtAll() bool { return r.CanSpeak(true) }

func (r *Router) voices(allowCloud bool) []Speaker {
	var out []Speaker
	add := func(s Speaker) {
		if s == nil || (s.Processing() != Local && !allowCloud) {
			return
		}
		out = append(out, s)
	}
	if r.ReplyMode.usesLocal() {
		add(r.LocalVoice)
	}
	if r.ReplyMode.usesCloud() {
		add(r.CloudVoice)
	}
	return out
}

// Speak reads text aloud. allowCloud is the reply's privacy answer,
// decided by the caller exactly as for Transcribe; a reply that may not
// leave the house and has no local voice gets ErrUnavailable, never the
// cloud (AC12). The text is made speakable first (SpeakableText).
func (r *Router) Speak(ctx context.Context, allowCloud bool, text string) (Spoken, error) {
	if r == nil || r.ReplyMode == ModeOff {
		return Spoken{}, ErrOff
	}
	text = SpeakableText(text)
	if text == "" {
		return Spoken{}, ErrEmpty
	}
	voices := r.voices(allowCloud)
	if len(voices) == 0 {
		return Spoken{}, ErrUnavailable
	}
	release, err := r.take(ctx)
	if err != nil {
		return Spoken{}, err
	}
	defer release()

	timeout := r.ReplyTimeout
	if timeout <= 0 {
		timeout = DefaultReplyTimeout
	}
	var last error
	for _, v := range voices {
		vctx, cancel := context.WithTimeout(ctx, timeout)
		audio, err := v.Speak(vctx, text)
		cancel()
		if err == nil {
			return Spoken{Audio: audio, MIME: SpokenMIME, Processing: v.Processing()}, nil
		}
		if ctx.Err() != nil {
			return Spoken{}, ctx.Err()
		}
		last = err
	}
	return Spoken{}, last
}

var (
	fencedCode = regexp.MustCompile("(?s)```.*?```")
	inlineCode = regexp.MustCompile("`([^`\n]*)`")
	mdImage    = regexp.MustCompile(`!\[([^\]]*)\]\([^)\s]*\)`)
	mdLink     = regexp.MustCompile(`\[([^\]]+)\]\((?:https?://|mailto:|/)[^)\s]*\)`)
	bareURL    = regexp.MustCompile(`\bhttps?://[^\s)>\]]+`)
	lineMarker = regexp.MustCompile(`^(?:#{1,6}\s+|>\s*|[-*+•]\s+|\d{1,3}[.)]\s+)`)
	emphasis   = regexp.MustCompile(`\*\*|__|~~`)
	spaces     = regexp.MustCompile(`\s+`)
)

// SpeakableText turns an answer written to be read into one that can be
// heard: code blocks and links are named rather than spelled out, list
// and heading marks are dropped, each line ends as a sentence so a list
// is not read as one breath, and anything past MaxSpokenChars is cut at
// a sentence end with a pointer to the message.
func SpeakableText(s string) string {
	s = fencedCode.ReplaceAllString(s, "\n(There's code in the message.)\n")
	s = mdImage.ReplaceAllString(s, "$1")
	s = mdLink.ReplaceAllString(s, "$1")
	s = bareURL.ReplaceAllString(s, "a link")
	s = inlineCode.ReplaceAllString(s, "$1")
	s = emphasis.ReplaceAllString(s, "")

	var parts []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(lineMarker.ReplaceAllString(strings.TrimSpace(line), ""))
		if line == "" || strings.Trim(line, "-=*_|: ") == "" {
			continue // blank lines, rules and table borders
		}
		if strings.Contains(line, "|") {
			// A table row: its cells, as a list.
			var cells []string
			for _, c := range strings.Split(strings.Trim(line, "| "), "|") {
				if c = strings.TrimSpace(c); c != "" {
					cells = append(cells, c)
				}
			}
			line = strings.Join(cells, ", ")
		}
		if r, _ := utf8.DecodeLastRuneInString(line); !strings.ContainsRune(".!?:;,…)\"'”’", r) {
			line += "."
		}
		parts = append(parts, line)
	}
	s = strings.TrimSpace(spaces.ReplaceAllString(strings.Join(parts, " "), " "))
	return clipSpoken(s)
}

// clipSpoken cuts text longer than MaxSpokenChars at the last sentence
// end before the limit, or at a word when there is none.
func clipSpoken(s string) string {
	if utf8.RuneCountInString(s) <= MaxSpokenChars {
		return s
	}
	const rest = " The rest is in the message."
	runes := []rune(s)
	cut := string(runes[:MaxSpokenChars-utf8.RuneCountInString(rest)])
	if i := strings.LastIndexAny(cut, ".!?"); i > len(cut)/2 {
		cut = cut[:i+1]
	} else if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i] + "…"
	}
	return cut + rest
}
