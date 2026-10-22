// Package extract normalizes untrusted public documentation for ADR-W047.
// It performs no network I/O and registers no tools. Output is data, never HTML
// safe for rendering or instructions with authority.
package extract

import (
	"bytes"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/research/publicfetch"
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
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
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
	count             int
	pending           strings.Builder
	normalize         bool
	lastCR            bool
	generatedBoundary bool
}

func (b *limitedText) appendRune(r rune) {
	if b.count <= MaxCharacters {
		b.WriteRune(r)
		b.count++
	}
}

// Structural boundaries require a newline. Coalesce generated boundaries and
// let a source newline replace the generated one, retaining Markdown whitespace.
func (b *limitedText) boundary() {
	if b.count == 0 || b.count > MaxCharacters || strings.Contains(b.pending.String(), "\n") {
		return
	}
	whitespace := b.pending.String()
	b.pending.Reset()
	b.pending.WriteByte('\n')
	if !b.normalize {
		b.pending.WriteString(whitespace)
	}
	b.generatedBoundary = true
}

func (b *limitedText) write(s string) {
	for _, r := range s {
		// Entity decoding can introduce CR/CRLF after response decoding.
		// Normalize before counting, including a pair split across nodes.
		if r == '\n' && b.lastCR {
			b.lastCR = false
			continue
		}
		b.lastCR = r == '\r'
		if b.lastCR {
			r = '\n'
		}
		if b.count > MaxCharacters {
			return
		}
		if unicode.IsSpace(r) {
			if b.generatedBoundary && r == '\n' {
				whitespace := strings.TrimPrefix(b.pending.String(), "\n")
				b.pending.Reset()
				b.pending.WriteString(whitespace)
				b.generatedBoundary = false
			}
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
		b.generatedBoundary = false
		for _, space := range b.pending.String() {
			b.appendRune(space)
		}
		b.pending.Reset()
		b.appendRune(r)
	}
}

type markupFilter struct {
	out             *limitedText
	base            *url.URL
	literalPrefix   string
	literals        []string
	commentLiterals []bool
}

// Parse a bounded input into the HTML tree rather than approximating HTML's
// implicit closures, foreign-content exits and raw-text states. Walk all nodes
// for decoded controls, while retaining only bounded, non-suppressed text.
func (f *markupFilter) feed(input string, preserve bool) error {
	// Also inspect tokens that tree construction may discard (for example,
	// attributes on a redundant head tag). Tree nodes are checked below too,
	// since foreign-content rules can expose additional decoded text.
	z := html.NewTokenizer(strings.NewReader(input))
	for z.Next() != html.ErrorToken {
		token := z.Token()
		if hasControls(token.Data) {
			return publicfetch.ErrUnsupported
		}
		for _, attr := range token.Attr {
			if hasControls(attr.Val) {
				return publicfetch.ErrUnsupported
			}
		}
	}
	if z.Err() != io.EOF {
		return publicfetch.ErrUnsupported
	}
	root, err := html.Parse(strings.NewReader(input))
	if err != nil {
		return publicfetch.ErrUnsupported
	}
	type frame struct {
		node       *html.Node
		suppressed bool
		inert      bool
		exit       bool
	}
	shadowHosts := make(map[*html.Node]bool)
	stack := []frame{{node: root}}
	for len(stack) > 0 {
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := item.node
		if hasControls(n.Data) {
			return publicfetch.ErrUnsupported
		}
		for _, attr := range n.Attr {
			if hasControls(attr.Val) {
				return publicfetch.ErrUnsupported
			}
		}
		if n.Type == html.ElementNode {
			if !item.exit && !item.inert && n.Namespace == "" {
				switch n.Data {
				case "noscript":
					item.inert = true
				case "template":
					if declarativeShadowTemplate(n) && !shadowHosts[n.Parent] {
						shadowHosts[n.Parent] = true
					} else {
						item.inert = true
					}
				}
			}
			// Retained-text suppression does not make rendered HTML controls
			// inert: foreign integration points and object fallback can contain
			// real HTML inputs. Ordinary templates remain inert documentation;
			// declarative shadow roots can contain active password controls.
			if !item.exit && !item.inert && n.Namespace == "" && n.Data == "input" {
				for _, a := range n.Attr {
					if a.Key == "type" && strings.EqualFold(a.Val, "password") {
						return publicfetch.ErrBlocked
					}
				}
			}
			switch n.Data {
			case "script", "style", "iframe", "object", "svg", "math", "template", "noscript", "title":
				item.suppressed = true
			}
		}
		// Generated comments retain their position inside tables; ordinary
		// source comments remain discarded. The prefix is collision-checked.
		if preserve && !item.exit && n.Type == html.CommentNode && strings.HasPrefix(n.Data, f.literalPrefix) {
			if err := f.writeLiteralText(n.Data, false, !item.suppressed); err != nil {
				return err
			}
			continue
		}
		if preserve && n.Type == html.TextNode && !item.exit {
			rcdata := n.Parent != nil && n.Parent.Namespace == "" && (n.Parent.Data == "textarea" || n.Parent.Data == "title")
			if err := f.writeLiteralText(n.Data, rcdata, !item.suppressed); err != nil {
				return err
			}
			continue
		}
		if !item.suppressed {
			if n.Type == html.TextNode && !item.exit {
				f.out.write(strings.Map(func(r rune) rune {
					if unicode.IsSpace(r) && r != '\r' && r != '\n' {
						return ' '
					}
					return r
				}, n.Data))
			} else if n.Type == html.ElementNode {
				if !item.exit {
					switch n.Data {
					case "td", "th":
						// Cells are separate columns even when their source has
						// no intervening whitespace. Keep inline text continuous.
						for previous := n.PrevSibling; previous != nil; previous = previous.PrevSibling {
							if previous.Type == html.ElementNode && (previous.Data == "td" || previous.Data == "th") {
								if f.out.pending.Len() == 0 {
									f.out.write(" ")
								}
								break
							}
						}
					case "a":
						if f.out.count <= MaxCharacters {
							for _, a := range n.Attr {
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
				switch n.Data {
				case "address", "article", "aside", "blockquote", "br", "button", "caption", "center",
					"dd", "details", "dialog", "dir", "div", "dl", "dt", "fieldset", "figcaption", "figure",
					"footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header", "hgroup", "hr",
					"legend", "li", "main", "menu", "nav", "ol", "option", "optgroup", "p", "pre", "search", "section", "summary",
					"table", "tbody", "textarea", "tfoot", "thead", "tr", "ul":
					f.out.boundary()
				}
			}
		}
		if !item.exit {
			stack = append(stack, frame{node: n, suppressed: item.suppressed, inert: item.inert, exit: true})
			for child := n.LastChild; child != nil; child = child.PrevSibling {
				stack = append(stack, frame{node: child, suppressed: item.suppressed, inert: item.inert})
			}
		}
	}
	return nil
}

// x/net/html retains declarative shadow templates as ordinary tree nodes.
// Inspect the first root for each eligible host without executing custom-element
// definitions. Hyphenated custom-host candidates are checked conservatively.
func declarativeShadowTemplate(n *html.Node) bool {
	host := n.Parent
	if host == nil || host.Type != html.ElementNode || host.Namespace != "" {
		return false
	}
	eligible := strings.Contains(host.Data, "-")
	switch host.Data {
	case "article", "aside", "blockquote", "body", "div", "footer", "h1", "h2", "h3", "h4", "h5", "h6", "header", "main", "nav", "p", "section", "span":
		eligible = true
	}
	if !eligible {
		return false
	}
	for _, attr := range n.Attr {
		if attr.Namespace == "" && attr.Key == "shadowrootmode" {
			return strings.EqualFold(attr.Val, "open") || strings.EqualFold(attr.Val, "closed")
		}
	}
	return false
}

// Restore markers as data, never markup or recursively expanded markers.
// RCDATA decodes references in restored literals; ordinary Markdown and HTML
// raw-text do not. Inspect every decoded literal even when output is full or
// suppressed so the retained-text cap cannot hide a forbidden control.
func (f *markupFilter) writeLiteralText(text string, rcdata, retain bool) error {
	write := func(value string, literal bool) error {
		if literal && rcdata {
			value = decodeRCDATA(value)
		}
		if hasControls(value) {
			return publicfetch.ErrUnsupported
		}
		if retain {
			f.out.write(value)
		}
		return nil
	}
	if f.literalPrefix == "" {
		return write(text, false)
	}
	for len(text) > 0 {
		at := strings.Index(text, f.literalPrefix)
		if at < 0 {
			return write(text, false)
		}
		before := text[:at]
		rest := text[at+len(f.literalPrefix):]
		end := strings.IndexByte(rest, '_')
		if end < 0 {
			return write(text, false)
		}
		index, err := strconv.Atoi(rest[:end])
		if err != nil || index < 0 || index >= len(f.literals) {
			if err := write(before+f.literalPrefix, false); err != nil {
				return err
			}
			text = rest
			continue
		}
		after := rest[end+1:]
		// In raw-text/RCDATA, generated comment wrappers remain text rather
		// than comment nodes. Remove only wrappers around known gap markers.
		if f.commentLiterals[index] && strings.HasSuffix(before, "<!--") && strings.HasPrefix(after, "-->") {
			before = strings.TrimSuffix(before, "<!--")
			after = strings.TrimPrefix(after, "-->")
		}
		if err := write(before, false); err != nil {
			return err
		}
		if err := write(f.literals[index], true); err != nil {
			return err
		}
		text = after
	}
	return nil
}

// Use the same character-reference decoder as the HTML parser, including
// semicolonless numeric references. Escaping only '<' keeps literal markup
// inert and prevents a literal closing tag from ending the RCDATA context.
func decodeRCDATA(value string) string {
	z := html.NewTokenizerFragment(strings.NewReader(strings.ReplaceAll(value, "<", "&lt;")), "textarea")
	if z.Next() == html.TextToken {
		return z.Token().Data
	}
	return ""
}

type markdownPart struct {
	gap     bool
	literal bool
	text    string
}

// Keep logical HTML segments and source container gaps distinct. Gap markers
// are positioned after assembling the full stream so tokenizer state is not
// reset between Goldmark nodes.
func appendHTMLParts(parts []markdownPart, segments []mdtext.Segment, source []byte) []markdownPart {
	previous := segments[0].Start
	for _, segment := range segments {
		if previous < segment.Start {
			parts = append(parts, markdownPart{gap: true, literal: true, text: string(source[previous:segment.Start])})
		}
		parts = append(parts, markdownPart{text: string(segment.Value(source))})
		previous = segment.Stop
	}
	return parts
}

var literalPrefixPattern = regexp.MustCompile(`WS_MD_LITERAL_[0-9]+_`)

// Choose a deterministic prefix absent from both raw and entity-decoded HTML.
// Occupancy scanning is linear in bounded input; trying every prefix against
// the entire input would let crafted examples force quadratic work.
func prepareMarkdown(parts []markdownPart, filter *markupFilter) (preparedHTML, logicalHTML string) {
	occupied := map[string]bool{}
	for _, part := range parts {
		if !part.literal {
			for _, variant := range []string{part.text, decodeRCDATA(part.text)} {
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
	type gap struct {
		offset int
		marker string
	}
	var gaps []gap
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
		filter.commentLiterals = append(filter.commentLiterals, part.gap)
		marker := filter.literalPrefix + strconv.Itoa(index) + "_"
		if part.gap {
			gaps = append(gaps, gap{markup.Len(), "<!--" + marker + "-->"})
		} else {
			markup.WriteString(marker)
		}
	}
	// Locate gaps in one continuous stream, retaining RCDATA/raw-text state.
	// Only gaps within actual tag syntax move to that tag's start; tag-like
	// text inside textarea/xmp stays data and keeps its original gap position.
	logical := markup.String()
	z := html.NewTokenizer(strings.NewReader(logical))
	offset, nextGap := 0, 0
	for kind := z.Next(); kind != html.ErrorToken; kind = z.Next() {
		end := offset + len(z.Raw())
		for nextGap < len(gaps) && gaps[nextGap].offset < end {
			if gaps[nextGap].offset > offset && kind != html.TextToken {
				gaps[nextGap].offset = offset
			}
			nextGap++
		}
		offset = end
	}
	var prepared strings.Builder
	position := 0
	for _, gap := range gaps {
		prepared.WriteString(logical[position:gap.offset])
		prepared.WriteString(gap.marker)
		position = gap.offset
	}
	prepared.WriteString(logical[position:])
	return prepared.String(), logical
}

// Markdown parsing identifies actual raw HTML spans. Autolinks, escaped angle
// brackets and code examples remain literal Markdown data. State-safe literal
// markers let one HTML parser retain raw-text/RCDATA state across all nodes,
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
		parts = append(parts, markdownPart{literal: true, text: input[position:start]})
		parts = appendHTMLParts(parts, segments, source)
		position = end
		return ast.WalkSkipChildren, nil
	})
	if err != nil {
		return "", false, err
	}
	parts = append(parts, markdownPart{literal: true, text: input[position:]})
	prepared, logical := prepareMarkdown(parts, &filter)
	// Validate the logical HTML before reinserting source-formatting gaps.
	// Its tree has authoritative namespace/integration-point semantics;
	// formatting markers must never rewrite an active input's attributes.
	// Retain no output in this pass, but still inspect all decoded literals.
	validation := filter
	validation.out = &limitedText{count: MaxCharacters + 1}
	if err := validation.feed(logical, true); err != nil {
		return "", false, err
	}
	if err := filter.feed(prepared, true); err != nil {
		return "", false, err
	}
	return out.String(), out.count > MaxCharacters, nil
}
