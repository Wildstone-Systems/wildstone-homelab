package theme

// What a theme is held to, proved against every theme that ships.
//
// The Console's own contrast check (console/scripts/contrast-check.mjs)
// reads literal values out of styles.css and therefore cannot see a
// theme chosen at run time. These tests are that check's other half:
// the same floors, applied to the tokens this package derives, for
// every palette in the binary and for a deliberately hostile one.

import (
	"fmt"
	"strings"
	"testing"
)

// tokenColour resolves a token to a colour for the test, skipping the
// ones that are deliberately relative or are not colours at all.
func tokenColour(t *testing.T, m map[string]string, name string) rgb {
	t.Helper()
	c, ok := parseHex(m[name])
	if !ok {
		t.Fatalf("%s is %q, which is not a flat colour", name, m[name])
	}
	return c
}

// Body copy, secondary ink and rail icons clear the text floor on every
// ground they are drawn on — the page, a card, and a drawer.
func TestEveryShippedThemeIsLegible(t *testing.T) {
	for _, theme := range Builtin() {
		t.Run(theme.Name, func(t *testing.T) {
			m := theme.Palette().Tokens().Map()
			bg := tokenColour(t, m, "--color-bg")
			surface := tokenColour(t, m, "--color-surface")
			elevated := tokenColour(t, m, "--color-elevated")
			grounds := map[string]rgb{"page": bg, "card": surface, "drawer": elevated}

			for _, c := range []struct {
				token string
				floor float64
			}{
				{"--color-text", 7},
				{"--muted", 4.5},
				{"--icon", 4.5},
				{"--color-accent", 4.5},
			} {
				ink := tokenColour(t, m, c.token)
				for where, ground := range grounds {
					if got := contrast(ink, ground); got < c.floor {
						t.Errorf("%s on the %s is %.2f:1, below %.1f", c.token, where, got, c.floor)
					}
				}
			}

			// A status word is drawn on its own tint as well as on the
			// page, and the tint is the pair people actually read.
			for _, s := range []string{"ok", "warn", "bad"} {
				ink := tokenColour(t, m, "--st-"+s)
				tint := tokenColour(t, m, "--st-"+s+"-bg")
				for where, ground := range map[string]rgb{"page": bg, "card": surface, "pill": tint} {
					if got := contrast(ink, ground); got < 4.5 {
						t.Errorf("--st-%s on the %s is %.2f:1, below 4.5", s, where, got)
					}
				}
			}

			// --mark carries no text; it carries sparkline bars and
			// tile glyphs, so it is held to the graphical floor.
			if got := contrast(tokenColour(t, m, "--mark"), bg); got < 3 {
				t.Errorf("--mark on the page is %.2f:1, below 3", got)
			}
		})
	}
}

// A card has to be distinguishable from the page it sits on, or the
// whole layout flattens into one field of colour.
func TestEveryShippedThemeSeparatesItsGrounds(t *testing.T) {
	for _, theme := range Builtin() {
		t.Run(theme.Name, func(t *testing.T) {
			m := theme.Palette().Tokens().Map()
			bg := tokenColour(t, m, "--color-bg")
			surface := tokenColour(t, m, "--color-surface")
			if got := contrast(bg, surface); got < 1.04 {
				t.Errorf("a card is %.3f:1 against the page, which is not a card", got)
			}
		})
	}
}

// An operator must be able to tell "this is fine" from "this is
// broken", whatever the palette calls its colours. Omarchy's
// matte-black ships an amber `green`; taking it would have made a
// healthy service and a warning the same colour.
func TestStatusStaysSemantic(t *testing.T) {
	for _, theme := range Builtin() {
		t.Run(theme.Name, func(t *testing.T) {
			m := theme.Palette().Tokens().Map()
			ok := tokenColour(t, m, "--st-ok")
			warn := tokenColour(t, m, "--st-warn")
			bad := tokenColour(t, m, "--st-bad")
			for _, c := range []struct {
				name     string
				colour   rgb
				lo, hi   float64
				familiar string
			}{
				{"--st-ok", ok, 70, 175, "green"},
				{"--st-warn", warn, 25, 70, "amber"},
				{"--st-bad", bad, 330, 25, "red"},
			} {
				if !inFamily(c.colour, c.lo, c.hi) {
					t.Errorf("%s is %s, which is not %s", c.name, c.colour.hex(), c.familiar)
				}
			}
		})
	}
}

