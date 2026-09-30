// Package extract normalizes untrusted public documentation for ADR-W047.
// It performs no network I/O and registers no tools. Output is data, never HTML
// safe for rendering or instructions with authority.
package extract

import (
	"bytes"
	stdhtml "html"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/research/publicfetch"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	mdtext "github.com/yuin/goldmark/text"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const MaxCharacters = 50000

// Document retains the transport's final URL and timestamps without refreshing
// them on extraction or cache reads. Text counts Unicode code points.
type Document struct {
	Source SourceInfo
	Text   string
}

type SourceRef struct {
	SourceID string `json:"sourceId"`
	Revision string `json:"revision"`
}

type SourceInfo struct {
	SourceRef
	URL         string `json:"url"`
	MediaType   string `json:"mediaType"`
	RetrievedAt string `json:"retrievedAt"`
	SourceDate  string `json:"sourceDate,omitempty"`
	Truncated   bool   `json:"truncated"`
}

// Extract accepts only the transport's supported representations. Unknown
// encodings, invalid UTF-8 and binary controls fail closed. HTML without a
// charset follows HTML sniffing; other text defaults to UTF-8.
func Extract(p publicfetch.Page) (Document, error) {
	if len(p.Body) > publicfetch.MaxBytes {
		return Document{}, publicfetch.ErrSize
	}
	if p.MediaType != "text/html" && p.MediaType != "text/markdown" && p.MediaType != "text/plain" {
		return Document{}, publicfetch.ErrUnsupported
	}
	u, err := url.Parse(p.URL)
	if err != nil || len(p.URL) > 2048 || !utf8.ValidString(p.URL) || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || p.RetrievedAt.IsZero() {
		return Document{}, ErrInput
	}
	if bytes.HasPrefix(p.Body, []byte("%PDF-")) {
		return Document{}, publicfetch.ErrUnsupported
	}
	body := p.Body
	label := strings.ToLower(strings.TrimSpace(p.Charset))
	if label != "" && label != "utf-8" && label != "utf8" {
		enc, _ := charset.Lookup(label)
		if enc == nil {
			return Document{}, publicfetch.ErrUnsupported
		}
		body, err = io.ReadAll(io.LimitReader(enc.NewDecoder().Reader(bytes.NewReader(body)), publicfetch.MaxBytes+1))
	} else if label == "" && p.MediaType == "text/html" {
		enc, _, _ := charset.DetermineEncoding(body, "")
		body, err = io.ReadAll(io.LimitReader(enc.NewDecoder().Reader(bytes.NewReader(body)), publicfetch.MaxBytes+1))
	}
	if err != nil || len(body) > publicfetch.MaxBytes || !utf8.Valid(body) {
		return Document{}, publicfetch.ErrUnsupported
	}
	text := strings.TrimPrefix(string(body), "\ufeff")
	if hasControls(text) {
		return Document{}, publicfetch.ErrUnsupported
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	bounded := false
	if p.MediaType != "text/plain" {
		text, bounded, err = extractMarkup(text, u, p.MediaType == "text/markdown")
		if err != nil {
			return Document{}, err
		}
	}
	if hasControls(text) {
		return Document{}, publicfetch.ErrUnsupported
	}
	text = strings.TrimSpace(text)
	runes := []rune(text)
	truncated := bounded || len(runes) > MaxCharacters
	if len(runes) > MaxCharacters {
		text = string(runes[:MaxCharacters])
	}
	source := SourceInfo{URL: p.URL, MediaType: p.MediaType, RetrievedAt: p.RetrievedAt.UTC().Format("2006-01-02T15:04:05Z07:00"), Truncated: truncated}
	if !p.SourceDate.IsZero() {
		source.SourceDate = p.SourceDate.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return Document{Source: source, Text: text}, nil
}

func hasControls(text string) bool {
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return true
		}
	}
	return false
}

// limitedText caps retained output during extraction, before links can expand
// a small hostile input. Keep one sentinel code point to disclose truncation.
type limitedText struct {
	strings.Builder
	count     int
	pending   strings.Builder
	normalize bool
}

func (b *limitedText) appendRune(r rune) {
	if b.count <= MaxCharacters {
		b.WriteRune(r)
		b.count++
	}
}
func (b *limitedText) write(s string) {
	for _, r := range s {
		if b.count > MaxCharacters {
			return
		}
		if unicode.IsSpace(r) {
			if b.count == 0 {
				continue
			}
			if b.normalize {
				if r == '\n' {
					b.pending.Reset()
					b.pending.WriteByte('\n')
				} else if b.pending.Len() == 0 {
					b.pending.WriteByte(' ')
				}
			} else if b.pending.Len() < 4*(MaxCharacters+1) {
				b.pending.WriteRune(r)
			}
			continue
		}
		for _, space := range b.pending.String() {
			b.appendRune(space)
		}
		b.pending.Reset()
		b.appendRune(r)
	}
}

type markupFilter struct {
	out           *limitedText
	base          *url.URL
	suppressed    string
	depth         int
	literalPrefix string
	literals      []string
}

func (f *markupFilter) feed(input string, preserve bool) error {
	z := html.NewTokenizer(strings.NewReader(input))
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if z.Err() != io.EOF {
				return publicfetch.ErrUnsupported
			}
			return nil
		}
		t := z.Token()
		// Character references are decoded by the tokenizer, after the raw
		// response check. Check every token even after the output cap so a
		// discarded suffix cannot conceal a forbidden decoded control.
		if hasControls(t.Data) {
			return publicfetch.ErrUnsupported
		}
		for _, attr := range t.Attr {
			if hasControls(attr.Val) {
				return publicfetch.ErrUnsupported
			}
		}
		if f.suppressed != "" {
			// Only normal/foreign containers nest. Raw-text/RCDATA opening
			// tags in their contents are text, not nested elements.
			switch f.suppressed {
			case "object", "template", "svg", "math":
				if (kind == html.StartTagToken || (kind == html.SelfClosingTagToken && t.Data != "svg" && t.Data != "math")) && t.Data == f.suppressed {
					f.depth++
				}
			}
			if kind == html.EndTagToken && t.Data == f.suppressed {
				f.depth--
				if f.depth == 0 {
					f.suppressed = ""
				}
			}
			continue
		}
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			switch t.Data {
			// Head/body wrappers have optional end/start tags in HTML. Drop
			// their tags, but suppress only actual metadata elements (title,
			// script, style, etc.), allowing implicit body content through.
			case "script", "style", "iframe", "object", "svg", "math", "template", "noscript", "title":
				// HTML ignores the slash on non-void elements. Only foreign
				// SVG/MathML elements here can actually self-close.
				if kind != html.SelfClosingTagToken || (t.Data != "svg" && t.Data != "math") {
					f.suppressed = t.Data
					f.depth = 1
				}
				continue
			case "input":
				for _, a := range t.Attr {
					if a.Key == "type" && strings.EqualFold(a.Val, "password") {
						return publicfetch.ErrBlocked
					}
				}
			case "a":
				if f.out.count <= MaxCharacters {
					for _, a := range t.Attr {
						if a.Key == "href" {
							if link, e := url.Parse(a.Val); e == nil {
								link = f.base.ResolveReference(link)
								if (link.Scheme == "https" || link.Scheme == "http") && link.User == nil && len(link.String()) <= 2048 {
									f.out.write(" (" + link.String() + ") ")
								}
							}
						}
					}
				}
			}
		}
		if kind == html.TextToken {
			if preserve {
				f.writeLiteralText(t.Data)
			} else {
				f.out.write(strings.Join(strings.Fields(t.Data), " "))
				f.out.write(" ")
			}
		} else if kind == html.StartTagToken || kind == html.EndTagToken || kind == html.SelfClosingTagToken {
			switch t.Data {
			case "p", "div", "br", "li", "pre", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
				f.out.write("\n")
			}
		}
	}
}

