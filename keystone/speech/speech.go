// Package speech turns a recorded voice message into text (ADR-W042).
//
// It is the one place Wildstone talks to a speech-to-text engine. The
// chat adapters only download audio; the portal only records it; both
// hand the bytes to Keystone, and Keystone hands them here. That keeps
// the choice between a local engine and a cloud one — the privacy
// decision — in a single spot an adapter cannot reach.
//
// Two engines, one wire shape: a local Speaches container and OpenAI
// both answer the OpenAI transcription API, so Client serves either and
// the only difference Wildstone cares about is Processing — whether the
// audio leaves the house.
//
// Audio is never stored. It is passed through in memory and dropped;
// nothing in this package writes it to disk, a log, or an error.
package speech

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"strings"
)

// Processing says where a Transcriber sends audio. There is no third
// answer, for the same reason runtimedb.ModelLocal/ModelCloud has none.
type Processing string

const (
	Local Processing = "local"
	Cloud Processing = "cloud"
)

// Transcriber turns audio into text.
type Transcriber interface {
	// Processing is where the audio goes. The Router refuses a Cloud
	// transcriber for a turn that may not leave the house.
	Processing() Processing
	// Transcribe returns what was said. audio is one whole recording;
	// mimeType is its container as the vendor labelled it (see
	// NormalizeMIME). language is an ISO-639-1 hint, or "" to detect.
	Transcribe(ctx context.Context, audio []byte, mimeType, language string) (string, error)
}

// Limits a recording must fit before anyone spends CPU, or a cloud
// call, on it (ADR-W042 §2.2). The adapters check the vendor-reported
// duration before downloading; Keystone checks both again, because an
// adapter is not the last word on what it sent.
const (
	// MaxAudioBytes bounds one recording. Two minutes of Opus voice is
	// well under 1 MB; 10 MB leaves room for a phone's AAC-in-MP4 and a
	// browser's WebM without admitting a film.
	MaxAudioBytes = 10 << 20
	// DefaultMaxSeconds is the longest recording accepted unless the
	// deployment says otherwise.
	DefaultMaxSeconds = 120
)

// Errors a caller turns into a reply a person can act on. None of them
// carries audio, a transcript, or a credential.
var (
	// ErrOff: voice is turned off for this deployment.
	ErrOff = errors.New("speech: voice messages are turned off")
	// ErrUnavailable: no engine this turn may use is installed or ready.
	// A turn that may not leave the house and a house with no local
	// engine end here, rather than falling through to the cloud.
	ErrUnavailable = errors.New("speech: no speech-to-text engine this conversation may use is available")
	// ErrTooLarge: the recording is over MaxAudioBytes or the duration
	// limit.
	ErrTooLarge = errors.New("speech: the recording is too long")
	// ErrUnsupported: not an audio container any engine here decodes.
	ErrUnsupported = errors.New("speech: that kind of recording is not supported")
	// ErrEmpty: the engine heard nothing it could write down.
	ErrEmpty = errors.New("speech: nothing could be made out in the recording")
	// ErrWarming: the local engine is still downloading its model. It
	// clears on its own; the caller should say "try again shortly".
	ErrWarming = errors.New("speech: the speech model is still being prepared")
)

// formats maps each accepted container to the file extension the
// OpenAI API infers the format from. Speaches decodes with FFmpeg and
// does not care; OpenAI refuses a name it does not recognise, so the
// extension is part of the contract, not decoration.
//
// Video types are here on purpose: Slack labels its audio clips
// video/mp4, and a Telegram round video note is MP4. The engines read
// the audio track and ignore the rest.
var formats = map[string]string{
	"audio/ogg":   "ogg", // Telegram and Discord voice notes (Opus)
	"audio/opus":  "ogg",
	"audio/webm":  "webm", // Chrome, Firefox, Edge MediaRecorder
	"video/webm":  "webm",
	"audio/mp4":   "mp4", // Safari MediaRecorder, phone voice memos
	"video/mp4":   "mp4", // Slack audio clips, Telegram video notes
	"audio/m4a":   "m4a",
	"audio/x-m4a": "m4a",
	"audio/mpeg":  "mp3",
	"audio/mp3":   "mp3",
	"audio/wav":   "wav",
	"audio/x-wav": "wav",
	"audio/wave":  "wav",
	"audio/flac":  "flac",
}

// NormalizeMIME reduces a vendor's content type to one this package
// accepts — "audio/webm;codecs=opus" becomes "audio/webm" — or returns
// ErrUnsupported.
func NormalizeMIME(contentType string) (string, error) {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return "", ErrUnsupported
	}
	mt = strings.ToLower(mt)
	if _, ok := formats[mt]; !ok {
		return "", ErrUnsupported
	}
	return mt, nil
}

// fileName is the name the upload is sent under.
func fileName(mimeType string) string {
	return "voice." + formats[mimeType]
}

// CheckSize refuses a recording before it is transcribed. seconds is
// the duration the vendor or browser reported, or 0 when it said
// nothing; maxSeconds <= 0 means DefaultMaxSeconds.
func CheckSize(size int, seconds float64, maxSeconds int) error {
	if maxSeconds <= 0 {
		maxSeconds = DefaultMaxSeconds
	}
	if size <= 0 {
		return ErrEmpty
	}
	if size > MaxAudioBytes || seconds > float64(maxSeconds) {
		return ErrTooLarge
	}
	return nil
}

// Mode is how the deployment has chosen to transcribe (ADR-W042 §3.3).
type Mode string

const (
	// ModeOff: voice messages are refused with a "please type" reply.
	ModeOff Mode = "off"
	// ModeLocal: the local engine only; never the cloud.
	ModeLocal Mode = "local"
	// ModeLocalThenCloud: the local engine, and the cloud one only when
	// the local one is missing or fails AND the turn may leave the
	// house.
	ModeLocalThenCloud Mode = "local-then-cloud"
	// ModeCloud: the cloud engine, for a turn that may leave the house.
	ModeCloud Mode = "cloud"
)

// ParseMode validates a configured mode.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(strings.ToLower(s))); m {
	case ModeOff, ModeLocal, ModeLocalThenCloud, ModeCloud:
		return m, nil
	case "":
		return ModeOff, nil
	}
	return "", fmt.Errorf("speech: mode must be off, local, local-then-cloud or cloud, not %q", s)
}

func (m Mode) usesLocal() bool { return m == ModeLocal || m == ModeLocalThenCloud }
func (m Mode) usesCloud() bool { return m == ModeCloud || m == ModeLocalThenCloud }
