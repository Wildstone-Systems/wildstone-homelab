package mimenorm

import (
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlToText reduces HTML to readable text with a tokenizer — no DOM,
// no CSS engine, no network. What it keeps is what a person reading the
// rendered mail with remote content off would see; what it drops, it
// counts on msg:
//
//   - every image (remote or inline): images carry no text, and a
//     remote one is a read receipt. Tiny or invisible ones are counted
//     as tracking pixels;
//   - script, style, head, template and embedded-object content;
//   - elements a renderer would hide (hidden attribute, display:none,
//     visibility:hidden, zero font size or opacity) — the favourite
//     hiding place for instructions aimed at a model;
//   - links become inert text: "label <url>", http/https/mailto only,
//     with known tracking parameters removed and nothing fetched.
func htmlToText(src string, msg *Message) string {
	if src == "" {
		return ""
	}
	z := html.NewTokenizer(strings.NewReader(src))
	var (
		b strings.Builder
		// stack holds open element names; hiddenAt is the stack depth
		// at which a hidden or skipped subtree began (-1 when none).
		stack    []string
		hiddenAt = -1
		href     string
		linkText strings.Builder
		inLink   bool
	)
	const maxStack = 512
	write := func(s string) {
		if inLink {
			linkText.WriteString(s)
			return
		}
		b.WriteString(s)
	}
	newline := func() {
		if inLink {
			linkText.WriteByte(' ')
			return
		}
		b.WriteByte('\n')
	}
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return finishHTMLText(b.String())
		case html.TextToken:
			if hiddenAt >= 0 {
				continue
			}
			write(collapseSpace(string(z.Text())))
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := atom.Lookup(name)
			attrs := readAttrs(z, hasAttr)
			void := tt == html.SelfClosingTagToken || voidElement(tag)

			if tag == atom.Img || tag == atom.Image {
				if hiddenAt < 0 {
					msg.RemoteContentBlocked++
					if trackingPixel(attrs) {
						msg.TrackingPixels++
					}
				}
				continue
			}
			if hiddenAt < 0 && (tag == atom.Link || tag == atom.Source || tag == atom.Track) {
				// Stylesheets, media sources: remote references with no text.
				if attrs["href"] != "" || attrs["src"] != "" {
					msg.RemoteContentBlocked++
				}
			}
			if void {
				if hiddenAt < 0 {
					if tag == atom.Br || tag == atom.Hr {
						newline()
					}
				}
				continue
			}
			if len(stack) >= maxStack {
				// Pathological nesting: stop descending, keep text flat.
				continue
			}
			stack = append(stack, string(name))
			if hiddenAt >= 0 {
				continue
			}
			if skippedElement(tag) {
				hiddenAt = len(stack) - 1
				if tag == atom.Iframe || tag == atom.Object || tag == atom.Embed || tag == atom.Video || tag == atom.Audio {
					msg.RemoteContentBlocked++
				}
				continue
			}
			if hiddenElement(attrs) {
				hiddenAt = len(stack) - 1
				msg.HiddenContentRemoved++
				continue
			}
			if blockElement(tag) {
				newline()
			}
			switch tag {
			case atom.Li:
				write("- ")
			case atom.Td, atom.Th:
				write(" ")
			case atom.A:
				if !inLink {
					href = safeLink(attrs["href"])
					inLink = true
					linkText.Reset()
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := atom.Lookup(name)
			// Pop to the matching open element, tolerating bad nesting.
			idx := -1
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i] == string(name) {
					idx = i
					break
				}
			}
			if idx < 0 {
				continue
			}
			stack = stack[:idx]
			if hiddenAt >= 0 {
				if idx <= hiddenAt {
					hiddenAt = -1
				}
				continue
			}
			if tag == atom.A && inLink {
				inLink = false
				label := strings.TrimSpace(collapseSpace(linkText.String()))
				switch {
				case href == "":
					b.WriteString(label)
				case label == "" || label == href:
					b.WriteString(href)
				default:
					b.WriteString(label + " <" + href + ">")
				}
			}
			if blockElement(tag) {
				b.WriteByte('\n')
			}
		}
	}
}

func finishHTMLText(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return sanitizeText(strings.Join(lines, "\n"))
}

