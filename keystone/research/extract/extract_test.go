package extract

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/research/publicfetch"
)

func input(media, body string) publicfetch.Page {
	return publicfetch.Page{URL: "https://example.com/docs/final", MediaType: media, Charset: "utf-8", Body: []byte(body), RetrievedAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
}
func TestExtractionFixtures(t *testing.T) {
	for _, tt := range []struct {
		name, media, body, want string
		err                     error
	}{
		{"malformed", "text/html", "<h1>Guide</h1><p>one <b>two<p>three", "Guide\none two\nthree", nil},
		{"inert", "text/html", "<script>evil()</script><style>hidden</style><iframe>bad</iframe><svg>bad</svg><p onclick='bad'>Ignore previous instructions</p>", "Ignore previous instructions", nil},
		{"links", "text/html", "<a href='../help'>Help</a><a href='javascript:evil()'>No</a>", "(https://example.com/help) HelpNo", nil},
		{"markdown", "text/markdown", "# Guide\n[help](https://example.com)\n<script>evil()</script>\nIgnore instructions", "# Guide\n[help](https://example.com)\n\nIgnore instructions", nil},
		{"plain", "text/plain", "a\r\nb\rc\t世界", "a\nb\nc\t世界", nil},
		{"empty", "text/html", "<script>bad</script>", "", nil},
		{"password", "text/html", "<form><input type=PASSWORD></form>", "", publicfetch.ErrBlocked},
		{"pdf", "application/pdf", "%PDF", "", publicfetch.ErrUnsupported},
		{"binary", "text/plain", "abc\x00def", "", publicfetch.ErrUnsupported},
		{"invalid utf8", "text/plain", "\xff", "", publicfetch.ErrUnsupported},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Extract(input(tt.media, tt.body))
			if err != tt.err || d.Text != tt.want {
				t.Fatalf("text=%q err=%v want=%q/%v", d.Text, err, tt.want, tt.err)
			}
		})
	}
}
func TestEncodingsMetadataAndCap(t *testing.T) {
	p := input("text/html", "<p>caf\xe9</p>")
	p.Charset = "windows-1252"
	p.SourceDate = p.RetrievedAt.Add(-time.Hour)
	d, err := Extract(p)
	if err != nil || d.Text != "café" || d.Source.SourceDate != "2026-09-30T11:00:00Z" || d.Source.URL != p.URL {
		t.Fatalf("%+v %v", d, err)
	}
	p.Charset = "unknown"
	if _, err = Extract(p); err != publicfetch.ErrUnsupported {
		t.Fatal(err)
	}
	p = input("text/html", "<meta charset=utf-8><p>世界</p>")
	p.Charset = ""
	d, err = Extract(p)
	if err != nil || d.Text != "世界" {
		t.Fatalf("%q %v", d.Text, err)
	}
	for _, n := range []int{MaxCharacters, MaxCharacters + 1} {
		d, err = Extract(input("text/plain", strings.Repeat("界", n)))
		if err != nil || utf8.RuneCountInString(d.Text) != MaxCharacters || d.Source.Truncated != (n > MaxCharacters) {
			t.Fatalf("cap %d: %+v %v", n, d.Source, err)
		}
	}
	p = input("text/plain", strings.Repeat("x", publicfetch.MaxBytes+1))
	if _, err = Extract(p); err != publicfetch.ErrSize {
		t.Fatal(err)
	}
}
func scope() Scope { return Scope{"person", "agent", "task", "private", "policy-1"} }
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(func(context.Context, Scope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestPagesUnicodeEscapingAndImmutableSources(t *testing.T) {
	for _, body := range []string{strings.Repeat("abc世界😀", 6000), strings.Repeat("\"\\<>&\t", 6000), ""} {
		s := newStore(t)
		p, err := s.Put(context.Background(), scope(), input("text/plain", body))
		if err != nil {
			t.Fatal(err)
		}
		expected, _ := Extract(input("text/plain", body))
		ref := p.Source.SourceRef
		var combined strings.Builder
		for {
			encoded, _ := p.Encode()
			if len(p.Text) > PageBytes || len(encoded) > BodyBytes || len(redact.Envelope(string(encoded))) > 4000 || !utf8.ValidString(p.Text) || !json.Valid(encoded) {
				t.Fatalf("invalid page %d/%d", len(p.Text), len(encoded))
			}
			combined.WriteString(p.Text)
			if p.NextCursor == "" {
				break
			}
			old := p
			p, err = s.Continue(context.Background(), scope(), p.NextCursor)
			if err != nil {
				t.Fatal(err)
			}
			repeat, e := s.Read(context.Background(), scope(), ref, old.NextCursor)
			if e != nil || repeat != p {
				t.Fatal("non deterministic page")
			}
		}
		if combined.String() != expected.Text || p.Truncated != expected.Source.Truncated {
			t.Fatal("lost data/truncation")
		}
		_, err = s.Put(context.Background(), scope(), input("text/plain", "changed"))
		if err != nil {
			t.Fatal(err)
		}
		first, err := s.Read(context.Background(), scope(), ref, "")
		if err != nil || first.Source.SourceRef != ref || !strings.HasPrefix(expected.Text, first.Text) {
			t.Fatal("source mutated")
		}
	}
}
func TestCursorIsolationRevocationAndExpiry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	p, err := s.Put(ctx, scope(), input("text/plain", strings.Repeat("x", 6000)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		other := scope()
		switch i {
		case 0:
			other.Person = "other"
		case 1:
			other.Agent = "other"
		case 2:
			other.Task = "other"
		case 3:
			other.Resource = "shared"
		case 4:
			other.PolicyRevision = "policy-2"
		}
		if _, err = s.Continue(ctx, other, p.NextCursor); err != ErrStale {
			t.Fatalf("scope %d: %v", i, err)
		}
	}
	for _, cursor := range []string{"bad", p.NextCursor[:20] + "!" + p.NextCursor[21:], strings.Repeat("x", 513)} {
		if _, err = s.Read(ctx, scope(), p.Source.SourceRef, cursor); err != ErrInput {
			t.Fatal(err)
		}
	}
	q, _ := s.Put(ctx, scope(), input("text/plain", strings.Repeat("y", 6000)))
	if _, err = s.Read(ctx, scope(), q.Source.SourceRef, p.NextCursor); err != ErrInput {
		t.Fatal("cross source", err)
	}
	ref := p.Source.SourceRef
	ref.Revision = strings.Repeat("0", 64)
	if _, err = s.Read(ctx, scope(), ref, p.NextCursor); err != ErrInput {
		t.Fatal(err)
	}
	s.authorize = func(context.Context, Scope) error { return ErrDenied }
	if _, err = s.Continue(ctx, scope(), p.NextCursor); err != ErrDenied {
		t.Fatal(err)
	}
	s.authorize = func(context.Context, Scope) error { return nil }
	now = now.Add(cacheTTL)
	if _, err = s.Continue(ctx, scope(), p.NextCursor); err != ErrStale {
		t.Fatal(err)
	}
	fresh := newStore(t)
	if _, err = fresh.Continue(ctx, scope(), p.NextCursor); err != ErrStale {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.Put(cancelled, scope(), input("text/plain", "x")); err != context.Canceled {
		t.Fatal(err)
	}
}
func TestCapacityAndConcurrentReaders(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	p, _ := s.Put(ctx, scope(), input("text/plain", strings.Repeat("x", 6000)))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Continue(ctx, scope(), p.NextCursor); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := 1; i < maxSources; i++ {
		if _, err := s.Put(ctx, scope(), input("text/plain", "x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Put(ctx, scope(), input("text/plain", "x")); err != ErrCapacity {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(cacheTTL) }
	if _, err := s.Put(ctx, scope(), input("text/plain", "x")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(nil); err != ErrDenied {
		t.Fatal(err)
	}
}

func TestValidEncodingTamperingAndMetadataBounds(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	p, err := s.Put(ctx, scope(), input("text/plain", strings.Repeat("x", 6000)))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := base64.RawURLEncoding.DecodeString(p.NextCursor)
	for _, index := range []int{0, 18, 30, 51} {
		tampered := append([]byte(nil), b...)
		tampered[index] ^= 1
		if _, err = s.Read(ctx, scope(), p.Source.SourceRef, base64.RawURLEncoding.EncodeToString(tampered)); err != ErrInput {
			t.Fatalf("tamper %d: %v", index, err)
		}
	}
	oversized := input("text/plain", "x")
	oversized.URL = "https://example.com/?" + strings.Repeat("&", 1800)
	if _, err = s.Put(ctx, scope(), oversized); err != publicfetch.ErrSize {
		t.Fatal("unbounded escaped metadata", err)
	}
	if _, err = Extract(input("text/plain", "%PDF-1.7")); err != publicfetch.ErrUnsupported {
		t.Fatal(err)
	}
	if _, err = s.Read(ctx, Scope{}, p.Source.SourceRef, ""); err != ErrInput {
		t.Fatal(err)
	}
}

func TestMarkdownSyntaxAndEmbeddedHTML(t *testing.T) {
	body := "Autolink: <https://example.com/path>\n\n```html\n<input type=password>\n<script>example()</script>\n```\n\nInline `<input type=password>` and \\<div>\n\n    <input type=password>\n\n"
	d, err := Extract(input("text/markdown", body))
	if err != nil || d.Text != strings.TrimSpace(body) {
		t.Fatalf("Markdown corrupted: %q %v", d.Text, err)
	}
	for _, body := range []string{"before <script>evil()</script> after", "before <style>evil()</style> after"} {
		d, err = Extract(input("text/markdown", body))
		if err != nil || d.Text != "before  after" {
			t.Fatalf("inline active content: %q %v", d.Text, err)
		}
	}
	if _, err = Extract(input("text/markdown", "before <input type=password> after")); err != publicfetch.ErrBlocked {
		t.Fatal(err)
	}
}

func TestLinkExpansionIsBoundedBeforeFinalTruncation(t *testing.T) {
	p := input("text/html", strings.Repeat("<a href=\"\">", publicfetch.MaxBytes/11))
	p.URL = "https://example.com/" + strings.Repeat("x", 2000)
	d, err := Extract(p)
	if err != nil || !d.Source.Truncated || utf8.RuneCountInString(d.Text) > MaxCharacters {
		t.Fatalf("bounded expansion: %+v %v", d.Source, err)
	}
	// Exercise the intermediate builder itself: the final cap alone would not
	// catch an amplification allocation before Extract returned.
	base, _ := url.Parse(p.URL)
	out := limitedText{normalize: true}
	f := markupFilter{out: &out, base: base}
	if err = f.feed(string(p.Body), false); err != nil || out.Len() > 4*(MaxCharacters+1) {
		t.Fatalf("intermediate output %d: %v", out.Len(), err)
	}
	if err = f.feed("<input type=password>", false); err != publicfetch.ErrBlocked {
		t.Fatal("challenge after cap", err)
	}
	for _, n := range []int{MaxCharacters, MaxCharacters + 1} {
		d, err = Extract(input("text/html", "<p>"+strings.Repeat("界", n)+"</p>"))
		if err != nil || utf8.RuneCountInString(d.Text) != MaxCharacters || d.Source.Truncated != (n > MaxCharacters) {
			t.Fatalf("HTML cap %d: %+v %v", n, d.Source, err)
		}
	}
}

func TestMarkdownHTMLSegmentsInContainers(t *testing.T) {
	for _, body := range []string{
		"> before <input\n> type=password> after",
		"- before <input\n  type=password> after",
		"> <form>\n> <input\n> type=password>\n> </form>",
	} {
		if _, err := Extract(input("text/markdown", body)); err != publicfetch.ErrBlocked {
			t.Fatalf("missed logical challenge %q: %v", body, err)
		}
	}
	body := "> before <a\n> href=\"/help\">Help</a> after"
	d, err := Extract(input("text/markdown", body))
	if err != nil || !strings.Contains(d.Text, "https://example.com/help") || !strings.Contains(d.Text, "Help") || strings.Count(d.Text, ">") != 2 {
		t.Fatalf("lost link/container: %q %v", d.Text, err)
	}
	d, err = Extract(input("text/markdown", "> <script>\n> evil()\n> </script>\n\nafter"))
	if err != nil || strings.Contains(d.Text, "evil") || !strings.HasSuffix(d.Text, "after") {
		t.Fatalf("active block: %q %v", d.Text, err)
	}
}

func TestDecodedControlsAreRefusedEvenPastTextCap(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, body := range []string{"<p>before &#x1b; after</p>", "<a href=\"/&#x1b;\">link</a>", "<p>" + strings.Repeat("x", MaxCharacters+1) + "&#27;</p>"} {
			if _, err := Extract(input(media, body)); err != publicfetch.ErrUnsupported {
				t.Fatalf("decoded control accepted %s: %v", media, err)
			}
		}
	}
}

func TestOptionalHTMLHeadAndBodyTags(t *testing.T) {
	for _, body := range []string{
		"<head><title>T</title><body><p>Visible</p>",
		"<head><title>T</title><p>Visible</p>",
		"<head><title>T</title><meta charset=utf-8><style>hidden</style><script>hidden()</script></head><body><p>Visible</p>",
		"<title>T</title><p>Visible</p>",
	} {
		d, err := Extract(input("text/html", body))
		if err != nil || d.Text != "Visible" {
			t.Fatalf("implicit head/body %q: %q %v", body, d.Text, err)
		}
	}
}

func TestNonVoidSelfClosingSyntaxStillSuppressesContent(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, tag := range []string{"script", "style", "title", "iframe", "object", "template", "noscript"} {
			body := "<" + tag + "/>hidden</" + tag + ">\n\nVisible"
			d, err := Extract(input(media, body))
			if err != nil || d.Text != "Visible" {
				t.Fatalf("%s %s leaked: %q %v", media, tag, d.Text, err)
			}
		}
		for _, tag := range []string{"svg", "math"} {
			d, err := Extract(input(media, "<"+tag+"/>\n\nVisible"))
			if err != nil || d.Text != "Visible" {
				t.Fatalf("%s %s swallowed following text: %q %v", media, tag, d.Text, err)
			}
		}
		d, err := Extract(input(media, "<template><template/>hidden</template>hidden</template>\n\nVisible"))
		if err != nil || d.Text != "Visible" {
			t.Fatalf("nested non-void slash: %q %v", d.Text, err)
		}
	}
}

func TestRawTextTagsDoNotNestAcrossMarkdownSpans(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, tag := range []string{"script", "style", "iframe", "noscript", "title"} {
			body := "before <" + tag + "><" + tag + ">hidden</" + tag + "> after"
			d, err := Extract(input(media, body))
			if err != nil || strings.Join(strings.Fields(d.Text), " ") != "before after" {
				t.Fatalf("%s %s lost raw-text suffix: %q %v", media, tag, d.Text, err)
			}
		}
	}
}

func TestMarkdownRetainsContinuousTextareaAndRawTextState(t *testing.T) {
	for _, tag := range []string{"textarea", "xmp", "noembed", "noframes"} {
		body := "before <" + tag + "><input type=password></" + tag + "> after"
		for _, media := range []string{"text/html", "text/markdown"} {
			d, err := Extract(input(media, body))
			if err != nil || strings.Join(strings.Fields(d.Text), " ") != "before <input type=password> after" {
				t.Fatalf("%s %s lost inert data: %q %v", media, tag, d.Text, err)
			}
		}
	}
	d, err := Extract(input("text/markdown", "before <textarea>&lt;input type=password&gt;</textarea> after"))
	if err != nil || !strings.Contains(d.Text, "after") {
		t.Fatalf("RCDATA references: %q %v", d.Text, err)
	}
	// Once the textarea ends, real controls must still be rejected.
	if _, err = Extract(input("text/markdown", "<textarea>example</textarea><input type=password>")); err != publicfetch.ErrBlocked {
		t.Fatal("real challenge after textarea", err)
	}
}

func TestMarkdownRawTextPreservesCharactersWithoutEntityExpansion(t *testing.T) {
	for _, tag := range []string{"xmp", "noembed", "noframes"} {
		body := "before <" + tag + ">A & B \"quoted\" 'single' <https://example.com> &amp;</" + tag + "> after"
		d, err := Extract(input("text/markdown", body))
		if err != nil || d.Text != "before A & B \"quoted\" 'single' <https://example.com> &amp; after" {
			t.Fatalf("%s escaped literal: %q %v", tag, d.Text, err)
		}
	}
	d, err := Extract(input("text/markdown", "before <plaintext>A & B \"quoted\" <https://example.com>"))
	if err != nil || d.Text != "before A & B \"quoted\" <https://example.com>" {
		t.Fatalf("plaintext escaped literal: %q %v", d.Text, err)
	}
}

func TestMarkdownLiteralMarkersCannotBeForgedOrExpandedRecursively(t *testing.T) {
	body := "<div>\nWS_MD_LITERAL_0_0_ WS_MD_LITERAL_&#49;_0_\n</div>\n\nWS_MD_LITERAL_2_0_ <xmp>A & B</xmp>"
	d, err := Extract(input("text/markdown", body))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WS_MD_LITERAL_0_0_", "WS_MD_LITERAL_1_0_", "WS_MD_LITERAL_2_0_", "A & B"} {
		if !strings.Contains(d.Text, want) {
			t.Fatalf("marker collision/lost data %q: %q", want, d.Text)
		}
	}
	again, err := Extract(input("text/markdown", body))
	if err != nil || again != d {
		t.Fatal("non-deterministic extraction")
	}
}

func TestForeignContentDoesNotConsumeFollowingHTML(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, foreign := range []string{"svg", "math"} {
			for _, child := range []string{"script", "style", "title", "textarea", "xmp", "plaintext"} {
				for _, wrapper := range []string{"", "object"} {
					body := "<" + foreign + "><" + child + "></" + foreign + ">"
					if wrapper != "" {
						body = "<" + wrapper + ">" + body + "</" + wrapper + ">"
					}
					d, err := Extract(input(media, body+"\n\nVisible"))
					if err != nil || d.Text != "Visible" {
						t.Fatalf("%s %s/%s/%s suffix: %q %v", media, wrapper, foreign, child, d.Text, err)
					}
					if _, err := Extract(input(media, body+"<input type=password>")); err != publicfetch.ErrBlocked {
						t.Fatalf("%s %s/%s/%s challenge: %v", media, wrapper, foreign, child, err)
					}
				}
			}
		}
	}
}

func TestForeignContentImplicitExit(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, foreign := range []string{"svg", "math"} {
			body := "<" + foreign + "><p>Visible"
			d, err := Extract(input(media, body))
			if err != nil || d.Text != "Visible" {
				t.Fatalf("%s %s implicit exit: %q %v", media, foreign, d.Text, err)
			}
			if _, err := Extract(input(media, body+"<input type=password>")); err != publicfetch.ErrBlocked {
				t.Fatalf("%s %s implicit challenge: %v", media, foreign, err)
			}
		}
	}
}

func TestDiscardedTreeTokensStillRejectDecodedControls(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		if _, err := Extract(input(media, "<head></head><head ignored='&#x1b;'>Visible")); err != publicfetch.ErrUnsupported {
			t.Fatalf("%s discarded token control: %v", media, err)
		}
	}
}

