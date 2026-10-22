package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/speech"
)

// fakeVoice stands in for Kokoro or OpenAI's voices.
type fakeVoice struct {
	where speech.Processing
	err   error
	calls atomic.Int32
	said  atomic.Value
}

func (f *fakeVoice) Processing() speech.Processing { return f.where }
func (f *fakeVoice) Speak(_ context.Context, text string) ([]byte, error) {
	f.calls.Add(1)
	f.said.Store(text)
	if f.err != nil {
		return nil, f.err
	}
	return []byte("ID3 spoken " + string(f.where)), nil
}

// speaking turns spoken replies on for this harness with one voice.
func (h *harness) speaking(where speech.Processing) *fakeVoice {
	v := &fakeVoice{where: where}
	h.s.Speech.ReplyMode = speech.ModeLocalThenCloud
	if where == speech.Local {
		h.s.Speech.LocalVoice = v
	} else {
		h.s.Speech.CloudVoice = v
	}
	return v
}

// takeSpeech asks for a reply's audio the way the adapter does.
func (h *harness) takeSpeech(t *testing.T, seq int64) (int, string, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/chat/outbound/"+strconv.FormatInt(seq, 10)+"/speech", nil)
	req.Header.Set("Authorization", "Bearer "+h.tokens[runtimedb.RoleConcierge])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

// A person who spoke is answered in voice too: the reply says so, and
// the adapter gets the answer — not the "I heard" line — read aloud,
// once.
func TestVoiceMessageGetsASpokenReply(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler([]*provider.ChatResponse{{Text: "**Breakfast** was logged at 07:15."}}, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	h.hearing("did anyone have breakfast?")
	voice := h.speaking(speech.Local)

	if resp, out := h.sendVoice(t, runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "conversation": "12345", "messageId": "v-1",
	}, oggClip, "audio/ogg"); resp.StatusCode != http.StatusAccepted || out.Queued == 0 {
		t.Fatalf("status = %d, out = %+v", resp.StatusCode, out)
	}
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 1 || !queued[0].Speak {
		t.Fatalf("queued = %+v, want one reply marked to speak", queued)
	}
	if voice.calls.Load() != 0 {
		t.Fatal("the reply was read aloud before the adapter asked for it")
	}

	status, ct, audio := h.takeSpeech(t, queued[0].Seq)
	if status != http.StatusOK || ct != speech.SpokenMIME || string(audio) != "ID3 spoken local" {
		t.Fatalf("speech = %d %q %q", status, ct, audio)
	}
	if said, _ := voice.said.Load().(string); said != "Breakfast was logged at 07:15." {
		t.Fatalf("voice read %q, want the answer made speakable and without the I-heard line", said)
	}
	// Once only.
	if status, _, _ := h.takeSpeech(t, queued[0].Seq); status != http.StatusNotFound {
		t.Fatalf("second take = %d, want 404", status)
	}

	var spoken []runtimedb.Event
	events, _ := h.s.DB.EventsAfter(0, 200)
	for _, e := range events {
		if e.Type == "chat.voice_reply" {
			spoken = append(spoken, e)
		}
	}
	if len(spoken) != 1 || spoken[0].Attrs["outcome"] != "spoken" || spoken[0].Attrs["engine"] != "local" {
		t.Fatalf("chat.voice_reply events = %+v", spoken)
	}
	if strings.Contains(spoken[0].Message+strings.Join(mapValues(spoken[0].Attrs), " "), "Breakfast") {
		t.Errorf("the event carries the words: %+v", spoken[0])
	}
}

// A typed message is answered in text only, even with voices on.
func TestTypedMessageGetsNoSpokenReply(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler([]*provider.ChatResponse{{Text: "ok"}}, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	h.hearing("unused")
	h.speaking(speech.Local)
	h.send(runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "conversation": "12345", "messageId": "t-1", "text": "hello",
	})
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 1 || queued[0].Speak {
		t.Fatalf("queued = %+v, want a text-only reply", queued)
	}
	if status, _, _ := h.takeSpeech(t, queued[0].Seq); status != http.StatusNotFound {
		t.Fatalf("take = %d, want 404", status)
	}
}

// AC12, for answers: an Agent that keeps what it is told in the house
// is never read aloud by a cloud voice. With only a cloud voice, its
// reply is text only.
func TestSpokenReplyForALocalAgentNeverReachesTheCloud(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler([]*provider.ChatResponse{{Text: "the alarm code is in the drawer"}}, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	h.hearing("where is the alarm code?")
	cloud := h.speaking(speech.Cloud)
	h.sendVoice(t, runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "conversation": "12345", "messageId": "v-2",
	}, oggClip, "audio/ogg")
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 1 || queued[0].Speak {
		t.Fatalf("queued = %+v, want text only", queued)
	}
	h.takeSpeech(t, queued[0].Seq)
	if cloud.calls.Load() != 0 {
		t.Fatal("a local-only Agent's answer reached the cloud voice")
	}
}

// Another bot's adapter cannot take this bot's reply.
func TestSpokenReplyIsTheConnectionsOwn(t *testing.T) {
	h := newHarness(t)
	if status, _, _ := h.takeSpeech(t, 999); status != http.StatusNotFound && status != http.StatusForbidden {
		t.Fatalf("unknown reply = %d", status)
	}
}
