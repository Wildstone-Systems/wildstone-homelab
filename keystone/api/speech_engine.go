package api

// The installed speech engine (#408). Installing speech-speaches turns
// local dictation on without an edit to KEYSTONE_ARGS: Keystone finds
// the engine's Container in desired state, checks it is on Keystone's
// own machine, and hands its loopback address to the speech Router,
// which uses it wherever the flags left the local engine open.

import (
	"fmt"
	"strconv"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
)

const (
	// speechEnginePackage is the package that provides a local engine.
	speechEnginePackage = "speech-speaches"
	// speechEngineContainer and speechEnginePort are the Container it
	// renders and the port Speaches listens on inside it.
	speechEngineContainer = "speaches"
	speechEnginePort      = 8000
)

// speechEngine is where an installed engine is and how to reach it.
type speechEngine struct {
	Node string
	Port uint32
}

// URL is the engine's API base as Keystone reaches it: over loopback,
// which is why the engine has to run on Keystone's own machine.
func (e speechEngine) URL() string {
	return "http://127.0.0.1:" + strconv.FormatUint(uint64(e.Port), 10) + "/v1"
}

// findSpeechEngine finds the installed engine in desired state. The
// port is read from the rendered Container rather than the install's
// params, because the plan records only the params the caller sent and
// a defaulted port is not among them.
func findSpeechEngine(current []resource.Resource) (speechEngine, bool) {
	for _, r := range current {
		if r.Kind != resource.KindContainer || r.Container == nil || r.Metadata.Name != speechEngineContainer {
			continue
		}
		if r.Metadata.Package != "" && r.Metadata.Package != speechEnginePackage {
			continue
		}
		for _, p := range r.Container.Ports {
			m, ok := resource.ParsePortMapping(p)
			if ok && m.Protocol == "tcp" && m.ContainerPort == speechEnginePort && m.HostPort != 0 {
				return speechEngine{Node: r.Metadata.Node, Port: m.HostPort}, true
			}
		}
	}
	return speechEngine{}, false
}

// speechEngineMisplaced says why an engine on node cannot be reached, or
// "" when it can or Keystone cannot tell. Keystone's own node is
// NodeID; the answer is only trusted when that node has joined, so a
// deployment whose node ids are not host names is never refused on a
// guess.
func (s *Server) speechEngineMisplaced(node string) string {
	if s.NodeID == "" || node == "" || node == s.NodeID || !s.nodeKnown(s.NodeID) {
		return ""
	}
	return fmt.Sprintf("The speech engine must run on Keystone's own machine (%s), because Keystone reaches it on 127.0.0.1 and a loopback port on %s is out of its reach. Install speech-speaches with node %s.",
		s.NodeID, node, s.NodeID)
}

func (s *Server) nodeKnown(id string) bool {
	nodes, err := s.DB.ListNodes()
	if err != nil {
		return false
	}
	for _, n := range nodes {
		if n.NodeID == id {
			return true
		}
	}
	return false
}

// RefreshSpeech brings the speech Router up to date with desired state.
// It runs at startup, after every approval, and on a timer, and costs a
// statefile read.
func (s *Server) RefreshSpeech() {
	if s.Speech == nil || s.State == nil {
		return
	}
	current, err := s.State.Load()
	if err != nil {
		s.Log.Warn("speech: reading desired state for the speech engine", "err", err)
		return
	}
	s.refreshSpeechFrom(current)
}

func (s *Server) refreshSpeechFrom(current []resource.Resource) {
	e, ok := findSpeechEngine(current)
	if !ok {
		s.Speech.SetInstalled("", s.Log)
		return
	}
	if why := s.speechEngineMisplaced(e.Node); why != "" {
		s.Log.Warn("speech: the installed speech engine is not used: " + why)
		s.Speech.SetInstalled("", s.Log)
		return
	}
	s.Speech.SetInstalled(e.URL(), s.Log)
}

// speechEngineBlockers are the reasons an installed engine does not
// give the household dictation: it is on the wrong machine, or a flag
// Keystone was started with overrides it. Each says what to change.
func (s *Server) speechEngineBlockers(current []resource.Resource) []string {
	e, ok := findSpeechEngine(current)
	if !ok {
		return nil
	}
	if why := s.speechEngineMisplaced(e.Node); why != "" {
		return []string{why}
	}
	if s.Speech == nil {
		return nil
	}
	var out []string
	// The flags, not the mode in effect: an engine the Router has not
	// been told about yet is not the operator's doing.
	mode, _ := speech.ParseMode(string(s.Speech.Mode))
	switch {
	case s.Speech.AutoLocal:
	case mode == speech.ModeOff:
		out = append(out, "Keystone was started with -speech-mode off, which keeps voice off whatever is installed. Remove -speech-mode from KEYSTONE_ARGS (or set it to local) and restart Keystone to use this engine.")
	case mode == speech.ModeCloud:
		out = append(out, "Keystone was started with -speech-mode cloud, so this local engine is not used. Set -speech-mode local (or local-then-cloud) in KEYSTONE_ARGS and restart Keystone to use it.")
	}
	if flag := s.Speech.FlagURL(); flag != "" && flag != e.URL() {
		out = append(out, fmt.Sprintf("Keystone was started with -speech-url %s, but this engine answers at %s. Remove -speech-url from KEYSTONE_ARGS (the installed engine is found on its own), or make the two agree, and restart Keystone.", flag, e.URL()))
	}
	return out
}

// speechEngineStatus is what the Services page shows about an installed
// engine's model, or nil when this is not the engine or Keystone does
// not use it.
func (s *Server) speechEngineStatus(pkg *pkgspec.Package, current []resource.Resource) *speech.Status {
	if pkg.Metadata.Name != speechEnginePackage || s.Speech == nil {
		return nil
	}
	e, ok := findSpeechEngine(current)
	if !ok {
		return nil
	}
	st := s.Speech.Status()
	if st.URL != e.URL() {
		return nil
	}
	return &st
}

// refuseMisplacedSpeechEngine refuses an install that could never work,
// before anything is proposed.
func (s *Server) refuseMisplacedSpeechEngine(pkg *pkgspec.Package, rendered []resource.Resource) error {
	if pkg.Metadata.Name != speechEnginePackage {
		return nil
	}
	e, ok := findSpeechEngine(rendered)
	if !ok {
		return nil
	}
	if why := s.speechEngineMisplaced(e.Node); why != "" {
		return fmt.Errorf("%s", why)
	}
	return nil
}
