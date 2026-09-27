package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerstore"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

// consoleDictation is a harness with named Butlers on: sandy (family)
// and alfred (admin), both on a local model.
func consoleDictation(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.person("op")
	h.agentAt(t, "family", "telegram")
	h.agentAt(t, "admin", "telegram")
	lease, err := h.s.DB.AcquireCoordinator("console-dictation-test")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Butlers = &butlerlive.Butlers{DB: h.s.DB, Ledger: butlerstore.New(h.s.DB, lease)}
	return h
}

// consoleDictate posts a recording the way the Console does: the bytes
// as the body, typed by their Content-Type.
func (h *harness) consoleDictate(t *testing.T, role runtimedb.Role, butler string, audio []byte, mimeType string) (int, map[string]string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/butlers/"+butler+"/transcribe?seconds=2.5", bytes.NewReader(audio))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.tokens[role])
	req.Header.Set("Content-Type", mimeType)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]string{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// The Console holder dictates to any live Butler, and the Butler is
// asked nothing: the words are for the text box.
func TestConsoleDictationFillsTheTextBox(t *testing.T) {
	h := consoleDictation(t)
	engine := h.hearing("how is the disk?")
	for _, butler := range []string{"alfred", "sandy"} {
		status, out := h.consoleDictate(t, runtimedb.RoleAdmin, butler, webmClip, "audio/webm;codecs=opus")
		if status != http.StatusOK || out["text"] != "how is the disk?" || out["engine"] != "local" {
			t.Fatalf("%s: status = %d, out = %v", butler, status, out)
		}
	}
	if engine.calls.Load() != 2 {
		t.Fatalf("engine calls = %d, want 2", engine.calls.Load())
	}
	var sessions []runtimedb.ButlerSession
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/butler/sessions", nil, &sessions)
	if len(sessions) != 0 {
		t.Fatalf("dictation opened a session: %+v", sessions)
	}
}

// Somebody who is not the owner reaches only the Butlers they were
// granted, exactly as asking does; the recording is not heard first.
func TestConsoleDictationKeepsButlerAccess(t *testing.T) {
	h := consoleDictation(t)
	engine := h.hearing("x")
	if status, _ := h.consoleDictate(t, runtimedb.RoleOperator, "sandy", webmClip, "audio/webm"); status != http.StatusNotFound {
		t.Fatalf("ungranted: status = %d, want 404", status)
	}
	if status, _ := h.consoleDictate(t, runtimedb.RoleAdmin, "nobody", webmClip, "audio/webm"); status != http.StatusNotFound {
		t.Fatalf("unknown butler: status = %d, want 404", status)
	}
	if engine.calls.Load() != 0 {
		t.Fatal("a recording for a Butler the caller cannot reach was transcribed")
	}
	if err := h.s.DB.GrantButlerAccess("sandy", "op", "root-admin"); err != nil {
		t.Fatal(err)
	}
	if status, out := h.consoleDictate(t, runtimedb.RoleOperator, "sandy", webmClip, "audio/webm"); status != http.StatusOK {
		t.Fatalf("granted: status = %d, out = %v", status, out)
	}
}

// AC12: sandy and alfred run on a local model, so a cloud-only house
// neither offers the mic nor sends their audio out.
func TestConsoleDictationForALocalButlerNeverReachesTheCloud(t *testing.T) {
	h := consoleDictation(t)
	cloud := &fakeEngine{where: speech.Cloud, text: "x"}
	h.s.Speech = &speech.Router{Mode: speech.ModeCloud, Cloud: cloud}
	if status, out := h.consoleDictate(t, runtimedb.RoleAdmin, "alfred", webmClip, "audio/webm"); status != http.StatusServiceUnavailable || out["error"] == "" {
		t.Fatalf("status = %d, out = %v", status, out)
	}
	if cloud.calls.Load() != 0 {
		t.Fatal("a local Butler's dictation was sent to the cloud")
	}
	var st map[string]any
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/butlers/alfred/status", nil, &st)
	if st["dictation"] == true {
		t.Fatalf("status offers the mic for a Butler it cannot serve: %v", st)
	}
}

func TestConsoleButlerStatusSaysWhenTheMicWorks(t *testing.T) {
	h := consoleDictation(t)
	var st map[string]any
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/butlers/alfred/status", nil, &st)
	if _, ok := st["dictation"]; ok {
		t.Fatalf("voice off: status = %v, want no mic", st)
	}
	h.hearing("x")
	st = nil
	h.do(runtimedb.RoleAdmin, "GET", "/api/v1/butlers/alfred/status", nil, &st)
	if st["dictation"] != true || st["dictationMaxSeconds"] != float64(speech.DefaultMaxSeconds) {
		t.Fatalf("local engine on: status = %v", st)
	}
}

func TestConsoleDictationRefusesOversizedRecordings(t *testing.T) {
	h := consoleDictation(t)
	engine := h.hearing("x")
	big := bytes.Repeat([]byte{1}, speech.MaxAudioBytes+1)
	if status, out := h.consoleDictate(t, runtimedb.RoleAdmin, "alfred", big, "audio/webm"); status != http.StatusRequestEntityTooLarge || out["error"] == "" {
		t.Fatalf("status = %d, out = %v", status, out)
	}
	if engine.calls.Load() != 0 {
		t.Fatal("an oversized recording reached the engine")
	}
}
