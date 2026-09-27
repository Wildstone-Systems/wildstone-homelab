package api

// Voice messages from the chat adapters (ADR-W042 slice 2).
//
// The adapter downloads the recording — it holds the vendor token and
// Keystone does not — and posts it here with the same fields a typed
// message carries. From then on it IS a typed message, handled by
// chatInbound, with three differences:
//
//   - it is transcribed only after the sender is known, the room check
//     has run and the message is claimed, so a stranger's audio is
//     dropped unheard and a redelivered clip is heard once;
//   - the engine is chosen by the Agent's privacy class: audio for an
//     Agent that runs on a local model never leaves the house;
//   - the words cannot open secret intake, answer a rule, or run a
//     slash command, because a recogniser mis-hears.
//
// The audio is read into memory, handed to the engine, and dropped. It
// is never written to disk (the multipart body is read part by part,
// not with ParseMultipartForm, which spills large files to a temp
// file), to a log, or to an event.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

// voiceClip is one recording on its way to an engine.
type voiceClip struct {
	Audio   []byte
	MIME    string
	Seconds float64
	// TooLarge says the adapter did not download the recording because
	// the vendor reported it over the size or length an adapter will
	// fetch. It still comes here, so the person — once the sender and
	// room checks have passed — is told to send something shorter,
	// rather than hearing nothing at all.
	TooLarge bool
}

// digest stands in for a voice message's text in its dedupe key, so an
// adapter that sends no message id still has a redelivered clip
// collapse onto the first.
func (v *voiceClip) digest() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%g\x00%t\x00%s", v.MIME, v.Seconds, v.TooLarge, v.Audio))
	return "voice:" + hex.EncodeToString(sum[:16])
}

// voiceMessage is the "message" part of a voice upload: a typed
// message's fields, with the duration the vendor reported instead of
// text.
type voiceMessage struct {
	inboundMessage
	AudioSeconds float64 `json:"audioSeconds,omitempty"`
	// TooLarge: the adapter left the recording on the vendor's servers
	// because it was reported too big to fetch; the audio part is then
	// empty (voiceClip.TooLarge).
	TooLarge bool `json:"tooLarge,omitempty"`
}

// maxVoiceMetaBytes bounds the "message" part, which is a handful of
// short identifiers.
const maxVoiceMetaBytes = 16 << 10

// receiveChatVoice is POST /api/v1/chat/voice: multipart/form-data with
// a "message" part (JSON, a voiceMessage) and an "audio" part (the
// recording, with its content type).
func (s *Server) receiveChatVoice(r *http.Request, id principal.Identity) (any, int, error) {
	msg, clip, st, err := readVoiceUpload(r)
	if err != nil {
		return nil, st, err
	}
	return s.chatInbound(r, id, msg, "", clip)
}

