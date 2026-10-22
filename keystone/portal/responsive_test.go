package portal

// The pages must fit a phone.
//
// Both issues name mobile — #197 calls out the phone picker
// specifically — and §15 says every page carries a phone layout rather
// than a narrowed desktop one. None of that is visible on the machine
// these pages are written on, which is how the Files table shipped with
// a column that grew to fit the longest filename and pushed the page
// sideways: correct on a laptop, broken on the thing it is for.
//
// So the patterns that cause horizontal overflow are checked instead of
// looked at. This cannot prove a page fits — only a browser at 390px
// can — but each rule below is a thing that DOES overflow, and every one
// of them was found in this tree rather than imagined.

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// phoneWidth is the narrowest viewport the design commits to: §15's
// mobile page renders every surface in a 390×780 frame.
const phoneWidth = 390

// contentWidth is what is left after the page frame's own side padding
// (.page uses var(--space-6) twice, ~17px each). A rule wider than this
// overflows on its own.
const contentWidth = phoneWidth - 40

// stripMedia removes @media blocks, whose widths are breakpoints rather
// than element sizes. Naive brace matching is enough: these files nest
// one level inside a media query and no further.
func stripMedia(css string) string {
	var out strings.Builder
	for {
		i := strings.Index(css, "@media")
		if i < 0 {
			out.WriteString(css)
			return out.String()
		}
		out.WriteString(css[:i])
		rest := css[i:]
		depth, j := 0, 0
		for ; j < len(rest); j++ {
			if rest[j] == '{' {
				depth++
			} else if rest[j] == '}' {
				depth--
				if depth == 0 {
					j++
					break
				}
			}
		}
		css = rest[j:]
	}
}

func styleOf(page string) string {
	var b strings.Builder
	for _, m := range regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindAllStringSubmatch(page, -1) {
		b.WriteString(m[1])
	}
	return b.String()
}

// A table must not be free to size its columns to its content.
//
// An auto-layout table grows to fit the longest cell, so one long
// filename widens the whole page and text-overflow never engages —
// nothing has constrained the width it would ellipsise against. Either
// the layout is fixed, or the table lives in something that scrolls
// sideways on its own.
func TestNoTableSizesItselfToItsContent(t *testing.T) {
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		page := string(body)
		if !strings.Contains(page, "<table") && !strings.Contains(page, `make("table")`) {
			continue
		}
		css := styleOf(page)
		if strings.Contains(css, "table-layout: fixed") {
			continue
		}
		if strings.Contains(css, "overflow-x: auto") || strings.Contains(css, "overflow-x:auto") {
			continue
		}
		t.Errorf("%s has a table with neither table-layout: fixed nor a sideways-scrolling wrapper: "+
			"one long filename will widen the page", name)
	}
}

// No element is wider than a phone can show.
//
// Only unguarded literals count: a width inside min() or calc() is
// already bounded by the viewport, which is how the Catalog's dialog is
// correct at 520px and the Files one was not at 440px.
func TestNoElementIsWiderThanAPhone(t *testing.T) {
	rule := regexp.MustCompile(`(max-width|min-width|width)\s*:\s*([^;}\n]+)`)
	literal := regexp.MustCompile(`^(\d+)px$`)
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		for _, m := range rule.FindAllStringSubmatch(stripMedia(styleOf(string(body))), -1) {
			value := strings.TrimSpace(m[2])
			lit := literal.FindStringSubmatch(value)
			if lit == nil {
				// min(), calc(), %, vw, ch, auto: all bounded by
				// something other than a hard number.
				continue
			}
			px, _ := strconv.Atoi(lit[1])
			if px > contentWidth {
				t.Errorf("%s: %s: %s is wider than a phone's %dpx of content",
					name, m[1], value, contentWidth)
			}
		}
	}
}

// A grid track that cannot shrink below a phone forces a second column
// off the screen.
//
// minmax(Npx, …) with auto-fill or auto-fit collapses to one column
// when N fits; it is N itself that must fit.
func TestNoGridTrackIsWiderThanAPhone(t *testing.T) {
	track := regexp.MustCompile(`minmax\(\s*(\d+)px`)
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		for _, m := range track.FindAllStringSubmatch(stripMedia(styleOf(string(body))), -1) {
			px, _ := strconv.Atoi(m[1])
			if px > contentWidth {
				t.Errorf("%s: a grid track of %dpx cannot fit a phone's %dpx", name, px, contentWidth)
			}
		}
	}
}

// Every page states a viewport, or a phone renders it at desktop width
// and scales it down to unreadable.
func TestEveryPageDeclaresTheViewport(t *testing.T) {
	for _, name := range pages {
		body, ok := Page(name)
		if !ok {
			continue
		}
		if !strings.Contains(string(body), `name="viewport"`) {
			t.Errorf("%s declares no viewport", name)
		}
	}
}

// Controls are big enough to hit with a thumb.
//
// §15 asks for larger tap targets on a phone. The canvas stylesheet
// raises only the suggested-question chips, because those were the only
// buttons on the mockup's phone frame — so every other control sat at
// the desktop height of about 29px, under the design's own 38px and
// well under the 44px a thumb wants.
func TestControlsAreThumbSizedOnAPhone(t *testing.T) {
	css, _, ok := Asset("assets/nocturne.css")
	if !ok {
		t.Fatal("the shared stylesheet is not served")
	}
	// The phone block, whichever one carries the tap-target rules.
	blocks := regexp.MustCompile(`(?s)@media \(max-width: 720px\)\s*\{(.*?)\n\}`).
		FindAllStringSubmatch(string(css), -1)
	if len(blocks) == 0 {
		t.Fatal("the stylesheet has no phone breakpoint")
	}
	var phone strings.Builder
	for _, b := range blocks {
		phone.WriteString(b[1])
	}

	// Every control a member presses, not just the chips.
	for _, selector := range []string{".hbtn", ".seg > button"} {
		if !strings.Contains(phone.String(), selector+" {") {
			t.Errorf("%s has no phone rule; it stays at the desktop height", selector)
		}
	}
	for _, m := range regexp.MustCompile(`min-height:\s*(\d+)px`).
		FindAllStringSubmatch(phone.String(), -1) {
		px, _ := strconv.Atoi(m[1])
		if px < 38 {
			t.Errorf("a phone control is %dpx tall; the design asks for 38", px)
		}
	}
}
