package portal

// The two surfaces as files a browser can be given.

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
	case "secrets", "files-widget", "files", "assistant-widget", "assistant", "home", "activity", "catalog", "calendar", "upkeep", "agenda":
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
// themeMarker is where a page says the household's theme belongs.
//
// A marker in each page rather than "before </head>" found by search:
// the pages are hand-written documents with inline <style> blocks of
// their own, and the token block has to land AFTER the design system's
// stylesheet link (so it wins on document order) and BEFORE the page's
// own rules (so a page that sets a colour from a token reads the
// themed one). Exactly one position satisfies both, and the page is the
// only thing that knows where it is.
const themeMarker = "<!--wildstone:theme-->"

// InjectTheme puts a themed token block into a page at its marker.
//
// css is produced by keystone/theme, whose parser has already proved
// every value in it is a #rrggbb colour — see the tests there. Nothing
// a household can paste reaches this function as anything else, which
// is what makes writing it into a <style> element safe. A page with no
// marker is returned unchanged rather than patched somewhere else,
// because a token block in the wrong place is worse than none.
func InjectTheme(body []byte, css string) []byte {
	if css == "" || !bytes.Contains(body, []byte(themeMarker)) {
		return body
	}
	return bytes.Replace(body, []byte(themeMarker),
		[]byte("<style>\n"+css+"</style>"), 1)
}

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
	".webp":  "image/webp",
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
//
// REVALIDATED, NOT IMMUTABLE. This used to answer with
// `max-age=31536000, immutable` over a URL that carries no version —
// /portal/assets/nocturne.css and nothing else. Those two together are
// a promise the bytes at that address will never change, and the
// browser keeps it: `immutable` suppresses the conditional request
// even on an ordinary reload. So a stylesheet fix shipped in a new
// Keystone reached a household that had never opened the dashboard,
// and NOBODY ELSE, for a year. That is not a cache, it is a freeze,
// and it silently made every deployed design change a no-op for the
// people who had already visited.
//
// A strong ETag over the bytes restores the conditional request. The
// cost is one 304 per asset per load — a URL space of one stylesheet,
// two fonts and forty icons, on a dashboard served over a household's
// own network — and the benefit is that shipping a fix ships it.
//
// The alternative was a content hash in the URL, which buys back the
// long cache. It is the better answer once these URLs are generated
// rather than written by hand in the page, and it is not this change:
// the icons are built at run time by the scripts in home.html, so
// hashing only the ones spelled out in the markup would leave exactly
// the sort of half-versioned asset space this comment is about.
func WriteAsset(w http.ResponseWriter, r *http.Request, body []byte, contentType string) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", etag)
	// max-age=0 rather than no-store: the copy in the browser is still
	// used, it is just checked first. must-revalidate says so to the
	// proxies a household might put in front of this.
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	// A quoted-string match, and the weak-comparison prefix is accepted
	// because a 304 is a cache decision, not an integrity claim.
	if match := r.Header.Get("If-None-Match"); match != "" {
		for _, candidate := range strings.Split(match, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == etag || candidate == "W/"+etag || candidate == "*" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// imageMagic are the file signatures this surface will accept as a
// picture, and the media type each one is served back as.
//
// SNIFFED, NEVER TRUSTED. A household banner is bytes a member chose,
// served from the same origin as the pages that hold everybody's
// session — so a caller that uploads an HTML file labelled image/png is
// describing a stored cross-site scripting bug, and the only reliable
// answer is to read the bytes. net/http's DetectContentType would do
// most of this, but it also returns text/html for anything it does not
// recognise, and "serve what it guessed" is the behaviour being avoided.
var imageMagic = []struct {
	prefix []byte
	// at is where the signature sits, for the formats that do not lead
	// with theirs.
	at    int
	media string
}{
	{prefix: []byte{0xFF, 0xD8, 0xFF}, media: "image/jpeg"},
	{prefix: []byte("\x89PNG\r\n\x1a\n"), media: "image/png"},
	{prefix: []byte("GIF87a"), media: "image/gif"},
	{prefix: []byte("GIF89a"), media: "image/gif"},
	// WebP is "RIFF????WEBP": the size is in between, so the second half
	// is checked at its own offset.
	{prefix: []byte("WEBP"), at: 8, media: "image/webp"},
}

// ImageType reports the media type of an uploaded picture, read from its
// own bytes, and ok=false for anything this surface will not serve back.
func ImageType(body []byte) (string, bool) {
	for _, sig := range imageMagic {
		end := sig.at + len(sig.prefix)
		if len(body) < end {
			continue
		}
		if string(body[sig.at:end]) != string(sig.prefix) {
			continue
		}
		// WebP's signature is split; the container has to be RIFF too,
		// or "WEBP" eight bytes into something else would pass.
		if sig.media == "image/webp" && (len(body) < 4 || string(body[:4]) != "RIFF") {
			continue
		}
		return sig.media, true
	}
	return "", false
}
