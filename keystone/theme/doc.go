// Package theme turns a third-party colour palette into the token block
// Wildstone's two household-facing surfaces are drawn from.
//
// The Console (ADR-W027) and the household portal share one design
// system, Nocturne, and both express it as CSS custom properties with
// no colour literal anywhere below the token block. That was done so
// the light theme could be a token swap; it is also, without any change
// to either sheet, the seam a whole third-party theme fits into.
//
// A theme here is an Omarchy `colors.toml` and nothing else: twenty-odd
// named colours and a mode. Omarchy themes also carry configuration for
// a compositor, a terminal and a status bar, none of which Wildstone
// runs, and none of which is read.
//
// The three things this package guarantees, and the reason it exists
// rather than a stylesheet per theme:
//
//   - A palette gives colours; a design system gives relationships. The
//     card ground, the drawer ground, the muted ink, the hairline, the
//     status tints and the shadow rings are all derived here, so an
//     imported theme looks like Wildstone wearing that palette.
//
//   - Legibility is enforced, not assumed. Every pair that carries text
//     is repaired to WCAG AA against every ground it is drawn on, and
//     body copy to AAA. A palette that cannot make a pair legible is
//     nudged until it can rather than refused.
//
//   - Status stays semantic. A terminal palette is under no obligation
//     to make `green` green; an operator screen is. A palette's colour
//     is taken for a status token only when it is in that colour's
//     family.
package theme
