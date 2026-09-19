package api

// Appearance: which theme a person is on, and which themes there are.
//
// ONE RESOLVER, TWO SURFACES. The Console and the household portal ask
// different doors — one an authenticated operator session, the other a
// household member arriving through the identity proxy — and both land
// on resolveAppearance below. That is the whole of why a theme "follows
// a person across surfaces": there is one row, one lookup and one
// derivation, so the dashboard on the kitchen tablet and the Console on
// a laptop cannot disagree about what somebody chose.
//
// THE CHOICE IS THE PERSON'S AND NOBODY ELSE'S. There is no
// administrative override here, deliberately. An Owner decides which
// themes EXIST for this household — that is what importing and removing
// are — and each person decides which one they read on. An Owner who
// could set somebody else's theme could make a screen their housemate
// cannot read, and no household setting is worth that.
//
// AN UNKNOWN NAME IS NOT AN ERROR. A theme may be removed while people
// are on it. Resolution returns "no theme" for a name it cannot find,
// which renders as the design's own look, and the row is left alone so
// that pasting the theme back restores everybody who had it.

import (
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/theme"
)

// appearance is what a surface needs to paint itself and to draw a
// picker: what this person is on, what else there is, and the tokens.
type appearance struct {
	// Theme is the name this person chose, or "" for the design's own
	// look. It is what they CHOSE, not what resolved — so a picker
	// shows a removed theme as still selected rather than silently
	// moving the highlight.
	Theme string `json:"theme"`
	// Tokens is the themed block, absent when there is no theme to
	// apply. The surface applies it as custom properties; there is no
	// second copy of the derivation in TypeScript.
	Tokens map[string]string `json:"tokens,omitempty"`
	// Dark says which ground the resolved theme sits on, so a surface
	// can set color-scheme and data-theme before it paints.
	Dark bool `json:"dark"`
	// Themes is everything this household can choose from.
	Themes []theme.Theme `json:"themes"`
}

// householdThemes parses the pasted themes.
//
// A stored theme that no longer parses is SKIPPED rather than returned
// as an error: the parser's rules can tighten between releases, and a
// household whose theme list stopped loading — taking the picker and
// everybody else's theme with it — would be a bad way to find that out.
func (s *Server) householdThemes() []theme.Theme {
	if s.DB == nil {
		return nil
	}
	rows, err := s.DB.ListHouseholdThemes()
	if err != nil {
		return nil
	}
	var out []theme.Theme
	for _, r := range rows {
		p, err := theme.ParsePalette(r.Colors)
		if err != nil {
			continue
		}
		t, err := theme.New(r.Name, p, true)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out
}

// themeCatalog is everything a person may choose: what ships in the
// binary, then what the household pasted in.
//
// A pasted theme whose name matches a shipped one REPLACES it rather
// than appearing twice. That is the household overriding a default,
// which is the same precedence a user theme has in Omarchy itself, and
// it is what lets a household retune a shipped theme it nearly likes.
func (s *Server) themeCatalog() []theme.Theme {
	out := theme.Builtin()
	for _, t := range s.householdThemes() {
		replaced := false
		for i := range out {
			if out[i].Name == t.Name {
				out[i], replaced = t, true
				break
			}
		}
		if !replaced {
			out = append(out, t)
		}
	}
	return out
}

// lookupTheme resolves a name against the catalog.
func (s *Server) lookupTheme(name string) (theme.Theme, bool) {
	if name == theme.Default {
		return theme.Theme{}, false
	}
	for _, t := range s.themeCatalog() {
		if t.Name == name {
			return t, true
		}
	}
	return theme.Theme{}, false
}

// resolveAppearance answers both surfaces.
func (s *Server) resolveAppearance(member string) appearance {
	a := appearance{Themes: s.themeCatalog(), Dark: true}
	if s.DB == nil {
		return a
	}
	chosen, err := s.DB.GetPersonTheme(member)
	if err != nil {
		// A database that cannot answer is not a reason to refuse a
		// page. The person gets the design's own look and the picker
		// still works.
		return a
	}
	a.Theme = chosen
	if t, ok := s.lookupTheme(chosen); ok {
		a.Tokens = t.Palette().Tokens().Map()
		a.Dark = t.Dark
	}
	return a
}

// setTheme records a choice for one person, refusing a name nothing
// answers to.
//
// Refusing here but tolerating an unknown name on READ is not an
// inconsistency. A write is somebody pressing a button on a list this
// server just sent them, so a name that is not on it is a bug or a
// forgery. A read may be happening months later, after the theme was
// removed, and must still paint a page.
func (s *Server) setTheme(member, name string) (int, error) {
	if s.DB == nil {
		return http.StatusServiceUnavailable, fmt.Errorf("this is not ready yet")
	}
	if name != theme.Default {
		if !theme.ValidName(name) {
			return http.StatusBadRequest, fmt.Errorf("%q is not a theme name", name)
		}
		if _, ok := s.lookupTheme(name); !ok {
			return http.StatusNotFound, fmt.Errorf("there is no theme called %q", name)
		}
	}
	if err := s.DB.SetPersonTheme(member, name); err != nil {
		return http.StatusBadRequest, err
	}
	return http.StatusOK, nil
}

// recordThemeEvent notes an Owner changing what the household may
// choose from.
//
// Which themes exist is household-visible configuration, so adding and
// removing one belongs in the activity log beside every other such
// change. A PERSON's own choice is not logged: what somebody likes to
// read on is nobody else's business, and a log of it would be a record
// of when each member was at their screen.
func (s *Server) recordThemeEvent(typ string, id principal.Identity, msg string) {
	if s.DB == nil {
		return
	}
	if _, err := s.DB.AppendEvent(runtimedb.Event{
		Type:    typ,
		Message: msg,
		Attrs:   map[string]string{"principal": id.Name, "actingFor": id.ActingFor},
	}); err != nil && s.Log != nil {
		s.Log.Error("event append failed", "type", typ, "err", err)
	}
}

// ------------------------------------------------------------- console

// getAppearance answers the Console for whoever is asking.
func (s *Server) getAppearance(_ *http.Request, id principal.Identity) (any, int, error) {
	return s.resolveAppearance(actingMember(id)), http.StatusOK, nil
}

// setAppearance records the Console caller's own choice.
func (s *Server) setAppearance(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Theme string `json:"theme"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if code, err := s.setTheme(actingMember(id), req.Theme); err != nil {
		return nil, code, err
	}
	return s.resolveAppearance(actingMember(id)), http.StatusOK, nil
}

// importTheme takes a pasted colors.toml.
//
// AN OWNER'S CALL, NOT A MEMBER'S — VerbManage at the mux — because it
// changes what the whole household sees on the picker, and because a
// household that let anybody add a theme would have given everybody a
// way to write into the control plane's database.
//
// The palette is parsed HERE, before it is stored, so a file that is
// not a palette is refused at the door with a message naming the line.
// Storing first and discovering later would put a row in the database
// that nothing can render and nothing removes.
func (s *Server) importTheme(r *http.Request, id principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this is not ready yet")
	}
	var req struct {
		Name   string `json:"name"`
		Colors string `json:"colors"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if !theme.ValidName(req.Name) {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"a theme's name is lower-case letters, digits and hyphens — %q is not", req.Name)
	}
	palette, err := theme.ParsePalette(req.Colors)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	t, err := theme.New(req.Name, palette, true)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.DB.SaveHouseholdTheme(req.Name, req.Colors, actingMember(id)); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.recordThemeEvent("theme.imported", id, fmt.Sprintf("%s added the theme %q", actingMember(id), t.Label))
	return t, http.StatusOK, nil
}

