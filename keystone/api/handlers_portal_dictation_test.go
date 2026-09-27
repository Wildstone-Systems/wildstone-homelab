package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

// dictationHousehold is a portal with two assistants granted to sam: a
// household one on a local model, and an operator one the dashboard may
// never reach.
func dictationHousehold(t *testing.T) *harness {
	t.Helper()
	h, _ := portalHarness(t)
	if err := h.s.DB.UpsertModel(runtimedb.Model{Name: "kitchen-box", Kind: runtimedb.ModelLocal,
		Provider: "ollama", ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.RecordButlerMember("sam", runtimedb.RoleMember, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.AdmitButlerMember("sam", "root-admin"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []runtimedb.Butler{
		{Name: "sandy", Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box"},
		{Name: "operator", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "kitchen-box"},
	} {
		if _, err := h.s.DB.CreateButler(b, "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.GrantButlerAccess(b.Name, "sam", "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *harness) dictate(t *testing.T, user, agent string, audio []byte, mimeType string) (int, map[string]string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("agent", agent)
	w.WriteField("seconds", "2.5")
	part, _ := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="audio"; filename="dictation.webm"`},
		"Content-Type":        {mimeType},
	})
	part.Write(audio)
	w.Close()
	res := h.member(http.MethodPost, "/api/v1/assistant/transcribe", user, "assistant_users", &body, w.FormDataContentType())
	defer res.Body.Close()
	out := map[string]string{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

var webmClip = []byte("\x1aE\xdf\xa3 webm opus")

// The words come back for the text box, and nothing is asked of the
// assistant: dictation is sound to text, not a turn.
func TestDictationFillsTheTextBoxAndAsksNothing(t *testing.T) {
	h := dictationHousehold(t)
	p := &scriptedProvider{}
	h.wireHouseholdButler(nil, runtimedb.ButlerModeFull)
	h.s.ButlerHost.Provider = p
	engine := h.hearing("add milk to the shopping list")

	status, out := h.dictate(t, "sam", "sandy", webmClip, "audio/webm;codecs=opus")
	if status != http.StatusOK || out["text"] != "add milk to the shopping list" || out["engine"] != "local" {
		t.Fatalf("status = %d, out = %v", status, out)
	}
	if engine.calls.Load() != 1 {
		t.Fatalf("engine calls = %d", engine.calls.Load())
	}
	if p.i != 0 {
		t.Fatal("dictation asked the assistant something")
	}
	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 0 {
		t.Fatalf("dictation opened a session: %+v", sessions)
	}
}

// The same ceiling as asking: an assistant the dashboard may not reach
// is not found, and the recording is never heard.
func TestDictationKeepsTheAssistantCeiling(t *testing.T) {
	h := dictationHousehold(t)
	engine := h.hearing("x")
	for _, agent := range []string{"operator", "nobody"} {
		if status, _ := h.dictate(t, "sam", agent, webmClip, "audio/webm"); status != http.StatusNotFound {
			t.Errorf("dictating to %q = %d, want 404", agent, status)
		}
	}
	if engine.calls.Load() != 0 {
		t.Fatal("a recording for an assistant the dashboard cannot reach was transcribed")
	}
}

// AC12: sandy runs on a local model, so with only a cloud engine the
// mic is not offered and a hand-made request is refused unheard.
func TestDictationForALocalAssistantNeverReachesTheCloud(t *testing.T) {
	h := dictationHousehold(t)
	cloud := &fakeEngine{where: speech.Cloud, text: "x"}
	h.s.Speech = &speech.Router{Mode: speech.ModeCloud, Cloud: cloud}

	status, out := h.dictate(t, "sam", "sandy", webmClip, "audio/webm")
	if status != http.StatusServiceUnavailable || out["error"] == "" {
		t.Fatalf("status = %d, out = %v", status, out)
	}
	if cloud.calls.Load() != 0 {
		t.Fatal("a local assistant's dictation was sent to the cloud")
	}
	if agents := h.rosterFor(t, "sam"); len(agents) != 1 || agents[0].Dictation {
		t.Fatalf("roster = %+v, want the mic hidden for sandy", agents)
	}
}

func (h *harness) rosterFor(t *testing.T, user string) []assistantAgent {
	t.Helper()
	res := h.member(http.MethodGet, "/api/v1/assistant/agents", user, "assistant_users", nil, "")
	defer res.Body.Close()
	var body struct {
		Agents []assistantAgent `json:"agents"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Agents
}

func TestTheRosterSaysWhenTheMicWorks(t *testing.T) {
	h := dictationHousehold(t)
	if agents := h.rosterFor(t, "sam"); len(agents) != 1 || agents[0].Dictation {
		t.Fatalf("voice off: roster = %+v, want no mic", agents)
	}
	h.hearing("x")
	if agents := h.rosterFor(t, "sam"); len(agents) != 1 || !agents[0].Dictation {
		t.Fatalf("local engine on: roster = %+v, want the mic", agents)
	}
}

func TestDictationErrorsAreForAPerson(t *testing.T) {
	h := dictationHousehold(t)
	cases := map[string]struct {
		router *speech.Router
		mime   string
		want   int
	}{
		"off":         {nil, "audio/webm", http.StatusServiceUnavailable},
		"silence":     {&speech.Router{Mode: speech.ModeLocal, Local: &fakeEngine{where: speech.Local, err: speech.ErrEmpty}}, "audio/webm", http.StatusUnprocessableEntity},
		"unsupported": {&speech.Router{Mode: speech.ModeLocal, Local: &fakeEngine{where: speech.Local, text: "x"}}, "image/png", http.StatusUnsupportedMediaType},
	}
	for name, c := range cases {
		h.s.Speech = c.router
		status, out := h.dictate(t, "sam", "sandy", webmClip, c.mime)
		if status != c.want || out["error"] == "" {
			t.Errorf("%s: status = %d (%v), want %d with a message", name, status, out, c.want)
		}
	}
}

// A write from another site is refused before anything is heard: the
// portal's same-site rule covers this door like every other.
func TestDictationRefusesCrossSiteWrites(t *testing.T) {
	h := dictationHousehold(t)
	engine := h.hearing("x")
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("agent", "sandy")
	part, _ := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="audio"; filename="a.webm"`},
		"Content-Type":        {"audio/webm"},
	})
	part.Write(webmClip)
	w.Close()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/assistant/transcribe", &body)
	req.Header.Set(TrustedIngressHeader, "sam")
	req.Header.Set(ConsoleGroupsHeader, "assistant_users")
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || engine.calls.Load() != 0 {
		t.Fatalf("cross-site dictation: status = %d, engine calls = %d", res.StatusCode, engine.calls.Load())
	}
}
