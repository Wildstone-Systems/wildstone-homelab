package portal

// The accessibility floor for every served surface.
//
// §14 lists an accessibility audit as pre-release work and names what
// it expects to find: "icon-only controls carry titles but not all
// carry labels; app cards mix <a> and role='button'; focus order
// through the widget band is untested; the skeletons respect
// prefers-reduced-motion but nothing else has been checked."
//
// An audit is a moment; these are the parts of it that can be held.
// Each one below is a defect that is invisible in a browser to somebody
// not using a screen reader, which is exactly the class of thing that
// rots between releases.

import (
	"regexp"
	"strings"
	"testing"
)

// stripComments removes HTML comments so a commented-out example never
// counts as markup. These files carry long explanatory headers, and
// several of them quote the very patterns being forbidden.
func stripComments(s string) string {
	return regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(s, "")
}

// Every button says what it does, out loud.
//
// A <button> with only an icon inside is a button whose accessible name
// is empty: a screen reader announces "button" and nothing else. The fix
// is an aria-label, and it is checked rather than reviewed because the
// page looks perfect either way.
func TestEveryIconOnlyControlHasAName(t *testing.T) {
	// A control whose entire content is one <i> glyph.
	iconOnly := regexp.MustCompile(`(?s)<(button|a)\b([^>]*)>\s*(?:<i\b[^>]*>\s*</i>\s*)+</(?:button|a)>`)
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		for _, m := range iconOnly.FindAllStringSubmatch(stripComments(string(body)), -1) {
			attrs := m[2]
			if !strings.Contains(attrs, "aria-label=") && !strings.Contains(attrs, "title=") {
				t.Errorf("%s: an icon-only <%s> has no accessible name: %s",
					name, m[1], strings.TrimSpace(m[0]))
			}
		}
	}
}

// Every button is type="button".
//
// A button with no type is a SUBMIT button. These pages carry no form
// today, so nothing breaks — which is the problem: it is a latent
// surprise waiting for the first page that grows one, and by then the
// button is somebody else's code.
func TestEveryButtonDeclaresItsType(t *testing.T) {
	button := regexp.MustCompile(`(?s)<button\b([^>]*)>`)
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		for _, m := range button.FindAllStringSubmatch(stripComments(string(body)), -1) {
			if !strings.Contains(m[1], "type=") {
				t.Errorf("%s: <button%s> has no type, so it defaults to submit", name, m[1])
			}
		}
	}
}

// Heading levels do not skip.
//
// Somebody navigating by heading uses the levels as an outline, and a
// jump from h1 to h3 tells them a section is missing. The Files page
// did exactly that.
func TestHeadingLevelsDoNotSkip(t *testing.T) {
	heading := regexp.MustCompile(`<h([1-6])\b`)
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		last := 0
		for _, m := range heading.FindAllStringSubmatch(stripComments(string(body)), -1) {
			level := int(m[1][0] - '0')
			if last != 0 && level > last+1 {
				t.Errorf("%s: heading jumps from h%d to h%d", name, last, level)
			}
			last = level
		}
	}
}

// Every page has exactly one h1.
//
// It is the page's name, and two of them means a reader cannot tell
// which one is the page.
func TestEveryPageHasOneTopHeading(t *testing.T) {
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		if n := strings.Count(stripComments(string(body)), "<h1"); n != 1 {
			t.Errorf("%s has %d <h1> elements, want exactly 1", name, n)
		}
	}
}

// Decorative glyphs are hidden from the accessibility tree.
//
// Phosphor draws through a font, so every <i> has a private-use
// character in it. Left visible, a screen reader reads the page as a
// stream of unpronounceable codepoints between the words.
func TestEveryDecorativeGlyphIsHidden(t *testing.T) {
	glyph := regexp.MustCompile(`<i\b([^>]*class="ph[^"]*"[^>]*)>`)
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		for _, m := range glyph.FindAllStringSubmatch(stripComments(string(body)), -1) {
			if !strings.Contains(m[1], "aria-hidden") {
				t.Errorf("%s: a glyph is not aria-hidden: <i%s>", name, m[1])
			}
		}
	}
}

// Every field a person types into has a label.
//
// A placeholder is not a label: it disappears the moment they type, and
// a screen reader may not announce it at all. A visually hidden <label
// for> or an aria-label is the fix, and .sr is the class for the first.
func TestEveryFieldHasALabel(t *testing.T) {
	field := regexp.MustCompile(`<(input|textarea|select)\b([^>]*)>`)
	labelFor := regexp.MustCompile(`<label\b[^>]*\bfor="([^"]+)"`)
	id := regexp.MustCompile(`\bid="([^"]+)"`)
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		page := stripComments(string(body))
		labelled := map[string]bool{}
		for _, m := range labelFor.FindAllStringSubmatch(page, -1) {
			labelled[m[1]] = true
		}
		for _, m := range field.FindAllStringSubmatch(page, -1) {
			attrs := m[2]
			// A hidden field is out of the accessibility tree and out of
			// the tab order: the file inputs these pages drive from a
			// visible button are the whole population.
			if strings.Contains(attrs, "hidden") || strings.Contains(attrs, `type="hidden"`) {
				continue
			}
			if strings.Contains(attrs, "aria-label=") {
				continue
			}
			ids := id.FindStringSubmatch(attrs)
			if ids != nil && labelled[ids[1]] {
				continue
			}
			t.Errorf("%s: <%s%s> has no label", name, m[1], attrs)
		}
	}
}

// Animation respects prefers-reduced-motion.
//
// The only animation on these surfaces is the loading skeleton's pulse,
// and the stylesheet already stops it — this holds that true as more is
// added.
func TestAnimationRespectsReducedMotion(t *testing.T) {
	css, _, ok := Asset("assets/nocturne.css")
	if !ok {
		t.Fatal("the shared stylesheet is not served")
	}
	sheet := string(css)
	if !strings.Contains(sheet, "prefers-reduced-motion") {
		t.Fatal("the stylesheet honours no reduced-motion preference")
	}
	// Every keyframe animation must be named in that block.
	for _, m := range regexp.MustCompile(`@keyframes\s+([a-zA-Z0-9_-]+)`).FindAllStringSubmatch(sheet, -1) {
		block := regexp.MustCompile(`(?s)@media \(prefers-reduced-motion: reduce\).*?\}\s*\}`).FindString(sheet)
		if !strings.Contains(block, m[1]) {
			t.Errorf("animation %q is not stopped under prefers-reduced-motion", m[1])
		}
	}
}