func TestHTMLPreservesInlineWordAndWhitespaceBoundaries(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"docu<em>ment</em>ation", "documentation"},
		{"one <em>two</em> three", "one two three"},
		{"one<em> two </em>three", "one two three"},
		{"one\t<span>\n two</span>  three", "one\ntwo three"},
		{"one<p>two</p>three", "one\ntwo\nthree"},
	} {
		d, err := Extract(input("text/html", tc.body))
		if err != nil || d.Text != tc.want {
			t.Fatalf("%q: got %q, want %q: %v", tc.body, d.Text, tc.want, err)
		}
	}
}

func TestHTMLIntegrationPointChallengesAreActive(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, body := range []string{
			"<svg><foreignObject><input type=password></foreignObject></svg>",
			"<svg><desc><input type=password></desc></svg>",
			"<math><mtext><input type=password></mtext></math>",
			"<math><annotation-xml encoding='text/html'><input type=password></annotation-xml></math>",
			"<object><input type=password></object>",
		} {
			if _, err := Extract(input(media, body)); err != publicfetch.ErrBlocked {
				t.Fatalf("%s integration point %q: %v", media, body, err)
			}
			if _, err := Extract(input(media, "<template>"+body+"</template>")); err != nil {
				t.Fatalf("%s inert template %q: %v", media, body, err)
			}
		}
		if _, err := Extract(input(media, "<svg><input type=password /></svg>")); err != nil {
			t.Fatalf("%s foreign-namespace input: %v", media, err)
		}
	}
}

