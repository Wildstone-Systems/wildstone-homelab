package portal

// The two surfaces as files a browser can be given.

import (
	"embed"
	"net/http"
	"path"
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
// "assistant-widget", "assistant", "home", "activity"), or ok=false for
// anything else. A
// closed set rather than a directory walk: the URL space this serves is
// four pages, and a handler that maps a path segment onto a filename is
// a handler somebody will later point at "../".
func Page(name string) ([]byte, bool) {
	switch name {
	case "files-widget", "files", "assistant-widget", "assistant", "home", "activity":
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
	// 'unsafe-inline' covers each page's own script and style, which are
	// in the file rather than beside it: one document per surface and no
	// bundler. Everything else is denied, so the model output and
	// filenames these pages render cannot become script even if the
	// escaping were wrong.
	//
	// 'self' is added to style-src and font-src, and that is what lets
	// the portal look like the household's own product. The design
	// system is a real stylesheet, Inter is a real font file and the app
	// launcher shows real logos — none of which a page can have under
	// default-src 'none' with no 'self' anywhere. The earlier policy
	// forced a system-font, letters-instead-of-logos approximation of a
	// design that already existed, which is a visible cost paid for no
	// security: 'self' here is THIS origin, which is Keystone serving
	// bytes out of its own binary (portal.Asset), not a CDN and not the
	// disk.
	//
	// img-src keeps data: for inline SVG fallbacks; connect-src stays
	// 'self' because every fetch these pages make is to this API. There
	// is deliberately still no script-src 'self': a page's behaviour
	// lives in the page, and an external script is the one thing that
	// would make "read the file to know what the surface does" false.
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"connect-src 'self'",
		"img-src 'self' data:",
		"style-src 'self' 'unsafe-inline'",
		"font-src 'self'",
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

// assetDirs are the two directories of static design files the portal
// serves, and the whole of what Asset will look in.
//
// A closed set for the same reason Page's is: the alternative is
// mapping a URL segment onto a directory name, and the first person to
// point that at ".." gets the embedded filesystem. Here they get false.
var assetDirs = map[string]bool{"assets": true, "icons": true}

// assetTypes is every media type this serves, by extension. A file whose
// extension is not here is not served, whatever it is — an embedded
// filesystem should not be able to hand a browser something the page
// author did not mean it to.
var assetTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".woff2": "font/woff2",
	".svg":   "image/svg+xml",
}

// Asset returns one static design file — a stylesheet, a font, or an app
// logo — by its "<dir>/<name>" path, with the media type to serve it as.
//
// These are the files that make the portal look like the household's
// own product rather than a form: Inter, the Phosphor glyph font, the
// design system's stylesheet, and the app icon set. They are deliberately
// NOT inlined into each page the way the pages' own CSS used to be —
// there are six surfaces now and the alternative is six copies of the
// same 200KB, re-sent on every navigation because a page is served
// no-store.
//
// Served with a long immutable cache, which is safe here and is not for
// the pages: an asset's bytes never change under its name (a new Inter
// would arrive as a new filename), while a page's change on every
// deploy. That is the same split cmd/keystone makes for the Console's
// hashed bundles.
func Asset(name string) ([]byte, string, bool) {
	clean := path.Clean("/" + name)
	dir, file := path.Split(strings.TrimPrefix(clean, "/"))
	dir = strings.TrimSuffix(dir, "/")
	if !assetDirs[dir] || file == "" {
		return nil, "", false
	}
	ctype, ok := assetTypes[strings.ToLower(path.Ext(file))]
	if !ok {
		return nil, "", false
	}
	b, err := web.ReadFile("web/" + dir + "/" + file)
	if err != nil {
		return nil, "", false
	}
	return b, ctype, true
}

// WriteAsset serves one static design file.
func WriteAsset(w http.ResponseWriter, body []byte, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
