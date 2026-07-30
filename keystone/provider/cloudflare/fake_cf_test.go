package cloudflare

// fakeCF is a minimal in-memory Cloudflare API v4 server for tests: just
// the endpoints this package's provider touches (zones lookup, dns_records
// list/create/update/delete), with the same envelope/status-code shapes,
// plus injectable failure modes for the partial-failure and rate-limit
// contracts (ADR-W004 A5).

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

type fakeCFRecord struct {
	id      string
	zoneID  string
	rtype   string
	name    string
	content string
	proxied bool
}

type fakeCF struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	zones   map[string]string // name -> id
	records map[string]*fakeCFRecord
	seq     int

	// rateLimitRemaining: while > 0, every request is answered with a 429
	// (and rateLimitRemaining is decremented) instead of being served.
	rateLimitRemaining int
	retryAfterSeconds  int

	// failGetCallN: when > 0, the Nth GET to the dns_records list endpoint
	// (1-indexed, counting from when armed) fails with a 500 instead of
	// being served. Used to model "Create's POST succeeds but the
	// follow-up read-back fails" (ADR-W004 A5 partial failure): Create's
	// own flow issues a pre-create lookup GET (call 1) then, after the
	// POST, a read-back GET (call 2) — failNextReadBack arms call 2.
	failGetCallN  int
	getListCalled int

	reqCounts map[string]int // "METHOD path-pattern" -> count
}

var dnsRecordsListPath = regexp.MustCompile(`^/zones/([^/]+)/dns_records$`)
var dnsRecordPath = regexp.MustCompile(`^/zones/([^/]+)/dns_records/([^/]+)$`)

func newFakeCF(t *testing.T) *fakeCF {
	f := &fakeCF{
		t:         t,
		zones:     map[string]string{},
		records:   map[string]*fakeCFRecord{},
		reqCounts: map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCF) URL() string { return f.srv.URL }

// addZone registers a zone by name, returning its assigned id.
func (f *fakeCF) addZone(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("zone-%d", f.seq)
	f.zones[name] = id
	return id
}

// setRecord seeds/overwrites a record directly in the fake, bypassing the
// provider entirely — used to simulate out-of-band drift (a human/another
// tool changed the record at Cloudflare).
func (f *fakeCF) setRecord(zoneID, rtype, name, content string, proxied bool) {
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
	f.records[id] = &fakeCFRecord{id: id, zoneID: zoneID, rtype: rtype, name: name, content: content, proxied: proxied}
}

// recordCount returns how many records exist matching zoneID+rtype+name —
// used by the partial-failure test to assert exactly one record exists
// after a retried Create.
func (f *fakeCF) recordCount(zoneID, rtype, name string) int {
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

func (f *fakeCF) getRecord(zoneID, rtype, name string) (*fakeCFRecord, bool) {
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

// setRateLimit makes the next n requests (across all endpoints) respond
// with 429 + Retry-After: retryAfterSeconds.
func (f *fakeCF) setRateLimit(n, retryAfterSeconds int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateLimitRemaining = n
	f.retryAfterSeconds = retryAfterSeconds
}

// failNextReadBack arms a one-shot failure of the read-back GET that
// follows a Create's POST — the SECOND dns_records list GET issued after
// this call (the first is Create's own pre-create lookup). See the
// failGetCallN doc comment for why it's the second, not the first.
func (f *fakeCF) failNextReadBack() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getListCalled = 0
	f.failGetCallN = 2
}

func (f *fakeCF) requestCount(method, pattern string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqCounts[method+" "+pattern]
}

func writeEnvelope(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"success": true,
		"errors":  []any{},
		"result":  result,
	})
}

func (f *fakeCF) handle(w http.ResponseWriter, r *http.Request) {
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

	// Never accept requests without an Authorization header — the same
	// shape real Cloudflare enforces, and a cheap assertion the provider
	// never accidentally drops auth.
	if r.Header.Get("Authorization") == "" {
		http.Error(w, `{"success":false,"errors":[{"code":9109,"message":"missing authorization"}]}`, http.StatusForbidden)
		return
	}

	if r.URL.Path == "/zones" && r.Method == http.MethodGet {
		f.mu.Lock()
		f.reqCounts["GET /zones"]++
		name := r.URL.Query().Get("name")
		id, ok := f.zones[name]
		f.mu.Unlock()
		if !ok {
			writeEnvelope(w, []cfZone{})
			return
		}
		writeEnvelope(w, []cfZone{{ID: id, Name: name}})
		return
	}

	if m := dnsRecordsListPath.FindStringSubmatch(r.URL.Path); m != nil {
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
			var out []cfDNSRecord
			f.mu.Lock()
			for _, rec := range f.records {
				if rec.zoneID == zoneID && (rtype == "" || rec.rtype == rtype) && (name == "" || rec.name == name) {
					out = append(out, cfDNSRecord{ID: rec.id, Type: rec.rtype, Name: rec.name, Content: rec.content, Proxied: rec.proxied})
				}
			}
			f.mu.Unlock()
			writeEnvelope(w, out)
			return

		case http.MethodPost:
			f.mu.Lock()
			f.reqCounts["POST /zones/{id}/dns_records"]++
			f.mu.Unlock()
			var body cfDNSRecord
			data, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(data, &body); err != nil {
				http.Error(w, `{"success":false,"errors":[{"code":1,"message":"bad body"}]}`, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.seq++
			id := fmt.Sprintf("rec-%d", f.seq)
			f.records[id] = &fakeCFRecord{id: id, zoneID: zoneID, rtype: body.Type, name: body.Name, content: body.Content, proxied: body.Proxied}
			f.mu.Unlock()
			writeEnvelope(w, cfDNSRecord{ID: id, Type: body.Type, Name: body.Name, Content: body.Content, Proxied: body.Proxied})
			return
		}
	}

	if m := dnsRecordPath.FindStringSubmatch(r.URL.Path); m != nil {
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
			var body cfDNSRecord
			data, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(data, &body); err != nil {
				http.Error(w, `{"success":false,"errors":[{"code":1,"message":"bad body"}]}`, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			rec.content = body.Content
			rec.proxied = body.Proxied
			f.mu.Unlock()
			writeEnvelope(w, cfDNSRecord{ID: rec.id, Type: rec.rtype, Name: rec.name, Content: rec.content, Proxied: rec.proxied})
			return

		case http.MethodDelete:
			f.mu.Lock()
			f.reqCounts["DELETE /zones/{id}/dns_records/{id}"]++
			delete(f.records, id)
			f.mu.Unlock()
			writeEnvelope(w, map[string]string{"id": id})
			return
		}
	}

	http.Error(w, `{"success":false,"errors":[{"code":0,"message":"unhandled by fake"}]}`, http.StatusNotFound)
}