// Exercise the representation boundary and active/inert ancestry together,
// rather than treating each parser-repair regression as an isolated example.
func TestChallengeDetectionAcrossRepresentationsAndOutputCap(t *testing.T) {
	cases := []struct {
		name, body string
		blocked    bool
	}{
		{"direct", "<input type=password>", true},
		{"case-and-entity", "<INPUT TYPE='pass&#119;ord'>", true},
		{"optional-head", "<head><title>Login</title><body><input type=password>", true},
		{"foreign-breakout", "<svg><p><input type=password>", true},
		{"foreign-integration", "<math><mtext><input type=password></mtext></math>", true},
		{"object-fallback", "<object><input type=password></object>", true},
		{"inert-template", "<template><svg><foreignObject><input type=password></foreignObject></svg></template>", false},
		{"raw-text", "<script><input type=password></script>", false},
		{"rcdata", "<textarea><input type=password></textarea>", false},
		{"encoded-example", "&lt;input type=password&gt;", false},
		{"comment", "<!-- <input type=password> -->", false},
	}
	for _, tc := range cases {
		for _, media := range []string{"text/html", "text/markdown", "text/plain"} {
			for _, prefix := range []string{"", strings.Repeat("x", MaxCharacters+1) + "\n\n"} {
				d, err := Extract(input(media, prefix+tc.body))
				wantBlocked := tc.blocked && media != "text/plain"
				if wantBlocked {
					if err != publicfetch.ErrBlocked {
						t.Fatalf("%s %s prefix=%d: want blocked, got %v", media, tc.name, len(prefix), err)
					}
				} else if err != nil || utf8.RuneCountInString(d.Text) > MaxCharacters || (prefix != "" && !d.Source.Truncated) {
					t.Fatalf("%s %s prefix=%d: unexpected result %+v %v", media, tc.name, len(prefix), d.Source, err)
				}
			}
		}
	}
}

