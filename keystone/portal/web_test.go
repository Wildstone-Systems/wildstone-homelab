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

// An uploaded banner's media type is read from its own bytes, never
// from what the upload claimed.
//
// This surface serves the picture back from the same origin as the
// pages that hold everybody's session, so a caller who uploads an HTML
// file labelled image/png is describing a stored cross-site scripting
// bug. The only reliable answer is not to believe them.
func TestTheBannerTypeIsReadFromTheBytes(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0}, "image/jpeg"},
		{"png", append([]byte("\x89PNG\r\n\x1a\n"), 0, 0), "image/png"},
		{"gif87", []byte("GIF87a....."), "image/gif"},
		{"gif89", []byte("GIF89a....."), "image/gif"},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBP"), 0), "image/webp"},
	}
	for _, c := range cases {
		got, ok := ImageType(c.body)
		if !ok || got != c.want {
			t.Errorf("%s: got %q ok=%v, want %q", c.name, got, ok, c.want)
		}
	}

	// Everything that is not a picture is refused, and the html case is
	// the one that matters: net/http's sniffer would call it text/html
	// and a "serve what it guessed" implementation would then serve it.
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"html", []byte("<!doctype html><script>alert(1)</script>")},
		{"svg", []byte("<svg xmlns='http://www.w3.org/2000/svg'><script/></svg>")},
		{"empty", nil},
		{"short", []byte{0xFF}},
		{"webp without riff", append([]byte("XXXX\x00\x00\x00\x00WEBP"), 0)},
		{"png suffix only", []byte("....\x89PNG\r\n\x1a\n")},
	} {
		if got, ok := ImageType(c.body); ok {
			t.Errorf("%s was accepted as %q", c.name, got)
		}
	}
}

// A collapsible panel's header answers its own question while it is
// shut.
//
// The Assistant page's three side panels are twelve capability rows, a
// memory list and a receipt per action — a column taller than the
// conversation it sits beside, and a lot of height for something a
// member reads once. They start shut. That is only disclosure rather
// than hiding if the header still says what is behind it, so each one
// carries a digest the script fills in, and a caret that turns.
func TestTheAssistantsPanelsDiscloseRatherThanHide(t *testing.T) {
	body, ok := Page("assistant")
	if !ok {
		t.Fatal("the assistant page is not served")
	}
	page := stripComments(string(body))

	// Nothing starts open: an <details open> here is the thing being
	// fixed.
	if regexp.MustCompile(`<details[^>]*\bopen\b`).MatchString(page) {
		t.Error("a side panel still starts open")
	}

	// Four panels — the three on the right plus the conversation list,
	// which grows without limit on a household that talks to its
	// assistant daily — and a digest on each, filled by the script.
	panels := regexp.MustCompile(`<details class="panel">`).FindAllString(page, -1)
	if len(panels) != 4 {
		t.Errorf("found %d collapsible panels, want 4", len(panels))
	}
	// New conversation is the common action and must not be buried
	// behind the list: it sits outside the disclosure.
	if regexp.MustCompile(`(?s)<details class="panel">.{0,400}?id="new"`).MatchString(page) {
		t.Error(`"New conversation" is inside a collapsed panel`)
	}
	for _, id := range []string{"caps-digest", "memory-digest", "receipts-digest", "convos-digest"} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("no %s in the markup: a shut panel would read as blank", id)
		}
		// And something must actually write to it.
		if !strings.Contains(page, `text("`+id+`"`) {
			t.Errorf("%s is never filled in", id)
		}
	}

	// The digest sits UNDER its title, not beside it.
	//
	// Side by side in a 268px column, "What was done in your name" and
	// "2 actions · 1 refused" are wider than the card — and both were
	// nowrap, so they did not wrap, they spilled out of it. Stacking is
	// the only arrangement that cannot overflow at that width whatever
	// the digest says.
	for _, id := range []string{"caps-digest", "memory-digest", "receipts-digest", "convos-digest"} {
		if !regexp.MustCompile(`display: block"[^>]*>[^<]*</span>\s*<span class="muted" id="` + id + `"`).MatchString(page) {
			t.Errorf("%s does not sit beneath its title on its own line", id)
		}
	}

	// And nothing in a panel header may be pinned to one line.
	for _, m := range regexp.MustCompile(`(?s)<summary class="wid-head"[^>]*>(.*?)</summary>`).FindAllStringSubmatch(page, -1) {
		if strings.Contains(m[1], "nowrap") {
			t.Error("a panel header pins content to one line; in a narrow column that overflows the card")
		}
	}

	// The caret is the only thing that says there is more behind a shut
	// panel, so it has to move.
	if !strings.Contains(page, "details.panel[open] > summary .ph-caret-down") {
		t.Error("the caret does not turn, so a shut panel looks like a dead heading")
	}
	// A summary that does not look pressable is not a control.
	if !regexp.MustCompile(`details\.panel > summary \{[^}]*cursor: pointer`).MatchString(page) {
		t.Error("the panel headers are not styled as pressable")
	}
}

