package theme

// An Omarchy palette, and the small parser that reads one.
//
// THE FORMAT IS SOMEBODY ELSE'S AND THAT IS THE POINT. Omarchy themes
// are distributed as a directory whose only portable part is
// `colors.toml`: twenty-odd named colours and a mode. Everything else
// in such a theme configures programs this project does not run — a
// Wayland compositor, a terminal, a status bar — and Omarchy's own
// installer strips the executable parts of a third-party theme for the
// same reason we ignore them here. So Wildstone consumes `colors.toml`
// and nothing else, which makes "use a theme from omarchythemes.com"
// one paste of one small text file rather than a git clone the control
// plane has to be trusted to run.
//
// WHY NOT A TOML LIBRARY. This is the whole grammar the format uses:
// blank lines, `# comment`, and `key = "value"`. Adding a dependency to
// Keystone — a binary that runs privileged on somebody's house — to
// parse twenty-six string assignments is a worse trade than thirty
// lines here. Anything the grammar does not cover is ignored rather
// than refused, because upstream themes already carry keys we have no
// use for (`hyprland_active_border`), and a new one appearing must not
// stop a household's theme loading.

import (
	"fmt"
	"strings"
)

// Palette is one colors.toml, with every field optional but Background
// and Foreground.
//
// The names are Omarchy's, not this project's: a person comparing this
// against the file they pasted should not have to translate. Where a
// theme omits a colour the zero value is the empty string, and the
// derivation substitutes something sensible rather than black.
type Palette struct {
	// Mode is "dark" or "light" — the ground the theme was drawn on.
	// It decides which way every derived step is mixed, so a palette
	// that lies about it looks wrong rather than merely different.
	Mode string

	Accent    string
	Selection string
	Muted     string

	Background        string
	DarkBackground    string
	DarkerBackground  string
	LighterBackground string

	Foreground       string
	DarkForeground   string
	LightForeground  string
	BrightForeground string

	Red     string
	Yellow  string
	Orange  string
	Green   string
	Cyan    string
	Blue    string
	Magenta string
	Brown   string

	BrightRed     string
	BrightYellow  string
	BrightGreen   string
	BrightCyan    string
	BrightBlue    string
	BrightMagenta string
}

// field returns the address of the Palette field a colors.toml key
// names, or nil for a key this project has no use for.
func (p *Palette) field(key string) *string {
	switch key {
	case "mode":
		return &p.Mode
	case "accent":
		return &p.Accent
	case "selection":
		return &p.Selection
	case "muted":
		return &p.Muted
	case "background", "bg":
		return &p.Background
	case "dark_background":
		return &p.DarkBackground
	case "darker_background":
		return &p.DarkerBackground
	case "lighter_background":
		return &p.LighterBackground
	case "foreground", "fg":
		return &p.Foreground
	case "dark_foreground":
		return &p.DarkForeground
	case "light_foreground":
		return &p.LightForeground
	case "bright_foreground":
		return &p.BrightForeground
	case "red":
		return &p.Red
	case "yellow":
		return &p.Yellow
	case "orange":
		return &p.Orange
	case "green":
		return &p.Green
	case "cyan":
		return &p.Cyan
	case "blue":
		return &p.Blue
	case "magenta":
		return &p.Magenta
	case "brown":
		return &p.Brown
	case "bright_red":
		return &p.BrightRed
	case "bright_yellow":
		return &p.BrightYellow
	case "bright_green":
		return &p.BrightGreen
	case "bright_cyan":
		return &p.BrightCyan
	case "bright_blue":
		return &p.BrightBlue
	case "bright_magenta":
		return &p.BrightMagenta
	}
	return nil
}

// MaxColorsBytes bounds one pasted colors.toml.
//
// Sixteen kilobytes is about forty times the largest theme Omarchy
// ships. It exists so that "paste a theme" cannot become "store an
// arbitrary blob in the control plane's database" by pasting something
// that is not a theme at all.
const MaxColorsBytes = 16 << 10

// ParsePalette reads a colors.toml.
//
// Unknown keys are skipped; a value that is not a colour is refused,
// because a palette that parses and then renders as black is a worse
// failure than one that does not parse. `mode` is the one non-colour
// value and is checked against its two legal spellings.
func ParsePalette(src string) (Palette, error) {
	if len(src) > MaxColorsBytes {
		return Palette{}, fmt.Errorf("theme: that file is larger than %d bytes", MaxColorsBytes)
	}
	var p Palette
	for n, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Palette{}, fmt.Errorf("theme: line %d is not `key = value`", n+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// A bare `"` on either end, and nothing else: a TOML string
		// with an escape in it is not a colour and not worth
		// supporting.
		value = strings.Trim(value, `"'`)
		into := p.field(key)
		if into == nil {
			continue
		}
		if key == "mode" {
			switch strings.ToLower(value) {
			case "dark", "light":
				*into = strings.ToLower(value)
				continue
			default:
				return Palette{}, fmt.Errorf("theme: line %d: mode is %q, and it must be dark or light", n+1, value)
			}
		}
		if _, ok := parseHex(value); !ok {
			return Palette{}, fmt.Errorf("theme: line %d: %s is %q, which is not a #rrggbb colour", n+1, key, value)
		}
		*into = value
	}
	if p.Background == "" || p.Foreground == "" {
		return Palette{}, fmt.Errorf("theme: a palette needs at least `background` and `foreground`")
	}
	if p.Mode == "" {
		// Not a default so much as a reading: the ground is right
		// there in the file, and trusting it beats assuming dark.
		bg, _ := parseHex(p.Background)
		p.Mode = "light"
		if bg.isDark() {
			p.Mode = "dark"
		}
	}
	return p, nil
}

// colour reads one palette field, falling back through the
// alternatives given and finally to fallback. Every derivation below
// goes through it, so a theme missing `orange` borrows `yellow`
// instead of rendering a hole.
func colour(fallback rgb, values ...string) rgb {
	for _, v := range values {
		if c, ok := parseHex(v); ok {
			return c
		}
	}
	return fallback
}

// Dark reports whether this palette's ground is a dark one.
func (p Palette) Dark() bool { return p.Mode != "light" }