func TestEntityDecodedLineEndingsNormalizeBeforeBounds(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, body := range []string{
			"<div>A&#13;B</div>",
			"<div>A&#13;&#10;B</div>",
			"<div>A&#13;<span>&#10;B</span></div>",
			"<div>A&#10;B</div>",
		} {
			d, err := Extract(input(media, body))
			if err != nil || d.Text != "A\nB" {
				t.Fatalf("%s %q: %q %v", media, body, d.Text, err)
			}
		}
		body := "<div>" + strings.Repeat("x", MaxCharacters-2) + "&#13;&#10;y</div>"
		d, err := Extract(input(media, body))
		if err != nil || utf8.RuneCountInString(d.Text) != MaxCharacters || d.Source.Truncated || !strings.HasSuffix(d.Text, "\ny") {
			t.Fatalf("%s normalized cap: %d %+v %v", media, utf8.RuneCountInString(d.Text), d.Source, err)
		}
	}
}

func TestCancellationDuringAuthorizationRemainsDistinct(t *testing.T) {
	for _, operation := range []string{"put", "read", "continue"} {
		checks := 1
		if operation != "read" {
			checks = 2
		}
		for target := 1; target <= checks; target++ {
			for _, deadline := range []bool{false, true} {
				for _, returnContextError := range []bool{false, true} {
					s := newStore(t)
					p, err := s.Put(context.Background(), scope(), input("text/plain", strings.Repeat("x", 6000)))
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(context.Background())
					want := context.Canceled
					if deadline {
						cancel()
						ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
						want = context.DeadlineExceeded
					}
					calls := 0
					s.authorize = func(ctx context.Context, _ Scope) error {
						calls++
						if calls == target {
							if !deadline {
								cancel()
							}
							<-ctx.Done()
							if returnContextError {
								return ctx.Err()
							}
						}
						return nil
					}
					switch operation {
					case "put":
						_, err = s.Put(ctx, scope(), input("text/plain", "new"))
					case "read":
						_, err = s.Read(ctx, scope(), p.Source.SourceRef, "")
					case "continue":
						_, err = s.Continue(ctx, scope(), p.NextCursor)
					}
					cancel()
					if err != want || calls != target || len(s.sources) != 1 {
						t.Fatalf("%s check=%d deadline=%v callbackErr=%v: err=%v calls=%d sources=%d", operation, target, deadline, returnContextError, err, calls, len(s.sources))
					}
				}
			}
		}
	}
}

