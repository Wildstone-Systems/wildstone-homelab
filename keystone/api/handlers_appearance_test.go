package api

// Appearance, and the two claims it makes that are not visible by
// looking at a screen: that a theme is ONE person's and follows them
// between surfaces, and that pasting somebody else's theme file cannot
// put anything but colours into a page.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/theme"
)

// memberAppearance reads one member's appearance through the portal
// door, the way the dashboard does.
func (h *harness) memberAppearance(t *testing.T, user string) appearance {
	t.Helper()
	res := h.member(http.MethodGet, "/api/v1/home/appearance", user, "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("GET appearance for %s: %d %s", user, res.StatusCode, body)
	}
	var a appearance
	if err := json.NewDecoder(res.Body).Decode(&a); err != nil {
		t.Fatalf("decoding appearance: %v", err)
	}
	return a
}

// chooseTheme sets one member's theme through the portal door.
func (h *harness) chooseTheme(t *testing.T, user, name string) int {
	t.Helper()
	body, err := json.Marshal(map[string]string{"theme": name})
	if err != nil {
		t.Fatal(err)
	}
	res := h.member(http.MethodPost, "/api/v1/home/appearance", user, "homepage_users",
		bytes.NewReader(body), "application/json")
	defer res.Body.Close()
	return res.StatusCode
}

// consoleAppearance reads the same thing through the Console door, as
// the person the token acts for.
func (h *harness) consoleAppearance(t *testing.T, token string) appearance {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/appearance", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("GET /api/v1/appearance: %d %s", res.StatusCode, body)
	}
	var a appearance
	if err := json.NewDecoder(res.Body).Decode(&a); err != nil {
		t.Fatalf("decoding appearance: %v", err)
	}
	return a
}

// THE PROPERTY THE FEATURE IS FOR. One person, one choice, every
// surface: the dashboard on the kitchen tablet and the Console on a
// laptop read the same row through the same resolver, so they cannot
// disagree about what somebody picked.
func TestAThemeFollowsAPersonAcrossSurfaces(t *testing.T) {
	h := homeHarness(t)
	token := h.console(runtimedb.RoleAdmin, "mika")

	if code := h.chooseTheme(t, "mika", "gruvbox"); code != http.StatusOK {
		t.Fatalf("choosing a theme on the dashboard: %d", code)
	}

	onConsole := h.consoleAppearance(t, token)
	if onConsole.Theme != "gruvbox" {
		t.Errorf("the Console reports %q, want the theme chosen on the dashboard", onConsole.Theme)
	}
	if onConsole.Tokens["--color-bg"] == "" {
		t.Error("the Console was given no token block for a theme that is set")
	}

	onPortal := h.memberAppearance(t, "mika")
	if onPortal.Tokens["--color-bg"] != onConsole.Tokens["--color-bg"] {
		t.Errorf("the two surfaces derived different grounds: %q vs %q",
			onPortal.Tokens["--color-bg"], onConsole.Tokens["--color-bg"])
	}
}

// And it is one PERSON's, not the household's: the last person to
// change it does not decide for everybody.
func TestOneMembersThemeIsNotAnothers(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")

	if code := h.chooseTheme(t, "sandy", "vantablack"); code != http.StatusOK {
		t.Fatalf("sandy choosing: %d", code)
	}
	if code := h.chooseTheme(t, "maya", "catppuccin-latte"); code != http.StatusOK {
		t.Fatalf("maya choosing: %d", code)
	}

	if got := h.memberAppearance(t, "sandy").Theme; got != "vantablack" {
		t.Errorf("sandy has %q", got)
	}
	maya := h.memberAppearance(t, "maya")
	if maya.Theme != "catppuccin-latte" {
		t.Errorf("maya has %q", maya.Theme)
	}
	if maya.Dark {
		t.Error("a light theme was reported as a dark one, so the page would paint the wrong scheme first")
	}
}

