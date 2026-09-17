package portal

// Guards on the served surfaces themselves.
//
// These exist because of how the portal's pages went wrong twice. Both
// times the failure was invisible to every other test in the tree: the
// handlers were right, the API was right, the tests were green, and the
// page did not look like the design — first because it hand-wrote an
// approximation of a stylesheet that was already on disk, then because
// it used class names the stylesheet does not define. Neither is
// something a reviewer reliably catches by reading, and neither shows
// up as an error in a browser. So they are assertions.

import (
	"regexp"
	"strings"
	"testing"
)

// pages are the surfaces Page serves.
var pages = []string{"home", "files", "assistant", "activity", "files-widget", "assistant-widget"}

// classAttr finds every class="..." in a page.
var classAttr = regexp.MustCompile(`class="([^"{}]+)"`)

// jsClass finds the class strings the pages build at run time through
// their make()/icon() helpers — make("div", "note note-bad") and the
// like. Without this the check would pass while every dynamically
// created node carried an undefined class, which is most of what these
// pages render.
var jsClass = regexp.MustCompile(`make\("[a-z]+",\s*"([^"]+)"`)

// cssClass finds every class a stylesheet defines.
var cssClass = regexp.MustCompile(`\.([a-zA-Z][a-zA-Z0-9_-]*)`)

// Every class a page uses is defined by the shared stylesheet or by the
// page's own <style> block.
//
// A class nothing defines is not an error anywhere: the browser ignores
// it, the element renders unstyled, and the page looks subtly wrong in a
// way that reads as "the design is a bit off" rather than as a bug.
func TestEveryClassThePagesUseIsDefined(t *testing.T) {
	shared, _, ok := Asset("assets/nocturne.css")
	if !ok {
		t.Fatal("the shared stylesheet is not served")
	}
	defined := map[string]bool{}
	for _, m := range cssClass.FindAllStringSubmatch(string(shared), -1) {
		defined[m[1]] = true
	}

	for _, name := range pages {
		body, found := Page(name)
		if !found {
			t.Errorf("%s is not served", name)
			continue
		}
		page := string(body)
		// The page's own <style> counts as definitions too.
		for _, block := range regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindAllStringSubmatch(page, -1) {
			for _, m := range cssClass.FindAllStringSubmatch(block[1], -1) {
				defined[m[1]] = true
			}
		}

		used := map[string]bool{}
		for _, m := range classAttr.FindAllStringSubmatch(page, -1) {
			for _, c := range strings.Fields(m[1]) {
				used[c] = true
			}
		}
		for _, m := range jsClass.FindAllStringSubmatch(page, -1) {
			for _, c := range strings.Fields(m[1]) {
				// Concatenated fragments like "note note-" + kind are
				// checked as their prefix only; the three completions
				// are asserted separately below.
				if strings.HasSuffix(c, "-") {
					continue
				}
				used[c] = true
			}
		}
		for c := range used {
			if !defined[c] {
				t.Errorf("%s uses class %q, which no stylesheet defines", name, c)
			}
		}
	}

	// The classes the pages assemble from a variable, spelled out. Each
	// of these is a real completion of a "prefix" + value expression.
	for _, c := range []string{
		"note-bad", "note-warn", "note-info",
		"pill-ok", "pill-warn", "pill-mute", "pill-bad",
		"hbtn-primary", "hbtn-secondary", "hbtn-ghost",
		"ph-caret-up", "ph-caret-down", "ph-caret-left", "ph-caret-right",
	} {
		if !defined[c] {
			t.Errorf("class %q is assembled at run time but no stylesheet defines it", c)
		}
	}
}

// Every Phosphor glyph the pages name has a rule in the subset.
//
// The icon fonts are subset from 1530 glyphs each down to the ones in
// use, so a glyph nobody remembered to add renders as a blank or a box.
// That is exactly what happened to the whole FILL family: the first
// subset collected "ph ph-*" and silently missed every "ph-fill ph-*",
// so the filled sparkle, shield and question mark the design uses for
// states drew nothing at all.
func TestEveryGlyphThePagesNameIsInTheSubset(t *testing.T) {
	shared, _, ok := Asset("assets/nocturne.css")
	if !ok {
		t.Fatal("the shared stylesheet is not served")
	}
	css := string(shared)

	fillClass := regexp.MustCompile(`ph-fill ph-([a-z0-9-]+)`)
	regClass := regexp.MustCompile(`(?:^|[^-\w])ph ph-([a-z0-9-]+)`)
	// icon("name", true) is the fill helper; icon("name") the outline.
	fillHelper := regexp.MustCompile(`icon\("([a-z0-9-]+)",\s*true\)`)
	regHelper := regexp.MustCompile(`icon\("([a-z0-9-]+)"\)`)

	for _, name := range pages {
		body, found := Page(name)
		if !found {
			continue
		}
		page := string(body)
		for _, m := range append(fillClass.FindAllStringSubmatch(page, -1),
			fillHelper.FindAllStringSubmatch(page, -1)...) {
			if strings.HasSuffix(m[1], "-") {
				// A prefix the page completes at run time, e.g.
				// "ph-caret-" + up/down. The completions are asserted
				// by name in the class test above.
				continue
			}
			if !strings.Contains(css, ".ph-fill.ph-"+m[1]+":before") {
				t.Errorf("%s draws a FILLED %q, which is not in the icon subset", name, m[1])
			}
		}
		for _, m := range append(regClass.FindAllStringSubmatch(page, -1),
			regHelper.FindAllStringSubmatch(page, -1)...) {
			if strings.HasSuffix(m[1], "-") {
				continue
			}
			if !strings.Contains(css, ".ph-"+m[1]+":before") {
				t.Errorf("%s draws %q, which is not in the icon subset", name, m[1])
			}
		}
	}
}

// The pages must reference the shared stylesheet, or the whole asset
// pipeline is dead weight and they render unstyled.
func TestEveryPageLinksTheSharedStylesheet(t *testing.T) {
	for _, name := range pages {
		body, found := Page(name)
		if !found {
			continue
		}
		if strings.HasSuffix(name, "-widget") {
			// The two framed widgets predate the shared stylesheet and
			// carry their own; they are reached from somebody else's
			// dashboard, not from the portal's own pages.
			continue
		}
		if !strings.Contains(string(body), "/portal/assets/nocturne.css") {
			t.Errorf("%s does not link the shared stylesheet", name)
		}
	}
}

// Nothing a page loads may come from outside this origin. The portal's
// CSP forbids it, so a stray absolute URL is a silently missing asset.
func TestThePagesFetchNothingFromElsewhere(t *testing.T) {
	external := regexp.MustCompile(`(?:src|href)="(https?:)?//[^"]+"`)
	for _, name := range pages {
		body, found := Page(name)
		if !found {
			continue
		}
		for _, m := range external.FindAllString(string(body), -1) {
			t.Errorf("%s reaches outside this origin: %s", name, m)
		}
	}
}
