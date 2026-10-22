package api

// Reading replies aloud on the dashboard and in the Console (ADR-W042
// part two).
//
// A listen button on each reply posts the reply's text here and plays
// what comes back. The door is as narrow as dictation's: the same
// sign-in, the same assistant ceiling, and the engine follows the
// assistant's privacy class — an assistant that keeps what it is told
// in the house has its answers read by the house's own voice, or not at
// all. It speaks and does nothing else; nothing is stored.
//
// The page sends the text rather than naming a stored turn so a reply
// can be read the moment it arrives. That lets a signed-in person have
// any text read by an assistant they already reach — nothing they could
// not do by asking it — and the text is bounded, redacted, and cut to
// MaxSpokenChars before any engine sees it.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/redact"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/speech"
)

// maxReadAloudBytes bounds the text a page may send to be read. A reply
// longer than this is still read: its first MaxSpokenChars are, and
// the page sends no more than that.
const maxReadAloudBytes = 64 << 10

// assistantSpeak is POST /api/v1/assistant/speak, {"agent","text"}:
// the text read aloud, as audio/mpeg.
func (s *Server) assistantSpeak(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	var req struct {
		Agent string `json:"agent"`
		Text  string `json:"text"`
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxReadAloudBytes)
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	b, status, err := s.portalVoiceButler(strings.TrimSpace(req.Agent), member)
	if err != nil {
		return nil, status, err
	}
	return s.readAloud(r, b.Name, req.Text, "assistant.read_aloud", map[string]string{"member": member})
}

// portalVoiceButler resolves the assistant a dashboard voice request
// (dictation, or a listen button) is for: one this member may reach
// from the dashboard. An assistant whose model is down still counts —
// voice does not need the model — so it is read again after
// ResolveButlerFor, which returns no record with ErrButlerUnreachable,
// and a retired one is still refused.
func (s *Server) portalVoiceButler(agent, member string) (runtimedb.Butler, int, error) {
	refused := fmt.Errorf("no assistant answers there for you")
	b, err := s.DB.ResolveButlerFor(agent, member)
	if errors.Is(err, runtimedb.ErrButlerUnreachable) {
		b, err = s.DB.GetButler(agent)
		if err == nil && !b.Live() {
			err = runtimedb.ErrButlerUnreachable
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, runtimedb.ErrButlerUnreachable), errors.Is(err, runtimedb.ErrNoButlerAccess),
		errors.Is(err, runtimedb.ErrNotFound), errors.Is(err, runtimedb.ErrNotAdmitted):
		return runtimedb.Butler{}, http.StatusNotFound, refused
	default:
		return runtimedb.Butler{}, http.StatusInternalServerError, err
	}
	if !portalMayReach(b) {
		return runtimedb.Butler{}, http.StatusNotFound, refused
	}
	return b, http.StatusOK, nil
}

// speakForButler is POST /api/v1/butlers/{name}/speak, {"text"}: the
// Console's listen button, with the Console dictation's access rule.
func (s *Server) speakForButler(r *http.Request, id principal.Identity) (any, int, error) {
	if s.Butlers == nil || !s.butlersConfigured() {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no butler is configured on this deployment")
	}
	b, err := s.consoleDictationButler(r.PathValue("name"), id)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no butler answers there for you")
	}
	var req struct {
		Text string `json:"text"`
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxReadAloudBytes)
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	return s.readAloud(r, b.Name, req.Text, "console.read_aloud",
		map[string]string{"member": actingMember(id), "surface": "console"})
}

// readAloud speaks text for one assistant and records the outcome —
// never the words.
func (s *Server) readAloud(r *http.Request, agent, text, event string, attrs map[string]string) (any, int, error) {
	if strings.TrimSpace(text) == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("there is nothing to read aloud")
	}
	spoken, err := s.Speech.Speak(r.Context(), s.dictationMayLeaveHouse(agent), redact.String(text))
	outcome := "spoken"
	if err != nil {
		outcome = voiceOutcome(err)
	}
	attrs["agent"], attrs["outcome"] = agent, outcome
	if spoken.Processing != "" {
		attrs["engine"] = string(spoken.Processing)
	}
	s.recordEvent(event, agent, "a reply read aloud: "+outcome, attrs)
	if err != nil {
		if outcome == "failed" {
			s.Log.Error("reading a reply aloud", "agent", agent, "err", err)
		}
		return nil, speakStatus(err), errors.New(readAloudMessage(err))
	}
	return audioResponse{MIME: spoken.MIME, Audio: spoken.Audio, Engine: string(spoken.Processing)}, http.StatusOK, nil
}

// canReadAloud tells a page whether to show the listen button for an
// assistant: replies are read aloud here, and a voice its privacy
// class allows exists.
func (s *Server) canReadAloud(agent string) bool {
	return s.Speech.CanSpeak(s.dictationMayLeaveHouse(agent))
}

// readAloudMessage is what the page shows when a reply cannot be read.
func readAloudMessage(err error) string {
	switch {
	case errors.Is(err, speech.ErrOff):
		return "Reading replies aloud isn't turned on here."
	case errors.Is(err, speech.ErrUnavailable):
		return "Reading aloud isn't available for this assistant."
	case errors.Is(err, speech.ErrWarming):
		return "The voice is still getting ready. Try again in a few minutes."
	case errors.Is(err, speech.ErrEmpty):
		return "There's nothing in that reply to read aloud."
	}
	return "That reply couldn't be read aloud just now. Try again in a moment."
}
