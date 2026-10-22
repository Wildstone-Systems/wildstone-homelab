package api

// The portal's writes, and the sibling service that must not be able to
// cause one.
//
// This is the one test in the tree that models the household's session
// honestly. Every other portal test issues a request the member's own
// page would issue; this one issues the request a DIFFERENT page under
// the same session domain would issue, which the proxy authenticates
// just as willingly — the cookie is scoped to the domain, not to us.
// Without the check under test, the two are indistinguishable at the
// handler, and a delete is a delete whoever caused it.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write issues one portal write with the browser-supplied provenance
// headers set by hand, so a test can say exactly what the browser would
// have reported.
func (h *harness) write(path, user, groups, origin, fetchSite string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(""))
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(TrustedIngressHeader, user)
	req.Header.Set(ConsoleGroupsHeader, groups)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	return res
}

// A page on any other host under the session domain is a signed-in
// browser away from a delete. The member is real, the proxy's header is
// real, and the only thing that distinguishes the request is where the
// browser says it came from.
func TestASiblingServiceCannotSpendAMembersSession(t *testing.T) {
	h, root := portalHarness(t)

	if res, body := h.upload("sam", "shared", "holiday.jpg", "pretend-jpeg"); res.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d (%v)", res.StatusCode, body)
	}
	stored := filepath.Join(root, "Shared", "holiday.jpg")
	if _, err := os.Stat(stored); err != nil {
		t.Fatalf("the upload did not land: %v", err)
	}

	del := "/api/v1/filedrop/delete?namespace=shared&name=holiday.jpg"
	for _, tc := range []struct {
		name      string
		origin    string
		fetchSite string
	}{
		{"a compromised sibling under the session domain", "https://books.example.test", "cross-site"},
		{"the same host on another port, which is another origin", h.srv.URL + ":8443", ""},
		{"an Origin from somewhere else entirely", "https://attacker.example", ""},
		{"a browser that reports the navigation as cross-site", h.srv.URL, "cross-site"},
		{"a page embedding us that reports same-site but not same-origin", h.srv.URL, "same-site"},
		{"a scripted caller carrying no provenance at all", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.unthrottled()
			res := h.write(del, "sam", "filedrop_users", tc.origin, tc.fetchSite)
			defer res.Body.Close()
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("delete = %d, want 403", res.StatusCode)
			}
			if _, err := os.Stat(stored); err != nil {
				t.Fatalf("the file was deleted anyway: %v", err)
			}
		})
	}

	// And the member's own page still works, both as a current browser
	// reports it and as one that sends Origin alone.
	h.unthrottled()
	res := h.write(del, "sam", "filedrop_users", h.srv.URL, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the member's own delete = %d, want 200", res.StatusCode)
	}
	if _, err := os.Stat(stored); !os.IsNotExist(err) {
		t.Fatalf("the file survived a delete from the portal itself: %v", err)
	}
}

// The framed widgets are embedded by a dashboard that may be served
// from another origin, and their READS must keep working there: the
// check is about writes, and a GET changes nothing.
func TestTheFramedWidgetsStillReadFromAnotherOrigin(t *testing.T) {
	h, _ := portalHarness(t)
	h.unthrottled()

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/filedrop/widget", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(TrustedIngressHeader, "sam")
	req.Header.Set(ConsoleGroupsHeader, "filedrop_users")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Sec-Fetch-Dest", "iframe")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET widget: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the framed widget = %d, want 200", res.StatusCode)
	}
}