// Literal markers are restored directly into the bounded text builder, never
// re-tokenized or recursively expanded. This works in both decoded RCDATA and
// undecoded raw-text modes without changing the original Markdown data.
func (f *markupFilter) writeLiteralText(text string) {
	if f.literalPrefix == "" {
		f.out.write(text)
		return
	}
	for len(text) > 0 && f.out.count <= MaxCharacters {
		at := strings.Index(text, f.literalPrefix)
		if at < 0 {
			f.out.write(text)
			return
		}
		f.out.write(text[:at])
		rest := text[at+len(f.literalPrefix):]
		end := strings.IndexByte(rest, '_')
		if end < 0 {
			f.out.write(text[at:])
			return
		}
		index, err := strconv.Atoi(rest[:end])
		if err != nil || index < 0 || index >= len(f.literals) {
			f.out.write(f.literalPrefix)
			text = rest
			continue
		}
		f.out.write(f.literals[index])
		text = rest[end+1:]
	}
}

type markdownPart struct {
	literal bool
	text    string
}

var literalPrefixPattern = regexp.MustCompile(`WS_MD_LITERAL_[0-9]+_`)

// Choose a deterministic prefix absent from both raw and entity-decoded HTML.
// Occupancy scanning is linear in bounded input; trying every prefix against
// the entire input would let crafted examples force quadratic work.
func prepareMarkdown(parts []markdownPart, filter *markupFilter) string {
	occupied := map[string]bool{}
	for _, part := range parts {
		if !part.literal {
			for _, variant := range []string{part.text, stdhtml.UnescapeString(part.text)} {
				for _, prefix := range literalPrefixPattern.FindAllString(variant, -1) {
					occupied[prefix] = true
				}
			}
		}
	}
	for n := 0; ; n++ {
		prefix := "WS_MD_LITERAL_" + strconv.Itoa(n) + "_"
		if !occupied[prefix] {
			filter.literalPrefix = prefix
			break
		}
	}
	var markup strings.Builder
	for _, part := range parts {
		if part.text == "" {
			continue
		}
		if !part.literal {
			markup.WriteString(part.text)
			continue
		}
		index := len(filter.literals)
		filter.literals = append(filter.literals, part.text)
		markup.WriteString(filter.literalPrefix + strconv.Itoa(index) + "_")
	}
	return markup.String()
}