// removeTheme takes a pasted theme back out.
//
// A shipped theme cannot be removed this way: it is in the binary, and
// a row saying otherwise would be a removal that came back on the next
// restart. A household that wants a shipped theme gone can paste a
// replacement over its name instead.
func (s *Server) removeTheme(r *http.Request, id principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this is not ready yet")
	}
	name := r.PathValue("name")
	if !theme.ValidName(name) {
		return nil, http.StatusNotFound, fmt.Errorf("there is no theme called %q", name)
	}
	removed, err := s.DB.DeleteHouseholdTheme(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !removed {
		if _, shipped := theme.LookupBuiltin(name); shipped {
			return nil, http.StatusConflict, fmt.Errorf(
				"%q ships with Wildstone and cannot be removed — paste a theme over its name to replace it", name)
		}
		return nil, http.StatusNotFound, fmt.Errorf("there is no theme called %q", name)
	}
	s.recordThemeEvent("theme.removed", id, fmt.Sprintf("%s removed the theme %q", actingMember(id), name))
	return map[string]any{"removed": name}, http.StatusOK, nil
}

// themeCSS is the block a portal page is served with, or "" for a
// person who has chosen nothing.
func (s *Server) themeCSS(member string) string {
	if s.DB == nil {
		return ""
	}
	chosen, err := s.DB.GetPersonTheme(member)
	if err != nil || chosen == theme.Default {
		return ""
	}
	t, ok := s.lookupTheme(chosen)
	if !ok {
		return ""
	}
	return t.Palette().Tokens().CSS()
}

// -------------------------------------------------------------- portal

// homeAppearance answers the household portal for the member whose
// request it is. The portal identity holds no RBAC verb at all
// (auth_portal.go), so the member arrives as an argument rather than as
// something to be read off a principal.
func (s *Server) homeAppearance(_ http.ResponseWriter, _ *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.resolveAppearance(member), http.StatusOK, nil
}

// homeSetAppearance records a household member's own choice.
//
// There is no importing here on purpose. Adding a theme is an Owner's
// act on the Console; the family surface chooses from what exists.
func (s *Server) homeSetAppearance(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	var req struct {
		Theme string `json:"theme"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if code, err := s.setTheme(member, req.Theme); err != nil {
		return nil, code, err
	}
	return s.resolveAppearance(member), http.StatusOK, nil
}
