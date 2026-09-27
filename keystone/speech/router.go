package speech

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	// DefaultTimeout bounds one transcription. A two-minute note on a
	// small model takes seconds on a modern CPU; a minute is the point
	// at which a person has given up waiting anyway.
	DefaultTimeout = 60 * time.Second
	// warmTimeout bounds one model download. Whisper models run from
	// ~75 MB (tiny) to ~1.6 GB (large-v3-turbo).
	warmTimeout = 30 * time.Minute
)

// Result is a transcription and where it was made, for the audit record
// (ADR-W042 §2.4) and for a caller that marks the turn.
type Result struct {
	Text       string
	Processing Processing
}

// Router picks the engine a turn may use and runs it (ADR-W042 §2.2
// steps 4 and 5). It is safe for concurrent use.
type Router struct {
	Mode Mode
	// Local and Cloud are the configured engines; nil means not
	// configured. Each is trusted to report its own Processing, and the
	// Router checks it rather than the slot it was put in, so a cloud
	// engine configured as "local" by mistake is still treated as cloud.
	Local, Cloud Transcriber
	// Language is an ISO-639-1 hint ("en"), or "" to detect.
	Language string
	// MaxSeconds bounds a recording's reported duration; <= 0 means
	// DefaultMaxSeconds.
	MaxSeconds int
	// Timeout bounds one engine call; <= 0 means DefaultTimeout.
	Timeout time.Duration

	once sync.Once
	// slot serialises transcriptions: one at a time per Keystone. On a
	// CPU engine two at once each take twice as long, and a queue is
	// fairer than a pile-up (ADR-W042 §6).
	slot chan struct{}
}

// Enabled reports whether voice is on and any engine is configured. A
// caller uses it to decide whether to show a mic button at all.
func (r *Router) Enabled() bool {
	return r != nil && r.Mode != ModeOff && len(r.engines(true)) > 0
}

// Available reports whether a turn with this permission could be
// transcribed.
func (r *Router) Available(allowCloud bool) bool {
	return r != nil && r.Mode != ModeOff && len(r.engines(allowCloud)) > 0
}

// engines lists, in order of preference, the engines a turn may use.
// Local first, always: a house that has one prefers it.
func (r *Router) engines(allowCloud bool) []Transcriber {
	var out []Transcriber
	add := func(t Transcriber) {
		if t == nil {
			return
		}
		if t.Processing() != Local && !allowCloud {
			return
		}
		out = append(out, t)
	}
	if r.Mode.usesLocal() {
		add(r.Local)
	}
	if r.Mode.usesCloud() {
		add(r.Cloud)
	}
	return out
}

// Transcribe turns one recording into text. allowCloud is the turn's
// privacy answer — false for an Agent or conversation that must stay in
// the house — and it is decided by the caller, which knows the Agent;
// this only obeys it. seconds is the reported duration, or 0.
//
// A turn that may not leave the house and has no local engine gets
// ErrUnavailable, never a quiet cloud fallback (AC12).
func (r *Router) Transcribe(ctx context.Context, allowCloud bool, audio []byte, mimeType string, seconds float64) (Result, error) {
	if r == nil || r.Mode == ModeOff {
		return Result{}, ErrOff
	}
	mt, err := NormalizeMIME(mimeType)
	if err != nil {
		return Result{}, err
	}
	if err := CheckSize(len(audio), seconds, r.MaxSeconds); err != nil {
		return Result{}, err
	}
	engines := r.engines(allowCloud)
	if len(engines) == 0 {
		return Result{}, ErrUnavailable
	}

	r.once.Do(func() { r.slot = make(chan struct{}, 1) })
	select {
	case r.slot <- struct{}{}:
		defer func() { <-r.slot }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var last error
	for _, t := range engines {
		tctx, cancel := context.WithTimeout(ctx, timeout)
		text, err := t.Transcribe(tctx, audio, mt, r.Language)
		cancel()
		if err == nil {
			return Result{Text: text, Processing: t.Processing()}, nil
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		// What the recording IS does not change with the engine: silence
		// is silence, and it is not worth sending out of the house to
		// find that out a second time.
		if errors.Is(err, ErrEmpty) || errors.Is(err, ErrUnsupported) || errors.Is(err, ErrTooLarge) {
			return Result{}, err
		}
		last = err
	}
	return Result{}, last
}

// Warm downloads the local engine's model ahead of the first voice
// message, retrying until it succeeds or ctx ends. Keystone runs it in
// the background at startup; the Speaches container may start after
// Keystone does, and a model download is minutes, not something to make
// the first person who speaks wait for.
func (r *Router) Warm(ctx context.Context, log *slog.Logger) {
	if r == nil || !r.Mode.usesLocal() {
		return
	}
	c, ok := r.Local.(*Client)
	if !ok || !c.DownloadsModels {
		return
	}
	backoff := 10 * time.Second
	for {
		wctx, cancel := context.WithTimeout(ctx, warmTimeout)
		err := c.download(wctx)
		cancel()
		if err == nil {
			log.Info("speech: local model ready", "model", c.Model)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errDownloading) {
			// A voice message's 404 started the download first. Wait
			// for it rather than asking for the same model again; the
			// next attempt finds it present and returns at once.
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		log.Warn("speech: local model not ready yet; voice messages will say so until it is", "model", c.Model, "err", err, "retry", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
}
