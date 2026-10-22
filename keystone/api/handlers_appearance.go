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
	"io"
	"net/http"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/portal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/theme"
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
	// Background says the resolved theme brings a picture, and which
	// version of it. The surface turns that into a URL of its own —
	// the two doors serve the same bytes at different addresses — and
	// the block's --wallpaper-veil is already sized for it.
	Background        bool  `json:"background,omitempty"`
	BackgroundVersion int64 `json:"backgroundVersion,omitempty"`
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
	// Pictures attach by NAME, to shipped themes as readily as to
	// pasted ones — otherwise putting a wallpaper behind Nord would
	// mean re-pasting Nord's palette first. One query for the whole
	// catalogue, and deliberately without the bytes: this list is read
	// on every load of every surface.
	//
	// A picture that SHIPPED is attached the same way, and is looked at
	// second: theme_backgrounds is the household's own choice and
	// shadows the default. Clearing an upload therefore falls back to
	// the shipped picture rather than to nothing, which is what
	// "remove my picture" should mean for a theme that came with one.
	shipped := s.shippedWallpapers()
	if s.DB != nil {
		if versions, err := s.DB.ThemeBackgroundVersions(); err == nil {
			for i := range out {
				if v, ok := versions[out[i].Name]; ok {
					out[i] = out[i].WithBackground(v)
					continue
				}
				if w, ok := shipped[out[i].Name]; ok {
					out[i] = out[i].WithBackground(w.version)
				}
			}
			return out
		}
	}
	// No database, or the version query failed: the shipped pictures
	// are still on disk and a theme should still arrive with one.
	for i := range out {
		if w, ok := shipped[out[i].Name]; ok {
			out[i] = out[i].WithBackground(w.version)
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
		a.Tokens = t.Tokens().Map()
		a.Dark = t.Dark
		a.Background = t.Background
		a.BackgroundVersion = t.BackgroundVersion
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
	tokens := t.Tokens()
	if t.Background {
		// The URL is added HERE rather than in the derivation because
		// it is the one token that differs between the two doors: the
		// Console and the portal serve the same picture at different
		// addresses. The version is a decimal integer from the
		// database and the name has been through theme.ValidName, so
		// there is nothing in this string a household chose the shape
		// of.
		tokens = append(tokens, theme.Token{
			Name: "--wallpaper",
			Value: fmt.Sprintf("url(%q)",
				fmt.Sprintf("/api/v1/home/appearance/themes/%s/background?v=%d", t.Name, t.BackgroundVersion)),
		})
	}
	return tokens.CSS()
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

// ------------------------------------------------------- the picture

// THE ONE PLACE A THEME ACCEPTS BYTES, and it is careful for exactly
// the reasons handlers_portal_banner.go is: what comes in is an image
// somebody found on the internet, and what goes out is served from the
// same origin as the pages that hold everybody's session. So the media
// type is decided from the BYTES and never from what the upload
// claimed, and anything that is not a format a browser will draw is
// refused rather than stored and served back as something the browser
// has to guess about.

// themeBackgroundSet stores the picture for a theme.
//
// The body IS the image, as the banner's is: a multipart request
// carries the caller's own description of what it sent, and here that
// description decides nothing anyway.
//
// A picture may attach to a SHIPPED theme as readily as to a pasted
// one. It is keyed on the name, in its own table, so putting a
// wallpaper behind Nord does not mean re-pasting Nord's palette first.
func (s *Server) themeBackgroundSet(r *http.Request, id principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this is not ready yet")
	}
	name := r.PathValue("name")
	if !theme.ValidName(name) {
		return nil, http.StatusNotFound, fmt.Errorf("there is no theme called %q", name)
	}
	if _, ok := s.lookupTheme(name); !ok {
		return nil, http.StatusNotFound, fmt.Errorf("there is no theme called %q", name)
	}
	if r.ContentLength > runtimedb.MaxThemeBackgroundBytes {
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that picture is too big — the limit is %d MB", runtimedb.MaxThemeBackgroundBytes>>20)
	}
	defer r.Body.Close()
	// One byte over the limit is still read, so a body that lied about
	// its length is caught by the same bound rather than by trusting
	// Content-Length twice.
	image, err := io.ReadAll(io.LimitReader(r.Body, runtimedb.MaxThemeBackgroundBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("that picture didn't arrive in one piece")
	}
	if len(image) > runtimedb.MaxThemeBackgroundBytes {
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that picture is too big — the limit is %d MB", runtimedb.MaxThemeBackgroundBytes>>20)
	}
	mediaType, ok := portal.ImageType(image)
	if !ok {
		return nil, http.StatusUnsupportedMediaType,
			fmt.Errorf("that file isn't a picture a browser will draw — PNG, JPEG, WebP, GIF or SVG-free formats only")
	}
	if err := s.DB.SetThemeBackground(name, image, mediaType, actingMember(id)); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.recordThemeEvent("theme.background", id,
		fmt.Sprintf("%s set the picture behind the theme %q", actingMember(id), name))
	t, _ := s.lookupTheme(name)
	return t, http.StatusOK, nil
}

// themeBackgroundClear takes the picture away and leaves the palette.
func (s *Server) themeBackgroundClear(r *http.Request, id principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this is not ready yet")
	}
	name := r.PathValue("name")
	if !theme.ValidName(name) {
		return nil, http.StatusNotFound, fmt.Errorf("there is no theme called %q", name)
	}
	removed, err := s.DB.DeleteThemeBackground(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !removed {
		// A theme that SHIPPED with a picture has one without anybody
		// having uploaded one, so "no picture" would be a lie here.
		// Say which case this is: there is nothing of the household's
		// to remove, and the default is not something to delete.
		if _, ok := s.shippedWallpaperFor(name); ok {
			return nil, http.StatusConflict,
				fmt.Errorf("%q is showing the picture it shipped with, which nobody uploaded and nothing here removes — upload your own to replace it", name)
		}
		return nil, http.StatusNotFound, fmt.Errorf("%q has no picture", name)
	}
	s.recordThemeEvent("theme.background", id,
		fmt.Sprintf("%s removed the picture behind the theme %q", actingMember(id), name))
	// Cleared back to the SHIPPED picture where there is one, rather
	// than to a flat ground. "Remove my picture" means the theme looks
	// the way it did out of the box.
	if _, ok := s.shippedWallpaperFor(name); ok {
		return map[string]any{"cleared": name, "revertedToShipped": true}, http.StatusOK, nil
	}
	return map[string]any{"cleared": name}, http.StatusOK, nil
}

// writeThemeBackground serves one theme's picture.
//
// ON THE PORTAL ONLY. The Console does not draw wallpapers — it is the
// screen somebody opens when the house is broken, and ADR-W027's dense
// operator layout is drawn on a flat ground on purpose — so it has no
// reason to fetch a megabyte of somebody's art. Its picker says a theme
// brings a picture; the dashboard is where the picture is.
//
// NOT SCOPED TO THE CALLER'S OWN THEME, deliberately: every theme in
// the catalogue is one this person may choose, so a picker showing what
// each one looks like is showing them nothing they could not see by
// picking it. Scoping it would buy no privacy and would cost the
// preview.
func (s *Server) writeThemeBackground(w http.ResponseWriter, r *http.Request) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusNotFound, fmt.Errorf("no picture")
	}
	name := r.PathValue("name")
	if !theme.ValidName(name) {
		return nil, http.StatusNotFound, fmt.Errorf("no picture")
	}
	image, mediaType := []byte(nil), ""
	if b, err := s.DB.GetThemeBackground(name); err == nil {
		image, mediaType = b.Image, b.MediaType
	} else if shipped, mt, _, serr := s.readShippedWallpaper(name); serr == nil {
		// The household uploaded nothing for this theme, but the theme
		// came with a picture. Second, not first: an upload shadows the
		// default (handlers_appearance.go says so where the catalogue
		// is built, and the two must not disagree).
		image, mediaType = shipped, mt
	} else {
		// Absent is a 404 and the page falls back to the flat ground,
		// which is a theme without a picture rather than an error.
		return nil, http.StatusNotFound, fmt.Errorf("no picture")
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Cacheable, unlike the household banner: a theme's wallpaper is
	// art the whole household can already see in the picker, not a
	// photograph of somebody's home, and the version in the URL changes
	// when the bytes do. A megabyte re-fetched on every page load would
	// be the most expensive thing on the dashboard.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image)
	return nil, http.StatusOK, nil
}

// homeThemeBackground serves the same bytes to the household portal,
// which is where a wallpaper is actually drawn.
func (s *Server) homeThemeBackground(w http.ResponseWriter, r *http.Request, _ string, _ []resource.Resource) (any, int, error) {
	return s.writeThemeBackground(w, r)
}
