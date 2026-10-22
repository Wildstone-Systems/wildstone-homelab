package api

// Dictation on the household dashboard (ADR-W042 slice 4).
//
// The mic button records in the browser and posts the clip here; the
// words come back to the page and go INTO THE TEXT BOX. Nothing is sent
// to an assistant from this endpoint: the person reads what was heard,
// fixes it, and presses Send, which is the ordinary /assistant/ask with
// all of its checks. That correction step is the one thing a dashboard
// can offer that a chat app cannot, and it keeps this door narrow — it
// turns sound into text for the person who made the sound, and does
// nothing else.
//
// The same ceilings as asking: the person comes from the forwarded
// identity (s.portal), the assistant must be one they may reach from
// here (portalMayReach), and the engine follows that assistant's
// privacy class, so dictating to an assistant that stays in the house
// never sends the recording out of it.

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/speech"
)

// dictationFieldBytes bounds the small text fields beside the audio.
const dictationFieldBytes = 256

// assistantTranscribe is POST /api/v1/assistant/transcribe:
// multipart/form-data with fields "agent" and (optionally) "seconds",
// and a file part "audio". It answers {"text": "..."}.
func (s *Server) assistantTranscribe(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	agent, clip, status, err := readDictation(r)
	if err != nil {
		return nil, status, err
	}

	// The ceiling, exactly as /assistant/ask applies it, so the mic
	// cannot confirm an assistant the text box would not.
	b, status, err := s.portalVoiceButler(agent, member)
	if err != nil {
		return nil, status, err
	}

	res, terr := s.Speech.Transcribe(r.Context(), s.dictationMayLeaveHouse(b.Name), clip.Audio, clip.MIME, clip.Seconds)
	outcome := "transcribed"
	if terr != nil {
		outcome = voiceOutcome(terr)
	}
	attrs := map[string]string{
		"member": member, "agent": b.Name, "outcome": outcome,
		"seconds": strconv.FormatFloat(clip.Seconds, 'f', 1, 64),
	}
	if res.Processing != "" {
		attrs["engine"] = string(res.Processing)
	}
	s.recordEvent("assistant.dictation", b.Name, "dictation on the dashboard: "+outcome, attrs)

	if terr != nil {
		if outcome == "failed" {
			s.Log.Error("transcribing dictation", "agent", b.Name, "err", terr)
		}
		return nil, dictationStatus(terr), errors.New(dictationMessage(terr, s.Speech))
	}
	return map[string]any{"text": res.Text, "engine": string(res.Processing)}, http.StatusOK, nil
}

// dictationMayLeaveHouse is voiceMayLeaveHouse for a named assistant:
// the recording may go to a cloud engine only when the assistant itself
// already sends what it is told out of the house.
func (s *Server) dictationMayLeaveHouse(agent string) bool {
	return s.butlersConfigured() && s.agentSendsOutOfHouse(agent)
}

// canDictate tells the dashboard whether to show the mic for an
// assistant: voice is on, and an engine this assistant may use exists.
func (s *Server) canDictate(agent string) bool {
	return s.Speech.Available(s.dictationMayLeaveHouse(agent))
}

// readDictation reads the upload part by part, holding the audio in
// memory — ParseMultipartForm would spill a large file to disk, and a
// recording is never written anywhere.
func readDictation(r *http.Request) (string, *voiceClip, int, error) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		return "", nil, http.StatusUnsupportedMediaType, fmt.Errorf("dictation is sent as multipart/form-data")
	}
	r.Body = http.MaxBytesReader(nil, r.Body, speech.MaxAudioBytes+(64<<10))
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, http.StatusBadRequest, fmt.Errorf("reading dictation: %w", err)
	}
	var (
		agent string
		clip  *voiceClip
		secs  float64
	)
	tooLarge := func(err error) bool {
		var mbe *http.MaxBytesError
		return errors.As(err, &mbe)
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if tooLarge(err) {
				return "", nil, http.StatusRequestEntityTooLarge, errors.New(dictationMessage(speech.ErrTooLarge, nil))
			}
			return "", nil, http.StatusBadRequest, fmt.Errorf("reading dictation: %w", err)
		}
		switch part.FormName() {
		case "agent", "seconds":
			raw, err := io.ReadAll(io.LimitReader(part, dictationFieldBytes+1))
			if err != nil || len(raw) > dictationFieldBytes {
				return "", nil, http.StatusBadRequest, fmt.Errorf("the %s field is too long", part.FormName())
			}
			v := strings.TrimSpace(string(raw))
			if part.FormName() == "agent" {
				agent = v
			} else if v != "" {
				secs, err = strconv.ParseFloat(v, 64)
				if err != nil || secs < 0 {
					return "", nil, http.StatusBadRequest, fmt.Errorf("seconds must be a number of seconds")
				}
			}
		case "audio":
			if clip != nil {
				return "", nil, http.StatusBadRequest, fmt.Errorf("more than one recording")
			}
			audio, err := io.ReadAll(io.LimitReader(part, speech.MaxAudioBytes+1))
			if err != nil || len(audio) > speech.MaxAudioBytes {
				if err == nil || tooLarge(err) {
					return "", nil, http.StatusRequestEntityTooLarge, errors.New(dictationMessage(speech.ErrTooLarge, nil))
				}
				return "", nil, http.StatusBadRequest, fmt.Errorf("reading the recording: %w", err)
			}
			clip = &voiceClip{Audio: audio, MIME: part.Header.Get("Content-Type")}
		default:
			return "", nil, http.StatusBadRequest, fmt.Errorf("unknown field %q", part.FormName())
		}
		part.Close()
	}
	if agent == "" {
		return "", nil, http.StatusBadRequest, fmt.Errorf("name an assistant")
	}
	if clip == nil {
		return "", nil, http.StatusBadRequest, fmt.Errorf("no recording was sent")
	}
	clip.Seconds = secs
	return agent, clip, http.StatusOK, nil
}

