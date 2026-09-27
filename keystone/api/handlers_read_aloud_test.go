package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

// voicing turns reading aloud on with one voice, and nothing else.
func (h *harness) voicing(where speech.Processing) *fakeVoice {
	h.s.Speech = &speech.Router{}
	return h.speaking(where)
}

func (h *harness) listen(t *testing.T, user, agent, text string) (int, string, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"agent": agent, "text": text})
	res := h.member(http.MethodPost, "/api/v1/assistant/speak", user, "assistant_users", bytes.NewReader(body), "application/json")
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), raw
}

// The listen button: the reply comes back as audio, read by the voice
// the assistant's privacy class allows, and the event has no words.
func TestTheDashboardReadsAReplyAloud(t *testing.T) {
	h := dictationHousehold(t)
	voice := h.voicing(speech.Local)
	status, ct, audio := h.listen(t, "sam", "sandy", "The **lights** are off.")
	if status != http.StatusOK || ct != speech.SpokenMIME || string(audio) != "ID3 spoken local" {
		t.Fatalf("listen = %d %q %q", status, ct, audio)
	}
	if said, _ := voice.said.Load().(string); said != "The lights are off." {
		t.Fatalf("voice read %q", said)
	}
	events, _ := h.s.DB.EventsAfter(0, 200)
	var found bool
	for _, e := range events {
		if e.Type == "assistant.read_aloud" {
			found = true
			if e.Attrs["outcome"] != "spoken" || e.Attrs["engine"] != "local" || e.Attrs["member"] != "sam" {
				t.Errorf("event = %+v", e)
			}
			for _, v := range e.Attrs {
				if v == "The **lights** are off." {
					t.Errorf("the event carries the words: %+v", e)
				}
			}
		}
	}
	if !found {
		t.Fatal("no assistant.read_aloud event")
	}
	if agents := h.rosterFor(t, "sam"); len(agents) != 1 || !agents[0].ReadAloud {
		t.Fatalf("roster = %+v, want the listen button", agents)
	}
}

// The same ceiling as asking: an Owner Agent is never read from the
// dashboard, and a stranger gets nothing.
func TestTheDashboardReadsOnlyAssistantsItReaches(t *testing.T) {
	h := dictationHousehold(t)
	voice := h.voicing(speech.Local)
	for _, tc := range []struct{ user, agent string }{{"sam", "operator"}, {"giulia", "sandy"}, {"sam", "nobody"}} {
		if status, _, _ := h.listen(t, tc.user, tc.agent, "hello"); status != http.StatusNotFound {
			t.Errorf("%s → %s = %d, want 404", tc.user, tc.agent, status)
		}
	}
	if voice.calls.Load() != 0 {
		t.Fatal("a refused request reached the voice")
	}
}

// AC12: a local assistant's reply is never read by a cloud voice, and
// its listen button is hidden when only a cloud voice exists.
func TestReadAloudForALocalAssistantNeverReachesTheCloud(t *testing.T) {
	h := dictationHousehold(t)
	cloud := h.voicing(speech.Cloud)
	if status, _, _ := h.listen(t, "sam", "sandy", "the alarm code is 1234"); status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if cloud.calls.Load() != 0 {
		t.Fatal("a local assistant's reply went to the cloud voice")
	}
	if agents := h.rosterFor(t, "sam"); len(agents) != 1 || agents[0].ReadAloud {
		t.Fatalf("roster = %+v, want no listen button", agents)
	}
}

func TestReadAloudOffByDefault(t *testing.T) {
	h := dictationHousehold(t)
	if status, _, _ := h.listen(t, "sam", "sandy", "hi"); status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if agents := h.rosterFor(t, "sam"); agents[0].ReadAloud {
		t.Fatal("the listen button shows with reading aloud off")
	}
}

func (h *harness) consoleListen(t *testing.T, role runtimedb.Role, butler, text string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/butlers/"+butler+"/speak", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.tokens[role])
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

// The Console's listen button, with the Console dictation's access.
func TestTheConsoleReadsAReplyAloud(t *testing.T) {
	h := consoleDictation(t)
	h.voicing(speech.Local)
	for _, butler := range []string{"alfred", "sandy"} {
		if status, audio := h.consoleListen(t, runtimedb.RoleAdmin, butler, "Disk is fine."); status != http.StatusOK || string(audio) != "ID3 spoken local" {
			t.Fatalf("%s: %d %q", butler, status, audio)
		}
	}
	if status, _ := h.consoleListen(t, runtimedb.RoleAdmin, "nobody", "x"); status != http.StatusNotFound {
		t.Fatalf("unknown butler = %d", status)
	}
	if status, _ := h.consoleListen(t, runtimedb.RoleAdmin, "alfred", "  "); status != http.StatusBadRequest {
		t.Fatalf("empty text = %d", status)
	}
	var st map[string]any
	h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/butlers/alfred/status", nil, &st)
	if st["readAloud"] != true {
		t.Fatalf("status = %v, want readAloud", st)
	}
}

// An assistant whose model is down still reads its earlier replies
// aloud and still takes dictation: voice does not need the model.
func TestVoiceWorksWhileTheAssistantsModelIsDown(t *testing.T) {
	h := dictationHousehold(t)
	if err := h.s.DB.UpsertModel(runtimedb.Model{Name: "kitchen-box", Kind: runtimedb.ModelLocal,
		Provider: "ollama", ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: false}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.ResolveButlerFor("sandy", "sam"); !errors.Is(err, runtimedb.ErrButlerUnreachable) {
		t.Fatalf("setup: resolve = %v, want the model down", err)
	}
	h.voicing(speech.Local)
	h.s.Speech.Mode, h.s.Speech.Local = speech.ModeLocal, &fakeEngine{where: speech.Local, text: "hello"}
	if status, _, audio := h.listen(t, "sam", "sandy", "Earlier reply."); status != http.StatusOK || len(audio) == 0 {
		t.Fatalf("listen = %d", status)
	}
	if status, out := h.dictate(t, "sam", "sandy", webmClip, "audio/webm"); status != http.StatusOK || out["text"] != "hello" {
		t.Fatalf("dictate = %d %v", status, out)
	}
	// Still only the dashboard's assistants: the operator one is refused
	// whether or not its model answers.
	if status, _, _ := h.listen(t, "sam", "operator", "x"); status != http.StatusNotFound {
		t.Fatalf("operator = %d", status)
	}
}
