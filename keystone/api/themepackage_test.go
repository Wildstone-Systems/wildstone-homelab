package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/theme"
)

// nordColors is a real palette in the shape colors.toml has.
// oneJPEG is a minimal JPEG: the sniffer reads magic bytes, so a PNG
// renamed .jpg would be served as the PNG it is and prove nothing.
var oneJPEG = []byte{
	0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01,
	0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9,
}

const nordColors = `mode = "dark"
background = "#2e3440"
foreground = "#d8dee9"
accent = "#88c0d0"
`

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A theme downloaded from its repository imports as it comes: nested
// under <repo>-<branch>, with the files that configure programs
// Wildstone does not run still in it.
func TestAThemePackageImportsAsDownloaded(t *testing.T) {
	pkg, err := readThemePackage(zipOf(t, map[string][]byte{
		"nord-master/colors.toml":              []byte(nordColors),
		"nord-master/neovim.lua":               []byte("-- not ours"),
		"nord-master/vscode.json":              []byte("{}"),
		"nord-master/backgrounds/1-city.png":   onePNG,
		"nord-master/backgrounds/omarchy.webp": []byte("logo wall"),
	}), "")
	if err != nil {
		t.Fatalf("importing: %v", err)
	}
	if pkg.name != "nord" {
		t.Errorf("name = %q, want nord — the branch suffix should come off", pkg.name)
	}
	if !strings.Contains(pkg.colors, "#2e3440") {
		t.Error("the palette did not come out of the archive")
	}
	if pkg.background != "1-city.png" {
		t.Errorf("picked %q, want 1-city.png — omarchy.webp is the logo wall", pkg.background)
	}
	if pkg.mediaType != "image/png" {
		t.Errorf("media type %q, want image/png sniffed from the bytes", pkg.mediaType)
	}
}

// An explicit name beats the directory's.
func TestAThemePackageTakesAGivenName(t *testing.T) {
	pkg, err := readThemePackage(zipOf(t, map[string][]byte{
		"whatever/colors.toml": []byte(nordColors),
	}), "midnight")
	if err != nil {
		t.Fatalf("importing: %v", err)
	}
	if pkg.name != "midnight" {
		t.Errorf("name = %q, want the given midnight", pkg.name)
	}
}

// A palette is the theme. An archive with no picture still imports.
func TestAThemePackageWithNoPictureStillImports(t *testing.T) {
	pkg, err := readThemePackage(zipOf(t, map[string][]byte{
		"nord/colors.toml": []byte(nordColors),
	}), "")
	if err != nil {
		t.Fatalf("importing: %v", err)
	}
	if len(pkg.image) != 0 {
		t.Error("invented a picture")
	}
}

