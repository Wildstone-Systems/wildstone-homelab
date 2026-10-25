// Package kubeapitest is an in-memory API server for kubeapi's callers'
// tests. It keeps Leases and ConfigMaps with real resourceVersion
// compare-and-swap, maps bearer tokens to users, and lets a test decide
// what dry-run admission says. It is not Kubernetes; the fence itself is
// proven against a real API server (scripts/engine-fence-check.sh).
package kubeapitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
)

// Server is the fake. Use New, then Client for each identity.
type Server struct {
	*httptest.Server

	mu     sync.Mutex
	rv     int
	leases map[string]*kubeapi.Lease
	cms    map[string]*kubeapi.ConfigMap
	tokens map[string]string
	// Admit decides a dry-run ConfigMap create for user. nil admits all.
	Admit func(user string, cm *kubeapi.ConfigMap) error
	// Fail, when it returns an error, fails the request before anything
	// else: a down or unreachable server.
	Fail func(method, path string) error
	// Writes counts successful non-dry-run writes per user.
	Writes map[string]int
}

// New starts a fake server.
func New() *Server {
	s := &Server{
		leases: map[string]*kubeapi.Lease{},
		cms:    map[string]*kubeapi.ConfigMap{},
		tokens: map[string]string{},
		Writes: map[string]int{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Client returns a client authenticated as user.
func (s *Server) Client(user string) *kubeapi.Client {
	s.mu.Lock()
	tok := fmt.Sprintf("token-%d", len(s.tokens)+1)
	s.tokens[tok] = user
	s.mu.Unlock()
	c, err := kubeapi.NewWithHTTP(s.URL, func() (string, error) { return tok, nil }, s.Server.Client())
	if err != nil {
		panic(err)
	}
	return c
}

// PutConfigMap stores a ConfigMap directly, as an administrator would.
func (s *Server) PutConfigMap(cm kubeapi.ConfigMap) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rv++
	cm.Metadata.ResourceVersion = strconv.Itoa(s.rv)
	s.cms[cm.Metadata.Namespace+"/"+cm.Metadata.Name] = &cm
}

// ConfigMap returns a stored ConfigMap, or nil.
func (s *Server) ConfigMap(ns, name string) *kubeapi.ConfigMap {
	s.mu.Lock()
	defer s.mu.Unlock()
	cm := s.cms[ns+"/"+name]
	if cm == nil {
		return nil
	}
	c := *cm
	return &c
}

// Lease returns a stored Lease, or nil.
func (s *Server) Lease(ns, name string) *kubeapi.Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[ns+"/"+name]
	if l == nil {
		return nil
	}
	c := *l
	return &c
}

// PutLease stores a Lease directly, as another holder would.
func (s *Server) PutLease(l kubeapi.Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rv++
	l.Metadata.ResourceVersion = strconv.Itoa(s.rv)
	s.leases[l.Metadata.Namespace+"/"+l.Metadata.Name] = &l
}

func status(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(kubeapi.StatusError{Code: code, Reason: reason, Message: msg})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if s.Fail != nil {
		if err := s.Fail(r.Method, r.URL.Path); err != nil {
			status(w, http.StatusServiceUnavailable, "ServiceUnavailable", err.Error())
			return
		}
	}
	s.mu.Lock()
	user, ok := s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	s.mu.Unlock()
	if !ok {
		status(w, http.StatusUnauthorized, "Unauthorized", "unknown token")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// api/v1/namespaces/<ns>/configmaps[/<name>] or
	// apis/coordination.k8s.io/v1/namespaces/<ns>/leases[/<name>]
	var kind, ns, name string
	switch {
	case len(parts) >= 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" && parts[4] == "configmaps":
		kind, ns = "cm", parts[3]
		if len(parts) == 6 {
			name = parts[5]
		}
	case len(parts) >= 6 && parts[0] == "apis" && parts[1] == "coordination.k8s.io" && parts[3] == "namespaces" && parts[5] == "leases":
		kind, ns = "lease", parts[4]
		if len(parts) == 7 {
			name = parts[6]
		}
	default:
		status(w, http.StatusNotFound, "NotFound", "no such path")
		return
	}
	body, _ := io.ReadAll(r.Body)
	dry := r.URL.Query().Get("dryRun") == "All"

	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "cm" {
		s.serveCM(w, r.Method, user, ns, name, body, dry)
		return
	}
	s.serveLease(w, r.Method, user, ns, name, body)
}

func (s *Server) serveCM(w http.ResponseWriter, method, user, ns, name string, body []byte, dry bool) {
	key := ns + "/" + name
	switch method {
	case http.MethodGet:
		cm := s.cms[key]
		if cm == nil {
			status(w, http.StatusNotFound, "NotFound", "configmap not found")
			return
		}
		_ = json.NewEncoder(w).Encode(cm)
	case http.MethodPost:
		var cm kubeapi.ConfigMap
		if err := json.Unmarshal(body, &cm); err != nil {
			status(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		if !dry {
			status(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "the fake only dry-runs creates")
			return
		}
		if s.Admit != nil {
			if err := s.Admit(user, &cm); err != nil {
				status(w, http.StatusForbidden, "Forbidden", err.Error())
				return
			}
		}
		_ = json.NewEncoder(w).Encode(cm)
	case http.MethodPut:
		var cm kubeapi.ConfigMap
		if err := json.Unmarshal(body, &cm); err != nil {
			status(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		cur := s.cms[key]
		if cur == nil {
			status(w, http.StatusNotFound, "NotFound", "configmap not found")
			return
		}
		if cm.Metadata.ResourceVersion != cur.Metadata.ResourceVersion {
			status(w, http.StatusConflict, "Conflict", "the object has been modified")
			return
		}
		s.rv++
		cm.Metadata.ResourceVersion = strconv.Itoa(s.rv)
		s.cms[key] = &cm
		s.Writes[user]++
		_ = json.NewEncoder(w).Encode(cm)
	default:
		status(w, http.StatusMethodNotAllowed, "MethodNotAllowed", method)
	}
}

func (s *Server) serveLease(w http.ResponseWriter, method, user, ns, name string, body []byte) {
	switch method {
	case http.MethodGet:
		l := s.leases[ns+"/"+name]
		if l == nil {
			status(w, http.StatusNotFound, "NotFound", "lease not found")
			return
		}
		_ = json.NewEncoder(w).Encode(l)
	case http.MethodPost, http.MethodPut:
		var l kubeapi.Lease
		if err := json.Unmarshal(body, &l); err != nil {
			status(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		key := ns + "/" + l.Metadata.Name
		cur := s.leases[key]
		if method == http.MethodPost && cur != nil {
			status(w, http.StatusConflict, "AlreadyExists", "lease exists")
			return
		}
		if method == http.MethodPut {
			if cur == nil {
				status(w, http.StatusNotFound, "NotFound", "lease not found")
				return
			}
			if l.Metadata.ResourceVersion != cur.Metadata.ResourceVersion {
				status(w, http.StatusConflict, "Conflict", "the object has been modified")
				return
			}
		}
		s.rv++
		l.Metadata.ResourceVersion = strconv.Itoa(s.rv)
		l.Metadata.Namespace = ns
		s.leases[key] = &l
		s.Writes[user]++
		if method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(l)
	default:
		status(w, http.StatusMethodNotAllowed, "MethodNotAllowed", method)
	}
}
