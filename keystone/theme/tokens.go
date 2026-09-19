package theme

// Turning a palette into the token set both Wildstone surfaces are
// drawn from.
//
// THE TOKENS ARE ALREADY THE CONTRACT. ADR-W027 made the Console a
// sheet in which "no rule below this block may name a colour literal,
// so that swapping the block swaps the console", and the household
// portal's nocturne.css was ported from the same canvases with the same
// property. That is the whole reason a third-party palette can be
// applied here at all: there is exactly one block to replace on each
// surface, and this file computes it.
//
// WHAT IS DERIVED AND WHAT IS BORROWED. A palette gives grounds, an ink
// and a set of named colours. It does not give a card ground, a drawer
// ground, a muted ink, a hairline, a status tint or a shadow ring —
// those are relationships, and relationships are what a design system
// owns. So the palette's colours are borrowed and everything BETWEEN
// them is derived here, which is why an imported theme comes out
// looking like Wildstone wearing that palette rather than like a
// terminal colour scheme pasted onto a web page.
//
// SEMANTIC STATUS IS NOT DECORATION. ADR-W027's token block says it in
// as many words: "an operator must be able to tell 'this is selected'
// from 'this is broken'". Terminal palettes are under no such
// obligation — Omarchy's matte-black ships `green = "#FFC107"`, which
// is amber — so a palette colour is taken for a status token only when
// it is actually in that colour's family, and the design's own semantic
// shade is used when it is not. A household should be able to pick a
// theme it likes without quietly losing the ability to see that
// something is wrong.

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Veil is how much of the page ground stays in front of a wallpaper.
//
// A theme's picture is the page's ground, not a decoration on it, so
// everything the surface draws directly on the page — headings, the
// muted labels between cards, the eyebrow rows — is drawn over an
// arbitrary photograph. A photograph has no contrast floor. It can be
// white where the ink is white and black where the ink is black, in
// the same image.
//
// So the ground stays: the picture shows THROUGH the page colour at
// this opacity, and every ink is then derived against the two extremes
// that veil can produce — the ground over a white pixel and the ground
// over a black one. Both are computed below and both are handed to the
// same repair every other token goes through. The consequence is
// visible and worth stating: a person who chooses a theme with a
// picture gets slightly brighter ink than the same theme without one,
// because that is what it costs to stay legible over an unknown image.
//
// The number itself is a judgement, and the only one here. Lower shows
// more of the picture and pushes the inks further; higher is a flatter
// ground that looks more like the theme without its wallpaper. This is
// the one line to change to trade those off.
const Veil = 0.86

// veilMargin is how much weaker than Veil the repair pretends the veil
// is.
//
// The browser composites rgba(ground, Veil) over the photograph at run
// time, in whatever precision it uses, and the difference between that
// and the arithmetic here lands in the second decimal place of a
// contrast ratio — which is exactly where a pair repaired to land
// EXACTLY on 4.50 falls off. So the inks are repaired against a
// slightly thinner veil than the one that ships. What the browser
// actually draws is then strictly easier to read than what was
// checked, with headroom rather than a coin toss.
const veilMargin = 0.02

// Token is one CSS custom property, ready to write.
type Token struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Tokens is a themed token block in the order it is written. Ordered
// rather than a map so that the CSS a given palette produces is
// byte-identical every time, which is what lets it be compared in a
// test and cached by a browser.
type Tokens []Token

// Map is the same block as JSON-friendly pairs, for the Console, which
// applies the tokens as inline custom properties rather than as a
// stylesheet.
func (t Tokens) Map() map[string]string {
	m := make(map[string]string, len(t))
	for _, tok := range t {
		m[tok.Name] = tok.Value
	}
	return m
}