// dictationStatus maps a transcription error onto an HTTP status the
// page can act on.
func dictationStatus(err error) int {
	switch {
	case errors.Is(err, speech.ErrOff), errors.Is(err, speech.ErrUnavailable), errors.Is(err, speech.ErrWarming):
		return http.StatusServiceUnavailable
	case errors.Is(err, speech.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, speech.ErrUnsupported):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, speech.ErrEmpty):
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadGateway
}

// dictationMessage is what the page shows under the text box. The chat
// wording says "type your message"; here the text box is right there,
// so it says what happened and that typing still works.
func dictationMessage(err error, r *speech.Router) string {
	switch {
	case errors.Is(err, speech.ErrOff):
		return "Dictation isn't turned on here yet. You can still type."
	case errors.Is(err, speech.ErrUnavailable):
		return "Dictation isn't available for this assistant. You can still type."
	case errors.Is(err, speech.ErrWarming):
		return "Dictation is still getting ready. Try again in a few minutes, or type."
	}
	return voiceReply(err, r)
}

// transcribeForButler is POST /api/v1/butlers/{name}/transcribe, the
// Console's dictation (ADR-W042). The body IS the recording, typed by
// its Content-Type — the same shape as the Console's wallpaper upload,
// so the Console keeps its one fetch path. ?seconds= carries the length
// the browser measured. It answers {"text"} and asks the Butler
// nothing: the words go into the Console's text box to be checked.
//
// Access is the ask path's, exactly: the Butler is resolved by
// butlerlive's own Resolve for the person the caller acts for, owner
// or not, so the mic reaches precisely the Butlers the text box does.
// The engine follows that Butler's privacy class.
func (s *Server) transcribeForButler(r *http.Request, id principal.Identity) (any, int, error) {
	if s.Butlers == nil || !s.butlersConfigured() {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no butler is configured on this deployment")
	}
	b, err := s.consoleDictationButler(r.PathValue("name"), id)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no butler answers there for you")
	}
	seconds := 0.0
	if v := strings.TrimSpace(r.URL.Query().Get("seconds")); v != "" {
		if seconds, err = strconv.ParseFloat(v, 64); err != nil || seconds < 0 {
			return nil, http.StatusBadRequest, fmt.Errorf("seconds must be a number of seconds")
		}
	}
	audio, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, speech.MaxAudioBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, http.StatusRequestEntityTooLarge, errors.New(dictationMessage(speech.ErrTooLarge, s.Speech))
		}
		return nil, http.StatusBadRequest, fmt.Errorf("reading the recording: %w", err)
	}

	res, terr := s.Speech.Transcribe(r.Context(), s.dictationMayLeaveHouse(b.Name), audio, r.Header.Get("Content-Type"), seconds)
	outcome := "transcribed"
	if terr != nil {
		outcome = voiceOutcome(terr)
	}
	attrs := map[string]string{
		"member": actingMember(id), "agent": b.Name, "outcome": outcome, "surface": "console",
		"seconds": strconv.FormatFloat(seconds, 'f', 1, 64),
	}
	if res.Processing != "" {
		attrs["engine"] = string(res.Processing)
	}
	s.recordEvent("console.dictation", b.Name, "dictation in the Console: "+outcome, attrs)
	if terr != nil {
		if outcome == "failed" {
			s.Log.Error("transcribing Console dictation", "agent", b.Name, "err", terr)
		}
		return nil, dictationStatus(terr), errors.New(dictationMessage(terr, s.Speech))
	}
	return map[string]any{"text": res.Text, "engine": string(res.Processing)}, http.StatusOK, nil
}

// consoleDictationButler resolves the Butler a Console caller may
// dictate to by the same two cases butlerlive.Butlers.Resolve applies
// to a named Console turn: the Console holder acting as owner reaches
// any live Butler, and anyone else only a Butler they were granted.
// Unlike asking, a Butler whose model is down still takes dictation;
// the words wait in the text box.
func (s *Server) consoleDictationButler(name string, id principal.Identity) (runtimedb.Butler, error) {
	if actingAsOwner(id) {
		b, err := s.DB.GetButler(name)
		if err != nil {
			return runtimedb.Butler{}, err
		}
		if !b.Live() {
			return runtimedb.Butler{}, runtimedb.ErrButlerUnreachable
		}
		return b, nil
	}
	b, err := s.DB.ResolveButlerFor(name, actingMember(id))
	if errors.Is(err, runtimedb.ErrButlerUnreachable) {
		// Access passed; only the Butler's model is down, or the Butler
		// is retired. ResolveButlerFor returns no record with this error,
		// so read it again, and still refuse a retired one.
		b, err = s.DB.GetButler(name)
		if err == nil && !b.Live() {
			err = runtimedb.ErrButlerUnreachable
		}
	}
	return b, err
}

// dictationInfo is what a Butler's status tells the Console about its
// mic: whether it works for this Butler, and how long it may record.
func (s *Server) dictationInfo(name string) (bool, int) {
	max := 0
	if s.Speech != nil {
		max = s.Speech.MaxSeconds
		if max <= 0 {
			max = speech.DefaultMaxSeconds
		}
	}
	return s.canDictate(name), max
}