// readVoiceUpload reads the two parts, in either order, holding the
// audio in memory and refusing anything over speech.MaxAudioBytes
// before it is all read.
func readVoiceUpload(r *http.Request) (inboundMessage, *voiceClip, int, error) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		return inboundMessage{}, nil, http.StatusUnsupportedMediaType,
			fmt.Errorf("a voice message is multipart/form-data with a message part and an audio part")
	}
	// Two parts and their headers; anything past this is not a voice
	// message.
	r.Body = http.MaxBytesReader(nil, r.Body, speech.MaxAudioBytes+maxVoiceMetaBytes+(64<<10))
	mr, err := r.MultipartReader()
	if err != nil {
		return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("reading voice message: %w", err)
	}

	var (
		meta     *voiceMessage
		clip     *voiceClip
		tooLarge = func(err error) bool {
			var mbe *http.MaxBytesError
			return errors.As(err, &mbe)
		}
	)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if tooLarge(err) {
				return inboundMessage{}, nil, http.StatusRequestEntityTooLarge, speech.ErrTooLarge
			}
			return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("reading voice message: %w", err)
		}
		switch part.FormName() {
		case "message":
			if meta != nil {
				return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("more than one message part")
			}
			raw, err := io.ReadAll(io.LimitReader(part, maxVoiceMetaBytes+1))
			if err != nil || len(raw) > maxVoiceMetaBytes {
				return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("the message part is missing or too large")
			}
			meta = &voiceMessage{}
			if err := json.Unmarshal(raw, meta); err != nil {
				return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("parsing message part: %w", err)
			}
		case "audio":
			if clip != nil {
				return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("more than one audio part")
			}
			audio, err := io.ReadAll(io.LimitReader(part, speech.MaxAudioBytes+1))
			if err != nil {
				if tooLarge(err) {
					return inboundMessage{}, nil, http.StatusRequestEntityTooLarge, speech.ErrTooLarge
				}
				return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("reading audio part: %w", err)
			}
			if len(audio) > speech.MaxAudioBytes {
				return inboundMessage{}, nil, http.StatusRequestEntityTooLarge, speech.ErrTooLarge
			}
			clip = &voiceClip{Audio: audio, MIME: part.Header.Get("Content-Type")}
		default:
			// An unknown part is a client that means something this
			// server does not; refusing says so, where skipping it would
			// not.
			return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("unknown part %q", part.FormName())
		}
		part.Close()
	}
	if meta == nil || clip == nil {
		return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("a voice message needs a message part and an audio part")
	}
	if strings.TrimSpace(meta.Text) != "" {
		return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("a voice message carries no text; send text to /api/v1/chat/messages")
	}
	if meta.AudioSeconds < 0 {
		return inboundMessage{}, nil, http.StatusBadRequest, fmt.Errorf("audioSeconds cannot be negative")
	}
	clip.Seconds, clip.TooLarge = meta.AudioSeconds, meta.TooLarge
	if clip.TooLarge {
		// Nothing to transcribe, and nothing that should have been sent.
		clip.Audio = nil
	}
	return meta.inboundMessage, clip, http.StatusOK, nil
}

// transcribeChatVoice turns the clip into words, or into the reply that
// tells the person why not. Exactly one of heard and reply is set;
// reason is for the adapter's response.
//
// Every outcome is recorded as one chat.voice event naming the account,
// the Agent, the engine and the outcome — never the words or the audio.
func (s *Server) transcribeChatVoice(ctx context.Context, at runtimedb.Connection, account string, clip *voiceClip) (heard, reply, reason string) {
	var (
		res speech.Result
		err error
	)
	if clip.TooLarge {
		err = speech.ErrTooLarge
	} else {
		res, err = s.Speech.Transcribe(ctx, s.voiceMayLeaveHouse(at), clip.Audio, clip.MIME, clip.Seconds)
	}

	outcome := "transcribed"
	if err != nil {
		outcome = voiceOutcome(err)
	}
	attrs := map[string]string{
		"account": account, "agent": at.Butler, "outcome": outcome,
		"seconds": strconv.FormatFloat(clip.Seconds, 'f', 1, 64),
	}
	if res.Processing != "" {
		attrs["engine"] = string(res.Processing)
	}
	s.recordEvent("chat.voice", at.ExternalID, "a voice message: "+outcome, attrs)

	if err != nil {
		if outcome == "failed" {
			// The engine's own error, which names the engine and a
			// status and never the audio or a key (speech.Client).
			s.Log.Error("transcribing a voice message", "agent", at.Butler, "err", err)
		}
		return "", voiceReply(err, s.Speech), "voice message not transcribed: " + outcome
	}
	heard = res.Text
	if len(heard) > maxAskBytes {
		// A two-minute recording is a few kilobytes of words; this is
		// the typed path's bound, applied without splitting a character.
		heard = strings.ToValidUTF8(heard[:maxAskBytes], "")
	}
	return heard, "", ""
}