// Choosing nothing is the resting state, and it renders as the design's
// own look rather than as a theme that happens to match it. Nothing
// about Wildstone's own appearance is reimplemented as a palette, so
// nothing about it can drift.
func TestChoosingNothingServesNoBlockAtAll(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")

	a := h.memberAppearance(t, "sandy")
	if a.Theme != "" {
		t.Errorf("a member who chose nothing has %q", a.Theme)
	}
	if len(a.Tokens) != 0 {
		t.Errorf("a member who chose nothing was served %d tokens", len(a.Tokens))
	}
	if len(a.Themes) < 20 {
		t.Errorf("the picker was offered %d themes", len(a.Themes))
	}

	res := h.member(http.MethodGet, "/home/", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	page, _ := io.ReadAll(res.Body)
	if bytes.Contains(page, []byte("--color-bg:")) {
		t.Error("a page for a member with no theme carries a token block")
	}
	if !bytes.Contains(page, []byte("<!--wildstone:theme-->")) {
		t.Error("the page lost its theme marker, so a theme could never be injected")
	}
}

// The page is served ALREADY themed rather than fetching its theme, so
// the wall tablet does not flash the default palette on every
// navigation while somebody is standing in front of it.
func TestAPortalPageIsServedAlreadyThemed(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	if code := h.chooseTheme(t, "sandy", "nord"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}

	res := h.member(http.MethodGet, "/home/", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	page, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	nord, ok := theme.LookupBuiltin("nord")
	if !ok {
		t.Fatal("nord is not in the binary")
	}
	want := nord.Palette().Tokens().Map()["--color-bg"]
	if !bytes.Contains(page, []byte("--color-bg: "+want)) {
		t.Errorf("the page does not carry nord's ground (%s)", want)
	}
	// After the design system's own stylesheet, or it would lose to it.
	style := bytes.Index(page, []byte("--color-bg: "+want))
	sheet := bytes.Index(page, []byte("nocturne.css"))
	if sheet < 0 || style < sheet {
		t.Error("the token block is not after the stylesheet it has to win against")
	}
	// And the block another member gets is their own.
	h.person("maya")
	other := h.member(http.MethodGet, "/home/", "maya", "homepage_users", nil, "")
	defer other.Body.Close()
	otherPage, _ := io.ReadAll(other.Body)
	if bytes.Contains(otherPage, []byte("--color-bg: "+want)) {
		t.Error("one member's theme was served to another")
	}
}

// A write names something off a list this server just sent, so a name
// that is not on it is a bug or a forgery and is refused.
func TestAnUnknownThemeIsRefusedOnWrite(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	for _, name := range []string{"no-such-theme", "../etc/passwd", "Gruvbox"} {
		if code := h.chooseTheme(t, "sandy", name); code == http.StatusOK {
			t.Errorf("choosing %q was allowed", name)
		}
	}
	// Clearing is not an unknown theme; it is the default.
	if code := h.chooseTheme(t, "sandy", ""); code != http.StatusOK {
		t.Errorf("clearing a theme: %d", code)
	}
}

// Removing a theme people are on must not break them. Their choice is
// left alone — so the theme coming back brings them back with it — and
// meanwhile they read on the design's own look.
func TestRemovingAThemeLeavesTheRoomReadable(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	token := h.console(runtimedb.RoleAdmin, "mika")

	const colors = "mode = \"dark\"\nbackground = \"#0b0b0b\"\nforeground = \"#f2f2f2\"\naccent = \"#88ccff\"\n"
	if code := h.importTheme(t, token, "our-house", colors); code != http.StatusOK {
		t.Fatalf("importing: %d", code)
	}
	if code := h.chooseTheme(t, "sandy", "our-house"); code != http.StatusOK {
		t.Fatalf("choosing the imported theme: %d", code)
	}

	if code := h.removeThemeAs(t, token, "our-house"); code != http.StatusOK {
		t.Fatalf("removing: %d", code)
	}
	after := h.memberAppearance(t, "sandy")
	if after.Theme != "our-house" {
		t.Errorf("the choice was rewritten to %q", after.Theme)
	}
	if len(after.Tokens) != 0 {
		t.Error("a removed theme still resolved to a token block")
	}

	// Pasting it back brings everybody who had it back with it.
	if code := h.importTheme(t, token, "our-house", colors); code != http.StatusOK {
		t.Fatalf("re-importing: %d", code)
	}
	if len(h.memberAppearance(t, "sandy").Tokens) == 0 {
		t.Error("pasting the theme back did not restore the member who had it")
	}
}

func (h *harness) importTheme(t *testing.T, token, name, colors string) int {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name, "colors": colors})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/appearance/themes", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func (h *harness) removeThemeAs(t *testing.T, token, name string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, h.srv.URL+"/api/v1/appearance/themes/"+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// Pasting a theme is the one path by which text somebody found on the
// internet reaches the control plane's database, so it is an Owner's
// act and the file is parsed before it is stored.
func TestPastingAThemeIsAnOwnersActAndIsParsedFirst(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")

	t.Run("a file that is not a palette is refused at the door", func(t *testing.T) {
		for _, bad := range []struct{ name, colors string }{
			{"broken", "background = \"not a colour\"\nforeground = \"#ffffff\"\n"},
			{"nothing", "accent = \"#ff0000\"\n"},
			{"markup", "background = \"#101010\"\nforeground = \"</style><script>alert(1)</script>\"\n"},
			{"Bad Name", "background = \"#101010\"\nforeground = \"#f0f0f0\"\n"},
		} {
			if code := h.importTheme(t, owner, bad.name, bad.colors); code != http.StatusBadRequest {
				t.Errorf("%s: importing gave %d, want 400", bad.name, code)
			}
		}
		// And nothing unparseable was stored on the way to being
		// refused.
		stored, err := h.s.DB.ListHouseholdThemes()
		if err != nil {
			t.Fatal(err)
		}
		if len(stored) != 0 {
			t.Errorf("%d refused theme(s) were stored anyway", len(stored))
		}
	})

	t.Run("a member cannot add one", func(t *testing.T) {
		member := h.console(runtimedb.RoleOperator, "sandy")
		code := h.importTheme(t, member, "theirs",
			"background = \"#101010\"\nforeground = \"#f0f0f0\"\n")
		if code == http.StatusOK {
			t.Error("a non-Owner added a theme for the whole household")
		}
	})

	t.Run("a theme in the binary cannot be removed", func(t *testing.T) {
		if code := h.removeThemeAs(t, owner, "nord"); code != http.StatusConflict {
			t.Errorf("removing a shipped theme gave %d, want 409", code)
		}
		if _, ok := theme.LookupBuiltin("nord"); !ok {
			t.Error("nord left the binary")
		}
	})
}

// A household may paste a theme over a shipped one's name to retune it
// — the same precedence a user theme has in Omarchy itself — and it
// replaces rather than duplicates.
func TestAPastedThemeOverridesAShippedOneOfTheSameName(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")
	h.person("sandy")

	const ours = "mode = \"dark\"\nbackground = \"#010203\"\nforeground = \"#f0f0f0\"\naccent = \"#88ccff\"\n"
	if code := h.importTheme(t, owner, "nord", ours); code != http.StatusOK {
		t.Fatalf("pasting over nord: %d", code)
	}
	if code := h.chooseTheme(t, "sandy", "nord"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}

	a := h.memberAppearance(t, "sandy")
	if got := a.Tokens["--color-bg"]; got != "#010203" {
		t.Errorf("the ground is %q, want the household's override", got)
	}
	seen := 0
	for _, th := range a.Themes {
		if th.Name == "nord" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("nord appears %d times in the picker", seen)
	}
}

// Nothing a household can paste reaches a page as anything but a CSS
// custom property value. The parser is the gate; this is the gate seen
// from outside, through the door an Owner actually uses.
func TestNothingPastedEscapesIntoAPage(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")
	h.person("sandy")

	// A palette that DOES parse, to prove the rendered page is a token
	// block and nothing else.
	if code := h.importTheme(t, owner, "ours",
		"mode = \"dark\"\nbackground = \"#010203\"\nforeground = \"#f0f0f0\"\n"); code != http.StatusOK {
		t.Fatalf("importing: %d", code)
	}
	if code := h.chooseTheme(t, "sandy", "ours"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}

	res := h.member(http.MethodGet, "/home/", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	page, _ := io.ReadAll(res.Body)
	block := string(page)
	start := strings.Index(block, "--color-bg: #010203")
	if start < 0 {
		t.Fatal("the theme did not reach the page")
	}
	// The injected element is one <style> holding one rule.
	open := strings.LastIndex(block[:start], "<style>")
	end := strings.Index(block[start:], "</style>")
	if open < 0 || end < 0 {
		t.Fatal("the token block is not inside a style element")
	}
	injected := block[open : start+end]
	for _, forbidden := range []string{"<script", "url(", "@import", "expression("} {
		if strings.Contains(injected, forbidden) {
			t.Errorf("the injected block contains %q", forbidden)
		}
	}
}

// The portal's appearance endpoints are on the Home surface, so they
// are reachable exactly when that surface is — not as a second door
// that stays open when the dashboard is not installed.
func TestAppearanceIsGatedOnTheHomeSurface(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	res := h.member(http.MethodGet, "/api/v1/home/appearance", "sandy", "some_other_group", nil, "")
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Error("a member outside the dashboard's group read the appearance endpoint")
	}
}

// ------------------------------------------------------- the wallpaper

// onePNG is the smallest thing portal.ImageType will accept as a
// picture: a real PNG signature. The bytes after it are never decoded
// by anything in this process — the media type comes from the
// signature, and the browser is what draws it.
var onePNG = append([]byte("\x89PNG\r\n\x1a\n"), []byte("wildstone test wallpaper")...)

func (h *harness) setBackground(t *testing.T, token, name string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut,
		h.srv.URL+"/api/v1/appearance/themes/"+name+"/background", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// A theme's picture belongs to the theme, so everybody who reads on
// that theme gets it and nobody else does — and it reaches the page as
// a background layer, not as the household's banner.
func TestAThemesPictureIsTheDashboardsBackground(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")
	h.person("sandy", "maya")

	if code := h.setBackground(t, owner, "nord", onePNG); code != http.StatusOK {
		t.Fatalf("setting a picture: %d", code)
	}
	if code := h.chooseTheme(t, "sandy", "nord"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}
	if code := h.chooseTheme(t, "maya", "gruvbox"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}

	res := h.member(http.MethodGet, "/home/", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	page, _ := io.ReadAll(res.Body)
	if !bytes.Contains(page, []byte("--wallpaper:")) {
		t.Error("the page carries no wallpaper for a theme that has one")
	}
	if !bytes.Contains(page, []byte("--wallpaper-veil:")) {
		t.Error("the picture is drawn with nothing in front of it")
	}
	// It is the page's GROUND, not the banner. The picture arrives as
	// --wallpaper, which nocturne.css draws as a layer of the page
	// background; the banner keeps pointing at the household's own
	// picture, untouched. A household without a picture has a plain banner.
	if !bytes.Contains(page, []byte(`--wallpaper: url("/api/v1/home/appearance/themes/nord/background?v=`)) {
		t.Error("the wallpaper does not point at the theme's picture")
	}
	if !bytes.Contains(page, []byte("/api/v1/home/banner?v=")) {
		t.Error("the household banner no longer uses its own uploaded picture")
	}

	// Somebody on a theme without a picture gets no wallpaper at all.
	other := h.member(http.MethodGet, "/home/", "maya", "homepage_users", nil, "")
	defer other.Body.Close()
	otherPage, _ := io.ReadAll(other.Body)
	if bytes.Contains(otherPage, []byte("--wallpaper:")) {
		t.Error("a theme with no picture served a wallpaper")
	}
	if bytes.Contains(otherPage, []byte("--wallpaper-veil:")) {
		t.Error("a theme with no picture served a veil, which would dim its ground for nothing")
	}
}

// The bytes come back on the household surface, where the picture is
// actually drawn, and the Console has no route for them at all.
func TestTheWallpaperIsServedToTheDashboardAndNotTheConsole(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")
	h.person("sandy")
	if code := h.setBackground(t, owner, "nord", onePNG); code != http.StatusOK {
		t.Fatalf("setting a picture: %d", code)
	}

	res := h.member(http.MethodGet, "/api/v1/home/appearance/themes/nord/background",
		"sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("reading the picture: %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want the type decided from the bytes", got)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Error("the picture is served without nosniff")
	}
	got, _ := io.ReadAll(res.Body)
	if !bytes.Equal(got, onePNG) {
		t.Error("the bytes that came back are not the bytes that went in")
	}

	// A theme with no picture is a 404, and the page falls back to its
	// flat ground rather than showing an error.
	missing := h.member(http.MethodGet, "/api/v1/home/appearance/themes/gruvbox/background",
		"sandy", "homepage_users", nil, "")
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("a theme with no picture gave %d, want 404", missing.StatusCode)
	}
}

// The same rules the household banner is held to, because this is the
// other place a household surface accepts an image.
func TestAThemePictureIsCheckedBeforeItIsStored(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")

	t.Run("what is not a picture is refused, whatever it claims", func(t *testing.T) {
		// An HTML file with an image's name is a stored XSS, and the
		// only reliable answer is not to believe the upload.
		html := []byte("<html><script>alert(1)</script></html>")
		if code := h.setBackground(t, owner, "nord", html); code != http.StatusUnsupportedMediaType {
			t.Errorf("an HTML upload gave %d, want 415", code)
		}
		if _, err := h.s.DB.GetThemeBackground("nord"); err == nil {
			t.Error("the refused upload was stored anyway")
		}
	})

	t.Run("a picture larger than the bound is refused", func(t *testing.T) {
		big := append(append([]byte{}, onePNG...),
			make([]byte, runtimedb.MaxThemeBackgroundBytes)...)
		if code := h.setBackground(t, owner, "nord", big); code != http.StatusRequestEntityTooLarge {
			t.Errorf("an oversized picture gave %d, want 413", code)
		}
	})

	t.Run("a theme nobody has heard of takes no picture", func(t *testing.T) {
		if code := h.setBackground(t, owner, "no-such-theme", onePNG); code != http.StatusNotFound {
			t.Errorf("setting a picture on an unknown theme gave %d, want 404", code)
		}
	})

	t.Run("a member cannot change what the household sees", func(t *testing.T) {
		member := h.console(runtimedb.RoleOperator, "sandy")
		if code := h.setBackground(t, member, "nord", onePNG); code == http.StatusOK {
			t.Error("a non-Owner set the picture behind a theme")
		}
	})
}

// Removing a pasted theme takes its picture with it. Unlike a person's
// choice — a few bytes of text that re-attach by themselves — an
// orphaned wallpaper is megabytes nothing lists and nothing can remove.
func TestRemovingAThemeTakesItsPictureWithIt(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")

	const colors = "mode = \"dark\"\nbackground = \"#0b0b0b\"\nforeground = \"#f2f2f2\"\n"
	if code := h.importTheme(t, owner, "our-house", colors); code != http.StatusOK {
		t.Fatalf("importing: %d", code)
	}
	if code := h.setBackground(t, owner, "our-house", onePNG); code != http.StatusOK {
		t.Fatalf("setting a picture: %d", code)
	}
	if code := h.removeThemeAs(t, owner, "our-house"); code != http.StatusOK {
		t.Fatalf("removing: %d", code)
	}
	if _, err := h.s.DB.GetThemeBackground("our-house"); err == nil {
		t.Error("the theme is gone and its picture is still in the database")
	}
}

// A picture attaches to a theme that ships in the binary as readily as
// to a pasted one — otherwise putting a wallpaper behind Nord would
// mean re-pasting Nord's palette first, which is a silly thing to make
// somebody do.
func TestAShippedThemeTakesAPictureWithoutBeingRepasted(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")
	h.person("sandy")

	if code := h.setBackground(t, owner, "gruvbox", onePNG); code != http.StatusOK {
		t.Fatalf("setting a picture on a shipped theme: %d", code)
	}
	stored, err := h.s.DB.ListHouseholdThemes()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Errorf("adding a picture invented %d pasted theme(s)", len(stored))
	}

	found := false
	for _, th := range h.memberAppearance(t, "sandy").Themes {
		if th.Name == "gruvbox" {
			found = th.Background
		}
	}
	if !found {
		t.Error("the picker does not know the shipped theme has a picture")
	}
}
