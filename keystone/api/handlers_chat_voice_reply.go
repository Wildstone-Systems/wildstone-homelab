package api

// Spoken replies to chat voice messages (ADR-W042 part two).
//
// A person who spoke gets the answer as text AND as a voice message,
// when the deployment reads replies aloud (-speech-replies) and the
// Agent may use a voice engine: the same privacy rule that chose where
// their words were heard chooses where the answer is spoken.
//
// The audio is not made when the reply is queued. The reply carries the
// text to speak; the adapter delivers the text first, then asks for the
// audio here and sends it as a voice message. So a slow or failed voice
// never holds up or loses the answer, and nothing is synthesised for a
// reply that was stopped before delivery.

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/redact"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/speech"
)

// replySpeech is the text to read aloud for the reply to a voice
// message on this Connection, or "" when the reply is text only.
func (s *Server) replySpeech(at runtimedb.Connection, answer string) string {
	if !s.Speech.CanSpeak(s.voiceMayLeaveHouse(at)) {
		return ""
	}
	return redact.String(answer)
}

// speakChatOutbound is POST /api/v1/chat/outbound/{seq}/speech: the
// adapter asks for a delivered reply read aloud, once. The answer is
// the audio itself (audio/mpeg); 404 means there is nothing (more) to
// speak for that message, and any other failure means send nothing —
// the text has already arrived.
func (s *Server) speakChatOutbound(r *http.Request, id principal.Identity) (any, int, error) {
	at, err := s.pollingConnection(r, id)
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("seq must be a number")
	}
	text, err := s.DB.TakeChatOutboundSpeech(at, seq)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		return nil, http.StatusNotFound, fmt.Errorf("no reply %d to read aloud on this connection", seq)
	case errors.Is(err, runtimedb.ErrPrivacy):
		s.recordSpokenReply(at, "private", "")
		return nil, http.StatusForbidden, fmt.Errorf("that reply may not be read aloud here")
	case err != nil:
		return nil, http.StatusInternalServerError, err
	}
	// The privacy answer is taken again now, not trusted from when the
	// reply was queued: an Agent moved to a local model since must not
	// have its answer spoken by a cloud voice.
	spoken, err := s.Speech.Speak(r.Context(), s.voiceMayLeaveHouse(at), text)
	if err != nil {
		outcome := voiceOutcome(err)
		s.recordSpokenReply(at, outcome, "")
		if outcome == "failed" {
			s.Log.Error("reading a reply aloud", "agent", at.Butler, "err", err)
		}
		return nil, speakStatus(err), fmt.Errorf("the reply could not be read aloud: %s", outcome)
	}
	s.recordSpokenReply(at, "spoken", spoken.Processing)
	return audioResponse{MIME: spoken.MIME, Audio: spoken.Audio, Engine: string(spoken.Processing)}, http.StatusOK, nil
}

// recordSpokenReply is the audit line for one reply read aloud: which
// Agent, which engine, how it went. Never the words or the audio.
func (s *Server) recordSpokenReply(at runtimedb.Connection, outcome string, engine speech.Processing) {
	attrs := map[string]string{"agent": at.Butler, "outcome": outcome}
	if engine != "" {
		attrs["engine"] = string(engine)
	}
	s.recordEvent("chat.voice_reply", at.ExternalID, "a reply read aloud: "+outcome, attrs)
}

// speakStatus maps a synthesis error to the status a caller acts on.
func speakStatus(err error) int {
	switch {
	case errors.Is(err, speech.ErrOff), errors.Is(err, speech.ErrUnavailable), errors.Is(err, speech.ErrWarming):
		return http.StatusServiceUnavailable
	case errors.Is(err, speech.ErrEmpty):
		return http.StatusUnprocessableEntity
	case errors.Is(err, speech.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadGateway
}
