package api

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

// speaches is a stand-in Speaches container on 127.0.0.1: it has no
// model until asked to download one, then transcribes everything as
// text.
type speaches struct {
	text       string
	downloaded atomic.Bool
	heard      atomic.Int32
}

func (f *speaches) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/models/"):
		f.downloaded.Store(true)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/audio/transcriptions":
		io.Copy(io.Discard, r.Body)
		if !f.downloaded.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.heard.Add(1)
		io.WriteString(w, `{"text":"`+f.text+`"}`)
	default:
		http.NotFound(w, r)
	}
}

// startSpeaches serves the stand-in on a loopback port of its own and
// returns that port, which is never the package's default.
func startSpeaches(t *testing.T, f *speaches) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if port == "18000" {
		t.Skip("the stand-in landed on the default port, which proves nothing")
	}
	return port
}

// speechHousehold is dictationHousehold running the shipped catalog on
// node n1, Keystone's own, with voice left to the installed engine: no
// -speech-mode, no -speech-url.
func speechHousehold(t *testing.T, r *speech.Router) *harness {
	t.Helper()
	h := dictationHousehold(t)
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Catalog = c
	for _, n := range []string{"n1", "n2"} {
		if err := h.s.DB.RecordNodeJoin(n); err != nil {
			t.Fatal(err)
		}
	}
	h.s.NodeID = "n1"
	h.s.Speech = r
	return h
}

func (h *harness) speechPackage(t *testing.T) packageView {
	t.Helper()
	var all []packageView
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/packages", nil, &all); res.StatusCode != http.StatusOK {
		t.Fatalf("listing packages = %d", res.StatusCode)
	}
	for _, p := range all {
		if p.Name == speechEnginePackage {
			return p
		}
	}
	t.Fatal("speech-speaches is not in the catalog")
	return packageView{}
}

// #408: installing speech-speaches on Keystone's node, on a port that is
// not the default, is all it takes. The mic appears, the model is
// fetched and reported, and dictation reaches the engine on that port.
func TestInstallingTheSpeechEngineTurnsDictationOn(t *testing.T) {
	engine := &speaches{text: "add eggs"}
	port := startSpeaches(t, engine)
	h := speechHousehold(t, &speech.Router{AutoLocal: true})

	if agents := h.rosterFor(t, "sam"); len(agents) != 1 || agents[0].Dictation {
		t.Fatalf("before the install: roster = %+v, want no mic", agents)
	}

	installCapability(t, h, speechEnginePackage, map[string]any{"node": "n1", "port": port})

	if agents := h.rosterFor(t, "sam"); len(agents) != 1 || !agents[0].Dictation {
		t.Fatalf("after the install: roster = %+v, want the mic", agents)
	}
	st := h.s.Speech.Status()
	if st.Mode != speech.ModeLocal || st.Source != speech.SourcePackage || st.URL != "http://127.0.0.1:"+port+"/v1" {
		t.Fatalf("status = %+v", st)
	}
	// Local only: installing a local engine turns on nothing that
	// leaves the house, and does not start reading replies aloud.
	if h.s.Speech.Cloud != nil || h.s.Speech.SpeaksAtAll() {
		t.Fatal("installing the local engine turned on the cloud or spoken replies")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		p := h.speechPackage(t)
		if len(p.Blockers) != 0 {
			t.Fatalf("blockers = %v", p.Blockers)
		}
		if p.Speech != nil && p.Speech.State == speech.ModelReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the model never became ready: %+v", p.Speech)
		}
		time.Sleep(20 * time.Millisecond)
	}

	status, out := h.dictate(t, "sam", "sandy", webmClip, "audio/webm")
	if status != http.StatusOK || out["text"] != "add eggs" || out["engine"] != "local" {
		t.Fatalf("dictation = %d %v", status, out)
	}
	if engine.heard.Load() != 1 {
		t.Fatalf("the engine heard %d recordings, want 1", engine.heard.Load())
	}
}