func TestMarkdownLiteralRestorationHonorsRCDATA(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		d, err := Extract(input(media, "before <textarea>A &amp; B &lt;input type=password&gt;</textarea> after"))
		if err != nil || d.Text != "before A & B <input type=password> after" {
			t.Fatalf("%s RCDATA: %q %v", media, d.Text, err)
		}
		for _, tag := range []string{"textarea", "title"} {
			for _, prefix := range []string{"", strings.Repeat("x", MaxCharacters+1) + "\n\n"} {
				body := prefix + "before <" + tag + ">A &amp; B &#x1b;</" + tag + "> after"
				if _, err := Extract(input(media, body)); err != publicfetch.ErrUnsupported {
					t.Fatalf("%s %s prefix=%d decoded control: %v", media, tag, len(prefix), err)
				}
			}
		}
	}
	// Literal references inside ordinary Markdown and raw-text are data,
	// while RCDATA references decode exactly once without becoming markup.
	for _, body := range []string{"A &amp; B &#x1b;", "before <xmp>A &amp; B &#x1b;</xmp> after"} {
		d, err := Extract(input("text/markdown", body))
		if err != nil || !strings.Contains(d.Text, "A &amp; B &#x1b;") {
			t.Fatalf("literal/raw-text changed: %q %v", d.Text, err)
		}
	}
	d, err := Extract(input("text/markdown", "before <textarea>&amp;#x1b;</textarea> after"))
	if err != nil || d.Text != "before &#x1b; after" {
		t.Fatalf("double-decoded RCDATA: %q %v", d.Text, err)
	}
}

