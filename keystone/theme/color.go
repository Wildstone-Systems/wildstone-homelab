package theme

// The colour arithmetic every derived token goes through.
//
// WHY THIS IS IN GO AND NOT IN CSS. The Console already holds a
// contrast contract — scripts/contrast-check.mjs proves the shipped
// token set clears WCAG AA before a build passes — and that check reads
// literal values out of styles.css. A theme chosen at run time never
// appears in styles.css, so nothing in the browser would catch a
// palette whose muted text fails 4.5:1 on its own ground. The guarantee
// therefore has to be made HERE, where the tokens are derived, and it
// is made by REPAIR rather than by refusal: an imported palette with an
// illegible pair is nudged until it is legible, because taking somebody
// else's theme away for failing an audit they cannot see is a worse
// answer than showing it a shade lighter than its author drew it.
//
// Everything below works in sRGB rather than OKLCH on purpose. WCAG's
// ratio is defined on sRGB relative luminance, so the space the repair
// walks in is the space the floor is measured in, and a step is
// guaranteed to move the number it is trying to move.

import (
	"fmt"
	"math"
	"strings"
)

// rgb is one colour, 0..255 per channel, kept as float64 so that a mix
// of two colours is not quantised at every step of a repair.
type rgb [3]float64

// white and black are the two poles a repair can walk towards. Nothing
// else is ever mixed in, so a repaired colour keeps its hue.
var (
	white = rgb{255, 255, 255}
	black = rgb{0, 0, 0}
)

// parseHex reads "#rgb", "#rrggbb" or the same without the hash.
//
// Returns ok=false rather than an error value because every caller's
// answer to a bad colour is the same: fall back to the token the design
// already had. A palette is data somebody typed, and one mistyped
// swatch must not take the whole theme down.
func parseHex(s string) (rgb, bool) {
	h := strings.TrimSpace(s)
	h = strings.TrimPrefix(h, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return rgb{}, false
	}
	var c rgb
	for i := 0; i < 3; i++ {
		var v int
		if _, err := fmt.Sscanf(h[i*2:i*2+2], "%02x", &v); err != nil {
			return rgb{}, false
		}
		c[i] = float64(v)
	}
	return c, true
}

// hex renders a colour back as "#rrggbb", rounded once, at the end.
func (c rgb) hex() string {
	round := func(v float64) int {
		return int(math.Round(math.Min(255, math.Max(0, v))))
	}
	return fmt.Sprintf("#%02x%02x%02x", round(c[0]), round(c[1]), round(c[2]))
}

// quantise rounds a colour to the eight-bit values it will actually be
// written as.
//
// Repairs are computed in floating point and SERVED as "#rrggbb", and
// the rounding between those two is enough to drop a pair from 4.50 to
// 4.49. So every contrast decision is made on the quantised colour: the
// number the test measures and the number the browser draws are then
// the same number.
func (c rgb) quantise() rgb {
	var q rgb
	for i, v := range c {
		q[i] = math.Round(math.Min(255, math.Max(0, v)))
	}
	return q
}

// rgba renders a colour with an alpha, for the one place a token needs
// to be see-through (the portal's soft glow).
func (c rgb) rgba(alpha float64) string {
	round := func(v float64) int {
		return int(math.Round(math.Min(255, math.Max(0, v))))
	}
	return fmt.Sprintf("rgba(%d, %d, %d, %.2f)", round(c[0]), round(c[1]), round(c[2]), alpha)
}

// mix blends a towards b. t=0 is a, t=1 is b.
//
// Plain linear sRGB-space interpolation, which is also exactly what CSS
// color-mix(in srgb, ...) does — so a token computed here and a token
// mixed in the stylesheet land on the same colour rather than nearly.
func mix(a, b rgb, t float64) rgb {
	return rgb{
		a[0] + (b[0]-a[0])*t,
		a[1] + (b[1]-a[1])*t,
		a[2] + (b[2]-a[2])*t,
	}
}

// luminance is WCAG relative luminance.
func (c rgb) luminance() float64 {
	lin := func(v float64) float64 {
		s := v / 255
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c[0]) + 0.7152*lin(c[1]) + 0.0722*lin(c[2])
}

// contrast is the WCAG ratio between two colours, 1..21.
func contrast(a, b rgb) float64 {
	x, y := a.luminance(), b.luminance()
	if y > x {
		x, y = y, x
	}
	return (x + 0.05) / (y + 0.05)
}

// isDark says which pole a ground sits nearer, which is what decides
// the direction every repair walks in.
//
// The threshold is the luminance at which white and black text draw
// level against a ground (both land near 4.5:1), not 0.5: a mid-grey
// looks halfway to the eye and is nowhere near halfway in luminance.
func (c rgb) isDark() bool { return c.luminance() < 0.18 }

// ensureContrast returns fg unchanged if it already clears target
// against every ground given, and otherwise the nearest colour along
// the line from fg to the pole away from those grounds that does.
//
// SEVERAL GROUNDS, NOT ONE, because the surfaces a token is drawn on
// are not one colour: muted text sits on the page AND on a card, and a
// status word sits on its own tinted pill. Repairing against the
// friendlier of those would ship the unfriendly one broken.
//
// The walk is a fixed 64 steps rather than a bisection. The function is
// monotonic in t once the direction is fixed, so bisection would be
// faster and would also land on a slightly different colour depending
// on how the interval happened to split — and these values end up in a
// golden test. A fixed grid is reproducible.
func ensureContrast(fg rgb, target float64, grounds ...rgb) rgb {
	if len(grounds) == 0 {
		return fg
	}
	worst := func(c rgb) float64 {
		q := c.quantise()
		w := math.Inf(1)
		for _, g := range grounds {
			if r := contrast(q, g.quantise()); r < w {
				w = r
			}
		}
		return w
	}
	if worst(fg) >= target {
		return fg
	}
	// Walk away from the ground the colour is losing against: towards
	// white over a dark ground, towards black over a light one.
	pole := white
	if !grounds[0].isDark() {
		pole = black
	}
	const steps = 64
	for i := 1; i <= steps; i++ {
		c := mix(fg, pole, float64(i)/steps)
		if worst(c) >= target {
			return c
		}
	}
	// Nothing on the line reaches the target: the grounds themselves
	// are too far apart for any one foreground to serve both. The pole
	// is the best available answer, and it is returned rather than an
	// error because a legible-as-possible screen beats a refused one.
	return pole
}
