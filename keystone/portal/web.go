package portal

// The two surfaces as files a browser can be given.

import (
	"embed"
	"net/http"
	"strings"
)

// web holds the portal's pages. Embedded rather than read from a
// directory at run time for the reason the Console is not: these are
// served by the control plane's own process, and a page that can be
// edited on disk under a running Keystone is a page that can be edited
// by anything that reaches the disk.
//
//go:embed web
var web embed.FS

// Page returns one embedded page by name ("files-widget", "files",
// "assistant-widget", "assistant", "home"), or ok=false for anything
// else. A
// closed set rather than a directory walk: the URL space this serves is
// four pages, and a handler that maps a path segment onto a filename is
// a handler somebody will later point at "../".
func Page(name string) ([]byte, bool) {
	switch name {
	case "files-widget", "files", "assistant-widget", "assistant", "home":
	default:
		return nil, false
	}
	b, err := web.ReadFile("web/" + name + ".html")
	if err != nil {
		return nil, false
	}
	return b, true
}

// WritePage serves a page with the headers a surface embedded in
// somebody else's dashboard needs.
//
// frameAncestors names who may frame it. It is derived from desired
// state by the caller — the dashboard's own browser address — rather
// than left open: a portal page that any site may frame is one a
// malicious page can put in front of a signed-in household member and
// drive with their session. An empty list means nobody but this origin,
// which is what an install with no dashboard should get.
func WritePage(w http.ResponseWriter, body []byte, frameAncestors []string) {
	ancestors := "'self'"
	if len(frameAncestors) > 0 {
		ancestors += " " + strings.Join(frameAncestors, " ")
	}
	// 'unsafe-inline' covers this page's own script and style, which are
	// in the file rather than beside it: one document per surface, no
	// bundler, and nothing fetched from anywhere. Everything else is
	// denied, so the model output and filenames these pages render
	// cannot become script even if the escaping below were wrong.
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"connect-src 'self'",
		"img-src 'self' data:",
		"style-src 'unsafe-inline'",
		"script-src 'unsafe-inline'",
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors " + ancestors,
	}, "; "))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Never cached: the page is behind a session, and a shared device
	// should not hand the next person a copy from disk.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
