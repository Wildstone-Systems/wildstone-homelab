package api

// The pictures that ship with the product, one per shipped theme.
//
// WHY A DIRECTORY AND NOT THE BINARY. Migration 0061 declined to
// vendor wallpapers, and its reason was specific: a picture is between
// half a megabyte and a megabyte and a half, so one per shipped theme
// would add fifteen megabytes to a CONTROL-PLANE BINARY that gets
// downloaded onto home servers. That objection is about the binary,
// and it still stands — so the pictures travel the way the Console's
// static files and the package catalogue already travel, as their own
// release artifact unpacked into /usr/share/wildstone. The binary does
// not grow by a byte, and a theme arrives with its picture.
//
// WHAT 0061 SAID THAT THIS DOES NOT REOPEN. Nothing is fetched at run
// time. The control plane reads a directory the installer populated
// from a checksum-verified artifact, exactly as it reads -catalog-dir,
// and a household with no route off its own network is unaffected.
//
// A HOUSEHOLD'S OWN UPLOAD STILL WINS. The shipped picture is a
// default, not a floor: theme_backgrounds is consulted first
// everywhere, and a row there shadows the file. Clearing the upload
// falls back to the shipped picture rather than to nothing, which is
// what "remove my picture" should mean for a theme that came with one.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/theme"
)

// wallpaperTypes is every extension a shipped picture may have, and the
// media type it is served as.
//
// A closed set, like portal.assetTypes: the media type is decided from
// this table rather than sniffed, and a file whose extension is not
// here is not served whatever it contains. The directory is populated
// by the installer from a verified artifact, but "trusted input" is not
// a reason to let an arbitrary filename choose a Content-Type.
var wallpaperTypes = map[string]string{
	".webp": "image/webp",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
}

// shippedWallpaper is one picture that came with the product.
type shippedWallpaper struct {
	path      string
	mediaType string
	// version changes when the bytes do, and a surface hangs it on the
	// image URL. Modification time rather than a hash of the contents:
	// this is read on every load of every surface, and hashing twenty-two
	// files of a megabyte each to paint a page is not a trade worth
	// making. tar preserves mtime, so every host that installed the same
	// release computes the same version.
	version int64
}

// shippedWallpapers indexes the wallpaper directory by theme name.
//
// One ReadDir rather than a Stat per theme, for the reason themeCatalog
// makes one query rather than twenty-two: this runs on every page load.
// An unreadable or unset directory is no pictures, never an error — a
// deployment that installed no wallpaper artifact is a supported
// posture, not a fault, and it must not take the theme picker down.
func (s *Server) shippedWallpapers() map[string]shippedWallpaper {
	if s.WallpaperDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.WallpaperDir)
	if err != nil {
		return nil
	}
	out := make(map[string]shippedWallpaper, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		mt, ok := wallpaperTypes[ext]
		if !ok {
			continue
		}
		// The slug is the filename, and it goes through the same
		// validator a pasted theme's name does. A directory entry
		// cannot become a path segment or a URL otherwise.
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if !theme.ValidName(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[name] = shippedWallpaper{
			path:      filepath.Join(s.WallpaperDir, e.Name()),
			mediaType: mt,
			version:   info.ModTime().Unix(),
		}
	}
	return out
}

// shippedWallpaper resolves one theme's shipped picture.
//
// The name is re-validated and the result re-joined from the directory
// rather than taken from a caller: this is reached from a path segment,
// and "the index was built from trusted names" is one refactor away
// from being false.
func (s *Server) shippedWallpaperFor(name string) (shippedWallpaper, bool) {
	if s.WallpaperDir == "" || !theme.ValidName(name) {
		return shippedWallpaper{}, false
	}
	w, ok := s.shippedWallpapers()[name]
	return w, ok
}

// readShippedWallpaper returns the bytes and media type of a shipped
// picture.
func (s *Server) readShippedWallpaper(name string) ([]byte, string, int64, error) {
	w, ok := s.shippedWallpaperFor(name)
	if !ok {
		return nil, "", 0, fmt.Errorf("no shipped picture for %q", name)
	}
	b, err := os.ReadFile(w.path)
	if err != nil {
		return nil, "", 0, err
	}
	return b, w.mediaType, w.version, nil
}

// ShippedWallpaperCount is how many themes arrived with a picture, for
// the line the control plane logs at startup. An operator who pointed
// -wallpaper-dir at the wrong place should find out from the log, not
// from a dashboard that is quietly flat.
func (s *Server) ShippedWallpaperCount() int { return len(s.shippedWallpapers()) }
