package main

import (
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

func validSpeech() speechConfig {
	return speechConfig{Mode: "off", MaxSeconds: 120, Timeout: time.Minute, Language: "auto"}
}

func TestSpeechRouterOffByDefault(t *testing.T) {
	r, warn, err := speechRouter(validSpeech())
	if err != nil || len(warn) != 0 {
		t.Fatalf("off: %v %v", warn, err)
	}
	if r.Enabled() {
		t.Error("voice must be off until a deployment turns it on")
	}
}

func TestSpeechRouterLocal(t *testing.T) {
	c := validSpeech()
	c.Mode, c.URL = "local", "http://127.0.0.1:18000/v1/"
	r, warn, err := speechRouter(c)
	if err != nil || len(warn) != 0 {
		t.Fatalf("local: %v %v", warn, err)
	}
	local, ok := r.Local.(*speech.Client)
	if !ok || local.BaseURL != "http://127.0.0.1:18000/v1" || local.Model != defaultSpeechModel || !local.DownloadsModels {
		t.Fatalf("local engine = %+v", r.Local)
	}
	if r.Cloud != nil {
		t.Error("local mode must not configure a cloud engine")
	}
	if !r.Enabled() || r.Available(false) != true {
		t.Error("a local engine serves a local-only turn")
	}
}

func TestSpeechRouterWarnsWhenAnEngineIsMissing(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	c := validSpeech()
	c.Mode = "local-then-cloud"
	r, warn, err := speechRouter(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(warn) != 2 {
		t.Fatalf("want a warning for the missing URL and one for the missing key, got %v", warn)
	}
	if r.Cloud == nil || r.Cloud.Processing() != speech.Cloud {
		t.Error("the cloud engine is configured even before its key arrives, so a key entered later takes effect")
	}
}

func TestSpeechRouterRefusesNonsense(t *testing.T) {
	for name, mutate := range map[string]func(*speechConfig){
		"mode":     func(c *speechConfig) { c.Mode = "whisper" },
		"max":      func(c *speechConfig) { c.MaxSeconds = 0 },
		"timeout":  func(c *speechConfig) { c.Timeout = time.Hour },
		"language": func(c *speechConfig) { c.Language = "english" },
	} {
		c := validSpeech()
		mutate(&c)
		if _, _, err := speechRouter(c); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// Spoken replies are their own switch: off by default, served locally
// by the same engine as transcription, and never given a cloud voice
// unless asked.
func TestSpeechRepliesAreTheirOwnSwitch(t *testing.T) {
	r, _, err := speechRouter(validSpeech())
	if err != nil || r.SpeaksAtAll() {
		t.Fatalf("default: err = %v, speaks = %v", err, r.SpeaksAtAll())
	}

	c := validSpeech()
	c.ReplyMode, c.URL = "local", "http://127.0.0.1:18000/v1"
	r, warn, err := speechRouter(c)
	if err != nil || len(warn) != 0 {
		t.Fatalf("local replies: %v %v", warn, err)
	}
	v, ok := r.LocalVoice.(*speech.Client)
	if !ok || v.BaseURL != "http://127.0.0.1:18000/v1" || v.Model != defaultReplyModel || v.Voice != defaultReplyVoice || !v.DownloadsModels {
		t.Fatalf("local voice = %+v", r.LocalVoice)
	}
	if r.CloudVoice != nil || r.Enabled() || !r.CanSpeak(false) {
		t.Errorf("replies without transcription: cloud=%v hears=%v speaks=%v", r.CloudVoice, r.Enabled(), r.CanSpeak(false))
	}

	c.ReplyMode, c.ReplyCloudVoice = "cloud", "sage"
	r, _, err = speechRouter(c)
	if err != nil {
		t.Fatal(err)
	}
	cv, ok := r.CloudVoice.(*speech.Client)
	if !ok || cv.Voice != "sage" || cv.Model != defaultReplyCloudModel || cv.Processing() != speech.Cloud || r.LocalVoice != nil {
		t.Fatalf("cloud voice = %+v, local = %v", r.CloudVoice, r.LocalVoice)
	}

	c.ReplyMode, c.URL = "local", ""
	if _, warn, _ := speechRouter(c); len(warn) == 0 {
		t.Error("local replies with no engine URL gave no warning")
	}
	c.ReplyMode = "loud"
	if _, _, err := speechRouter(c); err == nil {
		t.Error("an unknown reply mode was accepted")
	}
}
