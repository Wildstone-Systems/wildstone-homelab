package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// nordColors is a real palette in the shape colors.toml has.
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
