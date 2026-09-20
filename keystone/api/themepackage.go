package api

// A theme arriving as the directory its authors publish.
//
// THE PACKAGE FORMAT IS SOMEBODY ELSE'S, on purpose. An Omarchy theme
// is a directory — colors.toml, a backgrounds/ folder, and a handful of
// files configuring programs Wildstone does not run — and a household
// gets one by pressing Download ZIP on its repository. That zip is the
// package. Inventing a Wildstone theme format would mean asking people
// to repackage something they already have into something only this
// product reads.
//
// It also folds two steps into one. Pasting a colors.toml and then
// uploading a picture are the same act performed twice, and the second
// half was the one people skipped.
//
// NOTHING IS EXTRACTED TO DISK. Entries are read by name into memory,
// the palette goes through the parser the paste box already uses and
// the picture into the blob the upload already writes. There is no
// path derived from the archive, so there is no zip-slip to get wrong:
// the class of bug is absent rather than defended against.
//
// WHAT IS READ is a closed set — one colors.toml and one background.
// Everything else is IGNORED rather than refused: a theme carrying
// neovim.lua and a vscode.json is a normal theme, not a malformed one,
// and refusing it would make the common case an error.

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/theme"
)

const (
	// maxThemePackageBytes bounds the upload. A theme's palette is
	// under a kilobyte and its backgrounds directory is the rest; the
	// largest upstream set is about ten megabytes, and this leaves room
	// without accepting an arbitrary archive.
	maxThemePackageBytes = 64 << 20
	// maxThemePackageEntries bounds how many members are even looked
	// at. An Omarchy theme has a dozen; a zip with fifty thousand
	// entries is not a theme, and finding that out by walking them all
	// is the thing worth refusing.
	maxThemePackageEntries = 512
	// maxThemePaletteBytes bounds the one text file read out of the
	// archive. Declared uncompressed sizes are a claim, so the read is
	// limited too — a kilobyte of colours must not be able to announce
	// itself as a gigabyte and be believed.
	maxThemePaletteBytes = 256 << 10
)

// themePackage is what one archive yielded.
type themePackage struct {
	name       string
	colors     string
	image      []byte
	mediaType  string
	background string
}

// importThemePackage takes a zipped theme directory.
func (s *Server) importThemePackage(r *http.Request, id principal.Identity) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this is not ready yet")
	}
	if r.ContentLength > maxThemePackageBytes {
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that file is too big — the limit is %d MB", maxThemePackageBytes>>20)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxThemePackageBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("reading the upload: %w", err)
	}
	if len(body) > maxThemePackageBytes {
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that file is too big — the limit is %d MB", maxThemePackageBytes>>20)
	}
	pkg, err := readThemePackage(body, r.URL.Query().Get("name"))
	if err != nil {
		return nil, http.StatusBadRequest, err
	}

	// The palette through the same parser the paste box uses, so an
	// archive cannot carry anything a pasted file could not.
	palette, err := theme.ParsePalette(pkg.colors)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("%s: %w", pkg.background, err)
	}
	t, err := theme.New(pkg.name, palette, true)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.DB.SaveHouseholdTheme(pkg.name, pkg.colors, actingMember(id)); err != nil {
		return nil, http.StatusInternalServerError, err
	}

	out := map[string]any{"theme": t.Name, "label": t.Label}
	if len(pkg.image) > 0 {
		if err := s.DB.SetThemeBackground(pkg.name, pkg.image, pkg.mediaType, actingMember(id)); err != nil {
			// The palette is in and usable. Saying so beats failing the
			// whole import over the half that is decoration.
			out["pictureProblem"] = "The theme was added, but its picture could not be saved: " + err.Error()
		} else {
			out["picture"] = pkg.background
		}
	} else {
		out["pictureProblem"] = "The theme was added. It carried no picture this surface could read."
	}
	s.recordThemeEvent("theme.imported", id,
		fmt.Sprintf("%s added the theme %q from a package", actingMember(id), t.Label))
	return out, http.StatusOK, nil
}