func TestSemicolonlessRCDATAReferences(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, reference := range []string{"&#1", "&#x1b", "&#27"} {
			for _, prefix := range []string{"", strings.Repeat("x", MaxCharacters+1)} {
				body := prefix + "<textarea>" + reference + "</textarea>"
				if _, err := Extract(input(media, body)); err != publicfetch.ErrUnsupported {
					t.Fatalf("%s %s prefix=%d: %v", media, reference, len(prefix), err)
				}
			}
		}
		d, err := Extract(input(media, "<textarea>&#65</textarea>"))
		if err != nil || d.Text != "A" {
			t.Fatalf("%s printable reference: %q %v", media, d.Text, err)
		}
	}
}

func TestMarkdownContainerGapsRetainSourceOrdering(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"- <div>\n  Visible\n  </div>", "- \n  Visible"},
		{"> <div>\n> First\n> Second\n> </div>", "> \n> First\n> Second\n>"},
		{"- <div>\n  First\n  <em>Second</em>\n  Third\n  </div>", "- \n  First\n  Second\n  Third"},
	} {
		d, err := Extract(input("text/markdown", tc.body))
		if err != nil || d.Text != tc.want {
			t.Fatalf("%q: got %q want %q err=%v", tc.body, d.Text, tc.want, err)
		}
	}
	// Prefixes within tag syntax must still not mask real controls.
	if _, err := Extract(input("text/markdown", "- <div>\n  <input\n  type=password>\n  </div>")); err != publicfetch.ErrBlocked {
		t.Fatalf("multiline container challenge: %v", err)
	}
}