// Both assistant surfaces let a member see, and change, who they are
// talking to.
//
// The design says one entry is a label and not a menu, which is right
// about not drawing a dropdown with one item in it — and the first
// implementation read it as "no control at all", which left a member
// with no way to find out whether there is anyone else to talk to. The
// label is present either way and says why when pressed.
func TestBothAssistantSurfacesCarryAnAgentPicker(t *testing.T) {
	for _, name := range []string{"home", "assistant"} {
		body, ok := Page(name)
		if !ok {
			t.Fatalf("%s is not served", name)
			continue
		}
		page := stripComments(string(body))

		// The menu, for when there is more than one.
		if !strings.Contains(page, `id="agent-select"`) {
			t.Errorf("%s has no agent menu", name)
		}
		if !regexp.MustCompile(`sel\.addEventListener\("change"`).MatchString(page) {
			t.Errorf("%s never acts on a change of agent", name)
		}
		// Switching must start a new conversation. "I sent this to
		// Sandy" and "this went to Alfred" must not both be true of one
		// thread, and the server refuses a mismatched pair anyway.
		if !regexp.MustCompile(`(?s)sel\.addEventListener\("change".{0,600}?(session = ""|newConversation\(\))`).MatchString(page) {
			t.Errorf("%s switches agent without starting a new conversation", name)
		}
		// And the single-agent case says why rather than showing a dead
		// control.
		if !regexp.MustCompile(`(?s)agents\.length === 1.{0,400}?title = "The only assistant`).MatchString(page) {
			t.Errorf("%s does not explain why the agent cannot be changed when there is one", name)
		}
	}
}

// A panel opens to a few rows, not to everything it has.
//
// Twelve capability rows, a household's whole memory, every action of a
// long conversation: each is useful in full and overwhelming by
// default, so opening a panel is the first level of disclosure and
// "show more" is the second.
func TestPanelsOpenToAFewRowsRatherThanAllOfThem(t *testing.T) {
	body, ok := Page("assistant")
	if !ok {
		t.Fatal("the assistant page is not served")
	}
	page := stripComments(string(body))

	if !strings.Contains(page, "function discloseRows(") {
		t.Fatal("no second level of disclosure")
	}
	// Every list on the page goes through it, or grows without bound.
	for _, call := range []string{
		"discloseRows(box, rows,",  // what it may do
		"discloseRows(box, items,", // what it remembers
		"discloseRows(box, all,",   // what was done in your name
	} {
		if !strings.Contains(page, call) {
			t.Errorf("a list is rendered without the second level: %s", call)
		}
	}
	// The conversation list groups by day, so it has its own, and must
	// still cap.
	if !regexp.MustCompile(`paintConversations\(box, items, false\)`).MatchString(page) {
		t.Error("the conversation list is not capped")
	}

	// The control offers the REMAINDER. A member who has just read
	// "12 allowed" in the digest should not then be offered "12 more".
	for _, m := range regexp.MustCompile(`"Show " \+ (\w+)(?:\.length)? \+ " more"`).
		FindAllStringSubmatch(page, -1) {
		if m[1] == "items" || m[1] == "all" || m[1] == "rows" {
			t.Errorf(`"show more" counts the whole list (%s) rather than what is left`, m[1])
		}
	}
	if !strings.Contains(page, `"Show " + rest.length + " more"`) &&
		!strings.Contains(page, `"Show " + rest + " more"`) {
		t.Error("no control offering the remaining rows")
	}
}