// And an archive that is not a theme says so rather than half-importing.
func TestAnArchiveWithNoPaletteIsRefused(t *testing.T) {
	_, err := readThemePackage(zipOf(t, map[string][]byte{
		"stuff/readme.md": []byte("# not a theme"),
	}), "")
	if err == nil {
		t.Fatal("an archive with no colors.toml was accepted")
	}
	if !strings.Contains(err.Error(), "colors.toml") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

func TestSomethingThatIsNotAZipIsRefused(t *testing.T) {
	if _, err := readThemePackage([]byte("PK not really"), ""); err == nil {
		t.Fatal("arbitrary bytes were accepted as an archive")
	}
}

// A ../ in an entry name reaches nothing, because no name is ever
// joined to a path. This pins the property rather than a defence:
// the entry is simply not a colors.toml or a background, so it is
// ignored like every other file in the archive.
func TestAnEntryThatClimbsOutIsJustIgnored(t *testing.T) {
	pkg, err := readThemePackage(zipOf(t, map[string][]byte{
		"nord/colors.toml":             []byte(nordColors),
		"../../../etc/wildstone/pwned": []byte("nope"),
		"nord/../../escape.toml":       []byte("nope"),
	}), "")
	if err != nil {
		t.Fatalf("importing: %v", err)
	}
	if pkg.name != "nord" {
		t.Errorf("name = %q, want nord", pkg.name)
	}
	if !strings.Contains(pkg.colors, "#2e3440") {
		t.Error("the wrong colors.toml won")
	}
}

// A theme is a handful of files. Fifty thousand is not a theme, and
// finding that out by walking them all is the thing worth refusing.
func TestAnArchiveOfTooManyFilesIsRefused(t *testing.T) {
	many := map[string][]byte{"nord/colors.toml": []byte(nordColors)}
	for i := 0; i < maxThemePackageEntries+10; i++ {
		many["nord/filler/f"+strconv.Itoa(i)] = []byte("x")
	}
	if _, err := readThemePackage(zipOf(t, many), ""); err == nil {
		t.Fatal("an archive with too many entries was accepted")
	}
}

// The shallowest colors.toml wins, so a zip holding a theme inside
// another directory does not pick a nested one by iteration order.
func TestTheShallowestPaletteWins(t *testing.T) {
	pkg, err := readThemePackage(zipOf(t, map[string][]byte{
		"nord/colors.toml":              []byte(nordColors),
		"nord/vendor/other/colors.toml": []byte("mode = \"dark\"\nbackground = \"#000000\"\nforeground = \"#ffffff\"\naccent = \"#ff0000\"\n"),
	}), "")
	if err != nil {
		t.Fatalf("importing: %v", err)
	}
	if !strings.Contains(pkg.colors, "#2e3440") {
		t.Error("a nested colors.toml won over the theme's own")
	}
}

// End to end through the route the Console calls: one upload puts the
// palette and the picture in, and the theme is then choosable.
func TestUploadingAThemePackageAddsThemeAndPicture(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")
	h.person("sandy")

	body := zipOf(t, map[string][]byte{
		"nord-master/colors.toml":            []byte(nordColors),
		"nord-master/neovim.lua":             []byte("-- not ours"),
		"nord-master/backgrounds/1-city.png": onePNG,
	})
	req, err := http.NewRequest(http.MethodPost,
		h.srv.URL+"/api/v1/appearance/themes/package", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+owner)
	req.Header.Set("Content-Type", "application/zip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("uploading a theme package: status %d", res.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["theme"] != "nord" {
		t.Errorf("imported as %v, want nord", out["theme"])
	}
	if out["pictureProblem"] != nil {
		t.Errorf("the picture did not go in: %v", out["pictureProblem"])
	}

	// It is choosable, and it carries its picture.
	if code := h.chooseTheme(t, "sandy", "nord"); code != http.StatusOK {
		t.Fatalf("choosing the imported theme: %d", code)
	}
	img := h.member(http.MethodGet, "/api/v1/home/appearance/themes/nord/background",
		"sandy", "homepage_users", nil, "")
	defer img.Body.Close()
	if img.StatusCode != http.StatusOK {
		t.Fatalf("the imported theme's picture is not served: %d", img.StatusCode)
	}
	got, _ := io.ReadAll(img.Body)
	if !bytes.Equal(got, onePNG) {
		t.Error("the bytes served are not the ones in the archive")
	}
}

// Uploading is a manage act, like every other change to what the household
// can choose from.
func TestAMemberCannotUploadAThemePackage(t *testing.T) {
	h := homeHarness(t)
	reader := h.console(runtimedb.RoleOperator, "sandy")
	req, err := http.NewRequest(http.MethodPost,
		h.srv.URL+"/api/v1/appearance/themes/package",
		bytes.NewReader(zipOf(t, map[string][]byte{"nord/colors.toml": []byte(nordColors)})))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+reader)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("an operator uploading a theme: status %d, want 403", res.StatusCode)
	}
}

// A theme zipped by hand, which is how one actually arrives.
//
// The parser was written against a GitHub download — everything nested
// under <repo>-<branch>/ with the pictures in backgrounds/ — and a real
// upload was neither. colors.toml and a .jpg at the root imported the
// palette and silently dropped the wallpaper, which is the half
// somebody notices last. The palette here is a real Omarchy one, ASCII
// art banner and all the keys this product has no use for included.
func TestAHandZippedThemeImportsItsPicture(t *testing.T) {
	colors, err := os.ReadFile("testdata/omarchy-colors.toml")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := readThemePackage(zipOf(t, map[string][]byte{
		"colors.toml":       colors,
		"1-rain-runner.jpg": oneJPEG,
	}), "rain-runner")
	if err != nil {
		t.Fatalf("a flat archive will not import: %v", err)
	}
	if pkg.background != "1-rain-runner.jpg" {
		t.Errorf("picture = %q, want the loose jpg beside the palette", pkg.background)
	}
	if pkg.mediaType != "image/jpeg" {
		t.Errorf("media type %q, want image/jpeg sniffed from the bytes", pkg.mediaType)
	}
	// And the real palette parses, which the paste box already proved
	// but this path had never been shown.
	if _, err := theme.ParsePalette(pkg.colors); err != nil {
		t.Errorf("a real Omarchy palette does not parse: %v", err)
	}
}

// A backgrounds/ picture still wins over a loose one, so opening the
// search up did not make a theme's preview its wallpaper.
func TestBackgroundsFolderStillWinsOverALoosePicture(t *testing.T) {
	pkg, err := readThemePackage(zipOf(t, map[string][]byte{
		"nord/colors.toml":              []byte(nordColors),
		"nord/preview.png":              onePNG,
		"nord/backgrounds/1-proper.png": onePNG,
	}), "")
	if err != nil {
		t.Fatalf("importing: %v", err)
	}
	if pkg.background != "1-proper.png" {
		t.Errorf("picked %q, want the backgrounds/ picture", pkg.background)
	}
}

// And a preview at the root is not mistaken for the wallpaper when
// there is no backgrounds/ folder at all.
func TestAPreviewIsNotTheWallpaper(t *testing.T) {
	pkg, err := readThemePackage(zipOf(t, map[string][]byte{
		"nord/colors.toml": []byte(nordColors),
		"nord/preview.png": onePNG,
		"nord/wall.png":    onePNG,
	}), "")
	if err != nil {
		t.Fatalf("importing: %v", err)
	}
	if pkg.background != "wall.png" {
		t.Errorf("picked %q, want wall.png — preview.png is a picture OF the theme", pkg.background)
	}
}
