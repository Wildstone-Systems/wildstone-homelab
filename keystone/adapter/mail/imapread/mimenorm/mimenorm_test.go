package mimenorm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpus is the hostile MIME corpus shared with the reader tests.
const corpus = "../../../../../testing/fixtures/mail"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpus, name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func TestPlainMessage(t *testing.T) {
	m := Normalize(fixture(t, "plain.eml"), Limits{})
	if m.Outcome != OK {
		t.Fatalf("outcome %s (%s)", m.Outcome, m.Reason)
	}
	if m.Headers.MessageID != "plain-1@example.internal" {
		t.Errorf("message-id not normalized: %q", m.Headers.MessageID)
	}
	if len(m.Headers.From) != 1 || m.Headers.From[0].Address != "ada@example.internal" || m.Headers.From[0].Name != "Ada Example" {
		t.Errorf("from = %+v", m.Headers.From)
	}
	if m.Headers.Date.IsZero() || m.Headers.Date.Hour() != 13 {
		t.Errorf("date not parsed to UTC: %v", m.Headers.Date)
	}
	if m.TextSource != "plain" || !strings.Contains(m.Text, "October 15, 2026 from 6:00 to 7:30 p.m.") {
		t.Errorf("text = %q (%s)", m.Text, m.TextSource)
	}
	if strings.Contains(m.Text, "\r") {
		t.Error("line endings not normalized")
	}
}

