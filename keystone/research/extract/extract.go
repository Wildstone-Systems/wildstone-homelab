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
	if p.MediaType != "text/plain" {
		text, err = extractMarkup(text, u, p.MediaType == "text/markdown")
		if err != nil {
			return Document{}, err
		}
	}
	text = strings.TrimSpace(text)
	runes := []rune(text)
	truncated := len(runes) > MaxCharacters
	if truncated {
		text = string(runes[:MaxCharacters])
	}
	source := SourceInfo{URL: p.URL, MediaType: p.MediaType, RetrievedAt: p.RetrievedAt.UTC().Format("2006-01-02T15:04:05Z07:00"), Truncated: truncated}
	if !p.SourceDate.IsZero() {
		source.SourceDate = p.SourceDate.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return Document{Source: source, Text: text}, nil
}

// Tokenization tolerates malformed markup without executing it. Markdown syntax
// remains text; embedded HTML is stripped by the same conservative filter.
func extractMarkup(input string, base *url.URL, markdown bool) (string, error) {
	z := html.NewTokenizer(strings.NewReader(input))
	var out strings.Builder
	var suppressed string
	depth := 0
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if z.Err() != io.EOF {
				return "", publicfetch.ErrUnsupported
			}
			break
		}
		t := z.Token()
		if suppressed != "" {
			if kind == html.StartTagToken && t.Data == suppressed {
				depth++
			}
			if kind == html.EndTagToken && t.Data == suppressed {
				depth--
				if depth == 0 {
					suppressed = ""
				}
			}
			continue
		}
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			switch t.Data {
			case "script", "style", "iframe", "object", "svg", "math", "template", "noscript", "head":
				if kind != html.SelfClosingTagToken {
					suppressed = t.Data
					depth = 1
				}
				continue
			case "input":
				for _, a := range t.Attr {
					if a.Key == "type" && strings.EqualFold(a.Val, "password") {
						return "", publicfetch.ErrBlocked
					}
				}
			case "a":
				for _, a := range t.Attr {
					if a.Key == "href" {
						if link, e := url.Parse(a.Val); e == nil {
							link = base.ResolveReference(link)
							if (link.Scheme == "https" || link.Scheme == "http") && link.User == nil && len(link.String()) <= 2048 {
								out.WriteString(" (" + link.String() + ") ")
							}
						}
					}
				}
			}
		}
		if kind == html.TextToken {
			if markdown {
				out.WriteString(t.Data)
			} else {
				out.WriteString(strings.Join(strings.Fields(t.Data), " "))
				out.WriteByte(' ')
			}
		} else if kind == html.StartTagToken || kind == html.EndTagToken || kind == html.SelfClosingTagToken {
			switch t.Data {
			case "p", "div", "br", "li", "pre", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
				out.WriteByte('\n')
			}
		}
	}
	lines := strings.Split(out.String(), "\n")
	var cleaned []string
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			cleaned = append(cleaned, line)
		}
	}
	return strings.Join(cleaned, "\n"), nil
}
