package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/speech"
)

// speechConfig is the deployment's voice-message settings (ADR-W042
// §3.3). Like -ntfy-url and -calendar-url, every engine is off until
// named, and no flag ever carries a credential: the cloud engine's key
// is OpenAI's, from OPENAI_API_KEY or the Console, exactly as a Butler
// on an OpenAI route gets it.
type speechConfig struct {
	Mode string
	// ModeSet is whether -speech-mode was given. When it was not, an
	// installed speech-speaches engine turns local transcription on by
	// itself (#408); an explicit mode, "off" included, is kept.
	ModeSet    bool
	URL        string
	Model      string
	CloudModel string
	Language   string
	MaxSeconds int
	Timeout    time.Duration

	// Spoken replies (ADR-W042 part two). The local voice is served by
	// the same engine as transcription, at URL.
	ReplyMode       string
	ReplyModel      string
	ReplyVoice      string
	ReplyCloudModel string
	ReplyCloudVoice string
}

// Defaults for the flags. The local model is multilingual and small
// enough to answer a voice note in seconds on a CPU; an English-only
// house can choose Systran/faster-distil-whisper-small.en instead.
const (
	defaultSpeechModel      = speech.DefaultLocalModel
	defaultSpeechCloudModel = "gpt-4o-mini-transcribe"

	// Kokoro, the project's stated text-to-speech preference: small,
	// natural, and fast on a CPU. af_heart is its best-rated English
	// voice; Speaches lists the others at GET /v1/audio/speech/voices.
	defaultReplyModel      = speech.DefaultLocalVoiceModel
	defaultReplyVoice      = speech.DefaultLocalVoiceName
	defaultReplyCloudModel = "gpt-4o-mini-tts"
	defaultReplyCloudVoice = "coral"
)

// speechRouter builds the Router the API transcribes through. It fails
// only for a value that cannot mean anything; an engine that is named
// but not usable yet (no key, container not up) is a warning, because
// Keystone must start either way and the Router already answers "not
// available" for it.
func speechRouter(c speechConfig) (*speech.Router, []string, error) {
	mode, err := speech.ParseMode(c.Mode)
	if err != nil {
		return nil, nil, err
	}
	if c.MaxSeconds < 5 || c.MaxSeconds > 600 {
		return nil, nil, fmt.Errorf("-speech-max-seconds must be between 5 and 600, not %d", c.MaxSeconds)
	}
	if c.Timeout < 5*time.Second || c.Timeout > 10*time.Minute {
		return nil, nil, fmt.Errorf("-speech-timeout must be between 5s and 10m, not %s", c.Timeout)
	}
	lang := strings.ToLower(strings.TrimSpace(c.Language))
	if lang == "auto" {
		lang = ""
	}
	if lang != "" && (len(lang) != 2 || strings.Trim(lang, "abcdefghijklmnopqrstuvwxyz") != "") {
		return nil, nil, fmt.Errorf("-speech-language must be a two-letter code such as en, or auto, not %q", c.Language)
	}

	replyMode, err := speech.ParseMode(c.ReplyMode)
	if err != nil {
		return nil, nil, fmt.Errorf("-speech-replies: %w", err)
	}

	r := &speech.Router{Mode: mode, Language: lang, MaxSeconds: c.MaxSeconds, Timeout: c.Timeout, ReplyMode: replyMode,
		AutoLocal: !c.ModeSet, LocalModel: c.Model, LocalVoiceModel: c.ReplyModel, LocalVoiceName: c.ReplyVoice}
	var warnings []string
	url := strings.TrimRight(strings.TrimSpace(c.URL), "/")
	usesLocal := func(m speech.Mode) bool { return m == speech.ModeLocal || m == speech.ModeLocalThenCloud }
	usesCloud := func(m speech.Mode) bool { return m == speech.ModeCloud || m == speech.ModeLocalThenCloud }
	kind, _ := provider.LookupKind("openai")

	if usesLocal(mode) {
		if url != "" {
			r.Local = &speech.Client{BaseURL: url, Model: orDefault(c.Model, defaultSpeechModel), Where: speech.Local, DownloadsModels: true}
		} else {
			warnings = append(warnings, "no -speech-url is set, so the local speech engine is the speech-speaches package, once it is installed on this node")
		}
	}
	if usesCloud(mode) {
		r.Cloud = &speech.Client{BaseURL: kind.DefaultEndpoint, Model: orDefault(c.CloudModel, defaultSpeechCloudModel), Key: kind.Key, Where: speech.Cloud}
	}

	if usesLocal(replyMode) {
		if url != "" {
			r.LocalVoice = &speech.Client{BaseURL: url, Model: orDefault(c.ReplyModel, defaultReplyModel),
				Voice: orDefault(c.ReplyVoice, defaultReplyVoice), Where: speech.Local, DownloadsModels: true}
		} else {
			warnings = append(warnings, "no -speech-url is set, so the local voice that reads replies aloud is the speech-speaches package, once it is installed on this node")
		}
	}
	if usesCloud(replyMode) {
		r.CloudVoice = &speech.Client{BaseURL: kind.DefaultEndpoint, Model: orDefault(c.ReplyCloudModel, defaultReplyCloudModel),
			Voice: orDefault(c.ReplyCloudVoice, defaultReplyCloudVoice), Key: kind.Key, Where: speech.Cloud}
	}

	// Only the environment can be checked this early: the Console's key
	// file is opened later in startup, and a key entered there is picked
	// up on first use regardless.
	if (usesCloud(mode) || usesCloud(replyMode)) && strings.TrimSpace(os.Getenv(kind.KeyEnv)) == "" {
		warnings = append(warnings, "OPENAI_API_KEY is not set; unless an OpenAI key has been entered in the Console, only a local engine can transcribe or read replies aloud")
	}
	return r, warnings, nil
}

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}

// shortHostname is -node-id's default: the installer names a node
// `hostname -s` unless told otherwise, so on a default install this is
// Keystone's own node id. "" when the host name cannot be read.
func shortHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	return h
}
