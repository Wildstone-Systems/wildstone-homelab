package api

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// shipWallpapers populates a wallpaper directory and points the server
// at it, the way the installer points -wallpaper-dir at
// /usr/share/wildstone/wallpapers.
func shipWallpapers(t *testing.T, h *harness, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	h.s.WallpaperDir = dir
	return dir
}

// clearBackground removes a household's own upload, the way the
// Console's "remove picture" does.
func clearBackground(t *testing.T, h *harness, token, name string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete,
		h.srv.URL+"/api/v1/appearance/themes/"+name+"/background", nil)
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

// A theme that shipped with a picture has one before anybody uploads
// anything. This is the whole point of the wallpapers artifact: a
// household should not have to go and find art for a theme whose
// authors already drew some.
func TestAThemeArrivesWithThePictureItShippedWith(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy", "maya")
	shipWallpapers(t, h, map[string][]byte{"nord.png": onePNG})

	if code := h.chooseTheme(t, "sandy", "nord"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}
	if code := h.chooseTheme(t, "maya", "gruvbox"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}

	res := h.member(http.MethodGet, "/home/", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	page, _ := io.ReadAll(res.Body)
	if !bytes.Contains(page, []byte(`--wallpaper: url("/api/v1/home/appearance/themes/nord/background?v=`)) {
		t.Error("a theme that shipped with a picture drew no wallpaper")
	}
	if !bytes.Contains(page, []byte("--wallpaper-veil:")) {
		t.Error("the shipped picture is drawn with nothing in front of it")
	}

	// And the bytes are actually served.
	img := h.member(http.MethodGet, "/api/v1/home/appearance/themes/nord/background", "sandy", "homepage_users", nil, "")
	defer img.Body.Close()
	if img.StatusCode != http.StatusOK {
		t.Fatalf("serving the shipped picture: %d", img.StatusCode)
	}
	got, _ := io.ReadAll(img.Body)
	if !bytes.Equal(got, onePNG) {
		t.Error("the bytes served are not the shipped picture's")
	}
	if ct := img.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("media type came from somewhere other than the extension: %q", ct)
	}

	// A theme with no file in the directory is still flat, so shipping
	// pictures did not quietly give every theme one.
	other := h.member(http.MethodGet, "/home/", "maya", "homepage_users", nil, "")
	defer other.Body.Close()
	otherPage, _ := io.ReadAll(other.Body)
	if bytes.Contains(otherPage, []byte("--wallpaper:")) {
		t.Error("a theme with no shipped picture served a wallpaper")
	}
}

// The household's own upload wins. The shipped picture is a default,
// and a default that could not be replaced would be worse than none.
func TestAnUploadShadowsTheShippedPicture(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")
	h.person("sandy")
	shipWallpapers(t, h, map[string][]byte{"nord.png": onePNG})

	mine := append([]byte(nil), onePNG...)
	mine = append(mine, "  -- not the shipped one"...)
	if code := h.setBackground(t, owner, "nord", mine); code != http.StatusOK {
		t.Fatalf("uploading: %d", code)
	}
	if code := h.chooseTheme(t, "sandy", "nord"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}

	img := h.member(http.MethodGet, "/api/v1/home/appearance/themes/nord/background", "sandy", "homepage_users", nil, "")
	defer img.Body.Close()
	got, _ := io.ReadAll(img.Body)
	if bytes.Equal(got, onePNG) {
		t.Error("the shipped picture was served over the household's own upload")
	}
	if !bytes.Equal(got, mine) {
		t.Error("the household's own upload was not served")
	}
}

// Clearing an upload falls back to the shipped picture rather than to a
// flat ground: "remove my picture" means the theme looks the way it did
// out of the box, not that the theme loses the one it came with.
func TestClearingAnUploadFallsBackToTheShippedPicture(t *testing.T) {
	h := homeHarness(t)
	owner := h.console(runtimedb.RoleAdmin, "mika")
	h.person("sandy")
	shipWallpapers(t, h, map[string][]byte{"nord.png": onePNG})

	mine := append([]byte(nil), onePNG...)
	mine = append(mine, "  -- mine"...)
	if code := h.setBackground(t, owner, "nord", mine); code != http.StatusOK {
		t.Fatalf("uploading: %d", code)
	}
	if code := clearBackground(t, h, owner, "nord"); code != http.StatusOK {
		t.Fatalf("clearing: %d", code)
	}
	if code := h.chooseTheme(t, "sandy", "nord"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}

	img := h.member(http.MethodGet, "/api/v1/home/appearance/themes/nord/background", "sandy", "homepage_users", nil, "")
	defer img.Body.Close()
	if img.StatusCode != http.StatusOK {
		t.Fatalf("after clearing, the shipped picture is gone too: %d", img.StatusCode)
	}
	got, _ := io.ReadAll(img.Body)
	if !bytes.Equal(got, onePNG) {
		t.Error("clearing an upload did not fall back to the shipped picture")
	}
}

// Nothing in the directory becomes a picture just by being there. The
// installer fills this from a verified artifact, but the media type is
// still decided by a closed table and the slug still goes through
// ValidName — a stray file is skipped, not served.
func TestOnlyRealPictureFilesAreShipped(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	shipWallpapers(t, h, map[string][]byte{
		"NOTICE.md":      []byte("# provenance, not a wallpaper"),
		"nord.txt":       []byte("not a picture"),
		"not a slug.png": onePNG,
		"gruvbox.png":    onePNG,
	})

	if n := h.s.ShippedWallpaperCount(); n != 1 {
		t.Errorf("counted %d shipped pictures, want only gruvbox.png", n)
	}
	if code := h.chooseTheme(t, "sandy", "nord"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}
	res := h.member(http.MethodGet, "/api/v1/home/appearance/themes/nord/background", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a .txt beside the pictures was served as one: %d", res.StatusCode)
	}
}

// With no directory configured the themes are exactly what they were
// before wallpapers shipped. A deployment that installed no artifact is
// a supported posture, not a broken one.
func TestNoWallpaperDirLeavesEveryThemeFlat(t *testing.T) {
	h := homeHarness(t)
	h.person("sandy")
	if h.s.WallpaperDir != "" {
		t.Fatalf("the harness configured a wallpaper dir: %q", h.s.WallpaperDir)
	}
	if n := h.s.ShippedWallpaperCount(); n != 0 {
		t.Errorf("counted %d shipped pictures with no directory", n)
	}
	if code := h.chooseTheme(t, "sandy", "nord"); code != http.StatusOK {
		t.Fatalf("choosing: %d", code)
	}
	res := h.member(http.MethodGet, "/home/", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	page, _ := io.ReadAll(res.Body)
	if bytes.Contains(page, []byte("--wallpaper:")) {
		t.Error("a deployment with no wallpapers drew one anyway")
	}
}
