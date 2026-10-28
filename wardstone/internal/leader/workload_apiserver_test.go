package leader

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type workloadAPIServer struct {
	*httptest.Server
	mu         sync.Mutex
	rv         int
	d          *kubeapi.Deployment
	s          *kubeapi.Service
	patchStart chan struct{}
	patchDone  chan struct{}
}

func newWorkloadAPIServer(t *testing.T) *workloadAPIServer {
	t.Helper()
	s := &workloadAPIServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *workloadAPIServer) client(t *testing.T) *kubeapi.Client {
	t.Helper()
	c, err := kubeapi.NewWithHTTP(s.URL, func() (string, error) { return "test-token", nil }, s.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeObject(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeNotFound(w http.ResponseWriter) {
	writeObject(w, http.StatusNotFound, kubeapi.StatusError{Code: http.StatusNotFound, Reason: "NotFound", Message: "not found"})
}

func (s *workloadAPIServer) next(m *kubeapi.ObjectMeta, uid string) {
	s.rv++
	m.ResourceVersion = strconv.Itoa(s.rv)
	if m.UID == "" {
		m.UID = uid
	}
	m.Generation++
}

func (s *workloadAPIServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if s.patchStart != nil && (r.Method == http.MethodPost || r.Method == http.MethodPatch) && strings.Contains(r.URL.Path, "/deployments") {
		close(s.patchStart)
		<-s.patchDone
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.Contains(r.URL.Path, "/deployments"):
		s.serveDeployment(w, r)
	case strings.Contains(r.URL.Path, "/services"):
		s.serveService(w, r)
	default:
		writeNotFound(w)
	}
}

func (s *workloadAPIServer) serveDeployment(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if s.d == nil {
			writeNotFound(w)
			return
		}
		writeObject(w, http.StatusOK, s.d)
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		if r.Method == http.MethodPatch && r.Header.Get("Content-Type") != "application/apply-patch+yaml" {
			testingError(w, "wrong apply content type")
			return
		}
		var d kubeapi.Deployment
		if json.NewDecoder(r.Body).Decode(&d) != nil {
			testingError(w, "bad Deployment")
			return
		}
		if r.Method == http.MethodPost && s.d != nil || r.Method != http.MethodPost && (s.d == nil || d.Metadata.ResourceVersion == "" || d.Metadata.ResourceVersion != s.d.Metadata.ResourceVersion) {
			testingError(w, "Deployment resourceVersion conflict")
			return
		}
		s.next(&d.Metadata, "deployment-uid")
		s.d = &d
		writeObject(w, http.StatusOK, &d)
	case http.MethodDelete:
		var o kubeapi.DeleteOptions
		if json.NewDecoder(r.Body).Decode(&o) != nil || s.d == nil || o.Preconditions == nil || o.Preconditions.UID != s.d.Metadata.UID || o.Preconditions.ResourceVersion != s.d.Metadata.ResourceVersion {
			testingError(w, "bad Deployment delete precondition")
			return
		}
		s.d = nil
		writeObject(w, http.StatusOK, map[string]string{"status": "Success"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *workloadAPIServer) serveService(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if s.s == nil {
			writeNotFound(w)
			return
		}
		writeObject(w, http.StatusOK, s.s)
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		var service kubeapi.Service
		if json.NewDecoder(r.Body).Decode(&service) != nil {
			testingError(w, "bad Service")
			return
		}
		if r.Method == http.MethodPost && s.s != nil || r.Method != http.MethodPost && (s.s == nil || service.Metadata.ResourceVersion == "" || service.Metadata.ResourceVersion != s.s.Metadata.ResourceVersion) {
			testingError(w, "Service resourceVersion conflict")
			return
		}
		s.next(&service.Metadata, "service-uid")
		s.s = &service
		writeObject(w, http.StatusOK, &service)
	case http.MethodDelete:
		var o kubeapi.DeleteOptions
		if json.NewDecoder(r.Body).Decode(&o) != nil || s.s == nil || o.Preconditions == nil || o.Preconditions.UID != s.s.Metadata.UID || o.Preconditions.ResourceVersion != s.s.Metadata.ResourceVersion {
			testingError(w, "bad Service delete precondition")
			return
		}
		s.s = nil
		writeObject(w, http.StatusOK, map[string]string{"status": "Success"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func testingError(w http.ResponseWriter, message string) {
	writeObject(w, http.StatusConflict, kubeapi.StatusError{Code: http.StatusConflict, Reason: "Conflict", Message: message})
}

func TestWorkloadExecutorAgainstFakeAPIServer(t *testing.T) {
	s := newWorkloadAPIServer(t)
	defer s.Close()
	x := WorkloadExecutor{Client: s.client(t), Epoch: 7, Context: context.Background()}
	req := &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload()}
	_, err := x.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	current, err := x.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("idempotent API-server retry: %v", err)
	}
	stale := &wsv1.ApplyWorkload{Authority: authority(7, "rev-b"), ExpectedRevision: "stale", Workload: workload()}
	if _, err := x.Apply(context.Background(), stale); !IsRefusal(err) {
		t.Fatalf("stale revision = %v", err)
	}
	deleted, err := x.Delete(context.Background(), &wsv1.DeleteWorkload{Authority: authority(7, "rev-b"), Name: "tools-it-tools", Uid: current.Uid, ResourceVersion: current.ResourceVersion, ServiceUid: current.ServiceUid, ServiceResourceVersion: current.ServiceResourceVersion})
	if err != nil || !deleted.Deleted {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
}

func TestFakeAPIServerRequestIsCancelledOnLeadershipLoss(t *testing.T) {
	s := newWorkloadAPIServer(t)
	s.patchStart = make(chan struct{})
	s.patchDone = make(chan struct{})
	defer func() {
		close(s.patchDone)
		s.Close()
	}()
	termCtx, cancel := context.WithCancel(context.Background())
	x := WorkloadExecutor{Client: s.client(t), Epoch: 7, Context: termCtx}
	done := make(chan error, 1)
	go func() {
		_, err := x.Apply(context.Background(), &wsv1.ApplyWorkload{Authority: authority(7, "rev-a"), Workload: workload()})
		done <- err
	}()
	<-s.patchStart
	cancel()
	if err := <-done; err == nil {
		t.Fatal("API-server request survived leadership loss")
	}
}
