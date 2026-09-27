package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

// fakeEngine stands in for Speaches or OpenAI: it says what it was set
// to say and counts how often it was asked.
type fakeEngine struct {
	where speech.Processing
	text  string
	err   error
	calls atomic.Int32
}

func (f *fakeEngine) Processing() speech.Processing { return f.where }
func (f *fakeEngine) Transcribe(context.Context, []byte, string, string) (string, error) {
	f.calls.Add(1)
	return f.text, f.err
}

// hearing turns voice on for this harness with one local engine.
func (h *harness) hearing(text string) *fakeEngine {
	e := &fakeEngine{where: speech.Local, text: text}
	h.s.Speech = &speech.Router{Mode: speech.ModeLocal, Local: e}
	return e
}

var oggClip = []byte("OggS\x00\x02 a short opus voice note")

// sendVoice posts a voice message the way the chat adapter does.
func (h *harness) sendVoice(t *testing.T, role runtimedb.Role, meta map[string]any, audio []byte, mimeType string) (*http.Response, inboundResponse) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	raw, _ := json.Marshal(meta)
	if err := w.WriteField("message", string(raw)); err != nil {
		t.Fatal(err)
	}
	if audio != nil {
		part, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="audio"; filename="voice.ogg"`},
			"Content-Type":        {mimeType},
		})
		if err != nil {
			t.Fatal(err)
		}
		part.Write(audio)
	}
	w.Close()
	return h.postVoice(t, role, w.FormDataContentType(), &body)
}

func (h *harness) postVoice(t *testing.T, role runtimedb.Role, contentType string, body *bytes.Buffer) (*http.Response, inboundResponse) {
	t.Helper()
	req, err := http.NewRequest("POST", h.srv.URL+"/api/v1/chat/voice", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+h.tokens[role])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out inboundResponse
	if resp.StatusCode == http.StatusAccepted {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp, out
}

func voiceEvents(t *testing.T, h *harness) []runtimedb.Event {
	t.Helper()
	events, err := h.s.DB.EventsAfter(0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []runtimedb.Event
	for _, e := range events {
		if e.Type == "chat.voice" {
			out = append(out, e)
		}
	}
	return out
}

// The whole loop: a bound account speaks, the words reach the Agent as
// an ordinary turn, and the reply says what was heard.
func TestVoiceRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler([]*provider.ChatResponse{{Text: "Breakfast was logged at 07:15."}}, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	engine := h.hearing("did anyone have breakfast?")

	resp, out := h.sendVoice(t, runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "conversation": "12345",
		"messageId": "v-1", "audioSeconds": 3.2,
	}, oggClip, "audio/ogg")
	if resp.StatusCode != http.StatusAccepted || !out.Bound || out.Queued == 0 {
		t.Fatalf("status = %d, out = %+v", resp.StatusCode, out)
	}
	if engine.calls.Load() != 1 {
		t.Fatalf("engine calls = %d, want 1", engine.calls.Load())
	}
	queued := h.poll(t, runtimedb.RoleConcierge, "")
	if len(queued) != 1 {
		t.Fatalf("queued = %+v", queued)
	}
	if !strings.Contains(queued[0].Body, "I heard: “did anyone have breakfast?”") || !strings.Contains(queued[0].Body, "Breakfast was logged") {
		t.Fatalf("reply = %q, want what was heard and the answer", queued[0].Body)
	}
	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions?surface=family", nil, &sessions)
	if len(sessions) != 1 || sessions[0].ActingFor != "sam" {
		t.Fatalf("sessions = %+v, want one for the speaker", sessions)
	}

	events := voiceEvents(t, h)
	if len(events) != 1 {
		t.Fatalf("chat.voice events = %+v", events)
	}
	e := events[0]
	if e.Attrs["outcome"] != "transcribed" || e.Attrs["engine"] != "local" || e.Attrs["account"] != "12345" {
		t.Errorf("event = %+v", e)
	}
	if strings.Contains(e.Message+strings.Join(mapValues(e.Attrs), " "), "breakfast") {
		t.Errorf("the event carries the words: %+v", e)
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// F-155-12: a voice message from an account nobody admitted is dropped
// before any engine hears it.
func TestVoiceFromAStrangerIsNeverHeard(t *testing.T) {
	h := newHarness(t)
	ownExecutionFixture(t, h)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	h.agentAt(t, "family", "telegram")
	engine := h.hearing("anything")

	resp, out := h.sendVoice(t, runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "99999", "messageId": "v-1",
	}, oggClip, "audio/ogg")
	if resp.StatusCode != http.StatusAccepted || out.Bound {
		t.Fatalf("status = %d, out = %+v", resp.StatusCode, out)
	}
	if engine.calls.Load() != 0 {
		t.Fatal("a stranger's recording reached the speech engine")
	}
	if q := h.poll(t, runtimedb.RoleConcierge, ""); len(q) != 0 {
		t.Fatalf("a stranger was answered: %+v", q)
	}
}

// A room gets the same redirect a typed message gets, and the recording
// is not transcribed to produce it.
func TestVoiceInAGroupIsRedirectedUnheard(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "777")
	engine := h.hearing("whose turn is it for the bins?")

	resp, out := h.sendVoice(t, runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "777", "conversation": "group-1",
		"group": true, "addressed": true, "messageId": "g-1",
	}, oggClip, "audio/ogg")
	if resp.StatusCode != http.StatusAccepted || !out.Bound {
		t.Fatalf("status = %d, out = %+v", resp.StatusCode, out)
	}
	if engine.calls.Load() != 0 {
		t.Fatal("a group recording was transcribed")
	}
	q := h.poll(t, runtimedb.RoleConcierge, "")
	if len(q) != 1 || !strings.Contains(q[0].Body, "one-to-one") {
		t.Fatalf("group reply = %+v", q)
	}
}

// ADR-W042 §2.3: spoken words cannot store a secret or run a command.
// A spoken secret is recorded as disclosed, never stored, never
// repeated, and never shown to the Agent; a spoken command is a
// sentence for the Agent.
func TestVoiceCannotStoreASecret(t *testing.T) {
	for _, said := range []string{
		"/secret household wifi = hunter2",
		"slash secret household wifi equals hunter2 and the password is hunter2",
		"the alarm code is hunter2",
	} {
		t.Run(said, func(t *testing.T) {
			h := newHarness(t)
			p := &scriptedProvider{turns: []*provider.ChatResponse{{Text: "must not run"}}}
			h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
			h.s.ButlerHost.Provider = p
			h.bindAccount(t, "family", "sam", "telegram", "12345")
			h.hearing(said)

			resp, out := h.sendVoice(t, runtimedb.RoleConcierge, map[string]any{
				"transport": "telegram", "account": "12345", "messageId": "v-1",
			}, oggClip, "audio/ogg")
			if resp.StatusCode != http.StatusAccepted || out.Reason != "a spoken secret was not stored" {
				t.Fatalf("status = %d, out = %+v", resp.StatusCode, out)
			}
			if p.i != 0 {
				t.Fatal("a spoken secret reached the Agent")
			}
			q := h.poll(t, runtimedb.RoleConcierge, "")
			if len(q) != 1 || strings.Contains(q[0].Body, "hunter2") || !strings.Contains(q[0].Body, "Secrets page") {
				t.Fatalf("reply = %+v, want the secrets-page note and no echo of the value", q)
			}
			// Touching every stored secret counts them without reading one.
			res, err := h.s.DB.ExecForTest(`UPDATE household_secrets SET label=label`)
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := res.RowsAffected(); n != 0 {
				t.Fatalf("a spoken secret was stored (%d rows)", n)
			}
		})
	}
}

func TestVoiceCannotRunACommand(t *testing.T) {
	h := newHarness(t)
	p := &scriptedProvider{turns: []*provider.ChatResponse{{Text: "Here is how things are."}}}
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	h.s.ButlerHost.Provider = p
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	h.hearing("/status")

	resp, out := h.sendVoice(t, runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "messageId": "v-1",
	}, oggClip, "audio/ogg")
	if resp.StatusCode != http.StatusAccepted || out.Reason != "" {
		t.Fatalf("status = %d, out = %+v: a spoken command must not be handled as one", resp.StatusCode, out)
	}
	if p.i != 1 {
		t.Fatalf("the Agent was asked %d times, want once: the words are a sentence for it", p.i)
	}
}

// A clip redelivered by the transport is claimed once and heard once.
func TestVoiceRedeliveryIsHeardOnce(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"with a message id":    {"transport": "telegram", "account": "12345", "messageId": "v-1"},
		"without a message id": {"transport": "telegram", "account": "12345"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.wireHouseholdButler([]*provider.ChatResponse{{Text: "first"}, {Text: "second"}}, runtimedb.ButlerModeFull)
			h.bindAccount(t, "family", "sam", "telegram", "12345")
			engine := h.hearing("hello")
			for range 2 {
				if resp, _ := h.sendVoice(t, runtimedb.RoleConcierge, meta, oggClip, "audio/ogg"); resp.StatusCode != http.StatusAccepted {
					t.Fatalf("status = %d", resp.StatusCode)
				}
			}
			if engine.calls.Load() != 1 {
				t.Fatalf("engine calls = %d, want 1", engine.calls.Load())
			}
			if q := h.poll(t, runtimedb.RoleConcierge, ""); len(q) != 1 {
				t.Fatalf("queued = %+v, want one answer", q)
			}
		})
	}
}

// Every way a recording can go unheard ends in a reply that says what
// to do instead, and none of them reaches the Agent.
func TestVoiceFailuresSayPleaseType(t *testing.T) {
	cases := map[string]struct {
		router *speech.Router
		mime   string
		secs   float64
		want   string
	}{
		"voice off":   {nil, "audio/ogg", 3, "aren't turned on"},
		"too long":    {&speech.Router{Mode: speech.ModeLocal, Local: &fakeEngine{where: speech.Local, text: "x"}, MaxSeconds: 60}, "audio/ogg", 61, "under 1 minute"},
		"unsupported": {&speech.Router{Mode: speech.ModeLocal, Local: &fakeEngine{where: speech.Local, text: "x"}}, "image/png", 3, "couldn't open"},
		"silence":     {&speech.Router{Mode: speech.ModeLocal, Local: &fakeEngine{where: speech.Local, err: speech.ErrEmpty}}, "audio/ogg", 3, "couldn't make out"},
		"warming":     {&speech.Router{Mode: speech.ModeLocal, Local: &fakeEngine{where: speech.Local, err: speech.ErrWarming}}, "audio/ogg", 3, "still getting ready"},
		"engine down": {&speech.Router{Mode: speech.ModeLocal, Local: &fakeEngine{where: speech.Local, err: context.DeadlineExceeded}}, "audio/ogg", 3, "try again in a moment"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			p := &scriptedProvider{turns: []*provider.ChatResponse{{Text: "should not run"}}}
			h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
			h.s.ButlerHost.Provider = p
			h.bindAccount(t, "family", "sam", "telegram", "12345")
			h.s.Speech = c.router

			resp, out := h.sendVoice(t, runtimedb.RoleConcierge, map[string]any{
				"transport": "telegram", "account": "12345", "messageId": "v-1", "audioSeconds": c.secs,
			}, oggClip, c.mime)
			if resp.StatusCode != http.StatusAccepted || !strings.HasPrefix(out.Reason, "voice message not transcribed") {
				t.Fatalf("status = %d, out = %+v", resp.StatusCode, out)
			}
			q := h.poll(t, runtimedb.RoleConcierge, "")
			if len(q) != 1 || !strings.Contains(q[0].Body, c.want) || !strings.Contains(q[0].Body, "type") {
				t.Fatalf("reply = %+v, want it to contain %q and suggest typing", q, c.want)
			}
			if p.i != 0 {
				t.Fatal("an unheard recording reached the Agent")
			}
		})
	}
}

// AC12: an Agent that may not send out of the house never has its audio
// sent to a cloud engine, even when the house has only a cloud one.
func TestVoiceForALocalAgentNeverReachesTheCloud(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler([]*provider.ChatResponse{{Text: "should not run"}}, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	cloud := &fakeEngine{where: speech.Cloud, text: "hello"}
	h.s.Speech = &speech.Router{Mode: speech.ModeCloud, Cloud: cloud}

	h.sendVoice(t, runtimedb.RoleConcierge, map[string]any{
		"transport": "telegram", "account": "12345", "messageId": "v-1",
	}, oggClip, "audio/ogg")
	if cloud.calls.Load() != 0 {
		t.Fatal("a local-only Agent's recording was sent to the cloud")
	}
	q := h.poll(t, runtimedb.RoleConcierge, "")
	if len(q) != 1 || !strings.Contains(q[0].Body, "can't listen") {
		t.Fatalf("reply = %+v", q)
	}
}

func TestAgentSendsOutOfHouse(t *testing.T) {
	h := newHarness(t)
	h.agentAt(t, "family", "telegram") // "sandy", on a local model
	if h.s.agentSendsOutOfHouse("sandy") {
		t.Fatal("an Agent on a local model may not send audio out")
	}
	if h.s.agentSendsOutOfHouse("nobody") {
		t.Fatal("an unknown Agent answers no")
	}

	if err := h.s.DB.UpsertModel(runtimedb.Model{
		Name: "sky", Kind: runtimedb.ModelCloud, Provider: "openai",
		ProviderModel: "gpt", Readiness: runtimedb.ModelReady, Enabled: true,
	}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	seq, err := h.s.DB.CreateButler(runtimedb.Butler{
		Name: "cirrus", Directive: "help", Surface: runtimedb.ButlerSurfaceFamily,
		AgentClass: runtimedb.AgentClassMember, Model: "sky",
	}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	if !h.s.agentSendsOutOfHouse("cirrus") {
		t.Fatal("a native Agent on a cloud model already sends out of the house")
	}

	// An external runtime's class is the Owner's declaration, and an
	// undeclared one is treated as local.
	for class, want := range map[agentapi.ProcessingClass]bool{
		"":                           false,
		agentapi.ProcessingLocalOnly: false,
		agentapi.ProcessingCloud:     true,
	} {
		if err := h.s.DB.SetButlerRuntime(seq, runtimedb.ButlerRuntime{
			Type: agentapi.RuntimeHermes, Delivery: agentapi.DeliveryPull, ProcessingClass: class,
		}, "root-admin"); err != nil {
			t.Fatal(err)
		}
		if got := h.s.agentSendsOutOfHouse("cirrus"); got != want {
			t.Errorf("hermes declared %q: got %v, want %v", class, got, want)
		}
	}
}

func TestVoiceUploadIsValidated(t *testing.T) {
	h := newHarness(t)
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	h.bindAccount(t, "family", "sam", "telegram", "12345")
	h.hearing("x")
	meta := map[string]any{"transport": "telegram", "account": "12345"}

	// Not multipart at all.
	if resp, _ := h.postVoice(t, runtimedb.RoleConcierge, "application/json", bytes.NewBufferString(`{}`)); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("json body: status = %d, want 415", resp.StatusCode)
	}
	// No audio part.
	if resp, _ := h.sendVoice(t, runtimedb.RoleConcierge, meta, nil, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no audio: status = %d, want 400", resp.StatusCode)
	}
	// Text AND audio: which one did they mean?
	withText := map[string]any{"transport": "telegram", "account": "12345", "text": "hi"}
	if resp, _ := h.sendVoice(t, runtimedb.RoleConcierge, withText, oggClip, "audio/ogg"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("text with audio: status = %d, want 400", resp.StatusCode)
	}
	// Over the size bound: refused before it is all read.
	big := bytes.Repeat([]byte{1}, speech.MaxAudioBytes+1)
	if resp, _ := h.sendVoice(t, runtimedb.RoleConcierge, meta, big, "audio/ogg"); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: status = %d, want 413", resp.StatusCode)
	}
	// An adapter still cannot pick its Agent, by voice or by text.
	named := map[string]any{"transport": "telegram", "account": "12345", "butler": "alfred"}
	if resp, _ := h.sendVoice(t, runtimedb.RoleConcierge, named, oggClip, "audio/ogg"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("adapter naming a butler: status = %d, want 403", resp.StatusCode)
	}
}

func TestHeardLine(t *testing.T) {
	if got := heardLine("  turn   on\nthe lights "); got != "🎙️ I heard: “turn on the lights”\n\n" {
		t.Errorf("heardLine = %q", got)
	}
	if got := heardLine("my password is hunter2"); strings.Contains(got, "hunter2") {
		t.Errorf("heardLine repeated a secret: %q", got)
	}
	long := strings.Repeat("é", 400)
	if got := heardLine(long); !strings.HasSuffix(got, "…”\n\n") || strings.Count(got, "é") != 300 {
		t.Errorf("a long transcript is not clipped on a character boundary: %d runes", len([]rune(got)))
	}
}