// The two searches are separate, scoped, and each sits where its
// results appear.
//
// The old box filtered the app launcher from the top of the page, so
// its results were three cards below it. It is a web search now, and
// the app filter moved into the heading of the grid it filters. §7's
// objection stands and is met: what it refuses is a box that SILENTLY
// becomes an internet search, and both of these say which they are.
func TestTheWebSearchAndTheAppSearchAreSeparate(t *testing.T) {
	body, ok := Page("home")
	if !ok {
		t.Fatal("the front page is not served")
	}
	page := stripComments(string(body))

	for _, id := range []string{"websearch", "engine", "websearch-go"} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("no %s control", id)
		}
	}
	// The app filter is built into the launcher heading rather than
	// sitting in the markup at the top of the page.
	if strings.Contains(page, `<input id="appsearch"`) {
		t.Error("the app filter is back at the top of the page, away from its results")
	}
	if !strings.Contains(page, `input.id = "appsearch"`) {
		t.Error("nothing builds the app filter into the launcher heading")
	}

	// NOTHING may be sent anywhere as the member types. A fetch or an
	// XHR aimed at an engine would turn the box into a keystroke feed.
	for _, e := range Engines {
		host := strings.TrimPrefix(strings.Split(e.Query, "/?")[0], "https://")
		host = strings.Split(host, "/")[0]
		if regexp.MustCompile(`(?:fetch|XMLHttpRequest|src\s*=)[^\n]*` + regexp.QuoteMeta(host)).MatchString(page) {
			t.Errorf("the page contacts %s outside a deliberate navigation", host)
		}
	}
	// The search goes out as a link the member can see, opened in its
	// own tab — and never as a form, which this surface's
	// form-action 'none' would block anyway.
	if !strings.Contains(page, `rel="noopener noreferrer"`) {
		t.Error("the search link leaks a referrer: the engine learns the household's address")
	}
	if regexp.MustCompile(`<form\b`).MatchString(page) {
		t.Error("a form on this surface cannot submit; the CSP sets form-action 'none'")
	}
}

// The app filter cannot delete the box it is typed into.
//
// paintLauncher used to bail out when nothing matched, appending "no
// match" and returning — which skipped the group loop that BUILDS the
// filter input, so a member who mistyped had no box left to correct.
// Escape had nothing to reach either, and reloading the page was the
// only way back. A control that can remove itself is a trap.
func TestTheAppFilterSurvivesItsOwnNoMatch(t *testing.T) {
	body, ok := Page("home")
	if !ok {
		t.Fatal("the front page is not served")
	}
	page := stripComments(string(body))

	fn := regexp.MustCompile(`(?s)function paintLauncher\(term\) \{(.*?)\n  \}`).FindStringSubmatch(page)
	if fn == nil {
		t.Fatal("paintLauncher is not where this test can read it")
	}
	bodyText := fn[1]

	// The loop that renders the groups — and therefore the filter box —
	// must not be reachable only after an early exit.
	loop := strings.Index(bodyText, "groupOrder.forEach")
	if loop < 0 {
		t.Fatal("paintLauncher no longer iterates the groups")
	}
	if ret := strings.Index(bodyText, "return;"); ret >= 0 && ret < loop {
		t.Error("paintLauncher returns before rendering its groups; a no-match would take the filter box with it")
	}

	// And the empty case must still offer a way out that does not
	// require knowing a keyboard shortcut.
	if !strings.Contains(bodyText, "Nothing here matches") {
		t.Error("a filter matching nothing says nothing")
	}
	if !regexp.MustCompile(`(?s)Nothing here matches.{0,400}?"Clear"`).MatchString(bodyText) {
		t.Error("a no-match offers no control to clear the filter")
	}
}
