package speech

// An installed engine (#408): the speech-speaches package, found in the
// household's desired state rather than named by -speech-url.
//
// Installing the package is the household saying it wants voice, so
// when the operator has not chosen a -speech-mode, the engine turns
// local transcription on by itself. It turns on nothing else: the cloud
// and spoken replies stay separate choices, made by flag. A choice the
// operator did make always wins: an explicit mode is kept, and a
// -speech-url is used rather than the installed engine's port.

import (
	"context"
	"log/slog"
	"strings"
)

// ModelState is how far a local engine's model has got.
type ModelState string

const (
	// ModelStarting: nothing is known yet, or the engine is not
	// answering. Keystone keeps asking.
	ModelStarting ModelState = "starting"
	// ModelDownloading: the engine is fetching the model. Voice
	// messages are answered with "try again shortly" until it is done.
	ModelDownloading ModelState = "downloading"
	// ModelReady: the model is present; dictation works.
	ModelReady ModelState = "ready"
	// ModelFailed: the engine refused the download. Detail says why.
	ModelFailed ModelState = "failed"
)

// Where a local engine's address came from.
const (
	SourceFlag    = "flag"    // -speech-url
	SourcePackage = "package" // the installed speech-speaches package
)

// Status is what the Console shows about local transcription.
type Status struct {
	// Mode is the mode in effect: the flag's, or local when an
	// installed engine turned voice on.
	Mode Mode `json:"mode"`
	// Source is where the local engine's address came from, or "" for
	// no local engine.
	Source string `json:"source,omitempty"`
	URL    string `json:"url,omitempty"`
	Model  string `json:"model,omitempty"`
	// State and Detail describe the model; empty with no local engine.
	State  ModelState `json:"state,omitempty"`
	Detail string     `json:"detail,omitempty"`
}

// installedEngine is the engine SetInstalled was last given.
type installedEngine struct {
	url   string
	local *Client
	voice *Client
	stop  context.CancelFunc
}

// view is the configuration in effect for one call: the flags, with an
// installed engine filling what they left open.
type view struct {
	mode, replyMode Mode
	local           Transcriber
	voice           Speaker
	source          string
}

func (r *Router) current() view {
	v := view{mode: r.Mode, replyMode: r.ReplyMode, local: r.Local, voice: r.LocalVoice}
	if v.mode == "" {
		v.mode = ModeOff
	}
	if v.replyMode == "" {
		v.replyMode = ModeOff
	}
	if v.local != nil {
		v.source = SourceFlag
	}
	r.mu.RLock()
	in := r.installed
	r.mu.RUnlock()
	if in == nil {
		return v
	}
	if r.AutoLocal && v.mode == ModeOff {
		v.mode = ModeLocal
	}
	if v.mode.usesLocal() && v.local == nil {
		v.local, v.source = in.local, SourcePackage
	}
	if v.replyMode.usesLocal() && v.voice == nil {
		v.voice = in.voice
	}
	return v
}

// SetInstalled tells the Router where an installed local engine
// answers, e.g. http://127.0.0.1:18000/v1, or "" for none. It is called
// whenever desired state may have changed and is cheap when nothing
// did. A new engine's models are fetched in the background, as Warm
// does at startup; a replaced one's fetch is stopped.
func (r *Router) SetInstalled(url string, log *slog.Logger) {
	if r == nil {
		return
	}
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	r.mu.Lock()
	old := r.installed
	if (old == nil && url == "") || (old != nil && old.url == url) {
		r.mu.Unlock()
		return
	}
	if old != nil {
		old.stop()
	}
	if url == "" {
		r.installed = nil
		r.mu.Unlock()
		if log != nil {
			log.Info("speech: the installed speech engine is gone")
		}
		return
	}
	ctx, stop := context.WithCancel(context.Background())
	in := &installedEngine{
		url:   url,
		local: &Client{BaseURL: url, Model: orDefault(r.LocalModel, DefaultLocalModel), Where: Local, DownloadsModels: true},
		voice: &Client{BaseURL: url, Model: orDefault(r.LocalVoiceModel, DefaultLocalVoiceModel),
			Voice: orDefault(r.LocalVoiceName, DefaultLocalVoiceName), Where: Local, DownloadsModels: true},
		stop: stop,
	}
	r.installed = in
	r.mu.Unlock()

	v := r.current()
	if log == nil {
		log = slog.Default()
	}
	used := v.local == Transcriber(in.local) || v.voice == Speaker(in.voice)
	log.Info("speech: found the installed speech engine", "url", url, "mode", v.mode, "used", used)
	go func() {
		if v.local == Transcriber(in.local) {
			warmClient(ctx, log, in.local, "speech")
		}
		if v.voice == Speaker(in.voice) {
			warmClient(ctx, log, in.voice, "voice")
		}
	}()
}

// InstalledURL is the installed engine's address, or "".
func (r *Router) InstalledURL() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.installed == nil {
		return ""
	}
	return r.installed.url
}

// FlagURL is the local engine the flags named, or "" for none.
func (r *Router) FlagURL() string {
	if r == nil {
		return ""
	}
	if c, ok := r.Local.(*Client); ok {
		return c.BaseURL
	}
	if c, ok := r.LocalVoice.(*Client); ok {
		return c.BaseURL
	}
	return ""
}

// Status reports the mode in effect and how far the local model has
// got.
func (r *Router) Status() Status {
	if r == nil {
		return Status{Mode: ModeOff}
	}
	v := r.current()
	st := Status{Mode: v.mode}
	if !v.mode.usesLocal() || v.local == nil {
		return st
	}
	st.Source = v.source
	if c, ok := v.local.(*Client); ok {
		st.URL, st.Model = c.BaseURL, c.Model
		st.State, st.Detail = c.modelState()
	} else {
		st.State = ModelReady
	}
	return st
}

// Defaults for an installed engine's models, the same as the flags'.
const (
	DefaultLocalModel      = "Systran/faster-whisper-small"
	DefaultLocalVoiceModel = "speaches-ai/Kokoro-82M-v1.0-ONNX"
	DefaultLocalVoiceName  = "af_heart"
)

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}
