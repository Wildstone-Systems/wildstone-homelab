package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

// speechConfig is the deployment's voice-message settings (ADR-W042
// §3.3). Like -ntfy-url and -calendar-url, every engine is off until
// named, and no flag ever carries a credential: the cloud engine's key
// is OpenAI's, from OPENAI_API_KEY or the Console, exactly as a Butler
// on an OpenAI route gets it.
type speechConfig struct {
	Mode       string
	URL        string
	Model      string
	CloudModel string
	Language   string
	MaxSeconds int
	Timeout    time.Duration
}

// Defaults for the flags. The local model is multilingual and small
// enough to answer a voice note in seconds on a CPU; an English-only
// house can choose Systran/faster-distil-whisper-small.en instead.
const (
	defaultSpeechModel      = "Systran/faster-whisper-small"
	defaultSpeechCloudModel = "gpt-4o-mini-transcribe"
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

	r := &speech.Router{Mode: mode, Language: lang, MaxSeconds: c.MaxSeconds, Timeout: c.Timeout}
	var warnings []string
	if mode == speech.ModeOff {
		return r, nil, nil
	}

	if url := strings.TrimRight(strings.TrimSpace(c.URL), "/"); url != "" {
		model := strings.TrimSpace(c.Model)
		if model == "" {
			model = defaultSpeechModel
		}
		r.Local = &speech.Client{BaseURL: url, Model: model, Where: speech.Local, DownloadsModels: true}
	} else if mode == speech.ModeLocal || mode == speech.ModeLocalThenCloud {
		warnings = append(warnings, "no -speech-url is set, so there is no local speech engine; install the speech-speaches package on this node and set -speech-url http://127.0.0.1:18000/v1")
	}

	if mode == speech.ModeCloud || mode == speech.ModeLocalThenCloud {
		kind, _ := provider.LookupKind("openai")
		model := strings.TrimSpace(c.CloudModel)
		if model == "" {
			model = defaultSpeechCloudModel
		}
		r.Cloud = &speech.Client{BaseURL: kind.DefaultEndpoint, Model: model, Key: kind.Key, Where: speech.Cloud}
		// Only the environment can be checked this early: the Console's
		// key file is opened later in startup, and a key entered there
		// is picked up on the first voice message regardless.
		if strings.TrimSpace(os.Getenv(kind.KeyEnv)) == "" {
			warnings = append(warnings, "OPENAI_API_KEY is not set; unless an OpenAI key has been entered in the Console, only a local engine can transcribe")
		}
	}
	return r, warnings, nil
}