func TestMarkdownContainerGapsRespectRawTextState(t *testing.T) {
	for _, tag := range []string{"textarea", "xmp", "noembed", "noframes"} {
		for _, body := range []string{
			"> <" + tag + ">\n> A <foo\n> bar> B\n> </" + tag + ">",
			"> before <" + tag + ">A <foo\n> bar> B</" + tag + "> after",
		} {
			d, err := Extract(input("text/markdown", body))
			if err != nil || !strings.Contains(d.Text, "A <foo\n> bar> B") {
				t.Fatalf("%s %q reordered inert gaps: %q %v", tag, body, d.Text, err)
			}
		}
	}
}

func TestMarkdownFormattingGapsCannotChangeActiveControls(t *testing.T) {
	for _, wrappers := range [][2]string{
		{"<svg><title>", "</title></svg>"},
		{"<svg><desc>", "</desc></svg>"},
		{"<svg><foreignObject>", "</foreignObject></svg>"},
		{"<math><mtext>", "</mtext></math>"},
		{"<math><annotation-xml encoding='text/html'>", "</annotation-xml></math>"},
		{"<svg><script></svg><p>", "</p>"},
	} {
		body := "> " + wrappers[0] + "\n> <input\n> type=password>\n> " + wrappers[1]
		if _, err := Extract(input("text/markdown", body)); err != publicfetch.ErrBlocked {
			t.Fatalf("active integration point %q: %v", body, err)
		}
		inert := "> <template>" + wrappers[0] + "\n> <input\n> type=password>\n> " + wrappers[1] + "</template>"
		if _, err := Extract(input("text/markdown", inert)); err != nil {
			t.Fatalf("inert template %q: %v", inert, err)
		}
	}
	body := "> <svg><foreignObject><textarea>\n> <input\n> type=password>\n> </textarea></foreignObject></svg>"
	if _, err := Extract(input("text/markdown", body)); err != nil {
		t.Fatalf("inert RCDATA inside integration point: %v", err)
	}
}

func TestMarkdownTableGapsStayWithTheirRows(t *testing.T) {
	for _, body := range []string{
		"> <table>\n> <tr><td>A</td></tr>\n> <tr><td>B</td></tr>\n> </table>",
		"> <table>\n> <tbody>\n> <tr><td>A</td></tr>\n> <tr><td>B</td></tr>\n> </tbody>\n> </table>",
	} {
		d, err := Extract(input("text/markdown", body))
		a, b := strings.Index(d.Text, "A"), strings.Index(d.Text, "B")
		if err != nil || a < 0 || b <= a || !strings.Contains(d.Text[a+1:b], ">") || !strings.Contains(d.Text[b+1:], ">") || strings.Contains(d.Text, "WS_MD_LITERAL_") {
			t.Fatalf("table gaps moved: %q %v", d.Text, err)
		}
	}
	d, err := Extract(input("text/markdown", "> <table>\n> <tr><td>A</td></tr>\n> </table>"))
	if err != nil || d.Text != "> \n> A\n>" {
		t.Fatalf("trailing quote prefix moved before cell: %q %v", d.Text, err)
	}
}

