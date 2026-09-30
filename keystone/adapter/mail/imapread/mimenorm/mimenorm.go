// Package mimenorm turns one raw RFC 5322 message into bounded,
// normalized evidence (epic #326, slice S04).
//
// Mail is untrusted input written by strangers, so this package is
// built around limits rather than around features. Every byte that
// leaves it has passed a cap: the header section, the number of parts,
// the nesting depth, the decoded text, each calendar part. A message
// that breaks a cap is not dropped and not partially trusted; it comes
// back with an explicit Outcome (malformed, oversized, too_deep) and
// whatever bounded evidence was gathered before the cap was hit, so a
// caller can report it instead of treating it as irrelevant.
//
// What it produces:
//
//   - a fixed allowlist of decoded headers (never arbitrary X- headers,
//     which a sender controls and a reader might mistake for metadata);
//   - one plain-text body: text/plain parts when there are any, else
//     HTML reduced to text with no remote content — images are dropped,
//     links are kept as inert text with tracking parameters removed,
//     hidden and scripted content is discarded;
//   - text/calendar and .ics parts, decoded but otherwise raw, for the
//     deterministic invitation parser downstream;
//   - a list of every other part (name, type, size) marked unsupported,
//     so "there was a PDF we did not read" is visible rather than lost.
//
// It performs no I/O and fetches nothing. Content is data: nothing in a
// message can change what this package does beyond which bounded branch
// it takes.
package mimenorm

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// Outcome is the closed set of normalization results.
type Outcome string

const (
	// OK means every part was within limits and parsed.
	OK Outcome = "ok"
	// Malformed means the message's structure could not be parsed as
	// declared (a multipart with no boundary, a broken header section,
	// an unknown transfer encoding). Evidence gathered before the fault
	// is kept.
	Malformed Outcome = "malformed"
	// Oversized means the message, its header section or its part count
	// exceeded a limit.
	Oversized Outcome = "oversized"
	// TooDeep means multipart or message/rfc822 nesting exceeded
	// Limits.MaxDepth.
	TooDeep Outcome = "too_deep"
)

// Limits bounds everything this package reads. The defaults are the
// epic #326 brief's: headers 64 KiB, text 256 KiB, message 10 MiB,
// depth 16, parts 100.
type Limits struct {
	MaxMessageBytes  int64
	MaxHeaderBytes   int
	MaxTextBytes     int
	MaxDepth         int
	MaxParts         int
	MaxCalendarBytes int
	// MaxHTMLBytes bounds how much decoded HTML is tokenized before it
	// is reduced to text. HTML is markup-heavy, so it is allowed to be
	// larger than the text it produces.
	MaxHTMLBytes int
}

