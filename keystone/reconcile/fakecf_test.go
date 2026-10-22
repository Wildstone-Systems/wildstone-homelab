package reconcile

// A small, self-contained fake Cloudflare API v4 server for the DnsRecord
// end-to-end reconcile tests. This deliberately duplicates (in miniature)
// the fake in keystone/provider/cloudflare/fake_cf_test.go rather than
// importing it: Go test helpers are package-private, and reconcile's tests
// need only the handful of behaviors (list/create/update, one-shot
// read-back failure, bounded rate-limit) these end-to-end tests exercise —
// the exhaustive provider-level contract is already covered by the
// cloudflare package's own test suite.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
)

type reconcileFakeCFRecord struct {
	id      string
	zoneID  string
	rtype   string
	name    string
	content string
	proxied bool
}

type reconcileFakeCF struct {
	srv *httptest.Server

	mu      sync.Mutex
	zones   map[string]string
	records map[string]*reconcileFakeCFRecord
	seq     int

	rateLimitRemaining int
	retryAfterSeconds  int

	// serverErrorSkip requests are let through unmodified before
	// serverErrorRemaining starts counting down (0 means "start
	// immediately") — lets a test allow an initial phase (e.g. observe) to
	// succeed before a persistent outage begins during apply.
	serverErrorSkip int
	// serverErrorRemaining: once serverErrorSkip is exhausted, every
	// request is answered with a 503 (and serverErrorRemaining is
	// decremented) instead of being served — models a transient
	// Cloudflare-side outage (ADR-W004 A5 B8), distinct from
	// rateLimitRemaining's 429.
	serverErrorRemaining int

	failGetCallN  int
	getListCalled int

	reqCounts map[string]int
}

var reconcileDNSListPath = regexp.MustCompile(`^/zones/([^/]+)/dns_records$`)
var reconcileDNSRecordPath = regexp.MustCompile(`^/zones/([^/]+)/dns_records/([^/]+)$`)

func newFakeCFForReconcileTest(t *testing.T) *reconcileFakeCF {
	f := &reconcileFakeCF{zones: map[string]string{}, records: map[string]*reconcileFakeCFRecord{}, reqCounts: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *reconcileFakeCF) URL() string { return f.srv.URL }

func (f *reconcileFakeCF) addZone(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("zone-%d", f.seq)
	f.zones[name] = id
	return id
}

func (f *reconcileFakeCF) setRecord(zoneID, rtype, name, content string, proxied bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.records {
		if r.zoneID == zoneID && r.rtype == rtype && r.name == name {
			r.content = content
			r.proxied = proxied
			return
		}
	}
	f.seq++
	id := fmt.Sprintf("rec-%d", f.seq)
	f.records[id] = &reconcileFakeCFRecord{id: id, zoneID: zoneID, rtype: rtype, name: name, content: content, proxied: proxied}
}

func (f *reconcileFakeCF) getRecord(zoneID, rtype, name string) (*reconcileFakeCFRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.records {
		if r.zoneID == zoneID && r.rtype == rtype && r.name == name {
			cp := *r
			return &cp, true
		}
	}
	return nil, false
}

func (f *reconcileFakeCF) recordCount(zoneID, rtype, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.records {
		if r.zoneID == zoneID && r.rtype == rtype && r.name == name {
			n++
		}
	}
	return n
}

func (f *reconcileFakeCF) setRateLimit(n, retryAfterSeconds int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateLimitRemaining = n
	f.retryAfterSeconds = retryAfterSeconds
}

// setServerError makes the next n requests (across all endpoints) respond
// with 503 (Service Unavailable) — a transient control-plane failure,
// distinct from setRateLimit's 429.
func (f *reconcileFakeCF) setServerError(n int) {
	f.setServerErrorAfter(0, n)
}

// setServerErrorAfter lets the first skip requests through unmodified,
// then answers the next n requests with 503. Used to let an initial phase
// (e.g. the Reconciler's observe pre-pass) succeed before a persistent
// outage begins during apply, so a test can exhaust retryProvider's
// budget deterministically rather than racing which call sees the outage
// first.
func (f *reconcileFakeCF) setServerErrorAfter(skip, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverErrorSkip = skip
	f.serverErrorRemaining = n
}

// armFailGetCall fails the Nth GET (1-indexed) to the dns_records list
// endpoint from here on. Through the full Reconciler.Reconcile path a
// Create goes through three such calls in order: (1) the Reconciler's own
// observeProviders pre-pass, (2) Create's internal pre-create lookup, (3)
// Create's post-POST read-back — so n=3 models "the follow-up read-back
// fails" end-to-end (ADR-W004 A5 partial failure).
func (f *reconcileFakeCF) armFailGetCall(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getListCalled = 0
	f.failGetCallN = n
}

func (f *reconcileFakeCF) requestCount(method, pattern string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqCounts[method+" "+pattern]
}

func reconcileWriteEnvelope(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"success": true,
		"errors":  []any{},
		"result":  result,
	})
}

type reconcileCFZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type reconcileCFDNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

func (f *reconcileFakeCF) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.rateLimitRemaining > 0 {
		f.rateLimitRemaining--
		retryAfter := f.retryAfterSeconds
		f.mu.Unlock()
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"rate limited"}]}`)) //nolint:errcheck
		return
	}
	f.mu.Unlock()

	f.mu.Lock()
	if f.serverErrorSkip > 0 {
		f.serverErrorSkip--
	} else if f.serverErrorRemaining > 0 {
		f.serverErrorRemaining--
		f.mu.Unlock()
		http.Error(w, `{"success":false,"errors":[{"code":1101,"message":"fake: injected transient server error"}]}`, http.StatusServiceUnavailable)
		return
	}
	f.mu.Unlock()

	if r.Header.Get("Authorization") == "" {
		http.Error(w, `{"success":false,"errors":[{"code":9109,"message":"missing authorization"}]}`, http.StatusForbidden)
		return
	}

	if r.URL.Path == "/zones" && r.Method == http.MethodGet {
		name := r.URL.Query().Get("name")
		f.mu.Lock()
		id, ok := f.zones[name]
		f.mu.Unlock()
		if !ok {
			reconcileWriteEnvelope(w, []reconcileCFZone{})
			return
		}
		reconcileWriteEnvelope(w, []reconcileCFZone{{ID: id, Name: name}})
		return
	}

	if m := reconcileDNSListPath.FindStringSubmatch(r.URL.Path); m != nil {
		zoneID := m[1]
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			f.reqCounts["GET /zones/{id}/dns_records"]++
			if f.failGetCallN > 0 {
				f.getListCalled++
				if f.getListCalled == f.failGetCallN {
					f.failGetCallN = 0
					f.mu.Unlock()
					http.Error(w, `{"success":false,"errors":[{"code":1000,"message":"fake: injected read-back failure"}]}`, http.StatusInternalServerError)
					return
				}
			}
			f.mu.Unlock()

			rtype := r.URL.Query().Get("type")
			name := r.URL.Query().Get("name")
			var out []reconcileCFDNSRecord
			f.mu.Lock()
			for _, rec := range f.records {
				if rec.zoneID == zoneID && (rtype == "" || rec.rtype == rtype) && (name == "" || rec.name == name) {
					out = append(out, reconcileCFDNSRecord{ID: rec.id, Type: rec.rtype, Name: rec.name, Content: rec.content, Proxied: rec.proxied})
				}
			}
			f.mu.Unlock()
			reconcileWriteEnvelope(w, out)
			return

		case http.MethodPost:
			f.mu.Lock()
			f.reqCounts["POST /zones/{id}/dns_records"]++
			f.mu.Unlock()
			var body reconcileCFDNSRecord
			data, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(data, &body); err != nil {
				http.Error(w, `{"success":false,"errors":[{"code":1,"message":"bad body"}]}`, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.seq++
			id := fmt.Sprintf("rec-%d", f.seq)
			f.records[id] = &reconcileFakeCFRecord{id: id, zoneID: zoneID, rtype: body.Type, name: body.Name, content: body.Content, proxied: body.Proxied}
			f.mu.Unlock()
			reconcileWriteEnvelope(w, reconcileCFDNSRecord{ID: id, Type: body.Type, Name: body.Name, Content: body.Content, Proxied: body.Proxied})
			return
		}
	}

	if m := reconcileDNSRecordPath.FindStringSubmatch(r.URL.Path); m != nil {
		id := m[2]
		switch r.Method {
		case http.MethodPut:
			f.mu.Lock()
			f.reqCounts["PUT /zones/{id}/dns_records/{id}"]++
			rec, ok := f.records[id]
			f.mu.Unlock()
			if !ok {
				http.Error(w, `{"success":false,"errors":[{"code":81044,"message":"record not found"}]}`, http.StatusNotFound)
				return
			}
			var body reconcileCFDNSRecord
			data, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(data, &body); err != nil {
				http.Error(w, `{"success":false,"errors":[{"code":1,"message":"bad body"}]}`, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			rec.content = body.Content
			rec.proxied = body.Proxied
			f.mu.Unlock()
			reconcileWriteEnvelope(w, reconcileCFDNSRecord{ID: rec.id, Type: rec.rtype, Name: rec.name, Content: rec.content, Proxied: rec.proxied})
			return

		case http.MethodDelete:
			f.mu.Lock()
			f.reqCounts["DELETE /zones/{id}/dns_records/{id}"]++
			delete(f.records, id)
			f.mu.Unlock()
			reconcileWriteEnvelope(w, map[string]string{"id": id})
			return
		}
	}

	http.Error(w, `{"success":false,"errors":[{"code":0,"message":"unhandled by fake"}]}`, http.StatusNotFound)
}
