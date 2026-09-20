package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A stylesheet fix that ships in a new Keystone has to reach a browser
// that has already loaded the old one.
//
// This is a regression test with a real bug behind it: the assets were
// served `max-age=31536000, immutable` over URLs that carry no version,
// so a household that had opened the dashboard once kept the stylesheet
// it first downloaded for a year — `immutable` suppresses even the
// reload-time conditional request. Two deployed CSS fixes changed
// nothing on the screen of the only person looking at it.
func TestAnAssetCanBeReplacedByADeploy(t *testing.T) {
	body, ctype, ok := Asset("assets/nocturne.css")
	if !ok {
		t.Fatal("no stylesheet to serve")
	}

	rec := httptest.NewRecorder()
	WriteAsset(rec, httptest.NewRequest(http.MethodGet, "/portal/assets/nocturne.css", nil), body, ctype)
	res := rec.Result()
	defer res.Body.Close()

	cache := res.Header.Get("Cache-Control")
	if strings.Contains(cache, "immutable") {
		t.Errorf("assets are immutable over an unversioned URL, so a deploy cannot replace them: %q", cache)
	}
	if !strings.Contains(cache, "must-revalidate") && !strings.Contains(cache, "no-cache") {
		t.Errorf("nothing makes the browser check for a new copy: %q", cache)
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag, so a revalidation cannot be answered cheaply")
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("ETag is not a quoted-string: %q", etag)
	}

	// The browser holding that copy is told to keep it, with no body.
	again := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/portal/assets/nocturne.css", nil)
	req.Header.Set("If-None-Match", etag)
	WriteAsset(again, req, body, ctype)
	if again.Code != http.StatusNotModified {
		t.Errorf("a matching ETag was not answered with 304: %d", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Errorf("a 304 carried %d bytes of body", again.Body.Len())
	}

	// Different bytes are a different ETag, which is the whole point:
	// the browser asks, and this time it is told to take the new one.
	changed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/portal/assets/nocturne.css", nil)
	req.Header.Set("If-None-Match", etag)
	WriteAsset(changed, req, append(append([]byte(nil), body...), "\n/* a deployed fix */"...), ctype)
	if changed.Code != http.StatusOK {
		t.Errorf("a changed stylesheet was withheld as unmodified: %d", changed.Code)
	}
	if changed.Header().Get("ETag") == etag {
		t.Error("changed bytes kept the old ETag, so the freeze is still on")
	}
}
