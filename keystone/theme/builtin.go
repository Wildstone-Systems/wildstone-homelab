package theme

// The themes that ship in the binary, and the rules a theme's name
// obeys.
//
// VENDORED, NOT FETCHED. Each file under themes/ is the `colors.toml`
// of an Omarchy theme, copied in and embedded. A household with no
// route off its own network is a posture this project supports on
// purpose, so a theme that is only available when the house can reach
// the internet is a theme that is not available. See themes/NOTICE.md
// for provenance and licence.
//
// A THEME IS DATA. Nothing in a theme is executed, rendered as markup,
// or interpolated anywhere but into CSS custom property values, and
// every value has been through parseHex before it gets that far. That
// is what makes "paste a theme somebody else wrote" a safe thing for a
// household to do.

import (
	"embed"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

//go:embed themes/*.toml
var builtinFS embed.FS

// Theme is one named palette, as a surface offers it.
type Theme struct {
	// Name is the slug: what is stored against a person and what a
	// request names.
	Name string `json:"name"`
	// Label is what a picker shows.
	Label string `json:"label"`
	// Imported is true for a theme a household pasted in, false for
	// one that shipped in the binary. Surfaced so a picker can say
	// which themes an Owner may remove.
	Imported bool `json:"imported"`
	// Dark reports the ground the theme was drawn on.
	Dark bool `json:"dark"`
	// Swatch previews it.
	Swatch Swatch `json:"swatch"`
	// Background is true when this theme brings a picture. The bytes
	// are not in this package: palettes ship in the binary, pictures
	// are megabytes a household supplies (migration 0061), so what a
	// Theme carries is whether there is one and which version.
	Background bool `json:"background,omitempty"`
	// BackgroundVersion changes when the picture does, and a surface
	// hangs it on the image URL. Without it a browser holding the
	// previous wallpaper keeps drawing it: same URL, same bytes as far
	// as it knows.
	BackgroundVersion int64 `json:"backgroundVersion,omitempty"`

	palette Palette
}

// WithBackground returns the theme with a picture attached.
func (t Theme) WithBackground(version int64) Theme {
	t.Background, t.BackgroundVersion = true, version
	return t
}

// Tokens derives this theme's block, accounting for its picture.
//
// The one place both surfaces should ask, so that "this theme has a
// wallpaper" and "this theme's inks are repaired for a wallpaper"
// cannot come apart.
func (t Theme) Tokens() Tokens {
	return t.palette.TokensWith(Options{Wallpaper: t.Background})
}

// Palette returns the theme's colours.
func (t Theme) Palette() Palette { return t.palette }

// nameRE is the shape of a theme slug. Deliberately narrow: a theme
// name is stored, put in a URL and echoed into a picker, and none of
// those want to think about what else a name could contain.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidName reports whether s may be a theme's name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Label turns a slug into the words a picker shows: "tokyo-night"
// becomes "Tokyo Night".
func Label(name string) string {
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// New builds a theme from a name and a palette, deriving the label and
// the swatch.
//
// The palette is derived ONCE here rather than on every request: the
// swatch is the token derivation run to completion, and the surfaces
// ask for the theme list far more often than the list changes.
func New(name string, p Palette, imported bool) (Theme, error) {
	if !ValidName(name) {
		return Theme{}, fmt.Errorf("theme: %q is not a usable name — lower-case letters, digits and hyphens, up to 40 characters", name)
	}
	return Theme{
		Name:     name,
		Label:    Label(name),
		Imported: imported,
		Dark:     p.Dark(),
		Swatch:   p.Swatch(),
		palette:  p,
	}, nil
}

// builtins is the embedded set, parsed once at start rather than per
// request. A file in themes/ that does not parse is a build-time
// mistake in this repository, not a household's problem, so it panics:
// shipping a binary whose theme list is silently one short is worse
// than not shipping it.
var builtins = func() []Theme {
	entries, err := builtinFS.ReadDir("themes")
	if err != nil {
		panic("theme: embedded themes are missing: " + err.Error())
	}
	var out []Theme
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		body, err := builtinFS.ReadFile(path.Join("themes", e.Name()))
		if err != nil {
			panic("theme: reading " + e.Name() + ": " + err.Error())
		}
		p, err := ParsePalette(string(body))
		if err != nil {
			panic("theme: " + e.Name() + ": " + err.Error())
		}
		t, err := New(strings.TrimSuffix(e.Name(), ".toml"), p, false)
		if err != nil {
			panic("theme: " + e.Name() + ": " + err.Error())
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}()

// Builtin returns the themes that ship in the binary, in the order a
// picker should list them.
func Builtin() []Theme {
	out := make([]Theme, len(builtins))
	copy(out, builtins)
	return out
}

// LookupBuiltin finds one shipped theme by name.
func LookupBuiltin(name string) (Theme, bool) {
	for _, t := range builtins {
		if t.Name == name {
			return t, true
		}
	}
	return Theme{}, false
}

// Default is the name a person has when they have chosen nothing.
//
// The empty string rather than a theme, and that is the whole design of
// the feature's default case: with no choice recorded, no token block
// is served and both surfaces render exactly as they do today — the
// household's own hue, out of desired state, through the sheets that
// already ship. Nothing about Wildstone's own look is reimplemented as
// a theme, so nothing about it can drift.
const Default = ""