// voiceMayLeaveHouse answers ADR-W042 §2.2 step 4 for the Agent this
// message arrived at: may its audio go to a cloud engine?
//
// Only when the Agent itself already sends what it is told out of the
// house: a named Agent assigned a cloud model, and — for an external
// runtime — declared cloud by the Owner. An Agent on a local model, an
// external runtime whose class is local-only or undeclared, and the
// legacy single-provider Butler all answer no. So does any error: the
// safe answer to "can I not tell?" is local.
func (s *Server) voiceMayLeaveHouse(at runtimedb.Connection) bool {
	if at.Butler == "" || !s.butlersConfigured() {
		return false
	}
	return s.agentSendsOutOfHouse(at.Butler)
}

// agentSendsOutOfHouse is voiceMayLeaveHouse's answer for one named
// Agent, read from its assigned model and its runtime record.
func (s *Server) agentSendsOutOfHouse(name string) bool {
	b, err := s.DB.GetButler(name)
	if err != nil || b.ModelKind != runtimedb.ModelCloud {
		return false
	}
	rt, err := s.DB.GetButlerRuntime(b.Seq)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		return true // native by default, and its model is cloud
	case err != nil:
		return false
	case rt.Type == agentapi.RuntimeNative:
		return true
	default:
		return rt.ProcessingClass == agentapi.ProcessingCloud
	}
}

// voiceOutcome names an error for the event record.
func voiceOutcome(err error) string {
	switch {
	case errors.Is(err, speech.ErrOff):
		return "off"
	case errors.Is(err, speech.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, speech.ErrTooLarge):
		return "too_long"
	case errors.Is(err, speech.ErrUnsupported):
		return "unsupported"
	case errors.Is(err, speech.ErrEmpty):
		return "empty"
	case errors.Is(err, speech.ErrWarming):
		return "warming"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "failed"
}

// voiceReply is what the person is told when their voice message was
// not heard. Each one ends by saying what to do instead.
func voiceReply(err error, r *speech.Router) string {
	switch {
	case errors.Is(err, speech.ErrOff):
		return "Voice messages aren't turned on here yet. Please type your message."
	case errors.Is(err, speech.ErrUnavailable):
		return "I can't listen to voice messages in this conversation right now. Please type your message."
	case errors.Is(err, speech.ErrTooLarge):
		max := speech.DefaultMaxSeconds
		if r != nil && r.MaxSeconds > 0 {
			max = r.MaxSeconds
		}
		return fmt.Sprintf("That voice message is too long for me. Please keep it under %s, or type it.", secondsPhrase(max))
	case errors.Is(err, speech.ErrUnsupported):
		return "I couldn't open that recording. Please try again, or type your message."
	case errors.Is(err, speech.ErrEmpty):
		return "I couldn't make out any words in that recording. Please try again, or type it."
	case errors.Is(err, speech.ErrWarming):
		return "I'm still getting ready to understand voice messages. Please try again in a few minutes, or type your message."
	}
	return "I couldn't listen to that just now. Please try again in a moment, or type your message."
}

func secondsPhrase(n int) string {
	if n%60 == 0 {
		if n == 60 {
			return "1 minute"
		}
		return strconv.Itoa(n/60) + " minutes"
	}
	return strconv.Itoa(n) + " seconds"
}

// spokenSecretReply answers a voice message that sounded like a
// password or a /secret command.
const spokenSecretReply = "That sounded like a password or other secret. " +
	"The recording has already been sent to this messaging platform, so I haven't stored it and won't repeat it. " +
	secretEntryNote

// heardLine is the first line of a reply to a voice message: what was
// heard, clipped, so a mis-hearing shows beside the answer to it.
func heardLine(text string) string {
	const max = 300
	if redact.SecretSubmission(text) {
		// Belt and braces: the turn path stops a spoken secret before
		// any reply is written, and this line never repeats one either.
		return "🎙️ I heard your message.\n\n"
	}
	t := strings.Join(strings.Fields(text), " ")
	if r := []rune(t); len(r) > max {
		t = string(r[:max]) + "…"
	}
	return "🎙️ I heard: “" + t + "”\n\n"
}