// The hostile case: a palette whose ink is one step from its ground, so
// nothing in it is readable as drawn. It must come out legible rather
// than refused — a household that pasted a bad theme gets a readable
// approximation of it, not a broken dashboard.
func TestAnIllegiblePaletteIsRepairedRatherThanRefused(t *testing.T) {
	p, err := ParsePalette(`
mode = "dark"
background = "#202020"
foreground = "#242424"
accent = "#262626"
green = "#1e1e1e"
yellow = "#222222"
red = "#212121"
`)
	if err != nil {
		t.Fatal(err)
	}
	m := p.Tokens().Map()
	bg := tokenColour(t, m, "--color-bg")
	if got := contrast(tokenColour(t, m, "--color-text"), bg); got < 7 {
		t.Errorf("repaired ink is %.2f:1 on its ground, below 7", got)
	}
	if got := contrast(tokenColour(t, m, "--color-accent"), bg); got < 4.5 {
		t.Errorf("repaired accent is %.2f:1 on its ground, below 4.5", got)
	}
	// Every status colour in that palette is a grey, so none is in
	// family and all three fall back to the design's own shades.
	for _, s := range []string{"ok", "warn", "bad"} {
		if _, ok := parseHex(m["--st-"+s]); !ok {
			t.Errorf("--st-%s did not resolve to a colour", s)
		}
	}
}

// The parser takes what upstream themes actually contain, and refuses
// what is not a palette.
func TestParsingAPalette(t *testing.T) {
	t.Run("keys this project has no use for are ignored", func(t *testing.T) {
		p, err := ParsePalette(`
# a comment
mode = "dark"
background = "#101010"
foreground = "#f0f0f0"
hyprland_active_border = "rgba(26a269ee) rgba(2ec27eee) 45deg"
active_tab_background = "#a5a0b6"
`)
		if err != nil {
			t.Fatalf("a real upstream theme did not parse: %v", err)
		}
		if p.Background != "#101010" {
			t.Errorf("background = %q", p.Background)
		}
	})

	t.Run("a value that is not a colour is refused", func(t *testing.T) {
		_, err := ParsePalette("background = \"#101010\"\nforeground = \"url(javascript:alert(1))\"\n")
		if err == nil {
			t.Fatal("a non-colour foreground parsed")
		}
	})

	t.Run("a file with no ground is refused", func(t *testing.T) {
		if _, err := ParsePalette(`accent = "#ff0000"`); err == nil {
			t.Fatal("a palette with no background parsed")
		}
	})

	t.Run("a missing mode is read off the ground", func(t *testing.T) {
		p, err := ParsePalette("background = \"#ffffff\"\nforeground = \"#000000\"\n")
		if err != nil {
			t.Fatal(err)
		}
		if p.Dark() {
			t.Error("a white ground was read as a dark theme")
		}
	})

	t.Run("a file larger than the bound is refused", func(t *testing.T) {
		if _, err := ParsePalette(strings.Repeat("a", MaxColorsBytes+1)); err == nil {
			t.Fatal("an oversized file parsed")
		}
	})
}

// Nothing a palette can contain reaches the stylesheet as anything but
// a colour. The parser is the gate; this proves the gate holds for the
// shapes somebody would actually try.
func TestNothingEscapesIntoTheStylesheet(t *testing.T) {
	for _, attempt := range []string{
		`accent = "red; } :root { display: none"`,
		`accent = "#fff; background: url(http://elsewhere/)"`,
		`accent = "</style><script>alert(1)</script>"`,
		`accent = "expression(alert(1))"`,
	} {
		src := "background = \"#101010\"\nforeground = \"#f0f0f0\"\n" + attempt + "\n"
		if _, err := ParsePalette(src); err == nil {
			t.Errorf("this parsed and should not have: %s", attempt)
		}
	}

	// And a palette that DOES parse produces a block containing
	// nothing but tokens.
	for _, theme := range Builtin() {
		css := theme.Palette().Tokens().CSS()
		if strings.Count(css, "{") != 1 || strings.Count(css, "}") != 1 {
			t.Errorf("%s produced a block with more than one rule in it", theme.Name)
		}
		for _, forbidden := range []string{"<", ">", "url(", "@import", "expression("} {
			if strings.Contains(css, forbidden) {
				t.Errorf("%s: the block contains %q", theme.Name, forbidden)
			}
		}
	}
}

