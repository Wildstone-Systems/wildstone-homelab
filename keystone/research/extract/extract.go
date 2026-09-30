// Package extract normalizes untrusted public documentation for ADR-W047.
// It performs no network I/O and registers no tools. Output is data, never HTML
// safe for rendering or instructions with authority.
package extract

import (
	"bytes"
	"io"
	"net/url"
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
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return Document{}, publicfetch.ErrUnsupported
		}
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	bounded := false
	if p.MediaType != "text/plain" {
		text, bounded, err = extractMarkup(text, u, p.MediaType == "text/markdown")
		if err != nil {
			return Document{}, err
		}
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
	out        *limitedText
	base       *url.URL
	suppressed string
	depth      int
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
		if f.suppressed != "" {
			if kind == html.StartTagToken && t.Data == f.suppressed {
				f.depth++
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
			case "script", "style", "iframe", "object", "svg", "math", "template", "noscript", "head":
				if kind != html.SelfClosingTagToken {
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
				f.out.write(t.Data)
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

// Markdown parsing identifies actual raw HTML spans. Autolinks, escaped angle
// brackets and code examples remain literal Markdown data. The HTML filter's
// suppression state spans inline tags so inline script bodies are discarded.
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
		if filter.suppressed == "" {
			out.write(input[position:start])
		}
		// Container prefixes (quotes/list indentation) are not HTML. Segment
		// values reconstruct the logical tag, including parser padding, while
		// gaps remain inert Markdown outside the HTML tokenizer.
		var raw strings.Builder
		previous := start
		for _, segment := range segments {
			if filter.suppressed == "" {
				out.write(input[previous:segment.Start])
			}
			raw.Write(segment.Value(source))
			previous = segment.Stop
		}
		if err := filter.feed(raw.String(), true); err != nil {
			return ast.WalkStop, err
		}
		position = end
		return ast.WalkSkipChildren, nil
	})
	if err != nil {
		return "", false, err
	}
	if filter.suppressed == "" {
		out.write(input[position:])
	}
	return out.String(), out.count > MaxCharacters, nil
}
