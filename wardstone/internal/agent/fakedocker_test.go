package agent

// fakeDocker is a minimal in-memory Docker Engine API for tests: just the
// endpoints the agent's docker client touches, with the same status-code
// semantics.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type fakeContainer struct {
	id      string
	image   string
	labels  map[string]string
	env     []string
	running bool
}

type fakeVolume struct {
	name   string
	labels map[string]string
}

type fakeNetwork struct {
	name   string
	labels map[string]string
}

type fakeDocker struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	containers   map[string]*fakeContainer
	volumes      map[string]*fakeVolume
	networks     map[string]*fakeNetwork
	seq          int
	pruneFilters string // last filters query param /images/prune received
}

var containerPath = regexp.MustCompile(`^/containers/([^/]+)(/(json|start|stop|logs|stats))?$`)
var volumePath = regexp.MustCompile(`^/volumes/([^/]+)$`)
var networkPath = regexp.MustCompile(`^/networks/([^/]+)$`)

func newFakeDocker(t *testing.T) *fakeDocker {
	f := &fakeDocker{t: t, containers: map[string]*fakeContainer{}, volumes: map[string]*fakeVolume{}, networks: map[string]*fakeNetwork{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDocker) URL() string { return f.srv.URL }

func (f *fakeDocker) addForeign(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.containers[name] = &fakeContainer{
		id: fmt.Sprintf("foreign-%d", f.seq), image: "someone-elses:latest",
		labels: map[string]string{}, running: true,
	}
}

// addForeignVolume seeds a pre-existing volume without the wildstone owner
// label, simulating one created outside Wardstone's control.
func (f *fakeDocker) addForeignVolume(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[name] = &fakeVolume{name: name, labels: map[string]string{"someone-else": "true"}}
}

// addForeignNetwork seeds a pre-existing network without the wildstone
// owner label, simulating one created outside Wardstone's control.
func (f *fakeDocker) addForeignNetwork(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[name] = &fakeNetwork{name: name, labels: map[string]string{"someone-else": "true"}}
}

func (f *fakeDocker) lookup(key string) (*fakeContainer, string) {
	if c, ok := f.containers[key]; ok {
		return c, key
	}
	for name, c := range f.containers {
		if c.id == key {
			return c, name
		}
	}
	return nil, ""
}

func (f *fakeDocker) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/containers/create" && r.Method == http.MethodPost {
		name := r.URL.Query().Get("name")
		if _, exists := f.containers[name]; exists {
			http.Error(w, `{"message":"conflict"}`, http.StatusConflict)
			return
		}
		var body struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
			Env    []string          `json:"Env"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.seq++
		c := &fakeContainer{id: fmt.Sprintf("cid-%d", f.seq), image: body.Image, labels: body.Labels, env: body.Env}
		f.containers[name] = c
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"Id": c.id})
		return
	}

	if r.URL.Path == "/images/create" && r.Method == http.MethodPost {
		ref := r.URL.Query().Get("fromImage")
		if !strings.Contains(ref, "@sha256:") {
			// The agent must never send un-pinned refs; fail loudly if it does.
			http.Error(w, `{"message":"fake docker: refusing un-pinned pull"}`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.URL.Path == "/volumes/create" && r.Method == http.MethodPost {
		var body struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Real Docker semantics: idempotent, does NOT overwrite labels of an
		// existing volume with the same name.
		v, exists := f.volumes[body.Name]
		if !exists {
			v = &fakeVolume{name: body.Name, labels: body.Labels}
			f.volumes[body.Name] = v
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"Name": v.name, "Labels": v.labels})
		return
	}

	if vm := volumePath.FindStringSubmatch(r.URL.Path); vm != nil && r.Method == http.MethodGet {
		v, ok := f.volumes[vm[1]]
		if !ok {
			http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"Name": v.name, "Labels": v.labels})
		return
	}

	if r.URL.Path == "/networks/create" && r.Method == http.MethodPost {
		var body struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n, exists := f.networks[body.Name]
		if !exists {
			n = &fakeNetwork{name: body.Name, labels: body.Labels}
			f.networks[body.Name] = n
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"Id": "net-" + n.name, "Name": n.name})
		return
	}

	if nm := networkPath.FindStringSubmatch(r.URL.Path); nm != nil && r.Method == http.MethodGet {
		n, ok := f.networks[nm[1]]
		if !ok {
			http.Error(w, `{"message":"network not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"Name": n.name, "Labels": n.labels})
		return
	}

	if r.URL.Path == "/images/prune" && r.Method == http.MethodPost {
		f.pruneFilters = r.URL.Query().Get("filters")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ImagesDeleted": []map[string]string{
				{"Deleted": "sha256:x"},
				{"Deleted": "sha256:y"},
			},
			"SpaceReclaimed": 12345,
		})
		return
	}

	m := containerPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	c, name := f.lookup(m[1])
	if c == nil && !(m[3] == "" && r.Method == http.MethodDelete) {
		http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
		return
	}
	switch {
	case m[3] == "json":
		json.NewEncoder(w).Encode(map[string]any{
			"Id":    c.id,
			"State": map[string]any{"Running": c.running},
			"Config": map[string]any{
				"Image":  c.image,
				"Labels": c.labels,
			},
		})
	case m[3] == "start" && r.Method == http.MethodPost:
		if c.running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.running = true
		w.WriteHeader(http.StatusNoContent)
	case m[3] == "stop" && r.Method == http.MethodPost:
		if !c.running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.running = false
		w.WriteHeader(http.StatusNoContent)
	case m[3] == "logs":
		w.Write([]byte("fake log line 1\nfake log line 2\n"))
	case m[3] == "stats":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": 424242}},
			"memory_stats": map[string]any{"usage": 111111, "limit": 999999},
		})
	case m[3] == "" && r.Method == http.MethodDelete:
		if name == "" {
			http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
			return
		}
		delete(f.containers, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, `{"message":"unhandled by fake"}`, http.StatusNotImplemented)
	}
}