func TestHTMLTrackingIsStrippedAndNothingRemoteSurvives(t *testing.T) {
	m := Normalize(fixture(t, "html-tracking.eml"), Limits{})
	if m.Outcome != OK {
		t.Fatalf("outcome %s (%s)", m.Outcome, m.Reason)
	}
	if m.TextSource != "html" {
		t.Fatalf("text source %q", m.TextSource)
	}
	if m.Headers.Subject != "Your weekly digest ✨" {
		t.Errorf("subject = %q", m.Headers.Subject)
	}
	for _, want := range []string{"Hello & welcome to the weekly digest.", "- First item", "- Second item",
		"the article <https://example.internal/article?id=7>", "click me"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
	for _, banned := range []string{"track.example.internal", "cdn.example.internal", "utm_", "fbclid",
		"javascript:", "SYSTEM: ignore", "hidden tiny text", "evil.example.internal", "ads.example.internal",
		"color:red", "Digest\n"} {
		if strings.Contains(m.Text, banned) {
			t.Errorf("text leaks %q:\n%s", banned, m.Text)
		}
	}
	if m.TrackingPixels != 1 {
		t.Errorf("tracking pixels = %d, want 1", m.TrackingPixels)
	}
	// Two images and one iframe; <head> (with its stylesheet) is
	// dropped wholesale.
	if m.RemoteContentBlocked != 3 {
		t.Errorf("remote content blocked = %d, want 3", m.RemoteContentBlocked)
	}
	if m.HiddenContentRemoved != 2 {
		t.Errorf("hidden content removed = %d, want 2", m.HiddenContentRemoved)
	}
}

func TestCalendarInviteIsExtractedRawAndAttachmentsReported(t *testing.T) {
	m := Normalize(fixture(t, "ics-invite.eml"), Limits{})
	if m.Outcome != OK {
		t.Fatalf("outcome %s (%s)", m.Outcome, m.Reason)
	}
	if len(m.Calendars) != 2 {
		t.Fatalf("calendars = %d, want inline part and .ics attachment", len(m.Calendars))
	}
	for _, c := range m.Calendars {
		if !strings.Contains(c.Data, "DTSTART;TZID=America/New_York:20261015T180000") ||
			!strings.Contains(c.Data, "SUMMARY:School family night") || c.Oversized {
			t.Errorf("calendar part not raw: %+v", c)
		}
	}
	if m.Calendars[0].Method != "REQUEST" || m.Calendars[1].Name != "invite.ics" {
		t.Errorf("calendar metadata: %+v / %+v", m.Calendars[0], m.Calendars[1])
	}
	if m.TextSource != "plain" || !strings.Contains(m.Text, "School family night") || strings.Contains(m.Text, "<b>") {
		t.Errorf("text = %q (%s)", m.Text, m.TextSource)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	a := m.Attachments[0]
	if a.Name != "flyer.pdf" || a.ContentType != "application/pdf" || a.Reason != "unsupported" || a.Size != 616 {
		t.Errorf("pdf not reported faithfully (and name must be a bare label): %+v", a)
	}
}

func TestForwardedInviteInsideMessageRFC822(t *testing.T) {
	m := Normalize(fixture(t, "forwarded-invite.eml"), Limits{})
	if m.Outcome != OK {
		t.Fatalf("outcome %s (%s)", m.Outcome, m.Reason)
	}
	if len(m.Calendars) != 1 || !strings.Contains(m.Calendars[0].Data, "UID:fixture-invite-1@example.internal") {
		t.Fatalf("nested calendar not found: %+v", m.Calendars)
	}
	if m.Headers.MessageID != "fwd-1@example.internal" {
		t.Errorf("outer headers must win: %q", m.Headers.MessageID)
	}
}

func TestDeepNestingIsTooDeep(t *testing.T) {
	m := Normalize(fixture(t, "nested-deep.eml"), Limits{})
	if m.Outcome != TooDeep {
		t.Fatalf("outcome %s, want too_deep", m.Outcome)
	}
	if strings.Contains(m.Text, "bottom of the well") {
		t.Error("text below the depth limit leaked")
	}
	// A generous limit reads it fine: the cap, not the parser, decided.
	if m := Normalize(fixture(t, "nested-deep.eml"), Limits{MaxDepth: 64}); m.Outcome != OK || !strings.Contains(m.Text, "bottom of the well") {
		t.Errorf("with a larger depth: %s %q", m.Outcome, m.Text)
	}
}

func TestTooManyPartsIsOversized(t *testing.T) {
	m := Normalize(fixture(t, "many-parts.eml"), Limits{})
	if m.Outcome != Oversized || m.Reason != "parts" {
		t.Fatalf("outcome %s/%s, want oversized/parts", m.Outcome, m.Reason)
	}
	if m.Parts > DefaultLimits().MaxParts {
		t.Errorf("visited %d parts past the cap", m.Parts)
	}
	if !strings.Contains(m.Text, "lots of parts follow") {
		t.Error("evidence gathered before the cap was dropped")
	}
}

func TestMalformedCorpus(t *testing.T) {
	for _, tc := range []struct {
		file, reason, text string
	}{
		{"malformed-no-boundary.eml", "multipart_boundary", ""},
		{"malformed-encoding.eml", "transfer_encoding", "the readable part survives"},
		{"malformed-headers.eml", "headers", ""},
	} {
		m := Normalize(fixture(t, tc.file), Limits{})
		if m.Outcome != Malformed || m.Reason != tc.reason {
			t.Errorf("%s: %s/%s, want malformed/%s", tc.file, m.Outcome, m.Reason, tc.reason)
		}
		if tc.text != "" && !strings.Contains(m.Text, tc.text) {
			t.Errorf("%s: lost %q", tc.file, tc.text)
		}
		if strings.Contains(m.Text, "--whatever") {
			t.Errorf("%s: boundary markers leaked into text", tc.file)
		}
	}
	m := Normalize(fixture(t, "malformed-encoding.eml"), Limits{})
	if len(m.Attachments) != 1 || m.Attachments[0].Reason != "unknown_encoding" {
		t.Errorf("unreadable part not reported: %+v", m.Attachments)
	}
}

func TestOversizedMessageKeepsOnlyHeaders(t *testing.T) {
	raw := fixture(t, "plain.eml")
	m := Normalize(raw, Limits{MaxMessageBytes: int64(len(raw) - 1)})
	if m.Outcome != Oversized || m.Reason != "message" {
		t.Fatalf("outcome %s/%s", m.Outcome, m.Reason)
	}
	if m.Text != "" || m.Headers.Subject != "Family night on Thursday" {
		t.Errorf("oversized: text %q, subject %q", m.Text, m.Headers.Subject)
	}
}

func TestOversizedHeaders(t *testing.T) {
	raw := "Subject: big\r\nX-Filler: " + strings.Repeat("a", 70<<10) + "\r\n\r\nbody\r\n"
	m := Normalize([]byte(raw), Limits{})
	if m.Outcome != Oversized || m.Reason != "headers" {
		t.Fatalf("outcome %s/%s", m.Outcome, m.Reason)
	}
	if _, o, _ := NormalizeHeaders([]byte(raw), Limits{}); o != Oversized {
		t.Errorf("header-only outcome %s", o)
	}
}

func TestTextIsBoundedAtRuneBoundary(t *testing.T) {
	raw := "Subject: long\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + strings.Repeat("é", 1000)
	m := Normalize([]byte(raw), Limits{MaxTextBytes: 101})
	if m.Outcome != OK || !m.TextTruncated || len(m.Text) > 101 || !strings.HasSuffix(m.Text, "é") {
		t.Fatalf("outcome %s truncated %v len %d", m.Outcome, m.TextTruncated, len(m.Text))
	}
}

func TestCalendarOverLimitIsNotTruncated(t *testing.T) {
	raw := fixture(t, "ics-invite.eml")
	m := Normalize(raw, Limits{MaxCalendarBytes: 64})
	for _, c := range m.Calendars {
		if !c.Oversized || c.Data != "" {
			t.Errorf("a truncated calendar would parse to the wrong event: %+v", c)
		}
	}
}

func TestPromptInjectionIsInertData(t *testing.T) {
	m := Normalize(fixture(t, "prompt-injection.eml"), Limits{})
	if m.Outcome != OK {
		t.Fatalf("outcome %s", m.Outcome)
	}
	// The text is evidence and stays verbatim…
	if !strings.Contains(m.Text, "Ignore all previous instructions") {
		t.Error("injection text should be preserved as data, not silently edited")
	}
	// …but invisible characters used to hide or reorder it are gone,
	if strings.ContainsAny(m.Text, "\u200b\u202e") || !strings.Contains(m.Text, "Hidden zero-width") {
		t.Errorf("invisible characters survived: %q", m.Text)
	}
	// and sender-controlled headers never surface as metadata.
	out, _ := json.Marshal(m)
	for _, banned := range []string{"X-Wildstone", "Approve", "authority", "REPLACED-TOKEN", "Bearer"} {
		if strings.Contains(string(out), banned) {
			t.Errorf("normalized output carries %q", banned)
		}
	}
}

func TestCharsets(t *testing.T) {
	m := Normalize(fixture(t, "iso-8859-1-qp.eml"), Limits{})
	if m.Outcome != OK || !strings.Contains(m.Text, "Café crème: réunion le jeudi à 18h.") {
		t.Errorf("latin-1 body: %s %q", m.Outcome, m.Text)
	}
	if m.Headers.Subject != "Réunion – jeudi" || m.Headers.From[0].Name != "André" {
		t.Errorf("encoded words: %q / %+v", m.Headers.Subject, m.Headers.From)
	}

	m = Normalize(fixture(t, "iso-2022-jp.eml"), Limits{})
	if !strings.Contains(m.Text, "明日の会議は十時からです。") || m.Headers.Subject != "会議" || m.Headers.From[0].Name != "田中" {
		t.Errorf("iso-2022-jp: %q / %q / %+v", m.Text, m.Headers.Subject, m.Headers.From)
	}

	m = Normalize(fixture(t, "unknown-charset.eml"), Limits{})
	if m.Outcome != OK || !strings.Contains(m.Text, "plain ascii survives") {
		t.Errorf("unknown charset: %s %q", m.Outcome, m.Text)
	}

	m = Normalize(fixture(t, "invalid-utf8.eml"), Limits{})
	if m.Outcome != OK || !strings.Contains(m.Text, "valid then \uFFFD") {
		t.Errorf("invalid utf-8: %s %q", m.Outcome, m.Text)
	}
}

func TestEveryFixtureIsBoundedAndValidUTF8(t *testing.T) {
	entries, err := os.ReadDir(corpus)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".eml" {
			continue
		}
		n++
		m := Normalize(fixture(t, e.Name()), Limits{})
		switch m.Outcome {
		case OK, Malformed, Oversized, TooDeep:
		default:
			t.Errorf("%s: outcome %q outside the closed set", e.Name(), m.Outcome)
		}
		if _, err := json.Marshal(m); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
		if len(m.Text) > DefaultLimits().MaxTextBytes {
			t.Errorf("%s: text over the cap", e.Name())
		}
		if !json.Valid(mustJSON(t, m)) || strings.ContainsRune(m.Text, '\x00') {
			t.Errorf("%s: control bytes in output", e.Name())
		}
	}
	if n < 12 {
		t.Fatalf("corpus shrank to %d fixtures", n)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHTMLPathologicalNesting(t *testing.T) {
	src := strings.Repeat("<div>", 5000) + "deep text" + strings.Repeat("</div>", 5000) +
		`<div style="display:none">` + strings.Repeat("<p>", 2000) + "secret</div>visible"
	m := &Message{}
	out := htmlToText(src, m)
	if !strings.Contains(out, "deep text") || strings.Contains(out, "secret") {
		t.Errorf("pathological html: %q", out)
	}
}

func FuzzNormalize(f *testing.F) {
	entries, _ := os.ReadDir(corpus)
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(corpus, e.Name())); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		m := Normalize(raw, Limits{MaxTextBytes: 4096, MaxParts: 20, MaxDepth: 4})
		if len(m.Text) > 4096 || m.Parts > 20 {
			t.Fatalf("limits exceeded: text %d parts %d", len(m.Text), m.Parts)
		}
	})
}