// hueOf returns the colour's hue in degrees, and false for a grey,
// where a hue is not a meaningful question.
func hueOf(c rgb) (float64, bool) {
	maxv := math.Max(c[0], math.Max(c[1], c[2]))
	minv := math.Min(c[0], math.Min(c[1], c[2]))
	d := maxv - minv
	// Below this the colour is grey enough that its hue is numerical
	// noise: a #bebebe rounds to some hue, and it is not a green.
	if d < 12 {
		return 0, false
	}
	var h float64
	switch maxv {
	case c[0]:
		h = math.Mod((c[1]-c[2])/d, 6)
	case c[1]:
		h = (c[2]-c[0])/d + 2
	default:
		h = (c[0]-c[1])/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	return h, true
}

// inFamily says whether a colour is recognisably the colour a status
// token needs it to be. lo may be greater than hi, for red, which wraps
// through zero.
func inFamily(c rgb, lo, hi float64) bool {
	h, ok := hueOf(c)
	if !ok {
		return false
	}
	if lo <= hi {
		return h >= lo && h <= hi
	}
	return h >= lo || h <= hi
}

// The design's own semantic shades (ADR-W027), used when a palette's
// named colour is not in the family the token means.
var (
	semanticOK   = rgb{0x5f, 0xae, 0x86}
	semanticWarn = rgb{0xd9, 0xa2, 0x4b}
	semanticBad  = rgb{0xd9, 0x73, 0x6f}
)

// statusBase picks the colour a status token starts from: the
// palette's, when it is in family, and the design's when it is not.
func statusBase(fallback rgb, lo, hi float64, values ...string) rgb {
	for _, v := range values {
		c, ok := parseHex(v)
		if !ok {
			continue
		}
		if inFamily(c, lo, hi) {
			return c
		}
	}
	return fallback
}

// Options is what the surfaces know about a theme that the palette
// does not say.
type Options struct {
	// Wallpaper is true when this theme has a picture behind the page.
	// It widens every ink repair to cover what the picture can put
	// under the ground, and adds the veil the picture shows through.
	Wallpaper bool
}

// Tokens derives the block for a theme with no picture.
func (p Palette) Tokens() Tokens { return p.TokensWith(Options{}) }

// TokensWith derives the full themed block.
//
// Every ratio named below is WCAG AA on the grounds the token is
// actually drawn on, and the repair that enforces it is in color.go.
// The one exception is --mark, which carries no text: it marks
// sparkline bars and tile glyphs, so it is held to the 3:1 graphical
// floor instead of the 4.5:1 text one.
func (p Palette) TokensWith(o Options) Tokens {
	dark := p.Dark()
	pole := white
	if !dark {
		pole = black
	}

	bg := colour(rgb{0x16, 0x18, 0x26}, p.Background)

	// The card ground. A palette that gives no lighter ground, or gives
	// one indistinguishable from the page, gets one derived — two cards
	// on a page they cannot be told apart from is not a theme, it is a
	// missing token.
	surface := colour(mix(bg, pole, 0.07), p.LighterBackground)
	if math.Abs(surface.luminance()-bg.luminance()) < 0.006 {
		surface = mix(bg, pole, 0.07)
	}

	// The ground a drawer or dialog sits on. ADR-W027: one step ABOVE
	// the page on dark, and WHITER than the page on light, so a card
	// inside a drawer still reads as a card either way.
	elevated := mix(bg, surface, 0.35)
	if !dark {
		elevated = mix(surface, white, 0.75)
	}

	grounds := []rgb{bg, surface, elevated}
	// With a picture behind the page, the page ground is no longer one
	// colour: it is the ground veiled over whatever the photograph has
	// there. Only the two extremes need checking — a repair that holds
	// against a white pixel and a black one holds against everything
	// between, because luminance is monotonic along the blend.
	if o.Wallpaper {
		thin := Veil - veilMargin
		grounds = append(grounds, mix(white, bg, thin).quantise(), mix(black, bg, thin).quantise())
	}

	// Body copy, held to AAA rather than AA. Every screen in both
	// surfaces is mostly this token, the current design clears it
	// comfortably on both grounds, and the repair only ever moves the
	// ink further from the ground it is on — it cannot change its hue.
	text := ensureContrast(colour(pole, p.Foreground, p.BrightForeground), 7, grounds...)

	// Secondary ink and rail icons. Both start as a blend of the ink
	// towards the page and are then held to the text floor, which is
	// what the Console's own contrast check holds the shipped sheet to.
	// Icons start closer to full ink because a 1.5px stroke reads
	// lighter than text at the same ratio.
	muted := ensureContrast(mix(bg, text, 0.62), 4.5, grounds...)
	icon := ensureContrast(mix(bg, text, 0.72), 4.5, grounds...)

	accent := ensureContrast(colour(text, p.Accent, p.Blue, p.BrightBlue), 4.5, grounds...)
	// The accent tint that marks carry. Pale on dark, deep on light:
	// a pale tint on a pale ground disappears, which is the same
	// reasoning the light Console theme records for taking the 800 step
	// where dark takes the 300.
	mark := ensureContrast(mix(accent, pole, 0.35), 3, grounds...)

	ok := statusBase(semanticOK, 70, 175, p.Green, p.BrightGreen)
	warn := statusBase(semanticWarn, 25, 70, p.Yellow, p.Orange, p.BrightYellow)
	bad := statusBase(semanticBad, 330, 25, p.Red, p.BrightRed)

	// A status tint is the page with a breath of the status colour in
	// it, and the word on top is then held to 4.5:1 against that tint
	// as well as against the page.
	okBG, warnBG, badBG := mix(bg, ok, 0.18), mix(bg, warn, 0.18), mix(bg, bad, 0.18)
	// A status word appears on the page as well as on its own pill, so
	// it is held to every ground the page can be — the veiled extremes
	// included, when there is a picture behind it.
	onPageAnd := func(extra rgb) []rgb { return append(append([]rgb{}, grounds...), extra) }
	ok = ensureContrast(ok, 4.5, onPageAnd(okBG)...)
	warn = ensureContrast(warn, 4.5, onPageAnd(warnBG)...)
	bad = ensureContrast(bad, 4.5, onPageAnd(badBG)...)

	// Shadow rings. The design draws elevation as a hairline first and
	// a drop second, so the ring is a mix of ink into the ground and
	// only the drop is black.
	ring := func(t float64) string { return mix(bg, text, t).hex() }
	drop := func(a float64) string {
		if dark {
			return black.rgba(a)
		}
		return text.rgba(a * 0.3)
	}

	// The accent ramp the portal's own components reach for. On a dark
	// ground the steps go pale; on a light one they go deep, for the
	// reason recorded on --mark.
	step := func(t float64) string { return mix(accent, pole, t).hex() }

	scheme := "dark"
	if !dark {
		scheme = "light"
	}

	block := Tokens{
		{"color-scheme", scheme},

		{"--color-bg", bg.hex()},
		{"--color-surface", surface.hex()},
		{"--color-elevated", elevated.hex()},
		{"--color-text", text.hex()},

		{"--color-accent", accent.hex()},
		{"--mark", mark.hex()},

		{"--muted", muted.hex()},
		{"--icon", icon.hex()},
		// Hairlines and washes stay relative to the ink, exactly as the
		// shipped sheets write them: they carry no text, so there is
		// nothing to hold to a ratio, and leaving them as a mix means a
		// surface that later retunes its ink retunes these with it.
		{"--hair", "color-mix(in srgb, var(--color-text) 11%, transparent)"},
		{"--faint", "color-mix(in srgb, var(--color-text) 7%, transparent)"},
		{"--color-border", mix(bg, text, 0.18).hex()},
		{"--color-divider", "color-mix(in srgb, var(--color-text) 16%, transparent)"},

		{"--st-ok", ok.hex()},
		{"--st-ok-bg", okBG.hex()},
		{"--st-warn", warn.hex()},
		{"--st-warn-bg", warnBG.hex()},
		{"--st-bad", bad.hex()},
		{"--st-bad-bg", badBG.hex()},

		{"--color-accent-200", step(0.65)},
		{"--color-accent-300", step(0.45)},
		{"--color-accent-400", step(0.2)},
		{"--glow", mix(bg, accent, 0.45).hex()},
		{"--glow-soft", mix(bg, accent, 0.45).rgba(0.3)},

		{"--shadow-sm", fmt.Sprintf("0 0 0 1px %s", ring(0.22))},
		{"--shadow-md", fmt.Sprintf("0 0 0 1px %s, 0 6px 18px %s", ring(0.36), drop(0.55))},
		{"--shadow-lg", fmt.Sprintf("0 0 0 1px %s, 0 16px 40px %s", ring(0.52), drop(0.65))},
	}
	if o.Wallpaper {
		// A gradient rather than a colour, because it is a background
		// LAYER: it sits in the same stack as the picture, one step in
		// front of it. Two identical stops is how CSS spells "a flat
		// wash" in a slot that wants an image.
		block = append(block, Token{
			"--wallpaper-veil",
			fmt.Sprintf("linear-gradient(%s, %s)", bg.rgba(Veil), bg.rgba(Veil)),
		})
	}
	return block
}

// Swatch is the handful of colours a picker draws a theme as, so a
// person chooses by looking rather than by reading a name.
type Swatch struct {
	Background string `json:"background"`
	Surface    string `json:"surface"`
	Text       string `json:"text"`
	Accent     string `json:"accent"`
}

// Swatch reduces the derived tokens to what a preview tile needs. It
// reads the DERIVED values rather than the palette's raw ones, so the
// tile shows the theme as the surfaces will actually render it —
// including any contrast repair.
func (p Palette) Swatch() Swatch {
	m := p.Tokens().Map()
	return Swatch{
		Background: m["--color-bg"],
		Surface:    m["--color-surface"],
		Text:       m["--color-text"],
		Accent:     m["--color-accent"],
	}
}

// CSS renders the block both household surfaces are served.
//
// THE SELECTOR LIST IS LOAD-BEARING. Each shipped sheet defines its
// tokens three times — on :root, on :root[data-theme="light"], and
// inside a prefers-color-scheme media query — and the two attribute
// forms are MORE specific than a bare :root. A block written only for
// :root would therefore lose to the sheet it is meant to replace on
// exactly the machines whose owner had set a theme. Matching all three
// selectors makes this block equally specific and later in the
// document, which is what wins.
func (t Tokens) CSS() string {
	var b strings.Builder
	b.WriteString(":root,\n:root[data-theme=\"dark\"],\n:root[data-theme=\"light\"] {\n")
	for _, tok := range t {
		fmt.Fprintf(&b, "  %s: %s;\n", tok.Name, tok.Value)
	}
	b.WriteString("}\n")
	return b.String()
}

// Names returns the token names in sorted order, for tests that want to
// assert the shape of the block without depending on its order.
func (t Tokens) Names() []string {
	out := make([]string, 0, len(t))
	for _, tok := range t {
		out = append(out, tok.Name)
	}
	sort.Strings(out)
	return out
}