// DefaultLimits returns the brief's limits.
func DefaultLimits() Limits {
	return Limits{
		MaxMessageBytes:  10 << 20,
		MaxHeaderBytes:   64 << 10,
		MaxTextBytes:     256 << 10,
		MaxDepth:         16,
		MaxParts:         100,
		MaxCalendarBytes: 256 << 10,
		MaxHTMLBytes:     1 << 20,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxMessageBytes <= 0 {
		l.MaxMessageBytes = d.MaxMessageBytes
	}
	if l.MaxHeaderBytes <= 0 {
		l.MaxHeaderBytes = d.MaxHeaderBytes
	}
	if l.MaxTextBytes <= 0 {
		l.MaxTextBytes = d.MaxTextBytes
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxParts <= 0 {
		l.MaxParts = d.MaxParts
	}
	if l.MaxCalendarBytes <= 0 {
		l.MaxCalendarBytes = d.MaxCalendarBytes
	}
	if l.MaxHTMLBytes <= 0 {
		l.MaxHTMLBytes = d.MaxHTMLBytes
	}
	return l
}

// maxHeaderValue caps any single surfaced header value, after decoding.
const maxHeaderValue = 2048

// maxAddresses caps each surfaced address list.
const maxAddresses = 50

// Address is one decoded mailbox.
type Address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// Headers is the fixed allowlist of surfaced headers, decoded.
type Headers struct {
	MessageID  string    `json:"messageId,omitempty"`
	Date       time.Time `json:"date,omitempty"`
	DateRaw    string    `json:"dateRaw,omitempty"`
	From       []Address `json:"from,omitempty"`
	Sender     []Address `json:"sender,omitempty"`
	ReplyTo    []Address `json:"replyTo,omitempty"`
	To         []Address `json:"to,omitempty"`
	Cc         []Address `json:"cc,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	InReplyTo  string    `json:"inReplyTo,omitempty"`
	References []string  `json:"references,omitempty"`
	ListID     string    `json:"listId,omitempty"`
	// AutoSubmitted is the RFC 3834 value, useful to tell bulk mail from
	// a person's message. It is a hint, never an authority.
	AutoSubmitted string `json:"autoSubmitted,omitempty"`
}

// CalendarPart is one text/calendar or .ics part, decoded to UTF-8 but
// otherwise raw.
type CalendarPart struct {
	Name        string `json:"name,omitempty"`
	ContentType string `json:"contentType"`
	// Method is the iTIP METHOD content-type parameter (REQUEST, CANCEL…)
	// when the sender declared one.
	Method string `json:"method,omitempty"`
	Data   string `json:"data,omitempty"`
	// Oversized marks a part larger than Limits.MaxCalendarBytes. Its
	// Data is empty: a truncated calendar would parse to the wrong event.
	Oversized bool `json:"oversized,omitempty"`
}

// Attachment is a part this package does not read, reported so the
// caller can say so.
type Attachment struct {
	Name        string `json:"name,omitempty"`
	ContentType string `json:"contentType"`
	// Size is the decoded size, or -1 when the part was not read to the
	// end (a limit or a fault stopped it).
	Size int64 `json:"size"`
	// Reason is why it was not read: unsupported, unknown_encoding.
	Reason string `json:"reason"`
}

// Message is the normalized evidence for one message.
type Message struct {
	Outcome Outcome `json:"outcome"`
	// Reason names the limit or fault behind a non-ok Outcome.
	Reason  string  `json:"reason,omitempty"`
	Headers Headers `json:"headers"`
	// Text is the normalized body, at most Limits.MaxTextBytes.
	Text string `json:"text,omitempty"`
	// TextSource says where Text came from: plain, html, or empty.
	TextSource    string         `json:"textSource,omitempty"`
	TextTruncated bool           `json:"textTruncated,omitempty"`
	Calendars     []CalendarPart `json:"calendars,omitempty"`
	Attachments   []Attachment   `json:"attachments,omitempty"`
	// RemoteContentBlocked counts images and other remote references
	// removed from HTML. TrackingPixels is the subset that looked like
	// tracking beacons (tiny or invisible images).
	RemoteContentBlocked int `json:"remoteContentBlocked,omitempty"`
	TrackingPixels       int `json:"trackingPixels,omitempty"`
	// HiddenContentRemoved counts HTML elements dropped because a
	// reader would not see them (display:none and friends) — the usual
	// hiding place for text aimed at a model rather than a person.
	HiddenContentRemoved int `json:"hiddenContentRemoved,omitempty"`
	// Parts is how many MIME parts were visited.
	Parts int `json:"parts"`
}

// Normalize parses raw with the given limits. It never returns nil and
// never panics on hostile input; every problem becomes an Outcome.
func Normalize(raw []byte, limits Limits) *Message {
	limits = limits.withDefaults()
	n := &normalizer{limits: limits, msg: &Message{Outcome: OK}}
	if int64(len(raw)) > limits.MaxMessageBytes {
		// Headers only: they are what a caller needs to say which
		// message was too large.
		n.fail(Oversized, "message")
		if head, _, ok := splitHeader(raw, limits.MaxHeaderBytes); ok {
			if h, err := parseHeaderBlock(head); err == nil {
				n.msg.Headers = surfaceHeaders(h)
			}
		}
		return n.msg
	}
	n.run(raw)
	return n.msg
}

// NormalizeHeaders parses a header section alone — used for messages
// whose bodies are never fetched (oversized, or outside the filters).
func NormalizeHeaders(raw []byte, limits Limits) (Headers, Outcome, string) {
	limits = limits.withDefaults()
	head, _, ok := splitHeader(raw, limits.MaxHeaderBytes)
	if !ok {
		if len(raw) > limits.MaxHeaderBytes {
			return Headers{}, Oversized, "headers"
		}
		// A header-only fetch may legitimately lack the blank line.
		head = raw
	}
	h, err := parseHeaderBlock(head)
	if err != nil {
		return Headers{}, Malformed, "headers"
	}
	return surfaceHeaders(h), OK, ""
}

type normalizer struct {
	limits   Limits
	msg      *Message
	plain    strings.Builder
	html     strings.Builder
	textFull bool
}

// fail records the first non-ok outcome. Later faults do not overwrite
// it: the first is the one that stopped normal processing.
func (n *normalizer) fail(o Outcome, reason string) {
	if n.msg.Outcome == OK {
		n.msg.Outcome = o
		n.msg.Reason = reason
	}
}

func (n *normalizer) run(raw []byte) {
	head, body, ok := splitHeader(raw, n.limits.MaxHeaderBytes)
	if !ok {
		if len(raw) > n.limits.MaxHeaderBytes {
			n.fail(Oversized, "headers")
		} else {
			n.fail(Malformed, "headers")
		}
		return
	}
	h, err := parseHeaderBlock(head)
	if err != nil {
		n.fail(Malformed, "headers")
		return
	}
	n.msg.Headers = surfaceHeaders(h)
	n.part(h, body, 0)
	n.finish()
}

func (n *normalizer) finish() {
	text, source := n.plain.String(), "plain"
	if strings.TrimSpace(text) == "" {
		text, source = htmlToText(n.html.String(), n.msg), "html"
	}
	text = strings.TrimSpace(collapseBlankLines(text))
	if text == "" {
		source = ""
	}
	if len(text) > n.limits.MaxTextBytes {
		text = truncateUTF8(text, n.limits.MaxTextBytes)
		n.msg.TextTruncated = true
	}
	if n.textFull {
		n.msg.TextTruncated = true
	}
	n.msg.Text, n.msg.TextSource = text, source
}

// part handles one entity (the top-level message or a nested part).
func (n *normalizer) part(h textproto.MIMEHeader, body []byte, depth int) {
	if n.msg.Parts >= n.limits.MaxParts {
		n.fail(Oversized, "parts")
		return
	}
	n.msg.Parts++
	if depth > n.limits.MaxDepth {
		n.fail(TooDeep, "depth")
		return
	}

	mediaType, params := contentType(h)
	disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	name := partName(params, dparams)

	if strings.HasPrefix(mediaType, "multipart/") {
		n.multipart(params["boundary"], body, depth)
		return
	}
	if mediaType == "message/rfc822" || mediaType == "message/global" {
		decoded, err := decodeTransfer(h.Get("Content-Transfer-Encoding"), body, int(n.limits.MaxMessageBytes))
		if err != nil {
			n.fail(Malformed, "transfer_encoding")
			return
		}
		inner, innerBody, ok := splitHeader(decoded, n.limits.MaxHeaderBytes)
		if !ok {
			n.fail(Malformed, "nested_headers")
			return
		}
		ih, err := parseHeaderBlock(inner)
		if err != nil {
			n.fail(Malformed, "nested_headers")
			return
		}
		n.part(ih, innerBody, depth+1)
		return
	}

	isCalendar := mediaType == "text/calendar" || mediaType == "application/ics" ||
		strings.EqualFold(path.Ext(name), ".ics")
	attachment := strings.EqualFold(disposition, "attachment")

	switch {
	case isCalendar:
		n.calendar(h, mediaType, params, name, body)
	case mediaType == "text/plain" && !attachment:
		n.text(h, params, body, &n.plain)
	case mediaType == "text/html" && !attachment:
		n.text(h, params, body, &n.html)
	default:
		n.attachment(h, mediaType, name, body, "unsupported")
	}
}

func (n *normalizer) multipart(boundary string, body []byte, depth int) {
	if boundary == "" {
		n.fail(Malformed, "multipart_boundary")
		return
	}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	sawPart := false
	for {
		p, err := r.NextRawPart()
		if errors.Is(err, io.EOF) {
			if !sawPart {
				n.fail(Malformed, "multipart_empty")
			}
			return
		}
		if err != nil {
			n.fail(Malformed, "multipart")
			return
		}
		sawPart = true
		// A raw part's body is bounded by the enclosing body, which is
		// bounded by MaxMessageBytes.
		content, err := io.ReadAll(p)
		if err != nil {
			n.fail(Malformed, "multipart")
			return
		}
		n.part(p.Header, content, depth+1)
		if n.msg.Outcome == Oversized || n.msg.Outcome == TooDeep {
			// Stop walking: the caps exist to bound work.
			return
		}
	}
}

func (n *normalizer) text(h textproto.MIMEHeader, params map[string]string, body []byte, into *strings.Builder) {
	budget := n.limits.MaxTextBytes
	if into == &n.html {
		budget = n.limits.MaxHTMLBytes
	}
	// Decode a little past the budget so truncation is detectable.
	decoded, err := decodeTransfer(h.Get("Content-Transfer-Encoding"), body, budget*4+4)
	if err != nil {
		n.attachment(h, "text/unknown-encoding", "", nil, "unknown_encoding")
		n.fail(Malformed, "transfer_encoding")
		return
	}
	s := toUTF8(decoded, params["charset"])
	s = sanitizeText(s)
	room := budget - into.Len()
	if room <= 0 {
		n.textFull = true
		return
	}
	if len(s) > room {
		s = truncateUTF8(s, room)
		n.textFull = true
	}
	if into.Len() > 0 {
		into.WriteString("\n\n")
	}
	into.WriteString(s)
}

func (n *normalizer) calendar(h textproto.MIMEHeader, mediaType string, params map[string]string, name string, body []byte) {
	part := CalendarPart{Name: name, ContentType: mediaType, Method: strings.ToUpper(params["method"])}
	decoded, err := decodeTransfer(h.Get("Content-Transfer-Encoding"), body, n.limits.MaxCalendarBytes+1)
	switch {
	case err != nil:
		n.attachment(h, mediaType, name, nil, "unknown_encoding")
		n.fail(Malformed, "transfer_encoding")
		return
	case len(decoded) > n.limits.MaxCalendarBytes:
		part.Oversized = true
	default:
		part.Data = strings.ToValidUTF8(toUTF8(decoded, params["charset"]), "�")
	}
	n.msg.Calendars = append(n.msg.Calendars, part)
}

func (n *normalizer) attachment(h textproto.MIMEHeader, mediaType, name string, body []byte, reason string) {
	a := Attachment{Name: name, ContentType: mediaType, Size: -1, Reason: reason}
	if body != nil {
		if size, err := decodedSize(h.Get("Content-Transfer-Encoding"), body); err == nil {
			a.Size = size
		} else {
			a.Reason = "unknown_encoding"
		}
	}
	n.msg.Attachments = append(n.msg.Attachments, a)
}

// splitHeader finds the end of the header section within max bytes.
func splitHeader(raw []byte, max int) (head, body []byte, ok bool) {
	window := raw
	if len(window) > max+4 {
		window = window[:max+4]
	}
	i, sep := bytes.Index(window, []byte("\r\n\r\n")), 4
	if j := bytes.Index(window, []byte("\n\n")); j >= 0 && (i < 0 || j < i) {
		i, sep = j, 2
	}
	if i < 0 {
		// A message consisting only of headers is legal.
		if len(raw) <= max && len(raw) > 0 {
			return raw, nil, true
		}
		return nil, nil, false
	}
	if i > max {
		return nil, nil, false
	}
	return raw[:i+sep/2], raw[i+sep:], true
}

func parseHeaderBlock(head []byte) (textproto.MIMEHeader, error) {
	// ReadMIMEHeader wants the terminating blank line.
	buf := make([]byte, 0, len(head)+4)
	buf = append(buf, head...)
	if !bytes.HasSuffix(buf, []byte("\n")) {
		buf = append(buf, '\r', '\n')
	}
	buf = append(buf, '\r', '\n')
	h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(buf))).ReadMIMEHeader()
	if err != nil && len(h) == 0 {
		return nil, err
	}
	if h == nil {
		h = textproto.MIMEHeader{}
	}
	return h, nil
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return nil, fmt.Errorf("mimenorm: unknown charset %q", charset)
	}
	return enc.NewDecoder().Reader(input), nil
}

func decodeHeader(v string) string {
	d, err := wordDecoder.DecodeHeader(v)
	if err != nil {
		d = v
	}
	return boundString(sanitizeLine(d), maxHeaderValue)
}

func surfaceHeaders(h textproto.MIMEHeader) Headers {
	out := Headers{
		MessageID:     normalizeMsgID(h.Get("Message-Id")),
		Subject:       decodeHeader(h.Get("Subject")),
		InReplyTo:     normalizeMsgID(h.Get("In-Reply-To")),
		ListID:        decodeHeader(h.Get("List-Id")),
		AutoSubmitted: decodeHeader(h.Get("Auto-Submitted")),
		From:          addresses(h.Get("From")),
		Sender:        addresses(h.Get("Sender")),
		ReplyTo:       addresses(h.Get("Reply-To")),
		To:            addresses(h.Get("To")),
		Cc:            addresses(h.Get("Cc")),
	}
	if raw := strings.TrimSpace(h.Get("Date")); raw != "" {
		if t, err := mail.ParseDate(raw); err == nil {
			out.Date = t.UTC()
		}
		out.DateRaw = boundString(sanitizeLine(raw), 128)
	}
	for _, ref := range strings.Fields(h.Get("References")) {
		if len(out.References) == 32 {
			break
		}
		if id := normalizeMsgID(ref); id != "" {
			out.References = append(out.References, id)
		}
	}
	return out
}

var addressParser = &mail.AddressParser{WordDecoder: wordDecoder}

func addresses(v string) []Address {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	list, err := addressParser.ParseList(v)
	if err != nil {
		// Keep a bounded, clearly-unparsed rendering rather than
		// nothing: an unparsable From is itself evidence.
		return []Address{{Name: decodeHeader(v)}}
	}
	var out []Address
	for _, a := range list {
		if len(out) == maxAddresses {
			break
		}
		out = append(out, Address{
			Name:    boundString(sanitizeLine(a.Name), 256),
			Address: boundString(strings.ToLower(sanitizeLine(a.Address)), 320),
		})
	}
	return out
}

// normalizeMsgID trims angle brackets and whitespace and lowercases the
// domain side — the parts of a Message-ID that relays are known to vary.
func normalizeMsgID(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "<")
	if i := strings.IndexByte(v, '>'); i >= 0 {
		v = v[:i]
	}
	v = sanitizeLine(v)
	if at := strings.LastIndexByte(v, '@'); at >= 0 {
		v = v[:at+1] + strings.ToLower(v[at+1:])
	}
	return boundString(v, 998)
}

func contentType(h textproto.MIMEHeader) (string, map[string]string) {
	raw := h.Get("Content-Type")
	if raw == "" {
		return "text/plain", map[string]string{}
	}
	mt, params, err := mime.ParseMediaType(raw)
	if err != nil {
		// RFC 2045 §5.2: an unparsable type is treated as plain text —
		// but a broken multipart header must not become text, or its
		// boundary markers would leak into the body. Recover the bare
		// type when it is readable.
		bare := strings.ToLower(strings.TrimSpace(strings.SplitN(raw, ";", 2)[0]))
		if strings.HasPrefix(bare, "multipart/") {
			return bare, map[string]string{}
		}
		if bare != "" && strings.Contains(bare, "/") && !strings.ContainsAny(bare, " \t") {
			return bare, map[string]string{}
		}
		return "text/plain", map[string]string{}
	}
	return strings.ToLower(mt), params
}

func partName(params, dparams map[string]string) string {
	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	name = decodeHeader(name)
	// Keep only the final path element: a name is a label, never a path.
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" {
		name = ""
	}
	return boundString(name, 255)
}

// decodeTransfer decodes a content-transfer-encoding, reading at most
// max decoded bytes.
func decodeTransfer(cte string, body []byte, max int) ([]byte, error) {
	var r io.Reader
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "", "7bit", "8bit", "binary":
		r = bytes.NewReader(body)
	case "quoted-printable":
		r = quotedprintable.NewReader(bytes.NewReader(body))
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, &base64Cleaner{r: bytes.NewReader(body)})
	default:
		return nil, fmt.Errorf("mimenorm: unknown transfer encoding %q", cte)
	}
	out, err := io.ReadAll(io.LimitReader(r, int64(max)))
	if err != nil && len(out) == 0 {
		// Real-world base64 and QP are often slightly broken; keep what
		// decoded, fail only when nothing did.
		if len(body) == 0 {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func decodedSize(cte string, body []byte) (int64, error) {
	var r io.Reader
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "", "7bit", "8bit", "binary":
		return int64(len(body)), nil
	case "quoted-printable":
		r = quotedprintable.NewReader(bytes.NewReader(body))
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, &base64Cleaner{r: bytes.NewReader(body)})
	default:
		return -1, fmt.Errorf("mimenorm: unknown transfer encoding %q", cte)
	}
	n, err := io.Copy(io.Discard, r)
	if err != nil && n == 0 {
		return -1, err
	}
	return n, nil
}

// base64Cleaner drops everything that is not base64 alphabet, as mail
// base64 is line-wrapped and often decorated.
type base64Cleaner struct{ r io.Reader }

func (c *base64Cleaner) Read(p []byte) (int, error) {
	for {
		n, err := c.r.Read(p)
		j := 0
		for _, b := range p[:n] {
			if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '+' || b == '/' || b == '=' {
				p[j] = b
				j++
			}
		}
		if j > 0 || err != nil {
			return j, err
		}
	}
}

// toUTF8 converts from a declared charset. An unknown or absent charset
// is read as UTF-8 with invalid sequences replaced: guessing a legacy
// charset would produce confident-looking wrong text.
func toUTF8(b []byte, charset string) string {
	charset = strings.ToLower(strings.TrimSpace(charset))
	switch charset {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return strings.ToValidUTF8(string(b), "�")
	}
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return strings.ToValidUTF8(string(b), "�")
	}
	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return strings.ToValidUTF8(string(b), "�")
	}
	return strings.ToValidUTF8(string(out), "�")
}

// invisible reports format characters used to hide or reorder text:
// zero-width characters, bidirectional overrides, the BOM.
func invisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F,
		r >= 0x202A && r <= 0x202E,
		r >= 0x2060 && r <= 0x2069,
		r == 0xFEFF, r == 0x00AD:
		return true
	}
	return false
}

// sanitizeText removes control and invisible format characters,
// keeping newlines and tabs, and normalizes line endings.
func sanitizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			b.WriteByte('\n')
		case unicode.IsControl(r) || invisible(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeLine is sanitizeText for single-line values.
func sanitizeLine(s string) string {
	s = sanitizeText(s)
	s = strings.Join(strings.Fields(s), " ")
	return s
}

func boundString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return truncateUTF8(s, max)
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