func TestAdjacentTableCellsRemainSeparate(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, row := range []string{
			"<td>First</td><td>Second</td>",
			"<th>First</th><td>Second</td>",
			"<td>First</td><!-- comment --><td>Second</td>",
			"<td>Fir<em>st</em><td>Second",
		} {
			d, err := Extract(input(media, "<table><tr>"+row+"</tr></table>"))
			if err != nil || d.Text != "First Second" {
				t.Fatalf("%s %q: %q %v", media, row, d.Text, err)
			}
		}
	}
}

func TestTableCellSeparatorDoesNotDuplicateSourceWhitespace(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, separator := range []string{" ", "\t", "\n"} {
			d, err := Extract(input(media, "<table><tr><td>A</td>"+separator+"<td>B</td></tr></table>"))
			want := separator
			if media == "text/html" && separator == "\t" {
				want = " "
			}
			if err != nil || d.Text != "A"+want+"B" {
				t.Fatalf("%s separator=%q: %q %v", media, separator, d.Text, err)
			}
		}
		body := "<table><tr><td>" + strings.Repeat("x", MaxCharacters-2) + "</td> <td>B</td></tr></table>"
		d, err := Extract(input(media, body))
		if err != nil || d.Source.Truncated || utf8.RuneCountInString(d.Text) != MaxCharacters || !strings.HasSuffix(d.Text, " B") {
			t.Fatalf("%s normalized table cap: %d %+v %v", media, utf8.RuneCountInString(d.Text), d.Source, err)
		}
	}
}

func TestBlockElementsSeparateTextWithoutSplittingInlineWords(t *testing.T) {
	for _, tag := range []string{"address", "article", "aside", "blockquote", "center", "details", "dialog", "dir", "dl", "fieldset", "figure", "figcaption", "footer", "form", "header", "hgroup", "legend", "main", "menu", "nav", "ol", "search", "section", "summary", "ul"} {
		for _, media := range []string{"text/html", "text/markdown"} {
			body := "<" + tag + ">Fir<em>st</em></" + tag + "><" + tag + ">Second</" + tag + ">"
			d, err := Extract(input(media, body))
			if err != nil || strings.Join(strings.Fields(d.Text), " ") != "First Second" || !strings.Contains(d.Text, "\n") {
				t.Fatalf("%s %s block boundary: %q %v", media, tag, d.Text, err)
			}
		}
	}
	for _, body := range []string{"before<hr>after", "<dl><dt>First</dt><dd>Second</dd></dl>"} {
		d, err := Extract(input("text/html", body))
		if err != nil || !strings.Contains(d.Text, "\n") {
			t.Fatalf("%q block boundary: %q %v", body, d.Text, err)
		}
	}
}

func TestGeneratedBlockBoundariesCoalesce(t *testing.T) {
	for _, media := range []string{"text/html", "text/markdown"} {
		for _, body := range []string{"A<br>B", "<div>A</div><div>B</div>", "<div><section>A</section></div><div><section>B</section></div>", "<div>A</div>\n<div>B</div>"} {
			d, err := Extract(input(media, body))
			if err != nil || d.Text != "A\nB" {
				t.Fatalf("%s %q: %q %v", media, body, d.Text, err)
			}
			body = strings.Replace(body, "A", strings.Repeat("x", MaxCharacters-2), 1)
			d, err = Extract(input(media, body))
			if err != nil || d.Source.Truncated || utf8.RuneCountInString(d.Text) != MaxCharacters || !strings.HasSuffix(d.Text, "\nB") {
				t.Fatalf("%s boundary cap: %d %+v %v", media, utf8.RuneCountInString(d.Text), d.Source, err)
			}
		}
	}
	d, err := Extract(input("text/markdown", "<div>A</div>\n\n<div>B</div>"))
	if err != nil || d.Text != "A\n\nB" {
		t.Fatalf("source blank line lost: %q %v", d.Text, err)
	}
}
