// Package caldavfake is an in-process CalDAV server for tests of the
// mail review's exact calendar path (epic #326 S06), kept as production
// test infrastructure beside imapfake so the end-to-end proofs (S08)
// run against the same fixture S06's tests do.
//
// It serves several collections (/cal/<id>/) with real conditional-write
// semantics (If-None-Match: *, If-Match) and faults a test schedules: a
// PUT that fails before it is stored, one stored and then answered too
// late, one stored and then dropped, and GETs that fail.
package caldavfake

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// Server is an in-process CalDAV server with several collections
// (/cal/<id>/), real conditional-write semantics, and faults a test can
// schedule: a PUT that fails before it is stored, one that is stored
// and then answered too late (the client times out), one that is
// stored and then has its connection dropped, and GETs that fail.
type Server struct {
	*httptest.Server
	mu      sync.Mutex
	objects map[string]string
	etag    map[string]int
	// PUT faults, consumed one per PUT: "", "fail-before", "hang-after",
	// "drop-after", "forbidden".
	putFaults []string
	// getFaults fails this many GETs with 503.
	getFaults int
	puts      int
	deletes   int
}

// New starts a server; Close stops it.
func New() *Server {
	f := &Server{objects: map[string]string{}, etag: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	switch r.Method {
	case "REPORT":
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
		var paths []string
		for p := range f.objects {
			if strings.HasPrefix(p, r.URL.Path) {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(&b, `<response><href>%s</href><propstat><prop><getetag>"%d"</getetag><C:calendar-data>%s</C:calendar-data></prop></propstat></response>`,
				p, f.etag[p], xmlEscape(f.objects[p]))
		}
		b.WriteString(`</multistatus>`)
		f.mu.Unlock()
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(b.String()))
	case http.MethodGet:
		if f.getFaults > 0 {
			f.getFaults--
			f.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		obj, ok := f.objects[r.URL.Path]
		tag := f.etag[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, tag))
		_, _ = w.Write([]byte(obj))
	case http.MethodPut:
		f.puts++
		fault := ""
		if len(f.putFaults) > 0 {
			fault, f.putFaults = f.putFaults[0], f.putFaults[1:]
		}
		switch fault {
		case "fail-before":
			f.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case "forbidden":
			f.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, exists := f.objects[r.URL.Path]
		if r.Header.Get("If-None-Match") == "*" && exists {
			f.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if m := r.Header.Get("If-Match"); m != "" && (!exists || m != fmt.Sprintf(`"%d"`, f.etag[r.URL.Path])) {
			f.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		f.objects[r.URL.Path] = string(body)
		f.etag[r.URL.Path]++
		tag := f.etag[r.URL.Path]
		f.mu.Unlock()
		switch fault {
		case "hang-after":
			time.Sleep(600 * time.Millisecond)
		case "drop-after":
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, tag))
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		f.deletes++
		_, ok := f.objects[r.URL.Path]
		delete(f.objects, r.URL.Path)
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		f.mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Seed puts a plain event on a collection.
func (f *Server) Seed(calendar, uid, summary string, start, end time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := "/cal/" + calendar + "/" + uid + ".ics"
	f.objects[p] = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nSUMMARY:" + summary +
		"\r\nDTSTART:" + start.UTC().Format("20060102T150405Z") + "\r\nDTEND:" + end.UTC().Format("20060102T150405Z") +
		"\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	f.etag[p]++
}

// CandidateObjects lists the events the review engine wrote.
func (f *Server) CandidateObjects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.objects {
		if strings.Contains(p, "wildstone-candidate-") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Object returns the stored iCalendar at a path, "" when absent.
func (f *Server) Object(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[p]
}

// PutCount is how many PUTs arrived, faulted or not.
func (f *Server) PutCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

// Schedule queues PUT faults, consumed one per PUT: "" (none),
// "fail-before", "hang-after", "drop-after", "forbidden".
func (f *Server) Schedule(faults ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putFaults = append(f.putFaults, faults...)
}

// FailGets fails the next n GETs with 503.
func (f *Server) FailGets(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getFaults = n
}

// Objects lists every stored path.
func (f *Server) Objects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for p := range f.objects {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