func readAttrs(z *html.Tokenizer, more bool) map[string]string {
	attrs := map[string]string{}
	for n := 0; more && n < 64; n++ {
		var k, v []byte
		k, v, more = z.TagAttr()
		attrs[strings.ToLower(string(k))] = string(v)
	}
	return attrs
}

func collapseSpace(s string) string {
	if s == "" {
		return s
	}
	lead := s[0] == ' ' || s[0] == '\n' || s[0] == '\t' || s[0] == '\r'
	last := s[len(s)-1]
	trail := last == ' ' || last == '\n' || last == '\t' || last == '\r'
	f := strings.Join(strings.Fields(s), " ")
	if f == "" {
		return " "
	}
	if lead {
		f = " " + f
	}
	if trail {
		f += " "
	}
	return f
}

func voidElement(a atom.Atom) bool {
	switch a {
	case atom.Area, atom.Base, atom.Br, atom.Col, atom.Embed, atom.Hr, atom.Img,
		atom.Input, atom.Link, atom.Meta, atom.Param, atom.Source, atom.Track, atom.Wbr:
		return true
	}
	return false
}

func skippedElement(a atom.Atom) bool {
	switch a {
	case atom.Script, atom.Style, atom.Head, atom.Title, atom.Template, atom.Noscript,
		atom.Svg, atom.Math, atom.Iframe, atom.Object, atom.Video, atom.Audio,
		atom.Canvas, atom.Select, atom.Textarea, atom.Button, atom.Form:
		return true
	}
	return false
}

func blockElement(a atom.Atom) bool {
	switch a {
	case atom.P, atom.Div, atom.Li, atom.Ul, atom.Ol, atom.Tr, atom.Table, atom.H1,
		atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Blockquote, atom.Pre,
		atom.Section, atom.Article, atom.Header, atom.Footer, atom.Dt, atom.Dd, atom.Center:
		return true
	}
	return false
}

// hiddenElement reports markup a renderer would not show.
func hiddenElement(attrs map[string]string) bool {
	if _, ok := attrs["hidden"]; ok {
		return true
	}
	// aria-hidden alone is an accessibility hint that often wraps
	// visible decoration, so it is not treated as hiding.
	return styleHides(attrs["style"])
}

func styleHides(style string) bool {
	s := strings.ToLower(strings.Join(strings.Fields(style), ""))
	for _, decl := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		v = strings.TrimSuffix(v, "!important")
		switch k {
		case "display":
			if v == "none" {
				return true
			}
		case "visibility":
			if v == "hidden" || v == "collapse" {
				return true
			}
		case "opacity":
			if f, err := strconv.ParseFloat(v, 64); err == nil && f <= 0.05 {
				return true
			}
		case "font-size", "max-height", "height", "width", "max-width":
			if zeroLength(v) {
				return true
			}
		}
	}
	return false
}

func zeroLength(v string) bool {
	v = strings.TrimRight(v, "abcdefghijklmnopqrstuvwxyz%")
	f, err := strconv.ParseFloat(v, 64)
	return err == nil && f <= 0
}

// trackingPixel reports an image sized to be invisible.
func trackingPixel(attrs map[string]string) bool {
	tiny := func(v string) bool {
		v = strings.TrimSuffix(strings.TrimSpace(v), "px")
		f, err := strconv.ParseFloat(v, 64)
		return err == nil && f <= 1
	}
	if tiny(attrs["width"]) || tiny(attrs["height"]) {
		return true
	}
	return styleHides(attrs["style"]) || strings.Contains(strings.ToLower(strings.ReplaceAll(attrs["style"], " ", "")), "width:1px")
}

// trackingParams are query parameters that identify the reader, not
// the resource.
var trackingParams = []string{"utm_", "mc_", "_hs", "mkt_tok", "fbclid", "gclid", "dclid", "msclkid", "yclid", "igshid", "vero_", "oly_", "trk", "ref_src"}

// safeLink renders a link target as inert text, or "" when it should
// not be shown at all.
func safeLink(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "mailto":
		return boundString(sanitizeLine(u.String()), 512)
	default:
		return ""
	}
	u.User = nil
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		for _, p := range trackingParams {
			if strings.HasPrefix(lk, p) {
				q.Del(k)
				break
			}
		}
	}
	u.RawQuery = q.Encode()
	return boundString(sanitizeLine(u.String()), 512)
}