// Markdown parsing identifies actual raw HTML spans. Autolinks, escaped angle
// brackets and code examples remain literal Markdown data. State-safe literal
// markers let one tokenizer retain HTML raw-text/RCDATA state across all nodes,
// including non-suppressed textarea and xmp contents.
func extractMarkup(input string, base *url.URL, markdown bool) (string, bool, error) {
	out := limitedText{normalize: !markdown}
	filter := markupFilter{out: &out, base: base}
	if !markdown {
		if err := filter.feed(input, false); err != nil {
			return "", false, err
		}
		lines := strings.Split(out.String(), "\n")
		var cleaned []string
		for _, line := range lines {
			if line = strings.TrimSpace(line); line != "" {
				cleaned = append(cleaned, line)
			}
		}
		return strings.Join(cleaned, "\n"), out.count > MaxCharacters, nil
	}
	source := []byte(input)
	root := goldmark.DefaultParser().Parse(mdtext.NewReader(source))
	position := 0
	var parts []markdownPart
	err := ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var segments []mdtext.Segment
		switch n := node.(type) {
		case *ast.RawHTML:
			for i := 0; i < n.Segments.Len(); i++ {
				segments = append(segments, n.Segments.At(i))
			}
		case *ast.HTMLBlock:
			for i := 0; i < n.Lines().Len(); i++ {
				segments = append(segments, n.Lines().At(i))
			}
			if n.HasClosure() {
				segments = append(segments, n.ClosureLine)
			}
		}
		if len(segments) == 0 {
			return ast.WalkContinue, nil
		}
		start, end := segments[0].Start, segments[len(segments)-1].Stop
		if start < position {
			return ast.WalkContinue, nil
		}
		parts = append(parts, markdownPart{true, input[position:start]})
		// Container prefixes (quotes/list indentation) are not HTML. Segment
		// values reconstruct the logical tag, including parser padding, while
		// gaps remain inert Markdown outside the HTML tokenizer.
		var raw strings.Builder
		previous := start
		for _, segment := range segments {
			parts = append(parts, markdownPart{true, input[previous:segment.Start]})
			raw.Write(segment.Value(source))
			previous = segment.Stop
		}
		parts = append(parts, markdownPart{false, raw.String()})
		position = end
		return ast.WalkSkipChildren, nil
	})
	if err != nil {
		return "", false, err
	}
	parts = append(parts, markdownPart{true, input[position:]})
	if err := filter.feed(prepareMarkdown(parts, &filter), true); err != nil {
		return "", false, err
	}
	return out.String(), out.count > MaxCharacters, nil
}