// readThemePackage pulls a palette and at most one picture out of a
// zipped theme directory.
func readThemePackage(body []byte, want string) (themePackage, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return themePackage{}, fmt.Errorf("that does not read as a .zip file")
	}
	if len(zr.File) > maxThemePackageEntries {
		return themePackage{}, fmt.Errorf(
			"that archive holds %d files; a theme has a handful", len(zr.File))
	}

	var paletteFile *zip.File
	var pictures []*zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// The NAME is only ever compared, never joined to a path and
		// never opened as one. Cleaning it here is about matching
		// reliably, not about safety — nothing downstream touches the
		// filesystem.
		clean := path.Clean(strings.ReplaceAll(f.Name, `\`, "/"))
		base := strings.ToLower(path.Base(clean))
		switch {
		case base == "colors.toml":
			// The shallowest one wins. A GitHub zip nests the whole
			// theme under <repo>-<branch>/, and an archive of several
			// themes would otherwise be decided by iteration order.
			if paletteFile == nil || depth(clean) < depth(path.Clean(paletteFile.Name)) {
				paletteFile = f
			}
		case strings.Contains(strings.ToLower(clean)+"/", "/backgrounds/"):
			if _, ok := wallpaperTypes[strings.ToLower(path.Ext(base))]; ok {
				pictures = append(pictures, f)
			}
		}
	}
	if paletteFile == nil {
		return themePackage{}, fmt.Errorf(
			"no colors.toml in that archive — download the theme's folder from its repository and upload that")
	}

	colors, err := readZipText(paletteFile, maxThemePaletteBytes)
	if err != nil {
		return themePackage{}, err
	}

	pkg := themePackage{name: strings.TrimSpace(want), colors: colors}
	if pkg.name == "" {
		pkg.name = themeNameFromPath(paletteFile.Name)
	}
	if !theme.ValidName(pkg.name) {
		return themePackage{}, fmt.Errorf(
			"couldn't tell what to call this theme from the archive — give it a name (lower-case letters, digits and hyphens)")
	}

	if f := pickPackageBackground(pictures); f != nil {
		img, rerr := readZipBytes(f, runtimedb.MaxThemeBackgroundBytes)
		// A picture that will not come out of the archive is not worth
		// failing an import over; the palette is the theme.
		if rerr == nil {
			// SNIFFED, NOT TRUSTED. The extension chose which entries
			// were candidates; the bytes decide what is served, which
			// is the rule handlers_portal_banner.go already follows.
			if mt, ok := portal.ImageType(img); ok {
				pkg.image, pkg.mediaType, pkg.background = img, mt, path.Base(f.Name)
			}
		}
	}
	return pkg, nil
}

func depth(p string) int { return strings.Count(p, "/") }

// themeNameFromPath is the directory the palette sits in.
//
// GitHub names a downloaded zip's root <repo>-<branch>, so "tokyo-night
// -master" becomes "tokyo-night": a trailing branch word is stripped
// when one is there to strip. A household that disagrees passes a name.
func themeNameFromPath(name string) string {
	dir := path.Base(path.Dir(path.Clean(strings.ReplaceAll(name, `\`, "/"))))
	if dir == "." || dir == "/" || dir == "" {
		return ""
	}
	for _, branch := range []string{"-main", "-master", "-quattro", "-trunk"} {
		if strings.HasSuffix(dir, branch) {
			dir = strings.TrimSuffix(dir, branch)
			break
		}
	}
	return strings.ToLower(dir)
}

// pickPackageBackground chooses one picture out of the set a theme
// ships, by the rule wallpapers/NOTICE.md records for the vendored
// ones: the lowest-numbered, skipping the branded logo wall, and
// preferring one small enough to be a dashboard backdrop.
func pickPackageBackground(files []*zip.File) *zip.File {
	if len(files) == 0 {
		return nil
	}
	var named []*zip.File
	for _, f := range files {
		base := strings.ToLower(path.Base(f.Name))
		if base == "omarchy.webp" || strings.HasSuffix(base, "-omarchy.webp") {
			continue
		}
		named = append(named, f)
	}
	if len(named) == 0 {
		named = files
	}
	sort.SliceStable(named, func(i, j int) bool {
		return path.Base(named[i].Name) < path.Base(named[j].Name)
	})
	const prefer = 1_500_000
	for _, f := range named {
		if f.UncompressedSize64 <= prefer {
			return f
		}
	}
	smallest := named[0]
	for _, f := range named {
		if f.UncompressedSize64 < smallest.UncompressedSize64 {
			smallest = f
		}
	}
	return smallest
}

// readZipText reads one entry as text, bounded.
func readZipText(f *zip.File, limit int64) (string, error) {
	b, err := readZipBytes(f, limit)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readZipBytes reads one entry, bounded by what the caller will accept
// rather than by what the archive declares.
//
// The declared size is a claim the archive makes about itself, so the
// READ is limited and the claim is only used to fail early. A member
// that keeps producing bytes past the limit is refused rather than
// truncated: half a palette parses into a different theme, and half a
// picture is a broken one.
func readZipBytes(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("%s is larger than this surface accepts", path.Base(f.Name))
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("reading %s from the archive: %w", path.Base(f.Name), err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s from the archive: %w", path.Base(f.Name), err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than it said it was", path.Base(f.Name))
	}
	return b, nil
}