// The block has to win against the sheet it replaces. Each shipped
// sheet defines its tokens on :root, on :root[data-theme="light"] and
// inside a media query, and the attribute forms outrank a bare :root.
func TestTheBlockIsSpecificEnoughToWin(t *testing.T) {
	css := Builtin()[0].Palette().Tokens().CSS()
	for _, selector := range []string{":root,", `:root[data-theme="dark"],`, `:root[data-theme="light"] {`} {
		if !strings.Contains(css, selector) {
			t.Errorf("the block does not match %s, so a person who set a theme would not get one", selector)
		}
	}
}

// Both surfaces read these; a token dropped here is a screen that
// renders with a stale colour from the sheet underneath.
func TestTheBlockCoversEveryTokenTheSurfacesRead(t *testing.T) {
	// Every colour-bearing custom property named in console/src and in
	// keystone/portal/web. Spatial tokens (--space-*, --radius-*) and
	// the fonts are NOT themed: a palette says nothing about them, and
	// letting a theme move them would let a theme break a layout.
	want := []string{
		"--color-accent", "--color-accent-200", "--color-accent-300", "--color-accent-400",
		"--color-bg", "--color-border", "--color-divider", "--color-elevated",
		"--color-surface", "--color-text", "--faint", "--glow", "--glow-soft",
		"--hair", "--icon", "--mark", "--muted",
		"--shadow-lg", "--shadow-md", "--shadow-sm",
		"--st-bad", "--st-bad-bg", "--st-ok", "--st-ok-bg", "--st-warn", "--st-warn-bg",
	}
	have := map[string]bool{}
	for _, n := range Builtin()[0].Palette().Tokens().Names() {
		have[n] = true
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("%s is read by a surface and is not in the themed block", n)
		}
	}
}

// One palette, one block, every time: the CSS is served to browsers and
// compared in review, so it must not shuffle between builds.
func TestTheBlockIsStable(t *testing.T) {
	theme, ok := LookupBuiltin("tokyo-night")
	if !ok {
		t.Fatal("tokyo-night is not in the binary")
	}
	first := theme.Palette().Tokens().CSS()
	for i := 0; i < 8; i++ {
		if got := theme.Palette().Tokens().CSS(); got != first {
			t.Fatal("the same palette produced two different blocks")
		}
	}
}

// Every theme in the directory is loadable, named legally, and previews
// as four real colours.
func TestTheShippedSetLoads(t *testing.T) {
	all := Builtin()
	if len(all) < 20 {
		t.Fatalf("only %d themes shipped", len(all))
	}
	seen := map[string]bool{}
	for _, theme := range all {
		if !ValidName(theme.Name) {
			t.Errorf("%q is not a usable theme name", theme.Name)
		}
		if seen[theme.Name] {
			t.Errorf("%q ships twice", theme.Name)
		}
		seen[theme.Name] = true
		if theme.Label == "" {
			t.Errorf("%q has no label", theme.Name)
		}
		s := theme.Swatch
		for name, v := range map[string]string{
			"background": s.Background, "surface": s.Surface,
			"text": s.Text, "accent": s.Accent,
		} {
			if _, ok := parseHex(v); !ok {
				t.Errorf("%s: swatch %s is %q", theme.Name, name, v)
			}
		}
	}
}

func TestNamesAreCheckedBeforeTheyAreStored(t *testing.T) {
	for _, bad := range []string{"", "Tokyo Night", "../etc/passwd", "a/b", "théme", strings.Repeat("x", 41)} {
		if ValidName(bad) {
			t.Errorf("%q was accepted as a theme name", bad)
		}
	}
	for _, good := range []string{"a", "tokyo-night", "retro-82"} {
		if !ValidName(good) {
			t.Errorf("%q was refused as a theme name", good)
		}
	}
}

func TestLabelsReadAsWords(t *testing.T) {
	for in, want := range map[string]string{
		"tokyo-night":      "Tokyo Night",
		"catppuccin-latte": "Catppuccin Latte",
		"retro-82":         "Retro 82",
		"matte-black":      "Matte Black",
		"osaka-jade":       "Osaka Jade",
		"everforest":       "Everforest",
	} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %q, want %q", in, got, want)
		}
	}
}

// A sanity check that the derivation is actually using the palette
// rather than producing the same block for everything.
func TestDifferentPalettesProduceDifferentBlocks(t *testing.T) {
	seen := map[string]string{}
	for _, theme := range Builtin() {
		css := theme.Palette().Tokens().CSS()
		if other, dup := seen[css]; dup {
			t.Errorf("%s and %s produced identical blocks", theme.Name, other)
		}
		seen[css] = theme.Name
	}
	fmt.Fprintf(nopWriter{}, "%d distinct blocks", len(seen))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
