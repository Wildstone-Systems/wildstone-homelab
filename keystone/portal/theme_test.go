package portal

// Injecting a person's theme into a page.

import (
	"strings"
	"testing"
)

func TestInjectingATheme(t *testing.T) {
	const css = ":root { --color-bg: #010203; }\n"

	t.Run("the block lands at the marker", func(t *testing.T) {
		got := string(InjectTheme([]byte("<head>\n<!--wildstone:theme-->\n</head>"), css))
		if !strings.Contains(got, "<style>\n"+css+"</style>") {
			t.Errorf("the block is not in the page:\n%s", got)
		}
		if strings.Contains(got, themeMarker) {
			t.Error("the marker is still in the page")
		}
	})

	t.Run("no theme leaves the page exactly as it was", func(t *testing.T) {
		page := []byte("<head>\n<!--wildstone:theme-->\n</head>")
		if got := string(InjectTheme(page, "")); got != string(page) {
			t.Errorf("an empty block changed the page:\n%s", got)
		}
	})

	// A token block in the wrong place is worse than none: it would
	// either lose to the stylesheet it is meant to replace or win over
	// a page rule that was deliberately more specific.
	t.Run("a page with no marker is left alone rather than patched", func(t *testing.T) {
		page := []byte("<head></head><body>hello</body>")
		if got := string(InjectTheme(page, css)); got != string(page) {
			t.Errorf("a page with no marker was patched:\n%s", got)
		}
	})

	// Every page a person can open has somewhere to put their theme.
	// A page that quietly had none would be the one screen in the house
	// that ignored what they chose.
	t.Run("every full page carries a marker", func(t *testing.T) {
		for _, name := range []string{"home", "files", "assistant", "activity", "catalog", "calendar", "upkeep"} {
			body, ok := Page(name)
			if !ok {
				t.Errorf("%s is not served", name)
				continue
			}
			if !strings.Contains(string(body), themeMarker) {
				t.Errorf("%s has nowhere to put a theme", name)
			}
			// After the design system's stylesheet, or the block would
			// lose to the very rules it exists to replace.
			if strings.Index(string(body), themeMarker) < strings.Index(string(body), "nocturne.css") {
				t.Errorf("%s puts the theme before the stylesheet it has to win against", name)
			}
		}
	})

	// The widgets are deliberately NOT themed. They are iframed into
	// somebody else's dashboard card and painted to match that card
	// (their own two palettes, chosen by a ?theme= the dashboard sets),
	// so a person's theme there would fight the panel it sits on.
	t.Run("the embedded widgets are left to their host", func(t *testing.T) {
		for _, name := range []string{"files-widget", "assistant-widget"} {
			body, ok := Page(name)
			if !ok {
				t.Errorf("%s is not served", name)
				continue
			}
			if strings.Contains(string(body), themeMarker) {
				t.Errorf("%s is themed, and it is drawn on the dashboard's card rather than on ours", name)
			}
		}
	})
}