// Removing the engine takes the mic away again.
func TestRemovingTheSpeechEngineTurnsDictationOff(t *testing.T) {
	port := startSpeaches(t, &speaches{text: "x"})
	h := speechHousehold(t, &speech.Router{AutoLocal: true})
	installCapability(t, h, speechEnginePackage, map[string]any{"node": "n1", "port": port})

	var proposed map[string]any
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/capabilities/disable",
		map[string]any{"package": speechEnginePackage}, &proposed); res.StatusCode != http.StatusCreated {
		t.Fatalf("proposing removal = %d %v", res.StatusCode, proposed)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/plans/"+proposed["id"].(string)+"/approve", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("approving removal = %d", res.StatusCode)
	}
	if agents := h.rosterFor(t, "sam"); len(agents) != 1 || agents[0].Dictation {
		t.Fatalf("after removal: roster = %+v, want no mic", agents)
	}
}

// An engine on another machine could never be reached over loopback,
// so the install is refused with where to put it instead.
func TestTheSpeechEngineMustBeOnKeystonesNode(t *testing.T) {
	h := speechHousehold(t, &speech.Router{AutoLocal: true})
	var out map[string]any
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/capabilities",
		map[string]any{"package": speechEnginePackage, "params": map[string]any{"node": "n2"}}, &out)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("installing on n2 = %d %v, want 400", res.StatusCode, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "node n1") {
		t.Fatalf("error = %q, want it to name Keystone's node", msg)
	}
}

// When Keystone cannot tell which node it is on, nothing is refused on
// a guess.
func TestAnUnknownKeystoneNodeRefusesNothing(t *testing.T) {
	port := startSpeaches(t, &speaches{text: "x"})
	h := speechHousehold(t, &speech.Router{AutoLocal: true})
	h.s.NodeID = "not-a-joined-node"
	installCapability(t, h, speechEnginePackage, map[string]any{"node": "n2", "port": port})
	if !h.s.Speech.Enabled() {
		t.Fatal("dictation stayed off")
	}
}

// Choices the operator made are kept, and the Services page says what
// to change rather than leaving the mic silently missing.
func TestExplicitSpeechSettingsWin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		router  func() *speech.Router
		enabled bool
		blocker string
	}{
		{"mode off", func() *speech.Router { return &speech.Router{Mode: speech.ModeOff} }, false, "-speech-mode off"},
		{"mode cloud", func() *speech.Router { return &speech.Router{Mode: speech.ModeCloud} }, false, "-speech-mode cloud"},
		{"another url", func() *speech.Router {
			return &speech.Router{Mode: speech.ModeLocal, Local: &speech.Client{BaseURL: "http://127.0.0.1:1/v1", Where: speech.Local}}
		}, true, "-speech-url http://127.0.0.1:1/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := startSpeaches(t, &speaches{text: "x"})
			h := speechHousehold(t, tc.router())
			installCapability(t, h, speechEnginePackage, map[string]any{"node": "n1", "port": port})
			if got := h.s.Speech.Available(false); got != tc.enabled {
				t.Errorf("available = %v, want %v", got, tc.enabled)
			}
			if tc.enabled && h.s.Speech.Status().Source != speech.SourceFlag {
				t.Errorf("status = %+v, want the flag's engine kept", h.s.Speech.Status())
			}
			p := h.speechPackage(t)
			if len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0], tc.blocker) {
				t.Errorf("blockers = %v, want one naming %q", p.Blockers, tc.blocker)
			}
		})
	}
}

func TestFindSpeechEngineReadsThePublishedPort(t *testing.T) {
	// No Router: this reads desired state only, and nothing answers on
	// the port it names.
	h := speechHousehold(t, nil)
	installCapability(t, h, speechEnginePackage, map[string]any{"node": "n1", "port": "18123"})
	current, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := findSpeechEngine(current)
	if !ok || e.Node != "n1" || strconv.Itoa(int(e.Port)) != "18123" || e.URL() != "http://127.0.0.1:18123/v1" {
		t.Fatalf("engine = %+v, %v", e, ok)
	}
}
